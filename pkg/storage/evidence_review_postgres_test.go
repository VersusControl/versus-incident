package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresEvidenceDeletionGenerationAndOccurrenceHeartbeat(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("explicit isolated harness Postgres required")
	}
	open := func() Provider {
		provider, err := NewPostgres(PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal("isolated Postgres initialization failed")
		}
		t.Cleanup(func() { _ = provider.Close() })
		return provider
	}
	provider := open()
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "synthetic-review", ClusterID: fmt.Sprintf("cluster-%d", time.Now().UnixNano()), ClusterUID: "physical"}
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	initial := reviewEvidenceCapture(t, store, scope, "initial", at, `{"count":1}`)
	second := initial.Issues[0]
	second.Key = "second"
	second.UID = "second"
	initial.Issues = append(initial.Issues, second)
	if err := store.CommitEvidence(t.Context(), scope, initial); err != nil {
		t.Fatal(err)
	}
	query := EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Type: "issue", Limit: 1}
	first, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || first.Next == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	query.Cursor = first.Next
	before, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := reviewEvidenceCapture(t, store, scope, "heartbeat", at.Add(time.Minute), `{"count":1}`)
	second.LastSeen = heartbeat.At
	heartbeat.Issues = append(heartbeat.Issues, second)
	if err = store.CommitEvidence(t.Context(), scope, heartbeat); err != nil {
		t.Fatal(err)
	}
	key, _ := evidenceScopeKey(scope)
	var lastSeen time.Time
	if err = provider.(SQLAccessor).DB().QueryRowContext(t.Context(), `SELECT last_seen FROM kubernetes_evidence_issues WHERE scope_key=$1 AND issue_key=$2 AND closed_at IS NULL`, key, "pod-rule").Scan(&lastSeen); err != nil || !lastSeen.Equal(heartbeat.At) {
		t.Fatalf("occurrence freshness=%s err=%v", lastSeen, err)
	}
	freshPage, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: query.Since, Until: query.Until, Type: "issue"})
	if err != nil || freshPage.Total != 2 {
		t.Fatalf("heartbeat duplicated journal: total=%d err=%v", freshPage.Total, err)
	}
	after, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil {
		t.Fatal(err)
	}
	beforeRaw, _ := json.Marshal(before.Items)
	afterRaw, _ := json.Marshal(after.Items)
	if string(beforeRaw) != string(afterRaw) || before.Total != after.Total {
		t.Fatal("heartbeat changed a pinned page")
	}
	if err = store.DeleteEvidence(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	reopened := open().(EvidenceStore)
	checkpoint, err := reopened.EvidenceCheckpoint(t.Context(), scope)
	if err != nil || checkpoint.Generation != 1 || checkpoint.ID != "" {
		t.Fatalf("durable generation=%+v err=%v", checkpoint, err)
	}
	fresh := reviewEvidenceCapture(t, reopened, scope, "fresh", at.Add(2*time.Minute), `{"count":2}`)
	if err = reopened.CommitEvidence(t.Context(), scope, fresh); err != nil {
		t.Fatal(err)
	}
	initial.Expected = fresh.ID
	initial.At = fresh.At
	initial.CoveredThrough = time.Time{}
	initial.HistoricalOnly = true
	if err = reopened.CommitEvidence(t.Context(), scope, initial); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("pre-delete generation rebased: %v", err)
	}
	initial.ID = fresh.ID
	if err = reopened.CommitEvidence(t.Context(), scope, initial); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("capture dedup bypassed deletion fence: %v", err)
	}
	if _, err = reopened.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("pre-delete cursor accepted: %v", err)
	}
}
