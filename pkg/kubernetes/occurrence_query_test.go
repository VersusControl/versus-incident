package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestHistoricalOccurrenceFreshnessAndPinnedRootTimelineLookup(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			service, store, scope := reviewHistoryService(t)
			if backend == "file" {
				provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				service.SetChangeStorage(provider)
				store = provider.(storage.EvidenceStore)
			}
			now := time.Now().UTC()
			opened := now.Add(-2 * time.Hour)
			recent := now.Add(-time.Minute)
			capture := func(id string, at time.Time, severity string) {
				cp, err := store.EvidenceCheckpoint(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				batch := storage.EvidenceBatch{ID: id, Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: "synthetic"}
				for i := 0; i < 13; i++ {
					uid := fmt.Sprintf("pod-%02d", i)
					raw, _ := json.Marshal(Issue{Root: ObjectRef{Kind: "Pod", Namespace: "shop", Name: uid, UID: uid}, Rule: "pod.crash_loop", Severity: severity, Broken: true, FirstSeen: opened, LastSeen: at})
					batch.Issues = append(batch.Issues, storage.EvidenceIssue{Key: uid, Kind: "Pod", Namespace: "shop", UID: uid, Rule: "pod.crash_loop", Broken: true, FirstSeen: opened, LastSeen: at, Payload: raw})
				}
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatal(err)
				}
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatal(err)
				}
			}
			capture("opened", opened, "warning")
			capture("heartbeat", recent, "warning")
			full, err := service.Issues(t.Context(), IssueOptions{Since: opened.Add(-time.Minute), Until: now, Namespace: "shop", Limit: 100})
			if err != nil || len(full.Items) != 13 {
				t.Fatalf("full window: count=%d err=%v", len(full.Items), err)
			}
			for _, issue := range full.Items {
				if !issue.FirstSeen.Equal(opened) || !issue.LastSeen.Equal(recent) {
					t.Fatalf("fresh occurrence missing lastSeen: %+v", issue)
				}
			}
			root, err := service.Issues(t.Context(), IssueOptions{Since: now.Add(-time.Hour), Until: now, Namespace: "shop", RootUID: "pod-00", IncludeChanges: true})
			if err != nil || len(root.Items) != 1 || !root.Items[0].LastSeen.Equal(recent) {
				t.Fatalf("root Timeline recent lookup: %+v err=%v", root.Items, err)
			}
			options := IssueOptions{Since: now.Add(-time.Hour), Until: now, Namespace: "shop", Severity: "warning", Broken: true, Limit: 10}
			first, err := service.Issues(t.Context(), options)
			if err != nil || len(first.Items) != 10 || first.Next == "" {
				t.Fatalf("recent first page: count=%d err=%v", len(first.Items), err)
			}
			options.Cursor = first.Next
			capture("changed", now.Add(-30*time.Second), "critical")
			second, err := service.Issues(t.Context(), options)
			if err != nil || len(second.Items) != 3 || second.Next != "" {
				t.Fatalf("pinned continuation: count=%d err=%v", len(second.Items), err)
			}
			seen := map[string]bool{}
			for _, issue := range append(first.Items, second.Items...) {
				if seen[issue.OccurrenceID] || issue.Severity != "warning" || !issue.LastSeen.Equal(recent) {
					t.Fatalf("continuation observed later capture or duplicate: %+v", issue)
				}
				seen[issue.OccurrenceID] = true
			}
			latest, err := service.Issues(t.Context(), IssueOptions{Since: now.Add(-time.Hour), Until: now, Namespace: "shop", Severity: "critical", Limit: 100})
			if err != nil || len(latest.Items) != 13 {
				t.Fatalf("fresh snapshot missed change: %d %v", len(latest.Items), err)
			}
			journal, err := store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Since: opened.Add(-time.Minute), Until: now, Type: "issue"})
			if err != nil || journal.Total != 26 {
				t.Fatalf("heartbeat duplicated full journal: total=%d err=%v", journal.Total, err)
			}
			if err = store.DeleteEvidence(t.Context(), scope); err != nil {
				t.Fatal(err)
			}
			capture("after-delete", now.Add(-10*time.Second), "warning")
			if _, err = service.Issues(t.Context(), options); !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("deleted snapshot cursor survived: %v", err)
			}
		})
	}
}

