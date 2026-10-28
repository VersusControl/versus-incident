package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"

	"github.com/gofiber/fiber/v2"
)

// runtimeAIKeyResolver stands in for the enterprise runtime AI-settings
// resolver, answering the org-scoped key-set question the admin config endpoint
// reads. It carries a key so the test can prove the key itself never leaves the
// process even when the endpoint reports "configured".
type runtimeAIKeyResolver struct {
	key    string
	keySet bool
	ok     bool
	sawOrg string
}

func (r *runtimeAIKeyResolver) EffectiveKey(context.Context) (string, bool) { return r.key, true }
func (r *runtimeAIKeyResolver) EffectiveEnabled(context.Context) (bool, bool) {
	return true, false
}
func (r *runtimeAIKeyResolver) EffectiveAIKeySetForOrg(org string) (bool, bool) {
	r.sawOrg = org
	return r.keySet, r.ok
}

// getAgentConfigRaw issues the admin GET and returns the raw body alongside the
// decoded "ai" block, so a test can assert on what is NOT in the response.
func getAgentConfigRaw(t *testing.T, app *fiber.App) ([]byte, map[string]any) {
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
	return raw, body.AI
}

// TestAgentConfigAIKey_RuntimeOverrideOnly is the customer bug: the org's AI
// credential lives only in the runtime override, so reporting the YAML floor
// rendered api_key:"" and the UI claimed no key was configured while the worker
// was calling the model with it.
func TestAgentConfigAIKey_RuntimeOverrideOnly(t *testing.T) {
	app := configAdminApp(t)
	cfg := config.GetConfig()
	cfg.Agent.AI.APIKey = "" // no YAML key at all

	res := &runtimeAIKeyResolver{key: "sk-runtime-only-secret", keySet: true, ok: true}
	agent.SetAISettingsResolver(res)

	raw, ai := getAgentConfigRaw(t, app)
	if ai["api_key"] != "set" {
		t.Errorf("api_key = %v, want %q from the runtime override", ai["api_key"], "set")
	}
	// The marker is a boolean fact, never the credential or a prefix of it.
	if strings.Contains(string(raw), "sk-runtime-only-secret") {
		t.Fatal("the runtime AI key leaked into the config response")
	}
	if strings.Contains(string(raw), "sk-runtime") {
		t.Fatal("a prefix of the runtime AI key leaked into the config response")
	}
}

// TestAgentConfigAIKey_OverrideCanReportUnset proves the override is authoritative
// in BOTH directions: an org whose runtime override holds no key reports unset
// even though the YAML floor has one, so the UI stops claiming a key is live
// after the operator cleared it.
func TestAgentConfigAIKey_OverrideCanReportUnset(t *testing.T) {
	app := configAdminApp(t) // YAML key is set by the harness
	agent.SetAISettingsResolver(&runtimeAIKeyResolver{keySet: false, ok: true})

	_, ai := getAgentConfigRaw(t, app)
	if ai["api_key"] != "" {
		t.Errorf("api_key = %v, want empty from the runtime override", ai["api_key"])
	}
}

// TestAgentConfigAIKey_NoOpinionFallsBackToYAML proves a resolver that cannot
// answer degrades to the YAML floor instead of reporting a false "no key".
func TestAgentConfigAIKey_NoOpinionFallsBackToYAML(t *testing.T) {
	app := configAdminApp(t) // YAML key is set by the harness
	agent.SetAISettingsResolver(&runtimeAIKeyResolver{keySet: false, ok: false})

	_, ai := getAgentConfigRaw(t, app)
	if ai["api_key"] != "set" {
		t.Errorf("api_key = %v, want the YAML floor %q", ai["api_key"], "set")
	}
}

// TestAgentConfigAIKey_NoResolverIsCommunityPath proves the community path is
// untouched: with no resolver the endpoint reports the YAML floor.
func TestAgentConfigAIKey_NoResolverIsCommunityPath(t *testing.T) {
	app := configAdminApp(t)
	agent.SetAISettingsResolver(nil)

	_, ai := getAgentConfigRaw(t, app)
	if ai["api_key"] != "set" {
		t.Errorf("api_key = %v, want the YAML floor %q", ai["api_key"], "set")
	}

	config.GetConfig().Agent.AI.APIKey = ""
	_, ai = getAgentConfigRaw(t, app)
	if ai["api_key"] != "" {
		t.Errorf("api_key = %v, want empty with no YAML key and no resolver", ai["api_key"])
	}
}
