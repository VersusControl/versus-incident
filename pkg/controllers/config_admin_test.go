package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"

	"github.com/gofiber/fiber/v2"
)

const configAdminSecret = "test-gateway-secret"

// fakeAIResolver stands in for the enterprise runtime AI-settings resolver: it
// answers the org-scoped getter the admin config endpoint reads.
type fakeAIResolver struct {
	enabled  bool
	provider string
	ok       bool
	sawOrg   string
}

func (f *fakeAIResolver) EffectiveKey(context.Context) (string, bool)   { return "", false }
func (f *fakeAIResolver) EffectiveEnabled(context.Context) (bool, bool) { return false, false }
func (f *fakeAIResolver) EffectiveAISettingsForOrg(org string) (bool, string, bool) {
	f.sawOrg = org
	return f.enabled, f.provider, f.ok
}

// configAdminApp mounts the config admin routes over a config whose YAML AI
// block is disabled with a key set, the shape the customer report starts from.
func configAdminApp(t *testing.T) *fiber.App {
	t.Helper()
	loadGatewayConfig(t, configAdminSecret)

	cfg := config.GetConfig()
	prevSecret := cfg.GatewaySecret
	prevAI := cfg.Agent.AI
	cfg.GatewaySecret = configAdminSecret
	cfg.Agent.AI.Enable = false
	cfg.Agent.AI.Provider = "openai"
	cfg.Agent.AI.APIKey = "sk-super-secret-value"
	t.Cleanup(func() {
		cfg.GatewaySecret = prevSecret
		cfg.Agent.AI = prevAI
		agent.SetAISettingsResolver(nil)
	})

	app := fiber.New()
	NewConfigAdminController().Register(app.Group("/api"))
	return app
}

// getAgentConfigAI issues the admin GET and returns its "ai" block.
func getAgentConfigAI(t *testing.T, app *fiber.App) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/config/agent", nil)
	req.Header.Set("X-Gateway-Secret", configAdminSecret)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body struct {
		AI map[string]any `json:"ai"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, raw)
	}
	if got, ok := body.AI["api_key"].(string); !ok || got == "sk-super-secret-value" {
		t.Fatalf("api_key = %v, must stay a set/unset marker and never carry the key", body.AI["api_key"])
	}
	return body.AI
}

// TestAgentConfigAI_NoResolver_ReportsStatic proves the community path is
// unchanged: with no resolver registered the endpoint reports the YAML values.
func TestAgentConfigAI_NoResolver_ReportsStatic(t *testing.T) {
	app := configAdminApp(t)
	agent.SetAISettingsResolver(nil)

	ai := getAgentConfigAI(t, app)
	if ai["enable"] != false {
		t.Errorf("enable = %v, want the static false", ai["enable"])
	}
	if ai["provider"] != "openai" {
		t.Errorf("provider = %v, want the static %q", ai["provider"], "openai")
	}
	if ai["api_key"] != "set" {
		t.Errorf("api_key = %v, want %q", ai["api_key"], "set")
	}
}

// TestAgentConfigAI_RuntimeOverride_ReportsEnabled is the customer bug: AI
// switched on at runtime while the YAML says off must report enabled.
func TestAgentConfigAI_RuntimeOverride_ReportsEnabled(t *testing.T) {
	app := configAdminApp(t)
	res := &fakeAIResolver{enabled: true, provider: "ollama", ok: true}
	agent.SetAISettingsResolver(res)

	ai := getAgentConfigAI(t, app)
	if ai["enable"] != true {
		t.Errorf("enable = %v, want true from the runtime override", ai["enable"])
	}
	if ai["provider"] != "ollama" {
		t.Errorf("provider = %v, want %q from the runtime override", ai["provider"], "ollama")
	}
	if ai["api_key"] != "set" {
		t.Errorf("api_key = %v, want %q — the override must not change its shape", ai["api_key"], "set")
	}
}

// TestAgentConfigAI_ResolverError_FallsBackToStatic proves a resolver that
// cannot answer degrades to the YAML values rather than erroring or reporting
// a false "on", with the API key still a boolean marker.
func TestAgentConfigAI_ResolverError_FallsBackToStatic(t *testing.T) {
	app := configAdminApp(t)
	agent.SetAISettingsResolver(&fakeAIResolver{enabled: true, provider: "ollama", ok: false})

	ai := getAgentConfigAI(t, app)
	if ai["enable"] != false {
		t.Errorf("enable = %v, want the static false when the override is unavailable", ai["enable"])
	}
	if ai["provider"] != "openai" {
		t.Errorf("provider = %v, want the static %q", ai["provider"], "openai")
	}
	if ai["api_key"] != "set" {
		t.Errorf("api_key = %v, want %q", ai["api_key"], "set")
	}
}
