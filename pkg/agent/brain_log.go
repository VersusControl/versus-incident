package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/stats"
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
	// params are the resolved spike-detection knobs (defaults applied, poll
	// interval folded in) the detector scores against.
	params spikeParams
	// scrubber re-scrubs the captured sample line at the storage boundary
	// (defence-in-depth) inside RecordSample. It is the worker's pipeline
	// redactor; nil disables the re-scrub (input is already redacted).
	scrubber core.Scrubber

	// spikeMu guards streak, the per-pattern consecutive-spike counter that
	// implements the sustained-tick debounce.
	spikeMu sync.Mutex
	streak  map[string]int
}

// newLogBrain wires a log brain over the worker's shared miner/catalog plus the
// per-source name used for catalog attribution. scrub is the pipeline redactor,
// re-applied when the brain records a redacted sample line for the pattern.
// pollSeconds is the worker's tick duration, used to score a per-second rate.
func newLogBrain(source string, miner *Miner, catalog *Catalog, matcher *RegexMatcher, services *ServiceMatcher, ewmaAlpha float64, cat config.AgentCatalogConfig, scrub core.Scrubber, pollSeconds float64) *logBrain {
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
		params:    resolveSpikeParams(cat, pollSeconds),
		scrubber:  scrub,
		streak:    make(map[string]int),
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

// Expected returns the pre-fold baseline for a pattern at ts. It is the mean
// and standard deviation the spike detector scores against — the current
// seasonal bucket's rate once that bucket is confident, otherwise the global
// baseline. Logs have no per-key training gate (the lifecycle gates on MODE,
// not a confidence flag), so confident is always true — log novelty is
// expressed through the Detector's VerdictUnknown, not through suppression.
func (b *logBrain) Expected(ctx context.Context, key string, ts time.Time) (mean, std float64, confident bool) {
	p := b.catalog.Get(key)
	if p == nil {
		return 0, 0, true
	}
	mean, std = b.baselineExpected(p, ts)
	return mean, std, true
}

// baselineExpected picks the mean/std to score against at ts per the pattern's
// baseline mode. It reuses resolveBaseline (which also produces the human label
// the detector names in its explanation) and drops the label.
func (b *logBrain) baselineExpected(p *Pattern, ts time.Time) (mean, std float64) {
	mean, std, _ = b.resolveBaseline(p, ts)
	return mean, std
}

// resolveBaseline selects WHICH learned baseline the spike z-score is measured
// against for this pattern at ts, and returns a human label naming it for the
// explanation string. The mode is the pattern's own spike_baseline_mode when
// set, else the config default; an unrecognized value folds to "default".
//
// Time-of-day buckets are always folded, so any mode can be chosen with no
// re-learn:
//   - default     → the global rate baseline (mean = baseline_frequency,
//     spread = its variance); no label (reads as the plain baseline).
//   - average     → the cumulative arithmetic mean of the rate as the CENTER,
//     reusing the global EWMA spread as the scale. There is deliberately one
//     dispersion source: the arithmetic average is the center, the EWMA spread
//     is the wobble, so "average" and "default" differ only in the center.
//   - time_of_day → the current hour-of-day bucket once it has enough samples,
//     else the global baseline (a sparse hour never fires a spurious spike).
func (b *logBrain) resolveBaseline(p *Pattern, ts time.Time) (mean, std float64, label string) {
	global := stats.EWMA{Mean: p.BaselineFrequency, Variance: p.BaselineVariance, Count: p.Count}
	// The stored GLOBAL default applies unless the pattern carries its own
	// explicit override.
	mode := b.globalBaselineMode()
	if p.SpikeBaselineMode != "" {
		mode = normalizeBaselineMode(p.SpikeBaselineMode)
	}
	switch mode {
	case baselineModeAverage:
		return p.BaselineAvg, global.Std(), "the average baseline"
	case baselineModeTimeOfDay:
		if len(p.Seasonal) == stats.HoursPerDay {
			idx := stats.SeasonalIndex(ts, stats.HoursPerDay)
			bucket := p.Seasonal[idx]
			if bucket.Count >= b.params.MinBucketSamples {
				return bucket.Mean, bucket.Std(), fmt.Sprintf("the %02d:00 baseline", idx)
			}
		}
		// Sparse (or not-yet-folded) hour: fall back to the global baseline.
		return global.Mean, global.Std(), "the baseline"
	default:
		return global.Mean, global.Std(), ""
	}
}

// globalBaselineMode returns the operator-configured GLOBAL default baseline
// mode, read through the storage-backed spike settings on every call — the
// same read-through the report settings use, so there is no process-wide
// mutable settings global. A nil catalog (or an install with no persistence)
// yields the built-in default.
func (b *logBrain) globalBaselineMode() string {
	if b.catalog == nil {
		return baselineModeDefault
	}
	return LoadSpikeSettings(b.catalog.store).BaselineMode
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
		// Capture the representative POST-REDACTION example line for this
		// pattern. o.Samples[0].Message was already scrubbed by the worker before
		// Group ran; Signal.Raw is never read. RecordSample re-scrubs + caps it
		// and rides the same dirty→Persist path as the Upsert above.
		if len(o.Samples) > 0 {
			b.catalog.RecordSample(o.Key, o.Samples[0].Message, b.scrubber)
		}
	}
	return nil
}

