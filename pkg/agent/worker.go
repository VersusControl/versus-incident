package agent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/scheduler"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Worker runs the agent loop:
//
//	Pull (per source) → Redact → Cluster → Catalog upsert → (mode-specific tail)
//
// Training mode is fully end-to-end. Shadow and detect modes share the same
// classification path; shadow records would-have-alerted events to the
// shadow log, while detect calls the AI SRE and emits an incident through
// the existing services.CreateIncident pipeline.
type Worker struct {
	cfg      config.AgentConfig
	sources  []core.SignalSource
	cursors  *CursorStore // nil → in-memory fallback
	redactor *Redactor
	matcher  *RegexMatcher
	miner    *Miner
	catalog  *Catalog
	shadow   *ShadowLog // nil when shadow log is disabled
	detect   *DetectLog // nil when detect log is disabled

	// matchAll is the built-in "learn all" matcher ((?i).*) handed to metric
	// and trace sources on the log-brain fallback path, so their non-"error"
	// messages are not dropped by the log-tuned default pattern. Built once in
	// NewWorker. matcher (above) stays the LOGS matcher and is unchanged.
	matchAll *RegexMatcher
	// kindByName maps an enabled source NAME to its data-source KIND
	// (signalsources.KindOf(type)). It drives per-kind matcher selection in
	// matcherForSource; unknown/unregistered types resolve to KindUnknown.
	kindByName map[string]signalsources.Kind
	// metricsMatcher / tracesMatcher hold the OPTIONAL top-level per-kind regex
	// override built from agent.regex.metrics / agent.regex.traces. nil when
	// the operator did not set the key — matcherForSource then falls back to
	// matchAll (learn-all) for that kind. There is no per-source override.
	metricsMatcher *RegexMatcher
	tracesMatcher  *RegexMatcher

	// Detect-mode dependencies. All three are nil-safe: when ai.Detect
	// is nil the worker emits a deterministic templated alert instead of
	// calling AI, so a detection is never dropped for lack of a key.
	ai                       AIBundle
	emitter                  Emitter
	continueDetectionEpisode DetectionEpisodeRecorder
	healthRecorder           core.LogHealthRecorder
	orgID                    string

	pollInterval    time.Duration
	persistEvery    time.Duration
	lookback        time.Duration
	ewmaAlpha       float64
	services        *ServiceMatcher // regex-based service-name extractor
	newServiceGrace time.Duration   // 0 = disabled

	// brains maps a source NAME to its resolved per-type brain (Learner +
	// Detector). Enterprise metric/trace brains are resolved at construction
	// from cfg.Sources; any source without a registered brain — every source
	// in the OSS build — gets the built-in log brain, created lazily on first
	// tick. brainMu guards the lazy insert because ticks run concurrently.
	brains  map[string]typedBrain
	brainMu sync.Mutex
}

// Emitter delivers an AI finding to the rest of the system. In production
// this is services.CreateIncidentFromFinding; tests inject a capturing
// stub. A nil Emitter disables emission (worker logs the would-be call
// and moves on).
type Emitter func(f *core.AIFinding, r core.AgentResult, source, service string) error

// DetectionEpisodeRecorder records an occurrence without deciding whether it
// should be enriched or emitted.
type DetectionEpisodeRecorder func(storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error)

// WorkerOptions bundles the dependencies a Worker needs. Construction does
// not connect to anything; the worker dials lazily inside Run.
type WorkerOptions struct {
	Cfg      config.AgentConfig
	Sources  []core.SignalSource
	Cursors  *CursorStore // optional; pass nil for in-memory cursors
	Redactor *Redactor
	Matcher  *RegexMatcher
	Miner    *Miner
	Catalog  *Catalog
	Shadow   *ShadowLog      // optional; pass nil to disable shadow recording
	Detect   *DetectLog      // optional; pass nil to disable detect-call audit log
	Services *ServiceMatcher // optional; pass nil to disable service detection

	// AI is the detect-mode bundle (analyzer + cache + rate limiter).
	// Pass a zero-value AIBundle to disable AI emission.
	AI AIBundle
	// Emitter is invoked for each finding in detect mode. nil disables
	// emission (worker still calls AI and caches the result, but the
	// finding does not flow through to channels).
	Emitter                  Emitter
	ContinueDetectionEpisode DetectionEpisodeRecorder
	// HealthRecorder receives source-neutral post-grouping log projections.
	// It never pulls a source or classifies an observation itself.
	HealthRecorder core.LogHealthRecorder
	// OrgID is the trusted write organization for worker-produced records.
	// Empty preserves the single-tenant OSS default.
	OrgID string
}

