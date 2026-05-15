package controllers

import (
	"testing"

	"github.com/gofiber/fiber/v2"
)

// TestResolveRouteRegistered guards against the resolve endpoint silently
// disappearing from the route table — the symptom would be a 404 from a
// freshly built server even though the handler exists. We inspect the
// route table directly (no HTTP roundtrip) so the assertion isn't
// coupled to config initialization.
func TestResolveRouteRegistered(t *testing.T) {
	app := fiber.New()
	api := app.Group("/api")
	NewIncidentAdminController().Register(api)

	want := []struct {
		Method string
		Path   string
	}{
		{"GET", "/api/admin/incidents/"},
		{"GET", "/api/admin/incidents/:id"},
		{"POST", "/api/admin/incidents/:id/resolve"},
	}

	have := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		have[r.Method+" "+r.Path] = true
	}

	for _, w := range want {
		key := w.Method + " " + w.Path
		if !have[key] {
			t.Errorf("route %q not registered; have:\n%v", key, have)
		}
	}
}
