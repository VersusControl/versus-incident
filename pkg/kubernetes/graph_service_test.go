package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func TestGraphServiceUsesSharedIndexAndBoundsNeighborhood(t *testing.T) {
	scope := Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{
				map[string]any{"name": "apps", "preferredVersion": map[string]string{"version": "v1"}},
				map[string]any{"name": "batch", "preferredVersion": map[string]string{"version": "v1"}},
				map[string]any{"name": "networking.k8s.io", "preferredVersion": map[string]string{"version": "v1"}},
				map[string]any{"name": "autoscaling", "preferredVersion": map[string]string{"version": "v2"}},
			}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("nodes", "namespaces", "pods", "services", "ingresses", "configmaps", "secrets", "persistentvolumeclaims", "serviceaccounts")})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("deployments", "replicasets", "statefulsets", "daemonsets")})
		case "/apis/batch/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("jobs", "cronjobs")})
		case "/apis/networking.k8s.io/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("ingresses")})
		case "/apis/autoscaling/v2":
			writeJSON(writer, map[string]any{"resources": graphTestResources("horizontalpodautoscalers")})
		case "/api/v1/services":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"kind": "Service", "metadata": map[string]any{"name": "checkout", "namespace": "shop"}, "spec": map[string]any{"selector": map[string]string{"app": "checkout"}}}}})
		case "/api/v1/pods":
			pod := podFixture("checkout-1")
			pod["metadata"].(map[string]any)["namespace"] = "shop"
			pod["metadata"].(map[string]any)["labels"] = map[string]string{"app": "checkout"}
			writeJSON(writer, map[string]any{"items": []any{pod}})
		default:
			if request.Method == http.MethodGet {
				writeJSON(writer, map[string]any{"items": []any{}})
				return
			}
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, scope)
	index, err := service.indexes.ForScope(kubeindex.Scope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, CredentialID: scope.CredentialID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, kind := range graphIndexKinds {
		var records []kubeindex.Record
		switch kind {
		case "Service":
			records = []kubeindex.Record{{UID: "service", Kind: "Service", Namespace: "shop", Name: "checkout", Selector: map[string]string{"app": "checkout"}}}
		case "Pod":
			records = []kubeindex.Record{{UID: "pod", Kind: "Pod", Namespace: "shop", Name: "checkout-1", Labels: map[string]string{"app": "checkout"}}}
		}
		if err := index.Ingest(kind, records, true, now); err != nil {
			t.Fatal(err)
		}
	}
	graph, err := service.Graph(context.Background(), GraphQuery{MaxNodes: 10})
	if err != nil || len(graph.Nodes) != 2 || len(graph.Edges) != 1 || graph.Edges[0].Type != "exposes" || graph.Sync.State != "ready" {
		t.Fatalf("graph=%#v err=%v", graph, err)
	}
	neighborhood, err := service.Neighborhood(context.Background(), NeighborhoodQuery{Kind: "Service", Namespace: "shop", Name: "checkout", Hops: 9, MaxNodes: 1})
	if err != nil || len(neighborhood.Nodes) != 1 || neighborhood.Sync.State != "ready" {
		t.Fatalf("neighborhood=%#v err=%v", neighborhood, err)
	}
	if _, err := service.Neighborhood(context.Background(), NeighborhoodQuery{Kind: "Service", Name: "bad/name"}); err != ErrInvalidArguments {
		t.Fatalf("invalid neighborhood error=%v", err)
	}
	if err := index.Ingest("Service", []kubeindex.Record{
		{UID: "service-a", Kind: "Service", Namespace: "shop", Name: "service-a", Selector: map[string]string{"app": "a"}},
		{UID: "service-b", Kind: "Service", Namespace: "shop", Name: "service-b", Selector: map[string]string{"app": "b"}},
	}, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := index.Ingest("Pod", []kubeindex.Record{
		{UID: "pod-a", Kind: "Pod", Namespace: "shop", Name: "pod-a", Labels: map[string]string{"app": "a"}},
		{UID: "pod-b", Kind: "Pod", Namespace: "shop", Name: "pod-b", Labels: map[string]string{"app": "b"}},
		{UID: "pod-isolated", Kind: "Pod", Namespace: "shop", Name: "pod-isolated"},
		{UID: "pod-other-namespace", Kind: "Pod", Namespace: "other", Name: "pod-other"},
	}, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	page, err := service.Graph(context.Background(), GraphQuery{Namespace: "shop", ConnectedOnly: true, Limit: 1})
	if err != nil || len(page.Nodes) != 2 || len(page.Edges) != 1 || page.Next != "1" || !page.Truncated || page.Omitted["Pod"] != 1 {
		t.Fatalf("first connected graph page=%#v err=%v", page, err)
	}
	page, err = service.Graph(context.Background(), GraphQuery{Namespace: "shop", ConnectedOnly: true, Limit: 1, Cursor: page.Next})
	if err != nil || len(page.Nodes) != 2 || len(page.Edges) != 1 || page.Next != "" {
		t.Fatalf("second connected graph page=%#v err=%v", page, err)
	}
}

func TestCompleteGraphReturnsAllConnectedNamespaceRecordsWithoutPaging(t *testing.T) {
	server, requestedPaths := completeGraphTestServer(t, 251, 0, "")
	defer server.Close()
	scope := Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}
	service := newTestService(t, server.URL, scope)
	graph, err := service.Graph(context.Background(), GraphQuery{Namespace: "shop", ConnectedOnly: true, Complete: true})
	if err != nil || len(graph.Nodes) != 502 || len(graph.Edges) != 251 || graph.Next != "" || graph.Truncated || len(graph.Omitted) != 0 || graph.Sync.Partial || graph.Sync.State != "ready" {
		t.Fatalf("complete graph nodes=%d edges=%d next=%q truncated=%v omitted=%v sync=%+v err=%v", len(graph.Nodes), len(graph.Edges), graph.Next, graph.Truncated, graph.Omitted, graph.Sync, err)
	}
	for _, path := range requestedPaths() {
		if !strings.Contains(path, "/namespaces/shop/") {
			t.Fatalf("complete graph made an unscoped or unrelated namespace request: %s", path)
		}
	}
}

func TestOverviewGraphReturnsAllConnectedNamespacesWithoutPaging(t *testing.T) {
	server, requestedPaths := completeGraphTestServerMode(t, 251, 0, "", false, true)
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"})
	graph, err := service.OverviewGraph(context.Background())
	if err != nil || len(graph.Nodes) != 502 || len(graph.Edges) != 251 || graph.Next != "" || graph.Truncated || len(graph.Omitted) != 0 || graph.Sync.Partial || graph.Sync.State != "ready" {
		t.Fatalf("overview nodes=%d edges=%d next=%q truncated=%v omitted=%v sync=%+v err=%v", len(graph.Nodes), len(graph.Edges), graph.Next, graph.Truncated, graph.Omitted, graph.Sync, err)
	}
	counts := make(map[string]int)
	for _, node := range graph.Nodes {
		counts[node.Namespace]++
		if node.Group != node.Namespace {
			t.Fatalf("node not grouped by namespace: %+v", node)
		}
	}
	if counts["shop"] != 252 || counts["other"] != 250 {
		t.Fatalf("namespace counts=%v", counts)
	}
	paths := requestedPaths()
	if len(paths) != len(graphIndexKinds)-2+1 {
		t.Fatalf("collection requests=%v; want one per kind plus one pod continuation", paths)
	}
	for _, path := range paths {
		if strings.Contains(path, "/namespaces/") || strings.HasSuffix(path, "/nodes") || strings.HasSuffix(path, "/namespaces") {
			t.Fatalf("overview made unrelated or namespace-scoped request: %s", path)
		}
	}
	encoded, err := json.Marshal(graph)
	if err != nil || strings.Contains(string(encoded), "private-payload") {
		t.Fatalf("overview leaked resource payload: err=%v", err)
	}
}

func TestOverviewGraphFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		pairs     int
		pods      int
		forbidden string
		overlap   bool
		want      error
	}{
		{"denied kind", 1, 0, "/api/v1/pods", false, ErrForbidden},
		{"denied metadata kind", 1, 0, "/api/v1/secrets", false, ErrForbidden},
		{"record cap", 0, maxCompleteGraphRecords + 1, "", false, ErrCompleteGraphLimit},
		{"edge cap", 300, 0, "", true, ErrCompleteGraphLimit},
		{"relationship work cap", 1001, 0, "", false, ErrCompleteGraphLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := completeGraphTestServerMode(t, test.pairs, test.pods, test.forbidden, test.overlap, true)
			defer server.Close()
			service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
			graph, err := service.OverviewGraph(context.Background())
			if err != test.want || len(graph.Nodes) != 0 || graph.Next != "" || graph.Truncated {
				t.Fatalf("overview returned partial graph=%+v err=%v; want %v", graph, err, test.want)
			}
		})
	}
}

