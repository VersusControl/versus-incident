package controllers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// newOverrideApp builds a controller over a fresh catalog + override store and
// mounts the service-CRUD and override handlers directly (bypassing the auth
// middleware, which is covered by the auth-sweep test), returning the app plus
// the two stores so a test can assert side effects.
func newOverrideApp(t *testing.T) (*fiber.App, *agent.Catalog, *agent.ServiceOverrideStore) {
	t.Helper()
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	ov, err := agent.LoadServiceOverrideStore(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadServiceOverrideStore: %v", err)
	}
	ctrl := NewAgentController(cat, nil, nil, nil, ov, false)
	app := fiber.New()
	app.Post("/api/agent/services", ctrl.createService)
	app.Put("/api/agent/services/:name", ctrl.renameService)
	app.Delete("/api/agent/services/:name", ctrl.deleteService)
	app.Get("/api/agent/service-overrides", ctrl.listServiceOverrides)
	app.Post("/api/agent/service-overrides", ctrl.createServiceOverride)
	app.Delete("/api/agent/service-overrides/:id", ctrl.deleteServiceOverride)
	return app, cat, ov
}

func doJSON(t *testing.T, app *fiber.App, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

// TestCreateService_HappyAndConflict proves POST /services creates a manual
// service and rejects a duplicate with 409.
func TestCreateService_HappyAndConflict(t *testing.T) {
	app, cat, _ := newOverrideApp(t)

	code, body := doJSON(t, app, "POST", "/api/agent/services", map[string]any{"name": "payments"})
	if code != fiber.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%v", code, body)
	}
	if body["manual"] != true {
		t.Errorf("manual = %v, want true", body["manual"])
	}
	if info, ok := cat.Service("payments"); !ok || !info.Manual {
		t.Errorf("service not recorded as manual: ok=%v", ok)
	}

	code, _ = doJSON(t, app, "POST", "/api/agent/services", map[string]any{"name": "payments"})
	if code != fiber.StatusConflict {
		t.Errorf("duplicate create status = %d, want 409", code)
	}

	code, _ = doJSON(t, app, "POST", "/api/agent/services", map[string]any{"name": "  "})
	if code != fiber.StatusBadRequest {
		t.Errorf("blank name status = %d, want 400", code)
	}
}

// TestRenameService_RepointsOverrides proves renaming a manual service repoints
// its override rules and refuses to rename an auto-discovered service.
func TestRenameService_RepointsOverrides(t *testing.T) {
	app, cat, ov := newOverrideApp(t)
	_ = cat.CreateService("old")
	if _, err := ov.Put(storage.DefaultOrgID, agent.OverrideRule{
		SourceType: agent.OverrideSourceLog, Match: "p-1", Service: "old",
	}); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	code, body := doJSON(t, app, "PUT", "/api/agent/services/old", map[string]any{"name": "new"})
	if code != fiber.StatusOK {
		t.Fatalf("rename status = %d, want 200; body=%v", code, body)
	}
	if body["overrides_repointed"] != float64(1) {
		t.Errorf("overrides_repointed = %v, want 1", body["overrides_repointed"])
	}
	if n := ov.CountForService(storage.DefaultOrgID, "new"); n != 1 {
		t.Errorf("new service targeted by %d rules, want 1", n)
	}

	// Auto-discovered service cannot be renamed.
	cat.RegisterService("auto")
	code, _ = doJSON(t, app, "PUT", "/api/agent/services/auto", map[string]any{"name": "auto2"})
	if code != fiber.StatusBadRequest {
		t.Errorf("rename auto status = %d, want 400", code)
	}

	// Missing service → 404.
	code, _ = doJSON(t, app, "PUT", "/api/agent/services/ghost", map[string]any{"name": "x"})
	if code != fiber.StatusNotFound {
		t.Errorf("rename missing status = %d, want 404", code)
	}
}

// TestDeleteService_BlockedWhenOverridesTarget proves a manual service with
// override rules cannot be deleted (409) until the overrides are removed.
func TestDeleteService_BlockedWhenOverridesTarget(t *testing.T) {
	app, cat, ov := newOverrideApp(t)
	_ = cat.CreateService("payments")
	rule, _ := ov.Put(storage.DefaultOrgID, agent.OverrideRule{
		SourceType: agent.OverrideSourceLog, Match: "p-1", Service: "payments",
	})

	code, body := doJSON(t, app, "DELETE", "/api/agent/services/payments", nil)
	if code != fiber.StatusConflict {
		t.Fatalf("blocked delete status = %d, want 409; body=%v", code, body)
	}
	if body["overrides"] != float64(1) {
		t.Errorf("overrides count = %v, want 1", body["overrides"])
	}

	// Remove the override, then the delete succeeds.
	if ok, _ := ov.Delete(storage.DefaultOrgID, rule.ID); !ok {
		t.Fatalf("delete override failed")
	}
	code, _ = doJSON(t, app, "DELETE", "/api/agent/services/payments", nil)
	if code != fiber.StatusNoContent {
		t.Errorf("delete status = %d, want 204", code)
	}
	if _, ok := cat.Service("payments"); ok {
		t.Errorf("service still present after delete")
	}
}

// TestDeleteService_AutoRejected proves an auto-discovered service cannot be
// deleted (400).
func TestDeleteService_AutoRejected(t *testing.T) {
	app, cat, _ := newOverrideApp(t)
	cat.RegisterService("auto")
	code, _ := doJSON(t, app, "DELETE", "/api/agent/services/auto", nil)
	if code != fiber.StatusBadRequest {
		t.Errorf("delete auto status = %d, want 400", code)
	}
}

