package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOperationBudgetBoundsSlowPaginationWithoutLeakingGoroutines(t *testing.T) {
	var pageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/default/pods":
			pageRequests.Add(1)
			select {
			case <-time.After(40 * time.Millisecond):
				writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "next"}, "items": []any{podFixture("pod")}})
			case <-request.Context().Done():
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "test"})
	if _, err := service.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	ctx, cancel := withOperationBudget(context.Background(), 70*time.Millisecond, 50, 1<<20, 100)
	started := time.Now()
	page, err := service.listAll(ctx, ListOptions{ResourceID: "core~v1~pods", Namespace: "default"})
	cancel()
	if err != nil || time.Since(started) > 250*time.Millisecond || pageRequests.Load() > 2 || !page.Truncated || !containsString(page.Omitted, "budget_exhausted") || len(page.Partial) == 0 || page.Partial[len(page.Partial)-1].Class != "budget_exhausted" {
		t.Fatalf("page=%#v requests=%d elapsed=%s err=%v", page, pageRequests.Load(), time.Since(started), err)
	}
	runtime.GC()
	runtime.Gosched()
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines before=%d after=%d", before, after)
	}
}

func TestOperationBudgetBoundsCumulativeDecodedBytesAndItems(t *testing.T) {
	pagePayload, _ := json.Marshal(map[string]any{"metadata": map[string]any{"continue": "next"}, "items": []any{podFixture("pod")}})
	var pageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/default/pods":
			pageRequests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write(pagePayload)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "test"})
	if _, err := service.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := withOperationBudget(context.Background(), time.Second, 10, int64(len(pagePayload)+1), 100)
	page, err := service.listAll(ctx, ListOptions{ResourceID: "core~v1~pods", Namespace: "default"})
	cancel()
	if err != nil || pageRequests.Load() != 2 || len(page.Items) != 1 || !page.Truncated || !containsString(page.Omitted, "budget_exhausted") {
		t.Fatalf("byte-budget page=%#v requests=%d err=%v", page, pageRequests.Load(), err)
	}

	ctx, cancel = withOperationBudget(context.Background(), time.Second, 10, 1<<20, 0)
	direct, err := service.List(ctx, ListOptions{ResourceID: "core~v1~pods", Namespace: "default"})
	cancel()
	if err != nil || len(direct.Items) != 0 || !direct.Truncated || !containsString(direct.Omitted, "budget_exhausted") || len(direct.Partial) != 1 {
		t.Fatalf("item-budget page=%#v err=%v", direct, err)
	}
}

func TestServiceDiscoveryFallbackPaginationAndCache(t *testing.T) {
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			apiCalls.Add(1)
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "apps", "preferredVersion": map[string]any{"version": "v2"}, "versions": []any{map[string]any{"version": "v2"}, map[string]any{"version": "v1"}}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "shortNames": []string{"po"}, "verbs": []string{"get", "list"}}, map[string]any{"name": "pods/log", "kind": "Pod", "verbs": []string{"get"}}}})
		case "/apis/apps/v2":
			http.Error(writer, "missing", http.StatusNotFound)
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/default/pods":
			if request.URL.Query().Get("limit") != "1" {
				t.Errorf("limit = %q", request.URL.Query().Get("limit"))
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "next"}, "items": []any{podFixture("api-1")}})
		default:
			t.Errorf("unexpected Kubernetes request %s", request.URL.RequestURI())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"})
	first, err := service.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Discover(context.Background())
	if err != nil || len(second.Resources) != 2 || apiCalls.Load() != 1 {
		t.Fatalf("cached discovery resources=%d calls=%d err=%v", len(second.Resources), apiCalls.Load(), err)
	}
	if first.Resources[0].ID != "apps~v1~deployments" || first.Resources[1].ID != "core~v1~pods" {
		t.Fatalf("resources = %#v", first.Resources)
	}
	if len(first.Partial) != 0 {
		t.Fatalf("successful version fallback retained partial noise: %#v", first.Partial)
	}
	if _, err := service.resolve(context.Background(), "core~v1~pods"); err != nil {
		t.Fatalf("resolve from cached resources %#v: %v", second.Resources, err)
	}
	page, err := service.List(context.Background(), ListOptions{ResourceID: "core~v1~pods", Namespace: "default", Limit: 1})
	if err != nil || !page.Truncated || page.Continue != "next" || len(page.Items) != 1 {
		t.Fatalf("page = %#v err=%v", page, err)
	}
}

func TestListWorkloadsPaginatesAndValidatesKind(t *testing.T) {
	var podPages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/default/pods":
			podPages.Add(1)
			if request.URL.Query().Get("continue") == "page-2" {
				writeJSON(writer, map[string]any{"items": []any{podFixture("api-2")}})
				return
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "page-2"}, "items": []any{podFixture("api-1")}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	page, err := service.ListWorkloads(context.Background(), "default", "Pod", 3)
	if err != nil || len(page.Items) != 2 || page.Truncated || podPages.Load() != 2 {
		t.Fatalf("workloads = %#v pages=%d err=%v", page, podPages.Load(), err)
	}
	if page.Items[0].Name != "api-1" || page.Items[1].Name != "api-2" {
		t.Fatalf("workload names = %q, %q", page.Items[0].Name, page.Items[1].Name)
	}
	if _, err := service.ListWorkloads(context.Background(), "default", "ReplicaSet", 3); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("invalid workload kind error = %v", err)
	}
}

func TestListInvalidatesContinuationWhenServerExceedsLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/default/pods":
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "unsafe-next"}, "items": []any{podFixture("one"), podFixture("two")}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	page, err := newTestService(t, server.URL, Scope{ClusterID: "test"}).List(context.Background(), ListOptions{ResourceID: "core~v1~pods", Namespace: "default", Limit: 1})
	if err != nil || len(page.Items) != 1 || !page.Truncated || page.Continue != "" || !containsString(page.Omitted, "item_limit") || len(page.Partial) != 1 || page.Partial[0].Class != "item_limit" {
		t.Fatalf("page = %#v err=%v", page, err)
	}
}

