package agent

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// batchSource returns a fixed batch on every Pull — enough to drive tickSource
// through the seam without a real backend.
type batchSource struct {
	name    string
	signals []core.Signal
}

func (s *batchSource) Name() string { return s.name }
func (s *batchSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	return s.signals, time.Now().UTC(), nil
}

// repeatSignals builds n signals that share a template (only a trailing integer
// varies) so the miner clusters them into a single pattern with frequency n.
func repeatSignals(prefix string, n int) []core.Signal {
	out := make([]core.Signal, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, core.Signal{Message: prefix + strconv.Itoa(i)})
	}
	return out
}

func newSeamWorker(t *testing.T, mode string, src core.SignalSource, bundle AIBundle, emitter Emitter) *Worker {
	t.Helper()
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	m, errs := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	if len(errs) > 0 {
		t.Fatalf("NewRegexMatcher: %v", errs)
	}
	svc, errs := NewServiceMatcher([]string{`service=(\w+)`})
	if len(errs) > 0 {
		t.Fatalf("NewServiceMatcher: %v", errs)
	}
	w, err := NewWorker(WorkerOptions{
		Cfg: config.AgentConfig{
			Mode: mode,
			Catalog: config.AgentCatalogConfig{
				AutoPromoteAfter:      100,
				SpikeMultiplier:       5,
				SpikeMinFrequency:     5,
				SpikeMinBaselineCount: 20,
			},
		},
		Sources:  []core.SignalSource{src},
		Matcher:  m,
		Miner:    NewMiner(0.4, 4, 100),
		Catalog:  cat,
		Services: svc,
		AI:       bundle,
		Emitter:  emitter,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

// TestWorker_Seam_TrainingLearnsNoEmit proves a training tick drives the seam:
// the log brain groups + folds the batch into the catalog, and nothing is
// surfaced (no classification, no emit).
func TestWorker_Seam_TrainingLearnsNoEmit(t *testing.T) {
	src := &batchSource{name: "es", signals: repeatSignals("service=api oops id=", 5)}
	emit := 0
	w := newSeamWorker(t, "training", src, AIBundle{}, func(*core.AIFinding, core.AgentResult, string, string) error {
		emit++
		return nil
	})

	w.tickSource(context.Background(), src, "training")

	if w.catalog.Len() == 0 {
		t.Fatal("training tick learned no pattern")
	}
	if emit != 0 {
		t.Fatalf("training emitted %d times, want 0", emit)
	}
}

// TestWorker_Seam_DetectEmitsUnknown proves a brand-new pattern in detect mode
// flows Group → Expected → Learn → Classify(Unknown) → emitDetect → emitter.
func TestWorker_Seam_DetectEmitsUnknown(t *testing.T) {
	src := &batchSource{name: "es", signals: repeatSignals("service=api kaboom id=", 5)}
	agent := &fakeAgent{finding: &core.AIFinding{Title: "boom", Severity: "high", Confidence: 0.9}}
	emit := 0
	w := newSeamWorker(t, "detect", src, AIBundle{Detect: agent}, func(*core.AIFinding, core.AgentResult, string, string) error {
		emit++
		return nil
	})

	w.tickSource(context.Background(), src, "detect")

	if emit != 1 {
		t.Fatalf("detect emitted %d times, want 1 (one unknown pattern)", emit)
	}
}

// TestWorker_Seam_DetectSuppressesKnownPattern proves a pattern trained past the
// auto-promote threshold is classified Known and suppressed on a later detect
// tick — the byte-for-byte log behaviour the refactor must preserve.
func TestWorker_Seam_DetectSuppressesKnownPattern(t *testing.T) {
	src := &batchSource{name: "es", signals: repeatSignals("service=api steady id=", 5)}
	agent := &fakeAgent{finding: &core.AIFinding{Title: "x", Severity: "low", Confidence: 0.5}}
	emit := 0
	w := newSeamWorker(t, "detect", src, AIBundle{Detect: agent}, func(*core.AIFinding, core.AgentResult, string, string) error {
		emit++
		return nil
	})

	// 21 training ticks × 5 signals = 105 ≥ auto_promote_after(100), without
	// ever classifying/emitting during accumulation.
	for i := 0; i < 21; i++ {
		w.tickSource(context.Background(), src, "training")
	}
	if w.catalog.Len() == 0 {
		t.Fatal("no pattern learned during training")
	}

	// A steady detect tick now: the pattern is known and not spiking → suppressed.
	w.tickSource(context.Background(), src, "detect")
	if emit != 0 {
		t.Fatalf("known steady pattern emitted %d times, want 0", emit)
	}
}

// TestWorker_Seam_UsesRegisteredBrain proves the worker prefers a registered
// per-type brain over the default log brain for a matching configured source.
func TestWorker_Seam_UsesRegisteredBrain(t *testing.T) {
	const typ = "test-seam-brain"
	fb := &fakeBrain{
		kind:    "metrics",
		grouped: []core.Observation{{Key: "k", Service: "svc", Signal: "latency_p99", Frequency: 1}},
		verdict: core.TypedVerdict{Class: core.VerdictUnknown, Confident: true},
	}
	RegisterTypedBrain(typ, func(string, map[string]any) (core.SignalLearner, core.SignalDetector, error) {
		return fb, fb, nil
	})

	cat, _ := LoadCatalog(storage.NewMemory())
	src := &batchSource{name: "metrics-1", signals: []core.Signal{{Message: "ignored by fake brain"}}}
	agent := &fakeAgent{finding: &core.AIFinding{Title: "t", Severity: "high", Confidence: 0.9}}
	emit := 0
	w, err := NewWorker(WorkerOptions{
		Cfg: config.AgentConfig{
			Mode: "detect",
			Sources: []config.AgentSourceConfig{
				{Name: "metrics-1", Type: typ, Enable: true},
			},
		},
		Sources: []core.SignalSource{src},
		Miner:   NewMiner(0.4, 4, 100),
		Catalog: cat,
		AI:      AIBundle{Detect: agent},
		Emitter: func(*core.AIFinding, core.AgentResult, string, string) error { emit++; return nil },
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	w.tickSource(context.Background(), src, "detect")

	if fb.learned == 0 {
		t.Fatal("registered brain's Learn was never called — worker did not select it")
	}
	if emit != 1 {
		t.Fatalf("emitted %d times, want 1 (fake brain returned one Unknown)", emit)
	}
}
