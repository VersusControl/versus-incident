package kubernetes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestColdInventoryViewsOnlyReportSyncing(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reads.Add(1); http.Error(w, "unexpected read", 500) }))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	defer service.StopCapture()
	overview, err := service.Overview(t.Context())
	if err != nil || overview.Sync.State != "syncing" || overview.Sync.Partial || overview.Truncated || len(overview.Partial) > 0 {
		t.Fatalf("cold overview=%+v %v", overview, err)
	}
	workloads, err := service.Workloads(t.Context(), WorkloadListOptions{})
	if err != nil || workloads.Sync.State != "syncing" || workloads.Sync.Partial || workloads.Truncated {
		t.Fatalf("cold workloads=%+v %v", workloads, err)
	}
	issues, err := service.Issues(t.Context(), IssueOptions{RootUID: "root", IncludeChanges: true})
	if err != nil || issues.Sync.State != "syncing" || issues.Sync.Partial || len(issues.Partial) > 0 {
		t.Fatalf("cold issues=%+v %v", issues, err)
	}
	page, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"]})
	if err != nil || page.Sync.State != "syncing" || page.Sync.Partial || page.Truncated {
		t.Fatalf("cold nodes=%+v %v", page, err)
	}
	crashes, err := service.Crashes(t.Context(), kubechanges.CrashQuery{})
	if err != nil || crashes.Sync.State != "syncing" || crashes.Sync.Partial {
		t.Fatalf("cold crashes=%+v %v", crashes, err)
	}
	if reads.Load() != 0 {
		t.Fatalf("cold request triggered %d cluster reads", reads.Load())
	}
	graph, err := service.Graph(t.Context(), GraphQuery{Complete: true, Namespace: "shop"})
	if err != nil || graph.Sync.State != "syncing" || graph.Sync.Partial || reads.Load() != 0 {
		t.Fatalf("cold complete graph=%+v %v", graph.Sync, err)
	}
}

func TestAdditionalInventoryProjectionAndCachedPaging(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		offset, _ := strconv.Atoi(r.URL.Query().Get("continue"))
		items := []any{}
		for i := offset; i < offset+500 && i < 1501; i++ {
			items = append(items, map[string]any{"metadata": map[string]any{"uid": "widget-" + strconv.Itoa(i), "name": "widget-" + strconv.Itoa(i), "namespace": "shop", "labels": map[string]any{"app": "api"}}, "spec": map[string]any{"password": "MUST_NOT_PROJECT"}, "data": map[string]any{"private": "MUST_NOT_PROJECT"}})
		}
		next := ""
		if offset+500 < 1501 {
			next = strconv.Itoa(offset + 500)
		}
		writeJSON(w, map[string]any{"items": items, "metadata": map[string]string{"continue": next}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	definition := ResourceDefinition{ID: "example.io~v1~widgets", Kind: "Widget", Group: "example.io", Version: "v1", Resource: "widgets", Namespaced: true, Available: true}
	items, err := service.loadAdditionalInventory(t.Context(), definition)
	if err != nil || len(items) != 1501 || reads.Load() != 4 {
		t.Fatalf("items=%d pages=%d err=%v", len(items), reads.Load(), err)
	}
	for _, item := range items {
		for key := range item.Summary {
			if key != "resource_version" {
				t.Fatalf("unrecognized kind leaked spec/data: %+v", item.Summary)
			}
		}
	}
	seedInventory(t, service, nil)
	cache := service.changes.inventories[service.indexScope()]
	cache.generation.Resources = map[string][]ProjectedResource{definition.ID: items}
	cache.generation.ResourceStatus = map[string]kubeindex.KindStatus{definition.ID: {State: "ready", ObservedAt: time.Now().UTC(), Records: len(items)}}
	options := ListOptions{ResourceID: definition.ID, Namespace: "shop", Labels: "app=api", Limit: 200}
	total := 0
	for {
		page, err := service.CachedResources(t.Context(), options)
		if err != nil || page.Truncated || page.Sync.Partial {
			t.Fatalf("page=%+v %v", page, err)
		}
		total += len(page.Items)
		if page.Continue == "" {
			break
		}
		options.Continue = page.Continue
	}
	if total != 1501 || reads.Load() != 4 {
		t.Fatalf("paged cached total=%d cluster reads=%d", total, reads.Load())
	}
	state := cache.generation.ResourceStatus[definition.ID]
	state.ObservedAt = time.Now().Add(-11 * time.Minute)
	cache.generation.ResourceStatus[definition.ID] = state
	options.Continue = ""
	stale, err := service.CachedResources(t.Context(), options)
	if err != nil || stale.Sync.State != "stale" || !stale.Sync.Partial || len(stale.Items) == 0 || reads.Load() != 4 {
		t.Fatalf("stale generation lost data or freshness: %+v %v", stale.Sync, err)
	}
}

func TestInventoryHistoryDeltasAreNotSubscriberBounded(t *testing.T) {
	before := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now().Add(-time.Minute)}}}
	after := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now()}}}
	for i := 0; i < 5000; i++ {
		uid := "pod-" + strconv.Itoa(i)
		after.Records = append(after.Records, kubeindex.Record{UID: uid, Kind: "Pod", Name: uid})
	}
	count := 0
	for delta := range inventoryDeltas(before, after, []string{"Pod"}) {
		if delta.Resync {
			t.Fatal("inventory size created a history gap")
		}
		count++
	}
	if count != 5000 {
		t.Fatalf("deltas=%d", count)
	}
}

func TestInventoryCredentialRetirementHidesLastGood(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	var header atomic.Value
	header.Store("Bearer old-fixture")
	client, err := NewClient(Config{Endpoint: "http://127.0.0.1:1", AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return header.Load().(string), nil }}
	service = NewService(client, service.Scope(), 0)
	seedInventory(t, service, []kubeindex.Record{{UID: "old", Kind: "Pod", Namespace: "shop", Name: "old"}})
	view, _, err := service.CachedIndexSnapshot(t.Context(), "Pod")
	if err != nil || len(view.Records) != 1 {
		t.Fatalf("initial cache: %+v %v", view, err)
	}
	header.Store("Bearer new-fixture")
	view, _, err = service.CachedIndexSnapshot(t.Context(), "Pod")
	if err == nil || len(view.Records) != 0 || service.identity.ctx.Err() == nil {
		t.Fatalf("retired cache exposed: %+v %v", view, err)
	}
}

func TestInventoryReadRetryIsBoundedAndCancellable(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reads.Add(1); http.Error(w, "unavailable", 503) }))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	var page any
	if err := service.inventoryRead(t.Context(), "/api/v1/pods", &page, false); err == nil || reads.Load() != 3 {
		t.Fatalf("retry result=%v requests=%d", err, reads.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if err := service.inventoryRead(ctx, "/api/v1/pods", &page, false); err == nil || time.Since(start) > time.Second {
		t.Fatalf("canceled read=%v", err)
	}
}