func TestNamespacedReadsRequireNamespaceWithSharedArgumentError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		default:
			t.Fatalf("missing namespace reached cluster path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "test"})
	for name, read := range map[string]func() error{
		"get":      func() error { _, err := service.Get(context.Background(), "core~v1~pods", "", "api"); return err },
		"describe": func() error { _, err := service.Describe(context.Background(), "core~v1~pods", "", "api"); return err },
		"workload": func() error { _, err := service.GetWorkload(context.Background(), "", "Pod", "api"); return err },
	} {
		if err := read(); !errors.Is(err, ErrInvalidArguments) {
			t.Errorf("%s error = %v, want ErrInvalidArguments", name, err)
		}
	}
}

func TestSearchPaginatesFiltersAndRanksAcrossKinds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "services", "kind": "Service", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/api/v1/namespaces/default/pods":
			if request.URL.Query().Get("labelSelector") != "app=api" || request.URL.Query().Get("fieldSelector") != "status.phase=Running" {
				t.Errorf("selectors = %q, %q", request.URL.Query().Get("labelSelector"), request.URL.Query().Get("fieldSelector"))
			}
			if request.URL.Query().Get("continue") == "pods-2" {
				writeJSON(writer, map[string]any{"items": []any{podFixture("my-api")}})
				return
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "pods-2"}, "items": []any{podFixture("api-worker")}})
		case "/api/v1/namespaces/default/services":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"namespace": "default", "name": "api"}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	result, err := service.Search(context.Background(), SearchOptions{Query: "api", Namespace: "default", Labels: "app=api", Fields: "status.phase=Running", PerKindLimit: 3, TotalLimit: 10})
	if err != nil || result.Requests != 3 || len(result.Items) != 3 || result.Truncated {
		t.Fatalf("search = %#v err=%v", result, err)
	}
	if result.Items[0].Name != "api" || result.Items[1].Name != "api-worker" || result.Items[2].Name != "my-api" {
		t.Fatalf("ranked names = %q, %q, %q", result.Items[0].Name, result.Items[1].Name, result.Items[2].Name)
	}
	filtered, err := service.Search(context.Background(), SearchOptions{Query: "api", Namespace: "default", Category: "network"})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].Kind != "Service" {
		t.Fatalf("category search = %#v err=%v", filtered, err)
	}
	if _, err := service.Search(context.Background(), SearchOptions{Query: "api", Category: "mutation"}); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("invalid category error = %v", err)
	}
}

