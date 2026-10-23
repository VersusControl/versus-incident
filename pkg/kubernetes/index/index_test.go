package index

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPartialSyncNeverDeletesUnseenRecords(t *testing.T) {
	index := New(10)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if err := index.Ingest("Pod", []Record{pod("uid-a", "a"), pod("uid-b", "b")}, true, now); err != nil {
		t.Fatal(err)
	}
	if err := index.Ingest("Pod", []Record{pod("uid-a", "a")}, false, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	snapshot := index.Snapshot()
	if len(snapshot.Records) != 2 || !snapshot.Kinds["Pod"].Partial || snapshot.Kinds["Pod"].State != "partial" {
		t.Fatalf("partial snapshot records=%d status=%#v", len(snapshot.Records), snapshot.Kinds["Pod"])
	}
	if err := index.Ingest("Pod", []Record{pod("uid-a", "a")}, true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if snapshot := index.Snapshot(); len(snapshot.Records) != 1 || snapshot.Records[0].UID != "uid-a" {
		t.Fatalf("complete snapshot=%#v", snapshot.Records)
	}
}

func TestRefreshCompletionBoundsChildEvidence(t *testing.T) {
	index := New(10)
	started := time.Now().UTC().Add(-time.Minute)
	var loadedAt time.Time
	if err := index.Refresh(t.Context(), "Pod", 0, started, func(context.Context) ([]Record, bool, error) {
		loadedAt = time.Now().UTC()
		return []Record{pod("uid-a", "a")}, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	first := index.Snapshot().Kinds["Pod"]
	if !first.ObservedAt.Equal(started) || first.CompletedAt.Before(loadedAt) || first.CompletedAt.After(time.Now().UTC()) {
		t.Fatalf("LIST start and completion must bound evidence: %+v", first)
	}
	// Failed loads publish no new evidence and must not advance completion.
	if err := index.Refresh(t.Context(), "Pod", 0, time.Now().UTC(), func(context.Context) ([]Record, bool, error) {
		return nil, false, errors.New("unavailable")
	}); err == nil {
		t.Fatal("failed refresh succeeded")
	}
	if got := index.Snapshot().Kinds["Pod"]; !got.CompletedAt.Equal(first.CompletedAt) {
		t.Fatalf("failed refresh advanced evidence: %+v", got)
	}
	// Partial loads can publish new child records even though ObservedAt
	// continues to describe the last complete observation.
	partialAt := time.Now().UTC()
	if err := index.Ingest("Pod", []Record{pod("uid-b", "b")}, false, partialAt); err != nil {
		t.Fatal(err)
	}
	got := index.Snapshot().Kinds["Pod"]
	if !got.Partial || !got.ObservedAt.Equal(started) || got.CompletedAt.Before(partialAt) || !got.CompletedAt.After(first.CompletedAt) {
		t.Fatalf("partial load lost current evidence bound: %+v", got)
	}
}

func TestOwnerObservationBoundarySurvivesRelistsAndResetsWithIdentity(t *testing.T) {
	index := New(1)
	record := pod("uid-a", "a")
	record.Owners = []OwnerRef{{Kind: "ReplicaSet", UID: "owner-a", Name: "owner-a", Controller: true, ControllerKnown: true}}
	ingest := func(complete bool) Record {
		t.Helper()
		if err := index.Ingest("Pod", []Record{record}, complete, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		return index.Snapshot().Records[0]
	}
	first := ingest(true)
	if first.OwnerObservedAt.IsZero() {
		t.Fatal("missing initial boundary")
	}
	// Neither caller stamps nor refreshed status can reset an unchanged owner.
	record.OwnerObservedAt = first.OwnerObservedAt.Add(-time.Hour)
	record.Phase = "Failed"
	for _, complete := range []bool{false, true, true} {
		if got := ingest(complete); !got.OwnerObservedAt.Equal(first.OwnerObservedAt) {
			t.Fatalf("re-list moved boundary: %+v", got)
		}
	}
	copy := index.Snapshot()
	copy.Records[0].OwnerObservedAt = time.Time{}
	if index.Snapshot().Records[0].OwnerObservedAt.IsZero() {
		t.Fatal("snapshot mutated indexed boundary")
	}
	record.Owners[0].UID, record.Owners[0].Name = "owner-b", "owner-b"
	changed := ingest(true)
	if !changed.OwnerObservedAt.After(first.OwnerObservedAt) {
		t.Fatal("changed owner retained old boundary")
	}
	if err := index.Ingest("Pod", nil, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if len(index.Snapshot().Records) != 0 {
		t.Fatal("complete deletion retained record or boundary")
	}
	recreated := ingest(true)
	if !recreated.OwnerObservedAt.After(changed.OwnerObservedAt) {
		t.Fatal("recreated record inherited deleted boundary")
	}
	record.UID = "uid-b"
	if got := ingest(true); !got.OwnerObservedAt.After(recreated.OwnerObservedAt) {
		t.Fatal("new UID inherited previous record boundary")
	}
}

func TestSnapshotsAreDefensiveCopiesAndBounded(t *testing.T) {
	index := New(1)
	input := pod("uid-a", "a")
	input.Labels = map[string]string{"app": "checkout"}
	started := time.Now().UTC()
	input.LogContainers = []ContainerState{{Name: "app", Running: true, StartedAt: started}}
	if err := index.Ingest("Pod", []Record{input}, true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	snapshot := index.Snapshot()
	snapshot.Records[0].Labels["app"] = "changed"
	snapshot.Records[0].LogContainers[0].Name = "changed"
	input.LogContainers[0].StartedAt = time.Time{}
	if index.Snapshot().Records[0].Labels["app"] != "checkout" {
		t.Fatal("snapshot mutation changed index state")
	}
	retained := index.Snapshot().Records[0].LogContainers[0]
	if retained.Name != "app" || !retained.StartedAt.Equal(started) {
		t.Fatal("log container snapshots share caller memory")
	}
	invalid := pod("uid-overflow", "overflow")
	for i := 0; i < 65; i++ {
		invalid.LogContainers = append(invalid.LogContainers, ContainerState{Name: "app-" + strconv.Itoa(i)})
	}
	if err := index.Ingest("Pod", []Record{invalid}, true, time.Now()); !errors.Is(err, ErrInvalidRecord) {
		t.Fatal("unbounded log container catalog accepted", err)
	}
	if err := index.Ingest("Pod", []Record{pod("uid-b", "b"), pod("uid-c", "c")}, false, time.Time{}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("oversized input error=%v", err)
	}
	if len(index.Snapshot().Records) != 1 {
		t.Fatal("rejected input changed index state")
	}
}

func TestIndexDefaultsAndRecordSizeAreBounded(t *testing.T) {
	index := New(maxScopeRecords + 1)
	if index.maxRecords != defaultMaxRecords {
		t.Fatalf("record cap=%d, want %d", index.maxRecords, defaultMaxRecords)
	}
	if large := New(maxScopeRecords); large.maxRecords != 60000 {
		t.Fatal("scope cannot retain fifty thousand Pods and their controllers")
	}
	registry := NewRegistry(0, 0)
	if registry.maxScopes != defaultMaxScopes || registry.maxRecords != defaultMaxRecords {
		t.Fatalf("registry caps scopes=%d records=%d", registry.maxScopes, registry.maxRecords)
	}
	oversized := pod("uid-a", "a")
	oversized.Labels = make(map[string]string, maxLabels)
	for index := 0; index < maxLabels; index++ {
		oversized.Labels[strconv.Itoa(index)] = strings.Repeat("x", 256)
	}
	if err := index.Ingest("Pod", []Record{oversized}, true, time.Now()); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("oversized record error=%v", err)
	}
	if len(index.Snapshot().Records) != 0 {
		t.Fatal("oversized record changed index state")
	}
}

func TestContainerEvidenceSignalBoundsAndCopies(t *testing.T) {
	for _, signal := range []int32{-1, 0, 9, 64, 65} {
		store := New(10)
		record := pod("pod", "pod")
		record.Containers = []ContainerState{{Name: "setup", Init: true, Signal: signal, TerminatedReason: "Error", RestartCount: 3}}
		err := store.Ingest("Pod", []Record{record}, true, time.Now())
		if signal < 0 || signal > 64 {
			if !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("accepted signal %d", signal)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		record.Containers[0].Signal = 1
		snapshot := store.Snapshot()
		if snapshot.Records[0].Containers[0].Signal != signal || !snapshot.Records[0].Containers[0].Init {
			t.Fatal("input aliases index or init lost")
		}
		snapshot.Records[0].Containers[0].Signal = 2
		if store.Snapshot().Records[0].Containers[0].Signal != signal {
			t.Fatal("snapshot aliases index")
		}
	}
}

func TestRegistrySharesOnlyExactScope(t *testing.T) {
	registry := NewRegistry(2, 10)
	scope := Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "credential-a"}
	first, err := registry.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.ForScope(scope)
	if err != nil || first != second {
		t.Fatalf("same scope index pointers differ: %p %p err=%v", first, second, err)
	}
	other, err := registry.ForScope(Scope{OrgID: "org-b", ClusterID: scope.ClusterID, CredentialID: scope.CredentialID})
	if err != nil || other == first {
		t.Fatalf("different org shared index: %p %p err=%v", first, other, err)
	}
	if _, err := registry.ForScope(Scope{OrgID: "org-c", ClusterID: scope.ClusterID}); !errors.Is(err, ErrScopeLimit) {
		t.Fatalf("scope cap error=%v", err)
	}
}

func TestRefreshCoalescesPollingAndPreservesLastGoodRecords(t *testing.T) {
	index := New(10)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	started := make(chan struct{})
	resume := make(chan struct{})
	loader := func(context.Context) ([]Record, bool, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-resume
			return []Record{pod("uid-a", "a")}, true, nil
		}
		return nil, false, errors.New("unavailable")
	}
	first := make(chan error, 1)
	go func() { first <- index.Refresh(context.Background(), "Pod", time.Minute, now, loader) }()
	<-started
	second := make(chan error, 1)
	go func() { second <- index.Refresh(context.Background(), "Pod", time.Minute, now, loader) }()
	close(resume)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent refreshes called loader %d times", calls.Load())
	}
	if err := index.Refresh(context.Background(), "Pod", time.Minute, now.Add(30*time.Second), loader); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("poll interval did not suppress refresh: calls=%d", calls.Load())
	}
	if err := index.Refresh(context.Background(), "Pod", time.Minute, now.Add(2*time.Minute), loader); err == nil {
		t.Fatal("failed refresh unexpectedly succeeded")
	}
	snapshot := index.Snapshot()
	if len(snapshot.Records) != 1 || snapshot.Records[0].UID != "uid-a" {
		t.Fatalf("failed refresh discarded last-good records: %#v", snapshot.Records)
	}
	status := index.Status(now.Add(3 * time.Minute))
	if status.State != "error" || !status.Partial || status.AgeSeconds != 180 || status.Kinds["Pod"].Error != "sync_failed" {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestRefreshPartialDoesNotDeleteUnseenRecords(t *testing.T) {
	index := New(10)
	now := time.Now().UTC()
	if err := index.Refresh(context.Background(), "Pod", time.Minute, now, func(context.Context) ([]Record, bool, error) {
		return []Record{pod("uid-a", "a"), pod("uid-b", "b")}, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.Refresh(context.Background(), "Pod", 0, now.Add(time.Second), func(context.Context) ([]Record, bool, error) {
		return []Record{pod("uid-a", "a")}, false, nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := index.Snapshot()
	if len(snapshot.Records) != 2 || snapshot.Kinds["Pod"].State != "partial" || !snapshot.Kinds["Pod"].Partial {
		t.Fatalf("partial refresh altered last-good membership: %#v", snapshot)
	}
}

func TestRefreshGuardRejectsRetiredCommit(t *testing.T) {
	index := New(10)
	deltas, unsubscribe := index.Subscribe(1)
	defer unsubscribe()
	err := index.RefreshGuarded(t.Context(), "Pod", 0, time.Now(), func(context.Context) ([]Record, bool, error) {
		return []Record{pod("old", "old")}, true, nil
	}, func(commit func() error) error {
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) || len(index.Snapshot().Records) != 0 || len(deltas) != 0 {
		t.Fatal("retired load published records or deltas")
	}
	if err := index.Refresh(t.Context(), "Pod", time.Minute, time.Now(), func(context.Context) ([]Record, bool, error) {
		return []Record{pod("live", "live")}, true, nil
	}); err != nil || index.Snapshot().Records[0].UID != "live" {
		t.Fatal("retirement cancellation suppressed live retry")
	}
}

func TestSubscribeEmitsUpdatesAndDeletesOnlyAfterCompleteSync(t *testing.T) {
	index := New(10)
	if err := index.Ingest("Pod", []Record{pod("uid-a", "a"), pod("uid-b", "b")}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	deltas, unsubscribe := index.Subscribe(8)
	defer unsubscribe()
	updated := pod("uid-a", "a")
	updated.Phase = "Running"
	if err := index.Ingest("Pod", []Record{updated}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if delta := receiveDelta(t, deltas); delta.Op != "upsert" || delta.Old == nil || delta.New == nil || delta.New.Phase != "Running" {
		t.Fatalf("unexpected partial upsert delta: %#v", delta)
	}
	if len(deltas) != 0 {
		t.Fatal("partial sync emitted a delete for an unseen record")
	}
	if err := index.Ingest("Pod", []Record{updated}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if delta := receiveDelta(t, deltas); delta.Op != "delete" || delta.Old == nil || delta.Old.UID != "uid-b" {
		t.Fatalf("complete sync did not emit the expected delete: %#v", delta)
	}
}

func TestSubscribeSignalsResyncWhenConsumerFallsBehind(t *testing.T) {
	index := New(10)
	if err := index.Ingest("Pod", nil, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	deltas, unsubscribe := index.Subscribe(1)
	defer unsubscribe()
	if err := index.Ingest("Pod", []Record{pod("uid-a", "a"), pod("uid-b", "b")}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if delta := receiveDelta(t, deltas); !delta.Resync {
		t.Fatalf("slow consumer received stale delta instead of resync: %#v", delta)
	}
}

func TestSubscribeDoesNotEmitBeforeFirstCompleteSync(t *testing.T) {
	index := New(10)
	deltas, unsubscribe := index.Subscribe(8)
	defer unsubscribe()
	if err := index.Ingest("Pod", []Record{pod("uid-a", "a")}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 0 {
		t.Fatal("initial partial sync emitted a delta")
	}
	if err := index.Ingest("Pod", []Record{pod("uid-a", "a"), pod("uid-b", "b")}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 0 {
		t.Fatal("initial complete sync emitted a delta")
	}
	if err := index.Ingest("Pod", []Record{pod("uid-a", "a")}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if delta := receiveDelta(t, deltas); delta.Op != "delete" || delta.Old == nil || delta.Old.UID != "uid-b" {
		t.Fatalf("sync after initialization did not emit a delete: %#v", delta)
	}
}

func receiveDelta(t *testing.T, deltas <-chan Delta) Delta {
	t.Helper()
	select {
	case delta, open := <-deltas:
		if !open {
			t.Fatal("delta stream closed before the expected notification")
		}
		return delta
	default:
		t.Fatal("synchronous index update did not publish the expected delta")
		return Delta{}
	}
}

func pod(uid, name string) Record {
	return Record{UID: uid, Kind: "Pod", Namespace: "shop", Name: name}
}
