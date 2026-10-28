package controllers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

const countSecret = "test-gateway-secret"

// TestCountSettingsRoutesRegistered guards the /admin/agent/count-settings
// endpoints against silently dropping off the route table.
func TestCountSettingsRoutesRegistered(t *testing.T) {
	app := fiber.New()
	api := app.Group("/api")
	NewCountSettingsController().Register(api)

	have := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		have[r.Method+" "+r.Path] = true
	}
	for _, key := range []string{
		"GET /api/admin/agent/count-settings",
		"PUT /api/admin/agent/count-settings",
	} {
		if !have[key] {
			t.Errorf("route %q not registered; have:\n%v", key, have)
		}
	}
}

// countApp mounts the controller over a memory store with the gateway secret
// configured.
func countApp(t *testing.T) *fiber.App {
	t.Helper()
	loadGatewayConfig(t, countSecret)
	config.GetConfig().GatewaySecret = countSecret
	services.SetStorage(storage.NewMemory())
	t.Cleanup(func() { services.SetStorage(nil) })

	app := fiber.New()
	api := app.Group("/api")
	NewCountSettingsController().Register(api)
	return app
}

func countGet(t *testing.T, app *fiber.App) agent.CountSettings {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/admin/agent/count-settings", nil)
	req.Header.Set("X-Gateway-Secret", countSecret)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var got agent.CountSettings
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	return got
}

func countPut(t *testing.T, app *fiber.App, body string) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/admin/agent/count-settings", strings.NewReader(body))
	req.Header.Set("X-Gateway-Secret", countSecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("PUT %s: %v", body, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("PUT %s status = %d, want 200", body, resp.StatusCode)
	}
}

