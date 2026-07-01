package controllers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// loadServiceDetailConfig loads a minimal global config with a non-zero
// new-service grace so getServiceDetail can compute grace state.
func loadServiceDetailConfig(t *testing.T, grace string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `name: test
host: 0.0.0.0
port: 3000
public_host: http://localhost:3000
alert:
  slack:
    enable: false
oncall:
  enable: false
agent:
  new_service_grace: "` + grace + `"
redis:
  host: localhost
  port: 6379
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}

// newServiceDetailApp wires a catalog (with one in-grace service "api", one
// pattern attributed to it, and one pattern attributed to a different service)
// plus a memory store of incidents, and mounts getServiceDetail directly.
func newServiceDetailApp(t *testing.T) *fiber.App {
	t.Helper()
	loadServiceDetailConfig(t, "30m")

	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.RegisterService("api")
	cat.RegisterService("other")
	// One pattern for "api", one for "other" — only the "api" one must surface.
	cat.Upsert("p-api", "api failed to <*>", "es:prod", 7, 0.2, "default", "api")
	cat.Upsert("p-other", "other oops <*>", "es:prod", 3, 0.2, "default", "other")

	store := storage.NewMemory()
	now := time.Now().UTC()
	recs := []*storage.IncidentRecord{
		{ID: "i1", Service: "api", Title: "api down", CreatedAt: now.Add(-1 * time.Hour), Content: map[string]any{"severity": "critical"}},
		{ID: "i2", Service: "api", Title: "api slow", CreatedAt: now.Add(-2 * time.Hour), Content: map[string]any{"severity": "high"}},
		{ID: "i3", Service: "other", Title: "other down", CreatedAt: now.Add(-1 * time.Hour), Content: map[string]any{"severity": "critical"}},
		{ID: "i4", Service: "api", Title: "ancient", CreatedAt: now.Add(-40 * 24 * time.Hour), Content: map[string]any{"severity": "low"}},
	}
	for _, r := range recs {
		if err := store.SaveIncident(r); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}
	services.SetStorage(store)
	t.Cleanup(func() { services.SetStorage(nil) })

	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Get("/api/agent/services/:name", ctrl.getServiceDetail)
	return app
}

func getJSON(t *testing.T, app *fiber.App, path string) (int, map[string]any) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("unmarshal %q: %v", body, err)
		}
	}
	return resp.StatusCode, out
}

// TestServiceDetailRouteRegistered guards GET /api/agent/services/:name in the
// route table.
func TestServiceDetailRouteRegistered(t *testing.T) {
	app := fiber.New()
	api := app.Group("/api")
	NewAgentController(nil, nil, nil, nil, nil, false).Register(api)

	have := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		have[r.Method+" "+r.Path] = true
	}
	if !have["GET /api/agent/services/:name"] {
		t.Fatalf("route GET /api/agent/services/:name not registered; have:\n%v", have)
	}
}

// TestServiceDetail_OSSShape verifies the aggregate response: service meta +
// grace, service-scoped patterns, and a bounded incident summary — and proves
// the OSS-inert guarantee that NO metrics/traces fields appear.
func TestServiceDetail_OSSShape(t *testing.T) {
	app := newServiceDetailApp(t)

	code, body := getJSON(t, app, "/api/agent/services/api")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}

	if body["service"] != "api" {
		t.Errorf("service = %v, want api", body["service"])
	}
	if body["in_grace"] != true {
		t.Errorf("in_grace = %v, want true (service just registered, grace 30m)", body["in_grace"])
	}
	if rem, _ := body["grace_seconds_remaining"].(float64); rem <= 0 {
		t.Errorf("grace_seconds_remaining = %v, want > 0", body["grace_seconds_remaining"])
	}

	// Patterns: only the "api" pattern, never "other".
	patterns, _ := body["patterns"].([]any)
	if len(patterns) != 1 {
		t.Fatalf("patterns len = %d, want 1 (service-scoped)", len(patterns))
	}
	p0, _ := patterns[0].(map[string]any)
	if p0["id"] != "p-api" {
		t.Errorf("pattern id = %v, want p-api", p0["id"])
	}

	// Incidents: 2 in-window "api" incidents (the 40-day-old one and "other"
	// are excluded), with a severity histogram and a recent list.
	inc, _ := body["incidents"].(map[string]any)
	if c, _ := inc["count"].(float64); c != 2 {
		t.Errorf("incidents.count = %v, want 2", inc["count"])
	}
	if wd, _ := inc["window_days"].(float64); wd != float64(serviceIncidentWindowDays) {
		t.Errorf("incidents.window_days = %v, want %d", inc["window_days"], serviceIncidentWindowDays)
	}
	sev, _ := inc["severities"].(map[string]any)
	if sev["critical"] != float64(1) || sev["high"] != float64(1) {
		t.Errorf("severities = %v, want {critical:1, high:1}", sev)
	}
	recent, _ := inc["recent"].([]any)
	if len(recent) != 2 {
		t.Errorf("incidents.recent len = %d, want 2", len(recent))
	}

	// counts mirror the sections.
	counts, _ := body["counts"].(map[string]any)
	if counts["patterns"] != float64(1) || counts["incidents"] != float64(2) {
		t.Errorf("counts = %v, want {patterns:1, incidents:2}", counts)
	}

	// OSS-inert guarantee: no metrics/traces fields in the OSS response.
	for _, k := range []string{"metrics", "traces", "intel"} {
		if _, ok := body[k]; ok {
			t.Errorf("OSS response must not contain %q", k)
		}
	}
}

// TestServiceDetail_UnknownReturns404 verifies an unknown service is a clean
// 404 with the documented error body.
func TestServiceDetail_UnknownReturns404(t *testing.T) {
	app := newServiceDetailApp(t)

	code, body := getJSON(t, app, "/api/agent/services/nope")
	if code != fiber.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if body["error"] != "service not found" {
		t.Errorf("error = %v, want \"service not found\"", body["error"])
	}
}

// TestServiceDetail_GraceEnded verifies that a service whose grace was ended
// reports in_grace=false with zero remaining.
func TestServiceDetail_GraceEnded(t *testing.T) {
	loadServiceDetailConfig(t, "30m")
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.RegisterService("api")
	cat.EndServiceGrace("api") // FirstSeen → epoch, past grace
	services.SetStorage(storage.NewMemory())
	t.Cleanup(func() { services.SetStorage(nil) })

	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Get("/api/agent/services/:name", ctrl.getServiceDetail)

	code, body := getJSON(t, app, "/api/agent/services/api")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["in_grace"] != false {
		t.Errorf("in_grace = %v, want false (grace ended)", body["in_grace"])
	}
	if rem, _ := body["grace_seconds_remaining"].(float64); rem != 0 {
		t.Errorf("grace_seconds_remaining = %v, want 0", body["grace_seconds_remaining"])
	}
}

// TestServiceDetail_ServesSingleTenantOrgUnderForeignDeploymentOrg pins the org
// the service-detail read resolves: the OSS pattern catalog/service registry is
// single-tenant (always storage.DefaultOrgID), so the endpoint must serve it
// even when an enterprise org resolver stamps a non-default deployment org onto
// the request. This is the QA-025 regression: before the fix the read filtered
// the default-keyed registry by OrgFromContext ("b") and 404'd, gating the page
// out for a licensed deployment whose intel/baselines key under "b".
func TestServiceDetail_ServesSingleTenantOrgUnderForeignDeploymentOrg(t *testing.T) {
	loadServiceDetailConfig(t, "30m")
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.RegisterService("api")
	cat.Upsert("p-api", "api failed to <*>", "es:prod", 7, 0.2, "default", "api")
	services.SetStorage(storage.NewMemory())
	t.Cleanup(func() { services.SetStorage(nil) })

	// Simulate the enterprise deployment org: OrgInjector stamps "b" on every
	// request, mirroring the licensed binary. The OSS single-tenant catalog is
	// still keyed under default, so the read must NOT filter it out.
	middleware.SetOrgResolver(func(*fiber.Ctx) string { return "b" })
	t.Cleanup(func() { middleware.SetOrgResolver(nil) })

	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Use(middleware.OrgInjector())
	app.Get("/api/agent/services/:name", ctrl.getServiceDetail)

	code, body := getJSON(t, app, "/api/agent/services/api")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (must serve default-keyed registry under deployment org b); body=%v", code, body)
	}
	if body["service"] != "api" {
		t.Errorf("service = %v, want api", body["service"])
	}
	if c, _ := body["counts"].(map[string]any); c["patterns"] != float64(1) {
		t.Errorf("counts.patterns = %v, want 1 (default-keyed pattern resolved, not foreign org)", c["patterns"])
	}
}

// TestResetCatalog_WipesPatternsAndServices proves DELETE /api/agent/catalog
// empties every learned pattern AND discovered service, resets the shared
// miner, and returns a count summary of what was cleared.
func TestResetCatalog_WipesPatternsAndServices(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.RegisterService("api")
	cat.RegisterService("web")
	cat.Upsert("p-api", "api failed to <*>", "es:prod", 7, 0.2, "default", "api")
	cat.Upsert("p-web", "web oops <*>", "es:prod", 3, 0.2, "default", "web")

	miner := agent.NewMiner(0.4, 4, 100)
	miner.Cluster("api failed to connect")

	ctrl := NewAgentController(cat, miner, nil, nil, nil, false)
	app := fiber.New()
	app.Delete("/api/agent/catalog", ctrl.resetCatalog)

	resp, err := app.Test(httptest.NewRequest("DELETE", "/api/agent/catalog", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	if body["patterns"] != float64(2) {
		t.Errorf("patterns = %v, want 2", body["patterns"])
	}
	if body["services"] != float64(2) {
		t.Errorf("services = %v, want 2", body["services"])
	}
	if cat.Len() != 0 {
		t.Errorf("catalog not emptied: %d patterns remain", cat.Len())
	}
	if n := len(cat.AllServices()); n != 0 {
		t.Errorf("services not emptied: %d remain", n)
	}
	if n := len(miner.Snapshot()); n != 0 {
		t.Errorf("miner not reset: %d clusters remain", n)
	}
}

// TestResetCatalogRouteRegistered guards DELETE /api/agent/catalog in the
// route table and proves the removed POST /api/agent/flush endpoint is gone.
func TestResetCatalogRouteRegistered(t *testing.T) {
	app := fiber.New()
	api := app.Group("/api")
	NewAgentController(nil, nil, nil, nil, nil, false).Register(api)

	have := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		have[r.Method+" "+r.Path] = true
	}
	if !have["DELETE /api/agent/catalog"] {
		t.Fatalf("route DELETE /api/agent/catalog not registered; have:\n%v", have)
	}
	// The manual flush endpoint was removed — the catalog auto-persists.
	if have["POST /api/agent/flush"] {
		t.Errorf("route POST /api/agent/flush should be removed but is still registered")
	}
}
