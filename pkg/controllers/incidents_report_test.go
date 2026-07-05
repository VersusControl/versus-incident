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
	"github.com/VersusControl/versus-incident/pkg/report"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

const reportSecret = "test-gateway-secret"

// TestReportRoutesRegistered guards the new /admin/reports endpoints against
// silently dropping off the route table, and asserts the per-incident routes
// are gone.
func TestReportRoutesRegistered(t *testing.T) {
	app := fiber.New()
	api := app.Group("/api")
	NewIncidentAdminController().Register(api)
	NewReportsAdminController().Register(api)

	have := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		have[r.Method+" "+r.Path] = true
	}
	for _, key := range []string{
		"POST /api/admin/reports/incidents",
		"GET /api/admin/reports/incidents/report.png",
		"GET /api/admin/reports/settings",
		"PUT /api/admin/reports/settings",
	} {
		if !have[key] {
			t.Errorf("route %q not registered; have:\n%v", key, have)
		}
	}
	// The per-incident report routes must be removed.
	for _, gone := range []string{
		"POST /api/admin/incidents/:id/report",
		"GET /api/admin/incidents/:id/report.png",
	} {
		if have[gone] {
			t.Errorf("removed per-incident route %q is still registered", gone)
		}
	}
}

// reportTestSetup wires config, the real pure-Go renderer, a default redactor,
// a memory store with one ai_detect + one webhook incident (created now), and
// runtime settings enabling the report. Returns the mounted app + store.
func reportTestSetup(t *testing.T, ratePerMinute int) (*fiber.App, storage.Provider) {
	t.Helper()
	loadGatewayConfig(t, reportSecret)

	prevSecret := config.GetConfig().GatewaySecret
	config.GetConfig().GatewaySecret = reportSecret
	t.Cleanup(func() {
		config.GetConfig().GatewaySecret = prevSecret
		services.SetStorage(nil)
		services.SetReportRenderer(nil)
	})

	renderer, err := report.NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	services.SetReportRenderer(renderer)
	rdc, _ := agent.NewRedactor(false, nil)
	services.SetReportRedactor(rdc)

	st := storage.NewMemory()
	now := time.Now().UTC()
	agentRec := &storage.IncidentRecord{
		ID:        "agent0001",
		Title:     "Pool exhausted",
		Service:   "payments",
		Source:    "agent:elasticsearch:payments",
		Origin:    storage.OriginAIDetect,
		CreatedAt: now.Add(-1 * time.Minute),
		Content:   map[string]interface{}{"Summary": "connections exhausted", "Severity": "critical"},
	}
	webhookRec := &storage.IncidentRecord{
		ID:        "webhook01",
		Title:     "Disk full",
		Service:   "db",
		Source:    "webhook",
		Origin:    storage.OriginWebhook,
		CreatedAt: now.Add(-2 * time.Minute),
		Content:   map[string]interface{}{"title": "Disk full", "Severity": "high"},
	}
	if err := st.SaveIncident(agentRec); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	if err := st.SaveIncident(webhookRec); err != nil {
		t.Fatalf("save webhook: %v", err)
	}
	if err := services.SaveReportSettings(st, services.ReportSettings{Enable: true, IncludeChart: true, RatePerMinute: ratePerMinute, DefaultWindow: "today"}); err != nil {
		t.Fatalf("SaveReportSettings: %v", err)
	}
	services.SetStorage(st)

	app := fiber.New()
	api := app.Group("/api")
	NewIncidentAdminController().Register(api)
	NewReportsAdminController().Register(api)
	return app, st
}

// TestReportPNG_Windows asserts the preview endpoint renders a real PNG for
// each window (aggregating both AI-detect and webhook incidents).
func TestReportPNG_Windows(t *testing.T) {
	app, _ := reportTestSetup(t, 0)

	for _, w := range []string{"today", "24h", "7d"} {
		req := httptest.NewRequest("GET", "/api/admin/reports/incidents/report.png?window="+w, nil)
		req.Header.Set("X-Gateway-Secret", reportSecret)
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("%s: status = %d, want 200", w, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
			t.Fatalf("%s: content-type = %q, want image/png", w, ct)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
			t.Fatalf("%s: response is not a PNG (%d bytes)", w, len(data))
		}
	}
}

