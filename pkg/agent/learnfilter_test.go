package agent

import (
	"bytes"
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// fakeLearnExclusion drops a signal when its service OR its signal name is in
// the configured deny sets. It is concurrency-safe so a tick goroutine can call
// it without a race.
type fakeLearnExclusion struct {
	services map[string]bool
	signals  map[string]bool

	mu    sync.Mutex
	calls int
}

func (f *fakeLearnExclusion) ExcludeFromLearning(_ context.Context, service, signal string) bool {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.services[service] || f.signals[signal]
}

// installExclusion sets the process-wide slot for one test and restores nil.
func installExclusion(t *testing.T, x LearnExclusion) {
	t.Helper()
	SetLearnExclusion(x)
	t.Cleanup(func() { SetLearnExclusion(nil) })
}

// captureAgentLog redirects the standard logger into a buffer for the duration
// of a test so the tick stat line can be asserted.
func captureAgentLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf
}

// TestSetLearnExclusion_SlotRoundTrip covers the exported seam: nil by default,
// last-wins on install, and nil again after clearing.
func TestSetLearnExclusion_SlotRoundTrip(t *testing.T) {
	if learnExclusion() != nil {
		t.Fatal("learn-exclusion slot must be nil by default (OSS)")
	}
	a := &fakeLearnExclusion{}
	b := &fakeLearnExclusion{}
	SetLearnExclusion(a)
	if learnExclusion() != a {
		t.Fatal("install did not take effect")
	}
	SetLearnExclusion(b) // last-wins
	if learnExclusion() != b {
		t.Fatal("second install did not replace the first (last-wins)")
	}
	SetLearnExclusion(nil)
	if learnExclusion() != nil {
		t.Fatal("clearing the slot did not restore nil")
	}
}

// TestLearnExclusion_NilSlotUnchanged is the OSS golden guard (X30-T4 a): with
// no policy installed the tick is byte-for-byte unchanged — every signal is
// learned and the stat reports skipped_excluded=0.
func TestLearnExclusion_NilSlotUnchanged(t *testing.T) {
	if learnExclusion() != nil {
		t.Fatal("precondition: slot must be nil")
	}
	buf := captureAgentLog(t)

	src := &batchSource{name: "es", signals: repeatSignals("service=api steady id=", 5)}
	w := newSeamWorker(t, "training", src, AIBundle{}, nil)
	w.tickSource(context.Background(), src, "training")

	if w.catalog.Len() != 1 {
		t.Fatalf("catalog has %d patterns, want 1 (nil seam must not drop anything)", w.catalog.Len())
	}
	for _, p := range w.catalog.All() {
		if p.Count != 5 {
			t.Fatalf("pattern Count = %d, want 5 (full batch learned)", p.Count)
		}
	}
	if !strings.Contains(buf.String(), "skipped_excluded=0") {
		t.Fatalf("tick log missing skipped_excluded=0; got:\n%s", buf.String())
	}
}

// TestLearnExclusion_DropsBeforeGroup_AllModes is the core X30-T4 (b) proof: an
// installed exclusion drops an excluded SERVICE and an excluded METRIC signal
// BEFORE the brain's Group is called — verified identically in training, shadow
// AND detect (the mode switch is downstream of Group). Because the excluded
// signals never reach Group, they never reach the brain's miner/catalog/baseline.
func TestLearnExclusion_DropsBeforeGroup_AllModes(t *testing.T) {
	w, fb, src := newPreFilterWorker(t, "test-learnfilter-modes", signalsources.KindMetrics,
		config.AgentRegexConfig{}) // no per-kind override; only the exclusion filters
	installExclusion(t, &fakeLearnExclusion{
		services: map[string]bool{"payments": true}, // exclude this SERVICE
		signals:  map[string]bool{"cpu_util": true}, // exclude this METRIC signal
	})

	src.signals = []core.Signal{
		{Message: "keep me", Fields: map[string]interface{}{core.FieldService: "api", core.FieldSignal: "ok"}},
		{Message: "boom", Fields: map[string]interface{}{core.FieldService: "payments", core.FieldSignal: "errors"}},
		{Message: "metric", Fields: map[string]interface{}{core.FieldService: "infra", core.FieldSignal: "cpu_util"}},
	}

	for _, mode := range []string{"training", "shadow", "detect"} {
		fb.received = nil
		w.tickSource(context.Background(), src, mode)

		if len(fb.received) != 1 {
			t.Fatalf("[%s] brain received %d signals, want 1 (kept only)", mode, len(fb.received))
		}
		if got, _ := fb.received[0].Fields[core.FieldService].(string); got != "api" {
			t.Errorf("[%s] brain received service=%q, want api (excluded signals leaked into Group)", mode, got)
		}
		for _, s := range fb.received {
			svc, _ := s.Fields[core.FieldService].(string)
			sig, _ := s.Fields[core.FieldSignal].(string)
			if svc == "payments" || sig == "cpu_util" {
				t.Errorf("[%s] excluded signal reached Group: service=%q signal=%q", mode, svc, sig)
			}
		}
	}
}

