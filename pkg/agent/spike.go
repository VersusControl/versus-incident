package agent

import (
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/stats"
)

// spike.go — the log volume-spike detector.
//
// A known log pattern is re-flagged as a spike when its per-second match RATE
// jumps well above the learned normal. The bar is a z-score — how many standard
// deviations above the baseline this tick sits — so it self-scales to each
// pattern's own volatility: a +1000 burst on a chatty, high-volume pattern
// trips even though its mean is large, which the old volume-blind
// mean × multiplier ratio could not. An absolute ceiling is a deterministic
// safety net that always surfaces a tick past a hard count, even during warmup.
//
// All inputs are the numbers the fold already tracks (rate, mean, std) so a
// verdict is fully reconstructable from stored state — deterministic and
// auditable, no LLM in the decision.

// logMinBucketSamples is how many observations a seasonal bucket needs before
// the detector scores against it directly (below it, it falls back to the
// global baseline). Mirrors the enterprise brain's bucket-confidence gate.
const logMinBucketSamples = 5

// Baseline modes select WHICH learned baseline the spike z-score is measured
// against. Seasonal buckets are always folded, so the mode can be flipped with
// no re-learn.
const (
	// baselineModeDefault scores against the global rate baseline: mean =
	// baseline_frequency, spread = baseline_variance.
	baselineModeDefault = "default"
	// baselineModeAverage scores against the cumulative arithmetic mean of the
	// rate as the center, reusing the global EWMA spread as the scale.
	baselineModeAverage = "average"
	// baselineModeTimeOfDay scores against the current hour-of-day bucket
	// (24 UTC buckets), falling back to the global baseline until that hour is
	// confident.
	baselineModeTimeOfDay = "time_of_day"
)

// normalizeBaselineMode maps a raw config/pattern mode value to one of the
// three known modes, folding any unset or unrecognized value to the global
// default so a typo can never silently disable scoring.
func normalizeBaselineMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case baselineModeAverage:
		return baselineModeAverage
	case baselineModeTimeOfDay:
		return baselineModeTimeOfDay
	default:
		return baselineModeDefault
	}
}

// spikeParams are the resolved (defaults applied) spike-detection knobs. They
// drive both the baseline fold (the outlier-reject cutoff, the seasonal period)
// and the detector, so the two paths agree on what "normal" and "Nσ" mean.
type spikeParams struct {
	Z                float64 // z-score threshold; also the fold's outlier-reject cutoff
	AbsCeiling       int     // absolute safety net; 0 = disabled
	SustainTicks     int     // consecutive spiking ticks before firing; 1 = no debounce
	MinFrequency     int     // absolute noise floor on the tick count
	MinBaselineCount int     // warmup/confidence gate on total sightings
	PollSeconds      float64 // rate = tickCount / PollSeconds; 0 = score the raw count
	SeasonalPeriod   int     // hour-of-day buckets folded every tick (always 24 for logs)
	MinBucketSamples int     // seasonal bucket confidence gate
}

// resolveSpikeParams reads the catalog config, applies the founder-approved
// defaults for any unset key, and folds in the worker's poll interval.
func resolveSpikeParams(cat config.AgentCatalogConfig, pollSeconds float64) spikeParams {
	p := spikeParams{
		Z:                cat.SpikeZ,
		AbsCeiling:       cat.SpikeAbsCeiling,
		SustainTicks:     cat.SpikeSustainTicks,
		MinFrequency:     cat.SpikeMinFrequency,
		MinBaselineCount: cat.SpikeMinBaselineCount,
		PollSeconds:      pollSeconds,
		// Time-of-day seasonal is always folded, fixed to hour-of-day. The
		// operator picks whether to SCORE against it via the stored baseline
		// mode, but the 24 buckets are always kept up to date so the choice
		// needs no re-learn.
		SeasonalPeriod:   stats.HoursPerDay,
		MinBucketSamples: logMinBucketSamples,
	}
	if p.Z <= 0 {
		p.Z = config.DefaultSpikeZ
	}
	if p.SustainTicks <= 0 {
		p.SustainTicks = config.DefaultSpikeSustainTicks
	}
	if p.MinFrequency <= 0 {
		p.MinFrequency = config.DefaultSpikeMinFrequency
	}
	if p.MinBaselineCount <= 0 {
		p.MinBaselineCount = config.DefaultSpikeMinBaselineCount
	}
	if p.AbsCeiling < 0 {
		p.AbsCeiling = 0
	}
	return p
}

