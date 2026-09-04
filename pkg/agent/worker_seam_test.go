package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
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
				SpikeZ:                3.0,
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
	detectLog, err := LoadDetectLog(storage.NewMemory(), 10)
	if err != nil {
		t.Fatalf("LoadDetectLog: %v", err)
	}
	w.detect = detectLog
	recorderCalls := 0
	w.continueDetectionEpisode = func(storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
		recorderCalls++
		return storage.DetectionEpisodeDecision{}, storage.ErrNotFound
	}

	// A steady detect tick now: the pattern is known and not spiking → suppressed.
	w.tickSource(context.Background(), src, "detect")
	if recorderCalls != 1 || emit != 0 || atomic.LoadInt32(&agent.calls) != 0 || detectLog.Len() != 0 {
		t.Fatalf("known no-episode recorder/model/emitter/audit = %d/%d/%d/%d, want 1/0/0/0", recorderCalls, agent.calls, emit, detectLog.Len())
	}
}

func TestWorker_Seam_DetectKnownContinuationBypassesPipeline(t *testing.T) {
	src := &batchSource{name: "es", signals: repeatSignals("service=api steady id=", 5)}
	model := &fakeAgent{finding: &core.AIFinding{Title: "unused", Severity: "low"}}
	emitCalls := 0
	w := newSeamWorker(t, "detect", src, AIBundle{Detect: model}, func(*core.AIFinding, core.AgentResult, string, string) error {
		emitCalls++
		return nil
	})
	for index := 0; index < 21; index++ {
		w.tickSource(context.Background(), src, "training")
	}
	detectLog, err := LoadDetectLog(storage.NewMemory(), 10)
	if err != nil {
		t.Fatalf("LoadDetectLog: %v", err)
	}
	w.detect = detectLog
	var recorderCalls atomic.Int64
	var recorded storage.DetectionOccurrence
	w.continueDetectionEpisode = func(occurrence storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
		recorderCalls.Add(1)
		recorded = occurrence
		return storage.DetectionEpisodeDecision{
			Fingerprint: "fingerprint", EpisodeID: "episode", IncidentID: "incident",
			OccurrenceDelta: int64(occurrence.Frequency), OccurrenceCount: 110,
			Action: storage.DetectionEpisodeCoalesced,
		}, nil
	}

	w.tickSource(context.Background(), src, "detect")

	if recorderCalls.Load() != 1 || emitCalls != 0 || atomic.LoadInt32(&model.calls) != 0 {
		t.Fatalf("recorder/model/emitter calls = %d/%d/%d, want 1/0/0", recorderCalls.Load(), model.calls, emitCalls)
	}
	if !recorded.ExistingOnly || recorded.Identity.AgentKind != "detect" || recorded.Identity.Source != "es" ||
		recorded.Identity.Service != "api" || recorded.Identity.SignalKind != "logs" ||
		recorded.Identity.ConditionKey == "" || recorded.Frequency != 5 || recorded.ReceivedAt.IsZero() {
		t.Fatalf("recorded occurrence = %+v", recorded)
	}
	events := detectLog.All()
	if len(events) != 1 || events[0].EpisodeAction != string(storage.DetectionEpisodeCoalesced) ||
		events[0].NotificationOutcome != "not_applicable" || events[0].OccurrenceDelta != 5 ||
		len(events[0].Samples) != 0 || events[0].Template != "" || events[0].UserPrompt != "" || events[0].RawResponse != "" {
		t.Fatalf("known continuation audit = %+v", events)
	}
}

func TestWorker_Seam_DetectKnownContinuationErrorDoesNotEmit(t *testing.T) {
	src := &batchSource{name: "es", signals: repeatSignals("service=api steady id=", 5)}
	model := &fakeAgent{finding: &core.AIFinding{Title: "unused", Severity: "low"}}
	emitCalls := 0
	w := newSeamWorker(t, "detect", src, AIBundle{Detect: model}, func(*core.AIFinding, core.AgentResult, string, string) error {
		emitCalls++
		return nil
	})
	for index := 0; index < 21; index++ {
		w.tickSource(context.Background(), src, "training")
	}
	detectLog, err := LoadDetectLog(storage.NewMemory(), 10)
	if err != nil {
		t.Fatalf("LoadDetectLog: %v", err)
	}
	w.detect = detectLog
	w.continueDetectionEpisode = func(storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
		return storage.DetectionEpisodeDecision{}, errors.New(strings.Repeat("x", 400))
	}

	w.tickSource(context.Background(), src, "detect")

	if emitCalls != 0 || atomic.LoadInt32(&model.calls) != 0 {
		t.Fatalf("model/emitter calls = %d/%d, want 0/0", model.calls, emitCalls)
	}
	events := detectLog.All()
	if len(events) != 1 || events[0].EpisodeAction != "episode_error" || len(events[0].EpisodeError) != 256 ||
		events[0].NotificationOutcome != "not_applicable" {
		t.Fatalf("known continuation error audit = %+v", events)
	}
}

