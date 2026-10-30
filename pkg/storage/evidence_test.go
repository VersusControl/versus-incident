package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func evidenceTestBatch(id, expected string, at time.Time, present, complete bool) EvidenceBatch {
	batch := EvidenceBatch{ID: id, Expected: expected, At: at, Epoch: "synthetic-epoch"}
	if complete {
		batch.CoveredThrough = at
	}
	if present {
		batch.Issues = []EvidenceIssue{{Key: "resource-rule", Namespace: "shop", Kind: "Pod", UID: "pod-uid", Rule: "pod.crash_loop", Payload: json.RawMessage(`{"rule":"pod.crash_loop"}`)}}
	}
	return batch
}

func TestEvidenceLifecycleIsAtomicIdempotentAndCoverageSafe(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider = NewMemory()
			var err error
			dir := t.TempDir()
			if backend == "file" {
				provider, err = NewFile(FileOptions{DataDir: dir})
				if err != nil {
					t.Fatal(err)
				}
			}
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical-a"}
			origin := time.Now().UTC().Add(-time.Hour)
			batches := []EvidenceBatch{evidenceTestBatch("open", "", origin, true, true), evidenceTestBatch("partial", "open", origin.Add(time.Minute), false, false), evidenceTestBatch("seen", "partial", origin.Add(2*time.Minute), true, true), evidenceTestBatch("resolve", "seen", origin.Add(3*time.Minute), false, true), evidenceTestBatch("recur", "resolve", origin.Add(4*time.Minute), true, true)}
			for _, batch := range batches {
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatal(err)
				}
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatalf("duplicate failed: %v", err)
				}
			}
			if backend == "file" {
				provider, err = NewFile(FileOptions{DataDir: dir})
				if err != nil {
					t.Fatal(err)
				}
				store = provider.(EvidenceStore)
			}
			if err = store.CommitEvidence(t.Context(), scope, batches[0]); err != nil {
				t.Fatalf("retained duplicate delivery failed: %v", err)
			}
			page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour), Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			occurrences := map[string]bool{}
			for _, record := range page.Items {
				counts[record.Type]++
				var issue EvidenceIssue
				if err = json.Unmarshal(record.Payload, &issue); err != nil {
					t.Fatal(err)
				}
				occurrences[issue.OccurrenceID] = true
			}
			if counts["issue_opened"] != 2 || counts["issue_resolved"] != 1 || counts["issue_observed"] != 0 || len(occurrences) != 2 {
				t.Fatalf("lifecycle=%v occurrences=%d", counts, len(occurrences))
			}
			for _, foreign := range []EvidenceScope{{OrgID: "other", ClusterID: "cluster", ClusterUID: "physical-a"}, {OrgID: "org", ClusterID: "other", ClusterUID: "physical-a"}, {OrgID: "org", ClusterID: "cluster", ClusterUID: "physical-b"}} {
				foreignPage, err := store.QueryEvidence(t.Context(), foreign, EvidenceQuery{Since: origin, Until: origin.Add(time.Hour)})
				if err != nil || len(foreignPage.Items) != 0 {
					t.Fatal("scope leaked evidence")
				}
			}
			checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			bad := evidenceTestBatch("bad", checkpoint.ID, origin.Add(5*time.Minute), false, true)
			bad.Records = []EvidenceRecord{{ID: "invalid"}}
			if err = store.CommitEvidence(t.Context(), scope, bad); err == nil {
				t.Fatal("invalid event committed")
			}
			after, _ := store.EvidenceCheckpoint(t.Context(), scope)
			if after.ID != checkpoint.ID {
				t.Fatal("failed write advanced checkpoint")
			}
		})
	}
}

