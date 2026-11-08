package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// partedSaveStore saves a capture the way a store does that writes it in
// several transactions: each transaction runs inside the caller's fence, and
// between returns control between two of them.
type partedSaveStore struct {
	storage.Provider
	storage.EvidenceStore
	storage.BlobCAS
	between func()
	parts   int
}

func (store *partedSaveStore) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	if batch.Fence == nil {
		return store.EvidenceStore.CommitEvidence(ctx, scope, batch)
	}
	for part := 0; part < 3; part++ {
		if err := batch.Fence(ctx, func() error {
			store.parts++
			time.Sleep(20 * time.Millisecond)
			return nil
		}); err != nil {
			return err
		}
		if part == 1 && store.between != nil {
			store.between()
		}
	}
	final := batch
	final.Fence = nil
	return batch.Fence(ctx, func() error { return store.EvidenceStore.CommitEvidence(ctx, scope, final) })
}

// A large capture is saved in several transactions. The credential fence is
// held for one transaction at a time, so a live read issued between two of
// them completes before the save finishes instead of waiting for all of it.
func TestLiveReadsRunBetweenThePartsOfALargeHistorySave(t *testing.T) {
	t.Parallel()
	base := storage.NewMemory()
	t.Cleanup(func() { _ = base.Close() })
	store := &partedSaveStore{Provider: base, EvidenceStore: base.(storage.EvidenceStore), BlobCAS: base.(storage.BlobCAS)}
	service := issueHistoryService(t, store)
	service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return "Bearer live", nil }}
	now := time.Now().UTC()
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:fence-base", At: now.Add(-time.Minute), Epoch: "fixture"})
	seedInventory(t, service, []kubeindex.Record{brokenDeploymentRecord("web", "web", now.Add(-time.Minute))})
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	checkpoint, err := store.EvidenceStore.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	var liveFinished time.Time
	var liveErr error
	store.between = func() {
		done := make(chan error, 1)
		go func() {
			_, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop"})
			done <- err
		}()
		select {
		case liveErr = <-done:
			liveFinished = time.Now()
		case <-time.After(5 * time.Second):
			t.Error("a live read waited behind a history save that was between two of its transactions")
		}
	}
	generation := inventoryGeneration{EvidenceScope: scope, EvidenceBatch: &storage.EvidenceBatch{ID: "capture:fence-parts", Expected: checkpoint.ID, Generation: checkpoint.Generation, Principal: checkpoint.Principal, At: now, Epoch: "fixture"}}
	store.parts = 0
	if err := service.commitEvidence(t.Context(), generation); err != nil {
		t.Fatalf("parted save: %v (%v)", err, errors.Unwrap(err))
	}
	saveFinished := time.Now()
	if liveErr != nil {
		t.Fatalf("the live read during the save failed: %v", liveErr)
	}
	if liveFinished.IsZero() || !liveFinished.Before(saveFinished) {
		t.Fatal("the live read did not complete before the save finished")
	}
	if store.parts != 3 {
		t.Fatalf("the save ran %d fenced parts, want 3", store.parts)
	}
	if latest, err := store.EvidenceStore.EvidenceCheckpoint(t.Context(), scope); err != nil || latest.ID != "capture:fence-parts" {
		t.Fatalf("checkpoint after the save: %q %v", latest.ID, err)
	}
}