func TestWorker_Seam_DetectKnownContinuationUnsupportedIsSilent(t *testing.T) {
	w := &Worker{}
	detectLog, err := LoadDetectLog(storage.NewMemory(), 10)
	if err != nil {
		t.Fatalf("LoadDetectLog: %v", err)
	}
	w.detect = detectLog
	w.continueDetectionEpisode = func(storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
		return storage.DetectionEpisodeDecision{}, storage.ErrUnsupported
	}
	w.recordKnownDetectionContinuation("source", "logs", core.Observation{
		Key: "condition", Service: "checkout", Frequency: 1, Timestamp: time.Now().UTC(),
	})
	if detectLog.Len() != 0 {
		t.Fatalf("unsupported continuation audit events=%d, want 0", detectLog.Len())
	}
}

func TestWorker_Seam_DetectUsesFullObservationSeverity(t *testing.T) {
	w := &Worker{}
	var occurrence storage.DetectionOccurrence
	w.continueDetectionEpisode = func(recorded storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
		occurrence = recorded
		return storage.DetectionEpisodeDecision{}, storage.ErrNotFound
	}
	w.recordKnownDetectionContinuation("source", "logs", core.Observation{
		Key: "condition", Service: "checkout", Frequency: 2, Timestamp: time.Now().UTC(),
		Samples: []core.Signal{{Severity: "low"}}, StrongestSeverity: "critical",
	})
	if occurrence.Severity != "critical" {
		t.Fatalf("continuation severity=%q, want critical", occurrence.Severity)
	}
}

func TestWorker_Seam_KnownSpikeUsesNormalEmissionPath(t *testing.T) {
	src := &batchSource{name: "es", signals: repeatSignals("service=api steady id=", 5)}
	model := &fakeAgent{finding: &core.AIFinding{Title: "spike", Severity: "high", Confidence: 0.9}}
	emitCalls := 0
	w := newSeamWorker(t, "detect", src, AIBundle{Detect: model}, func(*core.AIFinding, core.AgentResult, string, string) error {
		emitCalls++
		return nil
	})
	for index := 0; index < 21; index++ {
		w.tickSource(context.Background(), src, "training")
	}
	recorderCalls := 0
	w.continueDetectionEpisode = func(storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
		recorderCalls++
		return storage.DetectionEpisodeDecision{}, storage.ErrNotFound
	}
	src.signals = repeatSignals("service=api steady id=", 500)

	w.tickSource(context.Background(), src, "detect")

	if recorderCalls != 0 || emitCalls != 1 || atomic.LoadInt32(&model.calls) != 1 {
		t.Fatalf("recorder/model/emitter calls = %d/%d/%d, want 0/1/1", recorderCalls, model.calls, emitCalls)
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

// singlePattern returns the sole pattern in the catalog, failing unless there
// is exactly one — the tests below drive a single template.
func singlePattern(t *testing.T, c *Catalog) *Pattern {
	t.Helper()
	all := c.All()
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 pattern, got %d", len(all))
	}
	return all[0]
}

// newTrainingWorker builds a training-mode worker over a fresh in-memory
// catalog with the given auto_promote_after, returning both so the test can
// inspect the persisted verdict. Spike is disabled (irrelevant in training,
// which never classifies).
func newTrainingWorker(t *testing.T, autoPromoteAfter int, src core.SignalSource) (*Worker, *Catalog) {
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
			Mode: "training",
			Catalog: config.AgentCatalogConfig{
				AutoPromoteAfter: autoPromoteAfter,
				SpikeMultiplier:  0,
			},
		},
		Sources:  []core.SignalSource{src},
		Matcher:  m,
		Miner:    NewMiner(0.4, 4, 100),
		Catalog:  cat,
		Services: svc,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w, cat
}

