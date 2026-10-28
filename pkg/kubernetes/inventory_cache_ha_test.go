package kubernetes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func seedInventoryDiscovery(service *Service) {
	resources := []ResourceDefinition{}
	for kind, id := range indexedResourceIDs {
		parts := strings.Split(id, "~")
		group := parts[0]
		if group == "core" {
			group = ""
		}
		resources = append(resources, ResourceDefinition{ID: id, Kind: kind, Group: group, Version: parts[1], Resource: parts[2], Namespaced: kind != "Node" && kind != "Namespace", Available: true})
	}
	service.cache[service.cacheKey()] = discoveryCacheEntry{discovery: Discovery{Resources: resources}, expires: time.Now().Add(time.Hour)}
	service.client.indexLimiter = nil
}

func TestInventoryReplicasShareOwnerGeneration(t *testing.T) {
	var podLists atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		items := []any{}
		if r.URL.Path == "/api/v1/pods" {
			podLists.Add(1)
			items = append(items, map[string]any{"metadata": map[string]any{"uid": "pod", "name": "pod", "namespace": "shop"}})
		}
		writeJSON(w, map[string]any{"items": items})
	}))
	defer server.Close()
	provider := storage.NewMemory()
	first := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	second := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	for _, service := range []*Service{first, second} {
		seedInventoryDiscovery(service)
		service.SetChangeStorage(provider)
		t.Cleanup(service.StopCapture)
	}
	first.startInventory(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for {
		view, status, err := first.CachedIndexSnapshot(t.Context(), "Pod")
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "ready" && len(view.Records) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first sync did not complete: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	second.startInventory(t.Context())
	var view kubeindex.Snapshot
	var status kubeindex.Status
	var err error
	deadline = time.Now().Add(3 * time.Second)
	for {
		view, status, err = second.CachedIndexSnapshot(t.Context(), "Pod")
		if err != nil || status.State == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica background hydration did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || status.State != "ready" || len(view.Records) != 1 || view.Records[0].UID != "pod" {
		t.Fatalf("replica did not consume scoped generation: %+v %+v %v", view, status, err)
	}
	if podLists.Load() != 1 {
		t.Fatalf("replicas performed %d Pod LISTs", podLists.Load())
	}
}

func TestInventoryUpgradeSyncFailureRetainsArchiveGeneration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer server.Close()
	provider := storage.NewMemory()
	service := newTestService(t, server.URL, Scope{OrgID: "licensed", ClusterID: "cluster"})
	seedInventoryDiscovery(service)
	service.readOrgs = []string{"licensed", "default"}
	service.SetChangeStorage(provider)
	defer service.StopCapture()
	archiveScope := service.ActiveScope()
	archiveScope.OrgID = "default"
	baseline := inventoryGeneration{View: kubeindex.Snapshot{Records: []kubeindex.Record{{UID: "old-pod", Kind: "Pod", Namespace: "shop", Name: "old-pod"}}, Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now().UTC().Add(-2 * time.Minute), Records: 1}}}}
	raw, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.WriteBlob(inventoryKey(archiveScope), raw); err != nil {
		t.Fatal(err)
	}
	service.startInventory(t.Context())
	deadline := time.Now().Add(8 * time.Second)
	for {
		view, status, err := service.CachedIndexSnapshot(t.Context(), "Pod")
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "error" {
			if len(view.Records) != 1 || view.Records[0].UID != "old-pod" {
				t.Fatalf("upgrade failure replaced last-good: %+v", view)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed sync did not publish truthful status: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, err := provider.ReadBlob(inventoryKey(archiveScope))
	if err != nil || string(after) != string(raw) {
		t.Fatal("upgrade mutated archive")
	}
}

type inventoryCASAttempt struct {
	won bool
	err error
}
type inventoryCASProbe struct {
	storage.Provider
	cas      storage.BlobCAS
	attempts chan inventoryCASAttempt
}

func (probe *inventoryCASProbe) CompareAndSwapBlob(key string, expected, replacement []byte) (bool, error) {
	won, err := probe.cas.CompareAndSwapBlob(key, expected, replacement)
	probe.attempts <- inventoryCASAttempt{won, err}
	return won, err
}

func TestInventoryExpiredOwnerCannotPublishOverSuccessor(t *testing.T) {
	podRead, releasePod := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/pods" {
			close(podRead)
			select {
			case <-releasePod:
			case <-r.Context().Done():
				return
			}
		}
		writeJSON(w, map[string]any{"items": []any{}})
	}))
	defer server.Close()
	base := storage.NewMemory()
	probe := &inventoryCASProbe{Provider: base, cas: base.(storage.BlobCAS), attempts: make(chan inventoryCASAttempt, 8)}
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	seedInventoryDiscovery(service)
	service.SetChangeStorage(probe)
	defer service.StopCapture()
	service.startInventory(t.Context())
	select {
	case attempt := <-probe.attempts:
		if !attempt.won || attempt.err != nil {
			t.Fatal(attempt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no lease claim")
	}
	select {
	case <-podRead:
	case <-time.After(5 * time.Second):
		t.Fatal("no Pod list")
	}
	// A successor's atomically published lease invalidates the previous
	// generation's expected bytes, even while the old read is still in flight.
	raw, err := base.ReadBlob(inventoryKey(service.ActiveScope()))
	if err != nil {
		t.Fatal(err)
	}
	var successor inventoryLeaseRecord
	if err := json.Unmarshal(raw, &successor); err != nil {
		t.Fatal(err)
	}
	successor.Owner, successor.Expires = "successor", time.Now().Add(time.Minute)
	raw, err = json.Marshal(successor)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.WriteBlob(inventoryKey(service.ActiveScope()), raw); err != nil {
		t.Fatal(err)
	}
	close(releasePod)
	select {
	case attempt := <-probe.attempts:
		if attempt.won || attempt.err != nil {
			t.Fatalf("stale owner published: %+v", attempt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no publish attempt")
	}
	got, err := base.ReadBlob(inventoryKey(service.ActiveScope()))
	if err != nil || string(got) != string(raw) {
		t.Fatal("successor state was overwritten")
	}
}
