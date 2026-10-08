package kubernetes

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
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

func TestServiceDiscoveryFindsArgoAfterManyAPIgroups(t *testing.T) {
	groups := make([]any, 0, 71)
	for index := 0; index < 70; index++ {
		name := "extension-" + strconv.Itoa(index) + ".example.test"
		groups = append(groups, map[string]any{"name": name, "preferredVersion": map[string]any{"version": "v1"}})
	}
	groups = append(groups, map[string]any{"name": "argoproj.io", "preferredVersion": map[string]any{"version": "v1alpha1"}})
	var groupRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": groups})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis/argoproj.io/v1alpha1":
			groupRequests.Add(1)
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "applications", "kind": "Application", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "rollouts", "kind": "Rollout", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		default:
			if strings.HasPrefix(request.URL.Path, "/apis/extension-") {
				groupRequests.Add(1)
				writeJSON(writer, map[string]any{"resources": []any{}})
				return
			}
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	discovery, err := service.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !containsResource(discovery.Resources, "argoproj.io~v1alpha1~applications") || !containsResource(discovery.Resources, "argoproj.io~v1alpha1~rollouts") {
		t.Fatalf("Argo resources missing after %d API group requests: %#v", groupRequests.Load(), discovery)
	}
	if len(discovery.Partial) != 0 {
		t.Fatalf("discovery was partial despite reachable API groups: %#v", discovery.Partial)
	}
}

func TestListAllRetriesOversizedPageAndKeepsReducedLimit(t *testing.T) {
	var requestedLimits []string
	var returnedItems []any
	for index := 0; index < 25; index++ {
		returnedItems = append(returnedItems, map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "deployment-" + strconv.Itoa(index), "namespace": "default", "annotations": map[string]string{"payload": strings.Repeat("x", 240)}},
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "apps", "preferredVersion": map[string]any{"version": "v1"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1/namespaces/default/deployments":
			requestedLimits = append(requestedLimits, request.URL.Query().Get("limit"))
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			start, _ := strconv.Atoi(request.URL.Query().Get("continue"))
			end := min(start+limit, len(returnedItems))
			continuation := ""
			if end < len(returnedItems) {
				continuation = strconv.Itoa(end)
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": continuation}, "items": returnedItems[start:end]})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	service.client.maxBodyBytes = 4096
	page, err := service.listAll(context.Background(), ListOptions{ResourceID: "apps~v1~deployments", Namespace: "default"})
	if err != nil || len(page.Items) != len(returnedItems) || page.Truncated {
		t.Fatalf("list items=%d truncated=%v err=%v limits=%v", len(page.Items), page.Truncated, err, requestedLimits)
	}
	if len(requestedLimits) < 4 || requestedLimits[0] != "100" || requestedLimits[len(requestedLimits)-1] != "10" {
		t.Fatalf("LIST limits did not fall back and persist: %v", requestedLimits)
	}
	for _, limit := range requestedLimits[len(requestedLimits)-2:] {
		if limit != "10" {
			t.Fatalf("later page did not retain the reduced limit: %v", requestedLimits)
		}
	}
}

func containsResource(resources []ResourceDefinition, id string) bool {
	for _, resource := range resources {
		if resource.ID == id {
			return true
		}
	}
	return false
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

func TestWorkloadsFiltersCountsAndPaginatesSharedIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{
				map[string]any{"name": "apps", "preferredVersion": map[string]string{"version": "v1"}},
				map[string]any{"name": "batch", "preferredVersion": map[string]string{"version": "v1"}},
			}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "statefulsets", "kind": "StatefulSet", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "daemonsets", "kind": "DaemonSet", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/apis/batch/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "jobs", "kind": "Job", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "cronjobs", "kind": "CronJob", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/api/v1/pods":
			writeJSON(writer, map[string]any{"items": []any{workloadListFixture("Pod", "checkout-pod"), workloadListFixture("Pod", "other-pod")}})
		case "/apis/apps/v1/deployments":
			writeJSON(writer, map[string]any{"items": []any{workloadListFixture("Deployment", "checkout-b"), workloadListFixture("Deployment", "checkout-a")}})
		case "/apis/apps/v1/statefulsets", "/apis/apps/v1/daemonsets", "/apis/batch/v1/jobs", "/apis/batch/v1/cronjobs":
			writeJSON(writer, map[string]any{"items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	page, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Query: "CHECK", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Kind != "Deployment" || page.Items[0].Name != "checkout-a" || page.Next != "1" || !page.Truncated {
		t.Fatalf("first workload page = %#v err=%v", page, err)
	}
	wantCounts := map[string]int{"Deployment": 2, "StatefulSet": 0, "DaemonSet": 0, "Job": 0, "CronJob": 0, "Pod": 1}
	if !reflect.DeepEqual(page.Counts, wantCounts) {
		t.Fatalf("workload counts = %#v, want %#v", page.Counts, wantCounts)
	}
	page, err = service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Query: "check", Limit: 1, Cursor: page.Next})
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "checkout-b" || page.Next != "2" {
		t.Fatalf("second workload page = %#v err=%v", page, err)
	}
	page, err = service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Query: "check", Limit: 1, Cursor: page.Next})
	if err != nil || len(page.Items) != 1 || page.Items[0].Kind != "Pod" || page.Next != "" || page.Truncated {
		t.Fatalf("last workload page = %#v err=%v", page, err)
	}
}

func TestListEventsUsesNextCursorAndNeverNullItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "events", "kind": "Event", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/shop/events":
			if request.URL.Query().Get("fieldSelector") != "type=Warning" {
				t.Errorf("event field selector = %q", request.URL.Query().Get("fieldSelector"))
			}
			if request.URL.Query().Get("continue") == "event-page-2" {
				writeJSON(writer, map[string]any{"items": []any{map[string]any{"kind": "Event", "metadata": map[string]any{"name": "second", "namespace": "shop"}, "type": "Warning"}}})
				return
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "event-page-2"}, "items": []any{map[string]any{"kind": "Event", "metadata": map[string]any{"name": "first", "namespace": "shop"}, "type": "Warning"}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	page, err := service.ListEvents(t.Context(), EventOptions{Namespace: "shop", Type: "Warning", Limit: 1})
	if err != nil || page.Items == nil || len(page.Items) != 1 || page.Items[0].Name != "first" || page.Next != "event-page-2" || !page.Truncated {
		t.Fatalf("first event page = %#v err=%v", page, err)
	}
	page, err = service.ListEvents(t.Context(), EventOptions{Namespace: "shop", Type: "Warning", Limit: 1, Cursor: page.Next})
	if err != nil || page.Items == nil || len(page.Items) != 1 || page.Items[0].Name != "second" || page.Next != "" || page.Truncated {
		t.Fatalf("second event page = %#v err=%v", page, err)
	}
}

func workloadListFixture(kind, name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": kind,
		"metadata": map[string]any{"uid": kind + "/" + name, "namespace": "shop", "name": name},
		"status":   map[string]any{"phase": "Running"},
	}
}