func TestEvidencePaginationWatermarkExcludesBackdatedConcurrentInserts(t *testing.T) {
	store := NewMemory().(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	origin := time.Now().UTC().Add(-time.Hour)
	batch := evidenceTestBatch("first", "", origin.Add(time.Minute), false, false)
	for i := 0; i < 12; i++ {
		batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("event-%02d", i), Type: "change", At: origin.Add(time.Duration(i) * time.Second), ObservedAt: batch.At, Epoch: batch.Epoch, Payload: json.RawMessage(`{}`)})
	}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	query := EvidenceQuery{Since: origin, Until: origin.Add(time.Hour), Limit: 3}
	first, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || first.Next == "" {
		t.Fatal("first page missing")
	}
	concurrent := evidenceTestBatch("concurrent", "first", origin.Add(2*time.Minute), false, false)
	concurrent.Records = []EvidenceRecord{{ID: "backdated", Type: "change", At: origin.Add(5 * time.Second), ObservedAt: concurrent.At, Epoch: concurrent.Epoch, Payload: json.RawMessage(`{}`)}}
	if err := store.CommitEvidence(t.Context(), scope, concurrent); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	page := first
	for {
		for _, record := range page.Items {
			if record.ID == "backdated" || seen[record.ID] {
				t.Fatal("paging included insert or duplicate")
			}
			seen[record.ID] = true
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
		page, err = store.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 12 {
		t.Fatalf("lost history: %d", len(seen))
	}
	query.Namespace = "other"
	if _, err = store.QueryEvidence(t.Context(), scope, query); err == nil {
		t.Fatal("cursor crossed authorized namespace")
	}
}

func TestEvidenceConcurrentWritersDoNotAdvanceOneCheckpointTwice(t *testing.T) {
	store := NewMemory().(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	origin := time.Now().UTC()
	var wait sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			results <- store.CommitEvidence(t.Context(), scope, evidenceTestBatch(fmt.Sprint(i), "", origin, true, true))
		}(i)
	}
	wait.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrEvidenceConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("checkpoint winners=%d", success)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.CommitEvidence(ctx, scope, evidenceTestBatch("cancelled", "", origin, false, false)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestFileEvidencePruningIsExplicitAndPreservesActiveOccurrence(t *testing.T) {
	provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	origin := time.Now().UTC().Add(-48 * time.Hour)
	if err = store.CommitEvidence(t.Context(), scope, evidenceTestBatch("first", "", origin, true, true)); err != nil {
		t.Fatal(err)
	}
	batch := evidenceTestBatch("later", "first", origin.Add(30*time.Hour), true, false)
	if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if page.Policy.Mode != "bounded_file" || page.Checkpoint.PrunedThrough.IsZero() || len(page.Items) != 0 {
		t.Fatalf("file policy=%+v checkpoint=%+v items=%d", page.Policy, page.Checkpoint, len(page.Items))
	}
	state, err := blobEvidenceRead(t.Context(), provider, scope)
	if err != nil {
		t.Fatal(err)
	}
	issue, exists := state.Active["resource-rule"]
	if !exists || !issue.FirstSeen.Equal(origin) || !issue.LastSeen.Equal(batch.At) || issue.ClosedAt != nil {
		t.Fatal("pruning lost active lifecycle")
	}
	if p := (&postgresProvider{}).EvidencePolicy(); p.Mode != "unbounded_database" || p.Retention != 0 || p.MaxRecords != 0 || p.MaxBytes != 0 {
		t.Fatal("database policy imposes retention")
	}
}

func TestPostgresEvidenceRetainsLargeHistoryWithoutApplicationPruning(t *testing.T) {
	dsn := os.Getenv("KUBERNETES_EVIDENCE_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("isolated synthetic Postgres DSN not supplied")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal("isolated Postgres setup failed")
	}
	defer provider.Close()
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "synthetic-evidence", ClusterID: fmt.Sprintf("capture-%d", time.Now().UnixNano()), ClusterUID: "synthetic-physical"}
	origin := time.Now().UTC().Add(-60 * 24 * time.Hour)
	expected := ""
	var first EvidenceBatch
	const total = 100500
	for offset := 0; offset < total; offset += 1000 {
		batch := EvidenceBatch{ID: fmt.Sprintf("batch-%d", offset), Expected: expected, At: time.Now().UTC(), Epoch: "synthetic-epoch"}
		for i := offset; i < min(offset+1000, total); i++ {
			payload, _ := json.Marshal(map[string]string{"summary": fmt.Sprintf("safe event %d %s", i, strings.Repeat("x", 1024))})
			batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("event-%06d", i), Type: "change", At: origin.Add(time.Duration(i) * 40 * time.Second), ObservedAt: batch.At, Namespace: "shop", Kind: "Pod", UID: "synthetic-pod", Epoch: batch.Epoch, Payload: payload})
		}
		if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
		if offset == 0 {
			first = batch
		}
		expected = batch.ID
	}
	if err = store.CommitEvidence(t.Context(), scope, first); err != nil {
		t.Fatal("old capture replay was not idempotent")
	}
	query := EvidenceQuery{Since: origin.Add(-time.Hour), Until: time.Now().UTC(), Type: "change", Limit: 500}
	count := 0
	seen := map[string]bool{}
	for {
		page, err := store.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != total || page.Policy.Mode != "unbounded_database" || !page.Checkpoint.PrunedThrough.IsZero() {
			t.Fatal("database history was capped or marked expired")
		}
		for _, record := range page.Items {
			if seen[record.ID] {
				t.Fatal("duplicate evidence while paging")
			}
			seen[record.ID] = true
			count++
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	if count != total {
		t.Fatalf("retained rows=%d want=%d", count, total)
	}
}

func TestEvidenceGapOverlapsWindowAndTrustedArchivesPageWithoutDuplicates(t *testing.T) {
	provider := NewMemory()
	store := provider.(EvidenceStore)
	now := time.Now().UTC()
	scope := EvidenceScope{OrgID: "default", ClusterID: "cluster", ClusterUID: "physical"}
	batch := EvidenceBatch{ID: "archive", At: now, Epoch: "synthetic"}
	for i := 0; i < 8; i++ {
		batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("event-%d", i), Type: "change", At: now.Add(time.Duration(i-10) * time.Hour), ObservedAt: now, Namespace: "shop", Kind: "Pod", UID: "pod", Epoch: "synthetic", Payload: json.RawMessage(`{}`)})
	}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	scope.OrgID = "licensed"
	gapPayload, _ := json.Marshal(map[string]time.Time{"from": now.Add(-20 * time.Hour), "to": now.Add(-time.Hour)})
	current := EvidenceBatch{ID: "current", At: now, Epoch: "synthetic", Records: []EvidenceRecord{batch.Records[0], {ID: "gap", Type: "coverage_gap", GapFrom: now.Add(-20 * time.Hour), GapTo: now.Add(-time.Hour), At: now.Add(-time.Hour), ObservedAt: now, Epoch: "synthetic", Payload: gapPayload}}}
	if err := store.CommitEvidence(t.Context(), scope, current); err != nil {
		t.Fatal(err)
	}
	scope.ReadOrgs = []string{"licensed", "default"}
	query := EvidenceQuery{Since: now.Add(-12 * time.Hour), Until: now.Add(-2 * time.Hour), Namespace: "shop", Type: "change", WithGaps: true, Limit: 2}
	count := 0
	seen := map[string]bool{}
	gaps := 0
	for {
		page, err := store.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 9 {
			t.Fatalf("trusted merge total=%d", page.Total)
		}
		for _, record := range page.Items {
			if seen[record.ID] {
				t.Fatal("archive duplicate")
			}
			seen[record.ID] = true
			if record.Type == "coverage_gap" {
				gaps++
			}
			count++
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	if count != 9 || gaps != 1 {
		t.Fatalf("merged rows=%d gaps=%d", count, gaps)
	}
}

func TestFileEvidenceCountAndBytePruningInvalidatesPagingWithoutClosingIssues(t *testing.T) {
	provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	now := time.Now().UTC()
	first := evidenceTestBatch("first", "", now.Add(-4*time.Hour), true, true)
	for i := 0; i < 12; i++ {
		first.Records = append(first.Records, EvidenceRecord{ID: fmt.Sprintf("initial-%d", i), Type: "change", At: first.At, ObservedAt: first.At, Epoch: first.Epoch, Payload: json.RawMessage(`{}`)})
	}
	if err = store.CommitEvidence(t.Context(), scope, first); err != nil {
		t.Fatal(err)
	}
	query := EvidenceQuery{Since: now.Add(-5 * time.Hour), Until: now, Limit: 2}
	page, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || page.Next == "" {
		t.Fatal("missing initial cursor")
	}
	cursor := page.Next
	later := evidenceTestBatch("later", "first", now, true, false)
	payload, _ := json.Marshal(map[string]string{"summary": strings.Repeat("x", 1000)})
	for i := 0; i < 6000; i++ {
		later.Records = append(later.Records, EvidenceRecord{ID: fmt.Sprintf("large-%d", i), Type: "change", At: now.Add(-2 * time.Hour).Add(time.Duration(i) * time.Second), ObservedAt: now, Epoch: later.Epoch, Payload: payload})
	}
	if err = store.CommitEvidence(t.Context(), scope, later); err != nil {
		t.Fatal(err)
	}
	state, err := blobEvidenceRead(t.Context(), provider, scope)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if len(raw) > 4<<20 || len(state.Records) > 5000 || state.Checkpoint.PruneRevision == 0 || len(state.Active) != 1 {
		t.Fatalf("unsafe file budget: bytes=%d rows=%d active=%d", len(raw), len(state.Records), len(state.Active))
	}
	query.Cursor = cursor
	if _, err = store.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatal("pruned cursor silently skipped evidence")
	}
}