func TestReportPNG_BadWindow400(t *testing.T) {
	app, _ := reportTestSetup(t, 0)
	req := httptest.NewRequest("GET", "/api/admin/reports/incidents/report.png?window=year", nil)
	req.Header.Set("X-Gateway-Secret", reportSecret)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestReportPNG_DefaultsToTodayWhenAbsent(t *testing.T) {
	app, _ := reportTestSetup(t, 0)
	req := httptest.NewRequest("GET", "/api/admin/reports/incidents/report.png", nil)
	req.Header.Set("X-Gateway-Secret", reportSecret)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (absent window → today)", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestReportPNG_RequiresAuth(t *testing.T) {
	app, _ := reportTestSetup(t, 0)
	req := httptest.NewRequest("GET", "/api/admin/reports/incidents/report.png?window=today", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestReportPNG_DisabledReturns501(t *testing.T) {
	app, st := reportTestSetup(t, 0)
	if err := services.SaveReportSettings(st, services.ReportSettings{Enable: false}); err != nil {
		t.Fatalf("SaveReportSettings: %v", err)
	}
	req := httptest.NewRequest("GET", "/api/admin/reports/incidents/report.png?window=today", nil)
	req.Header.Set("X-Gateway-Secret", reportSecret)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

func TestReportPNG_RateLimited429(t *testing.T) {
	// Use the 7d window so the "report:7d" limiter bucket is not shared with
	// other tests that hit today/24h.
	app, _ := reportTestSetup(t, 1)
	do := func() int {
		req := httptest.NewRequest("GET", "/api/admin/reports/incidents/report.png?window=7d", nil)
		req.Header.Set("X-Gateway-Secret", reportSecret)
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := do(); code != fiber.StatusOK {
		t.Fatalf("first = %d, want 200", code)
	}
	if code := do(); code != fiber.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429", code)
	}
}

// TestReportSend_NoEnabledChannel exercises the POST send endpoint through the
// real service: with no channels enabled in config, the resolved channel maps
// to no provider and the endpoint returns 400.
func TestReportSend_NoEnabledChannel(t *testing.T) {
	app, _ := reportTestSetup(t, 0)
	body := `{"channel":"slack"}`
	req := httptest.NewRequest("POST", "/api/admin/reports/incidents?window=today", strings.NewReader(body))
	req.Header.Set("X-Gateway-Secret", reportSecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestReportSettings_GetPutRoundTrip drives the runtime settings endpoints:
// GET returns the built-in defaults on a fresh store, PUT persists an update,
// and a subsequent GET reflects it (sanitized).
func TestReportSettings_GetPutRoundTrip(t *testing.T) {
	loadGatewayConfig(t, reportSecret)
	config.GetConfig().GatewaySecret = reportSecret
	st := storage.NewMemory()
	services.SetStorage(st)
	t.Cleanup(func() { services.SetStorage(nil) })

	app := fiber.New()
	api := app.Group("/api")
	NewReportsAdminController().Register(api)

	// GET → defaults (feature off).
	getReq := httptest.NewRequest("GET", "/api/admin/reports/settings", nil)
	getReq.Header.Set("X-Gateway-Secret", reportSecret)
	getResp, err := app.Test(getReq, -1)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var defaults services.ReportSettings
	_ = json.NewDecoder(getResp.Body).Decode(&defaults)
	getResp.Body.Close()
	if defaults.Enable || defaults.DefaultWindow != "today" || !defaults.IncludeChart {
		t.Fatalf("defaults = %+v, want off/today/charts-on", defaults)
	}

	// PUT → enable + 7d + a bogus window is sanitized back to a valid one.
	putBody := `{"enable":true,"default_channel":"slack","include_chart":false,"rate_per_minute":10,"default_window":"7d"}`
	putReq := httptest.NewRequest("PUT", "/api/admin/reports/settings", strings.NewReader(putBody))
	putReq.Header.Set("X-Gateway-Secret", reportSecret)
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := app.Test(putReq, -1)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if putResp.StatusCode != fiber.StatusOK {
		t.Fatalf("PUT status = %d, want 200", putResp.StatusCode)
	}
	putResp.Body.Close()

	// GET again → reflects the update.
	getReq2 := httptest.NewRequest("GET", "/api/admin/reports/settings", nil)
	getReq2.Header.Set("X-Gateway-Secret", reportSecret)
	getResp2, err := app.Test(getReq2, -1)
	if err != nil {
		t.Fatalf("GET2: %v", err)
	}
	var updated services.ReportSettings
	_ = json.NewDecoder(getResp2.Body).Decode(&updated)
	getResp2.Body.Close()
	if !updated.Enable || updated.DefaultChannel != "slack" || updated.IncludeChart || updated.RatePerMinute != 10 || updated.DefaultWindow != "7d" {
		t.Fatalf("updated = %+v", updated)
	}
}