// NewWorker validates options and applies defaults.
func NewWorker(opt WorkerOptions) (*Worker, error) {
	w := &Worker{
		cfg:                      opt.Cfg,
		sources:                  opt.Sources,
		cursors:                  opt.Cursors,
		redactor:                 opt.Redactor,
		matcher:                  opt.Matcher,
		miner:                    opt.Miner,
		catalog:                  opt.Catalog,
		shadow:                   opt.Shadow,
		detect:                   opt.Detect,
		services:                 opt.Services,
		ai:                       opt.AI,
		emitter:                  opt.Emitter,
		continueDetectionEpisode: opt.ContinueDetectionEpisode,
		healthRecorder:           opt.HealthRecorder,
		orgID:                    storage.NormalizeOrgID(opt.OrgID),
	}

	if w.miner == nil {
		return nil, fmt.Errorf("agent: miner is required")
	}
	if w.catalog == nil {
		return nil, fmt.Errorf("agent: catalog is required")
	}

	w.pollInterval = parseDurationOr(opt.Cfg.PollInterval, 30*time.Second)
	w.persistEvery = parseDurationOr(opt.Cfg.Catalog.PersistInterval, 30*time.Second)
	w.lookback = parseDurationOr(opt.Cfg.Lookback, 5*time.Minute)
	w.ewmaAlpha = 0.2 // configurable once spike detection lands
	w.newServiceGrace = parseDurationOr(opt.Cfg.NewServiceGrace, 0)

	// A tail's re-read span is also the span a replacement process replays
	// after an abrupt kill, and what it has to cover is every row learned since
	// the last catalog flush. This is the one place that knows both numbers, so
	// widen every tail here by one persist interval rather than trusting two
	// settings in two files to be tuned against each other.
	if widened := signalsources.ApplyTailReplaySpan(w.sources, w.persistEvery); len(widened) > 0 {
		log.Printf("agent: tail re-read span widened by the %s catalog persist_interval so an abrupt restart cannot lose unflushed rows: %s",
			w.persistEvery, signalsources.FormatTailWindows(widened))
	}

	// Wire the baseline fold from the poll interval + spike settings so
	// Catalog.Upsert folds a poll-independent per-second rate through the
	// outlier-resistant EWMA (and seasonal buckets when enabled), sharing the
	// detector's reject cutoff / seasonal period / confidence gate.
	w.catalog.SetBaselineFold(resolveSpikeParams(opt.Cfg.Catalog, w.pollInterval.Seconds()).fold())

	// One-time deprecation notice: spike_multiplier no longer drives detection
	// (the threshold moved from a volume-blind ratio to the z-score). It is
	// accepted-and-ignored so an existing config still loads.
	if opt.Cfg.Catalog.SpikeMultiplier != 0 {
		log.Printf("agent: config key `spike_multiplier` is DEPRECATED and ignored — spike detection now uses `spike_z` (a z-score). Remove spike_multiplier from your config; see docs/agent/spike.")
	}

	// Per-source-KIND regex default. The logs matcher is the global default
	// (w.matcher); metrics/traces get a build-once match-all matcher so their
	// non-"error" messages flow with zero operator config. The OPTIONAL
	// top-level per-kind override (agent.regex.metrics / agent.regex.traces),
	// when set, narrows that kind for all of its sources; there is no
	// per-source override.
	w.matchAll, _ = NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: "(?i).*"})
	if pat := opt.Cfg.Regex.Metrics; pat != "" {
		w.metricsMatcher, _ = NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: pat})
	}
	if pat := opt.Cfg.Regex.Traces; pat != "" {
		w.tracesMatcher, _ = NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: pat})
	}
	w.kindByName = make(map[string]signalsources.Kind, len(w.sources))
	for _, configured := range opt.Cfg.Sources {
		if configured.Enable {
			w.kindByName[configured.Name] = signalsources.KindOf(configured.Type)
		}
	}
	for _, source := range w.sources {
		for _, configured := range opt.Cfg.Sources {
			if configured.Enable && signalsources.MatchesConfiguredName(source.Name(), configured.Name) {
				w.kindByName[source.Name()] = signalsources.KindOf(configured.Type)
				break
			}
		}
	}

	// Resolve per-type brains. In OSS this registers nothing (the log brain is
	// the un-registered default); when Versus Enterprise is linked, its
	// metric/trace brains are built here for the matching configured sources.
	w.brains = make(map[string]typedBrain)
	w.resolveRegisteredBrains()

	return w, nil
}

