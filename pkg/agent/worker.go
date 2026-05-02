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
// shadow log, while detect logs the verdict (AI emission is wired up
// separately).
type Worker struct {
	cfg      config.AgentConfig
	sources  []core.SignalSource
	cursors  *CursorStore // nil → in-memory fallback
	redactor *Redactor
	matcher  *RegexMatcher
	miner    *Miner
	catalog  *Catalog
	shadow   *ShadowLog // nil when shadow log is disabled

	pollInterval    time.Duration
	persistEvery    time.Duration
	lookback        time.Duration
	ewmaAlpha       float64
	services        *ServiceMatcher // regex-based service-name extractor
	newServiceGrace time.Duration   // 0 = disabled
}

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
	Services *ServiceMatcher // optional; pass nil to disable service detection
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
		services: opt.Services,
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
	log.Printf("agent: starting worker mode=%s sources=%d poll=%s catalog=%s",
		mode, len(w.sources), w.pollInterval, w.cfg.CatalogPath())

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
			if b.serviceName != "_unknown" && w.newServiceGrace > 0 &&
				w.catalog.IsServiceInGrace(b.serviceName, w.newServiceGrace) {
				verdicts["grace"]++
				if b.isNew {
					log.Printf("%sagent[%s]: new pattern %s (service=%s in grace, learning only) → %s%s",
						colorGreen, mode, id, b.serviceName, truncateString(b.template, 120), colorReset)
				}
				continue
			}

			v := w.classify(id, len(b.signals))
			verdicts[v.String()]++
			if v == core.VerdictKnownPattern {
				continue
			}

			// Mode-specific sink: shadow records to NDJSON; detect will
			// call the AI analyzer and emit an incident.
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
				log.Printf("%sagent[detect]: pattern=%s service=%s tag=%s verdict=%s freq=%d (AI emission not yet wired)%s",
					colorGreen, id, b.serviceName, tag.RuleName, v, len(b.signals), colorReset)
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

// Classify is the (currently minimal) verdict logic for shadow/detect modes.
// No spike thresholding yet — anything new is unknown, anything we already
// had is known. Frequency-based spike detection ships in a follow-up.
// tickFrequency will be used for EWMA spike detection later.
func (w *Worker) classify(patternID string, _ int) core.AgentVerdict {
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
	if p.Verdict == "known" || p.Count >= threshold {
		if p.Verdict != "known" {
			w.catalog.MarkKnown(patternID)
		}
		return core.VerdictKnownPattern
	}
	return core.VerdictUnknown
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
