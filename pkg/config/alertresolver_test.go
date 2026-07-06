package config

import (
	"context"
	"errors"
	"testing"
)

// stubAlertResolver is a fake AlertConfigResolver that overlays only the
// channels named in `set`, so a test can prove partial override, clearing,
// and fail-safe behaviour.
type stubAlertResolver struct {
	// slackToken, when non-empty, overrides Alert.Slack.Token + enables Slack.
	slackToken string
	// telegramBot, when non-empty, overrides Alert.Telegram.BotToken + enables.
	telegramBot string
	// applied is returned as the applied flag.
	applied bool
	// panics, when true, makes ResolveAlert panic (fail-safe test).
	panics bool
	calls  int
}

func (s *stubAlertResolver) ResolveAlert(_ context.Context, base *AlertConfig) bool {
	s.calls++
	if s.panics {
		panic(errors.New("boom"))
	}
	if s.slackToken != "" {
		base.Slack.Enable = true
		base.Slack.Token = s.slackToken
	}
	if s.telegramBot != "" {
		base.Telegram.Enable = true
		base.Telegram.BotToken = s.telegramBot
	}
	return s.applied
}

// baseConfig builds a global cfg with a known YAML floor for the channel
// config, and installs it as the process cfg so cloneConfig has something to
// clone. It restores the previous cfg on cleanup.
func baseConfig(t *testing.T) {
	t.Helper()
	prev := cfg
	t.Cleanup(func() { cfg = prev })
	cfg = &Config{
		Name: "test",
		Alert: AlertConfig{
			Slack: SlackConfig{
				Enable:    true,
				Token:     "yaml-slack-token",
				ChannelID: "C-YAML",
			},
			Telegram: TelegramConfig{
				Enable:   false,
				BotToken: "yaml-telegram-token",
				ChatID:   "chat-yaml",
			},
			Email: EmailConfig{
				Enable:   true,
				SMTPHost: "smtp.yaml",
				Username: "yaml-user",
				Password: "yaml-pass",
				To:       "ops@yaml",
			},
		},
	}
}

// TestAlertConfigResolver_NilByDefault proves OSS registers nothing: the slot
// is nil, so the emission path uses the static YAML alert config — the
// byte-for-byte unchanged path.
func TestAlertConfigResolver_NilByDefault(t *testing.T) {
	SetAlertConfigResolver(nil)
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	if r := alertConfigResolver(); r != nil {
		t.Fatalf("alertConfigResolver() = %v, want nil in OSS", r)
	}
}

// TestAlertConfigResolver_LastWins proves the single slot is last-wins and
// that nil clears it, mirroring SetAISettingsResolver.
func TestAlertConfigResolver_LastWins(t *testing.T) {
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	first := &stubAlertResolver{slackToken: "one", applied: true}
	second := &stubAlertResolver{slackToken: "two", applied: true}

	SetAlertConfigResolver(first)
	if r := alertConfigResolver(); r != first {
		t.Fatalf("after first register: slot = %v, want first", r)
	}

	SetAlertConfigResolver(second)
	if r := alertConfigResolver(); r != second {
		t.Fatalf("after second register: slot = %v, want second (last-wins)", r)
	}

	SetAlertConfigResolver(nil)
	if r := alertConfigResolver(); r != nil {
		t.Fatalf("after nil register: slot = %v, want nil (cleared)", r)
	}
}

// TestGetConfigForAlert_NilResolver_YAMLUnchanged proves the OSS fast path:
// with no resolver and no params, GetConfigForAlert returns the GLOBAL cfg
// pointer (no clone) and the channel config is exactly the YAML floor.
func TestGetConfigForAlert_NilResolver_YAMLUnchanged(t *testing.T) {
	baseConfig(t)
	SetAlertConfigResolver(nil)
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	got := GetConfigForAlert(context.Background(), nil)
	if got != cfg {
		t.Fatalf("OSS fast path must return the global cfg pointer (no clone); got a different pointer")
	}
	if got.Alert.Slack.Token != "yaml-slack-token" {
		t.Fatalf("Slack token = %q, want YAML floor", got.Alert.Slack.Token)
	}
}

