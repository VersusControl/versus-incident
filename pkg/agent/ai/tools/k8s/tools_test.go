package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestNewReturnsSpecCatalogReadOnlyToolsAndAuthorizationFailsClosed(t *testing.T) {
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: "http://127.0.0.1", AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	tools := New(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0))
	if len(tools) != len(toolNames) {
		t.Fatalf("tools = %d, want %d", len(tools), len(toolNames))
	}
	for index, tool := range tools {
		if tool.Name() != toolNames[index] || tool.Description() == "" || tool.ArgsSchema()["type"] != "object" {
			t.Errorf("tool %d = %s", index, tool.Name())
		}
		result, err := tool.Invoke(context.Background(), nil)
		if err != nil || result.IsAvailable() || result.Reason != "infrastructure:view permission is required" {
			t.Errorf("unauthorized %s = %#v, %v", tool.Name(), result, err)
		}
	}
}

func TestGetKubernetesChangesToolUsesSharedBoundedServicePage(t *testing.T) {
	provider := storage.NewMemory()
	scope := kubeindex.Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}
	now := time.Now().UTC()
	store := kubechanges.NewStore(provider, scope)
	if err := store.Append([]kubechanges.Change{{ID: "change-a", Cluster: "cluster-a", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid-a", Type: kubechanges.ImageChanged, At: now}}, now); err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(nil, kubernetes.Scope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, CredentialID: scope.CredentialID}, 0)
	service.SetChangeStorage(provider)
	var changeTool core.Tool
	for _, candidate := range New(service) {
		if candidate.Name() == "get_k8s_changes" {
			changeTool = candidate
		}
	}
	if changeTool == nil {
		t.Fatal("get_k8s_changes is not registered")
	}
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	result, err := changeTool.Invoke(ctx, json.RawMessage(`{"since_minutes":60,"limit":10}`))
	encoded, _ := json.Marshal(result.Data)
	if err != nil || result == nil || !result.Found || len(encoded) > 6<<10 {
		t.Fatalf("change tool result=%#v bytes=%d err=%v", result, len(encoded), err)
	}
	if _, err := changeTool.Invoke(ctx, json.RawMessage(`{"since_minutes":1441}`)); err == nil {
		t.Fatal("out-of-range change window was accepted")
	}
}

func TestListRolloutsToolUsesSharedBoundedServicePage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeToolJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeToolJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "argoproj.io", "versions": []any{map[string]any{"groupVersion": "argoproj.io/v1alpha1", "version": "v1alpha1"}}, "preferredVersion": map[string]any{"groupVersion": "argoproj.io/v1alpha1", "version": "v1alpha1"}}}})
		case "/api/v1":
			writeToolJSON(writer, map[string]any{"resources": []any{}})
		case "/apis/argoproj.io/v1alpha1":
			writeToolJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "rollouts", "kind": "Rollout", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/argoproj.io/v1alpha1/namespaces/shop/rollouts":
			writeToolJSON(writer, map[string]any{"items": []any{map[string]any{
				"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout",
				"metadata": map[string]any{"name": "checkout", "namespace": "shop"},
				"status":   map[string]any{"phase": "Progressing", "currentStepIndex": 1},
			}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "cluster-a"}, 0)
	var candidate core.Tool
	for _, tool := range New(service) {
		if tool.Name() == "list_rollouts" {
			candidate = tool
		}
	}
	if candidate == nil {
		t.Fatal("list_rollouts is not registered")
	}
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	result, err := candidate.Invoke(ctx, json.RawMessage(`{"namespace":"shop","limit":2}`))
	encoded, _ := json.Marshal(result.Data)
	if err != nil || result == nil || !result.Found || len(encoded) > 6<<10 || result.Data["available"] != true {
		t.Fatalf("rollout tool result=%#v bytes=%d err=%v", result, len(encoded), err)
	}
}

func TestAllToolsConsumeTheExactSharedScopedService(t *testing.T) {
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: "http://127.0.0.1", AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "credential-a"}, 0)
	tools := New(service)
	if len(tools) != len(toolNames) {
		t.Fatalf("tools = %d", len(tools))
	}
	for _, candidate := range tools {
		implementation, ok := candidate.(*tool)
		if !ok || implementation.service != service {
			t.Fatalf("tool %T does not consume the shared service", candidate)
		}
	}
}