func TestListWorkloadsRetriesOversizedPagesAndKeepsReducedLimit(t *testing.T) {
	var requestedLimits []string
	var returnedItems []any
	for index := 0; index < 25; index++ {
		returnedItems = append(returnedItems, map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "deployment-" + strconv.Itoa(index), "namespace": "default", "annotations": map[string]string{"payload": strings.Repeat("x", 240)}},
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "apps", "preferredVersion": map[string]any{"version": "v1"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1/namespaces/default/deployments":
			requestedLimits = append(requestedLimits, request.URL.Query().Get("limit"))
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			start, _ := strconv.Atoi(request.URL.Query().Get("continue"))
			end := min(start+limit, len(returnedItems))
			continuation := ""
			if end < len(returnedItems) {
				continuation = strconv.Itoa(end)
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": continuation}, "items": returnedItems[start:end]})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	service.client.maxBodyBytes = 4096
	page, err := service.ListWorkloads(context.Background(), "default", "Deployment", 25)
	if err != nil || len(page.Items) != len(returnedItems) || page.Truncated {
		t.Fatalf("workloads=%d truncated=%v err=%v limits=%v", len(page.Items), page.Truncated, err, requestedLimits)
	}
	if len(requestedLimits) < 4 || requestedLimits[0] != "25" || requestedLimits[2] != "10" || requestedLimits[3] != "10" {
		t.Fatalf("workload LIST limits did not fall back and persist: %v", requestedLimits)
	}
}

func TestWorkloadLogsEncodeEmptyCollectionsAndClassifyUnavailablePreviousLogs(t *testing.T) {
	var includePod atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "apps", "preferredVersion": map[string]any{"version": "v1"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1/namespaces/default/deployments/checkout":
			writeJSON(writer, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "checkout", "namespace": "default"}, "spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]string{"app": "checkout"}}}})
		case "/api/v1/namespaces/default/pods":
			items := []any{}
			if includePod.Load() {
				pod := podFixture("checkout-pod")
				metadata := pod["metadata"].(map[string]any)
				metadata["labels"] = map[string]string{"app": "checkout"}
				metadata["ownerReferences"] = []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "checkout"}}
				items = append(items, pod)
			}
			writeJSON(writer, map[string]any{"items": items})
		default:
			if strings.HasSuffix(request.URL.Path, "/log") && request.URL.Query().Get("previous") == "true" {
				http.Error(writer, `previous terminated container "app" not found`, http.StatusBadRequest)
				return
			}
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	options := WorkloadLogOptions{Namespace: "default", Kind: "Deployment", Name: "checkout", Previous: true}

	empty, err := service.WorkloadLogs(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"lines", "pods", "omitted_pods"} {
		if strings.TrimSpace(string(fields[field])) != "[]" {
			t.Errorf("%s = %s, want [] in %s", field, fields[field], encoded)
		}
	}

	includePod.Store(true)
	logs, err := service.WorkloadLogs(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	for _, partial := range logs.Partial {
		if partial.Scope == "logs" && partial.Class == "previous_unavailable" {
			return
		}
	}
	t.Fatalf("missing previous container was not reported as a partial failure: %#v", logs.Partial)
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
		name              string
		nodeMetrics       bool
		invalidPodMetrics bool
		wantCPU           string
		wantMemory        string
		wantSource        string
		wantUsageFresh    bool
		wantOverviewFresh bool
		wantMetricsStatus string
	}{
		{name: "node metrics preferred", nodeMetrics: true, wantCPU: "1", wantMemory: "2147483648", wantSource: "node_metrics", wantOverviewFresh: true, wantMetricsStatus: "available"},
		{name: "pod metrics fallback", wantCPU: "1/4", wantMemory: "1073741824", wantSource: "pod_metrics", wantUsageFresh: true, wantOverviewFresh: true, wantMetricsStatus: "available"},
		{name: "invalid pod quantity", invalidPodMetrics: true, wantSource: "pod_metrics", wantMetricsStatus: "partial"},
	} {
		t.Run(test.name, func(t *testing.T) {
			timestamp := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
			podCPU := "250m"
			if test.invalidPodMetrics {
				podCPU = "invalid"
			}
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
					writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics", "metadata": map[string]any{"namespace": "payments", "name": "api-1"}, "timestamp": podTimestamp, "window": "30s", "containers": []any{map[string]any{"usage": map[string]any{"cpu": podCPU, "memory": "1Gi"}}}}}})
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
			if err != nil || usage.Availability == "unavailable" || usage.Fresh != test.wantUsageFresh || len(usage.Pods) != 1 || len(usage.Nodes) != wantNodes || usage.PodMetrics.Total != 1 || usage.PodMetrics.Complete == test.invalidPodMetrics {
				t.Fatalf("usage = %#v err=%v", usage, err)
			}
			if !test.invalidPodMetrics && (usage.Pods[0].CPU != "1/4" || usage.PodMetrics.CPU != "1/4") {
				t.Fatalf("pod metrics sample = %#v source=%#v", usage.Pods[0], usage.PodMetrics)
			}
			if test.invalidPodMetrics && (usage.Fresh || usage.PodMetrics.Fresh || usage.PodMetrics.Availability != "partial") {
				t.Fatalf("invalid pod metrics were reported fresh: %#v", usage)
			}
			if test.nodeMetrics && (usage.NodeMetrics.Total != 1 || !usage.NodeMetrics.Fresh || usage.NodeMetrics.CPU != "1") {
				t.Fatalf("node metrics = %#v", usage.NodeMetrics)
			}
			overview, err := service.Overview(context.Background())
			if err != nil || overview.MetricsStatus != test.wantMetricsStatus || overview.MetricsFresh != test.wantOverviewFresh || overview.UsageCPU != test.wantCPU || overview.UsageMemory != test.wantMemory || overview.UsageSource != test.wantSource || (!test.invalidPodMetrics && overview.MetricsObservedAt == nil) || (test.invalidPodMetrics && overview.MetricsObservedAt != nil) {
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
			if request.URL.Query().Get("limit") != strconv.Itoa(listAllPageSize) {
				t.Errorf("metrics page limit = %q", request.URL.Query().Get("limit"))
			}
			start := 0
			if continuation := request.URL.Query().Get("continue"); continuation != "" {
				start, _ = strconv.Atoi(continuation)
			}
			end := min(start+listAllPageSize, len(metrics))
			metadata := map[string]any{}
			if end < len(metrics) {
				metadata["continue"] = strconv.Itoa(end)
			}
			writeJSON(writer, map[string]any{"metadata": metadata, "items": metrics[start:end]})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	usage, err := service.Usage(context.Background(), "", 500)
	if err != nil || !usage.Truncated || len(usage.Nodes) != 500 || usage.NodeMetrics.Total != 501 || !usage.NodeMetrics.Complete || usage.NodeMetrics.Fresh || usage.NodeMetrics.Availability != "stale" || usage.NodeMetrics.CPU != "501" || !containsString(usage.Omitted, "metrics.k8s.io~v1beta1~nodes") || len(usage.Partial) != 1 {
		t.Fatalf("usage = %#v err=%v", usage, err)
	}
	overview, err := service.Overview(context.Background())
	if err != nil || overview.UsageSource != "node_metrics" || overview.MetricsFresh || overview.MetricsStatus != "stale" || overview.UsageCPU != "501" || !overview.Truncated || !containsString(overview.Omitted, "metrics.k8s.io~v1beta1~nodes") {
		t.Fatalf("overview = %#v err=%v", overview, err)
	}
}

func TestOverviewDoesNotAggregateIncompleteMetrics(t *testing.T) {
	timestamp := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	metrics := []any{
		map[string]any{"kind": "PodMetrics", "metadata": map[string]any{"namespace": "shop", "name": "pod-a"}, "timestamp": timestamp, "containers": []any{map[string]any{"usage": map[string]any{"cpu": "1", "memory": "1Gi"}}}},
		map[string]any{"kind": "PodMetrics", "metadata": map[string]any{"namespace": "shop", "name": "pod-b"}, "timestamp": timestamp, "containers": []any{map[string]any{"usage": map[string]any{"cpu": "2", "memory": "2Gi"}}}},
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
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "PodMetrics", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/metrics.k8s.io/v1beta1/pods":
			writeJSON(writer, map[string]any{"items": metrics})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	if _, err := service.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := withOperationBudget(context.Background(), time.Second, 256, 32<<20, 1)
	defer cancel()
	overview, err := service.Overview(ctx)
	if err != nil || overview.UsageSource != "pod_metrics" || overview.MetricsStatus != "partial" || overview.MetricsFresh || overview.UsageCPU != "" || overview.UsageMemory != "" {
		t.Fatalf("incomplete metrics overview=%+v err=%v", overview, err)
	}
}

func TestOverviewCountsPodsAcrossSafeInternalPages(t *testing.T) {
	const podCount = 6501
	var podRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/pods":
			podRequests.Add(1)
			if request.URL.Query().Get("limit") != strconv.Itoa(listAllPageSize) {
				t.Errorf("pod page limit = %q", request.URL.Query().Get("limit"))
			}
			start := 0
			if continuation := request.URL.Query().Get("continue"); continuation != "" {
				start, _ = strconv.Atoi(continuation)
			}
			end := min(start+listAllPageSize, podCount)
			items := make([]any, 0, end-start)
			for index := start; index < end; index++ {
				items = append(items, podFixture("pod-"+strconv.Itoa(index)))
			}
			metadata := map[string]any{}
			if end < podCount {
				metadata["continue"] = strconv.Itoa(end)
			}
			writeJSON(writer, map[string]any{"metadata": metadata, "items": items})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	service.indexes = nil
	overview, err := service.Overview(context.Background())
	if err != nil || overview.Pods != podCount || podRequests.Load() != 66 || containsString(overview.Omitted, "core~v1~pods") || containsString(overview.Omitted, "budget_exhausted") {
		t.Fatalf("overview=%#v pod requests=%d err=%v", overview, podRequests.Load(), err)
	}
	for _, partial := range overview.Partial {
		if partial.Class == "budget_exhausted" {
			t.Fatalf("overview unexpectedly exhausted its request budget: %#v", overview)
		}
	}
}

func TestOverviewMarksFailedPodCollectionUnavailableAndPreservesDiscoveryPartial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "optional.example.io", "preferredVersion": map[string]any{"version": "v1"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/optional.example.io/v1":
			http.Error(writer, "forbidden", http.StatusForbidden)
		case "/api/v1/pods":
			http.Error(writer, "response too large", http.StatusRequestEntityTooLarge)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	overview, err := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"}).Overview(context.Background())
	if err != nil || overview.Pods != 0 || !overview.Truncated || !overview.Sync.Partial || overview.Sync.State != "direct" || !containsString(overview.Omitted, "core~v1~pods") {
		t.Fatalf("overview=%#v err=%v", overview, err)
	}
	foundPodFailure, foundDiscoveryFailure := false, false
	for _, partial := range overview.Partial {
		foundPodFailure = foundPodFailure || partial.ResourceID == "core~v1~pods"
		foundDiscoveryFailure = foundDiscoveryFailure || partial.Scope == "discovery"
	}
	if !foundPodFailure || !foundDiscoveryFailure {
		t.Fatalf("partial failures=%#v", overview.Partial)
	}
}

func TestOverviewReadsMetricsBeforeInventoryBudgetIsExhausted(t *testing.T) {
	timestamp := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	var eventRequests atomic.Int32
	var podMetricRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{
				map[string]any{"name": "metrics.k8s.io", "preferredVersion": map[string]string{"version": "v1beta1"}},
				map[string]any{"name": "apps", "preferredVersion": map[string]string{"version": "v1"}},
			}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "nodes", "kind": "Node", "verbs": []string{"get", "list"}},
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "events", "kind": "Event", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "namespaces", "kind": "Namespace", "verbs": []string{"get", "list"}},
			}})
		case "/apis/metrics.k8s.io/v1beta1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "nodes", "kind": "NodeMetrics", "verbs": []string{"get", "list"}},
				map[string]any{"name": "pods", "kind": "PodMetrics", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "statefulsets", "kind": "StatefulSet", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/apis/metrics.k8s.io/v1beta1/nodes":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"kind": "NodeMetrics", "metadata": map[string]any{"name": "node-a"}, "timestamp": timestamp, "window": "30s", "usage": map[string]any{"cpu": "2", "memory": "2Gi"}}}})
		case "/apis/metrics.k8s.io/v1beta1/pods":
			podMetricRequests.Add(1)
			continuation, _ := strconv.Atoi(request.URL.Query().Get("continue"))
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": strconv.Itoa(continuation + 1)}, "items": []any{map[string]any{"kind": "PodMetrics", "metadata": map[string]any{"namespace": "shop", "name": "pod-a"}, "timestamp": timestamp, "containers": []any{map[string]any{"usage": map[string]any{"cpu": "1", "memory": "1Gi"}}}}}})
		case "/api/v1/events":
			eventRequests.Add(1)
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"kind": "Event", "type": "Warning"}}})
		default:
			writeJSON(writer, map[string]any{"items": []any{}})
		}
	}))
	defer server.Close()

	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	service.indexes = nil
	if _, err := service.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := withOperationBudget(context.Background(), time.Second, 4, 1<<20, 100)
	defer cancel()
	overview, err := service.Overview(ctx)
	if err != nil || overview.MetricsStatus != "available" || !overview.MetricsFresh || overview.UsageSource != "node_metrics" || overview.UsageCPU != "2" {
		t.Fatalf("overview metrics=%+v err=%v", overview, err)
	}
	if podMetricRequests.Load() != 3 {
		t.Fatalf("pod metrics requests=%d; node metrics should be read before the constrained pod collection", podMetricRequests.Load())
	}
	if overview.Warnings != 0 || eventRequests.Load() != 0 {
		t.Fatalf("overview warnings=%d event requests=%d; dashboard must not fetch duplicate Events", overview.Warnings, eventRequests.Load())
	}
	budgetExhausted := false
	for _, partial := range overview.Partial {
		budgetExhausted = budgetExhausted || partial.Class == "budget_exhausted"
	}
	if !overview.Truncated || !budgetExhausted {
		t.Fatalf("inventory budget exhaustion was not surfaced: %+v", overview)
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

func TestPodLogContainerNamesProjection(t *testing.T) {
	pod := podFixture("pod")
	spec := pod["spec"].(map[string]any)
	spec["initContainers"] = []any{map[string]any{"name": "setup", "env": []any{map[string]any{"value": "must-not-cross"}}}}
	spec["ephemeralContainers"] = []any{map[string]any{"name": "debug", "env": []any{map[string]any{"value": "must-not-cross"}}}}
	summary := safeSummary("Pod", pod, replacingScrubber{})
	containers, ok := summary["log_containers"].([]map[string]string)
	if !ok || len(containers) != 3 || containers[0]["type"] != "regular" || containers[1]["type"] != "init" || containers[2]["type"] != "ephemeral" {
		t.Fatalf("container names=%v", containers)
	}
	encoded, _ := json.Marshal(containers)
	if strings.Contains(string(encoded), "must-not-cross") || strings.Contains(string(encoded), "secret") {
		t.Fatal("raw environment crossed the log container projection")
	}
	var oversized []any
	for index := 0; index < maxProjectionItems+1; index++ {
		oversized = append(oversized, map[string]any{"name": "container-" + strconv.Itoa(index)})
	}
	spec["containers"] = oversized
	summary = safeSummary("Pod", pod, nil)
	if len(summary["log_containers"].([]map[string]string)) != maxProjectionItems || summary["log_containers_truncated"] != true {
		t.Fatal("container projection is unbounded")
	}
	if _, found := safeSummary("Deployment", pod, nil)["log_containers"]; found {
		t.Fatal("non-Pod log navigation metadata added")
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
	service.indexes = nil
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

func TestServiceSafeProjectionPartialOverviewAndLogs(t *testing.T) {
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

func logStreamTestContext() context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
}

func TestPodLogStreamFramingResumeAndScope(t *testing.T) {
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries = append(queries, request.URL.Query())
		for _, chunk := range []string{"2026-10-01T00:00:00Z token=sec", "ret\n2026-10-01T00:00:00Z repeated\n", "2026-10-01T00:00:00Z repeated\n2026-10-01T00:00:01Z final"} {
			writer.Write([]byte(chunk))
			writer.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	options := PodLogStreamOptions{Namespace: "default", Pod: "pod", Container: "app"}
	var events []PodLogStreamEvent
	collect := func(event PodLogStreamEvent) error { events = append(events, event); return nil }
	if err := service.StreamPodLogs(logStreamTestContext(), options, collect); err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || strings.Contains(events[0].Text, "secret") || events[1].Text != events[2].Text || events[2].Ordinal != 3 || events[4].Event != "end" || queries[0].Get("follow") != "true" || queries[0].Get("timestamps") != "true" || queries[0].Get("tailLines") != "500" {
		t.Fatalf("events=%+v queries=%v", events, queries)
	}
	encoded, _ := json.Marshal(logResumeCursor{Target: service.logTarget(options), Timestamp: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Ordinal: 2})
	options.Cursor = base64.RawURLEncoding.EncodeToString(encoded)
	events = nil
	if err := service.StreamPodLogs(logStreamTestContext(), options, collect); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Text != "repeated" || events[0].Ordinal != 3 || queries[1].Get("sinceTime") == "" || queries[1].Has("tailLines") {
		t.Fatalf("resume events=%+v", events)
	}
	other := service.Scoped(Scope{OrgID: "org-b", ClusterID: "cluster"})
	if err := other.ValidatePodLogStream(options); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("cross-org cursor accepted: %v", err)
	}
	options.Container = "other"
	if err := service.ValidatePodLogStream(options); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("cross-container cursor accepted: %v", err)
	}
}

func TestPodLogStreamLimitsAndPrevious(t *testing.T) {
	for _, test := range []struct {
		name, payload, reason string
		duration, idle        time.Duration
		bytes                 int64
	}{
		{"bytes", "2026-10-01T00:00:00Z secret\n", "bytes", time.Second, time.Second, 10},
		{"encoded-bytes", "2026-10-01T00:00:00Z secret\n", "bytes", time.Second, time.Second, 100},
		{"quiet-follow", "", "duration", 40 * time.Millisecond, 10 * time.Millisecond, MaxLogStreamBytes},
		{"duration", "", "duration", 20 * time.Millisecond, time.Second, MaxLogStreamBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(200)
				writer.(http.Flusher).Flush()
				if test.payload != "" {
					writer.Write([]byte(test.payload))
					return
				}
				<-request.Context().Done()
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{})
			service.SetScrubber(replacingScrubber{})
			var events []PodLogStreamEvent
			err := service.streamPodLogs(context.Background(), PodLogStreamOptions{Namespace: "default", Pod: "pod"}, func(event PodLogStreamEvent) error { events = append(events, event); return nil }, test.duration, test.idle, 5*time.Millisecond, test.bytes)
			if err != nil || len(events) < 2 || events[len(events)-2].Event != "limit" || events[len(events)-2].Reason != test.reason || events[len(events)-1].Event != "end" {
				t.Fatalf("events=%+v err=%v", events, err)
			}
			for _, event := range events {
				if event.Event == "line" {
					t.Fatal("partial or oversized line escaped")
				}
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("follow") != "false" || request.URL.Query().Get("previous") != "true" {
			t.Error("previous used follow")
		}
		writer.WriteHeader(400)
		writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"previous terminated container \"app\" in pod \"pod\" not found"}`))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{})
	service.SetScrubber(replacingScrubber{})
	err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "default", Pod: "pod", Previous: true}, func(PodLogStreamEvent) error { return nil })
	if !errors.Is(err, errPreviousUnavailable) || strings.Contains(DiagnoseError(err).Message, "secret") {
		t.Fatalf("previous error=%v", err)
	}
}

func TestPodLogStreamOversizedRecordsContinue(t *testing.T) {
	for _, size := range []int{MaxLogStreamLineBytes, MaxLogStreamLineBytes + 1, 3*MaxLogStreamLineBytes + 7} {
		for _, finalNewline := range []bool{false, true} {
			t.Run(strconv.Itoa(size)+"-"+strconv.FormatBool(finalNewline), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					writer.Write([]byte("2026-10-01T00:00:00Z " + strings.Repeat("x", size) + "secret\xff\n"))
					writer.Write([]byte("2026-10-01T00:00:01Z ordinary secret\n"))
					writer.Write([]byte("2026-10-01T00:00:02Z " + strings.Repeat("x", size) + "secret"))
					if finalNewline {
						writer.Write([]byte("\n"))
					}
				}))
				defer server.Close()
				service := newTestService(t, server.URL, Scope{})
				service.SetScrubber(replacingScrubber{})
				var events []PodLogStreamEvent
				err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "default", Pod: "pod"}, func(event PodLogStreamEvent) error { events = append(events, event); return nil })
				if err != nil || len(events) != 4 || events[0].Text != "[oversized log line omitted]" || events[1].Text != "ordinary [redacted]" || events[2].Text != "[oversized log line omitted]" || events[3].Reason != "complete" || events[3].Cursor == "" || events[2].Cursor != "" {
					t.Fatalf("oversized continuation: %+v err=%v", events, err)
				}
				for _, event := range events {
					if strings.Contains(event.Text, "secret") || strings.Contains(event.Text, "xxx") || event.Event == "limit" {
						t.Fatal("oversized fragment escaped or stopped stream")
					}
				}
			})
		}
	}
}

func TestPodLogStreamExactLineBoundaryAndLimitCursor(t *testing.T) {
	for _, size := range []int{MaxLogStreamLineBytes, MaxLogStreamLineBytes + 1} {
		prefix := "2026-10-01T00:00:00Z "
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Write([]byte(prefix + strings.Repeat("x", size-len(prefix)-1) + "\n"))
		}))
		service := newTestService(t, server.URL, Scope{})
		service.SetScrubber(replacingScrubber{})
		var events []PodLogStreamEvent
		err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "default", Pod: "pod"}, func(event PodLogStreamEvent) error { events = append(events, event); return nil })
		server.Close()
		if err != nil || len(events) != 2 {
			t.Fatalf("line boundary %d: %+v err=%v", size, events, err)
		}
		if size == MaxLogStreamLineBytes && events[0].Text != strings.Repeat("x", size-len(prefix)-1) || size > MaxLogStreamLineBytes && events[0].Text != "[oversized log line omitted]" {
			t.Fatalf("incorrect exact line boundary %d", size)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("2026-10-01T00:00:00Z safe\n2026-10-01T00:00:01Z " + strings.Repeat("x", 2000) + "\n"))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{})
	service.SetScrubber(replacingScrubber{})
	var events []PodLogStreamEvent
	err := service.streamPodLogs(context.Background(), PodLogStreamOptions{Namespace: "default", Pod: "pod"}, func(event PodLogStreamEvent) error { events = append(events, event); return nil }, time.Second, time.Second, time.Second, 1500)
	if err != nil || len(events) != 3 || events[0].Event != "line" || events[1].Event != "limit" || events[1].Reason != "bytes" || events[1].Retryable || events[1].Cursor == "" || events[2].Cursor != events[1].Cursor {
		t.Fatalf("limit cursor advanced past emitted data: %+v err=%v", events, err)
	}
	cursor, cursorErr := service.logCursor(PodLogStreamOptions{Namespace: "default", Pod: "pod", Cursor: events[2].Cursor})
	if cursorErr != nil || cursor.Timestamp.Format(time.RFC3339Nano) != events[0].Timestamp || cursor.Ordinal != events[0].Ordinal {
		t.Fatalf("limit cursor includes undelivered data: %+v %v", cursor, cursorErr)
	}
}

func TestPodLogStreamOversizedMalformedRecordContinues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte(strings.Repeat("secret", MaxLogStreamLineBytes) + "\n2026-10-01T00:00:00Z ordinary secret\n"))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{})
	service.SetScrubber(replacingScrubber{})
	var events []PodLogStreamEvent
	err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "default", Pod: "pod"}, func(event PodLogStreamEvent) error { events = append(events, event); return nil })
	if err != nil || len(events) != 3 || events[0].Text != "[oversized log line omitted]" || events[0].Cursor != "" || !events[0].ReplayUncertain || events[1].Text != "ordinary [redacted]" {
		t.Fatalf("malformed oversized record: %+v err=%v", events, err)
	}
}

func TestPodLogStreamDiscardBudgetAndCancellation(t *testing.T) {
	for _, cancelStream := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelStream), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			closed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				defer close(closed)
				writer.Write([]byte("2026-10-01T00:00:00Z " + strings.Repeat("secret", MaxLogStreamLineBytes)))
				writer.(http.Flusher).Flush()
				if cancelStream {
					cancel()
				}
				<-request.Context().Done()
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{})
			service.SetScrubber(replacingScrubber{})
			var events []PodLogStreamEvent
			budget := int64(2 * MaxLogStreamLineBytes)
			if cancelStream {
				budget = MaxLogStreamBytes
			}
			err := service.streamPodLogs(ctx, PodLogStreamOptions{Namespace: "default", Pod: "pod"}, func(event PodLogStreamEvent) error { events = append(events, event); return nil }, time.Second, time.Second, time.Second, budget)
			if cancelStream {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("discard cancellation: %v", err)
				}
			} else if err != nil || len(events) != 2 || events[0].Event != "limit" || events[0].Reason != "bytes" {
				t.Fatalf("discard budget: %+v err=%v", events, err)
			}
			for _, event := range events {
				if event.Event == "line" {
					t.Fatal("partial discarded record emitted")
				}
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("discard reader survived cancellation")
			}
		})
	}
}

func TestPodLogStreamInterleavedOccurrenceResume(t *testing.T) {
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries = append(queries, request.URL.Query())
		writer.Write([]byte("2026-10-01T00:00:01Z repeated\n2026-10-01T00:00:00Z repeated\n2026-10-01T00:00:01Z repeated\n2026-10-01T00:00:00Z repeated\n2026-10-01T00:00:02Z final\n"))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{})
	service.SetScrubber(replacingScrubber{})
	options := PodLogStreamOptions{Namespace: "default", Pod: "pod"}
	var events []PodLogStreamEvent
	collect := func(event PodLogStreamEvent) error { events = append(events, event); return nil }
	if err := service.StreamPodLogs(logStreamTestContext(), options, collect); err != nil || len(events) != 6 || events[2].Ordinal != 2 || events[3].Ordinal != 2 {
		t.Fatalf("interleaved: %+v err=%v", events, err)
	}
	encoded, _ := json.Marshal(logResumeCursor{Target: service.logTarget(options), Timestamp: time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC), Ordinal: 2, Occurrences: map[string]uint64{"2026-10-01T00:00:00Z": 1, "2026-10-01T00:00:01Z": 2}})
	options.Cursor = base64.RawURLEncoding.EncodeToString(encoded)
	events = nil
	if err := service.StreamPodLogs(logStreamTestContext(), options, collect); err != nil || len(events) != 3 || events[0].Timestamp != "2026-10-01T00:00:00Z" || events[0].Ordinal != 2 || events[0].Text != "repeated" || events[0].ReplayUncertain || events[2].ReplayUncertain {
		t.Fatalf("interleaved resume: %+v err=%v", events, err)
	}
	if queries[1].Get("sinceTime") != "2026-09-30T23:59:59.999999999Z" || queries[1].Has("tailLines") {
		t.Fatalf("overlap query: %v", queries[1])
	}
	options.Cursor = events[2].Cursor
	events = nil
	if err := service.StreamPodLogs(logStreamTestContext(), options, collect); err != nil || len(events) != 1 || events[0].Event != "end" || events[0].ReplayUncertain {
		t.Fatalf("successive resume lost identities: %+v err=%v", events, err)
	}
}

func TestPodLogStreamRetainedResumeBoundary(t *testing.T) {
	for _, spacing := range []time.Duration{time.Second, 3 * time.Second} {
		t.Run(spacing.String(), func(t *testing.T) {
			base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				since, _ := time.Parse(time.RFC3339Nano, request.URL.Query().Get("sinceTime"))
				for index := 0; index < 140; index++ {
					timestamp := base.Add(time.Duration(index) * spacing)
					if timestamp.Before(since) {
						continue
					}
					writer.Write([]byte(timestamp.Format(time.RFC3339Nano) + " repeated\n"))
				}
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{})
			service.SetScrubber(replacingScrubber{})
			options := PodLogStreamOptions{Namespace: "default", Pod: "pod"}
			var final PodLogStreamEvent
			lines := 0
			collect := func(event PodLogStreamEvent) error {
				final = event
				if event.Event == "line" {
					lines++
				}
				return nil
			}
			if err := service.StreamPodLogs(logStreamTestContext(), options, collect); err != nil || lines != 140 || final.ReplayUncertain != (spacing == time.Second) {
				t.Fatalf("initial lines=%d final=%+v err=%v", lines, final, err)
			}
			options.Cursor = final.Cursor
			lines = 0
			if err := service.StreamPodLogs(logStreamTestContext(), options, collect); err != nil || lines != 0 || final.ReplayUncertain {
				t.Fatalf("resume lines=%d final=%+v err=%v", lines, final, err)
			}
		})
	}
}

func TestPodLogStreamCheckpointBandwidth(t *testing.T) {
	const lineCount = 10000
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		for index := 0; index < lineCount; index++ {
			writer.Write([]byte(base.Add(time.Duration(index)*time.Millisecond).Format(time.RFC3339Nano) + " x\n"))
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{})
	service.SetScrubber(replacingScrubber{})
	options := PodLogStreamOptions{Namespace: "default", Pod: "pod"}
	lines, checkpoints, wireBytes := 0, 0, 0
	var final PodLogStreamEvent
	err := service.StreamPodLogs(logStreamTestContext(), options, func(event PodLogStreamEvent) error {
		payload, _ := json.Marshal(event)
		wireBytes += len(payload) + len(event.Event) + len("event: \ndata: \n\n")
		final = event
		if event.Event == "limit" {
			t.Fatalf("premature bandwidth limit at %d lines", lines)
		}
		if event.Event == "line" {
			lines++
			if event.Ordinal != 1 || event.Sequence != uint64(lines) {
				t.Fatalf("incorrect line identity: %+v", event)
			}
			if (event.Cursor != "") != (lines%logCursorCheckpoint == 0) {
				t.Fatalf("incorrect checkpoint cadence at line %d", lines)
			}
			if event.Cursor != "" {
				checkpoints++
				options.Cursor = event.Cursor
				if _, err := service.logCursor(options); err != nil {
					t.Fatalf("invalid checkpoint: %v", err)
				}
			}
		}
		return nil
	})
	if err != nil || lines != lineCount || checkpoints != lineCount/logCursorCheckpoint || wireBytes > MaxLogStreamBytes || final.Event != "end" || final.Reason != "complete" || final.Cursor == "" {
		t.Fatalf("lines=%d checkpoints=%d bytes=%d final=%+v err=%v", lines, checkpoints, wireBytes, final, err)
	}
	t.Logf("%d lines, %d line checkpoints, %d SSE bytes", lines, checkpoints, wireBytes)
}

func TestPodLogStreamCheckpointWireQuota(t *testing.T) {
	const budget = 12000
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte(strings.Repeat("2026-10-01T00:00:00Z repeated\n", 1000)))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{})
	service.SetScrubber(replacingScrubber{})
	options := PodLogStreamOptions{Namespace: "default", Pod: "pod"}
	lines, checkpoints, wireBytes := 0, 0, 0
	var final, limit PodLogStreamEvent
	err := service.streamPodLogs(context.Background(), options, func(event PodLogStreamEvent) error {
		payload, _ := json.Marshal(event)
		wireBytes += len(payload) + len(event.Event) + len("event: \ndata: \n\n")
		if event.Event == "line" {
			lines++
			if event.Cursor != "" {
				checkpoints++
			}
		}
		if event.Event == "limit" {
			limit = event
		}
		final = event
		return nil
	}, time.Second, time.Second, time.Second, budget)
	options.Cursor = final.Cursor
	cursor, cursorErr := service.logCursor(options)
	if err != nil || cursorErr != nil || lines < 32 || lines >= 1000 || checkpoints == 0 || wireBytes > budget || final.Event != "end" || limit.Reason != "bytes" || limit.Cursor != final.Cursor || cursor.Ordinal != uint64(lines) {
		t.Fatalf("lines=%d checkpoints=%d bytes=%d cursor=%+v limit=%+v final=%+v err=%v cursorErr=%v", lines, checkpoints, wireBytes, cursor, limit, final, err, cursorErr)
	}
}

func TestPodLogStreamInterruptedCheckpointOccurrences(t *testing.T) {
	const lineCount = 96
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		since, _ := time.Parse(time.RFC3339Nano, request.URL.Query().Get("sinceTime"))
		for index := 0; index < lineCount; index++ {
			timestamp := base.Add(time.Duration(index%2) * time.Second)
			if !timestamp.Before(since) {
				writer.Write([]byte(timestamp.Format(time.RFC3339Nano) + " repeated\n"))
			}
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{})
	service.SetScrubber(replacingScrubber{})
	options := PodLogStreamOptions{Namespace: "default", Pod: "pod"}
	seen := make(map[string]bool)
	interrupted := errors.New("client interrupted between checkpoints")
	lines := 0
	err := service.StreamPodLogs(logStreamTestContext(), options, func(event PodLogStreamEvent) error {
		if event.Event == "line" {
			lines++
			seen[event.Timestamp+"/"+strconv.FormatUint(event.Ordinal, 10)] = true
			if event.Cursor != "" {
				options.Cursor = event.Cursor
			}
			if lines == 39 {
				return interrupted
			}
		}
		return nil
	})
	if !errors.Is(err, interrupted) || options.Cursor == "" || len(seen) != 39 {
		t.Fatalf("interruption lines=%d identities=%d err=%v", lines, len(seen), err)
	}
	resumed, overlap := 0, 0
	err = service.StreamPodLogs(logStreamTestContext(), options, func(event PodLogStreamEvent) error {
		if event.ReplayUncertain {
			t.Fatalf("continuous replay marked uncertain: %+v", event)
		}
		if event.Event == "line" {
			resumed++
			if event.Ordinal != uint64(16+(resumed+1)/2) || event.Text != "repeated" || event.Sequence != uint64(resumed) {
				t.Fatalf("reconstructed occurrence: %+v", event)
			}
			identity := event.Timestamp + "/" + strconv.FormatUint(event.Ordinal, 10)
			if seen[identity] {
				overlap++
			}
			seen[identity] = true
		}
		return nil
	})
	if err != nil || resumed != 64 || overlap != 7 || len(seen) != lineCount {
		t.Fatalf("resume lines=%d overlap=%d distinct=%d err=%v", resumed, overlap, len(seen), err)
	}
}

func TestPodLogStreamQuietCheckpointAndDiscontinuousReplay(t *testing.T) {
	for _, discontinuous := range []bool{false, true} {
		t.Run(strconv.FormatBool(discontinuous), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Write([]byte("2026-10-01T00:00:00Z repeated\n"))
				writer.(http.Flusher).Flush()
				if !discontinuous {
					<-request.Context().Done()
				}
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{})
			service.SetScrubber(replacingScrubber{})
			options := PodLogStreamOptions{Namespace: "default", Pod: "pod"}
			if discontinuous {
				encoded, _ := json.Marshal(logResumeCursor{Target: service.logTarget(options), Timestamp: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Ordinal: 2})
				options.Cursor = base64.RawURLEncoding.EncodeToString(encoded)
			}
			var events []PodLogStreamEvent
			err := service.streamPodLogs(context.Background(), options, func(event PodLogStreamEvent) error {
				events = append(events, event)
				return nil
			}, 60*time.Millisecond, time.Second, 5*time.Millisecond, MaxLogStreamBytes)
			if err != nil || len(events) == 0 {
				t.Fatalf("events=%+v err=%v", events, err)
			}
			final := events[len(events)-1]
			if final.Event != "end" || final.Cursor == "" || final.ReplayUncertain != discontinuous {
				t.Fatalf("continuity verdict: %+v", events)
			}
			if !discontinuous {
				checkpoints := 0
				for _, event := range events {
					if event.Event == "heartbeat" && event.Cursor != "" {
						checkpoints++
						if event.Cursor != final.Cursor {
							t.Fatal("quiet checkpoint differs from final cursor")
						}
					}
				}
				if checkpoints != 1 || events[0].Event != "line" || events[0].Cursor != "" {
					t.Fatalf("quiet checkpoint count=%d events=%+v", checkpoints, events)
				}
			}
		})
	}
}

func TestPodLogStreamCursorHistoryIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		for index := 0; index < 140; index++ {
			writer.Write([]byte(base.Add(time.Duration(index)*time.Second).Format(time.RFC3339Nano) + " repeated\n"))
		}
		writer.Write([]byte(base.Format(time.RFC3339Nano) + " repeated\n" + base.Format(time.RFC3339Nano) + " repeated\n"))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{})
	service.SetScrubber(replacingScrubber{})
	options := PodLogStreamOptions{Namespace: "default", Pod: "pod"}
	var lines int
	var uncertain bool
	seen := make(map[string]bool)
	err := service.StreamPodLogs(logStreamTestContext(), options, func(event PodLogStreamEvent) error {
		if event.Event != "line" {
			return nil
		}
		lines++
		uncertain = event.ReplayUncertain
		identity := event.Timestamp + "/" + strconv.FormatUint(event.Ordinal, 10)
		if seen[identity] {
			t.Fatalf("repeated line identity collapsed after eviction: %+v", event)
		}
		seen[identity] = true
		if lines > 140 && event.Ordinal != uint64(lines-139) {
			t.Fatalf("late repeated occurrence ordinal: %+v", event)
		}
		if event.Cursor == "" {
			return nil
		}
		options.Cursor = event.Cursor
		cursor, err := service.logCursor(options)
		if err != nil || len(cursor.Occurrences) > maxLogCursorTimestamps {
			t.Fatalf("invalid or unbounded emitted cursor: %v", err)
		}
		return nil
	})
	if err != nil || lines != 142 || !uncertain {
		t.Fatalf("bounded cursor dropped lines: count=%d uncertainty=%v err=%v", lines, uncertain, err)
	}
}

func TestPodLogStreamTimestampTextEncodedBudgetAndNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("container") == "encoded" {
			writer.Write([]byte("2026-10-01T00:00:00Z " + strings.Repeat("\x00", 100) + "\n"))
			return
		}
		writer.Write([]byte("2026-10-01T00:00:00Z secret\n"))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{})
	service.SetScrubber(replacingScrubber{})
	for _, timestamps := range []bool{false, true} {
		var events []PodLogStreamEvent
		err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "default", Pod: "pod", Timestamps: timestamps}, func(event PodLogStreamEvent) error { events = append(events, event); return nil })
		if err != nil || len(events) != 2 || events[0].Timestamp != "2026-10-01T00:00:00Z" {
			t.Fatalf("timestamp metadata: %v %+v", err, events)
		}
		want := "[redacted]"
		if timestamps {
			want = "2026-10-01T00:00:00Z " + want
		}
		if events[0].Text != want {
			t.Fatal("timestamp text contract changed")
		}
	}
	var events []PodLogStreamEvent
	err := service.streamPodLogs(context.Background(), PodLogStreamOptions{Namespace: "default", Pod: "pod", Container: "encoded"}, func(event PodLogStreamEvent) error { events = append(events, event); return nil }, time.Second, time.Second, time.Second, 300)
	if err != nil || len(events) != 2 || events[0].Event != "limit" || events[0].Reason != "bytes" {
		t.Fatalf("encoded byte budget: %+v %v", events, err)
	}
	for _, name := range []string{"a%2fb", "a\x00b", "token:secret", ".pod", "pod..name", "Pod"} {
		if err := service.ValidatePodLogStream(PodLogStreamOptions{Namespace: "default", Pod: name}); !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("unsafe Pod name accepted: %q", name)
		}
	}
}

type logStreamTestTransport func(*http.Request) (*http.Response, error)

func TestPodLogStreamBadRequestStatusDiagnosis(t *testing.T) {
	for _, test := range []struct {
		name, body, code string
		previous         bool
	}{
		{"missing-previous", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"previous terminated container \"app\" in pod \"pod\" not found"}`, "previous_unavailable", true},
		{"invalid-container", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"container private-provider-detail is not valid for pod"}`, "invalid_arguments", true},
		{"container-required", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"a container name must be specified"}`, "invalid_arguments", true},
		{"raw-body", "private-provider-detail previous terminated container not found", "invalid_arguments", true},
		{"untrusted-reason", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"private-provider-detail","code":400,"message":"previous terminated container not found"}`, "invalid_arguments", true},
		{"wrong-code", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":403,"message":"previous terminated container not found"}`, "invalid_arguments", true},
		{"oversized", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"previous terminated container not found` + strings.Repeat("x", 4096) + `"}`, "invalid_arguments", true},
		{"current", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"previous terminated container not found"}`, "invalid_arguments", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newTestService(t, "http://localhost", Scope{})
			service.SetScrubber(replacingScrubber{})
			service.client.streamHTTP.Transport = logStreamTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})
			events := 0
			err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "default", Pod: "pod", Container: "app", Previous: test.previous}, func(PodLogStreamEvent) error { events++; return nil })
			detail := DiagnoseError(err)
			if err == nil || detail.Code != test.code || detail.Retryable || events != 0 {
				t.Fatalf("diagnosis=%+v events=%d", detail, events)
			}
			payload, _ := json.Marshal(detail)
			if strings.Contains(err.Error()+string(payload), "private-provider-detail") {
				t.Fatal("Status content escaped the diagnostic boundary")
			}
		})
	}
}