func TestCompleteTopologyValidatesEveryPageAndResponseSize(t *testing.T) {
	for _, test := range []struct {
		name      string
		namespace string
		secondNS  string
		repeat    bool
		pages     int
		large     bool
		want      error
	}{
		{name: "overview rejects missing namespace", secondNS: "", pages: 2, want: ErrGraphIncomplete},
		{name: "overview rejects unsafe namespace", secondNS: "bad/name", pages: 2, want: ErrGraphIncomplete},
		{name: "namespace rejects second page escape", namespace: "shop", secondNS: "other", pages: 2, want: ErrGraphIncomplete},
		{name: "overview rejects repeated continuation", secondNS: "other", pages: 3, repeat: true, want: ErrGraphIncomplete},
		{name: "overview rejects page cap", secondNS: "other", pages: kubernetesIndexMaxPages + 1, want: ErrCompleteGraphLimit},
		{name: "overview rejects encoded byte cap", secondNS: "other", pages: 30, large: true, want: ErrCompleteGraphLimit},
		{name: "overview preserves UID owners across namespaces", secondNS: "other", pages: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api":
					writeJSON(writer, map[string]any{"versions": []string{"v1"}})
				case "/apis":
					writeJSON(writer, map[string]any{"groups": []any{}})
				case "/api/v1":
					writeJSON(writer, map[string]any{"resources": graphTestResources("pods")})
				case "/api/v1/pods", "/api/v1/namespaces/shop/pods":
					page := 0
					if token := request.URL.Query().Get("continue"); token != "" {
						if _, err := fmt.Sscanf(token, "page-%d", &page); err != nil {
							t.Errorf("invalid continuation %q: %v", token, err)
						}
					}
					namespace := "shop"
					if page > 0 {
						namespace = test.secondNS
					}
					prefix := "pod-"
					pageSize := 1
					if test.large {
						prefix = strings.Repeat("x", 1024)
						pageSize = 100
					}
					items := make([]any, 0, pageSize)
					for item := 0; item < pageSize; item++ {
						number := page*pageSize + item
						uid := fmt.Sprintf("%s%d", prefix, number)
						ownerUID := fmt.Sprintf("%s%d", prefix, (number+1)%(test.pages*pageSize))
						items = append(items, map[string]any{"kind": "Pod", "metadata": map[string]any{
							"uid": uid, "name": uid, "namespace": namespace,
							"ownerReferences": []any{map[string]any{"kind": "Pod", "name": ownerUID, "uid": ownerUID}},
						}})
					}
					continuation := ""
					if page+1 < test.pages {
						continuation = fmt.Sprintf("page-%d", page+1)
						if test.repeat {
							continuation = "page-1"
						}
					}
					writeJSON(writer, map[string]any{"items": items, "metadata": map[string]any{"continue": continuation}})
				default:
					t.Errorf("unexpected collection path: %s", request.URL.Path)
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
			var graph kubegraph.Graph
			var err error
			if test.namespace != "" {
				graph, err = service.Graph(context.Background(), GraphQuery{Namespace: test.namespace, Complete: true})
			} else {
				graph, err = service.OverviewGraph(context.Background())
			}
			if err != test.want {
				t.Fatalf("graph nodes=%d edges=%d err=%v; want %v", len(graph.Nodes), len(graph.Edges), err, test.want)
			}
			if test.want != nil && (len(graph.Nodes) != 0 || graph.Next != "" || graph.Truncated) {
				t.Fatalf("failure returned partial graph: %+v", graph)
			}
			if test.want == nil && (len(graph.Nodes) != 2 || len(graph.Edges) != 2 || graph.Next != "" || graph.Truncated) {
				t.Fatalf("UID ownership graph=%+v", graph)
			}
		})
	}
}

