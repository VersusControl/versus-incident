package controllers

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// agent_paged_test.go — the pattern/service list reads now serve one bounded
// page plus a cheap total instead of the whole catalog, mirroring the
// incidents/analyses pager. These tests pin the additive paged envelope
// (total / offset / page_size / next_offset) and prove a full page advertises a
// continuation while an underfull page ends it.

func pagedListApp(t *testing.T, cat *agent.Catalog) *fiber.App {
	t.Helper()
	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	app := fiber.New()
	app.Get("/api/agent/patterns", ctrl.listPatterns)
	app.Get("/api/agent/services", ctrl.listServices)
	return app
}

// TestListPatterns_PagedEnvelope proves a bounded page_size returns only that
// many rows, the true whole-set total, and a next_offset that walks the pages
// without overlap — then nils out on the final underfull page.
func TestListPatterns_PagedEnvelope(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("p1", "alpha", "src", 5, 0.2, "default", "checkout")
	cat.Upsert("p2", "bravo", "src", 4, 0.2, "default", "orders")
	cat.Upsert("p3", "charlie", "src", 3, 0.2, "default", "cache")

	app := pagedListApp(t, cat)

	code, body := getJSON(t, app, "/api/agent/patterns?page_size=2")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
	patterns, _ := body["patterns"].([]any)
	if len(patterns) != 2 {
		t.Fatalf("patterns len = %d, want 2 (bounded page, not whole catalog)", len(patterns))
	}
	if body["total"] != float64(3) {
		t.Fatalf("total = %v, want 3 (whole-set count)", body["total"])
	}
	if body["page_size"] != float64(2) {
		t.Fatalf("page_size = %v, want 2", body["page_size"])
	}
	if body["next_offset"] != float64(2) {
		t.Fatalf("next_offset = %v, want 2 (full page ⇒ continue)", body["next_offset"])
	}

	// The final window is underfull ⇒ next_offset nils out.
	_, tail := getJSON(t, app, "/api/agent/patterns?offset=2&page_size=2")
	tailRows, _ := tail["patterns"].([]any)
	if len(tailRows) != 1 {
		t.Fatalf("tail len = %d, want 1", len(tailRows))
	}
	if tail["total"] != float64(3) {
		t.Fatalf("tail total = %v, want 3 (unchanged)", tail["total"])
	}
	if tail["next_offset"] != nil {
		t.Fatalf("tail next_offset = %v, want nil (underfull ⇒ last page)", tail["next_offset"])
	}
}

// TestListServices_PagedEnvelope proves the services list keeps its back-compat
// name→facts MAP shape while gaining the same bounded paged envelope.
func TestListServices_PagedEnvelope(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	for _, name := range []string{"checkout", "orders", "cache"} {
		cat.RegisterService(name)
	}

	app := pagedListApp(t, cat)

	code, body := getJSON(t, app, "/api/agent/services?page_size=2")
	if code != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
	// Back-compat: services stays a MAP, not an array.
	svcs, ok := body["services"].(map[string]any)
	if !ok {
		t.Fatalf("services is not a map: %T", body["services"])
	}
	if len(svcs) != 2 {
		t.Fatalf("services page len = %d, want 2 (bounded)", len(svcs))
	}
	if body["total"] != float64(3) {
		t.Fatalf("total = %v, want 3", body["total"])
	}
	if body["next_offset"] != float64(2) {
		t.Fatalf("next_offset = %v, want 2", body["next_offset"])
	}
	// Each page entry still carries the grace facts the UI reads.
	for name, raw := range svcs {
		m, _ := raw.(map[string]any)
		if _, has := m["manual"]; !has {
			t.Fatalf("service %q missing manual flag: %v", name, m)
		}
		if _, has := m["in_grace"]; !has {
			t.Fatalf("service %q missing in_grace: %v", name, m)
		}
	}
}