// fold derives the baseline-fold configuration from the spike params so the
// fold's outlier-reject cutoff, seasonal period, and confidence gates match the
// detector's exactly.
func (p spikeParams) fold() BaselineFold {
	return BaselineFold{
		PollSeconds:      p.PollSeconds,
		RejectZ:          p.Z,
		SeasonalPeriod:   p.SeasonalPeriod,
		MinBaselineCount: p.MinBaselineCount,
		MinBucketSamples: p.MinBucketSamples,
	}
}

// spikeResult is the detector's read on one tick: whether it is a spike, the
// z-score of its rate, the rate itself, and whether it tripped the absolute
// ceiling (which bypasses the sustained-tick debounce, being the hard net).
type spikeResult struct {
	Spike   bool
	Ceiling bool
	Z       float64
	Rate    float64
}

// evalSpike scores one tick against the pre-fold baseline (mean/std for the
// tick's time slot). It is pure: the same inputs always yield the same result.
//
//	rate  = tickFreq / pollSeconds (per-second; poll-independent)
//	z     = (rate − mean) / max(std, floor)
//	spike = (confident && z ≥ Z) || (AbsCeiling>0 && tickFreq ≥ AbsCeiling)
//
// The absolute ceiling fires regardless of the warmup gate. The min-frequency
// floor keeps a low-count pattern from paging on a handful of matches, and the
// warmup gate (total sightings ≥ MinBaselineCount) holds the z-score until the
// baseline is trustworthy.
func evalSpike(mean, std float64, prevCount, tickFreq int, p spikeParams) spikeResult {
	rate := float64(tickFreq)
	if p.PollSeconds > 0 {
		rate = float64(tickFreq) / p.PollSeconds
	}
	res := spikeResult{Z: stats.ZScore(rate, mean, std), Rate: rate}

	// Absolute ceiling — the deterministic "always page above N" net, valid
	// even during warmup.
	if p.AbsCeiling > 0 && tickFreq >= p.AbsCeiling {
		res.Spike = true
		res.Ceiling = true
		return res
	}

	// Absolute noise floor.
	if tickFreq < p.MinFrequency {
		return res
	}
	// Warmup/confidence gate + real baseline, then the z-score bar.
	confident := prevCount >= p.MinBaselineCount && mean > 0
	if confident && res.Z >= p.Z {
		res.Spike = true
	}
	return res
}

// spikeReason renders the deterministic, human-readable explanation stored in
// the detect audit log, e.g. "47.3/s = 4.2σ above the 02:00 baseline 38.4/s
// ± 3.1". The baselineLabel names which baseline was scored (empty for the
// global default, "the average baseline", or "the HH:00 baseline"); it is fully
// reconstructable from the stored numbers.
func spikeReason(res spikeResult, mean, std float64, tickFreq int, pollSeconds float64, baselineLabel string) string {
	if res.Ceiling && !(pollSeconds > 0 && res.Z >= 0) {
		// Ceiling fired during warmup (no trustworthy baseline to compare σ to).
		if pollSeconds > 0 {
			return fmt.Sprintf("%.1f/s (%d matches) at or above the absolute ceiling", res.Rate, tickFreq)
		}
		return fmt.Sprintf("%d matches at or above the absolute ceiling", tickFreq)
	}
	// A named baseline (average / time-of-day) is inserted between "above" and
	// the numbers so the operator can see which normal the tick was judged
	// against; the global default carries no label and reads as before.
	label := ""
	if baselineLabel != "" {
		label = baselineLabel + " "
	}
	if pollSeconds > 0 {
		return fmt.Sprintf("%.1f/s = %.1fσ above %s%.1f/s ± %.1f", res.Rate, res.Z, label, mean, std)
	}
	return fmt.Sprintf("%d = %.1fσ above %s%.1f ± %.1f", tickFreq, res.Z, label, mean, std)
}