func TestCompleteGraphUsesDiscoveredAlternateVersion(t *testing.T) {
	var deploymentPath string
	var pathMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "apps", "preferredVersion": map[string]string{"version": "v2"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("pods")})
		case "/apis/apps/v2":
			writeJSON(writer, map[string]any{"resources": graphTestResources("deployments")})
		case "/api/v1/namespaces/shop/pods":
			writeJSON(writer, map[string]any{"items": []any{}})
		case "/apis/apps/v2/namespaces/shop/deployments":
			pathMu.Lock()
			deploymentPath = request.URL.Path
			pathMu.Unlock()
			writeJSON(writer, map[string]any{"items": []any{}})
		default:
			if request.Method == http.MethodGet {
				writeJSON(writer, map[string]any{"items": []any{}})
				return
			}
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	if _, err := service.Graph(context.Background(), GraphQuery{Namespace: "shop", Complete: true}); err != nil {
		t.Fatal(err)
	}
	pathMu.Lock()
	defer pathMu.Unlock()
	if deploymentPath != "/apis/apps/v2/namespaces/shop/deployments" {
		t.Fatalf("deployment path = %q; complete graph should use the discovered matching version", deploymentPath)
	}
}

func TestCompleteGraphResponseSizeIsCountedWithoutBuildingPayload(t *testing.T) {
	small := kubegraph.Graph{
		Nodes: []kubegraph.Node{{ID: "<pod>", Kind: "Pod", Name: "bad\x01name", Group: "shop"}},
		Edges: []kubegraph.Edge{{From: "<pod>", To: "other", Type: kubegraph.Uses}}, Omitted: map[string]int{"Pod": 2},
		Next: "cursor", Truncated: true, Sync: kubegraph.SyncStatus{State: "ready", AgeSeconds: 1.5, Partial: true},
	}
	encoded, err := json.Marshal(small)
	if err != nil {
		t.Fatal(err)
	}
	size, err := completeGraphResponseSize(small)
	if err != nil || size != len(encoded) {
		t.Fatalf("counted graph size=%d encoded size=%d err=%v", size, len(encoded), err)
	}
	large := kubegraph.Graph{Nodes: make([]kubegraph.Node, maxCompleteGraphRecords), Edges: []kubegraph.Edge{}, Omitted: map[string]int{}, Sync: kubegraph.SyncStatus{State: "ready"}}
	for index := range large.Nodes {
		large.Nodes[index] = kubegraph.Node{ID: fmt.Sprintf("pod-%d", index), Kind: "Pod", Name: strings.Repeat("x", 1024), Group: "shop"}
	}
	largeSize, err := completeGraphResponseSize(large)
	if err != nil || largeSize <= maxCompleteGraphResponseBytes {
		t.Fatalf("large graph counted size=%d err=%v; want response cap exceeded", largeSize, err)
	}
}

