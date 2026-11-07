package storage

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

// retentionTestRows counts a scope's stored history rows: journal events and
// samples of ongoing issues.
func retentionTestRows(t *testing.T, p *postgresProvider, scope EvidenceScope) (events, samples int) {
	t.Helper()
	key, err := evidenceScopeKey(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM kubernetes_evidence_events WHERE scope_key=$1),(SELECT count(*) FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1)`, key).Scan(&events, &samples); err != nil {
		t.Fatal(err)
	}
	return events, samples
}

func settleRetention(t *testing.T, p *postgresProvider) {
	t.Helper()
	for i := 0; ; i++ {
		if i == 1000 {
			t.Fatal("retention never settled")
		}
		more, err := p.compactEvidenceBatch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
}

// Retention "0" keeps a month that is long past and every sample of an
// ongoing issue, and a page link over them keeps paging. The same rows under
// the empty (default 168h) retention lose the old month, and the link issued
// before the drop answers expired; a malformed link stays invalid.
func TestPostgresHistoryRetentionZeroKeepsAllAndDefaultDropsOldMonth(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated harness Postgres required")
	}
	t.Setenv("KUBERNETES_EVIDENCE_RETENTION", "")
	provider, err := NewPostgres(PostgresOptions{DSN: dsn, EvidenceRetention: "0"})
	if err != nil {
		t.Fatal(err)
	}
	keep := provider.(*postgresProvider)
	keep.maintenanceCancel()
	<-keep.maintenanceDone
	if keep.evidenceRetention != 0 {
		t.Fatalf("retention \"0\" resolved to %s", keep.evidenceRetention)
	}
	settlePartitionCopy(t, keep)

	now := time.Now().UTC()
	run := fmt.Sprint(now.UnixNano())
	oldMonth := time.Date(now.Year(), now.Month()-2, 10, 12, 0, 0, 0, time.UTC)
	thinAt := now.Add(-13 * 24 * time.Hour).Truncate(time.Hour)
	// Each scope holds one ongoing issue seen eight times six minutes apart,
	// so every sample is a candidate for thinning to one per hour.
	seed := func(scope EvidenceScope, at time.Time) EvidenceQuery {
		t.Helper()
		checkpoint, err := keep.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		previous := checkpoint.ID
		for i := 0; i < 8; i++ {
			batch := evidenceTestBatch(fmt.Sprintf("%s-%d", scope.ClusterID, i), previous, at.Add(time.Duration(6*i)*time.Minute), true, true)
			previous = batch.ID
			for n := 0; n < 3; n++ {
				issue := batch.Issues[0]
				issue.Key, issue.UID = fmt.Sprint(n), fmt.Sprint(n)
				issue.Broken, issue.FirstSeen, issue.LastSeen = true, at, batch.At
				batch.Issues = append(batch.Issues, issue)
			}
			batch.Issues = batch.Issues[1:]
			if err := keep.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
		}
		query := EvidenceQuery{Since: at.Add(-time.Hour), Until: at.Add(2 * time.Hour), Type: "occurrence", Limit: 1}
		page, err := keep.QueryEvidence(t.Context(), scope, query)
		if err != nil || page.Next == "" {
			t.Fatalf("page 1: next=%q err=%v", page.Next, err)
		}
		query.Cursor = page.Next
		return query
	}
	old := EvidenceScope{OrgID: "default", ClusterID: "retention-old-" + run, ClusterUID: "old"}
	thin := EvidenceScope{OrgID: "default", ClusterID: "retention-thin-" + run, ClusterUID: "thin"}
	oldQuery, thinQuery := seed(old, oldMonth), seed(thin, thinAt)
	oldEvents, oldSamples := retentionTestRows(t, keep, old)
	thinEvents, thinSamples := retentionTestRows(t, keep, thin)
	if oldSamples < 8 || thinSamples < 8 {
		t.Fatalf("seed stored too few samples to thin: old=%d thin=%d", oldSamples, thinSamples)
	}
	before, err := keep.QueryEvidence(t.Context(), old, oldQuery)
	if err != nil {
		t.Fatalf("page 2 before maintenance: %v", err)
	}

	settleRetention(t, keep)
	if events, samples := retentionTestRows(t, keep, old); events != oldEvents || samples != oldSamples {
		t.Fatalf("retention \"0\" removed old-month rows: events %d->%d samples %d->%d", oldEvents, events, oldSamples, samples)
	}
	if events, samples := retentionTestRows(t, keep, thin); events != thinEvents || samples != thinSamples {
		t.Fatalf("retention \"0\" thinned samples: events %d->%d samples %d->%d", thinEvents, events, thinSamples, samples)
	}
	if after, err := keep.QueryEvidence(t.Context(), old, oldQuery); err != nil || !reflect.DeepEqual(before.Items, after.Items) {
		t.Fatalf("retention \"0\" invalidated a page link: %v", err)
	}
	if _, err := keep.QueryEvidence(t.Context(), thin, thinQuery); err != nil {
		t.Fatalf("retention \"0\" invalidated a page link: %v", err)
	}
	if err = keep.Close(); err != nil {
		t.Fatal(err)
	}

	provider, err = NewPostgres(PostgresOptions{DSN: dsn, EvidenceRetention: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	def := provider.(*postgresProvider)
	def.maintenanceCancel()
	<-def.maintenanceDone
	if def.evidenceRetention != 168*time.Hour {
		t.Fatalf("empty retention resolved to %s, want 168h", def.evidenceRetention)
	}
	settleRetention(t, def)
	if events, samples := retentionTestRows(t, def, old); events != 0 || samples != 0 {
		t.Fatalf("default retention kept a month two months old: events=%d samples=%d", events, samples)
	}
	if _, samples := retentionTestRows(t, def, thin); samples >= thinSamples {
		t.Fatalf("default retention neither thinned nor dropped samples 13 days old: %d -> %d", thinSamples, samples)
	}
	if _, err = def.QueryEvidence(t.Context(), old, oldQuery); !errors.Is(err, ErrEvidenceCursorExpired) {
		t.Fatalf("link over a dropped month: %v, want expired", err)
	}
	malformed := oldQuery
	malformed.Cursor = "not-a-cursor"
	if _, err = def.QueryEvidence(t.Context(), old, malformed); !errors.Is(err, ErrInvalidEvidence) || errors.Is(err, ErrEvidenceCursorExpired) {
		t.Fatalf("malformed link: %v, want invalid and not expired", err)
	}
}
