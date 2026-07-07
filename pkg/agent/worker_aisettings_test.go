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

// TestEmitDetect_ResolverDisabled_RunsDry proves the runtime enable gate:
// a fake resolver that returns enabled=false drives the detect path to the
// dry branch — the AI agent is never called and nothing is emitted — even
// though AIBundle.Detect is wired.
func TestEmitDetect_ResolverDisabled_RunsDry(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	finding := &core.AIFinding{Title: "boom", Severity: "high", Confidence: 0.9}
	agent := &fakeAgent{finding: finding}

	emitted := 0
	emitter := func(*core.AIFinding, core.AgentResult, string, string) error {
		emitted++
		return nil
	}
	w := newWorkerForTest(t, AIBundle{Detect: agent}, emitter)

	// enabled=false, ok=true -> runtime says "AI off" -> dry.
	SetAISettingsResolver(&stubAISettings{enabled: false, enabledOK: true})

	outcome := w.emitDetect(
		context.Background(),
		"test", "pid-off", "boom", "svc-x",
		[]core.Signal{{Message: "boom"}},
		core.VerdictUnknown, 0, 0, 0, "",
	)

	if outcome != "dry" {
		t.Fatalf("outcome = %q, want dry when resolver disables AI", outcome)
	}
	if got := atomic.LoadInt32(&agent.calls); got != 0 {
		t.Fatalf("agent.calls = %d, want 0 (no AI call in dry)", got)
	}
	if emitted != 0 {
		t.Fatalf("emitter called %d times, want 0 in dry", emitted)
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
				"test", "pid-on", "boom", "svc-x",
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