func (transport logStreamTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestPodLogStreamAcceptsNegotiatedLogResponse(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(strconv.FormatBool(previous), func(t *testing.T) {
			service := newTestService(t, "http://localhost", Scope{})
			service.SetScrubber(replacingScrubber{})
			service.client.streamHTTP.Transport = logStreamTestTransport(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("Accept") != "*/*" {
					return &http.Response{StatusCode: http.StatusNotAcceptable, Body: io.NopCloser(strings.NewReader("private-provider-detail"))}, nil
				}
				if request.URL.Query().Get("follow") != strconv.FormatBool(!previous) || request.URL.Query().Get("timestamps") != "true" {
					t.Error("log stream options changed")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("2026-10-08T00:00:00Z token=secret\n"))}, nil
			})
			var events []PodLogStreamEvent
			err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "default", Pod: "pod", Container: "app", Previous: previous}, func(event PodLogStreamEvent) error {
				events = append(events, event)
				return nil
			})
			if err != nil {
				t.Fatalf("negotiated log stream failed: %v", err)
			}
			if len(events) != 2 || events[0].Event != "line" || strings.Contains(events[0].Text, "secret") || events[1].Event != "end" {
				t.Fatal("expected one redacted line and completion")
			}
		})
	}
}

func TestPodLogStreamConnectionErrorsPreserveSafeDiagnosis(t *testing.T) {
	for _, test := range []struct {
		name      string
		cause     error
		code      string
		retryable bool
	}{
		{name: "TLS", cause: x509.UnknownAuthorityError{}, code: "tls_verification_failed"},
		{name: "DNS", cause: &net.DNSError{Err: "private-provider-detail", Name: "private-endpoint.invalid"}, code: "dns_resolution_failed", retryable: true},
		{name: "dial", cause: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("private-provider-detail")}, code: "connection_failed", retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newTestService(t, "http://localhost", Scope{})
			service.client.streamHTTP = &http.Client{Transport: logStreamTestTransport(func(request *http.Request) (*http.Response, error) {
				return nil, &url.Error{Op: "Get", URL: "https://private-endpoint.invalid/?token=private-provider-detail", Err: test.cause}
			})}
			_, err := service.client.openPodLogStream(context.Background(), "/api/v1/namespaces/default/pods/pod/log", false)
			if !errors.Is(err, test.cause) {
				t.Fatal("connection cause lost")
			}
			detail := DiagnoseError(err)
			if detail.Code != test.code || detail.Retryable != test.retryable {
				t.Fatalf("diagnosis = %+v", detail)
			}
			payload, _ := json.Marshal(detail)
			for _, secret := range []string{"private-endpoint", "private-provider-detail", "token="} {
				if strings.Contains(err.Error(), secret) || strings.Contains(string(payload), secret) {
					t.Fatal("connection error exposed private data")
				}
			}
		})
	}
}

