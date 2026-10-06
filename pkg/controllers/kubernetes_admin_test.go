package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	k8stools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/k8s"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
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

func TestKubernetesTrafficRouteReturnsExplicitOSSUnavailableState(t *testing.T) {
	service := kubernetes.NewService(nil, kubernetes.Scope{ClusterID: "cluster-a"}, 0)
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	NewKubernetesAdminController(service).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/traffic?namespace=shop&window=5m", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("traffic route status=%d err=%v", response.StatusCode, err)
	}
	var result kubernetes.Traffic
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if result.Available || result.Reason == "" || result.Window != "5m" || len(result.Edges) != 0 {
		t.Fatalf("traffic response = %+v", result)
	}
}

func TestKubernetesGraphRouteForwardsCompleteQueryValidation(t *testing.T) {
	service := kubernetes.NewService(nil, kubernetes.Scope{ClusterID: "cluster-a"}, 0)
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	NewKubernetesAdminController(service).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/graph?complete=true", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("complete graph query status=%d err=%v", response.StatusCode, err)
	}
	response.Body.Close()
	for _, query := range []string{
		"complete=true&namespace=shop&limit=bad",
		"complete=true&namespace=shop&max_nodes=bad",
		"complete=true&namespace=shop&cursor=",
		"complete=true&namespace=shop&limit=",
		"complete=true&namespace=shop&max_nodes=",
		"complete=tru&namespace=shop",
		"complete=&namespace=shop",
		"complete=garbage&complete=true&namespace=shop",
	} {
		response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/graph?"+query, nil), -1)
		if err != nil || response.StatusCode != fiber.StatusBadRequest {
			t.Errorf("graph query %q status=%d err=%v", query, response.StatusCode, err)
		}
		response.Body.Close()
	}
}

func TestKubernetesOverviewGraphRouteRejectsQueriesAndRequiresPermission(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		app := fiber.New()
		app.Use(func(ctx *fiber.Ctx) error {
			middleware.MarkAuthorized(ctx)
			middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), allowed)
			return ctx.Next()
		})
		NewKubernetesAdminController(kubernetes.NewService(nil, kubernetes.Scope{ClusterID: "test"}, 0)).Register(app.Group("/api"))
		for _, query := range []string{"namespace=shop", "namespace=", "cursor=", "limit=1", "max_nodes=500", "complete=true", "connected_only=false", "unknown=value"} {
			response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/graph/overview?"+query, nil), -1)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			want := fiber.StatusForbidden
			if allowed {
				want = fiber.StatusBadRequest
			}
			if response.StatusCode != want {
				t.Errorf("allowed=%v query=%q status=%d want=%d", allowed, query, response.StatusCode, want)
			}
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
	for _, routes := range app.Stack() {
		for _, route := range routes {
			if route.Method == http.MethodGet && route.Path == "/api/admin/kubernetes/topology" {
				t.Fatal("topology route is registered")
			}
		}
	}
}

func TestKubernetesStreamRequiresInfrastructureView(t *testing.T) {
	var clusterRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		clusterRequests.Add(1)
		http.NotFound(writer, request)
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
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), false)
		return ctx.Next()
	})
	NewKubernetesAdminController(service).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/stream", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusForbidden || clusterRequests.Load() != 0 {
		t.Fatalf("stream status=%d cluster requests=%d err=%v", response.StatusCode, clusterRequests.Load(), err)
	}
}

