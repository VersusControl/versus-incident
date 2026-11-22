package kubernetes

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// transactionStore saves a capture in several timed transactions, confirming
// the lease before each through KeepLease as the Postgres store does. Lease
// writes can be made to fail.
type transactionStore struct {
	storage.Provider
	transactions int
	each         time.Duration
	saved        atomic.Int32
	failLease    atomic.Bool
}

func (store *transactionStore) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	if store.failLease.Load() {
		return false, errors.New("database unavailable")
	}
	return store.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

func (store *transactionStore) SetEvidenceAuthority(context.Context, storage.EvidenceScope, string, map[string]storage.EvidenceAuthority) error {
	return nil
}

func (store *transactionStore) EvidencePolicy() storage.EvidencePolicy {
	return storage.EvidencePolicy{Mode: "durable"}
}

func (store *transactionStore) CommitEvidence(ctx context.Context, _ storage.EvidenceScope, batch storage.EvidenceBatch) error {
	for range store.transactions {
		if batch.KeepLease != nil {
			if _, err := batch.KeepLease(ctx); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(store.each):
		}
		store.saved.Add(1)
	}
	return nil
}

func (store *transactionStore) QueryEvidence(context.Context, storage.EvidenceScope, storage.EvidenceQuery) (storage.EvidencePage, error) {
	return storage.EvidencePage{}, nil
}

func (store *transactionStore) EvidenceCheckpoint(context.Context, storage.EvidenceScope) (storage.EvidenceCheckpoint, error) {
	return storage.EvidenceCheckpoint{}, nil
}

func (store *transactionStore) DeleteEvidence(context.Context, storage.EvidenceScope) error {
	return nil
}

// journalLease claims a short lease on store for a service that saves
// history there, and returns a generation holding a capture to save.
func journalLease(t *testing.T, store *transactionStore, ttl time.Duration) (*Service, *inventoryLease, inventoryGeneration) {
	t.Helper()
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "cluster"})
	service.leaseTTL = ttl
	service.SetChangeStorage(store)
	service.changes.mu.Lock()
	service.changes.evidenceEnabled = true
	service.changes.mu.Unlock()
	lease, owned, err := service.claimInventory(t.Context(), store, "owner")
	if err != nil || !owned {
		t.Fatalf("claim: owned=%t %v", owned, err)
	}
	scope := storage.EvidenceScope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, ClusterUID: "physical"}
	generation := inventoryGeneration{Revision: "capture", EvidenceScope: scope, EvidenceBatch: &storage.EvidenceBatch{ID: "capture:test"}}
	return service, lease, generation
}

// A save that renews its lease between transactions goes on past the
// expiry it started with.
func TestHistorySaveContinuesPastItsStartingExpiryWhileTheLeaseRenews(t *testing.T) {
	ttl := 1200 * time.Millisecond
	store := &transactionStore{Provider: storage.NewMemory(), transactions: 8, each: 300 * time.Millisecond}
	service, lease, generation := journalLease(t, store, ttl)
	startingExpiry := lease.expiresAt()
	if err := lease.commitJournal(t.Context(), service, generation); err != nil {
		t.Fatalf("a save that kept its lease failed after %d transactions: %v", store.saved.Load(), err)
	}
	if store.saved.Load() != 8 || !time.Now().After(startingExpiry) {
		t.Fatalf("saved %d of 8 transactions, ended %s past the starting expiry", store.saved.Load(), time.Since(startingExpiry))
	}
	if !lease.expiresAt().After(startingExpiry) {
		t.Fatal("the save did not renew its lease")
	}
}

// A save stops once it can no longer keep its lease: when a renewal between
// transactions fails, and when one transaction outlasts the lease.
func TestHistorySaveStopsWhenItCannotKeepItsLease(t *testing.T) {
	ttl := 1200 * time.Millisecond
	t.Run("renewal fails near expiry", func(t *testing.T) {
		store := &transactionStore{Provider: storage.NewMemory(), transactions: 8, each: 300 * time.Millisecond}
		service, lease, generation := journalLease(t, store, ttl)
		store.failLease.Store(true)
		err := lease.commitJournal(t.Context(), service, generation)
		if err == nil || store.saved.Load() >= 8 {
			t.Fatalf("a save whose lease renewal failed went on: saved %d, err %v", store.saved.Load(), err)
		}
		if store.saved.Load() == 0 {
			t.Fatal("the save stopped before it needed a renewal")
		}
	})
	t.Run("transaction outlasts the lease", func(t *testing.T) {
		store := &transactionStore{Provider: storage.NewMemory(), transactions: 1, each: 3 * ttl}
		service, lease, generation := journalLease(t, store, ttl)
		started := time.Now()
		err := lease.commitJournal(t.Context(), service, generation)
		if !errors.Is(err, context.Canceled) || store.saved.Load() != 0 {
			t.Fatalf("a transaction past the lease expiry was not cut: saved %d, err %v", store.saved.Load(), err)
		}
		if took := time.Since(started); took > scaledTimeout(ttl+time.Second) {
			t.Fatalf("the save was cut %s after it started, want at the lease expiry", took)
		}
	})
}