// TestGetConfigForAlert_PartialOverride proves a resolver overrides only the
// channels it sets and leaves the rest at the YAML floor, on a CLONE (global
// cfg untouched).
func TestGetConfigForAlert_PartialOverride(t *testing.T) {
	baseConfig(t)
	SetAlertConfigResolver(&stubAlertResolver{slackToken: "runtime-slack", applied: true})
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	got := GetConfigForAlert(context.Background(), nil)

	// Overridden channel picks up the runtime value.
	if got.Alert.Slack.Token != "runtime-slack" || !got.Alert.Slack.Enable {
		t.Fatalf("Slack = (%q, enable=%v), want runtime override", got.Alert.Slack.Token, got.Alert.Slack.Enable)
	}
	// Un-overridden channels keep their YAML floor.
	if got.Alert.Telegram.BotToken != "yaml-telegram-token" {
		t.Fatalf("Telegram token = %q, want YAML floor (partial override)", got.Alert.Telegram.BotToken)
	}
	if got.Alert.Email.Username != "yaml-user" || got.Alert.Email.Password != "yaml-pass" {
		t.Fatalf("Email creds = (%q,%q), want YAML floor", got.Alert.Email.Username, got.Alert.Email.Password)
	}
	// Global cfg is never mutated.
	if cfg.Alert.Slack.Token != "yaml-slack-token" {
		t.Fatalf("global cfg Slack token mutated to %q, want YAML floor untouched", cfg.Alert.Slack.Token)
	}
	if got == cfg {
		t.Fatal("override path must return a CLONE, not the global cfg pointer")
	}
}

// TestGetConfigForAlert_ClearRevertsToYAML proves clearing the resolver (nil,
// or a resolver with no opinion) reverts the effective config to the YAML floor.
func TestGetConfigForAlert_ClearRevertsToYAML(t *testing.T) {
	baseConfig(t)
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	// First: an override is active.
	SetAlertConfigResolver(&stubAlertResolver{slackToken: "runtime-slack", applied: true})
	if got := GetConfigForAlert(context.Background(), nil); got.Alert.Slack.Token != "runtime-slack" {
		t.Fatalf("with override: Slack token = %q, want runtime", got.Alert.Slack.Token)
	}

	// Clear it: back to YAML on the next resolve (hot, no restart).
	SetAlertConfigResolver(nil)
	got := GetConfigForAlert(context.Background(), nil)
	if got.Alert.Slack.Token != "yaml-slack-token" {
		t.Fatalf("after clear: Slack token = %q, want YAML floor", got.Alert.Slack.Token)
	}
}

// TestGetConfigForAlert_NoOpinionResolver proves a resolver that returns
// applied=false leaves the clone at its YAML floor (no override swapped in).
func TestGetConfigForAlert_NoOpinionResolver(t *testing.T) {
	baseConfig(t)
	// slackToken empty + applied=false ⇒ resolver touches nothing and claims no
	// opinion.
	SetAlertConfigResolver(&stubAlertResolver{applied: false})
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	got := GetConfigForAlert(context.Background(), nil)
	if got.Alert.Slack.Token != "yaml-slack-token" {
		t.Fatalf("Slack token = %q, want YAML floor when resolver has no opinion", got.Alert.Slack.Token)
	}
}

// TestGetConfigForAlert_ResolverPanic_FailsSafeToYAML proves a panicking
// resolver does NOT crash the emission path and does NOT half-mutate the clone:
// the effective config falls back to the YAML floor for every channel.
func TestGetConfigForAlert_ResolverPanic_FailsSafeToYAML(t *testing.T) {
	baseConfig(t)
	SetAlertConfigResolver(&stubAlertResolver{slackToken: "runtime-slack", panics: true})
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	got := GetConfigForAlert(context.Background(), nil) // must not panic
	if got.Alert.Slack.Token != "yaml-slack-token" {
		t.Fatalf("after resolver panic: Slack token = %q, want YAML floor (fail-safe)", got.Alert.Slack.Token)
	}
	if got.Alert.Telegram.BotToken != "yaml-telegram-token" {
		t.Fatalf("after resolver panic: Telegram token = %q, want YAML floor", got.Alert.Telegram.BotToken)
	}
}

