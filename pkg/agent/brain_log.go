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

	threshold := b.cat.AutoPromoteAfter
	if threshold <= 0 {
		threshold = 100
	}
	isKnown := prevVerdict == "known" || postCount >= threshold
	if isKnown {
		if prevVerdict != "known" {
			b.catalog.MarkKnown(obs.Key)
		}
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
