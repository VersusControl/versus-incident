package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// TestEffectiveAIEnabled_NoResolver_OSSUnchanged proves that with no
// resolver registered and the static AI flag off, effectiveAIEnabled stays
// true: whether a detect agent exists at all is decided at construction.
func TestEffectiveAIEnabled_NoResolver_OSSUnchanged(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	w := newWorkerForTest(t, AIBundle{}, nil)
	for tick := 0; tick < 3; tick++ {
		if !w.effectiveAIEnabled(context.Background()) {
			t.Fatalf("tick=%d: effectiveAIEnabled=false, want true (OSS)", tick)
		}
	}
}

// TestEmitDetect_ResolverDisabled_EmitsTemplated proves the runtime enable
// gate: a fake resolver that returns enabled=false drives the detect path to
// the AI-off branch — the AI agent is never called — but the detection is NOT
// dropped. The worker emits a deterministic templated alert exactly once, even
// though AIBundle.Detect is wired.
func TestEmitDetect_ResolverDisabled_EmitsTemplated(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	finding := &core.AIFinding{Title: "boom", Severity: "high", Confidence: 0.9}
	agent := &fakeAgent{finding: finding}

	emitted := 0
	var got *core.AIFinding
	emitter := func(f *core.AIFinding, _ core.AgentResult, _, _ string) error {
		emitted++
		got = f
		return nil
	}
	w := newWorkerForTest(t, AIBundle{Detect: agent}, emitter)

	// enabled=false, ok=true -> runtime says "AI off" -> templated alert.
	SetAISettingsResolver(&stubAISettings{enabled: false, enabledOK: true})

	outcome := w.emitDetect(
		context.Background(),
		"test", "logs", "pid-off", "boom", "svc-x", 1,
		[]core.Signal{{Message: "boom"}},
		core.VerdictUnknown, 0, 0, 0, "",
	)

	if outcome != "emitted_basic" {
		t.Fatalf("outcome = %q, want emitted_basic when resolver disables AI", outcome)
	}
	if calls := atomic.LoadInt32(&agent.calls); calls != 0 {
		t.Fatalf("agent.calls = %d, want 0 (no AI call when disabled)", calls)
	}
	if emitted != 1 {
		t.Fatalf("emitter called %d times, want 1 (templated alert)", emitted)
	}
	if got == nil || got.Title == finding.Title {
		t.Fatalf("emitted finding should be the deterministic one, not the AI stub: %+v", got)
	}
}

// TestEmitDetect_ResolverEnabled_RunsAI proves enabled=true keeps the real
// detect call, and ok=false (no opinion) runs it when the static config has
// AI enabled with a key.
func TestEmitDetect_ResolverEnabled_RunsAI(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	cases := map[string]struct {
		resolver *stubAISettings
		static   config.AgentAIConfig
	}{
		"enabled_true":              {resolver: &stubAISettings{enabled: true, enabledOK: true}},
		"no_opinion_static_on":      {resolver: &stubAISettings{enabledOK: false}, static: config.AgentAIConfig{Enable: true, APIKey: "sk-static"}},
		"no_opinion_static_keyless": {resolver: &stubAISettings{enabledOK: false}, static: config.AgentAIConfig{Enable: true, Provider: "ollama"}},
	}
	for name, tc := range cases {
		resolver := tc.resolver
		t.Run(name, func(t *testing.T) {
			finding := &core.AIFinding{Title: "boom", Severity: "high", Confidence: 0.9}
			agent := &fakeAgent{finding: finding}
			emitted := 0
			emitter := func(*core.AIFinding, core.AgentResult, string, string) error {
				emitted++
				return nil
			}
			w := newWorkerForTest(t, AIBundle{Detect: agent}, emitter)
			w.cfg.AI = tc.static
			SetAISettingsResolver(resolver)

			outcome := w.emitDetect(
				context.Background(),
				"test", "logs", "pid-on", "boom", "svc-x", 1,
				[]core.Signal{{Message: "boom"}},
				core.VerdictUnknown, 0, 0, 0, "",
			)

			if outcome != "emitted" {
				t.Fatalf("outcome = %q, want emitted", outcome)
			}
			if got := atomic.LoadInt32(&agent.calls); got != 1 {
				t.Fatalf("agent.calls = %d, want 1", got)
			}
			if emitted != 1 {
				t.Fatalf("emitter called %d times, want 1", emitted)
			}
		})
	}
}

