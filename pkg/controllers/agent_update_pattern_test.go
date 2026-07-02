package controllers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// TestUpdatePattern_VerdictSetClearTagsOnly covers BUG 2's API contract: the
// pointer-typed updatePatternRequest.Verdict lets POST /patterns/:id express
// three distinct intents the UI sends —
//   - {"verdict":"known"} → SET the verdict
//   - {"verdict":""}      → CLEAR the verdict (was a silent no-op before)
//   - {"tags":[...]}      → tags-only, verdict absent, leave it unchanged
func TestUpdatePattern_VerdictSetClearTagsOnly(t *testing.T) {
	agent.SetCatalogStore(nil)
	const secret = "test-gateway-secret"
	// config.LoadConfig is sync.Once-guarded; ensure the global config exists
	// then pin the gateway secret directly (restored after) so authMiddleware
	// admits our requests regardless of suite order.
	loadGatewayConfig(t, secret)
	prevSecret := config.GetConfig().GatewaySecret
	config.GetConfig().GatewaySecret = secret
	t.Cleanup(func() { config.GetConfig().GatewaySecret = prevSecret })

	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("p-1", "one <*>", "es", 3, 0.2, "", "api")

	app := fiber.New()
	api := app.Group("/api")
	NewAgentController(cat, nil, nil, nil, nil, false).Register(api)

	post := func(body string) (int, *agent.Pattern) {
		req := httptest.NewRequest("POST", "/api/agent/patterns/p-1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gateway-Secret", secret)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var p agent.Pattern
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("unmarshal %q: %v", raw, err)
			}
		}
		return resp.StatusCode, &p
	}

	// SET: {"verdict":"known"} → verdict becomes "known".
	if code, p := post(`{"verdict":"known"}`); code != fiber.StatusOK || p.Verdict != "known" {
		t.Fatalf("set: code=%d verdict=%q, want 200/known", code, p.Verdict)
	}

	// TAGS-ONLY: verdict key absent → verdict unchanged (still "known"), tags applied.
	if code, p := post(`{"tags":["noisy"]}`); code != fiber.StatusOK || p.Verdict != "known" || len(p.Tags) != 1 || p.Tags[0] != "noisy" {
		t.Fatalf("tags-only: code=%d verdict=%q tags=%v, want 200/known/[noisy]", code, p.Verdict, p.Tags)
	}

	// CLEAR: {"verdict":""} → verdict cleared to "". Tags key absent → tags
	// left intact (proves clear is verdict-scoped).
	if code, p := post(`{"verdict":""}`); code != fiber.StatusOK || p.Verdict != "" {
		t.Fatalf("clear: code=%d verdict=%q, want 200/empty (clear must not be a no-op)", code, p.Verdict)
	}
	if got := cat.Get("p-1"); got.Verdict != "" || len(got.Tags) != 1 {
		t.Fatalf("post-clear catalog state: verdict=%q tags=%v, want empty verdict + [noisy] tags", got.Verdict, got.Tags)
	}
}