func TestGetWorkloadReturnsTypedBoundedRelatedFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{
				map[string]any{"name": "apps", "preferredVersion": map[string]any{"version": "v1"}},
				map[string]any{"name": "autoscaling", "preferredVersion": map[string]any{"version": "v2"}},
				map[string]any{"name": "policy", "preferredVersion": map[string]any{"version": "v1"}},
			}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/autoscaling/v2":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "horizontalpodautoscalers", "kind": "HorizontalPodAutoscaler", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/policy/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "poddisruptionbudgets", "kind": "PodDisruptionBudget", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1/namespaces/payments/deployments/api":
			writeJSON(writer, workloadFixture())
		case "/api/v1/namespaces/payments/pods":
			pod := podFixture("api-1")
			pod["metadata"].(map[string]any)["namespace"] = "payments"
			pod["metadata"].(map[string]any)["labels"] = map[string]any{"app": "api"}
			pod["status"].(map[string]any)["containerStatuses"] = []any{map[string]any{"restartCount": 3}}
			writeJSON(writer, map[string]any{"items": []any{pod}})
		case "/apis/autoscaling/v2/namespaces/payments/horizontalpodautoscalers":
			writeJSON(writer, map[string]any{"items": []any{
				map[string]any{"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler", "metadata": map[string]any{"namespace": "payments", "name": "api-scale"}, "spec": map[string]any{"scaleTargetRef": map[string]any{"kind": "Deployment", "name": "api"}}},
				map[string]any{"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler", "metadata": map[string]any{"namespace": "payments", "name": "other-scale"}, "spec": map[string]any{"scaleTargetRef": map[string]any{"kind": "Deployment", "name": "other"}}},
			}})
		case "/apis/policy/v1/namespaces/payments/poddisruptionbudgets":
			writeJSON(writer, map[string]any{"items": []any{
				map[string]any{"apiVersion": "policy/v1", "kind": "PodDisruptionBudget", "metadata": map[string]any{"namespace": "payments", "name": "api-budget"}, "spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "api"}}}},
				map[string]any{"apiVersion": "policy/v1", "kind": "PodDisruptionBudget", "metadata": map[string]any{"namespace": "payments", "name": "other-budget"}, "spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "other"}}}},
			}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	detail, err := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"}).GetWorkload(context.Background(), "payments", "Deployment", "api")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Desired == nil || *detail.Desired != 3 || detail.Ready == nil || *detail.Ready != 2 || detail.Generation == nil || *detail.Generation != 7 || detail.ObservedGeneration == nil || *detail.ObservedGeneration != 7 {
		t.Fatalf("replica/generation facts = %#v", detail)
	}
	if detail.UpdateStrategy != "RollingUpdate" || len(detail.Containers) != 1 || detail.Containers[0].Requests["cpu"] != "250m" || strings.Join(detail.Containers[0].Probes, ",") != "http" {
		t.Fatalf("container facts = %#v", detail.Containers)
	}
	if len(detail.Pods) != 1 || detail.Pods[0].RestartCount != 3 || detail.Pods[0].Node != "node-a" || len(detail.HPAs) != 1 || detail.HPAs[0].Name != "api-scale" || len(detail.PDBs) != 1 || detail.PDBs[0].Name != "api-budget" {
		t.Fatalf("related facts = %#v", detail)
	}
	if detail.TerminationGrace == nil || *detail.TerminationGrace != 45 || strings.Join(detail.Affinity, ",") != "pod_anti_affinity" || strings.Join(detail.TopologySpread, ",") != "zone:DoNotSchedule" {
		t.Fatalf("scheduling facts = %#v", detail)
	}
}

func TestUsageAndOverviewExposeMetricsAvailabilityFreshnessAndExactQuantities(t *testing.T) {
	for _, test := range []struct {
		name        string
		nodeMetrics bool
		wantCPU     string
		wantMemory  string
		wantSource  string
	}{
		{name: "node metrics preferred", nodeMetrics: true, wantCPU: "1", wantMemory: "2147483648", wantSource: "node_metrics"},
		{name: "pod metrics fallback", wantCPU: "1/4", wantMemory: "1073741824", wantSource: "pod_metrics"},
	} {
		t.Run(test.name, func(t *testing.T) {
			timestamp := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
			podTimestamp := timestamp
			if test.nodeMetrics {
				podTimestamp = time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api":
					writeJSON(writer, map[string]any{"versions": []string{"v1"}})
				case "/apis":
					writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "metrics.k8s.io", "preferredVersion": map[string]any{"version": "v1beta1"}}}})
				case "/api/v1":
					writeJSON(writer, map[string]any{"resources": []any{
						map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
						map[string]any{"name": "nodes", "kind": "Node", "verbs": []string{"get", "list"}},
					}})
				case "/apis/metrics.k8s.io/v1beta1":
					resources := []any{map[string]any{"name": "pods", "kind": "PodMetrics", "namespaced": true, "verbs": []string{"get", "list"}}}
					if test.nodeMetrics {
						resources = append(resources, map[string]any{"name": "nodes", "kind": "NodeMetrics", "verbs": []string{"get", "list"}})
					}
					writeJSON(writer, map[string]any{"resources": resources})
				case "/apis/metrics.k8s.io/v1beta1/namespaces/payments/pods", "/apis/metrics.k8s.io/v1beta1/pods":
					writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics", "metadata": map[string]any{"namespace": "payments", "name": "api-1"}, "timestamp": podTimestamp, "window": "30s", "containers": []any{map[string]any{"usage": map[string]any{"cpu": "250m", "memory": "1Gi"}}}}}})
				case "/apis/metrics.k8s.io/v1beta1/nodes":
					writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics", "metadata": map[string]any{"name": "node-a"}, "timestamp": timestamp, "window": "30s", "usage": map[string]any{"cpu": "1", "memory": "2Gi"}}}})
				case "/api/v1/pods":
					writeJSON(writer, map[string]any{"items": []any{podFixture("api-1")}})
				case "/api/v1/nodes":
					writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "node-a"}, "status": map[string]any{"allocatable": map[string]any{"cpu": "4", "memory": "8Gi"}}}}})
				default:
					if strings.Contains(request.URL.Path, "/deployments") || strings.Contains(request.URL.Path, "/statefulsets") || strings.Contains(request.URL.Path, "/daemonsets") || strings.Contains(request.URL.Path, "/jobs") || strings.Contains(request.URL.Path, "/cronjobs") || strings.Contains(request.URL.Path, "/namespaces") || strings.Contains(request.URL.Path, "/events") {
						http.NotFound(writer, request)
						return
					}
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
			usage, err := service.Usage(context.Background(), "payments", 10)
			wantNodes := 0
			if test.nodeMetrics {
				wantNodes = 1
			}
			wantUsageFresh := !test.nodeMetrics
			if err != nil || usage.Availability == "unavailable" || usage.Fresh != wantUsageFresh || len(usage.Pods) != 1 || len(usage.Nodes) != wantNodes || usage.Pods[0].CPU != "1/4" || usage.PodMetrics.Total != 1 || usage.PodMetrics.CPU != "1/4" {
				t.Fatalf("usage = %#v err=%v", usage, err)
			}
			if test.nodeMetrics && (usage.NodeMetrics.Total != 1 || !usage.NodeMetrics.Fresh || usage.NodeMetrics.CPU != "1") {
				t.Fatalf("node metrics = %#v", usage.NodeMetrics)
			}
			overview, err := service.Overview(context.Background())
			if err != nil || overview.MetricsStatus != "available" || !overview.MetricsFresh || overview.UsageCPU != test.wantCPU || overview.UsageMemory != test.wantMemory || overview.UsageSource != test.wantSource || overview.MetricsObservedAt == nil {
				t.Fatalf("overview metrics = %#v err=%v", overview, err)
			}
		})
	}
}

func TestOverviewUsesFullPreCapMetricsFreshnessAndTotals(t *testing.T) {
	freshTimestamp := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	staleTimestamp := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	metrics := make([]any, 0, 501)
	for index := 0; index < 501; index++ {
		timestamp := freshTimestamp
		if index == 500 {
			timestamp = staleTimestamp
		}
		metrics = append(metrics, map[string]any{"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics", "metadata": map[string]any{"name": "node-" + strconv.Itoa(index)}, "timestamp": timestamp, "window": "30s", "usage": map[string]any{"cpu": "1", "memory": "1Gi"}})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "metrics.k8s.io", "preferredVersion": map[string]any{"version": "v1beta1"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis/metrics.k8s.io/v1beta1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "nodes", "kind": "NodeMetrics", "verbs": []string{"get", "list"}}}})
		case "/apis/metrics.k8s.io/v1beta1/nodes":
			if request.URL.Query().Get("continue") == "node-page-2" {
				writeJSON(writer, map[string]any{"items": metrics[500:]})
				return
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "node-page-2"}, "items": metrics[:500]})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	usage, err := service.Usage(context.Background(), "", 500)
	if err != nil || !usage.Truncated || len(usage.Nodes) != 500 || usage.NodeMetrics.Total != 501 || usage.NodeMetrics.Fresh || usage.NodeMetrics.Availability != "stale" || usage.NodeMetrics.CPU != "501" || !containsString(usage.Omitted, "metrics.k8s.io~v1beta1~nodes") || len(usage.Partial) != 1 {
		t.Fatalf("usage = %#v err=%v", usage, err)
	}
	overview, err := service.Overview(context.Background())
	if err != nil || overview.UsageSource != "node_metrics" || overview.MetricsFresh || overview.MetricsStatus != "stale" || overview.UsageCPU != "501" || !overview.Truncated || !containsString(overview.Omitted, "metrics.k8s.io~v1beta1~nodes") {
		t.Fatalf("overview = %#v err=%v", overview, err)
	}
}

func TestUsageMarksMissingMetricsAPIUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	usage, err := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"}).Usage(context.Background(), "", 10)
	if err != nil || usage.Availability != "unavailable" || usage.Fresh || len(usage.Omitted) != 2 {
		t.Fatalf("usage = %#v err=%v", usage, err)
	}
}

func TestProjectionAndResultEncodedSizeBounds(t *testing.T) {
	labels := make(map[string]any, maxProjectionItems)
	conditions := make([]any, 0, maxProjectionItems)
	for index := 0; index < maxProjectionItems; index++ {
		key := strings.Repeat("k", 1900) + strconv.Itoa(index)
		labels[key] = strings.Repeat("v", maxStringBytes)
		conditions = append(conditions, map[string]any{"type": strings.Repeat("condition", 250), "status": "True", "reason": strings.Repeat("reason", 350)})
	}
	raw := map[string]any{
		"apiVersion": "example.io/v1", "kind": "Widget",
		"metadata": map[string]any{"namespace": "default", "name": "large", "labels": labels},
		"status":   map[string]any{"conditions": conditions},
		"spec":     map[string]any{"rawSecretPayload": strings.Repeat("must-not-cross", 10000)},
	}
	service := &Service{}
	projected := service.projectResource(ResourceDefinition{ID: "example.io~v1~widgets", Kind: "Widget"}, raw)
	encoded, err := json.Marshal(projected)
	if err != nil || len(encoded) > maxProjectionBytes || len(projected.ProjectionTruncated) == 0 || strings.Contains(string(encoded), "must-not-cross") {
		t.Fatalf("projection bytes=%d omitted=%v contains_raw=%v err=%v", len(encoded), projected.ProjectionTruncated, strings.Contains(string(encoded), "must-not-cross"), err)
	}

	items := make([]ProjectedResource, 40)
	for index := range items {
		items[index] = ProjectedResource{ResourceID: strings.Repeat("r", maxStringBytes), Kind: strings.Repeat("k", maxStringBytes), Name: strings.Repeat("n", maxStringBytes), Summary: map[string]any{"value": strings.Repeat("x", 24000)}}
	}
	bounded, truncated := enforceResultSize(items)
	resultBytes, _ := json.Marshal(bounded)
	if !truncated || len(bounded) >= len(items) || len(resultBytes) > maxResultBytes {
		t.Fatalf("result items=%d bytes=%d truncated=%v", len(bounded), len(resultBytes), truncated)
	}
}

func TestAggregateEncodedTrimAttributesDroppedTailWithoutDuplicateOmissions(t *testing.T) {
	items := []ProjectedResource{
		{ResourceID: "apps~v1~deployments", Kind: "Deployment", Name: "kept", Summary: map[string]any{"value": strings.Repeat("a", 600000)}},
		{ResourceID: "core~v1~pods", Kind: "Pod", Name: "dropped-pod", Summary: map[string]any{"value": strings.Repeat("b", 600000)}},
		{ResourceID: "core~v1~pods", Kind: "Pod", Name: "dropped-pod-2"},
		{ResourceID: "core~v1~services", Kind: "Service", Name: "dropped-service"},
	}
	bounded, omitted, truncated := trimAggregateItems(items, []string{"core~v1~pods", "encoded_result_size"})
	wantOmitted := []string{"core~v1~pods", "encoded_result_size", "category=pod", "core~v1~services", "category=network"}
	if !truncated || len(bounded) != 1 || strings.Join(omitted, ",") != strings.Join(wantOmitted, ",") {
		t.Fatalf("bounded=%d truncated=%v omitted=%v want=%v", len(bounded), truncated, omitted, wantOmitted)
	}
}

func TestTerminalEncodedSizeTruncationPropagatesThroughAggregates(t *testing.T) {
	var continuationRequests atomic.Int32
	items := make([]any, 0, 20)
	for itemIndex := 0; itemIndex < 20; itemIndex++ {
		labels := make(map[string]any, maxProjectionItems)
		for labelIndex := 0; labelIndex < maxProjectionItems; labelIndex++ {
			labels[strings.Repeat("k", 300)+strconv.Itoa(labelIndex)] = strings.Repeat("v", 300)
		}
		pod := podFixture("needle-" + strconv.Itoa(itemIndex))
		pod["metadata"].(map[string]any)["labels"] = labels
		items = append(items, pod)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "events", "kind": "Event", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/api/v1/pods", "/api/v1/namespaces/default/pods":
			if request.URL.Query().Get("continue") != "" {
				continuationRequests.Add(1)
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "page-2"}, "items": items})
		case "/api/v1/namespaces/default/pods/needle-0":
			writeJSON(writer, items[0])
		case "/api/v1/events", "/api/v1/namespaces/default/events":
			if request.URL.Query().Get("continue") != "" {
				continuationRequests.Add(1)
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "events-2"}, "items": items})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "test"})
	direct, err := service.List(context.Background(), ListOptions{ResourceID: "core~v1~pods", Namespace: "default"})
	if err != nil || !direct.Truncated || !direct.EncodedTruncated || direct.Continue != "" || len(direct.Partial) != 1 {
		t.Fatalf("direct list = %#v err=%v", direct, err)
	}
	all, err := service.listAll(context.Background(), ListOptions{ResourceID: "core~v1~pods", Namespace: "default"})
	if err != nil || !all.Truncated || !all.EncodedTruncated || !containsString(all.Omitted, "encoded_result_size") || len(all.Partial) == 0 {
		t.Fatalf("listAll = %#v err=%v", all, err)
	}
	workloads, err := service.ListWorkloads(context.Background(), "default", "Pod", 100)
	if err != nil || !workloads.Truncated || !containsString(workloads.Omitted, "encoded_result_size") || len(workloads.Partial) == 0 {
		t.Fatalf("workloads = %#v err=%v", workloads, err)
	}
	search, err := service.Search(context.Background(), SearchOptions{Query: "needle", Namespace: "default"})
	if err != nil || !search.Truncated || !containsString(search.Omitted, "encoded_result_size") || !containsString(search.Omitted, "core~v1~pods") || !containsString(search.Omitted, "category=pod") || len(search.Partial) == 0 || len(search.Continuations) != 0 {
		t.Fatalf("search = %#v err=%v", search, err)
	}
	overview, err := service.Overview(context.Background())
	if err != nil || !overview.Truncated || overview.UsageSource != "unavailable" || !containsString(overview.Omitted, "core~v1~pods") || len(overview.Partial) == 0 {
		t.Fatalf("overview = %#v err=%v", overview, err)
	}
	topology, err := service.Topology(context.Background(), "default", 20, 20)
	if err != nil || !topology.Truncated || !containsString(topology.Omitted, "core~v1~pods") || !containsString(topology.Omitted, "category=pod") || len(topology.Partial) == 0 {
		t.Fatalf("topology = %#v err=%v", topology, err)
	}
	workload, err := service.GetWorkload(context.Background(), "default", "Pod", "needle-0")
	if err != nil || !workload.Truncated || !containsString(workload.Omitted, "encoded_result_size") || len(workload.Partial) == 0 {
		t.Fatalf("workload = %#v err=%v", workload, err)
	}
	events, err := service.listAllEvents(context.Background(), EventOptions{Namespace: "default", Kind: "Pod", Name: "needle-0"})
	if err != nil || !events.Truncated || !events.EncodedTruncated || !containsString(events.Omitted, "encoded_result_size") || len(events.Partial) == 0 {
		t.Fatalf("events = %#v err=%v", events, err)
	}
	description, err := service.Describe(context.Background(), "core~v1~pods", "default", "needle-0")
	descriptionBytes, _ := json.Marshal(description)
	if err != nil || !description.Truncated || !containsString(description.Omitted, "encoded_result_size") || len(description.Partial) == 0 || len(descriptionBytes) > maxResultBytes {
		t.Fatalf("description = %#v err=%v", description, err)
	}
	if continuationRequests.Load() != 0 {
		t.Fatalf("aggregates reused continuation after local item drop: %d requests", continuationRequests.Load())
	}
}

