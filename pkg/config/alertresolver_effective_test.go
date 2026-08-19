package config

import (
	"context"
	"testing"
)

// TestEffectiveAlertConfig_NoResolverIsTheYAMLFloor proves the community path:
// with nothing registered the read-only accessor returns the global Alert
// config as-is, with no clone and no overlay.
func TestEffectiveAlertConfig_NoResolverIsTheYAMLFloor(t *testing.T) {
	SetAlertConfigResolver(nil)
	t.Cleanup(func() { SetAlertConfigResolver(nil) })
	baseConfig(t)

	got := EffectiveAlertConfig(context.Background())
	if got.Slack.Token != cfg.Alert.Slack.Token || got.Slack.Enable != cfg.Alert.Slack.Enable {
		t.Fatalf("Slack = %+v, want the YAML floor %+v", got.Slack, cfg.Alert.Slack)
	}
	if got.Telegram.BotToken != cfg.Alert.Telegram.BotToken {
		t.Fatalf("Telegram bot token = %q, want the YAML floor %q", got.Telegram.BotToken, cfg.Alert.Telegram.BotToken)
	}
}

// TestEffectiveAlertConfig_OverrideWins proves the read-only accessor reports
// what the emission path will actually use, so an admin view cannot go stale
// against a hot-configured channel.
func TestEffectiveAlertConfig_OverrideWins(t *testing.T) {
	baseConfig(t)
	SetAlertConfigResolver(&stubAlertResolver{slackToken: "runtime-slack", applied: true})
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	got := EffectiveAlertConfig(context.Background())
	if got.Slack.Token != "runtime-slack" || !got.Slack.Enable {
		t.Fatalf("Slack = (%q, enable=%v), want the runtime override", got.Slack.Token, got.Slack.Enable)
	}
	// Channels the resolver has no opinion on stay at the YAML floor.
	if got.Telegram.BotToken != cfg.Alert.Telegram.BotToken {
		t.Fatalf("Telegram bot token = %q, want the untouched YAML floor %q", got.Telegram.BotToken, cfg.Alert.Telegram.BotToken)
	}
}

// TestEffectiveAlertConfig_NoOpinionFallsBackToYAML proves a resolver that
// applies nothing leaves the reported config at the floor.
func TestEffectiveAlertConfig_NoOpinionFallsBackToYAML(t *testing.T) {
	baseConfig(t)
	SetAlertConfigResolver(&stubAlertResolver{slackToken: "runtime-slack", applied: false})
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	if got := EffectiveAlertConfig(context.Background()); got.Slack.Token != cfg.Alert.Slack.Token {
		t.Fatalf("Slack token = %q, want the YAML floor %q", got.Slack.Token, cfg.Alert.Slack.Token)
	}
}

// TestEffectiveAlertConfig_PanicFallsBackToYAML proves the read path is
// fail-safe in the same way the emission path is.
func TestEffectiveAlertConfig_PanicFallsBackToYAML(t *testing.T) {
	baseConfig(t)
	SetAlertConfigResolver(&stubAlertResolver{slackToken: "runtime-slack", panics: true})
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	if got := EffectiveAlertConfig(context.Background()); got.Slack.Token != cfg.Alert.Slack.Token {
		t.Fatalf("Slack token = %q, want the YAML floor %q after a panic", got.Slack.Token, cfg.Alert.Slack.Token)
	}
}

// TestEffectiveAlertConfig_NeverMutatesGlobal proves the resolver only ever
// sees a clone: the global config is untouched by a read.
func TestEffectiveAlertConfig_NeverMutatesGlobal(t *testing.T) {
	baseConfig(t)
	before := cfg.Alert.Slack
	SetAlertConfigResolver(&stubAlertResolver{slackToken: "runtime-slack", applied: true})
	t.Cleanup(func() { SetAlertConfigResolver(nil) })

	EffectiveAlertConfig(context.Background())
	if cfg.Alert.Slack.Token != before.Token || cfg.Alert.Slack.Enable != before.Enable {
		t.Fatalf("global Slack config mutated: %+v -> %+v", before, cfg.Alert.Slack)
	}
}
