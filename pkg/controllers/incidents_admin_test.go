package controllers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

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
		{"GET", "/api/admin/incidents/search"},
		{"GET", "/api/admin/incidents/:id"},
		{"POST", "/api/admin/incidents/:id/resolve"},
		{"POST", "/api/admin/incidents/:id/analyze"},
		{"GET", "/api/admin/incidents/:id/analyses"},
		{"GET", "/api/admin/capabilities/"},
		{"GET", "/api/admin/analyses/"},
		{"GET", "/api/admin/analyses/:analysis_id"},
		{"DELETE", "/api/admin/analyses/:analysis_id"},
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

// searcherStorage is a storage.Provider that also implements the optional
// storage.Searcher capability. The embedded interface satisfies Provider;
// only the two search methods are exercised.
type searcherStorage struct {
	storage.Provider
	incidents []*storage.IncidentRecord
}

func (s searcherStorage) SearchIncidents(query string, limit int) ([]*storage.IncidentRecord, error) {
	return s.incidents, nil
}

func (s searcherStorage) SearchAnalyses(string, int) ([]*storage.AnalysisRecord, error) {
	return nil, nil
}

// TestCapabilitiesReflectsSearcher verifies the capabilities probe reports
// search support exactly when the active storage backend implements
// storage.Searcher — true for a searcher-capable backend, false for the
// memory backend (the same path file storage takes).
func TestCapabilitiesReflectsSearcher(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })

	ctrl := NewIncidentAdminController()
	app := fiber.New()
	app.Get("/cap", ctrl.capabilities)

	cases := []struct {
		name string
		prov storage.Provider
		want bool
	}{
		{"memory backend has no search", storage.NewMemory(), false},
		{"searcher backend has search", searcherStorage{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			services.SetStorage(tc.prov)
			resp, err := app.Test(httptest.NewRequest("GET", "/cap", nil))
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			var got struct {
				Search bool `json:"search"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal %q: %v", body, err)
			}
			if got.Search != tc.want {
				t.Fatalf("search = %v, want %v", got.Search, tc.want)
			}
		})
	}
}

// TestSearchUnsupportedReturns501 verifies the search endpoint degrades to
// 501 Not Implemented when the backend is not a storage.Searcher, so the
// UI knows to fall back to client-side filtering.
func TestSearchUnsupportedReturns501(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })

	ctrl := NewIncidentAdminController()
	app := fiber.New()
	app.Get("/search", ctrl.search)

	services.SetStorage(storage.NewMemory())
	resp, err := app.Test(httptest.NewRequest("GET", "/search?q=db", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

// TestSearchSupportedReturnsResults verifies the search endpoint forwards
// to the backend's SearchIncidents and returns summarized incidents.
func TestSearchSupportedReturnsResults(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })

	ctrl := NewIncidentAdminController()
	app := fiber.New()
	app.Get("/search", ctrl.search)

	services.SetStorage(searcherStorage{incidents: []*storage.IncidentRecord{
		{ID: "inc-1", Title: "Database pool exhausted", Service: "payments"},
		{ID: "inc-2", Title: "Database latency", Service: "checkout"},
	}})

	resp, err := app.Test(httptest.NewRequest("GET", "/search?q=database", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		Incidents []struct {
			ID string `json:"id"`
		} `json:"incidents"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if len(got.Incidents) != 2 || got.Incidents[0].ID != "inc-1" {
		t.Fatalf("incidents = %+v, want 2 (inc-1 first)", got.Incidents)
	}
}
