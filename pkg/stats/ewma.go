// Package stats holds the shared, dependency-free numeric primitives every
// signal brain in the suite folds its baseline through: an
// exponentially-weighted mean/variance estimator, the z-score with a spread
// floor, an outlier-reject predicate, and the hour-of-day / hour-of-week
// seasonal bucketing. It lives in the OSS tree so the OSS log brain and the
// enterprise metric/trace brains share ONE implementation of the fold — the
// enterprise module imports this package one-way; this package imports nothing
// from the suite.
//
// All math is local and deterministic (no LLM, no I/O), so a verdict computed
// from these functions is fully reconstructable from the stored numbers.
package stats

import (
	"math"
	"time"
)

// HoursPerDay and HoursPerWeek are the two seasonal periods the suite uses:
// hour-of-day (a daily cycle — batch jobs, business hours) and hour-of-week
// (adds a weekly cycle — weekend-only jobs).
const (
	HoursPerDay  = 24
	HoursPerWeek = 7 * 24
)

// EWMA is an exponentially-weighted running mean and variance in West's
// incremental form. It is O(1) in time and space: each fold updates the mean
// and the running variance from the new sample alone, weighting recent samples
// by alpha. Std is sqrt(Variance).
//
// The JSON tags are the persisted wire shape shared with the enterprise typed
// baseline tables (the `seasonal` JSONB array and the global stat columns), so
// they must not change without a migration.
type EWMA struct {
	Mean     float64 `json:"mean"`
	Variance float64 `json:"variance"`
	Count    int     `json:"count"`
}

// Observe folds value into the running mean/variance with smoothing factor
// alpha. The first observation seeds the mean at value with zero variance;
// subsequent observations run the incremental update.
func (e *EWMA) Observe(value, alpha float64) {
	if e.Count == 0 {
		e.Mean = value
		e.Variance = 0
		e.Count = 1
		return
	}
	diff := value - e.Mean
	incr := alpha * diff
	e.Mean += incr
	// EWMA of the squared deviation — the running variance.
	e.Variance = (1 - alpha) * (e.Variance + diff*incr)
	e.Count++
}

// Std is the standard deviation: sqrt of the (non-negative) variance.
func (e EWMA) Std() float64 { return math.Sqrt(math.Max(0, e.Variance)) }

// ZScore returns how many standard deviations value sits from mean:
// (value - mean) / max(std, floor). The floor is a 1% relative spread (at
// least 1e-9), so a near-constant series can't produce an infinite score and a
// zero-variance baseline can't divide by zero.
func ZScore(value, mean, std float64) float64 {
	floor := math.Abs(mean) * 0.01
	if floor < 1e-9 {
		floor = 1e-9
	}
	if std < floor {
		std = floor
	}
	return (value - mean) / std
}

// ShouldReject reports whether value is a strong enough outlier to be held OUT
// of the fold so a single burst can't drag the baseline off the typical
// sample. It fires only once the estimator is confident (|z| >= rejectZ);
// during cold start it always returns false so the level can form. A rejectZ
// of zero or less disables rejection (every sample folds).
func ShouldReject(value, mean, std float64, confident bool, rejectZ float64) bool {
	if !confident || rejectZ <= 0 {
		return false
	}
	return math.Abs(ZScore(value, mean, std)) >= rejectZ
}

// SeasonalIndex maps ts to its seasonal bucket in [0, period). period 24 is
// hour-of-day; period 168 is hour-of-week (weekday*24 + hour). Any other
// positive period buckets the hour-of-week index modulo period; a period of
// zero or less returns 0 (no seasonality). Bucketing is done in UTC so the
// mapping is host-timezone- and DST-stable and therefore deterministic.
func SeasonalIndex(ts time.Time, period int) int {
	if period <= 0 {
		return 0
	}
	u := ts.UTC()
	switch period {
	case HoursPerDay:
		return u.Hour()
	case HoursPerWeek:
		return int(u.Weekday())*24 + u.Hour()
	default:
		return (int(u.Weekday())*24 + u.Hour()) % period
	}
}

// Expected returns the mean/std to score against at ts and whether the model is
// confident enough to score at all. It uses the current seasonal bucket once
// that bucket has at least minBucketSamples observations, otherwise it falls
// back to the global level — so a sparse bucket never produces a spurious
// verdict. confident stays false until the global level has at least
// minGlobalSamples observations (the warmup gate). A period of zero (or a
// seasonal slice that is not exactly period long) skips the seasonal lookup and
// scores against the global level.
func Expected(global EWMA, seasonal []EWMA, ts time.Time, period, minBucketSamples, minGlobalSamples int) (mean, std float64, confident bool) {
	if period > 0 && len(seasonal) == period {
		bucket := seasonal[SeasonalIndex(ts, period)]
		if minBucketSamples > 0 && bucket.Count >= minBucketSamples {
			return bucket.Mean, bucket.Std(), true
		}
	}
	if global.Count >= minGlobalSamples {
		return global.Mean, global.Std(), true
	}
	return global.Mean, global.Std(), false
}