// TestCountSettings_GetPutRoundTrip drives the endpoints: GET returns the
// built-in default on a fresh store, PUT persists a valid window, and the PUT
// response itself echoes the effective value so the UI need not re-fetch.
func TestCountSettings_GetPutRoundTrip(t *testing.T) {
	app := countApp(t)

	if got := countGet(t, app); got.Window != agent.CountWindow7d {
		t.Fatalf("default = %+v, want window=7d", got)
	}

	req := httptest.NewRequest("PUT", "/api/admin/agent/count-settings", strings.NewReader(`{"window":"30d"}`))
	req.Header.Set("X-Gateway-Secret", countSecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	var echoed agent.CountSettings
	_ = json.NewDecoder(resp.Body).Decode(&echoed)
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	if echoed.Window != agent.CountWindow30d {
		t.Fatalf("PUT echoed %+v, want window=30d", echoed)
	}
	if got := countGet(t, app); got.Window != agent.CountWindow30d {
		t.Fatalf("after PUT = %+v, want window=30d", got)
	}
}

// TestCountSettings_AcceptsEveryKnownWindow proves the API and the store agree
// on the option list the UI offers — including "all", which must survive as
// "all" rather than being folded into the default.
func TestCountSettings_AcceptsEveryKnownWindow(t *testing.T) {
	app := countApp(t)
	for _, w := range []string{
		agent.CountWindow24h,
		agent.CountWindow7d,
		agent.CountWindow30d,
		agent.CountWindow90d,
		agent.CountWindowAll,
	} {
		countPut(t, app, `{"window":"`+w+`"}`)
		if got := countGet(t, app); got.Window != w {
			t.Fatalf("window %q round-tripped as %+v", w, got)
		}
	}
}

// TestCountSettings_PutRejectsUnknownWindow proves an unrecognized window is
// rejected with a 400 and never persisted, so an operator is never told a
// setting took effect when it was quietly folded to the default.
func TestCountSettings_PutRejectsUnknownWindow(t *testing.T) {
	app := countApp(t)
	countPut(t, app, `{"window":"90d"}`)

	for _, bad := range []string{`{"window":"6mo"}`, `{"window":""}`, `{"window":"7D"}`} {
		req := httptest.NewRequest("PUT", "/api/admin/agent/count-settings", strings.NewReader(bad))
		req.Header.Set("X-Gateway-Secret", countSecret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("PUT %s: %v", bad, err)
		}
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("PUT %s status = %d, want 400", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// The rejected writes must not have disturbed the stored value.
	if got := agent.LoadCountSettings(services.Storage()); got.Window != agent.CountWindow90d {
		t.Fatalf("stored after rejected PUTs = %+v, want 90d", got)
	}
}

// TestCountSettings_PutTrimsWhitespace proves a padded value is accepted rather
// than 400'd — the window is copied off the pooled request buffer and trimmed
// before validation.
func TestCountSettings_PutTrimsWhitespace(t *testing.T) {
	app := countApp(t)
	countPut(t, app, `{"window":"  24h  "}`)
	if got := countGet(t, app); got.Window != agent.CountWindow24h {
		t.Fatalf("padded window stored as %+v, want 24h", got)
	}
}

// TestCountSettings_GuardRejectsMissingSecret proves the endpoints share the
// same gateway-secret guard as the rest of the admin surface.
func TestCountSettings_GuardRejectsMissingSecret(t *testing.T) {
	app := countApp(t)

	for _, tc := range []struct{ method, body string }{
		{"GET", ""},
		{"PUT", `{"window":"24h"}`},
	} {
		req := httptest.NewRequest(tc.method, "/api/admin/agent/count-settings", strings.NewReader(tc.body))
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

// seedCountWindowIncidents saves incidents at ages chosen so every window
// yields a DIFFERENT open count: 24h → 2, 7d → 3, 90d → 5, all → 5. Without a
// record in the 1-7 day band, the 24h and 7d rows are identical assertions and
// the test cannot tell those two windows apart.
func seedCountWindowIncidents(t *testing.T, st storage.Provider) {
	t.Helper()
	now := time.Now().UTC()
	ages := []time.Duration{
		time.Hour,           // inside every window
		2 * time.Hour,       // inside every window
		3 * 24 * time.Hour,  // outside 24h, inside 7d
		30 * 24 * time.Hour, // outside 7d/30d, inside 90d
		31 * 24 * time.Hour, // outside 7d/30d, inside 90d
	}
	for i, age := range ages {
		rec := &storage.IncidentRecord{
			ID:        string(rune('a' + i)),
			Title:     "incident",
			Origin:    storage.OriginWebhook,
			Source:    "webhook",
			CreatedAt: now.Add(-age),
		}
		if err := st.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident %d: %v", i, err)
		}
	}
}

// openCount drives GET counts and returns the open tally from the by_status
// breakdown every count surface reads.
func openCount(t *testing.T, app *fiber.App) (int, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", "/counts", nil))
	if err != nil {
		t.Fatalf("GET /counts: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		CountWindow string `json:"count_window"`
		ByStatus    struct {
			Open struct {
				Total int `json:"total"`
			} `json:"open"`
		} `json:"by_status"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	return got.ByStatus.Open.Total, got.CountWindow
}

// TestCountsEndpointHonoursWindow is the end-to-end proof that the setting
// actually reaches the numbers: the same store, read through the same handler,
// reports different totals as the window changes. Without this, every other
// test here could pass while the counts stayed unbounded.
func TestCountsEndpointHonoursWindow(t *testing.T) {
	loadGatewayConfig(t, countSecret)
	st := storage.NewMemory()
	services.SetStorage(st)
	t.Cleanup(func() { services.SetStorage(nil) })
	seedCountWindowIncidents(t, st)

	app := fiber.New()
	app.Get("/counts", NewIncidentAdminController().counts)

	cases := []struct {
		window string
		want   int
	}{
		{agent.CountWindow24h, 2},
		{agent.CountWindow7d, 3},
		{agent.CountWindow90d, 5},
		{agent.CountWindowAll, 5},
	}
	for _, tc := range cases {
		if err := agent.SaveCountSettings(st, agent.CountSettings{Window: tc.window}); err != nil {
			t.Fatalf("SaveCountSettings(%q): %v", tc.window, err)
		}
		got, countWindow := openCount(t, app)
		if got != tc.want || countWindow != tc.window {
			t.Fatalf("window %q: open = %d count_window = %q, want %d and %q", tc.window, got, countWindow, tc.want, tc.window)
		}
	}
}

// TestCountsEndpointDefaultsToSevenDays proves a store with NO settings blob
// is already windowed — the default is a real 7-day bound, not "all".
func TestCountsEndpointDefaultsToSevenDays(t *testing.T) {
	loadGatewayConfig(t, countSecret)
	st := storage.NewMemory()
	services.SetStorage(st)
	t.Cleanup(func() { services.SetStorage(nil) })
	seedCountWindowIncidents(t, st)

	app := fiber.New()
	app.Get("/counts", NewIncidentAdminController().counts)

	got, countWindow := openCount(t, app)
	if got != 3 || countWindow != agent.CountWindow7d {
		t.Fatalf("unconfigured open = %d count_window = %q, want 3 and 7d", got, countWindow)
	}
}