func TestJournalReadsRequireCurrentCaptureBatches(t *testing.T) {
	service, store, scope := reviewHistoryService(t)
	at := time.Now().UTC().Add(-time.Hour)
	payload, _ := json.Marshal(Issue{Root: ObjectRef{Kind: "Pod", UID: "pod", Namespace: "shop"}, Rule: "pod.crash_loop", Severity: "warning"})
	batch := storage.EvidenceBatch{ID: "persisted-unmarked", At: at, Epoch: "synthetic", Issues: []storage.EvidenceIssue{{Key: "key", Kind: "Pod", UID: "pod", Namespace: "shop", Rule: "pod.crash_loop", Payload: payload}}}
	if err := service.commitEvidence(t.Context(), inventoryGeneration{EvidenceScope: scope, EvidenceBatch: &batch}); err != nil {
		t.Fatal(err)
	}
	options := IssueOptions{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Namespace: "shop"}
	empty, err := service.Issues(t.Context(), options)
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("unmarked persisted batch became history: %+v %v", empty.Items, err)
	}
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil || checkpoint.ID != "" {
		t.Fatal("unmarked batch advanced capture checkpoint")
	}
	batch.ID = "capture:current"
	batch.At = at.Add(time.Minute)
	if err = service.commitEvidence(t.Context(), inventoryGeneration{EvidenceScope: scope, EvidenceBatch: &batch}); err != nil {
		t.Fatal(err)
	}
	if err = service.commitEvidence(t.Context(), inventoryGeneration{EvidenceScope: scope, EvidenceBatch: &batch}); err != nil {
		t.Fatal(err)
	}
	fresh, err := service.Issues(t.Context(), options)
	if err != nil || len(fresh.Items) != 1 || !fresh.Items[0].FirstSeen.Equal(batch.At) {
		t.Fatalf("current capture was not initialized independently: %+v %v", fresh.Items, err)
	}
}

func TestInventoryCycleAuthorityClassesSurviveClassification(t *testing.T) {
	for _, class := range []string{"forbidden", "unauthorized"} {
		previous := map[string]kubeindex.KindStatus{"Pod": {State: "error", Error: "timeout", ObservedAt: time.Now().UTC()}}
		if got := failedInventoryKinds(previous, class)["Pod"]; got.Error != class || !got.CycleFailure {
			t.Errorf("known authority failure lost to old status: %+v", got)
		}
	}

	for _, class := range []string{"forbidden", "unauthorized", "timeout", "unavailable", "unsupported"} {
		err := indexReadError{error: inventorySyncError(), class: class}
		if got := errorClass(err); got != class {
			t.Errorf("typed cycle class=%s became=%s", class, got)
		}
	}
}

func TestInventoryCycleAuthorityFaultOutranksSnapshotReadiness(t *testing.T) {
	for _, class := range []string{"forbidden", "unauthorized"} {
		for _, cycle := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cycle-%t", class, cycle), func(t *testing.T) {
				service, _, _ := reviewHistoryService(t)
				now := time.Now().UTC()
				generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{
					"Pod":        {State: "error", Error: class, CycleFailure: cycle, ObservedAt: now},
					"Deployment": {State: "ready", ObservedAt: now},
				}}}
				service.changes.mu.Lock()
				service.changes.inventories[service.indexScope()] = &inventoryCache{generation: generation}
				service.changes.mu.Unlock()
				_, _, err := service.CachedNamespaceSnapshot(t.Context(), "", "Pod", "Deployment")
				if !cycle {
					if err != nil {
						t.Fatalf("per-kind capability gap became whole-cycle denial: %v", err)
					}
					return
				}
				expected := ErrForbidden
				if class == "unauthorized" {
					expected = ErrUnauthorized
				}
				if !errors.Is(err, expected) {
					t.Fatalf("known cycle authority fault became success/warming: %v", err)
				}
			})
		}
	}
}