type logStreamFailureReader struct {
	ready      chan struct{}
	onClassify func()
}

func (reader *logStreamFailureReader) Read([]byte) (int, error) {
	close(reader.ready)
	return 0, reader
}

func (reader *logStreamFailureReader) Error() string {
	return "upstream read failure"
}

func (reader *logStreamFailureReader) Is(target error) bool {
	if target == io.EOF && reader.onClassify != nil {
		reader.onClassify()
	}
	return false
}

func TestPodLogStreamReadErrorContextTermination(t *testing.T) {
	for _, ending := range []string{"duration", "caller-cancel", "caller-deadline", "upstream"} {
		for _, boundary := range []string{"emit", "classification"} {
			t.Run(ending+"/"+boundary, func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				duration := time.Hour
				switch ending {
				case "duration":
					duration = time.Second
				case "caller-deadline":
					var cancelDeadline context.CancelFunc
					parent, cancelDeadline = context.WithTimeout(parent, time.Second)
					defer cancelDeadline()
				}
				failure := &logStreamFailureReader{ready: make(chan struct{})}
				service := newTestService(t, "http://localhost", Scope{})
				service.SetScrubber(replacingScrubber{})
				var streamContext context.Context
				service.client.streamHTTP = &http.Client{Transport: logStreamTestTransport(func(request *http.Request) (*http.Response, error) {
					streamContext = request.Context()
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(io.MultiReader(strings.NewReader("2026-10-01T00:00:00Z secret\n"), failure))}, nil
				})}
				endContext := func() {
					if ending == "caller-cancel" {
						cancel()
					}
					if ending != "upstream" {
						select {
						case <-streamContext.Done():
						case <-time.After(3 * time.Second):
							t.Fatal("stream context did not terminate at the expected boundary")
						}
					}
				}
				classified := false
				if boundary == "classification" {
					failure.onClassify = func() {
						classified = true
						endContext()
					}
				}
				var events []PodLogStreamEvent
				err := service.streamPodLogs(parent, PodLogStreamOptions{Namespace: "default", Pod: "pod"}, func(event PodLogStreamEvent) error {
					events = append(events, event)
					if event.Event == "line" {
						select {
						case <-failure.ready:
						case <-time.After(3 * time.Second):
							t.Fatal("upstream failure reader did not reach its boundary")
						}
						if boundary == "emit" {
							endContext()
						}
					}
					return nil
				}, duration, time.Hour, time.Hour, MaxLogStreamBytes)
				if boundary == "classification" && !classified {
					t.Fatal("read-error classification branch was not exercised")
				}
				if len(events) == 0 || events[0].Event != "line" || events[0].ReplayUncertain {
					t.Fatalf("initial events=%+v err=%v", events, err)
				}
				switch ending {
				case "duration":
					if err != nil || len(events) != 3 || events[1].Event != "limit" || events[2].Event != "end" || events[1].Reason != "duration" || events[2].Reason != "duration" || events[1].Cursor == "" || events[1].Cursor != events[2].Cursor || events[1].ReplayUncertain || events[2].ReplayUncertain {
						t.Fatalf("duration events=%+v err=%v", events, err)
					}
				case "caller-cancel", "caller-deadline":
					if !errors.Is(err, parent.Err()) || len(events) != 1 {
						t.Fatalf("canceled events=%+v err=%v parent=%v", events, err, parent.Err())
					}
				case "upstream":
					if err == nil || err.Error() != "kubernetes: log stream read failed" || len(events) != 2 || events[1].Event != "heartbeat" || events[1].Cursor == "" || !events[1].ReplayUncertain {
						t.Fatalf("upstream events=%+v err=%v", events, err)
					}
				}
			})
		}
	}
}