// Classify scores an already-folded observation against the pre-fold baseline
// snapshot (mean/std captured by Expected before Learn ran). A known pattern
// that is not spiking is normal; a burst — the tick's per-second rate sitting
// spike_z standard deviations above the learned normal, or crossing the
// absolute ceiling — supersedes "known" and flows to detect-AI. Logs always
// report Confident=true (see Expected); the worker suppresses a non-spiking
// known pattern via the VerdictKnownPattern gate.
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

	// The effective auto-promotion threshold gates count-based promotion: once
	// a pattern's sighting count reaches it, the pattern is marked "known" and
	// stops flowing to detect-AI on count alone. A non-positive configured
	// value is resolved to the default by isLogKnown (there is no way to turn
	// count-based promotion off), and an omitted key already arrives here as the
	// default via the config layer. A pattern already promoted to "known" stays
	// known regardless of threshold.
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
		res := evalSpike(mean, std, prevCount, tickFreq, b.params)
		if b.confirmSpike(obs.Key, res) {
			// Name the baseline the tick was scored against in the explanation.
			// The scored mean/std are the pre-fold snapshot the worker captured
			// (passed in); only the textual label — which mode, and the hour for
			// time-of-day — is re-derived from the post-fold pattern here.
			_, _, label := b.resolveBaseline(p, obs.Timestamp)
			return core.TypedVerdict{
				Class:     core.VerdictSpike,
				Confident: true,
				Score:     res.Z,
				Baseline:  prevBaseline,
				Reason:    spikeReason(res, mean, std, tickFreq, b.params.PollSeconds, label),
			}
		}
		return core.TypedVerdict{Class: core.VerdictKnownPattern, Confident: true, Baseline: prevBaseline}
	}
	return core.TypedVerdict{Class: core.VerdictUnknown, Confident: true, Baseline: prevBaseline}
}

// confirmSpike applies the sustained-tick debounce to a spike candidate. A
// non-spike resets the streak. An absolute-ceiling hit fires immediately (the
// deterministic safety net bypasses the debounce). Otherwise the z-score spike
// must persist for spike_sustain_ticks consecutive ticks before firing; the
// default of 1 fires on the first tick (no debounce). Once sustained it keeps
// firing every tick until a non-spiking tick resets the streak.
func (b *logBrain) confirmSpike(key string, res spikeResult) bool {
	b.spikeMu.Lock()
	defer b.spikeMu.Unlock()
	if !res.Spike {
		delete(b.streak, key)
		return false
	}
	if res.Ceiling {
		return true
	}
	b.streak[key]++
	return b.streak[key] >= b.params.SustainTicks
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
// A non-positive AutoPromoteAfter is resolved to the default by isLogKnown, so
// count-based promotion is always in effect; there is no "promotion disabled"
// state.
func (b *logBrain) Promote(key string) {
	p := b.catalog.Get(key)
	if p == nil {
		return
	}
	if p.Verdict != "known" && isLogKnown(p.Verdict, p.Count, b.cat.AutoPromoteAfter) {
		b.catalog.MarkKnown(key)
	}
}

// effectiveAutoPromote resolves the auto-promotion threshold used everywhere in
// the learning engine. A non-positive value (an operator who omitted, blanked,
// or zeroed the key, or a test that passes 0 directly) is treated as the
// default rather than as a "promotion disabled" state — there is no way to turn
// count-based promotion off. The config loader already normalizes the loaded
// config to a positive value; this is the belt-and-suspenders guard so a 0 that
// reaches the engine by any other path still resolves to a sane gate.
func effectiveAutoPromote(n int) int {
	if n <= 0 {
		return config.DefaultAutoPromoteAfter
	}
	return n
}

// isLogKnown is the SINGLE definition of log "known" — the exact predicate
// logBrain.Classify gates promotion on. Both Classify and LogReadiness call it
// so the classifier and the read-side readiness view can never drift; a drift
// test pins them equal. A pattern is known when an operator (or a prior
// auto-promotion) already marked it "known", OR the sighting count has reached
// the effective auto-promotion threshold. A non-positive autoPromoteAfter is
// resolved to the default (see effectiveAutoPromote), so count-based promotion
// is always in effect.
func isLogKnown(prevVerdict string, count, autoPromoteAfter int) bool {
	return prevVerdict == "known" || count >= effectiveAutoPromote(autoPromoteAfter)
}

// LogReadiness computes the readiness of one log pattern as a generic
// core.Readiness — the read-side view of "how close is this pattern to being
// treated as routine baseline (known)". It is a pure function that does NOT
// mutate the pattern: it reads the pattern's own counters (the same ones the
// gate compares) and the catalog's AutoPromoteAfter threshold.
//
//   - Seen   = the pattern's sighting count (the gate's own counter).
//   - Needed = the effective auto-promotion threshold — always a positive gate
//     (a non-positive configured value is resolved to the default), so there is
//     no indeterminate/manual-only case.
//   - Ready  = isLogKnown(...), so an already-"known"/"spike"-marked or
//     count-promoted pattern reports Ready.
//   - RatePerMin = the pattern's learned per-second sighting rate
//     (BaselineFrequency) rendered as sightings/minute (rate × 60), used to
//     derive an ETA. 0 is the stalled/unknown sentinel (no rate yet, no flow,
//     or no poll interval wired) — the UI then shows no ETA. It is only computed
//     while the pattern is not yet Ready, since a Ready pattern has no countdown.
func LogReadiness(p *Pattern, autoPromoteAfter int, pollInterval time.Duration) core.Readiness {
	if p == nil {
		return core.Readiness{}
	}
	r := core.Readiness{
		Seen:   p.Count,
		Needed: effectiveAutoPromote(autoPromoteAfter),
		Ready:  isLogKnown(p.Verdict, p.Count, autoPromoteAfter),
	}
	if !r.Ready && pollInterval > 0 && p.BaselineFrequency > 0 {
		// BaselineFrequency is now a per-second sighting rate, so sightings/min
		// is just rate × 60 — poll-interval-independent. pollInterval is kept as
		// the "worker wired?" guard: unset (0) means no live agent, so degrade
		// to no ETA.
		r.RatePerMin = p.BaselineFrequency * 60
	}
	return r
}
