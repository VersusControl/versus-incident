package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// newPreFilterWorker wires a worker whose single source binds to a fake
// enterprise-style brain — it groups by its own fixed observation and applies
// NO text filter, exactly like the licensed metric/trace brain. The brain is
// registered for an arbitrary metrics/traces-kind type so the source takes the
// kind-proper dispatch path (never the log brain). It returns the worker, the
// fake brain (so a test can inspect which signals actually reached Group after
// the worker's per-kind pre-filter), and the source to load signals into.
//
// kindType must be unique per call: signalsources.RegisterKind panics on a
// duplicate registration.
func newPreFilterWorker(t *testing.T, kindType string, kind signalsources.Kind, regex config.AgentRegexConfig) (*Worker, *fakeBrain, *batchSource) {
	t.Helper()
	signalsources.RegisterKind(kindType, kind)
	fb := &fakeBrain{
		kind:    string(kind),
		grouped: []core.Observation{{Key: "k", Service: "svc", Signal: "s", Frequency: 1}},
		// Known + confident → suppressed before any emit, so the test needs no
		// AI bundle or emitter; it asserts only on what the brain received.
		verdict: core.TypedVerdict{Class: core.VerdictKnownPattern, Confident: true},
	}
	RegisterTypedBrain(kindType, func(string, map[string]any) (core.SignalLearner, core.SignalDetector, error) {
		return fb, fb, nil
	})
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	src := &batchSource{name: "src-1"}
	w, err := NewWorker(WorkerOptions{
		Cfg: config.AgentConfig{
			Mode:    "detect",
			Regex:   regex,
			Sources: []config.AgentSourceConfig{{Name: "src-1", Type: kindType, Enable: true}},
		},
		Sources: []core.SignalSource{src},
		Miner:   NewMiner(0.4, 4, 100),
		Catalog: cat,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w, fb, src
}

// TestWorker_PerKindOverride_FiltersMetricsBeforeBrain is the regression
// guard: with agent.regex.metrics set, a metric signal whose message does NOT
// match is dropped BEFORE the enterprise-style brain's Group is called, while a
// matching one flows. This proves the per-kind override bites on the
// kind-proper (enterprise-brain) dispatch path, not just the log-brain fallback.
func TestWorker_PerKindOverride_FiltersMetricsBeforeBrain(t *testing.T) {
	w, fb, src := newPreFilterWorker(t, "test-prefilter-metrics", signalsources.KindMetrics,
		config.AgentRegexConfig{Metrics: "(?i)error_rate"})
	src.signals = []core.Signal{
		{Message: "metric svc/error_rate = 5.1"},
		{Message: "metric svc/request_rate = 120"},
		{Message: "metric svc/error_rate = 6.2"},
		{Message: "metric svc/request_rate = 130"},
	}

	w.tickSource(context.Background(), src, "detect")

	if len(fb.received) != 2 {
		t.Fatalf("brain received %d signals, want 2 (only error_rate)", len(fb.received))
	}
	for _, s := range fb.received {
		if !strings.Contains(s.Message, "error_rate") {
			t.Errorf("brain received a non-matching signal: %q", s.Message)
		}
	}
}

// TestWorker_PerKindOverride_FiltersTracesBeforeBrain proves the same narrowing
// for the traces kind via agent.regex.traces.
func TestWorker_PerKindOverride_FiltersTracesBeforeBrain(t *testing.T) {
	w, fb, src := newPreFilterWorker(t, "test-prefilter-traces", signalsources.KindTraces,
		config.AgentRegexConfig{Traces: "(?i)timeout"})
	src.signals = []core.Signal{
		{Message: "trace svc/checkout timeout span"},
		{Message: "trace svc/checkout ok span"},
		{Message: "trace svc/cart timeout span"},
	}

	w.tickSource(context.Background(), src, "detect")

	if len(fb.received) != 2 {
		t.Fatalf("brain received %d signals, want 2 (only timeout)", len(fb.received))
	}
	for _, s := range fb.received {
		if !strings.Contains(s.Message, "timeout") {
			t.Errorf("brain received a non-matching signal: %q", s.Message)
		}
	}
}

// TestWorker_PerKindOverride_EmptyFlowsAll is the default-behaviour guard: with
// NO per-kind override set, every metric signal reaches the brain (learn-all),
// exactly as today. This is the zero-config flow QA verified passing.
func TestWorker_PerKindOverride_EmptyFlowsAll(t *testing.T) {
	w, fb, src := newPreFilterWorker(t, "test-prefilter-metrics-all", signalsources.KindMetrics,
		config.AgentRegexConfig{})
	src.signals = []core.Signal{
		{Message: "metric svc/error_rate = 5.1"},
		{Message: "metric svc/request_rate = 120"},
		{Message: "metric svc/latency_p99 = 42"},
	}

	w.tickSource(context.Background(), src, "detect")

	if len(fb.received) != 3 {
		t.Fatalf("brain received %d signals, want 3 (all flow with no override)", len(fb.received))
	}
}
