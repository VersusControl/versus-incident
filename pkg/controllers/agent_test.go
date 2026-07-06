package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
// the request. This is the regression guard: before the fix the read filtered
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

// orgScopedCatalogStore is a minimal CatalogStore whose unified read view keys
// its service + patterns under a caller-chosen org, mirroring an enterprise
// partition store that keys a deployment's catalog under a non-default org.
// Load/Persist/Curate are no-ops; only Snapshot feeds the admin list + detail
// reads under test.
type orgScopedCatalogStore struct {
	patterns []*agent.Pattern
	services map[string]agent.ServiceInfo
}

func (s *orgScopedCatalogStore) Load() (map[string]*agent.Pattern, map[string]*agent.ServiceInfo, error) {
	return nil, nil, nil
}

func (s *orgScopedCatalogStore) Persist(map[string]*agent.Pattern, map[string]*agent.ServiceInfo) error {
	return nil
}

func (s *orgScopedCatalogStore) Snapshot() ([]*agent.Pattern, map[string]agent.ServiceInfo, error) {
	return s.patterns, s.services, nil
}

func (s *orgScopedCatalogStore) Curate(agent.CatalogEdit) error { return nil }

// TestServiceDetail_ResolvesServiceStoredUnderNonDefaultOrg proves the
// list/detail invariant: a service whose catalog entry is keyed under a
// NON-default org (an enterprise deployment org) both appears in listServices
// AND resolves via getServiceDetail. Before the fix the list showed it (no org
// filter) while the detail 404'd it (a hardcoded default-org filter), so a
// service in the list failed to open.
func TestServiceDetail_ResolvesServiceStoredUnderNonDefaultOrg(t *testing.T) {
	loadServiceDetailConfig(t, "30m")

	const deploymentOrg = "b"
	store := &orgScopedCatalogStore{
		patterns: []*agent.Pattern{
			{ID: "p-api", OrgID: deploymentOrg, Template: "api failed to <*>", Service: "api", Count: 7, Source: "es:prod"},
		},
		services: map[string]agent.ServiceInfo{
			"api": {OrgID: deploymentOrg, FirstSeen: time.Now().UTC()},
		},
	}
	agent.SetCatalogStore(store)
	t.Cleanup(func() { agent.SetCatalogStore(nil) })

	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	services.SetStorage(storage.NewMemory())
	t.Cleanup(func() { services.SetStorage(nil) })

	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Get("/api/agent/services", ctrl.listServices)
	app.Get("/api/agent/services/:name", ctrl.getServiceDetail)

	// The list surfaces the non-default-org service...
	code, listBody := getJSON(t, app, "/api/agent/services")
	if code != fiber.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%v", code, listBody)
	}
	svcs, _ := listBody["services"].(map[string]any)
	if _, ok := svcs["api"]; !ok {
		t.Fatalf("listServices must surface the non-default-org service; got %v", svcs)
	}

	// ...and the detail resolves it (was a 404 before the fix).
	code, body := getJSON(t, app, "/api/agent/services/api")
	if code != fiber.StatusOK {
		t.Fatalf("detail status = %d, want 200 (a service in the list must resolve in the detail); body=%v", code, body)
	}
	if body["service"] != "api" {
		t.Errorf("service = %v, want api", body["service"])
	}
	if c, _ := body["counts"].(map[string]any); c["patterns"] != float64(1) {
		t.Errorf("counts.patterns = %v, want 1 (non-default-org pattern resolved)", c["patterns"])
	}
}

