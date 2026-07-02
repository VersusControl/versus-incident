package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/scheduler"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// ANSI color codes used to make agent detection lines stand out from
// Versus's own error/info logs in the terminal. Disabled automatically when
// stdout is not a TTY (e.g. piped to a file or shipped to a log collector).
var (
	colorGreen = "\033[32m"
	colorReset = "\033[0m"
)

func init() {
	// Only emit color when stdout is a terminal. Best-effort — we don't import
	// a full TTY-detection library; checking for the env hint is enough.
	if os.Getenv("NO_COLOR") != "" {
		colorGreen = ""
		colorReset = ""
		return
	}
	fi, err := os.Stdout.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		colorGreen = ""
		colorReset = ""
	}
}

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
	// matcherForSource; unknown/unregistered types resolve to KindLogs.
	kindByName map[string]signalsources.Kind
	// metricsMatcher / tracesMatcher hold the OPTIONAL top-level per-kind regex
	// override built from agent.regex.metrics / agent.regex.traces. nil when
	// the operator did not set the key — matcherForSource then falls back to
	// matchAll (learn-all) for that kind. There is no per-source override.
	metricsMatcher *RegexMatcher
	tracesMatcher  *RegexMatcher

	// Detect-mode dependencies. All three are nil-safe: when ai.Detect
	// is nil the worker logs a "dry detect" line and skips emission.
	ai      AIBundle
	emitter Emitter

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
	Emitter Emitter
}

