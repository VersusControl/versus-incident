package controllers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"
	"github.com/VersusControl/versus-incident/pkg/servicetopology"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

func serviceHealthTestApp(t *testing.T, manager *servicehealth.Manager) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.SetRequestPermission(ctx, string(core.PermissionServiceHealthSettingsWrite), true)
		return ctx.Next()
	})
	controller := NewServiceHealthController(manager)
	app.Get("/api/agent/service-health", controller.getServiceHealth)
	app.Get("/api/agent/service-health/settings", controller.getServiceHealthSettings)
	app.Patch("/api/agent/service-health/settings", controller.patchServiceHealthSettings)
	return app
}

func TestServiceHealthEmptyInstallAndSettingsRoundTrip(t *testing.T) {
	app := serviceHealthTestApp(t, servicehealth.NewManager(storage.NewMemory()))
	response, err := app.Test(httptest.NewRequest("GET", "/api/agent/service-health", nil))
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("GET status = %v, err %v", response.StatusCode, err)
	}
	var envelope servicehealth.SnapshotEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Services) != 0 || len(envelope.Capabilities) == 0 {
		t.Fatalf("envelope = %#v", envelope)
	}
	request := httptest.NewRequest("PATCH", "/api/agent/service-health/settings", strings.NewReader(`{"interval_seconds":30,"window_seconds":60,"revision":0}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = app.Test(request)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("PATCH status = %v, err %v", response.StatusCode, err)
	}
	var settings servicehealth.Settings
	if err := json.NewDecoder(response.Body).Decode(&settings); err != nil {
		t.Fatal(err)
	}
	if settings.Revision != 1 {
		t.Fatalf("settings = %#v", settings)
	}
}

func TestServiceTopologyStaticGraphIsDeterministicAndDoesNotBlockHealth(t *testing.T) {
	health := servicehealth.NewManager(storage.NewMemory())
	graph := commontools.NewDependencyGraph(map[string][]string{
		"web": {"db", "api", "api"},
		"api": {"db"},
	})
	controller := NewServiceHealthControllerWithTopology(health, servicetopology.NewManager(graph, nil))
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	app.Get("/api/agent/service-topology", controller.getServiceTopology)
	app.Get("/api/agent/service-health", controller.getServiceHealth)

	response, err := app.Test(httptest.NewRequest("GET", "/api/agent/service-topology", nil))
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("topology status = %v, err %v", response.StatusCode, err)
	}
	var topology core.ServiceTopology
	if err := json.NewDecoder(response.Body).Decode(&topology); err != nil {
		t.Fatal(err)
	}
	wantNodes := []core.ServiceTopologyNode{{Service: "api"}, {Service: "db"}, {Service: "web"}}
	wantEdges := []core.ServiceTopologyEdge{
		{Service: "api", DependsOn: "db", Source: "operator_config"},
		{Service: "web", DependsOn: "api", Source: "operator_config"},
		{Service: "web", DependsOn: "db", Source: "operator_config"},
	}
	if topology.Availability != core.HealthReady || !reflect.DeepEqual(topology.Nodes, wantNodes) || !reflect.DeepEqual(topology.Edges, wantEdges) {
		t.Fatalf("topology = %#v", topology)
	}
	response, err = app.Test(httptest.NewRequest("GET", "/api/agent/service-health", nil))
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("service health status = %v, err %v", response.StatusCode, err)
	}
}

func TestServiceTopologyMissingGraphIsUnavailableAndHealthStillWorks(t *testing.T) {
	controller := NewServiceHealthControllerWithTopology(servicehealth.NewManager(storage.NewMemory()), servicetopology.NewManager(nil, nil))
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	app.Get("/topology", controller.getServiceTopology)
	app.Get("/health", controller.getServiceHealth)

	response, _ := app.Test(httptest.NewRequest("GET", "/topology", nil))
	var topology core.ServiceTopology
	if err := json.NewDecoder(response.Body).Decode(&topology); err != nil {
		t.Fatal(err)
	}
	if topology.Availability != core.HealthNotConfigured || len(topology.Nodes) != 0 || len(topology.Edges) != 0 {
		t.Fatalf("topology = %#v", topology)
	}
	response, _ = app.Test(httptest.NewRequest("GET", "/health", nil))
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("service health status = %d", response.StatusCode)
	}
}

func TestServiceTopologyRequiresExplicitInfrastructurePermission(t *testing.T) {
	controller := NewServiceHealthControllerWithTopology(servicehealth.NewManager(storage.NewMemory()), servicetopology.NewManager(nil, nil))
	for _, test := range []struct {
		name       string
		permission *bool
		want       int
	}{
		{name: "missing", want: fiber.StatusForbidden},
		{name: "denied", permission: boolPointer(false), want: fiber.StatusForbidden},
		{name: "allowed", permission: boolPointer(true), want: fiber.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := fiber.New(fiber.Config{Immutable: true})
			if test.permission != nil {
				app.Use(func(ctx *fiber.Ctx) error {
					middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), *test.permission)
					return ctx.Next()
				})
			}
			app.Get("/topology", controller.getServiceTopology)
			response, err := app.Test(httptest.NewRequest("GET", "/topology", nil))
			if err != nil || response.StatusCode != test.want {
				t.Fatalf("status = %v, err = %v", response.StatusCode, err)
			}
		})
	}

	app := fiber.New(fiber.Config{Immutable: true})
	app.Get("/health", controller.getServiceHealth)
	response, _ := app.Test(httptest.NewRequest("GET", "/health", nil))
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("independent Service Health status = %d", response.StatusCode)
	}
}

func boolPointer(value bool) *bool { return &value }

func TestServiceHealthSettingsBoundsConflictAndPermission(t *testing.T) {
	manager := servicehealth.NewManager(storage.NewMemory())
	app := serviceHealthTestApp(t, manager)
	for _, body := range []string{`{"interval_seconds":0,"window_seconds":60,"revision":0}`, `{"interval_seconds":90,"window_seconds":60,"revision":0}`} {
		request := httptest.NewRequest("PATCH", "/api/agent/service-health/settings", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response, _ := app.Test(request)
		if response.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("body %s status = %d", body, response.StatusCode)
		}
	}
	if _, err := manager.UpdateSettings("default", servicehealth.Settings{IntervalSeconds: 30, WindowSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("PATCH", "/api/agent/service-health/settings", strings.NewReader(`{"interval_seconds":60,"window_seconds":300,"revision":0}`))
	request.Header.Set("Content-Type", "application/json")
	response, _ := app.Test(request)
	if response.StatusCode != fiber.StatusConflict {
		t.Fatalf("conflict status = %d", response.StatusCode)
	}

	denied := fiber.New(fiber.Config{Immutable: true})
	denied.Use(func(ctx *fiber.Ctx) error {
		middleware.SetRequestPermission(ctx, string(core.PermissionServiceHealthSettingsWrite), false)
		return ctx.Next()
	})
	denied.Patch("/settings", NewServiceHealthController(manager).patchServiceHealthSettings)
	response, _ = denied.Test(httptest.NewRequest("PATCH", "/settings", strings.NewReader(`{"interval_seconds":60,"window_seconds":300,"revision":1}`)))
	if response.StatusCode != fiber.StatusForbidden {
		t.Fatalf("denied status = %d", response.StatusCode)
	}
	unknown := fiber.New(fiber.Config{Immutable: true})
	unknown.Patch("/settings", NewServiceHealthController(manager).patchServiceHealthSettings)
	response, _ = unknown.Test(httptest.NewRequest("PATCH", "/settings", strings.NewReader(`{"interval_seconds":60,"window_seconds":300,"revision":1}`)))
	if response.StatusCode != fiber.StatusForbidden {
		t.Fatalf("unknown permission status = %d", response.StatusCode)
	}
}

func TestServiceHealthSnapshotIsOrgIsolated(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	generatedAt := time.Now().UTC()
	for _, org := range []string{"org-a", "org-b"} {
		if err := manager.SaveSnapshot(org, servicehealth.SnapshotEnvelope{SnapshotID: org, GeneratedAt: generatedAt, SettingsRevision: 0, Services: []servicehealth.ServiceSnapshot{{OrgID: org, Service: org}}}); err != nil {
			t.Fatal(err)
		}
		stored, ok, err := manager.LoadSnapshot(org)
		if err != nil || !ok || stored.SnapshotID != org {
			t.Fatalf("stored snapshot for %q = id %q, found %t, err %v", org, stored.SnapshotID, ok, err)
		}
	}
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error { ctx.Locals(middleware.OrgContextKey, "org-a"); return ctx.Next() })
	controller := NewServiceHealthController(manager)
	app.Get("/health", controller.getServiceHealth)
	response, _ := app.Test(httptest.NewRequest("GET", "/health", nil))
	var envelope servicehealth.SnapshotEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Services) != 1 || envelope.Services[0].OrgID != "org-a" {
		t.Fatalf("snapshot id %q services = %#v, want one org-a service", envelope.SnapshotID, envelope.Services)
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"raw", "message", "credentials", "cursor", "silent_regressions", "regressing"} {
		if strings.Contains(strings.ToLower(string(body)), forbidden) {
			t.Fatalf("response contains forbidden field %q: %s", forbidden, body)
		}
	}
}

func TestServiceHealthAPIKeepsProducingWindowWhenSettingsArePending(t *testing.T) {
	manager := servicehealth.NewManager(storage.NewMemory())
	old := servicehealth.SnapshotEnvelope{SnapshotID: "old", GeneratedAt: time.Now().UTC(), SettingsRevision: 0, WindowSeconds: 300}
	if err := manager.SaveSnapshot("default", old); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UpdateSettings("default", servicehealth.Settings{IntervalSeconds: 30, WindowSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	response, err := serviceHealthTestApp(t, manager).Test(httptest.NewRequest("GET", "/api/agent/service-health", nil))
	if err != nil {
		t.Fatal(err)
	}
	var envelope servicehealth.SnapshotEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SettingsRevision != 0 || envelope.WindowSeconds != 300 || envelope.PendingSettings == nil || envelope.PendingSettings.Revision != 1 || envelope.PendingSettings.WindowSeconds != 60 {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestServiceHealthRouteRequiresGatewayAuthorization(t *testing.T) {
	if config.GetConfigOrNil() == nil {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("name: service-health-test\nhost: 127.0.0.1\nport: 3000\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := config.LoadConfig(path); err != nil {
			t.Fatal(err)
		}
	}
	app := fiber.New(fiber.Config{Immutable: true})
	NewServiceHealthController(servicehealth.NewManager(storage.NewMemory())).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest("GET", "/api/agent/service-health", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.ReadAll(response.Body)
	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}
	response, err = app.Test(httptest.NewRequest("GET", "/api/agent/service-topology", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.ReadAll(response.Body)
	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("topology status = %d", response.StatusCode)
	}
}