func TestDeriveTopologyEdgesCoversObservedIntentAndOwnershipBreadth(t *testing.T) {
	resources := map[string]ProjectedResource{}
	addResource := func(kind, namespace, name string, summary map[string]any, labels map[string]string, owners ...ObjectRef) {
		resources[objectID(kind, namespace, name)] = ProjectedResource{Kind: kind, Namespace: namespace, Name: name, Summary: summary, Labels: labels, Owners: owners}
	}
	addResource("Deployment", "payments", "api", map[string]any{}, nil)
	addResource("ReplicaSet", "payments", "api-rs", map[string]any{}, nil, ObjectRef{Kind: "Deployment", Name: "api"})
	addResource("Pod", "payments", "api-1", map[string]any{"config_maps": []string{"api-config"}, "secrets": []string{"api-secret"}, "persistent_volume_claims": []string{"api-data"}}, map[string]string{"app": "api"}, ObjectRef{Kind: "ReplicaSet", Name: "api-rs"})
	addResource("Pod", "payments", "fallback-1", map[string]any{}, map[string]string{"app": "fallback"})
	addResource("Service", "payments", "api", map[string]any{"selector": map[string]string{"app": "api"}}, nil)
	addResource("Service", "payments", "fallback", map[string]any{"selector": map[string]string{"app": "fallback"}}, nil)
	addResource("EndpointSlice", "payments", "api-slice", map[string]any{"services": []string{"api"}, "pods": []string{"api-1"}}, nil)
	addResource("Ingress", "payments", "public", map[string]any{"services": []string{"api"}}, nil)
	addResource("Gateway", "payments", "edge", map[string]any{}, nil)
	addResource("HTTPRoute", "payments", "api-route", map[string]any{"gateways": []string{"edge"}, "services": []string{"api"}}, nil)
	addResource("HorizontalPodAutoscaler", "payments", "api-scale", map[string]any{"target": ObjectRef{Kind: "Deployment", Name: "api"}}, nil)
	addResource("PodDisruptionBudget", "payments", "api-budget", map[string]any{"selector": map[string]string{"app": "api"}}, nil)
	addResource("ConfigMap", "payments", "api-config", map[string]any{}, nil, ObjectRef{Kind: "Widget", Name: "owner"})
	addResource("Secret", "payments", "api-secret", map[string]any{}, nil)
	addResource("PersistentVolumeClaim", "payments", "api-data", map[string]any{"persistent_volumes": []string{"pv-api"}}, nil)
	addResource("PersistentVolume", "", "pv-api", map[string]any{"storage_classes": []string{"fast"}}, nil)
	addResource("StorageClass", "", "fast", map[string]any{}, nil)
	addResource("Widget", "payments", "owner", map[string]any{}, nil)

	edges := map[TopologyEdge]bool{}
	deriveTopologyEdges(resources, func(edge TopologyEdge) {
		if _, from := resources[edge.From]; from {
			if _, to := resources[edge.To]; to {
				edges[edge] = true
			}
		}
	})
	wanted := []TopologyEdge{
		{From: objectID("Service", "payments", "api"), To: objectID("EndpointSlice", "payments", "api-slice"), Type: "routes_to"},
		{From: objectID("EndpointSlice", "payments", "api-slice"), To: objectID("Pod", "payments", "api-1"), Type: "routes_to"},
		{From: objectID("Service", "payments", "fallback"), To: objectID("Pod", "payments", "fallback-1"), Type: "selects", Intent: true},
		{From: objectID("Ingress", "payments", "public"), To: objectID("Service", "payments", "api"), Type: "routes_to"},
		{From: objectID("Gateway", "payments", "edge"), To: objectID("HTTPRoute", "payments", "api-route"), Type: "attaches"},
		{From: objectID("HTTPRoute", "payments", "api-route"), To: objectID("Service", "payments", "api"), Type: "routes_to"},
		{From: objectID("HorizontalPodAutoscaler", "payments", "api-scale"), To: objectID("Deployment", "payments", "api"), Type: "scales"},
		{From: objectID("PodDisruptionBudget", "payments", "api-budget"), To: objectID("Pod", "payments", "api-1"), Type: "protects"},
		{From: objectID("Pod", "payments", "api-1"), To: objectID("ConfigMap", "payments", "api-config"), Type: "references"},
		{From: objectID("Pod", "payments", "api-1"), To: objectID("Secret", "payments", "api-secret"), Type: "references"},
		{From: objectID("Pod", "payments", "api-1"), To: objectID("PersistentVolumeClaim", "payments", "api-data"), Type: "mounts"},
		{From: objectID("PersistentVolumeClaim", "payments", "api-data"), To: objectID("PersistentVolume", "", "pv-api"), Type: "binds"},
		{From: objectID("PersistentVolume", "", "pv-api"), To: objectID("StorageClass", "", "fast"), Type: "provisions"},
		{From: objectID("Deployment", "payments", "api"), To: objectID("Pod", "payments", "api-1"), Type: "owns"},
		{From: objectID("Widget", "payments", "owner"), To: objectID("ConfigMap", "payments", "api-config"), Type: "owns"},
	}
	for _, edge := range wanted {
		if !edges[edge] {
			t.Errorf("missing topology edge %#v; got %#v", edge, edges)
		}
	}
	if edges[TopologyEdge{From: objectID("Service", "payments", "api"), To: objectID("Pod", "payments", "api-1"), Type: "selects", Intent: true}] {
		t.Fatal("selector intent edge emitted despite observed EndpointSlice")
	}
}

