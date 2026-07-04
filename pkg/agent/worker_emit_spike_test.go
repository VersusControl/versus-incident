package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// newSpikeWorker builds a Worker with the deterministic emit-on-spike mode
// configured and NO AI agent (AIBundle{} => Detect == nil), plus a capturing
// emitter — the exact setup for the no-LLM alerting path.
func newSpikeWorker(t *testing.T, emitOnSpike bool, emitter Emitter) *Worker {
	t.Helper()
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	w, err := NewWorker(WorkerOptions{
		Sources: []core.SignalSource{stubSource{name: "test"}},
		Miner:   NewMiner(0.4, 4, 100),
		Catalog: cat,
		AI:      AIBundle{}, // Detect == nil => AI off
		Emitter: emitter,
		Cfg:     config.AgentConfig{Catalog: config.AgentCatalogConfig{EmitOnSpike: emitOnSpike}},
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

// A VerdictSpike with AI off + emit_on_spike=true emits a deterministic
// incident (no model call).
func TestWorker_EmitOnSpike_EmitsWithoutAI(t *testing.T) {
	var got struct {
		n int
		f *core.AIFinding
	}
	emitter := func(f *core.AIFinding, _ core.AgentResult, _, _ string) error {
		got.n++
		got.f = f
		return nil
	}
	w := newSpikeWorker(t, true, emitter)

	signals := []core.Signal{{Message: "flood", Source: "svc"}, {Message: "flood", Source: "svc"}}
	outcome := w.emitDetect(context.Background(), "test", "pid", "flood", "svc", signals, core.VerdictSpike, 1.0)

	if outcome != "emitted-stat" {
		t.Fatalf("outcome = %q, want emitted-stat", outcome)
	}
	if got.n != 1 {
		t.Fatalf("emitter called %d times, want 1", got.n)
	}
	if got.f == nil || !strings.Contains(got.f.Title, "Frequency spike") {
		t.Fatalf("finding = %+v, want a Frequency spike title", got.f)
	}
	if !strings.Contains(got.f.Summary, "no LLM") {
		t.Fatalf("summary should note the no-LLM path, got %q", got.f.Summary)
	}
}

// With emit_on_spike=false (default) a spike + no AI stays dry — behaviour
// preserved.
func TestWorker_EmitOnSpike_DisabledStaysDry(t *testing.T) {
	called := 0
	emitter := func(*core.AIFinding, core.AgentResult, string, string) error { called++; return nil }
	w := newSpikeWorker(t, false, emitter)

	out := w.emitDetect(context.Background(), "t", "p", "x", "s",
		[]core.Signal{{Message: "x"}}, core.VerdictSpike, 1.0)
	if out != "dry" || called != 0 {
		t.Fatalf("out=%q called=%d, want dry / 0", out, called)
	}
}

// Even with emit_on_spike=true, a non-spike verdict (Unknown) stays dry —
// only known-pattern spikes speak. Silence is the feature.
func TestWorker_EmitOnSpike_OnlySpikesEmit(t *testing.T) {
	called := 0
	emitter := func(*core.AIFinding, core.AgentResult, string, string) error { called++; return nil }
	w := newSpikeWorker(t, true, emitter)

	out := w.emitDetect(context.Background(), "t", "p", "x", "s",
		[]core.Signal{{Message: "x"}}, core.VerdictUnknown, 0)
	if out != "dry" || called != 0 {
		t.Fatalf("out=%q called=%d, want dry / 0", out, called)
	}
}
