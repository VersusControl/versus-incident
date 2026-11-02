package controllers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"github.com/gofiber/fiber/v2"
)

func TestWorkloadsHTTPRestartDistinguishesWarmingDeniedAndCompleteEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: restart durable inventory and await autonomous authority recovery")
	}
	gateway := middleware.GatewayAuthEnabled()
	middleware.SetGatewayAuthEnabled(false)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(gateway) })
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
	if err != nil {
		t.Fatal(err)
	}
	seed := httptest.NewRecorder()
	fixture.ServeHTTP(seed, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"synthetic-physical"}}}`)))
	if seed.Code != 200 {
		t.Fatal("namespace fixture failed")
	}
	var deny, empty, hold atomic.Bool
	blocked := make(chan struct{})
	deniedDiscovery := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hold.Load() {
			select {
			case <-blocked:
			case <-r.Context().Done():
				return
			}
		}
		if deny.Load() {
			if r.URL.Path == "/api/v1" {
				select {
				case deniedDiscovery <- struct{}{}:
				default:
				}
			}
			http.Error(w, "forbidden", 403)
			return
		}
		if empty.Load() {
			for _, kind := range []string{"pods", "deployments", "replicasets", "statefulsets", "daemonsets", "jobs", "cronjobs", "events"} {
				if strings.HasSuffix(r.URL.Path, "/"+kind) {
					kindName, api := "List", "v1"
					if strings.Contains(r.Header.Get("Accept"), "PartialObjectMetadataList") {
						kindName, api = "PartialObjectMetadataList", "meta.k8s.io/v1"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"kind": kindName, "apiVersion": api, "items": []any{}})
					return
				}
			}
		}
		fixture.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	open := func() storage.Provider {
		p, err := storage.NewFile(storage.FileOptions{DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	construct := func(provider storage.Provider, credential string) (*kubernetes.Service, *kubernetes.ServiceRegistry, *fiber.App) {
		client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: credential}, time.Minute)
		service.SetChangeStorage(provider)
		registry := kubernetes.NewServiceRegistry(service)
		t.Cleanup(registry.Close)
		app := fiber.New(fiber.Config{Immutable: true, DisableStartupMessage: true})
		app.Use(func(c *fiber.Ctx) error {
			middleware.MarkAuthorized(c)
			middleware.SetRequestPermission(c, string(core.PermissionInfrastructureView), true)
			return c.Next()
		})
		NewKubernetesAdminControllerWithRegistry(registry).Register(app.Group("/api"))
		return service, registry, app
	}
	request := func(app *fiber.App, path string) (int, kubernetes.WorkloadPage, string) {
		response, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil), 15000)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var page kubernetes.WorkloadPage
		_ = json.Unmarshal(raw, &page)
		return response.StatusCode, page, string(raw)
	}
	original, registry, app := construct(open(), "old-epoch")
	registry.StartCapture(t.Context())
	deadline := time.Now().Add(20 * time.Second)
	for {
		health, healthErr := original.SyncHealth(t.Context())
		if healthErr == nil && !health.FirstReadyAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("original inventory never published")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, path := range []string{"/api/admin/kubernetes/workloads?cluster=cluster", "/api/admin/kubernetes/workloads"} {
		status, page, _ := request(app, path)
		if status != 200 || len(page.Items) == 0 {
			t.Fatalf("before restart: status=%d page=%+v", status, page)
		}
	}
	registry.Close()
	// A different epoch sees the durable first-ready marker but must not borrow
	// the previous epoch's inventory. Hold collection off to inspect that boundary.
	hold.Store(true)
	warming, warmingRegistry, warmingApp := construct(open(), "new-epoch")
	warmingRegistry.StartCapture(t.Context())
	health, err := warming.SyncHealth(t.Context())
	if err != nil || health.FirstReadyAt.IsZero() || health.PendingKinds == 0 {
		t.Fatalf("warming health=%+v err=%v", health, err)
	}
	for _, path := range []string{"/api/admin/kubernetes/workloads?cluster=cluster", "/api/admin/kubernetes/workloads"} {
		status, page, raw := request(warmingApp, path)
		if status != 503 || !strings.Contains(raw, `"code":"inventory_not_ready"`) || !strings.Contains(raw, `"retryable":true`) || strings.Contains(raw, "checkout") {
			t.Errorf("warming restart returned authoritative success or old scope: status=%d sync=%+v body=%s", status, page.Sync, raw)
		}
	}
	warmingRegistry.Close()
	hold.Store(false)
	close(blocked)
	// Same-scope cold restore must prove current authority, not replay old data.
	deny.Store(true)
	denied, deniedRegistry, deniedApp := construct(open(), "old-epoch")
	deniedRegistry.StartCapture(t.Context())
	// Admit requests only after the producer has stored its failed-cycle state,
	// not just after one request independently proved the authority denial.
	select {
	case <-deniedDiscovery:
	case <-time.After(5 * time.Second):
		t.Fatal("denied discovery was never attempted")
	}
	phaseDeadline := time.NewTimer(5 * time.Second)
	defer phaseDeadline.Stop()
	phaseTick := time.NewTicker(5 * time.Millisecond)
	defer phaseTick.Stop()
	for {
		report, reportErr := denied.SyncHealth(t.Context())
		if reportErr == nil && report.State == "error" && report.FailingKinds > 0 {
			break
		}
		select {
		case <-phaseDeadline.C:
			t.Fatal("producer did not publish its authority-failure state")
		case <-phaseTick.C:
		}
	}
	for _, path := range []string{"/api/admin/kubernetes/workloads?cluster=cluster", "/api/admin/kubernetes/workloads"} {
		status, _, raw := request(deniedApp, path)
		if status != 403 || strings.Contains(raw, "checkout") {
			t.Errorf("denied cold restore path=%s: status=%d body=%s", path, status, raw)
		}
	}
	deniedRegistry.Close()
	deny.Store(false)
	_, restoredRegistry, restoredApp := construct(open(), "old-epoch")
	restoredRegistry.StartCapture(t.Context())
	status, page, _ := request(restoredApp, "/api/admin/kubernetes/workloads?cluster=cluster")
	if status != 200 || len(page.Items) == 0 {
		t.Fatalf("authorized cold restore lost last-good inventory: status=%d page=%+v", status, page)
	}
	missingStatus, _, missing := request(restoredApp, "/api/admin/kubernetes/workloads/Pod/definitely-absent?cluster=cluster&namespace=shop")
	if missingStatus != 404 || !strings.Contains(missing, `"code":"resource_unavailable"`) || !strings.Contains(missing, `"retryable":false`) {
		t.Fatalf("authoritative missing object was reclassified as warming: status=%d body=%s", missingStatus, missing)
	}
	restoredRegistry.Close()
	// A real completed empty LIST remains a successful zero, not a blanket error.
	empty.Store(true)
	emptyProvider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	complete, emptyRegistry, emptyApp := construct(emptyProvider, "complete-empty")
	emptyRegistry.StartCapture(t.Context())
	deadline = time.Now().Add(20 * time.Second)
	for {
		health, healthErr := complete.SyncHealth(t.Context())
		if healthErr == nil && !health.FirstReadyAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("complete-empty inventory never published: %+v %v", health, healthErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, path := range []string{"/api/admin/kubernetes/workloads?cluster=cluster", "/api/admin/kubernetes/workloads"} {
		status, page, _ := request(emptyApp, path)
		if status != 200 || len(page.Items) != 0 || page.Sync.State != "ready" || page.Sync.Partial {
			t.Fatalf("complete empty: status=%d page=%+v", status, page)
		}
	}
}