// TestLearnExclusion_ExcludedNeverReachesCatalog proves the same exclusion on
// the real OSS log brain: an excluded service's pattern is never folded into the
// catalog (the log brain's miner/catalog), while the kept service's pattern is.
func TestLearnExclusion_ExcludedNeverReachesCatalog(t *testing.T) {
	installExclusion(t, &fakeLearnExclusion{services: map[string]bool{"payments": true}})

	src := &batchSource{name: "es"}
	src.signals = append(repeatSignals("service=api ok id=", 4), repeatSignals("service=payments boom id=", 4)...)
	w := newSeamWorker(t, "training", src, AIBundle{}, nil)

	w.tickSource(context.Background(), src, "training")

	for _, p := range w.catalog.All() {
		if p.Service == "payments" || strings.Contains(p.Template, "boom") {
			t.Fatalf("excluded service reached the catalog: %+v", p)
		}
	}
	if w.catalog.Len() == 0 {
		t.Fatal("kept service produced no pattern; exclusion dropped too much")
	}
}

// TestLearnExclusion_TickStatReportsExcluded is X30-T4 (c): the tick stat line
// reports the excluded count.
func TestLearnExclusion_TickStatReportsExcluded(t *testing.T) {
	installExclusion(t, &fakeLearnExclusion{services: map[string]bool{"payments": true}})
	buf := captureAgentLog(t)

	src := &batchSource{name: "es"}
	// 4 kept (service=api) + 2 excluded (service=payments) → skipped_excluded=2.
	src.signals = append(repeatSignals("service=api ok id=", 4), repeatSignals("service=payments boom id=", 2)...)
	w := newSeamWorker(t, "training", src, AIBundle{}, nil)

	w.tickSource(context.Background(), src, "training")

	if !strings.Contains(buf.String(), "skipped_excluded=2") {
		t.Fatalf("tick log missing skipped_excluded=2; got:\n%s", buf.String())
	}
}

// repeatTagged builds n signals sharing one clustered template (only a trailing
// integer varies, so the log brain's miner folds them into a single Unknown
// pattern) and carrying the given service / signal Fields, so the learn-exclusion
// chokepoint can match them by SERVICE or by METRIC-signal name. An empty
// service/signal is simply omitted from Fields.
func repeatTagged(prefix, service, signal string, n int) []core.Signal {
	out := make([]core.Signal, 0, n)
	for i := 0; i < n; i++ {
		f := map[string]interface{}{}
		if service != "" {
			f[core.FieldService] = service
		}
		if signal != "" {
			f[core.FieldSignal] = signal
		}
		out = append(out, core.Signal{Message: prefix + strconv.Itoa(i), Fields: f})
	}
	return out
}

