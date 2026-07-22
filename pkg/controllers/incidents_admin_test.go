package controllers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

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
		{"GET", "/api/admin/incidents/counts"},
		{"GET", "/api/admin/incidents/intake-settings"},
		{"PUT", "/api/admin/incidents/intake-settings"},
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
	// capabilities now reads config.GetConfig() for the report block, so the
	// global config must exist (sync.Once-guarded; safe if a sibling test
	// already loaded it).
	loadGatewayConfig(t, "test-gateway-secret")

	ctrl := NewIncidentAdminController()
	app := fiber.New()
	app.Get("/cap", ctrl.capabilities)

	cases := []struct {
		name string
		prov storage.Provider
		want bool
	}{
		{"memory backend has no search", storage.NewMemory(), false},
		{"searcher backend has search", searcherStorage{Provider: storage.NewMemory()}, true},
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

// perOriginJSON mirrors one {ai_detect, webhook, total} status bucket.
type perOriginJSON struct {
	AIDetect int `json:"ai_detect"`
	Webhook  int `json:"webhook"`
	Total    int `json:"total"`
}

// byStatusJSON mirrors the additive counts.by_status breakdown: one per-origin
// bucket per status.
type byStatusJSON struct {
	Open     perOriginJSON `json:"open"`
	Acked    perOriginJSON `json:"acked"`
	Resolved perOriginJSON `json:"resolved"`
	All      perOriginJSON `json:"all"`
}

// incidentListResp mirrors the JSON shape returned by the list/search
// endpoints, including the additive origin counts and pagination meta.
type incidentListResp struct {
	Incidents []struct {
		ID     string `json:"id"`
		Origin string `json:"origin"`
	} `json:"incidents"`
	Counts struct {
		AIDetect int          `json:"ai_detect"`
		Webhook  int          `json:"webhook"`
		Total    int          `json:"total"`
		ByStatus byStatusJSON `json:"by_status"`
	} `json:"counts"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
}

// seedOriginStore returns a memory store with a mix of ai_detect, webhook,
// and LEGACY (no Origin field) records so the back-compat derivation is
// exercised end to end. Counts: ai_detect=4, webhook=3, total=7.
func seedOriginStore(t *testing.T) storage.Provider {
	t.Helper()
	mem := storage.NewMemory()
	recs := []*storage.IncidentRecord{
		{ID: "ai-1", Origin: storage.OriginAIDetect, Source: "agent:elasticsearch:app"},
		{ID: "ai-2", Origin: storage.OriginAIDetect, Source: "agent:loki:web"},
		{ID: "ai-3", Origin: storage.OriginAIDetect, Source: "agent"},
		{ID: "wh-1", Origin: storage.OriginWebhook, Source: "webhook"},
		{ID: "wh-2", Origin: storage.OriginWebhook, Source: "sns"},
		{ID: "legacy-agent", Source: "agent:splunk:x"}, // no Origin → derives ai_detect
		{ID: "legacy-webhook", Source: "sqs"},          // no Origin → derives webhook
	}
	for _, r := range recs {
		if err := mem.SaveIncident(r); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}
	return mem
}

func doList(t *testing.T, ctrl *IncidentAdminController, query string) incidentListResp {
	t.Helper()
	app := fiber.New()
	app.Get("/list", ctrl.list)
	resp, err := app.Test(httptest.NewRequest("GET", "/list"+query, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got incidentListResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	return got
}

// TestListPerOriginCounts verifies the list endpoint returns per-origin
// counts computed over the FULL set (including legacy records classified
// from their Source), regardless of any origin filter.
func TestListPerOriginCounts(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })
	services.SetStorage(seedOriginStore(t))
	ctrl := NewIncidentAdminController()

	got := doList(t, ctrl, "")
	if got.Counts.AIDetect != 4 || got.Counts.Webhook != 3 || got.Counts.Total != 7 {
		t.Fatalf("counts = %+v, want ai_detect=4 webhook=3 total=7", got.Counts)
	}
	if len(got.Incidents) != 7 {
		t.Fatalf("unfiltered incidents = %d, want 7", len(got.Incidents))
	}
}

// TestListOriginFilter verifies ?origin= narrows the returned rows while
// the per-origin counts stay whole-set — including legacy records whose
// origin is derived from Source.
func TestListOriginFilter(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })
	services.SetStorage(seedOriginStore(t))
	ctrl := NewIncidentAdminController()

	ai := doList(t, ctrl, "?origin=ai_detect")
	if len(ai.Incidents) != 4 {
		t.Fatalf("ai_detect incidents = %d, want 4", len(ai.Incidents))
	}
	for _, i := range ai.Incidents {
		if i.Origin != storage.OriginAIDetect {
			t.Fatalf("incident %s origin = %q, want ai_detect", i.ID, i.Origin)
		}
	}
	// Counts unchanged by the filter.
	if ai.Counts.AIDetect != 4 || ai.Counts.Webhook != 3 {
		t.Fatalf("filtered counts = %+v, want ai_detect=4 webhook=3", ai.Counts)
	}

	wh := doList(t, ctrl, "?origin=webhook")
	if len(wh.Incidents) != 3 {
		t.Fatalf("webhook incidents = %d, want 3", len(wh.Incidents))
	}
	for _, i := range wh.Incidents {
		if i.Origin != storage.OriginWebhook {
			t.Fatalf("incident %s origin = %q, want webhook", i.ID, i.Origin)
		}
	}
}

// TestListLegacyBackCompat proves a record persisted before the Origin
// field existed (no Origin, Source "agent:...") is both counted and
// filtered as ai_detect, and its summarized origin reflects the
// derivation rather than an empty string.
func TestListLegacyBackCompat(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })
	services.SetStorage(seedOriginStore(t))
	ctrl := NewIncidentAdminController()

	ai := doList(t, ctrl, "?origin=ai_detect")
	var foundLegacy bool
	for _, i := range ai.Incidents {
		if i.ID == "legacy-agent" {
			foundLegacy = true
			if i.Origin != storage.OriginAIDetect {
				t.Fatalf("legacy-agent origin = %q, want ai_detect", i.Origin)
			}
		}
	}
	if !foundLegacy {
		t.Fatal("legacy agent record was not classified into the ai_detect feed")
	}
}

// TestListPagination verifies the 100/page-style server pagination bounds
// the response window and reports total + page meta.
func TestListPagination(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })
	services.SetStorage(seedOriginStore(t))
	ctrl := NewIncidentAdminController()

	p1 := doList(t, ctrl, "?page=1&page_size=2")
	if len(p1.Incidents) != 2 {
		t.Fatalf("page 1 rows = %d, want 2", len(p1.Incidents))
	}
	if p1.Total != 7 || p1.Page != 1 || p1.PageSize != 2 {
		t.Fatalf("page 1 meta = total:%d page:%d size:%d, want 7/1/2", p1.Total, p1.Page, p1.PageSize)
	}

	// Last partial page: 7 rows / 2 per page → page 4 has 1 row.
	p4 := doList(t, ctrl, "?page=4&page_size=2")
	if len(p4.Incidents) != 1 {
		t.Fatalf("page 4 rows = %d, want 1", len(p4.Incidents))
	}

	// Past the end returns an empty window, never an error or a panic.
	p9 := doList(t, ctrl, "?page=9&page_size=2")
	if len(p9.Incidents) != 0 {
		t.Fatalf("page 9 rows = %d, want 0", len(p9.Incidents))
	}

	// Pagination composes with the origin filter: 4 ai_detect / 2 per page.
	ai2 := doList(t, ctrl, "?origin=ai_detect&page=2&page_size=2")
	if len(ai2.Incidents) != 2 || ai2.Total != 4 {
		t.Fatalf("ai_detect page 2 = rows:%d total:%d, want 2/4", len(ai2.Incidents), ai2.Total)
	}
}

// seedStatusStore returns a memory store with a known per-origin × per-status
// spread so the by_status breakdown has a non-trivial truth to match:
//
//	ai_detect: 2 open, 1 acked, 1 resolved (total 4)
//	webhook:   1 open, 1 acked, 2 resolved (total 4, incl. a legacy row)
func seedStatusStore(t *testing.T) storage.Provider {
	t.Helper()
	mem := storage.NewMemory()
	acked := time.Unix(1_700_000_000, 0).UTC()
	recs := []*storage.IncidentRecord{
		{ID: "ai-open-1", Origin: storage.OriginAIDetect, Source: "agent"},
		{ID: "ai-open-2", Origin: storage.OriginAIDetect, Source: "agent"},
		{ID: "ai-acked", Origin: storage.OriginAIDetect, Source: "agent", AckedAt: &acked},
		{ID: "ai-resolved", Origin: storage.OriginAIDetect, Source: "agent", Resolved: true},
		{ID: "wh-open", Origin: storage.OriginWebhook, Source: "webhook"},
		{ID: "wh-acked", Origin: storage.OriginWebhook, Source: "sns", AckedAt: &acked},
		{ID: "wh-resolved", Origin: storage.OriginWebhook, Source: "webhook", Resolved: true},
		{ID: "legacy-resolved", Source: "sqs", Resolved: true}, // derives webhook
	}
	for _, r := range recs {
		if err := mem.SaveIncident(r); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}
	return mem
}

func wantByStatus() byStatusJSON {
	return byStatusJSON{
		Open:     perOriginJSON{AIDetect: 2, Webhook: 1, Total: 3},
		Acked:    perOriginJSON{AIDetect: 1, Webhook: 1, Total: 2},
		Resolved: perOriginJSON{AIDetect: 1, Webhook: 2, Total: 3},
		All:      perOriginJSON{AIDetect: 4, Webhook: 4, Total: 8},
	}
}

// TestCountsEndpointByStatus verifies the dedicated /counts endpoint returns
// the authoritative per-origin × per-status breakdown — the single source of
// truth the header badge and the Now page read instead of tallying a bounded
// page of rows.
func TestCountsEndpointByStatus(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })
	services.SetStorage(seedStatusStore(t))
	ctrl := NewIncidentAdminController()

	app := fiber.New()
	app.Get("/counts", ctrl.counts)
	resp, err := app.Test(httptest.NewRequest("GET", "/counts", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		AIDetect int          `json:"ai_detect"`
		Webhook  int          `json:"webhook"`
		Total    int          `json:"total"`
		ByStatus byStatusJSON `json:"by_status"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}

	if got.ByStatus != wantByStatus() {
		t.Fatalf("by_status = %+v, want %+v", got.ByStatus, wantByStatus())
	}
	// The back-compat top-level counts stay unresolved-only (open + acked).
	if got.AIDetect != 3 || got.Webhook != 2 || got.Total != 5 {
		t.Fatalf("top-level unresolved counts = ai:%d wh:%d total:%d, want 3/2/5",
			got.AIDetect, got.Webhook, got.Total)
	}
}