func TestSpecMatchesKubernetesCatalogAndToolsetOrder(t *testing.T) {
	want := make([]string, len(specs))
	for index, spec := range specs {
		want[index] = spec.Name
	}
	var catalogNames []string
	for _, metadata := range aitools.Catalog() {
		if metadata.Group == aitools.GroupK8s && metadata.Name != "propose_action" {
			catalogNames = append(catalogNames, metadata.Name)
		}
	}
	if !equalStrings(catalogNames, want) {
		t.Fatalf("Kubernetes catalog = %v, want %v", catalogNames, want)
	}
	for _, toolset := range aitools.Toolsets() {
		if toolset.ID == "kubernetes" {
			if !equalStrings(toolset.ToolNames, want) {
				t.Fatalf("Kubernetes toolset = %v, want %v", toolset.ToolNames, want)
			}
			return
		}
	}
	t.Fatal("Kubernetes toolset is missing")
}

func TestKubernetesModelViewsStayWithinBudget(t *testing.T) {
	longName := strings.Repeat("x", 253)
	issues := make([]kubernetes.Issue, 50)
	items := make([]kubernetes.TopItem, 50)
	releases := make([]kubernetes.HelmRelease, 50)
	events := make([]kubernetes.ProjectedResource, 20)
	for index := range issues {
		issues[index] = kubernetes.Issue{Root: kubernetes.ObjectRef{Kind: "Deployment", Namespace: "checkout", Name: longName}, Rule: "workload.unavailable", Severity: "warning", Count: index + 1, Examples: []kubernetes.ObjectRef{{Kind: "Pod", Name: longName}}}
		items[index] = kubernetes.TopItem{ResourceUsage: kubernetes.ResourceUsage{Kind: "PodMetrics", Namespace: "checkout", Name: longName, CPU: "1/2", Memory: "1048576"}}
		releases[index] = kubernetes.HelmRelease{Namespace: "checkout", Name: longName, Current: kubernetes.HelmRevision{Revision: 20, Status: "failed"}, History: makeHelmHistory(), Health: "warning"}
		if index < len(events) {
			events[index] = kubernetes.ProjectedResource{Name: longName, Summary: map[string]any{"reason": "BackOff", "count": float64(index + 1)}}
		}
	}
	for name, data := range map[string]any{
		"issues": compactIssues(kubernetes.IssuePage{Items: issues, Totals: map[string]int{"warning": 50}, Truncated: true, Sync: kubernetes.SyncStatus{State: "direct", Partial: true}}, kubernetes.Scope{ClusterID: "cluster-a"}, ""),
		"graph":  compactGraph(kubegraph.Graph{Nodes: makeGraphNodes(50, longName), Edges: []kubegraph.Edge{{From: "node-0", To: "node-1", Type: kubegraph.Exposes}}, Omitted: map[string]int{"Pod": 20}, Sync: kubegraph.SyncStatus{State: "partial", Partial: true}}, kubernetes.Scope{ClusterID: "cluster-a"}),
		"top":    compactTop(kubernetes.TopPage{Items: items, Total: 50, Truncated: true, Availability: "available", Fresh: true}, kubernetes.Scope{ClusterID: "cluster-a"}),
		"gitops": compactGitOpsApps(kubernetes.GitOpsAppPage{Items: makeGitOpsApps(50, longName), Available: true}, kubernetes.Scope{ClusterID: "cluster-a"}, ""),
		"helm":   compactHelmReleases(releases, kubernetes.Scope{ClusterID: "cluster-a"}),
		"diagnosis": compactDiagnosis(kubernetes.Diagnosis{
			Workload:      kubernetes.WorkloadDetail{Kind: "Deployment", Namespace: "checkout", Name: longName, Pods: make([]kubernetes.WorkloadPod, 30)},
			WarningEvents: events, WorstPodLogs: &kubernetes.PodLogs{Pod: longName, Text: strings.Repeat("error: "+longName+"\n", 100)},
			Omitted: []string{"health_findings"}, Sync: kubernetes.SyncStatus{State: "direct", Partial: true},
		}, kubernetes.Scope{ClusterID: "cluster-a"}),
	} {
		encoded, err := json.Marshal(data)
		if err != nil || len(encoded) > 6<<10 {
			t.Errorf("%s model view has %d bytes, err=%v", name, len(encoded), err)
		}
	}
}

