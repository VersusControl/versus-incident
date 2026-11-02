package changes

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestLegacyCaptureBlobsAndEncodedSizeBoundary(t *testing.T) {
	for _, raw := range []string{``, `{}`, `{"changes":[],"gaps":[]}`, `{"changes":[],"break_logs":{"snapshots":[],"gaps":[],"jobs":{}}}`} {
		t.Run(fmt.Sprintf("schema-%d", len(raw)), func(t *testing.T) {
			current, err := decodeSnapshot([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if err := breaklogs.Merge(&current.BreakLogs, nil, nil, "", time.Now().UTC()); err != nil {
				t.Fatal("legacy missing manifest fields rejected", err)
			}
		})
	}
	raw := make([]byte, maxStoreBytes)
	for i := range raw {
		raw[i] = ' '
	}
	copy(raw, `{}`)
	if _, err := decodeSnapshot(raw); err != nil {
		t.Fatal("exact encoded cap rejected", err)
	}
	if _, err := decodeSnapshot(append(raw, ' ')); !errors.Is(err, ErrStoreLimit) {
		t.Fatal("encoded cap+1 accepted", err)
	}
	if _, err := decodeSnapshot([]byte(`{"break_logs":`)); !errors.Is(err, ErrStoreLimit) {
		t.Fatal("malformed old blob accepted", err)
	}
}

func acceptanceSettleStore(t *testing.T) (*Store, breaklogs.Manifest, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	store := NewStore(storage.NewMemory(), index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "credential"})
	if err := store.ClaimLease("owner", now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	manifest := breaklogs.Manifest{ID: "selected", Namespace: "shop", RootUID: "root", BreakAt: now, From: now.Add(-time.Minute), To: now, CapturedAt: now, SettleAt: now}
	if err := store.CommitBreakLogs(nil, nil, "", "owner", now, manifest); err != nil {
		t.Fatal(err)
	}
	return store, manifest, now
}

func TestConcurrentDuplicateSettleClaimAndCompletion(t *testing.T) {
	store, manifest, now := acceptanceSettleStore(t)
	var claimed breaklogs.Manifest
	var mu sync.Mutex
	var successes atomic.Int32
	var running sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		peer := NewStore(store.provider, index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "credential"})
		running.Add(1)
		go func() {
			defer running.Done()
			<-start
			claim, err := peer.ClaimBreakSettle(manifest.ID, "owner", now)
			if err == nil {
				successes.Add(1)
				mu.Lock()
				claimed = claim
				mu.Unlock()
			} else if !errors.Is(err, ErrStoreLimit) {
				t.Error(err)
			}
		}()
	}
	close(start)
	running.Wait()
	if successes.Load() != 1 || claimed.Attempts != 1 {
		t.Fatalf("duplicate claims consumed attempts: successes=%d claim=%+v", successes.Load(), claimed)
	}
	successes.Store(0)
	for i := 0; i < 12; i++ {
		peer := NewStore(store.provider, index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "credential"})
		running.Add(1)
		go func() {
			defer running.Done()
			if err := peer.CommitBreakSettle(claimed, nil, false, "owner", now); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrLeaseLost) {
				t.Error(err)
			}
		}()
	}
	running.Wait()
	if successes.Load() != 1 {
		t.Fatalf("duplicate completion accepted %d times", successes.Load())
	}
	state, err := store.BreakLogs(now)
	if err != nil || state.Manifests[0].Attempts != 1 || !state.Manifests[0].Done {
		t.Fatal(state, err)
	}
}

func TestSettleCompletionRejectsExpiredAttemptLease(t *testing.T) {
	store, manifest, now := acceptanceSettleStore(t)
	claim, err := store.ClaimBreakSettle(manifest.ID, "owner", now)
	if err != nil {
		t.Fatal(err)
	}
	// Capture ownership remains valid, but the admission/attempt lease is over.
	// A late request must not commit after another query can take its slot.
	err = store.CommitBreakSettle(claim, nil, false, "owner", claim.SettlingUntil)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired attempt committed at %s: %v", claim.SettlingUntil, err)
	}
	state, err := store.BreakLogs(now)
	if err != nil || state.Manifests[0].Done {
		t.Fatal("expired completion changed persisted progress", state, err)
	}
}