// newModeOutputWorker wires a worker on the REAL OSS log brain with a shadow
// log AND a capturing emitter, so a test can assert the OUTPUT of the shadow and
// detect tails (would-alert records / emitted incidents), not just what reached
// the brain. mode drives the tick; bundle/emitter drive the detect path.
func newModeOutputWorker(t *testing.T, mode string, src core.SignalSource, bundle AIBundle, emitter Emitter) (*Worker, *ShadowLog) {
	t.Helper()
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	sh, err := LoadShadowLog(storage.NewMemory(), 0)
	if err != nil {
		t.Fatalf("LoadShadowLog: %v", err)
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
		Shadow:   sh,
		Services: svc,
		AI:       bundle,
		Emitter:  emitter,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w, sh
}

// TestLearnExclusion_ExcludedProducesNoOutput_ShadowAndDetect is the founder's
// "invisible in ALL modes" proof (X30). The learn-time drop test above asserts
// the excluded signal never reaches the brain's Group; this one goes one step
// further and asserts the OUTPUT of the downstream tails: an excluded SERVICE
// and an excluded METRIC produce ZERO shadow events in shadow mode and ZERO
// emitted incidents in detect mode — not merely zero learned baselines.
//
// The control sub-cases run the SAME would-surface batch with NO exclusion
// installed and prove it DOES record a shadow event / DOES emit an incident, so
// the four zeros above are real suppression rather than an inert harness.
func TestLearnExclusion_ExcludedProducesNoOutput_ShadowAndDetect(t *testing.T) {
	// First-seen patterns the OSS log brain classifies Unknown+confident, so in
	// shadow each WOULD record a "would alert" event and in detect each WOULD
	// call the AI and emit an incident — unless the chokepoint drops it first.
	excludedService := repeatTagged("service=payments boom id=", "payments", "", 5)
	excludedMetric := repeatTagged("metric infra spike id=", "infra", "cpu_util", 5)
	keptService := repeatTagged("service=api kaboom id=", "api", "", 5)

	// A fresh detect bundle per sub-case: the fake AI returns one Unknown
	// finding, the emitter counts incidents. Built per call so the result cache
	// never carries a finding across sub-cases.
	newAgentBundle := func() AIBundle {
		return AIBundle{Detect: &fakeAgent{finding: &core.AIFinding{Title: "x", Severity: "high", Confidence: 0.9}}}
	}

	t.Run("shadow: excluded service → zero shadow events", func(t *testing.T) {
		installExclusion(t, &fakeLearnExclusion{services: map[string]bool{"payments": true}})
		src := &batchSource{name: "es", signals: excludedService}
		w, sh := newModeOutputWorker(t, "shadow", src, AIBundle{}, nil)
		w.tickSource(context.Background(), src, "shadow")
		if sh.Len() != 0 {
			t.Fatalf("shadow recorded %d events for an excluded service, want 0", sh.Len())
		}
	})

	t.Run("shadow: excluded metric → zero shadow events", func(t *testing.T) {
		installExclusion(t, &fakeLearnExclusion{signals: map[string]bool{"cpu_util": true}})
		src := &batchSource{name: "es", signals: excludedMetric}
		w, sh := newModeOutputWorker(t, "shadow", src, AIBundle{}, nil)
		w.tickSource(context.Background(), src, "shadow")
		if sh.Len() != 0 {
			t.Fatalf("shadow recorded %d events for an excluded metric, want 0", sh.Len())
		}
	})

	t.Run("detect: excluded service → zero incidents", func(t *testing.T) {
		installExclusion(t, &fakeLearnExclusion{services: map[string]bool{"payments": true}})
		emit := 0
		src := &batchSource{name: "es", signals: excludedService}
		w, _ := newModeOutputWorker(t, "detect", src, newAgentBundle(),
			func(*core.AIFinding, core.AgentResult, string, string) error { emit++; return nil })
		w.tickSource(context.Background(), src, "detect")
		if emit != 0 {
			t.Fatalf("detect emitted %d incidents for an excluded service, want 0", emit)
		}
	})

	t.Run("detect: excluded metric → zero incidents", func(t *testing.T) {
		installExclusion(t, &fakeLearnExclusion{signals: map[string]bool{"cpu_util": true}})
		emit := 0
		src := &batchSource{name: "es", signals: excludedMetric}
		w, _ := newModeOutputWorker(t, "detect", src, newAgentBundle(),
			func(*core.AIFinding, core.AgentResult, string, string) error { emit++; return nil })
		w.tickSource(context.Background(), src, "detect")
		if emit != 0 {
			t.Fatalf("detect emitted %d incidents for an excluded metric, want 0", emit)
		}
	})

	t.Run("control: kept service DOES record in shadow", func(t *testing.T) {
		if learnExclusion() != nil {
			t.Fatal("precondition: slot must be nil for the control")
		}
		src := &batchSource{name: "es", signals: keptService}
		w, sh := newModeOutputWorker(t, "shadow", src, AIBundle{}, nil)
		w.tickSource(context.Background(), src, "shadow")
		if sh.Len() == 0 {
			t.Fatal("control: kept service produced zero shadow events — harness never fires, the zeros above are meaningless")
		}
	})

	t.Run("control: kept service DOES emit in detect", func(t *testing.T) {
		if learnExclusion() != nil {
			t.Fatal("precondition: slot must be nil for the control")
		}
		emit := 0
		src := &batchSource{name: "es", signals: keptService}
		w, _ := newModeOutputWorker(t, "detect", src, newAgentBundle(),
			func(*core.AIFinding, core.AgentResult, string, string) error { emit++; return nil })
		w.tickSource(context.Background(), src, "detect")
		if emit == 0 {
			t.Fatal("control: kept service produced zero incidents — harness never fires, the zeros above are meaningless")
		}
	})
}