func workloadFixture() map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"namespace": "payments", "name": "api", "generation": 7},
		"spec": map[string]any{
			"replicas": 3, "strategy": map[string]any{"type": "RollingUpdate"}, "selector": map[string]any{"matchLabels": map[string]any{"app": "api"}},
			"template": map[string]any{"spec": map[string]any{
				"terminationGracePeriodSeconds": 45,
				"affinity":                      map[string]any{"podAntiAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{}}}},
				"topologySpreadConstraints":     []any{map[string]any{"topologyKey": "zone", "whenUnsatisfiable": "DoNotSchedule"}},
				"containers":                    []any{map[string]any{"name": "api", "image": "example/api:v1", "readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/ready"}}, "resources": map[string]any{"requests": map[string]any{"cpu": "250m", "memory": "128Mi"}, "limits": map[string]any{"cpu": "1", "memory": "512Mi"}}}},
			}},
		},
		"status": map[string]any{"observedGeneration": 7, "replicas": 3, "readyReplicas": 2, "availableReplicas": 2, "unavailableReplicas": 1, "conditions": []any{map[string]any{"type": "Progressing", "status": "True", "reason": "NewReplicaSetAvailable"}}},
	}
}

func TestServiceSafeProjectionTopologyPartialAndLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "nodes", "kind": "Node", "verbs": []string{"get", "list"}},
				map[string]any{"name": "namespaces", "kind": "Namespace", "verbs": []string{"get", "list"}},
				map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/api/v1/namespaces/default/secrets/db":
			writeJSON(writer, map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": map[string]any{"namespace": "default", "name": "db", "managedFields": []any{"secret"}, "annotations": map[string]any{"kubectl.kubernetes.io/last-applied-configuration": "secret"}}, "data": map[string]any{"token": "c2VjcmV0LXRva2Vu"}})
		case "/api/v1/namespaces/default/secrets":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"namespace": "default", "name": "db"}, "data": map[string]any{"password": "c2VjcmV0"}}}})
		case "/api/v1/namespaces/default/pods", "/api/v1/pods":
			writeJSON(writer, map[string]any{"items": []any{podFixture("api-1")}})
		case "/api/v1/nodes":
			http.Error(writer, "raw forbidden status", http.StatusForbidden)
		case "/api/v1/namespaces":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"kind": "Namespace", "metadata": map[string]any{"name": "default"}}}})
		case "/api/v1/namespaces/default/pods/api-1/log":
			if request.Header.Get("Accept") != "*/*" {
				http.Error(writer, "unsupported media type", http.StatusNotAcceptable)
				return
			}
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = writer.Write([]byte("safe log line"))
		default:
			t.Errorf("unexpected Kubernetes request %s", request.URL.RequestURI())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	secret, err := service.Get(context.Background(), "core~v1~secrets", "default", "db")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(secret)
	if strings.Contains(string(encoded), "c2VjcmV0LXRva2Vu") || strings.Contains(string(encoded), "last-applied") || strings.Join(secret.Summary["keys"].([]string), ",") != "token" {
		t.Fatalf("unsafe secret projection: %s", encoded)
	}
	topology, err := service.Topology(context.Background(), "default", 20, 20)
	if err != nil || len(topology.Nodes) != 3 || len(topology.Edges) != 2 {
		t.Fatalf("topology = %#v err=%v", topology, err)
	}
	overview, err := service.Overview(context.Background())
	if err != nil || len(overview.Partial) == 0 || overview.Partial[0].Class != "forbidden" {
		t.Fatalf("overview = %#v err=%v", overview, err)
	}
	logs, err := service.PodLogs(context.Background(), "default", "api-1", "api", false, 60, 10)
	if err != nil || logs.Text != "safe log line" {
		t.Fatalf("logs = %#v err=%v", logs, err)
	}
}

func TestPodLogsOmitsEmptyContainerAndPreservesPreviousAndExplicitContainer(t *testing.T) {
	var requests []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Query())
		_, _ = writer.Write([]byte("log line"))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	for _, test := range []struct {
		container string
		previous  bool
	}{
		{"", false},
		{"", true},
		{"api", false},
	} {
		if _, err := service.PodLogs(context.Background(), "default", "api-1", test.container, test.previous, 60, 10); err != nil {
			t.Fatalf("PodLogs(%q, %v): %v", test.container, test.previous, err)
		}
	}
	if requests[0].Has("container") || requests[1].Has("container") {
		t.Fatalf("empty container query present: %v", requests[:2])
	}
	if requests[1].Get("previous") != "true" || requests[2].Get("container") != "api" {
		t.Fatalf("log queries = %v", requests)
	}
}

func TestPodLogsMapsMultiContainerBadRequestAndAcceptsExplicitContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("container") == "" {
			http.Error(writer, "a container name must be specified", http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte("sidecar log"))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	if _, err := service.PodLogs(context.Background(), "default", "multi", "", false, 60, 10); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("empty container error = %v", err)
	}
	logs, err := service.PodLogs(context.Background(), "default", "multi", "sidecar", false, 60, 10)
	if err != nil || logs.Container != "sidecar" || logs.Text != "sidecar log" {
		t.Fatalf("explicit container logs=%#v error=%v", logs, err)
	}
}