func TestKubernetesChangesRouteReturnsBoundedDTOAndRejectsInvalidTime(t *testing.T) {
	service := kubernetes.NewService(nil, kubernetes.Scope{ClusterID: "cluster-a"}, 0)
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	store := kubechanges.NewStore(provider, kubeindex.Scope{ClusterID: "cluster-a"})
	now := time.Now().UTC()
	if err := store.Append([]kubechanges.Change{
		{ID: "change-a", Cluster: "cluster-a", Kind: "Deployment", Namespace: "shop", Name: "api-a", UID: "uid-a", Type: kubechanges.Created, At: now.Add(-time.Minute)},
		{ID: "change-b", Cluster: "cluster-a", Kind: "Deployment", Namespace: "shop", Name: "api-b", UID: "uid-b", Type: kubechanges.Created, At: now},
	}, now); err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	NewKubernetesAdminController(service).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/changes?limit=1", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("changes status=%d err=%v", response.StatusCode, err)
	}
	var body struct {
		Items     []kubernetes.Change `json:"items"`
		Next      string              `json:"next"`
		Truncated bool                `json:"truncated"`
		Gaps      []any               `json:"gaps"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || body.Items == nil || body.Gaps == nil || len(body.Items) != 1 || body.Next == "" || !body.Truncated {
		t.Fatalf("changes DTO=%#v decode err=%v", body, err)
	}
	response.Body.Close()
	second, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/changes?limit=1&cursor="+body.Next, nil), -1)
	if err != nil || second.StatusCode != fiber.StatusOK {
		t.Fatalf("changes cursor status=%d err=%v", second.StatusCode, err)
	}
	var secondPage struct {
		Items []kubernetes.Change `json:"items"`
		Next  string              `json:"next"`
	}
	if err := json.NewDecoder(second.Body).Decode(&secondPage); err != nil || secondPage.Items == nil || len(secondPage.Items) != 1 || secondPage.Items[0].Name == body.Items[0].Name || secondPage.Next != "" {
		t.Fatalf("changes cursor DTO=%#v decode err=%v", secondPage, err)
	}
	second.Body.Close()
	bad, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/changes?since=not-a-time", nil), -1)
	if err != nil || bad.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("invalid time status=%d err=%v", bad.StatusCode, err)
	}
}

func TestKubernetesRolloutsListRouteKeepsNameRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			_ = json.NewEncoder(writer).Encode(map[string]any{"versions": []string{"v1"}})
		case "/apis":
			_ = json.NewEncoder(writer).Encode(map[string]any{"groups": []any{map[string]any{"name": "argoproj.io", "versions": []any{map[string]any{"groupVersion": "argoproj.io/v1alpha1", "version": "v1alpha1"}}, "preferredVersion": map[string]any{"groupVersion": "argoproj.io/v1alpha1", "version": "v1alpha1"}}}})
		case "/api/v1":
			_ = json.NewEncoder(writer).Encode(map[string]any{"resources": []any{}})
		case "/apis/argoproj.io/v1alpha1":
			_ = json.NewEncoder(writer).Encode(map[string]any{"resources": []any{map[string]any{"name": "rollouts", "kind": "Rollout", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/argoproj.io/v1alpha1/namespaces/shop/rollouts":
			if request.URL.Query().Get("continue") == "page-2" {
				_ = json.NewEncoder(writer).Encode(map[string]any{"items": []any{map[string]any{"kind": "Rollout", "metadata": map[string]any{"name": "second", "namespace": "shop"}, "status": map[string]any{"phase": "Healthy"}}}})
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"metadata": map[string]any{"continue": "page-2"}, "items": []any{map[string]any{"kind": "Rollout", "metadata": map[string]any{"name": "checkout", "namespace": "shop"}, "status": map[string]any{"phase": "Progressing"}}}})
		case "/apis/argoproj.io/v1alpha1/namespaces/shop/rollouts/checkout":
			_ = json.NewEncoder(writer).Encode(map[string]any{"kind": "Rollout", "metadata": map[string]any{"name": "checkout", "namespace": "shop"}, "status": map[string]any{"phase": "Progressing"}})
		default:
			http.NotFound(writer, request)
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
	NewKubernetesAdminController(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "cluster-a"}, 0)).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/rollouts?namespace=shop&limit=1", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("rollouts route status=%d err=%v", response.StatusCode, err)
	}
	var page kubernetes.RolloutPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil || page.Items == nil || len(page.Items) != 1 || page.Next != "page-2" || !page.Truncated {
		t.Fatalf("rollouts response=%#v decode err=%v", page, err)
	}
	response.Body.Close()
	response, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/rollouts?namespace=shop&limit=1&cursor=page-2", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("rollouts cursor status=%d err=%v", response.StatusCode, err)
	}
	page = kubernetes.RolloutPage{}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil || len(page.Items) != 1 || page.Items[0].Name != "second" || page.Next != "" {
		t.Fatalf("rollouts cursor response=%#v decode err=%v", page, err)
	}
	response.Body.Close()
	response, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/rollouts/shop/checkout", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("named rollout route status=%d err=%v", response.StatusCode, err)
	}
	var rollout kubernetes.Rollout
	if err := json.NewDecoder(response.Body).Decode(&rollout); err != nil || rollout.Name != "checkout" {
		t.Fatalf("named rollout response=%#v decode err=%v", rollout, err)
	}
	response.Body.Close()
}

func TestKubernetesEventsRouteDefaultsWarningAndPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			_ = json.NewEncoder(writer).Encode(map[string]any{"versions": []string{"v1"}})
		case "/apis":
			_ = json.NewEncoder(writer).Encode(map[string]any{"groups": []any{}})
		case "/api/v1":
			_ = json.NewEncoder(writer).Encode(map[string]any{"resources": []any{map[string]any{"name": "events", "kind": "Event", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/shop/events":
			if request.URL.Query().Get("fieldSelector") != "type=Warning" {
				t.Errorf("event field selector=%q", request.URL.Query().Get("fieldSelector"))
			}
			if request.URL.Query().Get("continue") == "page-2" {
				_ = json.NewEncoder(writer).Encode(map[string]any{"items": []any{map[string]any{"kind": "Event", "metadata": map[string]any{"name": "second", "namespace": "shop"}, "type": "Warning"}}})
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"metadata": map[string]any{"continue": "page-2"}, "items": []any{map[string]any{"kind": "Event", "metadata": map[string]any{"name": "first", "namespace": "shop"}, "type": "Warning"}}})
		default:
			http.NotFound(writer, request)
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
	NewKubernetesAdminController(kubernetes.NewService(client, kubernetes.Scope{ClusterID: "cluster-a"}, 0)).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/events?namespace=shop&limit=1", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("events route status=%d err=%v", response.StatusCode, err)
	}
	var page kubernetes.EventPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil || page.Items == nil || len(page.Items) != 1 || page.Items[0].Name != "first" || page.Next != "page-2" {
		t.Fatalf("events response=%#v decode err=%v", page, err)
	}
	response.Body.Close()
	response, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/events?namespace=shop&limit=1&cursor=page-2", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("events cursor status=%d err=%v", response.StatusCode, err)
	}
	page = kubernetes.EventPage{}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil || page.Items == nil || len(page.Items) != 1 || page.Items[0].Name != "second" || page.Next != "" {
		t.Fatalf("events cursor response=%#v decode err=%v", page, err)
	}
	response.Body.Close()
}

func TestChatKubernetesAttachmentRequiresScopedInfrastructurePermission(t *testing.T) {
	var discoveryRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		discoveryRequests.Add(1)
		switch request.URL.Path {
		case "/api":
			_ = json.NewEncoder(writer).Encode(map[string]any{"versions": []string{"v1"}})
		case "/apis":
			_ = json.NewEncoder(writer).Encode(map[string]any{"groups": []any{}})
		case "/api/v1":
			_ = json.NewEncoder(writer).Encode(map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	registry := kubernetes.NewServiceRegistry(kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: "cluster-a"}, 0))
	SetChatKubernetesServiceResolver(func(orgID string) *kubernetes.Service { return registry.ResolveOrg(orgID) })
	t.Cleanup(func() { SetChatKubernetesServiceResolver(nil); middleware.SetOrgResolver(nil) })
	middleware.SetOrgResolver(func(ctx *fiber.Ctx) string { return ctx.Get("X-Test-Org") })
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), ctx.Get("X-Allow-Infra") == "true")
		return ctx.Next()
	})
	app.Get("/validate", func(ctx *fiber.Ctx) error {
		attachment := &core.ChatAttachment{Resource: &core.ChatResourceRef{Provider: "kubernetes", Cluster: ctx.Query("cluster"), ResourceID: "core~v1~pods", Namespace: "shop", Name: "checkout"}}
		if err := validateChatKubernetesAttachment(ctx, attachment); err != nil {
			return err
		}
		return ctx.JSON(attachment)
	})
	request := func(cluster, allowed string) (*http.Response, error) {
		req := httptest.NewRequest(http.MethodGet, "/validate?cluster="+cluster, nil)
		req.Header.Set("X-Test-Org", "org-a")
		req.Header.Set("X-Allow-Infra", allowed)
		return app.Test(req, -1)
	}
	allowed, err := request("cluster-a", "true")
	if err != nil || allowed.StatusCode != fiber.StatusOK {
		t.Fatalf("allowed attachment status=%d err=%v", allowed.StatusCode, err)
	}
	wrongCluster, err := request("cluster-b", "true")
	if err != nil || wrongCluster.StatusCode != fiber.StatusForbidden {
		t.Fatalf("foreign cluster status=%d err=%v", wrongCluster.StatusCode, err)
	}
	denied, err := request("cluster-a", "false")
	if err != nil || denied.StatusCode != fiber.StatusForbidden {
		t.Fatalf("missing permission status=%d err=%v", denied.StatusCode, err)
	}
	if discoveryRequests.Load() != 3 {
		t.Fatalf("unauthorized attachment attempts caused discovery egress: requests=%d", discoveryRequests.Load())
	}
}

func TestWriteKubernetesReturnsSafeActionableDiagnostics(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "credentials", err: kubernetes.ErrCredentialUnavailable, status: fiber.StatusBadGateway, code: "credential_unavailable"},
		{name: "authentication", err: kubernetes.ErrUnauthorized, status: fiber.StatusBadGateway, code: "cluster_authentication_failed"},
		{name: "permission", err: kubernetes.ErrForbidden, status: fiber.StatusForbidden, code: "cluster_permission_denied"},
		{name: "configuration", err: kubernetes.ErrInvalidEndpoint, status: fiber.StatusBadRequest, code: "connector_configuration_invalid"},
		{name: "timeout", err: context.DeadlineExceeded, status: fiber.StatusGatewayTimeout, code: "request_timeout"},
		{name: "fallback", err: errors.New("provider secret response"), status: fiber.StatusBadGateway, code: "read_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previous) })
			app := fiber.New()
			app.Get("/", func(ctx *fiber.Ctx) error { return writeKubernetes(ctx, nil, test.err) })
			response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil), -1)
			if err != nil || response.StatusCode != test.status {
				t.Fatalf("status=%d err=%v", response.StatusCode, err)
			}
			var body struct {
				Error     string `json:"error"`
				Code      string `json:"code"`
				Action    string `json:"action"`
				Retryable bool   `json:"retryable"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Code != test.code || body.Error == "" || body.Action == "" {
				t.Fatalf("body = %+v", body)
			}
			if bytes.Contains(logs.Bytes(), []byte("provider secret response")) || body.Error == "provider secret response" || body.Action == "provider secret response" {
				t.Fatalf("raw cause leaked: body=%+v logs=%q", body, logs.String())
			}
			if !bytes.Contains(logs.Bytes(), []byte("code="+test.code)) {
				t.Fatalf("safe code absent from log: %q", logs.String())
			}
		})
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
	for _, orgID := range []string{"org-a", "org-b"} {
		request := httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/graph/overview", nil)
		request.Header.Set("X-Test-Org", orgID)
		response, err := app.Test(request, -1)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != fiber.StatusOK || controller.resolve(orgID).Scope().OrgID != orgID {
			t.Fatalf("overview org=%q status=%d scope=%+v", orgID, response.StatusCode, controller.resolve(orgID).Scope())
		}
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
