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

	// Detect-mode dependencies. All three are nil-safe: when ai.SRE is
	// nil the worker logs a "dry detect" line and skips emission.
	ai      AIBundle
	emitter Emitter

	pollInterval    time.Duration
	persistEvery    time.Duration
	lookback        time.Duration
	ewmaAlpha       float64
	services        *ServiceMatcher // regex-based service-name extractor
	newServiceGrace time.Duration   // 0 = disabled
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

	if len(w.sources) == 0 {
		return nil, fmt.Errorf("agent: no enabled sources")
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

	// Stagger initial pull so multiple sources don't hammer their backends
	// at the same instant on startup.
	tick := time.NewTicker(w.pollInterval)
	defer tick.Stop()

	persist := time.NewTicker(w.persistEvery)
	defer persist.Stop()

	// Run one tick immediately so users don't wait `poll_interval` for
	// signs of life.
	w.tick(ctx, mode)

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
			w.tick(ctx, mode)
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
// affect the others — the worker keeps moving.
func (w *Worker) tick(ctx context.Context, mode string) {
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

	// Group by pattern within this tick so we can update the EWMA with
	// per-pattern frequency, not per-signal.
	type bucket struct {
		template    string
		signals     []core.Signal
		isNew       bool
		tag         MatchResult
		serviceName string // extracted from first signal's Fields
	}
	buckets := make(map[string]*bucket)

	skippedNoMatch := 0
	for _, sig := range signals {
		if sig.Message == "" {
			continue
		}
		// Regex pre-filter: only signals matching at least one rule (named or
		// default) are worth learning from. This keeps boring noise out of the
		// catalog. To learn from every line, configure
		// `regex.default_pattern: ".*"`.
		var tag MatchResult
		if w.matcher != nil {
			tag = w.matcher.Match(sig.Message)
			if !tag.Matched() {
				skippedNoMatch++
				continue
			}
		}
		id, template, isNew := w.miner.Cluster(sig.Message)
		if id == "" {
			continue
		}
		b := buckets[id]
		if b == nil {
			svc := w.services.Extract(sig.Message)
			if svc == "" {
				svc = "_unknown"
			}
			b = &bucket{template: template, isNew: isNew, tag: tag, serviceName: svc}
			buckets[id] = b
		}
		b.signals = append(b.signals, sig)
	}

	// Update catalog and produce verdicts.
	verdicts := make(map[string]int) // verdict-name → count, for stats
	for id, b := range buckets {
		// Snapshot pattern state BEFORE upsert so spike detection can
		// compare this tick against the prior baseline rather than the
		// freshly-smoothed value.
		var prevBaseline float64
		var prevCount int
		if prev := w.catalog.Get(id); prev != nil {
			prevBaseline = prev.BaselineFrequency
			prevCount = prev.Count
		}

		w.catalog.Upsert(id, b.template, src.Name(), len(b.signals), w.ewmaAlpha, b.tag.RuleName, b.serviceName)

		// Track the service so the grace window starts even in training mode.
		if b.serviceName != "_unknown" {
			if w.catalog.RegisterService(b.serviceName) {
				log.Printf("agent: new service discovered: %s", b.serviceName)
			}
		}

		tag := b.tag

		switch mode {
		case "training":
			// Pure observation. No verdict, no incident.
			verdicts["learned"]++
			if b.isNew {
				log.Printf("%sagent: new pattern %s (source=%s tag=%s) → %s%s",
					colorGreen, id, src.Name(), tag.RuleName, truncateString(b.template, 120), colorReset)
			}

		case "shadow", "detect":
			// New-service grace is shared by shadow and detect: shadow is
			// meant to be a faithful preview of detect, so whatever detect
			// filters out, shadow filters too. During grace the signal is
			// learned (catalog already upserted above) but not surfaced as
			// a "would alert" / AI candidate.
			// Remove b.serviceName != "_unknown"
			if w.newServiceGrace > 0 &&
				w.catalog.IsServiceInGrace(b.serviceName, w.newServiceGrace) {
				verdicts["grace"]++
				if b.isNew {
					log.Printf("%sagent[%s]: new pattern %s (service=%s in grace, learning only) → %s%s",
						colorGreen, mode, id, b.serviceName, truncateString(b.template, 120), colorReset)
				}
				continue
			}

			v := w.classify(id, len(b.signals), prevBaseline, prevCount)
			verdicts[v.String()]++
			if v == core.VerdictKnownPattern {
				continue
			}

			// Mode-specific sink: shadow records to NDJSON; detect calls
			// the AI SRE and emits an incident.
			if mode == "shadow" {
				sample := ""
				if len(b.signals) > 0 {
					sample = b.signals[0].Message
				}
				if w.shadow != nil {
					w.shadow.Record(src.Name(), id, b.template, sample,
						b.tag.RuleName, v.String(), len(b.signals))
				}
				log.Printf("%sagent[shadow]: would alert pattern=%s service=%s tag=%s verdict=%s freq=%d%s",
					colorGreen, id, b.serviceName, tag.RuleName, v, len(b.signals), colorReset)
			} else {
				outcome := w.emitDetect(ctx, src.Name(), id, b.template, b.serviceName, b.signals, v, prevBaseline)
				verdicts["emit_"+outcome]++
			}

		default:
			log.Printf("agent: unknown mode=%q, treating as training", mode)
			verdicts["learned"]++
		}
	}

	w.saveCursor(ctx, src.Name(), newCursor)

	log.Printf("agent: tick %s signals=%d matched=%d patterns=%d skipped_no_match=%d verdicts=%v cursor=%s",
		src.Name(), len(signals), len(signals)-skippedNoMatch, len(buckets), skippedNoMatch, verdicts, newCursor.Format(time.RFC3339))
}

// emitDetect handles one Unknown / Spike pattern in detect mode. Returns
// a short outcome label ("emitted" | "cached" | "dry" | "quota" |
// "ai_error" | "send_error") used as a stat suffix in the tick log.
//
// The function is nil-safe through and through:
//   - w.ai.SRE == nil          → "dry detect": classify, log, do not emit.
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
	}

	// Build a partial DetectEvent up front so every outcome path can
	// stamp itself into the audit log.
	evt := &DetectEvent{
		Source:    source,
		PatternID: patternID,
		Template:  template,
		Service:   service,
		Verdict:   verdict.String(),
		Frequency: len(signals),
		Baseline:  prevBaseline,
		Samples:   sampleMessages(signals, 3),
	}

	// 1. Dry detect — analyzer not configured.
	if w.ai.SRE == nil {
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
	call, err := w.ai.SRE.Analyze(ctx, result)
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

// classify decides the verdict for a pattern given the current tick's
// frequency and the pattern's pre-upsert state. The catalog has already
// been upserted by the caller, so prevBaseline/prevCount reflect what the
// pattern looked like BEFORE this tick — which is what we need for
// spike detection (otherwise a 5× spike would smear into the EWMA before
// we ever see it).
func (w *Worker) classify(patternID string, tickFreq int, prevBaseline float64, prevCount int) core.AgentVerdict {
	p := w.catalog.Get(patternID)
	if p == nil {
		return core.VerdictUnknown
	}
	// A pattern is "known" once it has been observed enough times to be
	// considered baseline. The auto_promote_after threshold doubles as the
	// detect-mode cutoff for "we trust this is normal".
	threshold := w.cfg.Catalog.AutoPromoteAfter
	if threshold <= 0 {
		threshold = 100
	}
	isKnown := p.Verdict == "known" || p.Count >= threshold
	if isKnown {
		if p.Verdict != "known" {
			w.catalog.MarkKnown(patternID)
		}
		// A known pattern can still spike: a steady drip suddenly
		// jumping to a flood is exactly the case AI analysis should
		// see. Spike supersedes "known" only when configured.
		if isSpike(prevBaseline, prevCount, tickFreq,
			w.cfg.Catalog.SpikeMultiplier,
			w.cfg.Catalog.SpikeMinFrequency,
			w.cfg.Catalog.SpikeMinBaselineCount) {
			return core.VerdictSpike
		}
		return core.VerdictKnownPattern
	}
	return core.VerdictUnknown
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
	if w.cursors != nil {
		if t, ok := w.cursors.Get(ctx, name); ok {
			return t
		}
	}
	return time.Now().UTC().Add(-w.lookback)
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