func TestEveryToolModelViewStaysWithinBudget(t *testing.T) {
	items := make([]any, 40)
	for index := range items {
		items[index] = map[string]any{
			"kind": "Pod", "namespace": "shop", "name": "checkout-" + strconv.Itoa(index),
			"summary": strings.Repeat("projected field ", 100),
		}
	}
	for _, name := range toolNames {
		view := compactModelView(map[string]any{
			"items": items, "events": items, "message": strings.Repeat("bounded detail ", 800), "continue": "page-2",
		}, arguments{Kind: "Deployment", Namespace: "shop", Name: "checkout", ResourceID: "apps~v1~deployments"}, kubernetes.Scope{ClusterID: "cluster-a"})
		encoded, err := json.Marshal(view)
		if err != nil || len(encoded) > 6<<10 {
			t.Errorf("%s model view bytes=%d err=%v", name, len(encoded), err)
		}
		if view["sync"] == nil || view["truncated"] != true || view["next"] == nil {
			t.Errorf("%s model view lacks sync/truncation metadata: %+v", name, view)
		}
	}
	view := compactModelView(map[string]any{"kind": "Deployment", "namespace": "shop", "name": "checkout"}, arguments{Kind: "Deployment", Namespace: "shop", Name: "checkout"}, kubernetes.Scope{ClusterID: "cluster-a"})
	if view["ref"] != "k8s://cluster-a/shop/Deployment/checkout" || view["links"] == nil {
		t.Fatalf("resource model view lacks citation links: %+v", view)
	}
}

func TestGetKubernetesNeighborhoodToolUsesSharedIndex(t *testing.T) {
	scope := kubernetes.Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeToolJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeToolJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeToolJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "services", "kind": "Service", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/api/v1/services":
			writeToolJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"uid": "service", "namespace": "shop", "name": "checkout"}, "spec": map[string]any{"selector": map[string]any{"app": "checkout"}}}}})
		case "/api/v1/pods":
			pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"uid": "pod", "namespace": "shop", "name": "checkout-1", "labels": map[string]any{"app": "checkout"}}, "status": map[string]any{"phase": "Running"}}
			writeToolJSON(writer, map[string]any{"items": []any{pod}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, scope, time.Minute)
	if _, _, err := service.IndexSnapshot(context.Background(), "Service", "Pod"); err != nil {
		t.Fatal(err)
	}
	var candidate core.Tool
	for _, tool := range New(service) {
		if tool.Name() == "get_k8s_neighborhood" {
			candidate = tool
		}
	}
	if candidate == nil {
		t.Fatal("get_k8s_neighborhood was not registered")
	}
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	result, err := candidate.Invoke(ctx, json.RawMessage(`{"mode":"namespace","namespace":"shop","max_nodes":50,"include_traffic":true}`))
	var nodes []any
	if result != nil {
		nodes, _ = result.Data["nodes"].([]any)
	}
	if err != nil || result == nil || !result.Found || len(nodes) != 2 {
		t.Fatalf("neighborhood result=%#v err=%v", result, err)
	}
	traffic, ok := result.Data["traffic"].(map[string]any)
	if !ok || traffic["available"] != false || traffic["reason"] == "" {
		t.Fatalf("missing-source traffic view=%#v", result.Data["traffic"])
	}
	if _, err := candidate.Invoke(ctx, json.RawMessage(`{"mode":"namespace"}`)); err == nil {
		t.Fatal("namespace mode accepted a missing namespace")
	}
}

func makeGraphNodes(count int, name string) []kubegraph.Node {
	nodes := make([]kubegraph.Node, count)
	for index := range nodes {
		nodes[index] = kubegraph.Node{ID: "node-" + strconv.Itoa(index), Kind: "Pod", Namespace: "shop", Name: name, Group: "shop"}
	}
	return nodes
}

func writeToolJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

func makeHelmHistory() []kubernetes.HelmRevision {
	history := make([]kubernetes.HelmRevision, 20)
	for index := range history {
		history[index] = kubernetes.HelmRevision{Revision: 20 - index, Status: "failed"}
	}
	return history
}

func makeGitOpsApps(count int, name string) []kubernetes.GitOpsApp {
	apps := make([]kubernetes.GitOpsApp, count)
	for index := range apps {
		apps[index] = kubernetes.GitOpsApp{Tool: "argocd", Kind: "Application", Namespace: "checkout", Name: name, Sync: "OutOfSync", Health: "Degraded", Message: name, Source: "https://example.test/" + name}
	}
	return apps
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestAuthorizedContextCarriesInfrastructureView(t *testing.T) {
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		t.Fatal("permission missing")
	}
}

func TestFilterAuthorizedOmitsKubernetesForBackgroundCaller(t *testing.T) {
	client, _ := kubernetes.NewClient(kubernetes.Config{Endpoint: "http://127.0.0.1", AllowLoopbackHTTP: true})
	tools := New(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0))
	if got := FilterAuthorized(context.Background(), tools); len(got) != 0 {
		t.Fatalf("background tools = %d", len(got))
	}
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	if got := FilterAuthorized(ctx, tools); len(got) != len(toolNames) {
		t.Fatalf("authorized tools = %d, want %d", len(got), len(toolNames))
	}
}

