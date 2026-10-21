package kubernetes

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/kref"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Temporarily hide only this throwaway database's blob table during the
// optional history read. The returned error is from real Postgres, not a
// substituted transport or a synthetic storage error.
type unavailableHistoryTable struct {
	storage.Provider
	db       *sql.DB
	reads    int
	failAt   int
	injected bool
}

func (p *unavailableHistoryTable) CompareAndSwapBlob(key string, old, replacement []byte) (bool, error) {
	return p.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, old, replacement)
}

func (p *unavailableHistoryTable) ReadBlob(key string) ([]byte, error) {
	if !strings.HasPrefix(key, "kubernetes/changes/") {
		return p.Provider.ReadBlob(key)
	}
	p.reads++
	if p.reads != p.failAt {
		return p.Provider.ReadBlob(key)
	}
	if _, err := p.db.Exec("ALTER TABLE vs_blobs RENAME TO unavailable_history_blobs"); err != nil {
		return nil, err
	}
	data, readErr := p.Provider.ReadBlob(key)
	_, restoreErr := p.db.Exec("ALTER TABLE unavailable_history_blobs RENAME TO vs_blobs")
	if restoreErr != nil {
		return nil, fmt.Errorf("restore owned blob table: %w", restoreErr)
	}
	p.injected = readErr != nil
	return data, readErr
}

func TestPostgresIssueAlertOptionalHistoryOutageAndLeaseFence(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN required")
	}
	base, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal("open dedicated Postgres database failed")
	}
	t.Cleanup(func() { _ = base.Close() })
	db := base.(storage.SQLAccessor).DB()
	var database string
	if err := db.QueryRow("SELECT current_database()").Scan(&database); err != nil || !strings.HasPrefix(database, "qa_") {
		t.Fatal("throwaway harness qa_ database required for table-unavailability probe")
	}
	provider := &unavailableHistoryTable{Provider: base, db: db}
	t.Cleanup(func() {
		// No data is deleted, including on a failed assertion.
		_, _ = db.Exec("ALTER TABLE IF EXISTS unavailable_history_blobs RENAME TO vs_blobs")
	})
	s := alertTestService(t)
	s.SetChangeStorage(provider)
	if err := s.SaveIssueAlertSettings(t.Context(), true, "warning", "acceptance-admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.changeStore().ClaimLease("acceptance-owner", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	items := make([]kubebreaks.Break, 12)
	for i := range items {
		severity := "warning"
		if i == 11 {
			severity = "critical"
		}
		items[i] = kubebreaks.Break{
			Root: kref.Ref{UID: fmt.Sprint(i), Name: fmt.Sprintf("checkout-%d", i), Kind: "Deployment", Namespace: "shop"},
			Rule: "pod.crash_loop", Severity: severity, BrokeAt: time.Now().UTC().Add(time.Second),
		}
	}
	provider.reads, provider.failAt = 0, 3
	var alerts []IssueAlert
	s.SetIssueAlertSink(func(_ context.Context, alert IssueAlert) (Outcome, error) {
		alerts = append(alerts, alert)
		return Outcome{NotificationOutcome: "sent"}, nil
	})
	if err := s.emitIssueAlerts(t.Context(), items, "acceptance-owner", time.Now().UTC(), &issueAlertWindow{}); err != nil {
		t.Fatal(err)
	}
	if !provider.injected || len(alerts) != 11 || !alerts[0].HistoryIncomplete {
		t.Fatalf("real history outage did not degrade to alert facts: outage=%v alerts=%d", provider.injected, len(alerts))
	}
	if alerts[10].RollupCount != 2 || alerts[10].Severity != "critical" {
		t.Fatalf("critical overflow lost: count=%d severity=%s", alerts[10].RollupCount, alerts[10].Severity)
	}
	alerts = nil
	if err := s.emitIssueAlerts(t.Context(), items, "stale-owner", time.Now().UTC(), &issueAlertWindow{}); err == nil || len(alerts) != 0 {
		t.Fatal("lease failure was incorrectly treated as optional history failure")
	}
}
