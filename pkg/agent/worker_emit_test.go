package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// fakeAgent is a minimal core.AIAgent that records calls and returns a
// canned AICallResult. It is enough to drive the worker's detect emit
// path end-to-end without spinning up a real model or HTTP server.
type fakeAgent struct {
	calls   int32
	finding *core.AIFinding
	err     error
}

func (f *fakeAgent) Name() string          { return "fake" }
func (f *fakeAgent) Kind() core.AITaskKind { return core.AITaskDetect }

func (f *fakeAgent) Run(_ context.Context, _ core.AITask) (*core.AICallResult, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}
	return &core.AICallResult{
		Finding:     f.finding,
		UserPrompt:  "u",
		RawResponse: "r",
		Model:       "fake-model",
		DurationMs:  1,
	}, nil
}

type stubSource struct{ name string }

func (s stubSource) Name() string { return s.name }
func (s stubSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	return nil, time.Time{}, nil
}

func newWorkerForTest(t *testing.T, bundle AIBundle, emitter Emitter) *Worker {
	t.Helper()
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	w, err := NewWorker(WorkerOptions{
		Sources: []core.SignalSource{stubSource{name: "test"}},
		Miner:   NewMiner(0.4, 4, 100),
		Catalog: cat,
		AI:      bundle,
		Emitter: emitter,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

// TestWorker_EmitDetect_HappyPath wires a Worker with a fake AIAgent
// and a capturing emitter, then drives one Unknown-verdict pattern
// through emitDetect. It asserts:
//
//   - the agent's Run is called exactly once
//   - the emitter sees the same finding the agent returned
//   - the worker's outcome label is "emitted"
//
// This is the Detect contract: Detect (not the legacy SRE) routes
// through the agent, and findings still land in the emitter unchanged.
func TestWorker_EmitDetect_HappyPath(t *testing.T) {
	finding := &core.AIFinding{
		Title:      "ServiceX null pointer",
		Summary:    "stack on /checkout",
		Severity:   "high",
		Confidence: 0.9,
	}
	agent := &fakeAgent{finding: finding}

	var emitted struct {
		called  int
		finding *core.AIFinding
		source  string
		service string
	}
	emitter := func(f *core.AIFinding, _ core.AgentResult, source, service string) error {
		emitted.called++
		emitted.finding = f
		emitted.source = source
		emitted.service = service
		return nil
	}

	w := newWorkerForTest(t, AIBundle{
		Detect: agent,
		// Cache and Rate intentionally nil — both Get/Put and Allow
		// are nil-safe; Allow returns true when the receiver is nil.
	}, emitter)

	signals := []core.Signal{{Message: "boom", Source: "svc-x"}}
	outcome := w.emitDetect(
		context.Background(),
		"test", "pid-1", "boom",
		"svc-x", signals,
		core.VerdictUnknown,
		0, 0, 0, "",
	)

	if outcome != "emitted" {
		t.Fatalf("outcome = %q, want emitted", outcome)
	}
	if got := atomic.LoadInt32(&agent.calls); got != 1 {
		t.Fatalf("agent.calls = %d, want 1", got)
	}
	if emitted.called != 1 {
		t.Fatalf("emitter called %d times, want 1", emitted.called)
	}
	if emitted.finding == nil || emitted.finding.Title != finding.Title {
		t.Fatalf("emitter got finding %+v, want %+v", emitted.finding, finding)
	}
	if emitted.source != "test" || emitted.service != "svc-x" {
		t.Fatalf("emitter args = (%q,%q), want (test,svc-x)", emitted.source, emitted.service)
	}
}

// TestWorker_EmitDetect_DryWhenNoAgent asserts the worker preserves
// the "dry detect" behaviour when AIBundle.Detect is nil — the bundle
// is allowed to be zero-valued, and emitDetect must not panic / not
// call the emitter.
func TestWorker_EmitDetect_DryWhenNoAgent(t *testing.T) {
	called := 0
	emitter := func(*core.AIFinding, core.AgentResult, string, string) error {
		called++
		return nil
	}

	w := newWorkerForTest(t, AIBundle{}, emitter)

	outcome := w.emitDetect(
		context.Background(),
		"test", "pid-2", "boom", "svc-x",
		[]core.Signal{{Message: "boom"}},
		core.VerdictUnknown,
		0, 0, 0, "",
	)

	if outcome != "dry" {
		t.Fatalf("outcome = %q, want dry", outcome)
	}
	if called != 0 {
		t.Fatalf("emitter called %d times in dry mode, want 0", called)
	}
}

// TestWorker_EmitDetect_HonorsDeclaredSeverityFloor covers the severity-floor case: when an
// incoming Signal carries an operator-declared Severity (e.g. an anomaly rule's
// `severity: critical`), the emitted finding must not be demoted below it even
// when the AI rates it lower. A signal with no declared severity defers to the
// AI verdict.
func TestWorker_EmitDetect_HonorsDeclaredSeverityFloor(t *testing.T) {
	tests := []struct {
		name           string
		signalSeverity string
		aiSeverity     string
		wantSeverity   string
	}{
		{"declared critical floors a medium AI verdict", "critical", "medium", "critical"},
		{"declared high floors a low AI verdict", "high", "low", "high"},
		{"AI may escalate above the declared floor", "high", "critical", "critical"},
		{"empty declared severity defers to AI", "", "medium", "medium"},
		{"non-canonical declared severity is ignored (log path)", "error", "low", "low"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{finding: &core.AIFinding{
				Title:    "t",
				Summary:  "s",
				Severity: tc.aiSeverity,
			}}

			var got *core.AIFinding
			emitter := func(f *core.AIFinding, _ core.AgentResult, _, _ string) error {
				got = f
				return nil
			}

			w := newWorkerForTest(t, AIBundle{Detect: agent}, emitter)

			signals := []core.Signal{{
				Message:  "metric svc/error_rate = 0.44",
				Source:   "demo-prom",
				Severity: tc.signalSeverity,
			}}
			outcome := w.emitDetect(
				context.Background(),
				"demo-prom", "pid-sev", "metric <*> = <*>",
				"svc", signals,
				core.VerdictSpike,
				0, 0, 0, "",
			)

			if outcome != "emitted" {
				t.Fatalf("outcome = %q, want emitted", outcome)
			}
			if got == nil {
				t.Fatal("emitter never called")
			}
			if got.Severity != tc.wantSeverity {
				t.Fatalf("emitted severity = %q, want %q", got.Severity, tc.wantSeverity)
			}
		})
	}
}