// TestListCarriesByStatus verifies the list response carries the same
// authoritative by_status breakdown, so the Incidents page reads server counts
// off its existing page fetch (no extra request) — and those counts reflect the
// WHOLE set even when only a bounded page of rows is returned.
func TestListCarriesByStatus(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })
	services.SetStorage(seedStatusStore(t))
	ctrl := NewIncidentAdminController()

	// A tiny page so the returned rows are a strict subset of the set; the
	// counts must still describe all 8 incidents.
	got := doList(t, ctrl, "?page_size=2")
	if len(got.Incidents) != 2 {
		t.Fatalf("page rows = %d, want 2 (bounded)", len(got.Incidents))
	}
	if got.Counts.ByStatus != wantByStatus() {
		t.Fatalf("list by_status = %+v, want %+v", got.Counts.ByStatus, wantByStatus())
	}
	// Reconciliation invariants: statuses sum to origin total, origins sum to
	// the status total — the same guarantee every UI surface now relies on.
	bs := got.Counts.ByStatus
	if bs.Open.AIDetect+bs.Acked.AIDetect+bs.Resolved.AIDetect != bs.All.AIDetect {
		t.Errorf("ai_detect statuses do not sum to its total: %+v", bs)
	}
	if bs.Open.AIDetect+bs.Open.Webhook != bs.Open.Total {
		t.Errorf("open origins do not sum to open total: %+v", bs.Open)
	}
}