// TestEmitDetect_ResolverNoAnswer_StaticOffSkipsAI proves that when the
// runtime resolver has no stored settings, a static config with AI disabled
// or without a credential makes no AI call and emits the templated alert.
func TestEmitDetect_ResolverNoAnswer_StaticOffSkipsAI(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	cases := map[string]config.AgentAIConfig{
		"ai_disabled":        {Enable: false, APIKey: "sk-stale", Model: "gpt-4o-mini"},
		"enabled_but_no_key": {Enable: true, Provider: "openai", Model: "gpt-4o-mini"},
	}
	for name, static := range cases {
		t.Run(name, func(t *testing.T) {
			agent := &fakeAgent{finding: &core.AIFinding{Title: "boom", Severity: "high", Confidence: 0.9}}
			emitted := 0
			emitter := func(*core.AIFinding, core.AgentResult, string, string) error {
				emitted++
				return nil
			}
			w := newWorkerForTest(t, AIBundle{Detect: agent}, emitter)
			w.cfg.AI = static
			SetAISettingsResolver(&stubAISettings{enabledOK: false})

			outcome := w.emitDetect(
				context.Background(),
				"test", "metrics", "pid-static-off", "boom", "svc-x", 1,
				[]core.Signal{{Message: "boom"}},
				core.VerdictUnknown, 0, 0, 0, "",
			)

			if outcome != "emitted_basic" {
				t.Fatalf("outcome = %q, want emitted_basic", outcome)
			}
			if calls := atomic.LoadInt32(&agent.calls); calls != 0 {
				t.Fatalf("agent.calls = %d, want 0 (no AI call when static config has AI off)", calls)
			}
			if emitted != 1 {
				t.Fatalf("emitter called %d times, want 1", emitted)
			}
		})
	}
}

// With no resolver (OSS), AI enabled for a provider that needs a key but with
// no key and no base_url is off: the detect path emits the templated alert and
// never sends incident data to the provider unauthenticated. A key, a keyless
// provider or a configured base_url turns it back on.
func TestEffectiveAIEnabled_NoResolverRequiresUsableCredential(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	cases := []struct {
		name string
		ai   config.AgentAIConfig
		want bool
	}{
		{"enabled without a key", config.AgentAIConfig{Enable: true, Provider: "openai"}, false},
		{"enabled with a blank key", config.AgentAIConfig{Enable: true, Provider: "openai", APIKey: "  "}, false},
		{"enabled with a key", config.AgentAIConfig{Enable: true, Provider: "openai", APIKey: "sk-test"}, true},
		{"keyless provider", config.AgentAIConfig{Enable: true, Provider: "ollama"}, true},
		{"configured base_url", config.AgentAIConfig{Enable: true, Provider: "openai", BaseURL: "http://llm.internal:8080/v1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveAIEnabled(context.Background(), tc.ai); got != tc.want {
				t.Fatalf("EffectiveAIEnabled = %v, want %v", got, tc.want)
			}
		})
	}

	finding := &core.AIFinding{Title: "boom", Severity: "high", Confidence: 0.9}
	agent := &fakeAgent{finding: finding}
	emitted := 0
	w := newWorkerForTest(t, AIBundle{Detect: agent}, func(*core.AIFinding, core.AgentResult, string, string) error {
		emitted++
		return nil
	})
	w.cfg.AI = config.AgentAIConfig{Enable: true, Provider: "openai"}
	outcome := w.emitDetect(
		context.Background(),
		"test", "logs", "pid-nokey", "boom", "svc-x", 1,
		[]core.Signal{{Message: "boom"}},
		core.VerdictUnknown, 0, 0, 0, "",
	)
	if outcome != "emitted_basic" || atomic.LoadInt32(&agent.calls) != 0 || emitted != 1 {
		t.Fatalf("outcome = %q, agent calls = %d, emitted = %d; want the templated alert and no model call", outcome, atomic.LoadInt32(&agent.calls), emitted)
	}
}