func TestPodLogStreamCancellationAndAdmission(t *testing.T) {
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("2026-10-01T00:00:00Z secret\n"))
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
		close(closed)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "cancel"})
	service.SetScrubber(replacingScrubber{})
	ctx, cancel := context.WithCancel(logStreamTestContext())
	err := service.StreamPodLogs(ctx, PodLogStreamOptions{Namespace: "default", Pod: "pod"}, func(PodLogStreamEvent) error { cancel(); return context.Canceled })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream not canceled")
	}
	if err := service.StreamPodLogs(context.Background(), PodLogStreamOptions{}, func(PodLogStreamEvent) error { return nil }); !errors.Is(err, ErrForbidden) {
		t.Fatal("missing authorization accepted")
	}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
			release()
		}
	}()
	for org := 0; org < 5; org++ {
		for stream := 0; stream < 4; stream++ {
			release, err := acquireLogStream(strconv.Itoa(org))
			if err != nil {
				t.Fatal(err)
			}
			releases = append(releases, release)
		}
		if _, err := acquireLogStream(strconv.Itoa(org)); !errors.Is(err, ErrLogStreamBusy) {
			t.Fatal("org cap not enforced")
		}
	}
	if _, err := acquireLogStream("another"); !errors.Is(err, ErrLogStreamBusy) {
		t.Fatal("process cap not enforced")
	}
}

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

