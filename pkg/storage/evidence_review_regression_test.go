package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func reviewEvidenceCapture(t *testing.T, store EvidenceStore, scope EvidenceScope, id string, at time.Time, payload string) EvidenceBatch {
	t.Helper()
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	batch := EvidenceBatch{ID: id, Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at, Epoch: "synthetic", CoveredThrough: at}
	if payload != "" {
		batch.Issues = []EvidenceIssue{{Key: "pod-rule", Kind: "Pod", Namespace: "shop", UID: "pod", Rule: "pod.crash_loop", LastSeen: at, Payload: json.RawMessage(payload)}}
	}
	return batch
}

func TestEvidenceUnchangedPollsUpdateOccurrenceWithoutAppendingHistory(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		for _, heartbeat := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/heartbeat-%t", backend, heartbeat), func(t *testing.T) {
				var provider Provider = NewMemory()
				if backend == "file" {
					var err error
					provider, err = NewFile(FileOptions{DataDir: t.TempDir()})
					if err != nil {
						t.Fatal(err)
					}
				}
				store := provider.(EvidenceStore)
				scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
				origin := time.Now().UTC().Add(-time.Hour)
				for i := 0; i < 3; i++ {
					at := origin.Add(time.Duration(i) * time.Minute)
					payload := `{"severity":"warning","count":1}`
					if heartbeat {
						raw, _ := json.Marshal(map[string]any{"severity": "warning", "count": 1, "first_seen": origin, "last_seen": at})
						payload = string(raw)
					}
					batch := reviewEvidenceCapture(t, store, scope, fmt.Sprintf("poll-%d", i), at, payload)
					if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
						t.Fatal(err)
					}
					if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
						t.Fatal(err)
					}
				}
				page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour), Type: "issue"})
				if err != nil {
					t.Fatal(err)
				}
				if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Type != "issue_opened" {
					t.Fatalf("unchanged polls duplicated journal payloads: total=%d items=%d", page.Total, len(page.Items))
				}
				state, err := blobEvidenceRead(t.Context(), provider, scope)
				if err != nil {
					t.Fatal(err)
				}
				active := state.Active["pod-rule"]
				if !active.LastSeen.Equal(origin.Add(2 * time.Minute)) {
					t.Fatal("occurrence last-seen stopped advancing")
				}
				changed := reviewEvidenceCapture(t, store, scope, "changed", origin.Add(3*time.Minute), `{"severity":"critical","count":2}`)
				if err = store.CommitEvidence(t.Context(), scope, changed); err != nil {
					t.Fatal(err)
				}
				partial := reviewEvidenceCapture(t, store, scope, "partial", origin.Add(4*time.Minute), "")
				partial.CoveredThrough = time.Time{}
				if err = store.CommitEvidence(t.Context(), scope, partial); err != nil {
					t.Fatal(err)
				}
				closed := reviewEvidenceCapture(t, store, scope, "closed", origin.Add(5*time.Minute), "")
				if err = store.CommitEvidence(t.Context(), scope, closed); err != nil {
					t.Fatal(err)
				}
				page, err = store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour), Type: "issue"})
				if err != nil {
					t.Fatal(err)
				}
				counts := map[string]int{}
				for _, record := range page.Items {
					counts[record.Type]++
				}
				if counts["issue_opened"] != 1 || counts["issue_observed"] != 1 || counts["issue_resolved"] != 1 {
					t.Fatalf("meaningful lifecycle history=%v", counts)
				}
			})
		}
	}
}

func TestFileEvidenceDeletionIsScopedAtomicAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	provider, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	other := scope
	other.ClusterID = "other"
	at := time.Now().UTC().Add(-time.Hour)
	for _, target := range []EvidenceScope{scope, other} {
		batch := reviewEvidenceCapture(t, store, target, "initial", at, `{"count":1}`)
		if err = store.CommitEvidence(t.Context(), target, batch); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := evidenceScopeKey(scope)
	if err = provider.(BlobDeleter).DeleteBlob(key); !errors.Is(err, ErrUnsupported) {
		t.Fatal("generic model-state deletion restrictions changed")
	}
	if err = store.DeleteEvidence(t.Context(), scope); err != nil {
		t.Fatalf("scoped evidence deletion failed: %v", err)
	}
	reopened, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	store = reopened.(EvidenceStore)
	query := EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Type: "issue"}
	page, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || page.Total != 0 {
		t.Fatalf("deleted history returned after reopen: total=%d err=%v", page.Total, err)
	}
	foreign, err := store.QueryEvidence(t.Context(), other, query)
	if err != nil || foreign.Total != 1 {
		t.Fatal("evidence deletion crossed its scope")
	}
	fresh := reviewEvidenceCapture(t, store, scope, "fresh", at.Add(time.Minute), `{"count":2}`)
	if err = store.CommitEvidence(t.Context(), scope, fresh); err != nil {
		t.Fatal("fresh capture could not resume after deletion")
	}
}