func TestCompleteGraphRejectsIncompleteReadsAndSafetyLimits(t *testing.T) {
	t.Run("permission denied", func(t *testing.T) {
		server, _ := completeGraphTestServer(t, 1, 0, "/api/v1/namespaces/shop/pods")
		defer server.Close()
		service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
		if _, err := service.Graph(context.Background(), GraphQuery{Namespace: "shop", Complete: true}); err != ErrForbidden {
			t.Fatalf("complete graph permission error=%v", err)
		}
	})
	t.Run("record limit", func(t *testing.T) {
		server, _ := completeGraphTestServer(t, 0, maxCompleteGraphRecords+1, "")
		defer server.Close()
		service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
		if _, err := service.Graph(context.Background(), GraphQuery{Namespace: "shop", Complete: true}); err != ErrCompleteGraphLimit {
			t.Fatalf("complete graph limit error=%v", err)
		}
	})
	t.Run("edge limit", func(t *testing.T) {
		server, _ := completeGraphTestServerMode(t, 150, 150, "", true)
		defer server.Close()
		service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
		if _, err := service.Graph(context.Background(), GraphQuery{Namespace: "shop", Complete: true}); err != ErrCompleteGraphLimit {
			t.Fatalf("complete graph edge limit error=%v", err)
		}
	})
}

func TestCompleteGraphRejectsPagingControlsAndUnsafeNamespaces(t *testing.T) {
	service := NewService(nil, Scope{ClusterID: "cluster-a"}, 0)
	for _, query := range []GraphQuery{
		{Complete: true},
		{Namespace: "bad/name", Complete: true},
		{Namespace: "shop", Complete: true, Cursor: "1"},
		{Namespace: "shop", Complete: true, Limit: 1},
		{Namespace: "shop", Complete: true, MaxNodes: 1},
	} {
		if _, err := service.Graph(context.Background(), query); err != ErrInvalidArguments {
			t.Errorf("complete graph query %+v error=%v", query, err)
		}
	}
}

func completeGraphTestServer(t *testing.T, connectedPairs, podCount int, forbiddenPath string) (*httptest.Server, func() []string) {
	return completeGraphTestServerMode(t, connectedPairs, podCount, forbiddenPath, false)
}

