package controllers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"
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
	for _, org := range []string{"org-a", "org-b"} {
		if err := manager.SaveSnapshot(org, servicehealth.SnapshotEnvelope{SnapshotID: org, GeneratedAt: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC), SettingsRevision: 0, Services: []servicehealth.ServiceSnapshot{{OrgID: org, Service: org}}}); err != nil {
			t.Fatal(err)
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
		t.Fatalf("envelope = %#v", envelope)
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
}
