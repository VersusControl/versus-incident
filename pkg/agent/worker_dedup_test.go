package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// TestWorker_EmitDedup_SuppressesRepeat drives the same (service, pattern)
// through emitDetect twice and asserts the second emit is suppressed
// ("deduped", emitter not called again) — the core fix for the per-tick
// re-emit/re-notify loop. A distinct pattern is unaffected.
func TestWorker_EmitDedup_SuppressesRepeat(t *testing.T) {
	finding := &core.AIFinding{Title: "x", Severity: "high", Confidence: 0.9}
	ag := &fakeAgent{finding: finding}
	var emitted int
	emitter := func(_ *core.AIFinding, _ core.AgentResult, _, _ string) error {
		emitted++
		return nil
	}
	w := newWorkerForTest(t, AIBundle{Detect: ag}, emitter)

	sig := []core.Signal{{Message: "boom"}}
	o1 := w.emitDetect(context.Background(), "test", "pid-1", "boom", "svc-x", sig, core.VerdictUnknown, 0)
	o2 := w.emitDetect(context.Background(), "test", "pid-1", "boom", "svc-x", sig, core.VerdictUnknown, 0)

	if o1 != "emitted" {
		t.Errorf("first outcome = %q, want emitted", o1)
	}
	if o2 != "deduped" {
		t.Errorf("second outcome = %q, want deduped", o2)
	}
	if emitted != 1 {
		t.Errorf("emitter called %d times, want 1 (dedup must suppress the repeat)", emitted)
	}

	if o3 := w.emitDetect(context.Background(), "test", "pid-2", "boom", "svc-x", sig, core.VerdictUnknown, 0); o3 != "emitted" {
		t.Errorf("distinct pattern outcome = %q, want emitted", o3)
	}
}

// TestWorker_EmitDedup_FailedSendReleasesWindow asserts a failed send does
// not consume the dedup window: the next tick retries instead of being
// silently suppressed.
func TestWorker_EmitDedup_FailedSendReleasesWindow(t *testing.T) {
	ag := &fakeAgent{finding: &core.AIFinding{Title: "x", Severity: "high"}}
	var calls int
	emitter := func(_ *core.AIFinding, _ core.AgentResult, _, _ string) error {
		calls++
		return errors.New("telegram down")
	}
	w := newWorkerForTest(t, AIBundle{Detect: ag}, emitter)
	sig := []core.Signal{{Message: "boom"}}

	o1 := w.emitDetect(context.Background(), "test", "pid-1", "boom", "svc-x", sig, core.VerdictUnknown, 0)
	o2 := w.emitDetect(context.Background(), "test", "pid-1", "boom", "svc-x", sig, core.VerdictUnknown, 0)
	if o1 != "send_error" || o2 != "send_error" {
		t.Errorf("outcomes = %q,%q; want send_error,send_error (failed send must not consume the window)", o1, o2)
	}
	if calls != 2 {
		t.Errorf("emitter called %d times, want 2 (retry after release)", calls)
	}
}
