package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func timelineAdmissionFixture(t *testing.T) (*Service, storage.Provider, TimelineQuery) {
	t.Helper()
	provider := storage.NewMemory()
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(provider)
	seedInventory(t, service, nil)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		item := Change{ID: "change-" + strconv.Itoa(i), UID: "pod", Cluster: "cluster", Kind: "Pod", Name: "pod", Namespace: "shop", Type: kubechanges.Created, At: now.Add(-time.Duration(i+1) * time.Minute)}
		if err := service.changeStore().Append([]Change{item}, now); err != nil {
			t.Fatal(err)
		}
	}
	return service, provider, TimelineQuery{Since: now.Add(-time.Hour), Until: now, Limit: 1}
}

func TestTimelineFirstPagePollingDoesNotReserveEmptyOrSinglePages(t *testing.T) {
	for _, scenario := range []string{"single-page", "syncing", "empty"} {
		service, provider, query := timelineAdmissionFixture(t)
		if scenario == "syncing" {
			service.changes.inventories[service.indexScope()].generation.View.Kinds = nil
		}
		if scenario == "empty" {
			provider = storage.NewMemory()
			service.SetChangeStorage(provider)
		}
		query.Limit = 200
		for i := 0; i < timelineSnapshotSlots+16; i++ {
			query.Until = query.Until.Add(time.Nanosecond)
			page, err := service.Timeline(t.Context(), query)
			if err != nil || page.Next != "" {
				t.Fatalf("poll %d scenario=%s: next=%q err=%v", i, scenario, page.Next, err)
			}
			if scenario == "empty" && page.Total != 0 {
				t.Fatal("empty polling fixture unexpectedly contains data")
			}
		}
		raw, err := provider.ReadBlob(service.timelineKey(0) + "/index")
		if err != nil || len(raw) != 0 {
			t.Fatalf("non-paginated polling reserved shared capacity: bytes=%d err=%v", len(raw), err)
		}
	}
}

func TestTimelineEquivalentReadsReuseAndRequesterFloodLeavesCapacity(t *testing.T) {
	service, _, query := timelineAdmissionFixture(t)
	alice := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "alice"})
	bob := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "bob"})
	var first string
	for i := 0; i < timelineSnapshotSlots+16; i++ {
		page, err := service.Timeline(alice, query)
		if err != nil {
			t.Fatalf("equivalent read %d exhausted slots: %v", i, err)
		}
		if first == "" {
			first = page.Next
		}
		if page.Next != first {
			t.Fatal("equivalent scoped snapshot was not reused")
		}
	}
	for i := 0; i < timelineSnapshotSlots+16; i++ {
		query.Until = query.Until.Add(time.Nanosecond)
		_, err := service.Timeline(alice, query)
		if err != nil && err != ErrIndexStreamBusy {
			t.Fatal(err)
		}
	}
	page, err := service.Timeline(bob, query)
	if err != nil || page.Next == "" {
		t.Fatalf("one requester starved another: %+v %v", page, err)
	}
}

func TestInventoryRetiredOwnersDoNotAccumulateDataSlots(t *testing.T) {
	provider := storage.NewMemory()
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now()}}, Records: []kubeindex.Record{{UID: "pod", Kind: "Pod", Name: "pod", Namespace: "shop"}}}}
	for i := 0; i < 6; i++ {
		if i > 0 {
			raw, _ := provider.ReadBlob(inventoryKey(service.ActiveScope()))
			var record inventoryLeaseRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			record.Expires = time.Now().Add(-time.Second)
			raw, _ = json.Marshal(record)
			if err := provider.WriteBlob(inventoryKey(service.ActiveScope()), raw); err != nil {
				t.Fatal(err)
			}
		}
		lease, owned, err := service.claimInventory(t.Context(), provider, "owner-"+strconv.Itoa(i))
		if err != nil || !owned {
			t.Fatalf("claim %d: %v %v", i, owned, err)
		}
		if err := lease.publish(t.Context(), service, &generation); err != nil {
			t.Fatal(err)
		}
		rows, err := provider.ListBlobs(inventoryKey(service.ActiveScope()) + "/data/")
		if err != nil || len(rows) > 2 {
			t.Fatalf("owner %d orphaned full inventories: blobs=%d err=%v", i, len(rows), err)
		}
		got, _, err := readInventoryGeneration(provider, service.ActiveScope(), "")
		if err != nil || len(got.View.Records) != 1 {
			t.Fatalf("GC lost last-good: %+v %v", got, err)
		}
	}
}

func TestNamespaceRequestHintsAreBoundedCandidatesNotPermanentInventory(t *testing.T) {
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	for i := 0; i < 256; i++ {
		service.queueInventoryNamespace("missing-" + strconv.Itoa(i))
	}
	cache := service.changes.inventories[service.indexScope()]
	cache.mu.RLock()
	polling := len(cache.namespaces)
	cache.mu.RUnlock()
	if polling != 0 {
		t.Fatalf("%d unverified request hints permanently entered inventory polling", polling)
	}
}

func TestInventoryPermanentPayloadFaultRecollectsOnlyAfterFencedClaim(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(strconv.FormatBool(missing), func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				items := []any{}
				if r.URL.Path == "/api/v1/pods" {
					items = append(items, map[string]any{"metadata": map[string]any{"uid": "recovered", "name": "recovered", "namespace": "shop"}})
				}
				writeJSON(w, map[string]any{"items": items})
			}))
			defer server.Close()
			provider := storage.NewMemory()
			service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
			seedInventoryDiscovery(service)
			service.SetChangeStorage(provider)
			lease, _, err := service.claimInventory(t.Context(), provider, "retired")
			if err != nil {
				t.Fatal(err)
			}
			generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now().Add(-time.Minute)}}, Records: []kubeindex.Record{{UID: "old", Name: "old", Kind: "Pod", Namespace: "shop"}}}}
			if err := lease.publish(t.Context(), service, &generation); err != nil {
				t.Fatal(err)
			}
			key, _ := inventoryDataKey(service.ActiveScope(), lease.record)
			if missing {
				raw, _ := provider.ReadBlob(key)
				if _, err := provider.(storage.BlobCAS).CompareAndSwapBlob(key, raw, nil); err != nil {
					t.Fatal(err)
				}
			} else if err := provider.WriteBlob(key, []byte("{")); err != nil {
				t.Fatal(err)
			}
			service.startInventory(t.Context())
			defer service.StopCapture()
			time.Sleep(50 * time.Millisecond)
			if reads.Load() != 0 {
				t.Fatal("corrupt payload bypassed an active owner's fence")
			}
			record := lease.record
			record.Expires = time.Now().Add(-time.Second)
			raw, _ := json.Marshal(record)
			if err := provider.WriteBlob(inventoryKey(service.ActiveScope()), raw); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				view, status, err := service.CachedIndexSnapshot(context.Background(), "Pod")
				if err == nil && status.State == "ready" && len(view.Records) == 1 && view.Records[0].UID == "recovered" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("permanent payload fault prevented fenced re-collection: %+v %v", status, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