func TestParseQuantityIsExact(t *testing.T) {
	for input, want := range map[string]string{
		"250m": "1/4", "1.5Gi": "1610612736", "9007199254740993": "9007199254740993", "1u": "1/1000000",
		"2E": "2000000000000000000", "1Ei": "1152921504606846976", "12e3": "12000", "5e-3": "1/200", "1.25P": "1250000000000000",
	} {
		value, err := ParseQuantity(input)
		if err != nil || value.RatString() != want {
			t.Errorf("ParseQuantity(%q) = %v, %v; want %s", input, value, err, want)
		}
	}
	if _, err := ParseQuantity("not-a-quantity"); err == nil {
		t.Fatal("invalid quantity accepted")
	}
}

func TestServiceCachesPartialDiscoveryWithShortTTL(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			calls.Add(1)
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "test"})
	first, err := service.Discover(context.Background())
	if err != nil || len(first.Partial) != 1 {
		t.Fatalf("first discovery = %#v, %v", first, err)
	}
	if _, err := service.Discover(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatalf("partial discovery calls=%d err=%v", calls.Load(), err)
	}
}

func TestPartialDiscoveryIsReusedAcrossRepeatedLists(t *testing.T) {
	var discoveryCalls, listCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			discoveryCalls.Add(1)
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "optional.example", "preferredVersion": map[string]any{"version": "v1"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/optional.example/v1":
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
		case "/api/v1/namespaces/default/pods":
			listCalls.Add(1)
			writeJSON(writer, map[string]any{"items": []any{podFixture("api")}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "test"})
	for range 2 {
		if _, err := service.List(context.Background(), ListOptions{ResourceID: "core~v1~pods", Namespace: "default"}); err != nil {
			t.Fatal(err)
		}
	}
	if discoveryCalls.Load() != 1 || listCalls.Load() != 2 {
		t.Fatalf("calls discovery=%d list=%d, want 1/2", discoveryCalls.Load(), listCalls.Load())
	}
}

func TestSearchWithOptionalDiscoveryFailureHasBoundedRequestCount(t *testing.T) {
	var discoveryCalls, optionalCalls, listCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			discoveryCalls.Add(1)
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "optional.example", "preferredVersion": map[string]any{"version": "v1"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "services", "kind": "Service", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/apis/optional.example/v1":
			optionalCalls.Add(1)
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
		case "/api/v1/pods", "/api/v1/services":
			listCalls.Add(1)
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "next"}, "items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	result, err := newTestService(t, server.URL, Scope{ClusterID: "test"}).Search(context.Background(), SearchOptions{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if discoveryCalls.Load() != 1 || optionalCalls.Load() != 1 || listCalls.Load() != 2*maxSearchKindPages || result.Requests != int(listCalls.Load()) || result.Requests > result.RequestBudget || len(result.Partial) != 1 {
		t.Fatalf("calls discovery=%d optional=%d list=%d result=%#v", discoveryCalls.Load(), optionalCalls.Load(), listCalls.Load(), result)
	}
}

func TestSearchAppliesEligibilityBeforeKindCap(t *testing.T) {
	resources := make([]any, 0, 71)
	for index := 0; index < 70; index++ {
		resources = append(resources, map[string]any{"name": "resource" + strconv.Itoa(index), "kind": "Widget", "namespaced": true, "verbs": []string{"get", "list"}})
	}
	resources = append(resources, map[string]any{"name": "services", "kind": "Service", "namespaced": true, "verbs": []string{"get", "list"}})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": resources})
		case "/api/v1/namespaces/default/services":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"namespace": "default", "name": "needle-service"}}}})
		default:
			t.Fatalf("ineligible resource requested: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	result, err := newTestService(t, server.URL, Scope{ClusterID: "test"}).Search(context.Background(), SearchOptions{Query: "needle", Namespace: "default", Category: "network"})
	if err != nil || len(result.Items) != 1 || result.Items[0].Name != "needle-service" || result.Requests != 1 || result.Truncated || len(result.Omitted) != 0 {
		t.Fatalf("search = %#v err=%v", result, err)
	}
}

func TestTopologyIngestBudgetRetainsBreadthWithoutMaterializingPopulation(t *testing.T) {
	var podLimit, returnedItems atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "autoscaling", "preferredVersion": map[string]any{"version": "v2"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "services", "kind": "Service", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "persistentvolumeclaims", "kind": "PersistentVolumeClaim", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/apis/autoscaling/v2":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "horizontalpodautoscalers", "kind": "HorizontalPodAutoscaler", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/default/pods":
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			podLimit.Store(int32(limit))
			items := make([]any, 0, limit)
			for index := 0; index < limit; index++ {
				items = append(items, podFixture("pod-"+strconv.Itoa(index)))
			}
			returnedItems.Add(int32(len(items)))
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "more-of-10000"}, "items": items})
		case "/api/v1/namespaces/default/services":
			returnedItems.Add(1)
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"namespace": "default", "name": "api"}}}})
		case "/api/v1/namespaces/default/persistentvolumeclaims":
			returnedItems.Add(1)
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"namespace": "default", "name": "data"}}}})
		case "/apis/autoscaling/v2/namespaces/default/horizontalpodautoscalers":
			returnedItems.Add(1)
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler", "metadata": map[string]any{"namespace": "default", "name": "api-scale"}, "spec": map[string]any{"scaleTargetRef": map[string]any{"kind": "Deployment", "name": "api"}}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	topology, err := newTestService(t, server.URL, Scope{ClusterID: "test"}).Topology(context.Background(), "default", 8, 8)
	if err != nil || len(topology.Nodes) > 8 || !topology.Truncated {
		t.Fatalf("topology = %#v err=%v", topology, err)
	}
	if podLimit.Load() <= 0 || podLimit.Load() >= 10000 || returnedItems.Load() > 24 {
		t.Fatalf("pod limit=%d returned=%d, want bounded by nodeCap+2*edgeCap", podLimit.Load(), returnedItems.Load())
	}
	kinds := map[string]bool{}
	for _, node := range topology.Nodes {
		kinds[node.Kind] = true
	}
	for _, kind := range []string{"Pod", "Service", "HorizontalPodAutoscaler", "PersistentVolumeClaim"} {
		if !kinds[kind] {
			t.Errorf("representative %s missing from %#v", kind, topology.Nodes)
		}
	}
	if !containsString(topology.Omitted, "core~v1~pods") || !containsString(topology.Omitted, "category=pod") {
		t.Fatalf("topology omissions = %v", topology.Omitted)
	}
}