func TestEvidenceDeletionGenerationFencesRebasedBatches(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider = NewMemory()
			dir := t.TempDir()
			if backend == "file" {
				var err error
				provider, err = NewFile(FileOptions{DataDir: dir})
				if err != nil {
					t.Fatal(err)
				}
			}
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
			at := time.Now().UTC().Add(-time.Hour)
			// A queued initial capture has an empty checkpoint too; deletion must fence it.
			initial := reviewEvidenceCapture(t, store, scope, "initial", at, `{"count":1}`)
			if err := store.DeleteEvidence(t.Context(), scope); err != nil {
				t.Fatal(err)
			}
			deleted, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil || deleted.Generation != initial.Generation+1 || deleted.ID != "" {
				t.Fatalf("missing deletion fence: %+v %v", deleted, err)
			}
			fresh := reviewEvidenceCapture(t, store, scope, "fresh", at.Add(time.Minute), `{"count":2}`)
			if err = store.CommitEvidence(t.Context(), scope, fresh); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, historical := range []bool{false, true} {
				stale := initial
				stale.Expected = checkpoint.ID
				stale.At = checkpoint.At.Add(time.Second)
				stale.HistoricalOnly = historical
				stale.CoveredThrough = time.Time{}
				if err = store.CommitEvidence(t.Context(), scope, stale); !errors.Is(err, ErrEvidenceConflict) {
					t.Fatalf("rebased stale generation accepted: %v", err)
				}
			}
			if backend == "file" {
				reopened, err := NewFile(FileOptions{DataDir: dir})
				if err != nil {
					t.Fatal(err)
				}
				store = reopened.(EvidenceStore)
				checkpoint, err = store.EvidenceCheckpoint(t.Context(), scope)
				if err != nil || checkpoint.Generation != deleted.Generation {
					t.Fatal("deletion fence was not durable")
				}
			}
			// A reused capture ID must not turn a stale generation into an idempotent success.
			stale := initial
			stale.ID = fresh.ID
			if err = store.CommitEvidence(t.Context(), scope, stale); !errors.Is(err, ErrEvidenceConflict) {
				t.Fatalf("generation bypassed by duplicate ID: %v", err)
			}
			query := EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Type: "issue"}
			page, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil || page.Total != 1 {
				t.Fatalf("post-delete evidence corrupted: total=%d err=%v", page.Total, err)
			}
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			if err = store.DeleteEvidence(cancelled, scope); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled delete: %v", err)
			}
		})
	}
}

func TestEvidenceHeartbeatsKeepPinnedJournalImmutable(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider = NewMemory()
			if backend == "file" {
				var err error
				provider, err = NewFile(FileOptions{DataDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
			}
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
			at := time.Now().UTC().Add(-time.Hour)
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
			changed := reviewEvidenceCapture(t, store, scope, "changed", at.Add(2*time.Minute), `{"count":2}`)
			second.LastSeen = changed.At
			changed.Issues = append(changed.Issues, second)
			if err = store.CommitEvidence(t.Context(), scope, changed); err != nil {
				t.Fatal(err)
			}
			after, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil {
				t.Fatal(err)
			}
			beforeRaw, _ := json.Marshal(before.Items)
			afterRaw, _ := json.Marshal(after.Items)
			if before.Total != after.Total || !bytes.Equal(beforeRaw, afterRaw) {
				t.Fatal("later observations mutated a pinned journal page")
			}
			if err = store.DeleteEvidence(t.Context(), scope); err != nil {
				t.Fatal(err)
			}
			fresh := reviewEvidenceCapture(t, store, scope, "fresh", at.Add(3*time.Minute), `{"count":3}`)
			fresh.Issues = append(fresh.Issues, second)
			if err = store.CommitEvidence(t.Context(), scope, fresh); err != nil {
				t.Fatal(err)
			}
			if _, err = store.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceCursorExpired) {
				t.Fatalf("pre-delete cursor survived fresh capture: %v", err)
			}
		})
	}
}

func TestFileEvidenceDeleteRacesQueuedCaptureAtomically(t *testing.T) {
	provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	at := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 12; i++ {
		batch := reviewEvidenceCapture(t, store, scope, fmt.Sprintf("capture-%d", i), at.Add(time.Duration(i)*time.Minute), `{"count":1}`)
		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		workers.Add(2)
		go func() { defer workers.Done(); <-start; results <- store.CommitEvidence(t.Context(), scope, batch) }()
		go func() { defer workers.Done(); <-start; results <- store.DeleteEvidence(t.Context(), scope) }()
		close(start)
		workers.Wait()
		close(results)
		for err := range results {
			if err != nil && !errors.Is(err, ErrEvidenceConflict) {
				t.Fatal(err)
			}
		}
		state, err := blobEvidenceRead(t.Context(), provider, scope)
		if err != nil || len(state.Records) != 0 || len(state.Active) != 0 || state.Checkpoint.Generation != uint64(i+1) {
			t.Fatalf("atomic delete lost its fence or leaked payload: %+v %v", state.Checkpoint, err)
		}
	}
}