func TestDirectToolOutcomesDistinguishForbiddenUnavailableEmptyAndBackendError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			_ = json.NewEncoder(writer).Encode(map[string]any{"versions": []string{"v1"}})
		case "/apis":
			_ = json.NewEncoder(writer).Encode(map[string]any{"groups": []any{}})
		case "/api/v1":
			_ = json.NewEncoder(writer).Encode(map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/default/pods/denied":
			http.Error(writer, "hidden status", http.StatusForbidden)
		case "/api/v1/namespaces/default/pods/missing":
			http.NotFound(writer, request)
		case "/api/v1/namespaces/default/pods/broken":
			http.Error(writer, "backend", http.StatusInternalServerError)
		case "/api/v1/namespaces/default/pods":
			_ = json.NewEncoder(writer).Encode(map[string]any{"items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	tools := New(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0))
	authorized := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	resourceTool := tools[3]
	for _, test := range []struct {
		name   string
		status string
	}{
		{"denied", "forbidden"}, {"missing", "unavailable"},
	} {
		result, invokeErr := resourceTool.Invoke(authorized, json.RawMessage(`{"resource_id":"core~v1~pods","namespace":"default","name":"`+test.name+`"}`))
		if invokeErr != nil || result == nil || !result.IsAvailable() || result.Found || result.Data["status"] != test.status {
			t.Errorf("%s result=%#v err=%v", test.name, result, invokeErr)
		}
	}
	empty, err := tools[2].Invoke(authorized, json.RawMessage(`{"resource_id":"core~v1~pods","namespace":"default"}`))
	if err != nil || empty == nil || empty.Found || empty.Data["status"] != nil {
		t.Fatalf("empty result=%#v err=%v", empty, err)
	}
	if result, err := resourceTool.Invoke(authorized, json.RawMessage(`{"resource_id":"core~v1~pods","namespace":"default","name":"broken"}`)); err == nil || result != nil {
		t.Fatalf("backend result=%#v err=%v", result, err)
	}
}

func TestNamespacedReadSchemaAndRuntimeReturnInvalidArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			_ = json.NewEncoder(writer).Encode(map[string]any{"versions": []string{"v1"}})
		case "/apis":
			_ = json.NewEncoder(writer).Encode(map[string]any{"groups": []any{}})
		case "/api/v1":
			_ = json.NewEncoder(writer).Encode(map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		default:
			t.Fatalf("missing namespace reached cluster path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	tools := New(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0))
	authorized := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	for _, index := range []int{3, 5} {
		result, invokeErr := tools[index].Invoke(authorized, json.RawMessage(`{"resource_id":"core~v1~pods","kind":"Pod","name":"api"}`))
		var toolErr *core.ToolError
		if result != nil || !errors.As(invokeErr, &toolErr) || toolErr.Code != core.ToolErrorInvalidArguments || toolErr.Message != "invalid Kubernetes tool arguments" {
			t.Errorf("%s result=%#v error=%#v", tools[index].Name(), result, invokeErr)
		}
	}
	workloadRequired, _ := tools[5].ArgsSchema()["required"].([]string)
	if !contains(workloadRequired, "namespace") {
		t.Fatalf("get_workload required = %v", workloadRequired)
	}
	properties := tools[3].ArgsSchema()["properties"].(map[string]any)
	namespace := properties["namespace"].(map[string]any)
	if namespace["description"] != "Required when the discovered resource is namespaced." {
		t.Fatalf("namespace schema = %#v", namespace)
	}
}

func TestPodLogsSchemaAllowsDefaultContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("container") == "" {
			http.Error(writer, "a container name must be specified", http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte("sidecar log"))
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	logTool := New(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0))[7]
	required, _ := logTool.ArgsSchema()["required"].([]string)
	if contains(required, "container") || !contains(required, "namespace") || !contains(required, "name") {
		t.Fatalf("get_pod_logs required = %v", required)
	}
	properties := logTool.ArgsSchema()["properties"].(map[string]any)
	container := properties["container"].(map[string]any)
	if container["description"] != "Optional for single-container pods; required for multi-container pods." {
		t.Fatalf("container schema = %#v", container)
	}
	authorized := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	result, invokeErr := logTool.Invoke(authorized, json.RawMessage(`{"namespace":"default","name":"multi"}`))
	var toolErr *core.ToolError
	if result != nil || !errors.As(invokeErr, &toolErr) || toolErr.Code != core.ToolErrorInvalidArguments {
		t.Fatalf("multi-container result=%#v error=%#v", result, invokeErr)
	}
	result, invokeErr = logTool.Invoke(authorized, json.RawMessage(`{"namespace":"default","name":"multi","container":"sidecar"}`))
	if invokeErr != nil || result == nil || !result.Found {
		t.Fatalf("explicit container result=%#v error=%#v", result, invokeErr)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