func TestTopologyNodeCapRetainsResourceBreadthAndOmissions(t *testing.T) {
	resources := []any{
		map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
		map[string]any{"name": "services", "kind": "Service", "namespaced": true, "verbs": []string{"get", "list"}},
		map[string]any{"name": "persistentvolumeclaims", "kind": "PersistentVolumeClaim", "namespaced": true, "verbs": []string{"get", "list"}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": resources})
		case "/api/v1/namespaces/default/pods":
			items := make([]any, 0, 20)
			for index := 0; index < 20; index++ {
				items = append(items, podFixture("pod-"+strconv.Itoa(index)))
			}
			writeJSON(writer, map[string]any{"items": items})
		case "/api/v1/namespaces/default/services":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"namespace": "default", "name": "api"}}}})
		case "/api/v1/namespaces/default/persistentvolumeclaims":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"namespace": "default", "name": "data"}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	topology, err := newTestService(t, server.URL, Scope{ClusterID: "test"}).Topology(context.Background(), "default", 4, 20)
	if err != nil || !topology.Truncated || len(topology.Nodes) != 4 {
		t.Fatalf("topology = %#v err=%v", topology, err)
	}
	kinds := map[string]bool{}
	for _, node := range topology.Nodes {
		kinds[node.Kind] = true
	}
	if !kinds["Pod"] || !kinds["Service"] || !kinds["PersistentVolumeClaim"] || !containsString(topology.Omitted, "core~v1~pods") {
		t.Fatalf("breadth kinds=%v omitted=%v", kinds, topology.Omitted)
	}
}

func TestTopologyExactNodeCapNamesOmittedResourceAndCategory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/default/pods":
			writeJSON(writer, map[string]any{"items": []any{podFixture("pod-1")}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	topology, err := newTestService(t, server.URL, Scope{ClusterID: "test"}).Topology(context.Background(), "default", 1, 20)
	if err != nil || !topology.Truncated || len(topology.Nodes) != 1 || !containsString(topology.Omitted, "core~v1~pods") || !containsString(topology.Omitted, "category=pod") {
		t.Fatalf("topology = %#v err=%v", topology, err)
	}
}

func TestServiceCacheKeyIsolatesOrgClusterAndCredential(t *testing.T) {
	client, err := NewClient(Config{Endpoint: "http://127.0.0.1", AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	a := NewService(client, Scope{OrgID: "a", ClusterID: "c", CredentialID: "x"}, time.Minute)
	b := NewService(client, Scope{OrgID: "b", ClusterID: "c", CredentialID: "x"}, time.Minute)
	c := NewService(client, Scope{OrgID: "a", ClusterID: "d", CredentialID: "y"}, time.Minute)
	if a.cacheKey() == b.cacheKey() || a.cacheKey() == c.cacheKey() {
		t.Fatal("cache scopes collide")
	}
}

func TestServiceRegistrySharesInstancesAndIsolatesScopedDiscovery(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			calls.Add(1)
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	base := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"})
	registry := NewServiceRegistry(base)
	orgA := registry.ResolveOrg("org-a")
	if orgA != base || registry.ResolveOrg("org-a") != orgA {
		t.Fatal("same scope did not reuse the service instance")
	}
	orgB := registry.ResolveOrg("org-b")
	clusterB := registry.Resolve(Scope{OrgID: "org-a", ClusterID: "cluster-b", CredentialID: "cred-b"})
	if orgB == orgA || clusterB == orgA || orgB.cacheKey() == orgA.cacheKey() || clusterB.cacheKey() == orgA.cacheKey() {
		t.Fatal("org/cluster/credential scopes collided")
	}
	for _, service := range []*Service{orgA, orgA, orgB, orgB, clusterB, clusterB} {
		if _, err := service.Discover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("discovery calls = %d, want one per scoped key", calls.Load())
	}
}

func newTestService(t *testing.T, endpoint string, scope Scope) *Service {
	t.Helper()
	client, err := NewClient(Config{Endpoint: endpoint, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return NewService(client, scope, time.Minute)
}
func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}
func podFixture(name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"namespace": "default",
			"name":      name,
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "api-abc",
			}},
		},
		"spec": map[string]any{
			"nodeName": "node-a",
			"containers": []any{map[string]any{
				"name": "api", "env": []any{map[string]any{"name": "TOKEN", "value": "secret"}},
			}},
		},
		"status": map[string]any{"phase": "Running"},
	}
}
func TestErrorClassDoesNotExposeStatus(t *testing.T) {
	if got := errorClass(errors.New("raw secret status")); got != "unavailable" {
		t.Fatal(got)
	}
}

type replacingScrubber struct{}

func (replacingScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, "secret", "[redacted]")
}

func TestServiceScrubsPodLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { _, _ = writer.Write([]byte("token=secret")) }))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "test"})
	service.SetScrubber(replacingScrubber{})
	logs, err := service.PodLogs(context.Background(), "default", "pod", "app", false, 60, 10)
	if err != nil || strings.Contains(logs.Text, "secret") {
		t.Fatalf("logs = %#v err=%v", logs, err)
	}
}

func TestServiceTruncatesOversizedPodLogsAtCallerCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(strings.Repeat("x", maxPodLogBytes+4096)))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "test"})
	logs, err := service.PodLogs(context.Background(), "default", "pod", "app", false, 60, 10)
	if err != nil || !logs.Truncated || len(logs.Text) != maxPodLogBytes {
		t.Fatalf("logs bytes=%d truncated=%v err=%v", len(logs.Text), logs.Truncated, err)
	}
}
