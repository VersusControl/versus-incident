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

func TestSnapshotsAreDefensiveCopiesAndBounded(t *testing.T) {
	index := New(1)
	input := pod("uid-a", "a")
	input.Labels = map[string]string{"app": "checkout"}
	if err := index.Ingest("Pod", []Record{input}, true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	snapshot := index.Snapshot()
	snapshot.Records[0].Labels["app"] = "changed"
	if index.Snapshot().Records[0].Labels["app"] != "checkout" {
		t.Fatal("snapshot mutation changed index state")
	}
	if err := index.Ingest("Pod", []Record{pod("uid-b", "b"), pod("uid-c", "c")}, false, time.Time{}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("oversized input error=%v", err)
	}
	if len(index.Snapshot().Records) != 1 {
		t.Fatal("rejected input changed index state")
	}
}

func TestIndexDefaultsAndRecordSizeAreBounded(t *testing.T) {
	index := New(defaultMaxRecords + 1)
	if index.maxRecords != defaultMaxRecords {
		t.Fatalf("record cap=%d, want %d", index.maxRecords, defaultMaxRecords)
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
