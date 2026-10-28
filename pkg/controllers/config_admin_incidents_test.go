package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"

	"github.com/gofiber/fiber/v2"
)

// runtimeChannelResolver stands in for the enterprise runtime-channel resolver:
// it overlays Slack's credentials and enable flag onto the caller-owned clone,
// exactly as the emission path's resolver does.
type runtimeChannelResolver struct {
	enable    bool
	token     string
	channelID string
	applied   bool
}

func (r runtimeChannelResolver) ResolveAlert(_ context.Context, base *config.AlertConfig) bool {
	base.Slack.Enable = r.enable
	if r.token != "" {
		base.Slack.Token = r.token
	}
	if r.channelID != "" {
		base.Slack.ChannelID = r.channelID
	}
	return r.applied
}

// incidentsConfigChannel issues the admin GET and returns the raw body plus the
// named channel's entry from the alert.channels list.
func incidentsConfigChannel(t *testing.T, app *fiber.App, id string) ([]byte, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/config/incidents", nil)
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
		Alert struct {
			Channels []map[string]any `json:"channels"`
		} `json:"alert"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, raw)
	}
	for _, ch := range body.Alert.Channels {
		if ch["id"] == id {
			return raw, ch
		}
	}
	t.Fatalf("channel %q missing from the response (%s)", id, raw)
	return nil, nil
}

// channelField pulls one labelled field's reported value out of a channel entry.
func channelField(t *testing.T, channel map[string]any, label string) any {
	t.Helper()
	fields, ok := channel["fields"].([]any)
	if !ok {
		t.Fatalf("channel fields missing or wrong type: %v", channel["fields"])
	}
	for _, f := range fields {
		m, ok := f.(map[string]any)
		if ok && m["label"] == label {
			return m["value"]
		}
	}
	t.Fatalf("field %q missing from channel %v", label, channel)
	return nil
}

// TestIncidentsConfig_NoResolverReportsStatic proves the community path is
// unchanged: with no resolver registered the endpoint reports the YAML floor,
// where the harness config leaves Slack disabled with no token.
func TestIncidentsConfig_NoResolverReportsStatic(t *testing.T) {
	app := configAdminApp(t)
	config.SetAlertConfigResolver(nil)
	t.Cleanup(func() { config.SetAlertConfigResolver(nil) })

	_, slack := incidentsConfigChannel(t, app, "slack")
	if slack["enable"] != false {
		t.Errorf("enable = %v, want the static false", slack["enable"])
	}
	if got := channelField(t, slack, "Token"); got != "" {
		t.Errorf("Token marker = %v, want empty (no YAML token)", got)
	}
}

// TestIncidentsConfig_RuntimeOverrideIsReported is the staleness bug, the same
// class as the AI toggle and the report channel picker: an operator who
// hot-configured Slack saw the endpoint still report it disabled with no
// credential, while the very next incident was delivered through it.
func TestIncidentsConfig_RuntimeOverrideIsReported(t *testing.T) {
	app := configAdminApp(t)
	config.SetAlertConfigResolver(runtimeChannelResolver{
		enable: true, token: "xoxb-runtime-secret", channelID: "C-RUNTIME", applied: true,
	})
	t.Cleanup(func() { config.SetAlertConfigResolver(nil) })

	raw, slack := incidentsConfigChannel(t, app, "slack")
	if slack["enable"] != true {
		t.Errorf("enable = %v, want true from the runtime override", slack["enable"])
	}
	if got := channelField(t, slack, "Token"); got != "set" {
		t.Errorf("Token marker = %v, want %q from the runtime override", got, "set")
	}
	if got := channelField(t, slack, "Channel ID"); got != "set" {
		t.Errorf("Channel ID marker = %v, want %q", got, "set")
	}
	// Secrets stay markers: the resolved credential never reaches the response.
	for _, secret := range []string{"xoxb-runtime-secret", "C-RUNTIME"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("runtime channel secret %q leaked into the config response", secret)
		}
	}
}

// TestIncidentsConfig_ResolverNoOpinionFallsBackToYAML proves a resolver that
// applies nothing leaves the reported config at the YAML floor.
func TestIncidentsConfig_ResolverNoOpinionFallsBackToYAML(t *testing.T) {
	app := configAdminApp(t)
	config.SetAlertConfigResolver(runtimeChannelResolver{enable: true, token: "xoxb-ignored", applied: false})
	t.Cleanup(func() { config.SetAlertConfigResolver(nil) })

	_, slack := incidentsConfigChannel(t, app, "slack")
	if slack["enable"] != false {
		t.Errorf("enable = %v, want the static false when the resolver has no opinion", slack["enable"])
	}
	if got := channelField(t, slack, "Token"); got != "" {
		t.Errorf("Token marker = %v, want empty", got)
	}
}

// TestIncidentsConfig_PanickingResolverFallsBackToYAML proves the read path is
// fail-safe in the same way the emission path is: a misbehaving resolver leaves
// the endpoint reporting the YAML floor instead of erroring.
func TestIncidentsConfig_PanickingResolverFallsBackToYAML(t *testing.T) {
	app := configAdminApp(t)
	config.SetAlertConfigResolver(panickingAlertResolver{})
	t.Cleanup(func() { config.SetAlertConfigResolver(nil) })

	_, slack := incidentsConfigChannel(t, app, "slack")
	if slack["enable"] != false {
		t.Errorf("enable = %v, want the static false after a resolver panic", slack["enable"])
	}
}

type panickingAlertResolver struct{}

func (panickingAlertResolver) ResolveAlert(context.Context, *config.AlertConfig) bool {
	panic("boom")
}

// TestIncidentsConfig_GlobalConfigNotMutated proves the read path honours the
// never-mutate-global rule: the resolver runs against a clone, so the global
// YAML floor is the same before and after the request.
func TestIncidentsConfig_GlobalConfigNotMutated(t *testing.T) {
	app := configAdminApp(t)
	config.SetAlertConfigResolver(runtimeChannelResolver{
		enable: true, token: "xoxb-runtime-secret", applied: true,
	})
	t.Cleanup(func() { config.SetAlertConfigResolver(nil) })

	before := config.GetConfig().Alert.Slack
	incidentsConfigChannel(t, app, "slack")
	if after := config.GetConfig().Alert.Slack; after.Enable != before.Enable || after.Token != before.Token {
		t.Fatalf("global Slack config mutated: %+v -> %+v", before, after)
	}
}

// TestEnabledAlertChannels_RidesTheRuntimeResolver covers the report channel
// picker's FALLBACK path — a runtime channel resolver registered but no channel
// lister. It used to read the static global config, so it offered a channel set
// the emission path would not have used.
func TestEnabledAlertChannels_RidesTheRuntimeResolver(t *testing.T) {
	configAdminApp(t) // loads a config whose YAML floor leaves Slack disabled
	config.SetAlertConfigResolver(nil)
	t.Cleanup(func() { config.SetAlertConfigResolver(nil) })

	if got := enabledAlertChannels(config.EffectiveAlertConfig(context.Background())); len(got) != 0 {
		t.Fatalf("channels = %v, want none from the YAML floor", got)
	}

	config.SetAlertConfigResolver(runtimeChannelResolver{enable: true, token: "xoxb-runtime-secret", applied: true})
	got := enabledAlertChannels(config.EffectiveAlertConfig(context.Background()))
	if len(got) != 1 || got[0] != "slack" {
		t.Fatalf("channels = %v, want [slack] from the runtime override", got)
	}
}
