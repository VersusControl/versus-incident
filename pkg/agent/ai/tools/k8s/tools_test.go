package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
)

func TestNewReturnsNineReadOnlyToolsAndAuthorizationFailsClosed(t *testing.T) {
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: "http://127.0.0.1", AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	tools := New(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0))
	if len(tools) != 9 {
		t.Fatalf("tools = %d, want 9", len(tools))
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

func TestAllNineToolsConsumeTheExactSharedScopedService(t *testing.T) {
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
	if got := FilterAuthorized(ctx, tools); len(got) != 9 {
		t.Fatalf("authorized tools = %d", len(got))
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
	logTool := New(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0))[8]
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