// Run drives the worker until ctx is canceled. It is intended to be called
// in a goroutine from cmd/main.go.
func (w *Worker) Run(ctx context.Context) {
	mode := w.cfg.Mode
	if mode == "" {
		mode = "training"
	}
	log.Printf("agent: starting worker mode=%s sources=%d poll=%s",
		mode, len(w.sources), w.pollInterval)

	// Surface at boot that detect mode is running without AI: detections still
	// emit incidents, but as deterministic templated alerts rather than
	// AI-enriched ones. Only note it in community mode — when a runtime
	// AISettingsResolver is registered the enable flag can switch AI on later.
	// The configured mode is left untouched.
	if mode == "detect" && !w.cfg.AI.Enable && aiSettingsResolver() == nil {
		log.Printf("agent: detect mode without AI — emitting deterministic templated alerts (configure agent.ai to enrich them)")
	}

	// Start the recurring-evaluation scheduler. It runs registered
	// read-only / analyze-kind jobs on their own interval, bound to this
	// worker's context so shutdown cancels in-flight jobs. In community
	// mode no job is registered, so this starts nothing and OSS behaviour
	// is byte-for-byte unchanged.
	go scheduler.NewFromRegistry().Run(ctx)

	// Stagger initial pull so multiple sources don't hammer their backends
	// at the same instant on startup.
	tick := time.NewTicker(w.pollInterval)
	defer tick.Stop()

	persist := time.NewTicker(w.persistEvery)
	defer persist.Stop()

	// Run one tick immediately so users don't wait `poll_interval` for
	// signs of life. The effective mode is resolved at the top of every
	// tick (see tick), so a registered resolver can hot-switch it without
	// a restart; OSS resolves to the YAML floor unchanged.
	w.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Printf("agent: worker stopping; flushing catalog…")
			flushed := true
			if err := w.catalog.Persist(); err != nil {
				log.Printf("agent: final catalog flush failed: %v", err)
				flushed = false
			} else {
				log.Printf("agent: catalog flushed on shutdown (%d patterns)", w.catalog.Len())
			}
			if w.shadow != nil {
				if err := w.shadow.Persist(); err != nil {
					log.Printf("agent: final shadow flush failed: %v", err)
				}
			}
			if w.detect != nil {
				if err := w.detect.Persist(); err != nil {
					log.Printf("agent: final detect flush failed: %v", err)
				}
			}
			if flushed {
				// This worker's ctx is already canceled, so the commit needs a
				// live one of its own or every backend call fails instantly and
				// the restart replays the last window for nothing.
				commitCtx, cancel := context.WithTimeout(context.Background(), shutdownCommitTimeout)
				w.commitSources(commitCtx)
				cancel()
			}
			return
		case <-tick.C:
			w.tick(ctx)
		case <-persist.C:
			flushed := true
			if w.catalog.Dirty() {
				if err := w.catalog.Persist(); err != nil {
					log.Printf("agent: catalog flush failed: %v", err)
					flushed = false
				} else {
					log.Printf("agent: catalog flushed (%d patterns)", w.catalog.Len())
				}
			}
			if w.shadow != nil && w.shadow.Dirty() {
				if err := w.shadow.Persist(); err != nil {
					log.Printf("agent: shadow flush failed: %v", err)
				} else {
					log.Printf("agent: shadow log flushed (%d events)", w.shadow.Len())
				}
			}
			if w.detect != nil && w.detect.Dirty() {
				if err := w.detect.Persist(); err != nil {
					log.Printf("agent: detect flush failed: %v", err)
				} else {
					log.Printf("agent: detect log flushed (%d events)", w.detect.Len())
				}
			}
			// The rows this cycle learned are now in storage, so the ids that
			// suppress re-reading them may become durable too — in that order,
			// never the other way round.
			if flushed {
				w.commitSources(ctx)
			}
		}
	}
}

// shutdownCommitTimeout bounds the post-flush commit on the shutdown path. A
// container stop gives the process a limited grace period; overrunning it turns
// a graceful stop into a kill, so the commit gets a short, explicit budget.
const shutdownCommitTimeout = 5 * time.Second

// commitSources lets every source that stages delivery state make it durable.
// It runs ONLY after the catalog flush that put this cycle's rows in storage,
// which is what keeps the two writes from inverting: a source's record of "I
// already delivered this row" can never outlive the row itself.
//
// A source with no staged state does not implement core.SourceCommitter and is
// skipped, so it behaves exactly as it did before the seam existed. A failing
// commit is logged and the loop continues — the pending ids stay pending and
// the next commit retries them, so the cost is a possible replay, never a hole.
func (w *Worker) commitSources(ctx context.Context) {
	for _, src := range w.sources {
		committer, ok := src.(core.SourceCommitter)
		if !ok {
			continue
		}
		if err := committer.Commit(ctx); err != nil {
			log.Printf("agent: %s: persisting the delivered-row set failed: %v (a restart before the next commit may re-emit up to one reorder window; no row is lost)", src.Name(), err)
		}
	}
}

// StartWorker runs the worker in its own goroutine and returns a channel that
// closes when Run has returned — that is, when the shutdown flush AND the
// commit that follows it have finished. A process that exits without waiting on
// this races its own final flush and drops everything learned since the last
// periodic one.
func StartWorker(ctx context.Context, w *Worker) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	return done
}

// WaitForShutdownFlush blocks until the worker signalled by done has finished
// its shutdown flush, or until timeout elapses. It reports whether the flush
// completed, so the caller can say so rather than exiting silently mid-write.
// A nil channel (agent disabled) returns immediately.
func WaitForShutdownFlush(done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