func completeGraphTestServerMode(t *testing.T, connectedPairs, podCount int, forbiddenPath string, overlapSelectors bool, overview ...bool) (*httptest.Server, func() []string) {
	t.Helper()
	allNamespaces := len(overview) > 0 && overview[0]
	services := make([]any, 0, connectedPairs)
	pods := make([]any, 0, connectedPairs+podCount)
	for number := 0; number < connectedPairs; number++ {
		app := fmt.Sprintf("app-%d", number)
		namespace := "shop"
		if allNamespaces && number%2 != 0 {
			namespace = "other"
		}
		selector := app
		if overlapSelectors {
			selector = "shared"
		}
		services = append(services, map[string]any{
			"kind": "Service", "metadata": map[string]any{"name": "service-" + app, "namespace": namespace, "uid": "service-" + app},
			"spec": map[string]any{"selector": map[string]string{"app": selector}},
		})
		pods = append(pods, map[string]any{
			"kind": "Pod", "metadata": map[string]any{"name": "pod-" + app, "namespace": namespace, "uid": "pod-" + app, "labels": map[string]string{"app": selector}},
			"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "env": []any{map[string]any{"name": "TOKEN", "value": "private-payload"}}}}},
		})
	}
	for number := 0; number < podCount; number++ {
		pods = append(pods, map[string]any{
			"kind": "Pod", "metadata": map[string]any{"name": fmt.Sprintf("isolated-%d", number), "namespace": "shop", "uid": fmt.Sprintf("isolated-%d", number)},
		})
	}
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		if strings.Contains(path, "/namespaces/") || allNamespaces && strings.HasPrefix(path, "/api/v1/") || allNamespaces && strings.HasPrefix(path, "/apis/") && strings.Count(path, "/") == 4 {
			mu.Lock()
			paths = append(paths, path)
			mu.Unlock()
			if !allNamespaces && path != forbiddenPath && !strings.Contains(path, "/namespaces/shop/") {
				t.Errorf("namespace graph read escaped selected namespace: %s", path)
			}
		}
		if path == forbiddenPath {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		switch path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{
				map[string]any{"name": "apps", "preferredVersion": map[string]string{"version": "v1"}},
				map[string]any{"name": "batch", "preferredVersion": map[string]string{"version": "v1"}},
				map[string]any{"name": "networking.k8s.io", "preferredVersion": map[string]string{"version": "v1"}},
				map[string]any{"name": "autoscaling", "preferredVersion": map[string]string{"version": "v2"}},
			}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("nodes", "namespaces", "pods", "services", "ingresses", "configmaps", "secrets", "persistentvolumeclaims", "serviceaccounts")})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("deployments", "replicasets", "statefulsets", "daemonsets")})
		case "/apis/batch/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("jobs", "cronjobs")})
		case "/apis/networking.k8s.io/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("ingresses")})
		case "/apis/autoscaling/v2":
			writeJSON(writer, map[string]any{"resources": graphTestResources("horizontalpodautoscalers")})
		case "/api/v1/namespaces/shop/services":
			writeJSON(writer, map[string]any{"items": services})
		case "/api/v1/services":
			writeJSON(writer, map[string]any{"items": services})
		case "/api/v1/pods":
			if connectedPairs > 1 && request.URL.Query().Get("continue") == "" {
				writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "page-2"}, "items": pods[:1]})
			} else if connectedPairs > 1 {
				writeJSON(writer, map[string]any{"items": pods[1:]})
			} else {
				writeJSON(writer, map[string]any{"items": pods})
			}
		case "/api/v1/secrets", "/api/v1/configmaps":
			if !strings.Contains(request.Header.Get("Accept"), "as=PartialObjectMetadataList") {
				t.Errorf("overview requested full payload for %s", path)
			}
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "disconnected", "namespace": "shop", "uid": path}, "data": map[string]string{"value": "private-payload"}}}})
		case "/api/v1/namespaces/shop/pods":
			writeJSON(writer, map[string]any{"items": pods})
		case "/api/v1/namespaces/shop/configmaps":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "disconnected", "namespace": "shop", "uid": "configmap-disconnected"}}}})
		default:
			if request.Method == http.MethodGet {
				writeJSON(writer, map[string]any{"items": []any{}})
				return
			}
			http.NotFound(writer, request)
		}
	}))
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func graphTestResources(names ...string) []map[string]any {
	kinds := map[string]string{
		"nodes": "Node", "namespaces": "Namespace", "pods": "Pod", "services": "Service",
		"ingresses": "Ingress", "configmaps": "ConfigMap", "secrets": "Secret",
		"persistentvolumeclaims": "PersistentVolumeClaim", "serviceaccounts": "ServiceAccount",
		"deployments": "Deployment", "replicasets": "ReplicaSet", "statefulsets": "StatefulSet",
		"daemonsets": "DaemonSet", "jobs": "Job", "cronjobs": "CronJob",
		"horizontalpodautoscalers": "HorizontalPodAutoscaler",
	}
	resources := make([]map[string]any, 0, len(names))
	for _, name := range names {
		resources = append(resources, map[string]any{"name": name, "kind": kinds[name], "namespaced": name != "nodes" && name != "namespaces", "verbs": []string{"get", "list"}})
	}
	return resources
}