// TestServiceDetail_PatternRowsCarryRedactedSamplesAndBaselines proves the
// service-detail pattern rows now carry the same redacted sample ring + learned
// baseline numbers the pattern-detail read returns, and that redaction holds: a
// secret planted in a recorded raw line must never reach the response.
func TestServiceDetail_PatternRowsCarryRedactedSamplesAndBaselines(t *testing.T) {
	loadServiceDetailConfig(t, "30m")

	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	red, errs := agent.NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatalf("NewRedactor: %v", errs)
	}
	// Fold a per-second rate into hour-of-day buckets so the pattern carries a
	// real average, variance, and seasonal baseline (not just the legacy
	// frequency), matching what the log-patterns page shows.
	cat.SetBaselineFold(agent.BaselineFold{PollSeconds: 1, SeasonalPeriod: 24})
	cat.RegisterService("api")
	cat.Upsert("p-api", "api failed to <*>", "es:prod", 7, 0.2, "default", "api") // seed
	cat.Upsert("p-api", "api failed to <*>", "es:prod", 3, 0.2, "default", "api") // fold → variance
	// A secret in the recorded line must be re-scrubbed at the storage boundary
	// so it never reaches the detail response.
	cat.RecordSample("p-api", "api failed to auth password=hunter2 for order", red)
	cat.RecordSample("p-api", "api failed to connect to db", red)

	services.SetStorage(storage.NewMemory())
	t.Cleanup(func() { services.SetStorage(nil) })

	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Get("/api/agent/services/:name", ctrl.getServiceDetail)

	code, body := getJSON(t, app, "/api/agent/services/api")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}

	patterns, _ := body["patterns"].([]any)
	if len(patterns) != 1 {
		t.Fatalf("patterns len = %d, want 1", len(patterns))
	}
	row, _ := patterns[0].(map[string]any)

	// Samples: present, redacted, and the same oldest→newest ring the
	// pattern-detail read returns.
	samples, ok := row["samples"].([]any)
	if !ok || len(samples) != 2 {
		t.Fatalf("samples = %v, want a 2-entry redacted ring", row["samples"])
	}
	for _, s := range samples {
		if line, _ := s.(string); strings.Contains(line, "hunter2") {
			t.Fatalf("secret leaked into a service-detail sample: %q", line)
		}
	}
	if latest, _ := samples[len(samples)-1].(string); latest != "api failed to connect to db" {
		t.Errorf("latest sample = %q, want the most-recently recorded line", latest)
	}

	// Baseline numbers: every field the log-patterns page shows must be present
	// under the same JSON names as the pattern model.
	for _, k := range []string{"baseline_frequency", "baseline_avg", "baseline_variance", "seasonal"} {
		if _, present := row[k]; !present {
			t.Errorf("service-detail pattern row missing %q", k)
		}
	}
	if f, _ := row["baseline_frequency"].(float64); f <= 0 {
		t.Errorf("baseline_frequency = %v, want > 0 (folded during learn)", row["baseline_frequency"])
	}
	if avg, _ := row["baseline_avg"].(float64); avg <= 0 {
		t.Errorf("baseline_avg = %v, want > 0 (folded during learn)", row["baseline_avg"])
	}
	seasonal, ok := row["seasonal"].([]any)
	if !ok || len(seasonal) != 24 {
		t.Fatalf("seasonal = %v, want 24 hour-of-day buckets", row["seasonal"])
	}
}

// TestClearPatterns_WipesPatternsKeepsServices proves DELETE /api/agent/patterns
// empties every learned pattern, resets the shared miner, LEAVES discovered
// services intact, and returns a count of the patterns cleared.
func TestClearPatterns_WipesPatternsKeepsServices(t *testing.T) {
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
	app.Delete("/api/agent/patterns", ctrl.clearPatterns)

	resp, err := app.Test(httptest.NewRequest("DELETE", "/api/agent/patterns", nil))
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
	if _, ok := body["services"]; ok {
		t.Errorf("clear-patterns response carries a services count %v, want none", body["services"])
	}
	if cat.Len() != 0 {
		t.Errorf("patterns not emptied: %d remain", cat.Len())
	}
	if n := len(cat.AllServices()); n != 2 {
		t.Errorf("services touched by clear-patterns: %d, want 2 (services must survive)", n)
	}
	if n := len(miner.Snapshot()); n != 0 {
		t.Errorf("miner not reset: %d clusters remain", n)
	}
}

