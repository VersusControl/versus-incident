package controllers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

const intakeSecret = "test-gateway-secret"

// TestIntakeSettings_GetPutRoundTrip drives the intake settings endpoints: GET
// returns the default (auto-resolve ON) on a fresh store, PUT persists a
// disable, and a subsequent GET reflects it.
func TestIntakeSettings_GetPutRoundTrip(t *testing.T) {
	loadGatewayConfig(t, intakeSecret)
	config.GetConfig().GatewaySecret = intakeSecret
	st := storage.NewMemory()
	services.SetStorage(st)
	t.Cleanup(func() { services.SetStorage(nil) })

	app := fiber.New()
	api := app.Group("/api")
	NewIncidentAdminController().Register(api)

	// GET → default ON.
	getReq := httptest.NewRequest("GET", "/api/admin/incidents/intake-settings", nil)
	getReq.Header.Set("X-Gateway-Secret", intakeSecret)
	getResp, err := app.Test(getReq, -1)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var got services.IntakeSettings
	_ = json.NewDecoder(getResp.Body).Decode(&got)
	getResp.Body.Close()
	if !got.AutoResolveWebhook {
		t.Fatalf("default = %+v, want auto_resolve_webhook true", got)
	}

	// PUT → disable auto-resolve.
	putReq := httptest.NewRequest("PUT", "/api/admin/incidents/intake-settings", strings.NewReader(`{"auto_resolve_webhook":false}`))
	putReq.Header.Set("X-Gateway-Secret", intakeSecret)
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := app.Test(putReq, -1)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if putResp.StatusCode != fiber.StatusOK {
		t.Fatalf("PUT status = %d, want 200", putResp.StatusCode)
	}
	var afterPut services.IntakeSettings
	_ = json.NewDecoder(putResp.Body).Decode(&afterPut)
	putResp.Body.Close()
	if afterPut.AutoResolveWebhook {
		t.Fatalf("PUT response = %+v, want auto_resolve_webhook false", afterPut)
	}

	// GET again → reflects the disable.
	getReq2 := httptest.NewRequest("GET", "/api/admin/incidents/intake-settings", nil)
	getReq2.Header.Set("X-Gateway-Secret", intakeSecret)
	getResp2, err := app.Test(getReq2, -1)
	if err != nil {
		t.Fatalf("GET2: %v", err)
	}
	var got2 services.IntakeSettings
	_ = json.NewDecoder(getResp2.Body).Decode(&got2)
	getResp2.Body.Close()
	if got2.AutoResolveWebhook {
		t.Fatalf("after PUT, GET = %+v, want false", got2)
	}
}

// TestIntakeSettings_RequiresGatewaySecret proves the intake settings routes
// are gated by the same X-Gateway-Secret guard as the rest of the admin
// surface: a request without the header is rejected.
func TestIntakeSettings_RequiresGatewaySecret(t *testing.T) {
	loadGatewayConfig(t, intakeSecret)
	config.GetConfig().GatewaySecret = intakeSecret
	services.SetStorage(storage.NewMemory())
	t.Cleanup(func() { services.SetStorage(nil) })

	app := fiber.New()
	api := app.Group("/api")
	NewIncidentAdminController().Register(api)

	req := httptest.NewRequest("GET", "/api/admin/incidents/intake-settings", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without the gateway secret", resp.StatusCode)
	}
}