type trafficContributorFunc func(context.Context, TrafficQuery) (Traffic, error)

func (contributor trafficContributorFunc) ReadTraffic(ctx context.Context, query TrafficQuery) (Traffic, error) {
	return contributor(ctx, query)
}

func TestTrafficServiceRequiresContributorAndSharesItAcrossScopes(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "credential-a"}, 0)
	missing, err := service.Traffic(context.Background(), TrafficQuery{})
	if err != nil || missing.Available || len(missing.Edges) != 0 || missing.Reason == "" || missing.Window != "15m" {
		t.Fatalf("missing contributor result=%+v err=%v", missing, err)
	}
	called := false
	service.SetTrafficContributor(trafficContributorFunc(func(_ context.Context, query TrafficQuery) (Traffic, error) {
		called = true
		if query.Namespace != "shop" || query.Source != "tempo" || query.Window != "5m" {
			t.Fatalf("traffic query = %+v", query)
		}
		observedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
		errorRate := 0.2
		return Traffic{Available: true, Source: "tempo", Window: "1h", ObservedAt: observedAt, Edges: []TrafficEdge{{From: TrafficWorkload{Namespace: "shop", Kind: "Deployment", Name: "checkout"}, To: TrafficWorkload{Namespace: "shop", Kind: "Deployment", Name: "payments"}, ErrorRate: &errorRate}}}, nil
	}))
	result, err := service.Scoped(Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "credential-a"}).Traffic(context.Background(), TrafficQuery{Namespace: "shop", Source: "tempo", Window: "5m"})
	if err != nil || !called || !result.Available || result.Source != "tempo" || result.Window != "1h" || !result.ObservedAt.Equal(time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)) || len(result.Edges) != 1 {
		t.Fatalf("traffic result=%+v called=%v err=%v", result, called, err)
	}
	if result.Edges[0].RatePerSec != nil || result.Edges[0].ErrorRate == nil || *result.Edges[0].ErrorRate != 0.2 || result.Edges[0].P95Ms != nil || result.Edges[0].BytesPerSec != nil {
		t.Fatalf("traffic edge metrics = %+v, want absent values to remain unavailable", result.Edges[0])
	}
	if _, err := service.Traffic(context.Background(), TrafficQuery{Window: "24h"}); err != ErrInvalidArguments {
		t.Fatalf("invalid window error=%v", err)
	}
}

func TestTrafficSortsBeforeApplyingResponseCaps(t *testing.T) {
	service := NewService(nil, Scope{ClusterID: "cluster-a"}, 0)
	service.SetTrafficContributor(trafficContributorFunc(func(context.Context, TrafficQuery) (Traffic, error) {
		result := Traffic{Available: true}
		for value := 0; value < 101; value++ {
			rate := float64(value) / 100
			result.Edges = append(result.Edges, TrafficEdge{From: TrafficWorkload{Name: string(rune('a' + value%26))}, ErrorRate: &rate})
		}
		for value := 21; value > 0; value-- {
			result.External = append(result.External, string(rune('a'+value)))
		}
		return result, nil
	}))
	result, err := service.Traffic(context.Background(), TrafficQuery{})
	if err != nil || len(result.Edges) != 100 || len(result.External) != 20 || !result.Truncated {
		t.Fatalf("capped traffic result edges=%d external=%d truncated=%v err=%v", len(result.Edges), len(result.External), result.Truncated, err)
	}
	if result.Edges[0].ErrorRate == nil || *result.Edges[0].ErrorRate != 1 || result.Edges[99].ErrorRate == nil || *result.Edges[99].ErrorRate != 0.01 {
		t.Fatalf("edge cap did not retain the highest-ranked results: first=%+v last=%+v", result.Edges[0], result.Edges[99])
	}
	if result.External[0] != "b" || result.External[len(result.External)-1] != "u" {
		t.Fatalf("external cap is not deterministic: %v", result.External)
	}
}