// TestGetConfigForAlert_Precedence_RuntimeThenParams proves the precedence
// layering: the runtime override supplies credentials + enable, and the
// existing per-incident routing params still apply ON TOP (routing is a
// distinct axis and must not be clobbered by the credential override).
func TestGetConfigForAlert_Precedence_RuntimeThenParams(t *testing.T) {
	baseConfig(t)
	SetAlertConfigResolver(&stubAlertResolver{slackToken: "runtime-slack", applied: true})
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	params := map[string]string{"slack_channel_id": "C-PERINCIDENT"}
	got := GetConfigForAlert(context.Background(), &params)

	// Runtime override wins for the credential.
	if got.Alert.Slack.Token != "runtime-slack" {
		t.Fatalf("Slack token = %q, want runtime override", got.Alert.Slack.Token)
	}
	// Per-incident routing param wins for the channel id, on top.
	if got.Alert.Slack.ChannelID != "C-PERINCIDENT" {
		t.Fatalf("Slack channel id = %q, want per-incident routing param on top", got.Alert.Slack.ChannelID)
	}
	// Global cfg untouched.
	if cfg.Alert.Slack.ChannelID != "C-YAML" || cfg.Alert.Slack.Token != "yaml-slack-token" {
		t.Fatalf("global cfg mutated: %+v", cfg.Alert.Slack)
	}
}

// TestGetConfigForAlert_ParamsOnly_NoResolver proves the existing per-incident
// routing overlay still works with no resolver registered (community): params
// overlay on a clone, YAML credentials preserved.
func TestGetConfigForAlert_ParamsOnly_NoResolver(t *testing.T) {
	baseConfig(t)
	SetAlertConfigResolver(nil)
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	params := map[string]string{"slack_channel_id": "C-PERINCIDENT"}
	got := GetConfigForAlert(context.Background(), &params)

	if got == cfg {
		t.Fatal("params path must return a clone, not the global cfg pointer")
	}
	if got.Alert.Slack.ChannelID != "C-PERINCIDENT" {
		t.Fatalf("Slack channel id = %q, want per-incident routing param", got.Alert.Slack.ChannelID)
	}
	if got.Alert.Slack.Token != "yaml-slack-token" {
		t.Fatalf("Slack token = %q, want YAML floor", got.Alert.Slack.Token)
	}
	if cfg.Alert.Slack.ChannelID != "C-YAML" {
		t.Fatalf("global cfg mutated: channel id = %q", cfg.Alert.Slack.ChannelID)
	}
}

// mutatingAlertResolver returns a different Slack token on each ResolveAlert
// call, simulating an operator hot-rotating the credential in the store between
// two incidents. It proves the emission path re-reads the resolver PER call
// (read-through, no cached effective config), so a runtime change lands on the
// NEXT incident with no re-registration and no restart.
type mutatingAlertResolver struct{ tokens []string }

func (m *mutatingAlertResolver) ResolveAlert(_ context.Context, base *AlertConfig) bool {
	if len(m.tokens) == 0 {
		return false
	}
	tok := m.tokens[0]
	m.tokens = m.tokens[1:]
	base.Slack.Enable = true
	base.Slack.Token = tok
	return true
}

// TestGetConfigForAlert_ReadThrough_HotReload proves the hot-reload
// semantics at the layer the effective config is resolved: with the SAME
// resolver registered once, a credential change between two resolves is
// observed on the SECOND call (the emission path re-resolves per incident and
// rebuilds providers from the fresh clone — no sender cache, no restart).
func TestGetConfigForAlert_ReadThrough_HotReload(t *testing.T) {
	baseConfig(t)
	SetAlertConfigResolver(&mutatingAlertResolver{tokens: []string{"rotated-1", "rotated-2"}})
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	first := GetConfigForAlert(context.Background(), nil)
	if first.Alert.Slack.Token != "rotated-1" || !first.Alert.Slack.Enable {
		t.Fatalf("first incident Slack = (%q, enable=%v), want rotated-1", first.Alert.Slack.Token, first.Alert.Slack.Enable)
	}
	// No re-registration, no restart — the very next incident picks up the
	// rotated credential because the config is resolved read-through per call.
	second := GetConfigForAlert(context.Background(), nil)
	if second.Alert.Slack.Token != "rotated-2" {
		t.Fatalf("second incident Slack token = %q, want rotated-2 (hot-reload, no restart)", second.Alert.Slack.Token)
	}
	// Each incident got its own clone; the global cfg is never mutated.
	if first == second || cfg.Alert.Slack.Token != "yaml-slack-token" {
		t.Fatalf("expected distinct clones and an untouched global YAML floor; global token=%q", cfg.Alert.Slack.Token)
	}
}
