package kubernetes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageHistoryCaptureCadenceQuantitiesAndInterest(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/apis/metrics.k8s.io/v1beta1/pods" {
			t.Errorf("unexpected request %s", request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		calls.Add(1)
		writeJSON(writer, map[string]any{"items": []any{
			map[string]any{"kind": "PodMetrics", "metadata": map[string]any{"name": "api-00", "namespace": "shop"}, "timestamp": now.Format(time.RFC3339Nano), "window": "30s", "containers": []any{map[string]any{"usage": map[string]any{"cpu": "250m", "memory": "64Mi"}}}},
			map[string]any{"kind": "PodMetrics", "metadata": map[string]any{"name": "unopened", "namespace": "shop"}, "timestamp": now.Format(time.RFC3339Nano), "window": "30s", "containers": []any{map[string]any{"usage": map[string]any{"cpu": "1", "memory": "1Gi"}}}},
		}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.cache[service.cacheKey()] = discoveryCacheEntry{expires: now.Add(time.Hour), discovery: Discovery{Resources: []ResourceDefinition{{ID: "metrics.k8s.io~v1beta1~pods", Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods", Kind: "PodMetrics", Namespaced: true, Available: true, Verbs: []string{"get", "list"}}}}}
	seedWorkloadStreamIndex(t, service, workloadStreamRecords("StatefulSet", 1, 1))
	first, err := service.UsageHistory(logStreamTestContext(), "shop", "api-00")
	if err != nil || first.Availability != "unavailable" || len(first.Samples) != 0 || first.LiveSince.IsZero() {
		t.Fatalf("live fallback: %+v %v", first, err)
	}
	service.sampleUsageHistory(context.Background(), now)
	service.sampleUsageHistory(context.Background(), now.Add(10*time.Second))
	history, err := service.UsageHistory(logStreamTestContext(), "shop", "api-00")
	if err != nil || len(history.Samples) != 1 || history.Samples[0].CPUMilli != 250 || history.Samples[0].MemoryBytes != 64<<20 || calls.Load() != 1 {
		t.Fatalf("capture samples: %+v calls=%d err=%v", history, calls.Load(), err)
	}
	unopened, err := service.UsageHistory(logStreamTestContext(), "shop", "unopened")
	if err != nil || len(unopened.Samples) != 0 {
		t.Fatal("uninterested Pod was sampled")
	}
	if _, err := service.UsageHistory(context.Background(), "shop", "api-00"); err != ErrForbidden {
		t.Fatal("reader permission bypass", err)
	}
	if _, err := service.UsageHistory(logStreamTestContext(), "../shop", "api-00"); err != ErrInvalidArguments {
		t.Fatal("unsafe history target accepted", err)
	}
}

func TestUsageHistoryRetentionPodCapScopeAndDefensiveCopies(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	now := time.Now().UTC()
	state := service.usageHistory
	state.Lock()
	scope := state.scope(service.cacheKey())
	scope.pods["shop/api"] = &usagePodHistory{uid: "pod", since: now.Add(-time.Hour), opened: now, last: now, samples: []UsageSample{
		{Timestamp: now.Add(-7 * time.Hour), CPUMilli: 1},
		{Timestamp: now.Add(-time.Minute), CPUMilli: 2},
	}}
	pruneUsage(scope, now)
	if len(scope.pods["shop/api"].samples) != 1 {
		t.Fatal("retention not enforced")
	}
	for i := 1; i < usageHistoryPods; i++ {
		scope.pods["shop/pod-"+strconv.Itoa(i)] = &usagePodHistory{opened: now, last: now}
	}
	if state.podCount(now) != usageHistoryPods {
		t.Fatal("unexpected history Pod accounting")
	}
	state.Unlock()
	// A nil transport can still serve the in-memory history, but caller access
	// and credential epoch validation remain enforced.
	result, err := service.UsageHistory(logStreamTestContext(), "shop", "api")
	if err != nil || len(result.Samples) != 1 {
		t.Fatalf("retained history %+v %v", result, err)
	}
	result.Samples[0].CPUMilli = 900
	again, err := service.UsageHistory(logStreamTestContext(), "shop", "api")
	if err != nil || again.Samples[0].CPUMilli != 2 {
		t.Fatal("caller mutated shared history")
	}
	other := service.Scoped(Scope{OrgID: "licensed", ClusterID: "cluster"})
	empty, err := other.UsageHistory(logStreamTestContext(), "shop", "new")
	if err != nil || len(empty.Samples) != 0 {
		t.Fatal("new scope history fallback failed")
	}
	state.Lock()
	if state.podCount(now) != usageHistoryPods {
		t.Fatal("history Pod cap is not process-state-wide")
	}
	state.Unlock()
	other.readOrgs = []string{"licensed", "default"}
	upgraded, err := other.UsageHistory(logStreamTestContext(), "shop", "api")
	if err != nil || len(upgraded.Samples) != 1 || upgraded.Samples[0].CPUMilli != 2 {
		t.Fatalf("trusted upgrade read failed %+v %v", upgraded, err)
	}
	foreign := service.Scoped(Scope{OrgID: "untrusted", ClusterID: "cluster"})
	denied, err := foreign.UsageHistory(logStreamTestContext(), "shop", "api")
	if err != nil || len(denied.Samples) != 0 {
		t.Fatal("unconfigured read org leaked history")
	}
}

func TestUsageHistorySamplerCanceledAndQuantityGuards(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.sampleUsageHistory(ctx, time.Now())
	if _, ok := quantityFloat("not-a-quantity"); ok {
		t.Fatal("invalid quantity accepted")
	}

	if _, ok := quantityFloat("1/0"); ok {
		t.Fatal("non-finite quantity accepted")
	}
}

func TestUsageHistorySharedAcrossRegistryClustersAndNotCredentialEpochs(t *testing.T) {
	first := NewService(nil, Scope{OrgID: "default", ClusterID: "one"}, 0)
	second := NewService(nil, Scope{OrgID: "default", ClusterID: "two"}, 0)
	_ = NewClusterRegistry(true, []ClusterEntry{{Info: ClusterInfo{ID: "one"}, Service: first}, {Info: ClusterInfo{ID: "two"}, Service: second}})
	if first.usageHistory != second.usageHistory {
		t.Fatal("cluster samplers do not share their bounded store")
	}
	now := time.Now().UTC()
	first.usageHistory.Lock()
	first.usageHistory.scope(first.cacheKey()).pods["shop/api"] = &usagePodHistory{uid: "pod", last: now, since: now, samples: []UsageSample{{Timestamp: now, CPUMilli: 1}}}
	first.usageHistory.Unlock()
	different := first.Scoped(Scope{OrgID: "default", ClusterID: "one", CredentialID: "new-epoch"})
	result, err := different.UsageHistory(logStreamTestContext(), "shop", "api")
	if err != nil || len(result.Samples) != 0 {
		t.Fatalf("credential epoch history leaked: %+v %v", result, err)
	}
}