// TestClearPatterns_RewindsWiredCursors proves DELETE /api/agent/patterns
// rewinds the wired poll-cursor store so the SAME running worker re-reads its
// lookback window and relearns — the fix for the founder's "cleared patterns
// never come back until I recreate the container" halt. It asserts the exact
// shared CursorStore the worker mines through is emptied by the clear.
func TestClearPatterns_RewindsWiredCursors(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("p-api", "api failed to <*>", "es:prod", 7, 0.2, "default", "api")

	miner := agent.NewMiner(0.4, 4, 100)
	cursors := agent.NewCursorStore(nil) // in-memory
	ctx := context.Background()
	if err := cursors.Set(ctx, "es:prod", time.Now().UTC()); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	if _, ok := cursors.Get(ctx, "es:prod"); !ok {
		t.Fatal("cursor should be seeded before the clear")
	}

	ctrl := NewAgentController(cat, miner, nil, nil, nil, false).SetCursorStore(cursors)
	app := fiber.New()
	app.Delete("/api/agent/patterns", ctrl.clearPatterns)

	resp, err := app.Test(httptest.NewRequest("DELETE", "/api/agent/patterns", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if _, ok := cursors.Get(ctx, "es:prod"); ok {
		t.Error("cursor still present after clear-patterns; the worker will not re-read the window and learning stays halted")
	}
}

// TestClearServices_WipesServicesKeepsPatterns proves DELETE /api/agent/services
// empties every discovered service, LEAVES learned patterns AND the miner
// intact, and returns a count of the services cleared.
func TestClearServices_WipesServicesKeepsPatterns(t *testing.T) {
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
	app.Delete("/api/agent/services", ctrl.clearServices)

	resp, err := app.Test(httptest.NewRequest("DELETE", "/api/agent/services", nil))
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
	if body["services"] != float64(2) {
		t.Errorf("services = %v, want 2", body["services"])
	}
	if _, ok := body["patterns"]; ok {
		t.Errorf("clear-services response carries a patterns count %v, want none", body["patterns"])
	}
	if n := len(cat.AllServices()); n != 0 {
		t.Errorf("services not emptied: %d remain", n)
	}
	if cat.Len() != 2 {
		t.Errorf("patterns touched by clear-services: %d, want 2 (patterns must survive)", cat.Len())
	}
	if n := len(miner.Snapshot()); n != 1 {
		t.Errorf("miner touched by clear-services: %d clusters, want 1 (miner must survive)", n)
	}
}

// TestClearRoutesRegistered guards the two scoped clear routes in the route
// table and proves the removed combined DELETE /api/agent/catalog is gone.
func TestClearRoutesRegistered(t *testing.T) {
	app := fiber.New()
	api := app.Group("/api")
	NewAgentController(nil, nil, nil, nil, nil, false).Register(api)

	have := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		have[r.Method+" "+r.Path] = true
	}
	if !have["DELETE /api/agent/patterns"] {
		t.Fatalf("route DELETE /api/agent/patterns not registered; have:\n%v", have)
	}
	if !have["DELETE /api/agent/services"] {
		t.Fatalf("route DELETE /api/agent/services not registered; have:\n%v", have)
	}
	// The combined catalog reset was split into the two scoped routes above.
	if have["DELETE /api/agent/catalog"] {
		t.Errorf("route DELETE /api/agent/catalog should be removed but is still registered")
	}
	// The per-id delete routes are DISTINCT from the collection-level clears.
	if !have["DELETE /api/agent/patterns/:id"] {
		t.Errorf("route DELETE /api/agent/patterns/:id must still be registered")
	}
	if !have["DELETE /api/agent/services/:name"] {
		t.Errorf("route DELETE /api/agent/services/:name must still be registered")
	}
}

// loadGatewayConfig loads a minimal global config carrying a gateway secret so
// the full agent admin router (authMiddleware) admits requests presenting it.
func loadGatewayConfig(t *testing.T, secret string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `name: test
host: 0.0.0.0
port: 3000
public_host: http://localhost:3000
gateway_secret: "` + secret + `"
alert:
  slack:
    enable: false
oncall:
  enable: false
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

// TestScopedDeleteRoutes_ResolveToCorrectHandler proves Fiber's path-shape
// routing keeps the collection-level clears (DELETE /patterns, /services)
// distinct from the item-level deletes (DELETE /patterns/:id, /services/:name):
// each request resolves to its OWN handler, never colliding. Driven through the
// full Register() router (authMiddleware included) so it exercises the real
// route table, not a hand-mounted subset.
func TestScopedDeleteRoutes_ResolveToCorrectHandler(t *testing.T) {
	agent.SetCatalogStore(nil)
	const secret = "test-gateway-secret"
	// config.LoadConfig is sync.Once-guarded, so a sibling test may have already
	// consumed the one-shot load with a secret-less config. Ensure the global
	// config exists, then pin the gateway secret directly (restored after) so
	// authMiddleware admits our requests regardless of suite order.
	loadGatewayConfig(t, secret)
	prevSecret := config.GetConfig().GatewaySecret
	config.GetConfig().GatewaySecret = secret
	t.Cleanup(func() { config.GetConfig().GatewaySecret = prevSecret })

	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("p-1", "one <*>", "es", 3, 0.2, "", "api")
	cat.Upsert("p-2", "two <*>", "es", 5, 0.2, "", "web")
	cat.RegisterService("api")
	if err := cat.CreateService("manual-svc"); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	miner := agent.NewMiner(0.4, 4, 100)
	miner.Cluster("one 1")

	app := fiber.New()
	api := app.Group("/api")
	NewAgentController(cat, miner, nil, nil, nil, false).Register(api)

	del := func(path string) (int, map[string]any) {
		req := httptest.NewRequest("DELETE", path, nil)
		req.Header.Set("X-Gateway-Secret", secret)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if len(body) > 0 {
			_ = json.Unmarshal(body, &out)
		}
		return resp.StatusCode, out
	}

	// Item-DELETE → deletePattern: removes exactly ONE pattern (204), p-2 stays.
	if code, _ := del("/api/agent/patterns/p-1"); code != fiber.StatusNoContent {
		t.Fatalf("DELETE /patterns/p-1 code=%d, want 204 (item handler)", code)
	}
	if cat.Len() != 1 || cat.Get("p-2") == nil {
		t.Fatalf("item-DELETE did not resolve to deletePattern: Len=%d (want 1, p-2 present)", cat.Len())
	}

	// Collection-DELETE → clearPatterns: wipes ALL (200, {ok, patterns}).
	code, body := del("/api/agent/patterns")
	if code != fiber.StatusOK {
		t.Fatalf("DELETE /patterns code=%d, want 200 (collection handler)", code)
	}
	if body["ok"] != true || body["patterns"] != float64(1) {
		t.Fatalf("collection-DELETE body = %v, want {ok:true, patterns:1}", body)
	}
	if cat.Len() != 0 {
		t.Fatalf("collection-DELETE left %d patterns, want 0", cat.Len())
	}

	// Item-DELETE → deleteService: removes the manual service (204), api stays.
	if code, _ := del("/api/agent/services/manual-svc"); code != fiber.StatusNoContent {
		t.Fatalf("DELETE /services/manual-svc code=%d, want 204 (item handler)", code)
	}
	if _, ok := cat.AllServices()["manual-svc"]; ok {
		t.Fatalf("item-DELETE did not remove the manual service (wrong handler)")
	}
	if _, ok := cat.AllServices()["api"]; !ok {
		t.Fatalf("item-DELETE wrongly wiped the auto service (collection handler ran)")
	}

	// Collection-DELETE → clearServices: wipes ALL services (200, {ok, services}).
	code, body = del("/api/agent/services")
	if code != fiber.StatusOK {
		t.Fatalf("DELETE /services code=%d, want 200 (collection handler)", code)
	}
	if body["ok"] != true {
		t.Fatalf("collection-DELETE services body = %v, want ok:true", body)
	}
	if n := len(cat.AllServices()); n != 0 {
		t.Fatalf("collection-DELETE left %d services, want 0", n)
	}
}

// TestListServices_AlwaysSerializesManualFlag proves the services API returns
// the "manual" origin for EVERY row — an auto-discovered row returns
// "manual":false EXPLICITLY (not dropped by omitempty) so the UI can always
// render an Auto-vs-Manual origin column.
//
// This test intentionally does NOT load a global config: listServices' grace
// computation (serviceGraceDuration) guards config access through
// GetConfigOrNil and degrades to grace-disabled when config is unloaded, so the
// handler must not panic here. Keeping the test config-free makes it
// order-independent — it passes standalone (`-run TestListServices…`) as well
// as in a full-package run where a sibling has already loaded config.
func TestListServices_AlwaysSerializesManualFlag(t *testing.T) {
	agent.SetCatalogStore(nil)
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.RegisterService("auto-svc") // auto-discovered → manual:false
	if err := cat.CreateService("manual-svc"); err != nil {
		t.Fatalf("CreateService: %v", err)
	}

	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Get("/api/agent/services", ctrl.listServices)

	resp, err := app.Test(httptest.NewRequest("GET", "/api/agent/services", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// Assert on the RAW bytes so an omitempty regression (which drops the key
	// for false) is caught, not silently defaulted back to false on decode.
	if !strings.Contains(string(raw), `"manual":false`) {
		t.Fatalf("auto service dropped explicit manual:false; body=%s", raw)
	}

	var body struct {
		Services map[string]struct {
			Manual bool `json:"manual"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if body.Services["auto-svc"].Manual {
		t.Errorf("auto-svc manual=true, want false")
	}
	if !body.Services["manual-svc"].Manual {
		t.Errorf("manual-svc manual=false, want true")
	}
}

// TestListServices_ReportsGraceStatus proves the services LIST now carries the
// per-service grace status (in_grace + grace_seconds_remaining) that only the
// service DETAIL endpoint used to return — the fix for the founder's bug where
// the list showed every service as "tracked" even while it was still in grace.
func TestListServices_ReportsGraceStatus(t *testing.T) {
	agent.SetCatalogStore(nil)
	loadServiceDetailConfig(t, "30m")

	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.RegisterService("fresh")   // just seen → still inside its 30m grace
	cat.RegisterService("expired") // seen, then forced out of grace
	if !cat.EndServiceGrace("expired") {
		t.Fatal("EndServiceGrace(expired) = false, want true")
	}

	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Get("/api/agent/services", ctrl.listServices)

	code, body := getJSON(t, app, "/api/agent/services")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
	svcs, _ := body["services"].(map[string]any)

	fresh, _ := svcs["fresh"].(map[string]any)
	if fresh["in_grace"] != true {
		t.Errorf("fresh in_grace = %v, want true (just registered, 30m grace)", fresh["in_grace"])
	}
	if rem, _ := fresh["grace_seconds_remaining"].(float64); rem <= 0 {
		t.Errorf("fresh grace_seconds_remaining = %v, want > 0", rem)
	}

	expired, _ := svcs["expired"].(map[string]any)
	if expired["in_grace"] != false {
		t.Errorf("expired in_grace = %v, want false (grace ended)", expired["in_grace"])
	}
	if rem, _ := expired["grace_seconds_remaining"].(float64); rem != 0 {
		t.Errorf("expired grace_seconds_remaining = %v, want 0", rem)
	}
}

// TestListServices_GraceStatusAgreesWithDetail is the direct guard for the
// list-vs-detail status inconsistency: both endpoints now derive grace from the
// SAME graceStatus helper, so the "in grace" status they report for the same
// service must agree.
func TestListServices_GraceStatusAgreesWithDetail(t *testing.T) {
	agent.SetCatalogStore(nil)
	loadServiceDetailConfig(t, "30m")

	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.RegisterService("api")

	store := storage.NewMemory()
	services.SetStorage(store)
	t.Cleanup(func() { services.SetStorage(nil) })

	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Get("/api/agent/services", ctrl.listServices)
	app.Get("/api/agent/services/:name", ctrl.getServiceDetail)

	_, list := getJSON(t, app, "/api/agent/services")
	svcs, _ := list["services"].(map[string]any)
	api, _ := svcs["api"].(map[string]any)

	_, detail := getJSON(t, app, "/api/agent/services/api")

	if api["in_grace"] != detail["in_grace"] {
		t.Errorf("in_grace disagreement: list=%v detail=%v", api["in_grace"], detail["in_grace"])
	}
	lr, _ := api["grace_seconds_remaining"].(float64)
	dr, _ := detail["grace_seconds_remaining"].(float64)
	if diff := lr - dr; diff < -2 || diff > 2 {
		t.Errorf("grace_seconds_remaining differ by >2s: list=%v detail=%v", lr, dr)
	}
}
