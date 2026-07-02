package agent

import (
	"context"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// logBrain is the OSS default SignalLearner + SignalDetector: the drain-miner
// template catalog (the Learner) plus the frequency-novelty classifier (the
// Detector), refactored behind the per-type seam WITHOUT any behaviour change.
// A source whose type has no enterprise brain registered uses this — i.e. every
// source in the open-source build.
//
// One logBrain is built per source (it carries the owning source name for
// catalog attribution), but all log brains SHARE the worker's single Miner and
// Catalog — both internally synchronised — exactly as the pre-seam worker did.
type logBrain struct {
	source    string
	miner     *Miner
	catalog   *Catalog
	matcher   *RegexMatcher
	services  *ServiceMatcher
	ewmaAlpha float64
	cat       config.AgentCatalogConfig
}

// newLogBrain wires a log brain over the worker's shared miner/catalog plus the
// per-source name used for catalog attribution.
func newLogBrain(source string, miner *Miner, catalog *Catalog, matcher *RegexMatcher, services *ServiceMatcher, ewmaAlpha float64, cat config.AgentCatalogConfig) *logBrain {
	if ewmaAlpha <= 0 {
		ewmaAlpha = 0.2
	}
	return &logBrain{
		source:    source,
		miner:     miner,
		catalog:   catalog,
		matcher:   matcher,
		services:  services,
		ewmaAlpha: ewmaAlpha,
		cat:       cat,
	}
}

// Compile-time proof the log brain satisfies both halves of the seam.
var (
	_ core.SignalLearner  = (*logBrain)(nil)
	_ core.SignalDetector = (*logBrain)(nil)
)

// Kind implements core.SignalLearner / core.SignalDetector.
func (b *logBrain) Kind() string { return "logs" }

// Group clusters the tick's signals into one Observation per drain template,
// applying the same regex pre-filter and service extraction the pre-seam worker
// performed inline. Signals with an empty message, a non-matching regex, or an
// un-clusterable shape are dropped — none of them reached a bucket before
// either. Buckets are emitted in first-seen order for deterministic ticks
// (the pre-seam worker iterated a map; order never affected per-pattern
// verdicts, which are independent).
func (b *logBrain) Group(ctx context.Context, batch []core.Signal) ([]core.Observation, error) {
	type bucket struct {
		template string
		isNew    bool
		service  string
		signals  []core.Signal
	}
	buckets := make(map[string]*bucket)
	order := make([]string, 0, len(batch))

	for _, sig := range batch {
		if sig.Message == "" {
			continue
		}
		// Regex pre-filter: only signals matching at least one rule (named or
		// default) are worth learning from. To learn from every line, configure
		// regex.default_pattern: ".*".
		if b.matcher != nil {
			if !b.matcher.Match(sig.Message).Matched() {
				continue
			}
		}
		id, template, isNew := b.miner.Cluster(sig.Message)
		if id == "" {
			continue
		}
		bk := buckets[id]
		if bk == nil {
			svc := b.services.Extract(sig.Message)
			if svc == "" {
				svc = "_unknown"
			}
			// Manual-attribution override (Service-Override seam): an operator's
			// stored correction WINS over regex detection (and over "_unknown").
			// The match key is the mined pattern identity or a message substring.
			// A nil resolver (no override wired) returns svc unchanged.
			svc = ResolveServiceOverride(ctx, ServiceOverrideInput{
				SourceType: OverrideSourceLog,
				Service:    svc,
				Pattern:    id,
				Message:    sig.Message,
			})
			bk = &bucket{template: template, isNew: isNew, service: svc}
			buckets[id] = bk
			order = append(order, id)
		}
		bk.signals = append(bk.signals, sig)
	}

	now := time.Now().UTC()
	obs := make([]core.Observation, 0, len(buckets))
	for _, id := range order {
		bk := buckets[id]
		obs = append(obs, core.Observation{
			Key:       id,
			Service:   bk.service,
			Signal:    bk.template,
			Timestamp: now,
			Value:     float64(len(bk.signals)),
			Frequency: len(bk.signals),
			Samples:   bk.signals,
			IsNew:     bk.isNew,
		})
	}
	return obs, nil
}

// Expected returns the pre-fold EWMA baseline for a pattern. Logs have no
// per-key training gate (the lifecycle gates on MODE, not on a confidence
// flag), so confident is always true — log novelty is expressed through the
// Detector's VerdictUnknown, not through suppression. std is unused for logs.
func (b *logBrain) Expected(ctx context.Context, key string, ts time.Time) (mean, std float64, confident bool) {
	if p := b.catalog.Get(key); p != nil {
		return p.BaselineFrequency, 0, true
	}
	return 0, 0, true
}

// Learn folds the tick's observations into the catalog (one Upsert per
// pattern), exactly as the pre-seam worker did. The regex rule tag is
// re-derived from the bucket's first representative signal so attribution is
// byte-identical to the inlined path (Samples[0] is the signal that created
// the bucket, whose tag the worker used).
func (b *logBrain) Learn(ctx context.Context, obs []core.Observation) error {
	for _, o := range obs {
		ruleName := ""
		if b.matcher != nil && len(o.Samples) > 0 {
			ruleName = b.matcher.Match(o.Samples[0].Message).RuleName
		}
		b.catalog.Upsert(o.Key, o.Signal, b.source, o.Frequency, b.ewmaAlpha, ruleName, o.Service)
	}
	return nil
}

// Classify reproduces the pre-seam classify()/isSpike() decision exactly. It is
// called AFTER Learn has folded the tick, so the catalog holds the post-upsert
// count; the pre-fold baseline arrives via mean (snapshotted by Expected before
// Learn ran) and the pre-fold count is recovered as post-count − tick frequency.
// Logs always report Confident=true (see Expected); the worker suppresses a
// non-spiking known pattern via the VerdictKnownPattern gate, as before.
func (b *logBrain) Classify(obs core.Observation, mean, std float64, confident bool) core.TypedVerdict {
	tickFreq := obs.Frequency
	prevBaseline := mean

	p := b.catalog.Get(obs.Key)
	if p == nil {
		// Learn folds the tick before Classify runs, so the pattern always
		// exists here; mirror the pre-seam nil guard defensively.
		return core.TypedVerdict{Class: core.VerdictUnknown, Confident: true, Baseline: prevBaseline}
	}
	postCount := p.Count              // post-fold (Learn already ran this tick)
	prevCount := postCount - tickFreq // recover the pre-fold count
	prevVerdict := p.Verdict          // Upsert never mutates Verdict, so == pre-fold

	// AutoPromoteAfter ≤ 0 disables count-based promotion entirely ("0 disables
	// the promotion"): a pattern is never marked "known" by sighting count
	// alone, so it keeps flowing to detect-AI however often it is seen. The 100
	// default for an UNSET key is supplied by the embedded default_config layer
	// (loaded as the base before user overrides), so an omitted key arrives here
	// as 100 — only an explicit 0 (or negative) reaches the disabled branch. A
	// pattern already promoted to "known" stays known regardless of threshold.
	threshold := b.cat.AutoPromoteAfter
	isKnown := isLogKnown(prevVerdict, postCount, threshold)
	if isKnown {
		// Classify is a PURE scoring read: it does not persist the promotion.
		// Persisting "known" is done on the LEARN path by Promote (called by
		// the worker after this Classify has consumed the pre-fold verdict), so
		// a pattern is promoted in EVERY mode — training included — not only on
		// the shadow/detect classify path. Both gate on the same isLogKnown
		// predicate, so the verdict and readiness views can never drift.
		//
		// A known pattern can still spike — a steady drip suddenly flooding is
		// exactly what detect-AI should see. Spike supersedes known.
		if isSpike(prevBaseline, prevCount, tickFreq,
			b.cat.SpikeMultiplier, b.cat.SpikeMinFrequency, b.cat.SpikeMinBaselineCount) {
			score := 0.0
			if prevBaseline > 0 {
				score = float64(tickFreq) / prevBaseline
			}
			return core.TypedVerdict{
				Class:     core.VerdictSpike,
				Confident: true,
				Score:     score,
				Baseline:  prevBaseline,
			}
		}
		return core.TypedVerdict{Class: core.VerdictKnownPattern, Confident: true, Baseline: prevBaseline}
	}
	return core.TypedVerdict{Class: core.VerdictUnknown, Confident: true, Baseline: prevBaseline}
}

// Promote persists a count-based auto-promotion on the LEARN path so it runs in
// EVERY mode — training, shadow and detect — not only when Classify runs. The
// worker calls it (via the signalPromoter seam) once per folded observation,
// AFTER any Classify has read the pre-fold verdict for this tick, so promotion
// never corrupts the detector's spike-vs-known math (Classify still recovers
// prevCount = postCount − tickFreq and reads the pre-fold verdict).
//
// It gates on the SAME isLogKnown predicate the detector and the readiness view
// use, and persists only on the CROSSING — the guard prevVerdict != "known"
// makes it a no-op once already promoted — so a pattern that reaches
// auto_promote_after has its stored Verdict flipped to "known" exactly once. In
// training this is what keeps the Verdict column and the readiness "To known"
// column in agreement (both derive from isLogKnown).
//
// AutoPromoteAfter <= 0 disables count-based promotion entirely: for a not-yet-
// known pattern isLogKnown then returns false, so Promote never marks a pattern
// known by count in any mode; an operator-set "known" is untouched.
func (b *logBrain) Promote(key string) {
	p := b.catalog.Get(key)
	if p == nil {
		return
	}
	if p.Verdict != "known" && isLogKnown(p.Verdict, p.Count, b.cat.AutoPromoteAfter) {
		b.catalog.MarkKnown(key)
	}
}

// isLogKnown is the SINGLE definition of log "known" — the exact predicate
// logBrain.Classify gates promotion on. Both Classify and LogReadiness call it
// so the classifier and the read-side readiness view can never drift; a drift
// test pins them equal. A pattern is known when an operator (or a prior
// auto-promotion) already marked it "known", OR count-based promotion is
// enabled (autoPromoteAfter > 0) and the sighting count has reached it.
// autoPromoteAfter <= 0 disables count-based promotion, so only a pre-set
// "known" verdict counts.
func isLogKnown(prevVerdict string, count, autoPromoteAfter int) bool {
	return prevVerdict == "known" || (autoPromoteAfter > 0 && count >= autoPromoteAfter)
}

// LogReadiness computes the readiness of one log pattern as a generic
// core.Readiness — the read-side view of "how close is this pattern to being
// treated as routine baseline (known)". It is a pure function that does NOT
// mutate the pattern: it reads the pattern's own counters (the same ones the
// gate compares) and the catalog's AutoPromoteAfter threshold.
//
//   - Seen   = the pattern's sighting count (the gate's own counter).
//   - Needed = autoPromoteAfter when count-promotion is enabled (>0); 0 is the
//     indeterminate sentinel when promotion is disabled (autoPromoteAfter<=0 →
//     manual-only).
//   - Ready  = isLogKnown(...), so an already-"known"/"spike"-marked or
//     count-promoted pattern reports Ready.
//   - RatePerMin = the pattern's per-tick sighting EWMA (BaselineFrequency)
//     converted to sightings/minute via pollInterval, used to derive an ETA.
//     0 is the stalled/unknown sentinel (no rate yet, no flow, or no poll
//     interval wired) — the UI then shows no ETA. It is only computed while the
//     pattern is not yet Ready, since a Ready pattern has no countdown.
func LogReadiness(p *Pattern, autoPromoteAfter int, pollInterval time.Duration) core.Readiness {
	if p == nil {
		return core.Readiness{}
	}
	r := core.Readiness{
		Seen:  p.Count,
		Ready: isLogKnown(p.Verdict, p.Count, autoPromoteAfter),
	}
	if autoPromoteAfter > 0 {
		r.Needed = autoPromoteAfter
	} // else Needed=0 → indeterminate (manual-only promotion)
	if !r.Ready && pollInterval > 0 && p.BaselineFrequency > 0 {
		// BaselineFrequency is sightings/tick; convert to sightings/min.
		r.RatePerMin = p.BaselineFrequency / pollInterval.Minutes()
	}
	return r
}
