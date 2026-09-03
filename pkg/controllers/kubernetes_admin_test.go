package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	k8stools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/k8s"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/gofiber/fiber/v2"
)

func TestKubernetesAdminAuthorizationAndDiscoveryAdapter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"versions": []string{"v1"}})
			return
		}
		if request.URL.Path == "/api/v1" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
			return
		}
		if request.URL.Path == "/apis" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"groups": []any{}})
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0)
	build := func(authorized bool, permitted *bool) *fiber.App {
		app := fiber.New()
		if authorized {
			app.Use(func(ctx *fiber.Ctx) error {
				middleware.MarkAuthorized(ctx)
				if permitted != nil {
					middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), *permitted)
				}
				return ctx.Next()
			})
		}
		NewKubernetesAdminController(service).Register(app.Group("/api"))
		return app
	}
	allowed, denied := true, false
	for _, test := range []struct {
		authorized bool
		permitted  *bool
		want       int
	}{{true, &denied, 403}, {true, nil, 403}, {true, &allowed, 200}} {
		response, err := build(test.authorized, test.permitted).Test(httptest.NewRequest("GET", "/api/admin/kubernetes/resources/discovery", nil), -1)
		if err != nil || response.StatusCode != test.want {
			t.Errorf("authorized=%v permitted=%v status=%v err=%v", test.authorized, test.permitted, response.StatusCode, err)
		}
	}
}

func TestKubernetesAdminRegistersOnlyGetRoutes(t *testing.T) {
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: "http://127.0.0.1", AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0)
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error { middleware.MarkAuthorized(ctx); return ctx.Next() })
	NewKubernetesAdminController(service).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest("POST", "/api/admin/kubernetes/resources", nil), -1)
	if err != nil || response.StatusCode == fiber.StatusOK {
		t.Fatalf("POST status=%v err=%v", response.StatusCode, err)
	}
}

func TestKubernetesHTTPResolvesInjectedOrgThroughSharedServiceRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			_ = json.NewEncoder(writer).Encode(map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			_ = json.NewEncoder(writer).Encode(map[string]any{"resources": []any{}})
		case "/apis":
			_ = json.NewEncoder(writer).Encode(map[string]any{"groups": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	base := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: "cluster-a", CredentialID: "credential-a"}, 0)
	registry := kubernetes.NewServiceRegistry(base)
	controller := NewKubernetesAdminControllerWithRegistry(registry)
	middleware.SetOrgResolver(func(ctx *fiber.Ctx) string { return ctx.Get("X-Test-Org") })
	t.Cleanup(func() { middleware.SetOrgResolver(nil) })
	app := fiber.New()
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	controller.Register(app.Group("/api"))
	request := httptest.NewRequest("GET", "/api/admin/kubernetes/resources/discovery", nil)
	request.Header.Set("X-Test-Org", "org-a")
	response, err := app.Test(request, -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("status=%d err=%v", response.StatusCode, err)
	}
	resolved := controller.resolve("org-a")
	if resolved != registry.ResolveOrg("org-a") || resolved.Scope() != (kubernetes.Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "credential-a"}) {
		t.Fatalf("resolved scope = %#v", resolved.Scope())
	}
	if controller.resolve("org-b") == resolved {
		t.Fatal("distinct organizations shared one service instance")
	}
}

func TestKubernetesAIAndHTTPAdaptersReturnEquivalentResourceFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			_ = json.NewEncoder(writer).Encode(map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			_ = json.NewEncoder(writer).Encode(map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis":
			_ = json.NewEncoder(writer).Encode(map[string]any{"groups": []any{}})
		case "/api/v1/namespaces/payments/pods":
			_ = json.NewEncoder(writer).Encode(map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"namespace": "payments", "name": "api"}, "spec": map[string]any{"nodeName": "node-a"}, "status": map[string]any{"phase": "Running"}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0)
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	NewKubernetesAdminController(service).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/resources?resource_id=core~v1~pods&namespace=payments", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("HTTP status=%v err=%v", response.StatusCode, err)
	}
	var httpPage kubernetes.ResourcePage
	if err := json.NewDecoder(response.Body).Decode(&httpPage); err != nil {
		t.Fatal(err)
	}
	authorized := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	toolResult, err := k8stools.New(service)[2].Invoke(authorized, json.RawMessage(`{"resource_id":"core~v1~pods","namespace":"payments"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(toolResult.Data)
	var aiPage kubernetes.ResourcePage
	if err := json.Unmarshal(encoded, &aiPage); err != nil {
		t.Fatal(err)
	}
	if len(httpPage.Items) != 1 || len(aiPage.Items) != 1 {
		t.Fatalf("HTTP=%#v AI=%#v", httpPage, aiPage)
	}
	httpItem, aiItem := httpPage.Items[0], aiPage.Items[0]
	if httpItem.ResourceID != aiItem.ResourceID || httpItem.Kind != aiItem.Kind || httpItem.Namespace != aiItem.Namespace || httpItem.Name != aiItem.Name || httpItem.Summary["phase"] != aiItem.Summary["phase"] || httpItem.Summary["node"] != aiItem.Summary["node"] {
		t.Fatalf("semantic facts differ: HTTP=%#v AI=%#v", httpItem, aiItem)
	}
}

func TestKubernetesNamespacedReadsReturnBadRequestWithoutNamespace(t *testing.T) {
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
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	NewKubernetesAdminController(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0)).Register(app.Group("/api"))
	for _, path := range []string{
		"/api/admin/kubernetes/resources/core~v1~pods/api",
		"/api/admin/kubernetes/resources/core~v1~pods/api/describe",
		"/api/admin/kubernetes/workloads/Pod/api",
	} {
		response, requestErr := app.Test(httptest.NewRequest(http.MethodGet, path, nil), -1)
		if requestErr != nil || response.StatusCode != fiber.StatusBadRequest {
			t.Errorf("%s status=%d err=%v", path, response.StatusCode, requestErr)
		}
	}
}

func TestKubernetesPodLogsReturnsBadRequestForMultiContainerPodWithoutContainer(t *testing.T) {
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
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	NewKubernetesAdminController(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "test"}, 0)).Register(app.Group("/api"))
	response, requestErr := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/pods/default/multi/logs", nil), -1)
	if requestErr != nil || response.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("empty container status=%d err=%v", response.StatusCode, requestErr)
	}
	response, requestErr = app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/pods/default/multi/logs?container=sidecar", nil), -1)
	if requestErr != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("explicit container status=%d err=%v", response.StatusCode, requestErr)
	}
}
