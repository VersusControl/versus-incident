package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestScopedInventoryCollectsCompleteNamespaceWithoutGlobalListPermission(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if !strings.Contains(r.URL.Path, "/namespaces/shop/") {
			http.Error(w, "forbidden", 403)
			return
		}
		items := []any{}
		next := ""
		if strings.HasSuffix(r.URL.Path, "/pods") {
			offset, _ := strconv.Atoi(r.URL.Query().Get("continue"))
			for i := offset; i < offset+500 && i < 1501; i++ {
				item := map[string]any{"metadata": map[string]any{"uid": "p-" + strconv.Itoa(i), "name": "p-" + strconv.Itoa(i), "namespace": "shop"}}
				if strings.HasPrefix(r.URL.Path, "/apis/metrics.k8s.io/") {
					item["timestamp"], item["window"], item["containers"] = time.Now().UTC().Format(time.RFC3339Nano), "30s", []any{map[string]any{"name": "app", "usage": map[string]any{"cpu": "1m", "memory": "1Mi"}}}
				} else if i == 0 {
					item["status"] = map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "restartCount": 1, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}}
				}
				items = append(items, item)
			}
			if offset+500 < 1501 {
				next = strconv.Itoa(offset + 500)
			}
		}
		writeJSON(w, map[string]any{"items": items, "metadata": map[string]string{"continue": next}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	seedInventoryDiscovery(service)
	entry := service.cache[service.cacheKey()]
	entry.discovery.Resources = append(entry.discovery.Resources, ResourceDefinition{ID: "metrics.k8s.io~v1beta1~pods", Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods", Kind: "PodMetrics", Namespaced: true, Available: true})
	service.cache[service.cacheKey()] = entry
	service.client.namespace = "shop"
	service.startInventory(t.Context())
	defer service.StopCapture()
	deadline := time.Now().Add(8 * time.Second)
	for {
		view, status, err := service.CachedNamespaceSnapshot(t.Context(), "shop", "Pod")
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "ready" {
			if len(view.Records) != 1501 {
				t.Fatalf("scoped inventory cut off: %d", len(view.Records))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scoped collection unavailable: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	namespaces, err := service.Namespaces(t.Context())
	if err != nil || len(namespaces.Items) != 1 || namespaces.Items[0] != "shop" {
		t.Fatalf("readable namespace missing: %+v %v", namespaces, err)
	}
	deadline = time.Now().Add(8 * time.Second)
	for {
		usage, err := service.cachedUsage(t.Context(), "shop", true)
		if err != nil {
			t.Fatal(err)
		}
		if usage.PodMetrics.Complete && usage.PodMetrics.Total == 1501 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scoped usage sample did not complete: %+v", usage.PodMetrics)
		}
		time.Sleep(time.Millisecond)
	}
	before := reads.Load()
	workloads, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Kind: "Pod", Limit: 200})
	if err != nil || len(workloads.Items) != 200 || workloads.Counts["Pod"] != 1501 || workloads.Truncated || workloads.Sync.Partial {
		t.Fatalf("workloads=%+v %v", workloads, err)
	}
	issues, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true})
	if err != nil || len(issues.Items) == 0 {
		t.Fatalf("scoped issues empty: %+v %v", issues, err)
	}
	usage, err := service.cachedUsage(t.Context(), "shop", true)
	if err != nil || usage.PodMetrics.Total != 1501 {
		t.Fatalf("scoped metrics=%+v %v", usage.PodMetrics, err)
	}
	if reads.Load() > before+1 {
		t.Fatal("views refreshed objects instead of one scoped authority check")
	}
}

type inventoryStorageProbe struct {
	storage.Provider
	dataReads   atomic.Int32
	dataWrites  atomic.Int32
	casBytes    atomic.Int64
	corruptOnce atomic.Bool
}

func (probe *inventoryStorageProbe) ReadBlob(key string) ([]byte, error) {
	data, err := probe.Provider.ReadBlob(key)
	if strings.Contains(key, "/data/") {
		probe.dataReads.Add(1)
		if probe.corruptOnce.CompareAndSwap(true, false) {
			var generation inventoryGeneration
			if json.Unmarshal(data, &generation) == nil && len(generation.View.Records) > 0 {
				generation.View.Records[0].Name = ""
				data, _ = json.Marshal(generation)
			}
		}
	}
	return data, err
}
func (probe *inventoryStorageProbe) WriteBlob(key string, data []byte) error {
	if strings.Contains(key, "/data/") {
		probe.dataWrites.Add(1)
	}
	return probe.Provider.WriteBlob(key, data)
}
func (probe *inventoryStorageProbe) CompareAndSwapBlob(key string, old, replacement []byte) (bool, error) {
	for {
		previous := probe.casBytes.Load()
		if int64(len(old)+len(replacement)) <= previous || probe.casBytes.CompareAndSwap(previous, int64(len(old)+len(replacement))) {
			break
		}
	}
	return probe.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, old, replacement)
}