func TestEvidenceNonObjectIssuePayloadIsStoredAndCountedUnavailable(t *testing.T) {
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider
			var err error
			switch backend {
			case "memory":
				provider = NewMemory()
			case "file":
				provider, err = NewFile(FileOptions{DataDir: t.TempDir()})
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("explicit isolated harness Postgres required")
				}
				provider, err = NewPostgres(PostgresOptions{DSN: dsn})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer provider.Close()
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "synthetic-review", ClusterID: fmt.Sprintf("non-object-%d", time.Now().UnixNano()), ClusterUID: "physical"}
			at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
			batch := reviewEvidenceCapture(t, store, scope, "capture:non-object", at, `{"severity":"warning"}`)
			batch.Issues[0].AuthorizationKind = "Pod"
			malformed := batch.Issues[0]
			malformed.Key, malformed.Rule, malformed.Payload = "malformed", "pod.image_pull", json.RawMessage(`[]`)
			batch.Issues = append(batch.Issues, malformed)
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatalf("one non-object issue payload rejected the whole capture: %v", err)
			}
			page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute), Type: "occurrence", EnforceAuthority: true, AllowedKinds: []string{"Pod"}, Limit: 10})
			if err != nil || page.Total != 2 || len(page.Items) != 2 || page.Counts["issue_opened"] != 1 || page.Counts["coverage_gap"] != 1 || page.Counts["severity:warning"] != 1 {
				t.Fatalf("non-object payload was not stored and counted unavailable: total=%d items=%d counts=%v err=%v", page.Total, len(page.Items), page.Counts, err)
			}
		})
	}
}

func TestEvidencePointInTimeOccurrenceUsesListAvailabilityRule(t *testing.T) {
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider
			var err error
			switch backend {
			case "memory":
				provider = NewMemory()
			case "file":
				provider, err = NewFile(FileOptions{DataDir: t.TempDir()})
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("explicit isolated harness Postgres required")
				}
				provider, err = NewPostgres(PostgresOptions{DSN: dsn})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer provider.Close()
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "synthetic-review", ClusterID: fmt.Sprintf("point-in-time-%d", time.Now().UnixNano()), ClusterUID: "physical"}
			at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
			batch := reviewEvidenceCapture(t, store, scope, "capture:point-in-time", at, `{"severity":"warning"}`)
			batch.Issues[0].AuthorizationKind = "Pod"
			batch.Issues[0].Key = "valid"
			payloads := map[string]string{"non-object": `[]`, "numeric-severity": `{"severity":5}`}
			for key, payload := range payloads {
				issue := batch.Issues[0]
				issue.Key, issue.Rule, issue.Payload = key, "pod."+key, json.RawMessage(payload)
				batch.Issues = append(batch.Issues, issue)
			}
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
			window := EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute), Type: "occurrence", Limit: 10}
			all, err := store.QueryEvidence(t.Context(), scope, window)
			if err != nil || len(all.Items) != 3 {
				t.Fatalf("occurrences: items=%d err=%v", len(all.Items), err)
			}
			for _, item := range all.Items {
				var issue EvidenceIssue
				if err := json.Unmarshal(item.Payload, &issue); err != nil {
					t.Fatal(err)
				}
				expected := "coverage_gap"
				if issue.Key == "valid" {
					expected = "issue_opened"
				}
				for _, kinds := range [][]string{{"Pod"}, {"Event"}} {
					want := expected
					if kinds[0] != "Pod" {
						want = "coverage_gap"
					}
					query := window
					query.PointInTime, query.OccurrenceID = true, item.ID
					query.EnforceAuthority, query.AllowedKinds = true, kinds
					page, err := store.QueryEvidence(t.Context(), scope, query)
					if err != nil || page.Total != 1 || page.Counts[want] != 1 || len(page.Items) != 1 {
						t.Fatalf("%s read with %v: want one %s, got total=%d counts=%v items=%d err=%v", issue.Key, kinds, want, page.Total, page.Counts, len(page.Items), err)
					}
					if kinds[0] != "Pod" && page.Items[0].Type != "coverage_gap" {
						t.Fatalf("%s unauthorized read returned %s", issue.Key, page.Items[0].Type)
					}
				}
			}
			// Availability comes from the version read model, so a scope still
			// converting to it answers not-ready rather than guessing.
			if postgres, ok := provider.(*postgresProvider); ok {
				key, _ := evidenceScopeKey(scope)
				if _, err := postgres.db.Exec(`UPDATE kubernetes_evidence_scopes SET read_model_ready=FALSE WHERE scope_key=$1`, key); err != nil {
					t.Fatal(err)
				}
				query := window
				query.PointInTime, query.OccurrenceID = true, all.Items[0].ID
				if _, err := store.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceNotReady) {
					t.Fatalf("converting scope served a point-in-time read: %v", err)
				}
				if _, err := postgres.db.Exec(`UPDATE kubernetes_evidence_scopes SET read_model_ready=TRUE WHERE scope_key=$1`, key); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