// TestWorker_Seam_TrainingPromotesVerdictToKnown is the founder's exact repro:
// in TRAINING mode the worker folds/learns but never calls Classify, yet a
// pattern that crosses auto_promote_after must still have its stored Verdict
// persisted to "known", so the Verdict column and the readiness "To known"
// column agree. On the pre-fix code — where promotion lived ONLY inside
// Classify, which training never calls — the stored Verdict stayed "" while
// LogReadiness (count-based) reported Ready, and the two diverged. This asserts
// they agree in training; it fails on the old code and passes on the new.
func TestWorker_Seam_TrainingPromotesVerdictToKnown(t *testing.T) {
	const threshold = 10
	const poll = 30 * time.Second
	// 5 signals sharing a template per tick → count grows 5, 10, …
	src := &batchSource{name: "es", signals: repeatSignals("service=api steady id=", 5)}
	w, cat := newTrainingWorker(t, threshold, src)

	// Tick 1: 5 sightings, below the threshold. Verdict uncurated, not ready —
	// and the two views agree while below the line too.
	w.tickSource(context.Background(), src, "training")
	p := singlePattern(t, cat)
	if p.Count != 5 {
		t.Fatalf("count after 1 training tick = %d, want 5", p.Count)
	}
	if p.Verdict == "known" {
		t.Fatalf("promoted early at count=5 (threshold %d)", threshold)
	}
	if LogReadiness(p, threshold, poll).Ready {
		t.Fatalf("readiness Ready at count=5, want not ready (below threshold)")
	}

	// Tick 2: crosses the threshold (count=10). The stored Verdict MUST now be
	// "known" AND readiness Ready — the two agree in training. This is the
	// assertion that bites the pre-fix code.
	w.tickSource(context.Background(), src, "training")
	p = singlePattern(t, cat)
	if p.Count != 10 {
		t.Fatalf("count after 2 training ticks = %d, want 10", p.Count)
	}
	if p.Verdict != "known" {
		t.Fatalf("training crossed threshold but stored Verdict = %q, want %q "+
			"(promotion must run on the learn path in training mode)", p.Verdict, "known")
	}
	if !LogReadiness(p, threshold, poll).Ready {
		t.Fatalf("LogReadiness.Ready = false after crossing threshold, want true")
	}
	// The founder's exact invariant: the persisted-verdict view and the
	// count-based readiness view agree.
	if (p.Verdict == "known") != LogReadiness(p, threshold, poll).Ready {
		t.Fatalf("Verdict/readiness disagree: verdict=%q ready=%v",
			p.Verdict, LogReadiness(p, threshold, poll).Ready)
	}
}

// TestWorker_Seam_TrainingZeroThresholdNormalizesAndPromotes proves a
// non-positive auto_promote_after is treated as the default (100) on the
// training learn path — there is no "promotion disabled" state. A pattern seen
// past the default IS marked "known" and readiness flips to ready, exactly as
// it would with the default configured explicitly.
func TestWorker_Seam_TrainingZeroThresholdNormalizesAndPromotes(t *testing.T) {
	for _, threshold := range []int{0, -5} {
		src := &batchSource{name: "es", signals: repeatSignals("service=api chatter id=", 5)}
		w, cat := newTrainingWorker(t, threshold, src) // <=0 normalizes to the default 100

		// Below the default: 19 ticks × 5 = 95 sightings, not yet known.
		for i := 0; i < 19; i++ {
			w.tickSource(context.Background(), src, "training")
		}
		p := singlePattern(t, cat)
		if p.Count != 95 {
			t.Fatalf("threshold=%d: count = %d, want 95", threshold, p.Count)
		}
		if p.Verdict == "known" {
			t.Fatalf("threshold=%d: promoted early at count=95 (default gate is 100)", threshold)
		}
		if LogReadiness(p, threshold, 30*time.Second).Ready {
			t.Fatalf("threshold=%d: readiness Ready below the default gate, want not ready", threshold)
		}

		// Cross the default: 20th tick → 100 sightings, now known.
		w.tickSource(context.Background(), src, "training")
		p = singlePattern(t, cat)
		if p.Count != 100 {
			t.Fatalf("threshold=%d: count = %d, want 100", threshold, p.Count)
		}
		if p.Verdict != "known" {
			t.Fatalf("threshold=%d: count reached 100 but Verdict = %q, want %q "+
				"(<=0 must normalize to the default, not disable promotion)", threshold, p.Verdict, "known")
		}
		if !LogReadiness(p, threshold, 30*time.Second).Ready {
			t.Fatalf("threshold=%d: readiness not Ready at the default gate, want ready", threshold)
		}
		if got := LogReadiness(p, threshold, 30*time.Second).Needed; got != 100 {
			t.Fatalf("threshold=%d: readiness Needed = %d, want 100 (normalized default)", threshold, got)
		}
	}
}