func TestInventoryLeaseRenewalDoesNotReadOrRewriteData(t *testing.T) {
	probe := &inventoryStorageProbe{Provider: storage.NewMemory()}
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	lease, owned, err := service.claimInventory(t.Context(), probe, "owner")
	if err != nil || !owned {
		t.Fatalf("claim: %v %v", owned, err)
	}
	generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now()}}, Records: []kubeindex.Record{{Kind: "Pod", UID: "pod", Name: "pod"}}}}
	if err := lease.publish(t.Context(), service, &generation); err != nil {
		t.Fatal(err)
	}
	writes := probe.dataWrites.Load()
	for i := 0; i < 3; i++ {
		if err := lease.renew(t.Context(), service); err != nil {
			t.Fatal(err)
		}
	}
	if probe.dataReads.Load() != 0 || probe.dataWrites.Load() != writes || probe.casBytes.Load() > 2048 {
		t.Fatalf("lease touched payload: reads=%d writes=%d CAS=%d", probe.dataReads.Load(), probe.dataWrites.Load(), probe.casBytes.Load())
	}
	if _, _, err := readInventoryGeneration(probe, service.ActiveScope(), ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := readInventoryGeneration(probe, service.ActiveScope(), generation.Revision); err != nil {
			t.Fatal(err)
		}
	}
	if probe.dataReads.Load() != 1 {
		t.Fatalf("unchanged revision decoded %d times", probe.dataReads.Load())
	}
}

func TestInventoryHydrationFailureRetriesAndWorkerCancels(t *testing.T) {
	probe := &inventoryStorageProbe{Provider: storage.NewMemory()}
	service := newTestService(t, "http://127.0.0.1:1", Scope{ClusterID: "cluster"})
	service.SetChangeStorage(probe)
	lease, _, err := service.claimInventory(t.Context(), probe, "other-owner")
	if err != nil {
		t.Fatal(err)
	}
	generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now(), Records: 1}}, Records: []kubeindex.Record{{Kind: "Pod", UID: "pod", Name: "pod"}}}}
	if err := lease.publish(t.Context(), service, &generation); err != nil {
		t.Fatal(err)
	}
	probe.corruptOnce.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	service.startInventory(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for {
		view, status, err := service.CachedIndexSnapshot(t.Context(), "Pod")
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "ready" && len(view.Records) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registered worker did not recover: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	service.changes.mu.Lock()
	done := service.changes.inventories[service.indexScope()].done
	service.changes.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retry worker ignored cancellation")
	}
}

func TestInventoryPreferredVersionAndCadenceAreNotEveryPoll(t *testing.T) {
	resources := []ResourceDefinition{{ID: "example~v2~widgets", Group: "example", Version: "v2", Kind: "Widget", Resource: "widgets", Available: true}, {ID: "example~v1~widgets", Group: "example", Version: "v1", Kind: "Widget", Resource: "widgets", Available: true}, {ID: "apps~v1beta1~deployments", Group: "apps", Version: "v1beta1", Kind: "Deployment", Resource: "deployments", Available: true}}
	canonical := canonicalInventoryResources(resources)
	if len(canonical) != 1 || canonical[0].Version != "v2" {
		t.Fatalf("duplicate versions scheduled: %+v", canonical)
	}
	now := time.Now()
	for _, kind := range []string{"Pod", "Namespace", "ConfigMap", "HorizontalPodAutoscaler"} {
		if inventoryDue(kubeindex.KindStatus{State: "ready", LastAttemptAt: now}, inventoryCadence(kind), now.Add(15*time.Second)) {
			t.Fatalf("%s was scheduled every poll", kind)
		}
		if !inventoryDue(kubeindex.KindStatus{State: "ready", LastAttemptAt: now}, inventoryCadence(kind), now.Add(inventoryCadence(kind))) {
			t.Fatalf("%s missed its cadence", kind)
		}
	}
}
