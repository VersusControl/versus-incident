package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// TestEffectiveAIEnabled_NoResolver_OSSUnchanged proves that with no
// resolver registered effectiveAIEnabled is always true — the worker runs
// the real detect call exactly as today.
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
// detect call, and ok=false (no opinion) also keeps today's behaviour.
func TestEmitDetect_ResolverEnabled_RunsAI(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	cases := map[string]*stubAISettings{
		"enabled_true": {enabled: true, enabledOK: true},
		"no_opinion":   {enabled: false, enabledOK: false},
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			finding := &core.AIFinding{Title: "boom", Severity: "high", Confidence: 0.9}
			agent := &fakeAgent{finding: finding}
			emitted := 0
			emitter := func(*core.AIFinding, core.AgentResult, string, string) error {
				emitted++
				return nil
			}
			w := newWorkerForTest(t, AIBundle{Detect: agent}, emitter)
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