func TestSharedBreakStateKeepsExactReplicaClocks(t *testing.T) {
	store, manifest, now := acceptanceSettleStore(t)
	ahead := now.Add(2 * time.Second)
	item := breaklogs.Snapshot{Cluster: "cluster", Namespace: manifest.Namespace, RootUID: manifest.RootUID, Pod: "api-0", PodUID: "old", Container: "app", Slot: "previous", CapturedAt: ahead, From: now.Add(-time.Minute), To: now,
		Lines: []breaklogs.Line{{At: now.Add(-time.Second), Text: "original failure"}}}
	item.ID = breaklogs.Identity(item)
	manifest.ID = "ahead"
	manifest.CapturedAt = ahead
	if err := store.CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "", "owner", ahead, manifest); err != nil {
		t.Fatal(err)
	}
	state, err := store.BreakLogs(now)
	if err != nil || len(state.Snapshots) != 1 || !state.Snapshots[0].CapturedAt.Equal(ahead) {
		t.Fatal("lagging reader rejected or rewrote replica timestamp", state, err)
	}
	next := manifest
	next.ID, next.CapturedAt = "lagging", now
	if err := store.CommitBreakLogs(nil, nil, "", "owner", now, next); err != nil {
		t.Fatal("lagging commit rejected shared state", err)
	}
	state, err = store.BreakLogs(now)
	if err != nil || len(state.Manifests) != 3 || state.Snapshots[0].Lines[0].Text != "original failure" {
		t.Fatal("lagging commit lost shared evidence", state, err)
	}
	for _, stored := range state.Manifests {
		if stored.ID == "ahead" && !stored.CapturedAt.Equal(ahead) {
			t.Fatal("replica manifest timestamp rewritten", stored)
		}
	}
	for _, offset := range []time.Duration{breaklogs.ClockTolerance, breaklogs.ClockTolerance + time.Nanosecond} {
		state := breaklogs.State{}
		item.CapturedAt = now.Add(offset)
		err := breaklogs.Merge(&state, []breaklogs.Snapshot{item}, nil, "", now)
		if (err == nil) != (offset == breaklogs.ClockTolerance) {
			t.Fatal("snapshot skew boundary", offset, err)
		}
		manifest.CapturedAt = item.CapturedAt
		err = breaklogs.MergeManifests(&breaklogs.State{}, []breaklogs.Manifest{manifest}, now)
		if (err == nil) != (offset == breaklogs.ClockTolerance) {
			t.Fatal("manifest skew boundary", offset, err)
		}
	}
}

func TestDeploymentSlotRaceAndExactExpiry(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Now().UTC()
	var successes atomic.Int32
	var running sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		store := NewStore(provider, index.Scope{OrgID: "default", ClusterID: fmt.Sprint(i), CredentialID: "credential"})
		running.Add(1)
		go func(i int) {
			defer running.Done()
			<-start
			if err := store.AcquireBreakSettleSlot(fmt.Sprint(i), now); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrStoreLimit) {
				t.Error(err)
			}
		}(i)
	}
	close(start)
	running.Wait()
	if successes.Load() != 2 {
		t.Fatalf("shared CAS admitted %d queries", successes.Load())
	}
	store := NewStore(provider, index.Scope{OrgID: "default", ClusterID: "other", CredentialID: "credential"})
	if err := store.AcquireBreakSettleSlot("too-early", now.Add(30*time.Second-time.Nanosecond)); !errors.Is(err, ErrStoreLimit) {
		t.Fatal("slot expired early", err)
	}
	if err := store.AcquireBreakSettleSlot("at-expiry", now.Add(30*time.Second)); err != nil {
		t.Fatal("expired slots blocked recovery", err)
	}
	raw, err := provider.ReadBlob(breakSettleAdmissionKey)
	var slots []breakSettleSlot
	if err != nil || json.Unmarshal(raw, &slots) != nil || len(slots) != 1 || slots[0].Token != "at-expiry" {
		t.Fatal("expired slots retained", string(raw), err)
	}
}