func TestEvidenceRefinementsPreservePinnedPagesAndPreferObservedFacts(t *testing.T) {
	store := NewMemory().(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	now := time.Now().UTC().Add(-time.Minute)
	initial := EvidenceBatch{ID: "initial", At: now, Epoch: "synthetic", Records: []EvidenceRecord{{ID: "target", Type: "change", At: now.Add(-time.Second), ObservedAt: now, Epoch: "synthetic", Priority: 1, Payload: json.RawMessage(`{"count":1}`)}, {ID: "other", Type: "change", At: now, ObservedAt: now, Epoch: "synthetic", Payload: json.RawMessage(`{}`)}}}
	if err := store.CommitEvidence(t.Context(), scope, initial); err != nil {
		t.Fatal(err)
	}
	query := EvidenceQuery{Since: now.Add(-time.Hour), Until: now.Add(time.Hour), Limit: 1}
	first, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || first.Next == "" {
		t.Fatal("initial paging unavailable")
	}
	refined := initial.Records[0]
	refined.At = now.Add(time.Second)
	refined.ObservedAt = now.Add(time.Second)
	refined.Priority = 2
	refined.Payload = json.RawMessage(`{"count":3}`)
	if err = store.CommitEvidence(t.Context(), scope, EvidenceBatch{ID: "refined", Expected: "initial", At: now.Add(time.Second), Epoch: "synthetic", Records: []EvidenceRecord{refined}}); err != nil {
		t.Fatal(err)
	}
	query.Cursor = first.Next
	old, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || len(old.Items) != 1 || string(old.Items[0].Payload) != `{"count":1}` {
		t.Fatal("refinement moved or erased an already pinned row")
	}
	query.Cursor = ""
	latest, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || latest.Total != 2 || latest.Items[0].ID != "target" || string(latest.Items[0].Payload) != `{"count":3}` {
		t.Fatal("new query did not see strongest facts")
	}
	weak := refined
	weak.Priority = 1
	weak.Payload = json.RawMessage(`{"count":2}`)
	if err = store.CommitEvidence(t.Context(), scope, EvidenceBatch{ID: "weak", Expected: "refined", At: now.Add(2 * time.Second), Epoch: "synthetic", Records: []EvidenceRecord{weak}}); err != nil {
		t.Fatal(err)
	}
	latest, err = store.QueryEvidence(t.Context(), scope, query)
	if err != nil || string(latest.Items[0].Payload) != `{"count":3}` {
		t.Fatal("reconstruction replaced stronger observed evidence")
	}
}

func TestHistoricalObservationCannotReopenAResolvedIssue(t *testing.T) {
	provider := NewMemory()
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	now := time.Now().UTC()
	if err := store.CommitEvidence(t.Context(), scope, evidenceTestBatch("open", "", now, true, true)); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitEvidence(t.Context(), scope, evidenceTestBatch("resolved", "open", now.Add(time.Minute), false, true)); err != nil {
		t.Fatal(err)
	}
	late := evidenceTestBatch("late", "resolved", now.Add(time.Minute), true, false)
	late.HistoricalOnly = true
	late.Issues[0].LastSeen = now
	if err := store.CommitEvidence(t.Context(), scope, late); err != nil {
		t.Fatal(err)
	}
	state, err := blobEvidenceRead(t.Context(), provider, scope)
	if err != nil || len(state.Active) != 0 {
		t.Fatal("older observation reopened a resolved occurrence")
	}
	if !state.Checkpoint.CoveredThrough.Equal(now.Add(time.Minute)) {
		t.Fatal("late observation changed committed coverage")
	}
	found := false
	for _, record := range state.Records {
		if record.Type != "issue_observed" {
			continue
		}
		var issue EvidenceIssue
		if err = json.Unmarshal(record.Payload, &issue); err != nil {
			t.Fatal(err)
		}
		found = found || issue.ObservationOnly
	}
	if !found {
		t.Fatal("older safe evidence was silently dropped")
	}
}

func TestEvidenceCountCacheBoundsQueryWorkWithoutRetainingCallerMaps(t *testing.T) {
	provider := &postgresProvider{}
	original := map[string]int{"change": 7}
	provider.rememberEvidenceCounts("query", 7, original)
	original["change"] = 0
	total, counts, ok := provider.evidenceCounts("query")
	if !ok || total != 7 || counts["change"] != 7 {
		t.Fatal("query count cache retained mutable caller data")
	}
	counts["change"] = 100
	_, again, _ := provider.evidenceCounts("query")
	if again["change"] != 7 {
		t.Fatal("query response mutated cached counts")
	}
	for i := 0; i < 200; i++ {
		provider.rememberEvidenceCounts(fmt.Sprint(i), i, map[string]int{"change": i})
	}
	if len(provider.evidenceQueries) > 128 {
		t.Fatal("query metadata cache grew without bound")
	}
	provider.evidenceQueries["expired"] = evidenceCountSnapshot{Expires: time.Now().Add(-time.Second)}
	if _, _, ok = provider.evidenceCounts("expired"); ok {
		t.Fatal("expired query metadata was reused")
	}
	generation := provider.evidenceCountGeneration()
	finish := provider.beginEvidenceDeletion()
	provider.rememberEvidenceCounts("stale", 9, map[string]int{"change": 9}, generation)
	if _, _, ok := provider.evidenceCounts("stale"); ok {
		t.Fatal("delete allowed an in-flight old count publication")
	}
	finish()
	provider.rememberEvidenceCounts("stale", 9, map[string]int{"change": 9}, generation)
	if _, _, ok := provider.evidenceCounts("stale"); ok {
		t.Fatal("old query repopulated count metadata after deletion")
	}
	if policy := provider.EvidencePolicy(); policy.MaxRecords != 0 || policy.MaxBytes != 0 || policy.Retention != 0 {
		t.Fatal("query cache bounds became history retention limits")
	}
}
