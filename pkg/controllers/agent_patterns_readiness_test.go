package controllers

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// patternsApp mounts listPatterns directly (no auth middleware) over a catalog
// with two patterns: one whose count has crossed AutoPromoteAfter (Ready by the
// count gate) and one still learning. SetCatalogConfig wires the threshold +
// poll interval the readiness computation needs.
func patternsApp(t *testing.T, cat *agent.Catalog, cfg config.AgentCatalogConfig, poll time.Duration) *fiber.App {
	t.Helper()
	ctrl := NewAgentController(cat, nil, nil, nil, nil, false).SetCatalogConfig(cfg, poll)
	app := fiber.New()
	app.Get("/api/agent/patterns", ctrl.listPatterns)
	app.Get("/api/agent/patterns/:id", ctrl.getPattern)
	return app
}

// TestListPatterns_CarriesReadiness proves each /api/agent/patterns row carries
// an additive `readiness` object computed at the read boundary, with the exact
// shape the UI consumes. Logs need no license — the field is always
// present.
func TestListPatterns_CarriesReadiness(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	// Count-promoted (150 >= 100 threshold) → Ready by the gate even though the
	// on-disk verdict is still "" (Classify never ran in this read-only test).
	cat.Upsert("p-known", "known thing <*>", "es:prod", 150, 0.2, "default", "api")
	// Still learning: 40 sightings < 100.
	cat.Upsert("p-learning", "new thing <*>", "es:prod", 40, 0.2, "default", "api")

	app := patternsApp(t, cat, config.AgentCatalogConfig{AutoPromoteAfter: 100}, 30*time.Second)

	code, body := getJSON(t, app, "/api/agent/patterns")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}

	patterns, _ := body["patterns"].([]any)
	if len(patterns) != 2 {
		t.Fatalf("patterns len = %d, want 2", len(patterns))
	}

	byID := map[string]map[string]any{}
	for _, raw := range patterns {
		p, _ := raw.(map[string]any)
		id, _ := p["id"].(string)
		byID[id] = p
	}

	// Known-by-count row.
	known := byID["p-known"]
	if known == nil {
		t.Fatal("row p-known missing")
	}
	// Embedded Pattern fields are still promoted to the top level.
	if known["count"] != float64(150) {
		t.Errorf("p-known count = %v, want 150 (embedded Pattern field promoted)", known["count"])
	}
	kr, _ := known["readiness"].(map[string]any)
	if kr == nil {
		t.Fatal("p-known missing readiness object")
	}
	if kr["ready"] != true {
		t.Errorf("p-known readiness.ready = %v, want true (count 150 >= 100)", kr["ready"])
	}
	if kr["seen"] != float64(150) {
		t.Errorf("p-known readiness.seen = %v, want 150", kr["seen"])
	}
	if kr["needed"] != float64(100) {
		t.Errorf("p-known readiness.needed = %v, want 100", kr["needed"])
	}
	// Ready ⇒ no ETA countdown.
	if kr["rate_per_min"] != float64(0) {
		t.Errorf("p-known readiness.rate_per_min = %v, want 0 (Ready ⇒ no ETA)", kr["rate_per_min"])
	}

	// Learning row.
	learning := byID["p-learning"]
	if learning == nil {
		t.Fatal("row p-learning missing")
	}
	lr, _ := learning["readiness"].(map[string]any)
	if lr == nil {
		t.Fatal("p-learning missing readiness object")
	}
	if lr["ready"] != false {
		t.Errorf("p-learning readiness.ready = %v, want false (40 < 100)", lr["ready"])
	}
	if lr["seen"] != float64(40) {
		t.Errorf("p-learning readiness.seen = %v, want 40", lr["seen"])
	}
	if lr["needed"] != float64(100) {
		t.Errorf("p-learning readiness.needed = %v, want 100", lr["needed"])
	}
	// BaselineFrequency 40/tick ÷ 0.5 min/tick = 80/min.
	if lr["rate_per_min"] != float64(80) {
		t.Errorf("p-learning readiness.rate_per_min = %v, want 80", lr["rate_per_min"])
	}
}

// TestListPatterns_UnsetCatalogConfigDegradesSafely proves that when no
// worker/config is wired (SetCatalogConfig never called), readiness is still
// present and degrades to Needed=0/RatePerMin=0 (renders as "Learning", no ETA)
// rather than being absent or panicking.
func TestListPatterns_UnsetCatalogConfigDegradesSafely(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("p1", "thing <*>", "es:prod", 150, 0.2, "default", "api")

	// No SetCatalogConfig call.
	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Get("/api/agent/patterns", ctrl.listPatterns)

	code, body := getJSON(t, app, "/api/agent/patterns")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	patterns, _ := body["patterns"].([]any)
	if len(patterns) != 1 {
		t.Fatalf("patterns len = %d, want 1", len(patterns))
	}
	p0, _ := patterns[0].(map[string]any)
	r, _ := p0["readiness"].(map[string]any)
	if r == nil {
		t.Fatal("readiness object missing even with unset config")
	}
	if r["needed"] != float64(0) {
		t.Errorf("readiness.needed = %v, want 0 (unset threshold → indeterminate)", r["needed"])
	}
	if r["rate_per_min"] != float64(0) {
		t.Errorf("readiness.rate_per_min = %v, want 0 (unset poll → no ETA)", r["rate_per_min"])
	}
	// Ready is false: threshold unset (0) disables count promotion and verdict is "".
	if r["ready"] != false {
		t.Errorf("readiness.ready = %v, want false (no count gate, verdict empty)", r["ready"])
	}
}

// TestGetPattern_DoesNotPersistReadiness proves the on-disk Pattern schema is
// unchanged: the single-pattern endpoint (which serves the raw *Pattern) must
// NOT carry a readiness field — readiness is computed only at the list boundary,
// never persisted.
func TestGetPattern_DoesNotPersistReadiness(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("p1", "thing <*>", "es:prod", 150, 0.2, "default", "api")

	app := patternsApp(t, cat, config.AgentCatalogConfig{AutoPromoteAfter: 100}, 30*time.Second)

	code, body := getJSON(t, app, "/api/agent/patterns/p1")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, ok := body["readiness"]; ok {
		t.Errorf("GET /patterns/:id must not carry readiness (raw on-disk Pattern), got %v", body["readiness"])
	}
	if body["id"] != "p1" {
		t.Errorf("id = %v, want p1", body["id"])
	}
}