func TestWorkloadLogPodsPrioritizeHealthThenNewest(t *testing.T) {
	older := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	pods := []WorkloadPod{
		{Name: "running", Phase: "Running", CreatedAt: newer},
		{Name: "pending", Phase: "Pending", CreatedAt: newer},
		{Name: "failed-old", Phase: "Failed", CreatedAt: older},
		{Name: "failed-new", Phase: "Failed", CreatedAt: newer},
	}
	prioritizeWorkloadLogPods(pods)
	want := []string{"failed-new", "failed-old", "pending", "running"}
	for index, pod := range pods {
		if pod.Name != want[index] {
			t.Fatalf("pod order = %#v, want %v", pods, want)
		}
	}
}

func TestWorkloadLogGrepIsLiteralAndUsesScrubbedText(t *testing.T) {
	scrubbed := replacingScrubber{}.Scrub("2026-10-05T12:00:00Z token=secret and pattern.*")
	if got := filterWorkloadLogLines(scrubbed, "secret"); len(got) != 0 {
		t.Fatalf("secret grep returned scrubbed data: %v", got)
	}
	if got := filterWorkloadLogLines(scrubbed, "pattern.*"); len(got) != 1 {
		t.Fatalf("literal grep results = %v", got)
	}
	if got := filterWorkloadLogLines(scrubbed, "pattern.x"); len(got) != 0 {
		t.Fatalf("grep treated punctuation as a pattern: %v", got)
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