// TestCreateServiceOverride_RequiresExistingTarget proves an override can only
// point at a service that exists (referential integrity with delete-block).
func TestCreateServiceOverride_RequiresExistingTarget(t *testing.T) {
	app, cat, ov := newOverrideApp(t)

	// Unknown target → 400.
	code, _ := doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
		"source_type": "log", "match": "p-1", "service": "ghost",
	})
	if code != fiber.StatusBadRequest {
		t.Fatalf("override to unknown service status = %d, want 400", code)
	}

	// Create the target, then the override is accepted.
	_ = cat.CreateService("payments")
	code, body := doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
		"source_type": "log", "match": "p-1", "service": "payments",
	})
	if code != fiber.StatusCreated {
		t.Fatalf("override create status = %d, want 201; body=%v", code, body)
	}
	if body["service"] != "payments" {
		t.Errorf("service = %v, want payments", body["service"])
	}
	if n := len(ov.List(storage.DefaultOrgID)); n != 1 {
		t.Errorf("stored rule count = %d, want 1", n)
	}

	// Invalid source_type → 400.
	code, _ = doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
		"source_type": "bogus", "match": "x", "service": "payments",
	})
	if code != fiber.StatusBadRequest {
		t.Errorf("bad source_type status = %d, want 400", code)
	}
}

// TestListAndDeleteServiceOverride covers the list + delete endpoints.
func TestListAndDeleteServiceOverride(t *testing.T) {
	app, cat, ov := newOverrideApp(t)
	_ = cat.CreateService("payments")
	rule, _ := ov.Put(storage.DefaultOrgID, agent.OverrideRule{
		SourceType: agent.OverrideSourceMetric, Match: "http_5xx", Service: "payments",
	})

	code, body := doJSON(t, app, "GET", "/api/agent/service-overrides", nil)
	if code != fiber.StatusOK {
		t.Fatalf("list status = %d, want 200", code)
	}
	if arr, ok := body["overrides"].([]any); !ok || len(arr) != 1 {
		t.Errorf("overrides = %v, want 1 entry", body["overrides"])
	}

	code, _ = doJSON(t, app, "DELETE", "/api/agent/service-overrides/"+rule.ID, nil)
	if code != fiber.StatusNoContent {
		t.Errorf("delete status = %d, want 204", code)
	}
	code, _ = doJSON(t, app, "DELETE", "/api/agent/service-overrides/missing", nil)
	if code != fiber.StatusNotFound {
		t.Errorf("delete missing status = %d, want 404", code)
	}
}

// TestCreateServiceOverride_ImmediatelyRepointsLogPattern is the regression
// proof for the "reassign never takes effect" bug: creating a log override
// whose match is an existing pattern id must re-point that pattern's Service
// IMMEDIATELY — visible on the very next read, with NO re-observation / fresh
// matching traffic. A substring match (no pattern with that id) must NOT touch
// any pattern (it falls back to the lazy re-observation path).
func TestCreateServiceOverride_ImmediatelyRepointsLogPattern(t *testing.T) {
	agent.SetCatalogStore(nil)
	app, cat, _ := newOverrideApp(t)

	// A pattern is already learned and attributed to the wrong service. No
	// worker is running, so NO fresh matching log line will ever re-cluster it.
	const patternID = "p-ec7767235887"
	cat.Upsert(patternID, "cache miss for key <*>", "app-logs", 3, 0.2, "", "web")
	if got := cat.Get(patternID); got == nil || got.Service != "web" {
		t.Fatalf("seed Service = %v, want web", got)
	}

	// The operator creates the target service, then reassigns the pattern.
	_ = cat.CreateService("cache")
	code, body := doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
		"source_type": "log", "match": patternID, "service": "cache",
	})
	if code != fiber.StatusCreated {
		t.Fatalf("override create status = %d, want 201; body=%v", code, body)
	}

	// IMMEDIATELY — the very next read — reflects the new service, with no
	// re-observation. This is exactly what GET /api/agent/patterns serves.
	if got := cat.Get(patternID); got == nil || got.Service != "cache" {
		t.Fatalf("Service after reassign = %v, want cache (immediate re-point, no re-observation)", got)
	}
	// Nothing else regressed: the fold count is untouched.
	if got := cat.Get(patternID); got.Count != 3 {
		t.Errorf("Count = %d, want 3 (re-point must not disturb the fold)", got.Count)
	}

	// A substring-match override (no pattern with that id) must NOT retroactively
	// touch any pattern — it relies on the lazy re-observation path instead.
	cat.Upsert("p-other", "connection refused <*>", "app-logs", 1, 0.2, "", "web")
	code, _ = doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
		"source_type": "log", "match": "connection refused", "service": "cache",
	})
	if code != fiber.StatusCreated {
		t.Fatalf("substring override create status = %d, want 201", code)
	}
	if got := cat.Get("p-other"); got == nil || got.Service != "web" {
		t.Errorf("substring override retroactively re-pointed p-other to %v; want web untouched", got)
	}
}

// TestServiceOverrideRoutesRegistered guards the new routes in the table.
func TestServiceOverrideRoutesRegistered(t *testing.T) {
	app := fiber.New()
	api := app.Group("/api")
	NewAgentController(nil, nil, nil, nil, nil, false).Register(api)

	have := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		have[r.Method+" "+r.Path] = true
	}
	for _, want := range []string{
		"POST /api/agent/services",
		"PUT /api/agent/services/:name",
		"DELETE /api/agent/services/:name",
		"GET /api/agent/service-overrides",
		"POST /api/agent/service-overrides",
		"DELETE /api/agent/service-overrides/:id",
	} {
		if !have[want] {
			t.Errorf("route %q not registered", want)
		}
	}
}
