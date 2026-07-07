package controllers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

const spikeSecret = "test-gateway-secret"

// TestSpikeSettingsRoutesRegistered guards the /admin/agent/spike-settings
// endpoints against silently dropping off the route table.
func TestSpikeSettingsRoutesRegistered(t *testing.T) {
	app := fiber.New()
	api := app.Group("/api")
	NewSpikeAdminController().Register(api)

	have := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		have[r.Method+" "+r.Path] = true
	}
	for _, key := range []string{
		"GET /api/admin/agent/spike-settings",
		"PUT /api/admin/agent/spike-settings",
	} {
		if !have[key] {
			t.Errorf("route %q not registered; have:\n%v", key, have)
		}
	}
}

// spikeApp mounts the controller over a memory store with the gateway secret
// configured, returning the app + store.
func spikeApp(t *testing.T) *fiber.App {
	t.Helper()
	loadGatewayConfig(t, spikeSecret)
	config.GetConfig().GatewaySecret = spikeSecret
	st := storage.NewMemory()
	services.SetStorage(st)
	t.Cleanup(func() { services.SetStorage(nil) })

	app := fiber.New()
	api := app.Group("/api")
	NewSpikeAdminController().Register(api)
	return app
}

// TestSpikeSettings_GetPutRoundTrip drives the settings endpoints: GET returns
// the built-in default on a fresh store, PUT persists a valid mode, and a
// subsequent GET reflects it.
func TestSpikeSettings_GetPutRoundTrip(t *testing.T) {
	app := spikeApp(t)

	// GET → default.
	getReq := httptest.NewRequest("GET", "/api/admin/agent/spike-settings", nil)
	getReq.Header.Set("X-Gateway-Secret", spikeSecret)
	getResp, err := app.Test(getReq, -1)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var defaults agent.SpikeSettings
	_ = json.NewDecoder(getResp.Body).Decode(&defaults)
	getResp.Body.Close()
	if defaults.BaselineMode != "default" {
		t.Fatalf("defaults = %+v, want baseline_mode=default", defaults)
	}

	// PUT → time_of_day.
	putReq := httptest.NewRequest("PUT", "/api/admin/agent/spike-settings", strings.NewReader(`{"baseline_mode":"time_of_day"}`))
	putReq.Header.Set("X-Gateway-Secret", spikeSecret)
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := app.Test(putReq, -1)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if putResp.StatusCode != fiber.StatusOK {
		t.Fatalf("PUT status = %d, want 200", putResp.StatusCode)
	}
	putResp.Body.Close()

	// GET again → reflects the update.
	getReq2 := httptest.NewRequest("GET", "/api/admin/agent/spike-settings", nil)
	getReq2.Header.Set("X-Gateway-Secret", spikeSecret)
	getResp2, err := app.Test(getReq2, -1)
	if err != nil {
		t.Fatalf("GET2: %v", err)
	}
	var updated agent.SpikeSettings
	_ = json.NewDecoder(getResp2.Body).Decode(&updated)
	getResp2.Body.Close()
	if updated.BaselineMode != "time_of_day" {
		t.Fatalf("updated = %+v, want baseline_mode=time_of_day", updated)
	}
}

// TestSpikeSettings_PutRejectsUnknownMode proves an unrecognized mode is
// rejected with a 400 and never persisted (the stored value stays the default).
func TestSpikeSettings_PutRejectsUnknownMode(t *testing.T) {
	app := spikeApp(t)

	putReq := httptest.NewRequest("PUT", "/api/admin/agent/spike-settings", strings.NewReader(`{"baseline_mode":"wat"}`))
	putReq.Header.Set("X-Gateway-Secret", spikeSecret)
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := app.Test(putReq, -1)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if putResp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("PUT status = %d, want 400", putResp.StatusCode)
	}
	putResp.Body.Close()

	// The bogus mode must not have been written — GET still returns the default.
	if got := agent.LoadSpikeSettings(services.Storage()); got.BaselineMode != "default" {
		t.Fatalf("stored after rejected PUT = %+v, want default", got)
	}
}

// TestSpikeSettings_GuardRejectsMissingSecret proves the endpoints share the
// same gateway-secret guard as the rest of the admin surface.
func TestSpikeSettings_GuardRejectsMissingSecret(t *testing.T) {
	app := spikeApp(t)

	for _, tc := range []struct {
		method, body string
	}{
		{"GET", ""},
		{"PUT", `{"baseline_mode":"average"}`},
	} {
		var reader *strings.Reader
		if tc.body != "" {
			reader = strings.NewReader(tc.body)
		} else {
			reader = strings.NewReader("")
		}
		req := httptest.NewRequest(tc.method, "/api/admin/agent/spike-settings", reader)
		// No X-Gateway-Secret header.
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("%s unauthenticated status = %d, want 401", tc.method, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