// NewWorker validates options and applies defaults.
func NewWorker(opt WorkerOptions) (*Worker, error) {
	w := &Worker{
		cfg:      opt.Cfg,
		sources:  opt.Sources,
		cursors:  opt.Cursors,
		redactor: opt.Redactor,
		matcher:  opt.Matcher,
		miner:    opt.Miner,
		catalog:  opt.Catalog,
		shadow:   opt.Shadow,
		detect:   opt.Detect,
		services: opt.Services,
		ai:       opt.AI,
		emitter:  opt.Emitter,
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
	w.kindByName = make(map[string]signalsources.Kind, len(opt.Cfg.Sources))
	for _, s := range opt.Cfg.Sources {
		if !s.Enable {
			continue
		}
		w.kindByName[s.Name] = signalsources.KindOf(s.Type)
	}

	// Resolve per-type brains. In OSS this registers nothing (the log brain is
	// the un-registered default); when Versus Enterprise is linked, its
	// metric/trace brains are built here for the matching configured sources.
	w.brains = make(map[string]typedBrain)
	w.resolveRegisteredBrains()

	return w, nil
}

// agentName returns the agent identifier passed to the mode resolver.
// The agent config carries no name field today, so this is the empty
// string; it exists as the single seam the resolver keys on, ready for a
// real identifier without touching the call site.
func (w *Worker) agentName() string { return "" }

// effectiveMode resolves the lifecycle mode for one tick. It starts from
// the static YAML floor (cfg.Mode, defaulting to training) and lets a
// registered ModeResolver override it. It fails closed: a nil resolver, an
// ok=false result, or an invalid mode all keep the YAML floor. OSS
// registers no resolver, so this is one nil-check and returns cfg.Mode —
// byte-for-byte unchanged.
func (w *Worker) effectiveMode(ctx context.Context) string {
	yaml := w.cfg.Mode
	if yaml == "" {
		yaml = "training"
	}
	if r := modeResolver(); r != nil {
		if m, ok := r.Mode(ctx, w.agentName()); ok && isValidMode(m) {
			return m
		}
	}
	return yaml
}

// effectiveAIEnabled reports whether the detect path should call the AI on
// this tick. It starts from "enabled" (today's behaviour) and lets a
// registered AISettingsResolver disable it at runtime. It fails open: a nil
// resolver or an ok=false result keeps today's behaviour, so the worker
// runs the real detect call exactly as before. OSS registers no resolver,
// so this is one nil-check returning true — byte-for-byte unchanged. The
// resolver is read live, so a hot toggle takes effect on the next call
// without a restart.
func (w *Worker) effectiveAIEnabled(ctx context.Context) bool {
	if r := aiSettingsResolver(); r != nil {
		if enabled, ok := r.EffectiveEnabled(ctx); ok {
			return enabled
		}
	}
	return true
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

	// Surface the dead-end config once at boot: detect mode wants to emit
	// incidents but AI is disabled, so the detect path runs dry. Only warn
	// in community mode — when a runtime AISettingsResolver is registered
	// the enable flag can switch AI on later, so it is not a dead end. The
	// configured mode is left untouched.
	if mode == "detect" && !w.cfg.AI.Enable && aiSettingsResolver() == nil {
		log.Printf("agent: WARN detect mode configured but AI disabled — running dry detect, no incidents will be emitted")
	}

	// Start the recurring-evaluation scheduler (E13). It runs registered
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
			if err := w.catalog.Persist(); err != nil {
				log.Printf("agent: final catalog flush failed: %v", err)
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
			return
		case <-tick.C:
			w.tick(ctx)
		case <-persist.C:
			if w.catalog.Dirty() {
				if err := w.catalog.Persist(); err != nil {
					log.Printf("agent: catalog flush failed: %v", err)
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
		}
	}
}

// tick runs one poll across every source. Errors from one source never
// affect the others — the worker keeps moving. The effective lifecycle
// mode is resolved once here, at the top of the cycle, so every source in
// this tick runs under one coherent mode and a hot-switch takes effect on
// the next cycle.
func (w *Worker) tick(ctx context.Context) {
	mode := w.effectiveMode(ctx)
	var wg sync.WaitGroup
	for _, src := range w.sources {
		wg.Add(1)
		go func(s core.SignalSource) {
			defer wg.Done()
			w.tickSource(ctx, s, mode)
		}(src)
	}
	wg.Wait()
}

func (w *Worker) tickSource(ctx context.Context, src core.SignalSource, mode string) {
	since := w.loadCursor(ctx, src.Name())

	signals, newCursor, err := src.Pull(ctx, since)
	if err != nil {
		log.Printf("agent: pull from %s failed: %v", src.Name(), err)
		return
	}
	if len(signals) == 0 {
		// Persist a legitimately-advanced cursor even though this tick emitted
		// nothing. A source that scanned a window and found no signals reports
		// newCursor > since to say "I am caught up to here"; dropping it would
		// strand the cursor and force the next tick to re-scan the same empty
		// range forever. This never skips unprocessed data: by the SignalSource
		// contract newCursor is the max timestamp the source actually scanned,
		// so anything after it is still pulled next tick. The OSS query sources
		// return newCursor == since on an empty pull, so this is a no-op for
		// them; it only bites a source that advances its cursor without emitting.
		if newCursor.After(since) {
			w.saveCursor(ctx, src.Name(), newCursor)
		}
		return
	}

	// Cap batch size as a safety net.
	if w.cfg.BatchMax > 0 && len(signals) > w.cfg.BatchMax {
		log.Printf("agent: %s returned %d signals, truncating to batch_max=%d",
			src.Name(), len(signals), w.cfg.BatchMax)
		signals = signals[:w.cfg.BatchMax]
	}

	// Redact every payload before doing anything else with it.
	for i := range signals {
		if w.redactor != nil {
			signals[i].Message = w.redactor.Scrub(signals[i].Message)
			signals[i].Fields = w.redactor.ScrubFields(signals[i].Fields)
			// Raw is intentionally left alone — operators never see it
			// outside admin debug; redacting it would hide useful structure.
		}
	}

	// Learn-exclusion chokepoint (X30 "Disable-Learn"). Runs immediately after
	// redaction and BEFORE the pre-brain matcher / grouping, so an excluded
	// (service, signal) never folds into the model in ANY mode — the mode
	// switch (training/shadow/detect) is downstream of learner.Group. OSS
	// installs no policy ⇒ learnExclusion() is nil ⇒ this block is skipped and
	// the tick is byte-for-byte unchanged.
	excluded := 0
	if x := learnExclusion(); x != nil {
		kept := signals[:0]
		for _, sig := range signals {
			svc, _ := sig.Fields[core.FieldService].(string)
			if svc == "" {
				svc = w.services.Extract(sig.Message)
				if svc == "" {
					svc = "_unknown"
				}
			}
			signalName, _ := sig.Fields[core.FieldSignal].(string)
			if x.ExcludeFromLearning(ctx, svc, signalName) {
				excluded++
				continue
			}
			kept = append(kept, sig)
		}
		signals = kept
	}

	// Decision 1B per-kind override (agent.regex.metrics / agent.regex.traces):
	// when the operator set it, drop metric/trace signals whose message does NOT
	// match BEFORE the brain sees them, so the override bites brain-agnostically
	// — including a licensed source bound to its enterprise brain, which never
	// builds a log brain. Logs are never pre-filtered here (the log brain owns
	// their default_pattern + rules); an unset override ⇒ nil matcher ⇒ all
	// signals flow (learn-all), unchanged. pulled keeps the pre-filter count so
	// the tick's skipped_no_match still reports the dropped signals.
	pulled := len(signals)
	if pf := w.preBrainMatcher(src.Name()); pf != nil {
		kept := signals[:0]
		for _, sig := range signals {
			if pf.Match(sig.Message).Matched() {
				kept = append(kept, sig)
			}
		}
		signals = kept
	}

	// Group by pattern within this tick so we can update the EWMA with
	// per-pattern frequency, not per-signal.
	//
	// The keying, learning and scoring are owned by the source's per-type
	// brain (the SignalLearner + SignalDetector). In OSS every source uses the
	// built-in log brain (drain-miner catalog + frequency-novelty classifier);
	// when an enterprise metric/trace brain is registered for the source's
	// type it plugs into this exact lifecycle. The worker stays type-agnostic.
	learner, detector := w.brainFor(src.Name())

	observations, err := learner.Group(ctx, signals)
	if err != nil {
		log.Printf("agent: grouping signals from %s failed: %v", src.Name(), err)
		return
	}

	// Update the model and produce verdicts, one observation at a time.
	verdicts := make(map[string]int) // verdict-name → count, for stats
	matched := 0                     // signals that landed in an observation
	for _, o := range observations {
		matched += o.Frequency

		// Track the service so the grace window starts even in training mode.
		if o.Service != "" && o.Service != "_unknown" {
			if w.catalog.RegisterService(o.Service) {
				log.Printf("agent: new service discovered: %s", o.Service)
			}
		}

		// Snapshot the learned expectation BEFORE folding this tick so a
		// deviation is judged against the prior baseline rather than the
		// freshly-smoothed value (otherwise a spike would smear into the EWMA
		// before we ever classify it). Then fold the tick into the model.
		mean, std, confident := learner.Expected(ctx, o.Key, o.Timestamp)
		if err := learner.Learn(ctx, []core.Observation{o}); err != nil {
			log.Printf("agent: learning key=%s from %s failed: %v", o.Key, src.Name(), err)
			continue
		}

		// Mode-specific tail: training logs discovery only; shadow/detect
		// classify against the pre-fold snapshot and sink. Split into its own
		// method (with early-returns for grace/known/suppressed) so the
		// promotion step below always runs, on every path, in every mode.
		w.handleObservation(ctx, mode, src, o, mean, std, confident, detector, verdicts)

		// Auto-promotion runs on the LEARN path, once per folded observation, in
		// EVERY mode — including training, which never calls Classify. It is
		// sequenced AFTER handleObservation so any Classify has already consumed
		// the pre-fold verdict this tick; a pattern that just crossed
		// auto_promote_after now has its stored Verdict persisted to "known", so
		// the Verdict column and the readiness "To known" column agree in every
		// mode. Brains that own their own promotion (the enterprise metric/trace
		// brains) do not implement the seam and are a no-op here.
		promoteByCount(learner, o.Key)
	}

	w.saveCursor(ctx, src.Name(), newCursor)

	log.Printf("agent: tick %s signals=%d matched=%d patterns=%d skipped_no_match=%d skipped_excluded=%d verdicts=%v cursor=%s",
		src.Name(), pulled, matched, len(observations), pulled-matched, excluded, verdicts, newCursor.Format(time.RFC3339))
}

// handleObservation runs the mode-specific tail for one ALREADY-FOLDED
// observation: training logs discovery only; shadow/detect classify against the
// pre-fold snapshot (mean/std/confident) and sink (shadow → NDJSON, detect →
// AI + emit). It performs NO promotion — the caller runs promoteByCount AFTER
// this returns, so the detector's Classify reads the pre-fold verdict before
// promotion persists it. Early-returns replace the old inline `continue`s so
// the caller's promotion step still runs on the grace / not-confident / known
// suppressed paths.
func (w *Worker) handleObservation(
	ctx context.Context,
	mode string,
	src core.SignalSource,
	o core.Observation,
	mean, std float64,
	confident bool,
	detector core.SignalDetector,
	verdicts map[string]int,
) {
	switch mode {
	case "training":
		// Pure observation. No verdict, no incident.
		verdicts["learned"]++
		if o.IsNew {
			log.Printf("%sagent: new pattern %s (source=%s tag=%s) → %s%s",
				colorGreen, o.Key, src.Name(), w.shadowTag(o), truncateString(o.Signal, 120), colorReset)
		}

	case "shadow", "detect":
		// New-service grace is shared by shadow and detect: shadow is meant
		// to be a faithful preview of detect, so whatever detect filters
		// out, shadow filters too. During grace the signal is learned
		// (folded above) but not surfaced as a "would alert" / AI candidate.
		if w.newServiceGrace > 0 &&
			w.catalog.IsServiceInGrace(o.Service, w.newServiceGrace) {
			verdicts["grace"]++
			if o.IsNew {
				log.Printf("%sagent[%s]: new pattern %s (service=%s in grace, learning only) → %s%s",
					colorGreen, mode, o.Key, o.Service, truncateString(o.Signal, 120), colorReset)
			}
			return
		}

		v := detector.Classify(o, mean, std, confident)
		verdicts[v.Class.String()]++

		// Per-key training gate: a model that is not yet confident for this
		// key suppresses in EVERY mode. Logs are always confident, so this
		// is a no-op for the OSS log brain; it is what lets a metric/trace
		// brain keep learning a fresh key without firing.
		if !v.Confident {
			return
		}
		// A known, non-spiking pattern is normal — nothing to surface.
		if v.Class == core.VerdictKnownPattern {
			return
		}

		// Mode-specific sink: shadow records to NDJSON; detect calls
		// the AI SRE and emits an incident.
		if mode == "shadow" {
			sample := ""
			if len(o.Samples) > 0 {
				sample = o.Samples[0].Message
			}
			if w.shadow != nil {
				w.shadow.Record(src.Name(), o.Key, o.Signal, sample,
					w.shadowTag(o), v.Class.String(), o.Frequency)
			}
			log.Printf("%sagent[shadow]: would alert pattern=%s service=%s tag=%s verdict=%s freq=%d%s",
				colorGreen, o.Key, o.Service, w.shadowTag(o), v.Class, o.Frequency, colorReset)
		} else {
			outcome := w.emitDetect(ctx, src.Name(), o.Key, o.Signal, o.Service, o.Samples, v.Class, v.Baseline)
			verdicts["emit_"+outcome]++
		}

	default:
		log.Printf("agent: unknown mode=%q, treating as training", mode)
		verdicts["learned"]++
	}
}

// signalPromoter is an OPTIONAL capability a brain may implement to persist a
// count-based auto-promotion on the LEARN path, after a tick has folded. The
// worker calls it via promoteByCount once per observation in EVERY mode, so a
// pattern that crosses its promotion threshold is marked in training exactly as
// it is in shadow/detect. Brains that own their own promotion (the enterprise
// metric/trace brains) simply do not implement it, so the seam stays generic.
type signalPromoter interface {
	// Promote persists a count-based promotion for key if it has just crossed
	// the threshold. It must be idempotent (a no-op once already promoted) and
	// safe to call every tick.
	Promote(key string)
}

// promoteByCount runs the learn-path promotion for brains that opt into the
// signalPromoter seam (the OSS log brain). It is a no-op for brains without it,
// keeping the worker type-agnostic. Called AFTER the mode tail so the detector
// has already read the pre-fold verdict this tick.
func promoteByCount(learner core.SignalLearner, key string) {
	if p, ok := learner.(signalPromoter); ok {
		p.Promote(key)
	}
}

// emitDetect handles one Unknown / Spike pattern in detect mode. Returns
// a short outcome label ("emitted" | "cached" | "dry" | "quota" |
// "ai_error" | "send_error") used as a stat suffix in the tick log.
//
// The function is nil-safe through and through:
//   - w.ai.Detect == nil       → "dry detect": classify, log, do not emit.
//   - w.ai.Cache hit           → reuse the prior finding without re-calling AI.
//   - w.ai.Rate.Allow == false → skip this call (would-be cost shed).
//   - w.emitter == nil         → AI was called and cached, but no incident is sent
//     (shadow-of-detect: useful for validating AI output without alerting).
func (w *Worker) emitDetect(
	ctx context.Context,
	source, patternID, template, service string,
	signals []core.Signal,
	verdict core.AgentVerdict,
	prevBaseline float64,
) string {
	result := core.AgentResult{
		Verdict:       verdict,
		PatternID:     patternID,
		Template:      template,
		SampleSignals: signals,
		Frequency:     len(signals),
		Baseline:      prevBaseline,
		RuleSeverity:  strongestSeverity(signals),
	}

	// Build a partial DetectEvent up front so every outcome path can
	// stamp itself into the audit log.
	evt := &DetectEvent{
		Source:       source,
		PatternID:    patternID,
		Template:     template,
		Service:      service,
		Verdict:      verdict.String(),
		Frequency:    len(signals),
		Baseline:     prevBaseline,
		Samples:      sampleMessages(signals, 3),
		RuleSeverity: result.RuleSeverity,
	}

	// 1. Dry detect — analyzer not configured, or a runtime resolver
	// disabled AI for this tick. In both cases: classify, log, do not call
	// AI, do not emit. The resolver is read live so an enterprise off→on
	// toggle takes effect on the next call without a restart; OSS registers
	// no resolver so this collapses to the original `w.ai.Detect == nil`
	// check — byte-for-byte unchanged.
	if w.ai.Detect == nil || !w.effectiveAIEnabled(ctx) {
		log.Printf("%sagent[detect:dry]: pattern=%s service=%s verdict=%s freq=%d (ai.enable=false)%s",
			colorGreen, patternID, service, verdict, len(signals), colorReset)
		evt.Outcome = "dry"
		w.detect.Record(evt)
		return "dry"
	}

	// 2. Cache hit — reuse the prior finding.
	if cached, ok := w.ai.Cache.Get(patternID); ok {
		log.Printf("%sagent[detect]: cache hit pattern=%s verdict=%s freq=%d%s",
			colorGreen, patternID, verdict, len(signals), colorReset)
		evt.Finding = cached
		outcome := w.send(cached, result, source, service, "cached")
		evt.Outcome = outcome
		if outcome == "send_error" {
			evt.Error = "emitter returned error (see logs)"
		}
		w.detect.Record(evt)
		return outcome
	}

	// 3. Rate limit guard.
	if !w.ai.Rate.Allow() {
		log.Printf("agent[detect]: AI quota exceeded; skipping pattern=%s freq=%d", patternID, len(signals))
		evt.Outcome = "quota"
		w.detect.Record(evt)
		return "quota"
	}

	// 4. Real AI call.
	call, err := w.ai.Detect.Run(ctx, core.DetectTask{Result: result})
	if err != nil {
		log.Printf("agent[detect]: AI analyze failed pattern=%s: %v", patternID, err)
		evt.Outcome = "ai_error"
		evt.Error = err.Error()
		w.detect.Record(evt)
		return "ai_error"
	}
	finding := call.Finding
	w.ai.Cache.Put(patternID, finding)

	evt.Finding = finding
	evt.Model = call.Model
	evt.UserPrompt = call.UserPrompt
	evt.RawResponse = call.RawResponse
	evt.DurationMs = call.DurationMs

	outcome := w.send(finding, result, source, service, "emitted")
	evt.Outcome = outcome
	if outcome == "send_error" {
		evt.Error = "emitter returned error (see logs)"
	}
	w.detect.Record(evt)
	return outcome
}

// send delegates to the configured emitter and translates errors into
// the worker's outcome vocabulary.
func (w *Worker) send(finding *core.AIFinding, result core.AgentResult, source, service, okLabel string) string {
	// Honour an operator-declared severity as a floor: the AI may escalate
	// but must not silently demote below the rule-declared severity (QA-006).
	clampSeverityFloor(finding, result.RuleSeverity)
	if w.emitter == nil {
		log.Printf("%sagent[detect]: pattern=%s service=%s severity=%s confidence=%.2f title=%q (no emitter wired)%s",
			colorGreen, result.PatternID, service, finding.Severity, finding.Confidence,
			truncateString(finding.Title, 80), colorReset)
		return okLabel
	}
	if err := w.emitter(finding, result, source, service); err != nil {
		log.Printf("agent[detect]: emit failed pattern=%s: %v", result.PatternID, err)
		return "send_error"
	}
	log.Printf("%sagent[detect]: emitted pattern=%s service=%s severity=%s confidence=%.2f title=%q%s",
		colorGreen, result.PatternID, service, finding.Severity, finding.Confidence,
		truncateString(finding.Title, 80), colorReset)
	return okLabel
}

// shadowTag re-derives the regex rule name for an observation from its first
// representative signal. It is used only for audit/shadow attribution and for
// the discovery log line, mirroring the tag the pre-seam worker captured from
// the bucket's first signal. Returns "" when no matcher is configured or the
// observation carries no raw samples (e.g. a metric/trace observation), so the
// field is simply omitted downstream.
func (w *Worker) shadowTag(o core.Observation) string {
	if w.matcher == nil || len(o.Samples) == 0 {
		return ""
	}
	return w.matcher.Match(o.Samples[0].Message).RuleName
}

// matcherForSource resolves the regex pre-filter for a source by name,
// applying the per-source-KIND precedence (highest → lowest):
//
//  1. the OPTIONAL top-level per-kind override — agent.regex.metrics /
//     agent.regex.traces (metricsMatcher / tracesMatcher), when set;
//  2. the per-kind built-in default — match-all for metrics/traces;
//  3. the global logs matcher (w.matcher) for the logs kind and any
//     unknown/unregistered type (which KindOf defaults to KindLogs).
//
// Only the log-brain path consults this; metric/trace sources bound to their
// proper enterprise brain group by fields and never hit it. The Decision 1B
// per-kind override still bites those sources via preBrainMatcher, which the
// worker applies to the signals before any brain (enterprise or log) sees them.
func (w *Worker) matcherForSource(name string) *RegexMatcher {
	switch w.kindByName[name] {
	case signalsources.KindMetrics:
		if w.metricsMatcher != nil {
			return w.metricsMatcher
		}
		return w.matchAll
	case signalsources.KindTraces:
		if w.tracesMatcher != nil {
			return w.tracesMatcher
		}
		return w.matchAll
	default:
		return w.matcher
	}
}

// preBrainMatcher returns the OPTIONAL per-kind text pre-filter applied to a
// metrics/traces source's signals BEFORE they reach its brain (enterprise or
// log). It is non-nil only when the operator set agent.regex.metrics /
// agent.regex.traces for that kind (Decision 1B); logs and the learn-all
// default return nil, so no pre-filter runs — logs are filtered inside the log
// brain, and an unset override lets metrics/traces learn all. Applying the
// override here (rather than inside a brain) makes it bite brain-agnostically,
// which is the only way it can narrow a licensed source that binds to its
// enterprise brain and never builds a log brain.
func (w *Worker) preBrainMatcher(name string) *RegexMatcher {
	switch w.kindByName[name] {
	case signalsources.KindMetrics:
		return w.metricsMatcher
	case signalsources.KindTraces:
		return w.tracesMatcher
	default:
		return nil
	}
}

// isSpike returns true when the current tick frequency exceeds the
// pattern's prior EWMA baseline by `multiplier`, subject to two safety
// floors:
//
//   - tickFreq must be at least minFreq (don't trigger on absolute counts
//     so small the ratio is meaningless).
//   - prevCount must be at least minBaselineCount (don't treat a
//     barely-seen pattern's first big tick as a spike; let it accumulate
//     a real baseline first).
//
// multiplier <= 0 disables spike detection entirely.
func isSpike(prevBaseline float64, prevCount, tickFreq int, multiplier float64, minFreq, minBaselineCount int) bool {
	if multiplier <= 0 {
		return false
	}
	if minFreq <= 0 {
		minFreq = 5
	}
	if minBaselineCount <= 0 {
		minBaselineCount = 20
	}
	if tickFreq < minFreq {
		return false
	}
	if prevCount < minBaselineCount {
		return false
	}
	if prevBaseline <= 0 {
		return false
	}
	return float64(tickFreq) > multiplier*prevBaseline
}

// -----------------------------------------------------------------------------
// cursor helpers
// -----------------------------------------------------------------------------

func (w *Worker) loadCursor(ctx context.Context, name string) time.Time {
	now := time.Now().UTC()
	if w.cursors != nil {
		if t, ok := w.cursors.Get(ctx, name); ok {
			// Never start a tick from a cursor ahead of the wall clock. A cursor
			// persisted before the tailing sources bounded their scan at `now`
			// could be poisoned into the future by a single future-dated
			// document (observed live: docs dated 2048). Left as-is it makes
			// every `>= cursor` query match nothing until that time arrives —
			// the source silently stops learning. Treating a future cursor like
			// no cursor at all heals it in one tick: we resume from the lookback
			// window and the source (now `lte: now`-bounded) advances normally.
			if !t.After(now) {
				return t
			}
			log.Printf("agent: cursor for %s is in the future (%s); resetting to lookback window", name, t.Format(time.RFC3339))
		}
	}
	return now.Add(-w.lookback)
}

func (w *Worker) saveCursor(ctx context.Context, name string, t time.Time) {
	if w.cursors == nil {
		return
	}
	if err := w.cursors.Set(ctx, name, t); err != nil {
		log.Printf("agent: failed to persist cursor for %s: %v", name, err)
	}
}

// -----------------------------------------------------------------------------
// utility
// -----------------------------------------------------------------------------

func parseDurationOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// sampleMessages returns up to `max` non-empty Message fields from
// signals — used to attach a few representative log lines to the
// detect audit log.
func sampleMessages(signals []core.Signal, max int) []string {
	if max <= 0 {
		return nil
	}
	out := make([]string, 0, max)
	for _, s := range signals {
		if s.Message == "" {
			continue
		}
		out = append(out, s.Message)
		if len(out) == max {
			break
		}
	}
	return out
}

// strongestSeverity returns the highest-ranked operator-declared severity
// carried by the grouped signals (the rule's `severity`), normalized to a
// canonical value. Returns "" when no signal carries a recognised declared
// severity (e.g. auto-discovered metric signals or log signals), so the
// downstream floor is simply absent and the AI decides.
func strongestSeverity(signals []core.Signal) string {
	best := ""
	bestRank := 0
	for _, s := range signals {
		if r := utils.SeverityRank(s.Severity); r > bestRank {
			bestRank = r
			best = utils.NormalizeSeverity(s.Severity)
		}
	}
	return best
}

// clampSeverityFloor raises finding.Severity up to floor when floor is a
// recognised severity stronger than the AI's verdict. It never demotes — an
// AI escalation above the floor is preserved. A no-op when floor is empty.
func clampSeverityFloor(f *core.AIFinding, floor string) {
	if f == nil || floor == "" {
		return
	}
	if utils.SeverityRank(floor) > utils.SeverityRank(f.Severity) {
		f.Severity = utils.NormalizeSeverity(floor)
	}
}
