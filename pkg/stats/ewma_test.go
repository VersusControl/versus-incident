package stats

import (
	"math"
	"testing"
	"time"
)

// TestEWMAObserveSeedThenTrack proves the first observation seeds the mean at
// the value with zero variance, and later observations run the incremental
// mean/variance update (recent samples weighted by alpha).
func TestEWMAObserveSeedThenTrack(t *testing.T) {
	var e EWMA
	e.Observe(10, 0.2)
	if e.Mean != 10 || e.Variance != 0 || e.Count != 1 {
		t.Fatalf("seed: got mean=%v var=%v count=%d, want 10/0/1", e.Mean, e.Variance, e.Count)
	}
	// mean += alpha*(value-mean) = 10 + 0.2*(20-10) = 12.
	e.Observe(20, 0.2)
	if math.Abs(e.Mean-12) > 1e-9 {
		t.Fatalf("mean after second fold = %v, want 12", e.Mean)
	}
	if e.Variance <= 0 {
		t.Fatalf("variance after a jump = %v, want > 0", e.Variance)
	}
	if e.Count != 2 {
		t.Fatalf("count = %d, want 2", e.Count)
	}
}

// TestEWMAConvergesToConstant proves a steady stream converges the mean to the
// level and the variance toward zero.
func TestEWMAConvergesToConstant(t *testing.T) {
	var e EWMA
	for i := 0; i < 200; i++ {
		e.Observe(50, 0.3)
	}
	if math.Abs(e.Mean-50) > 1e-6 {
		t.Fatalf("mean = %v, want ~50", e.Mean)
	}
	if e.Std() > 1e-3 {
		t.Fatalf("std = %v, want ~0 on a constant stream", e.Std())
	}
}

// TestZScoreSpreadFloor proves the 1% relative floor keeps a near-constant
// series from producing an infinite z-score.
func TestZScoreSpreadFloor(t *testing.T) {
	// mean 100, std 0 → floor = 1% of 100 = 1 → z = (110-100)/1 = 10.
	if z := ZScore(110, 100, 0); math.Abs(z-10) > 1e-9 {
		t.Fatalf("z = %v, want 10 (spread floor 1%% of mean)", z)
	}
	// mean 0, std 0 → floor = 1e-9, finite (not NaN/Inf).
	if z := ZScore(1, 0, 0); math.IsInf(z, 0) || math.IsNaN(z) {
		t.Fatalf("z = %v, want a finite value even at mean=std=0", z)
	}
	// A real spread is used when it exceeds the floor.
	if z := ZScore(120, 100, 5); math.Abs(z-4) > 1e-9 {
		t.Fatalf("z = %v, want 4 (20/5)", z)
	}
}

// TestShouldReject proves the outlier hold-out fires only once confident and
// only beyond the cutoff, and is disabled by a non-positive cutoff.
func TestShouldReject(t *testing.T) {
	// Not confident (cold start) → never reject.
	if ShouldReject(1000, 100, 5, false, 3) {
		t.Fatal("cold-start reject: want false, folds every sample while warming")
	}
	// Confident and 4σ out with a 3σ cutoff → reject.
	if !ShouldReject(120, 100, 5, true, 3) {
		t.Fatal("confident 4σ outlier: want reject")
	}
	// Confident but within the cutoff → keep.
	if ShouldReject(110, 100, 5, true, 3) {
		t.Fatal("confident 2σ sample: want keep")
	}
	// Cutoff disabled → never reject.
	if ShouldReject(1e9, 100, 5, true, 0) {
		t.Fatal("rejectZ<=0: want no rejection")
	}
}

// TestSeasonalIndex proves hour-of-day (24) and hour-of-week (168) bucketing in
// UTC, plus the disabled (period<=0) and modulo fallbacks.
func TestSeasonalIndex(t *testing.T) {
	// 2026-07-06 is a Monday; 14:30 UTC.
	ts := time.Date(2026, 7, 6, 14, 30, 0, 0, time.UTC)
	if got := SeasonalIndex(ts, HoursPerDay); got != 14 {
		t.Fatalf("hour-of-day index = %d, want 14", got)
	}
	// Monday = Weekday()==1 → 1*24 + 14 = 38.
	if got := SeasonalIndex(ts, HoursPerWeek); got != 38 {
		t.Fatalf("hour-of-week index = %d, want 38", got)
	}
	// A non-UTC zone with the same instant buckets identically (UTC-stable).
	loc := time.FixedZone("UTC-5", -5*3600)
	if got := SeasonalIndex(ts.In(loc), HoursPerDay); got != 14 {
		t.Fatalf("hour-of-day index in another zone = %d, want 14 (UTC-bucketed)", got)
	}
	if got := SeasonalIndex(ts, 0); got != 0 {
		t.Fatalf("disabled period index = %d, want 0", got)
	}
}

// TestExpectedSeasonalFallback proves Expected uses the seasonal bucket once it
// is confident and otherwise falls back to the global level, gating on the
// global warmup.
func TestExpectedSeasonalFallback(t *testing.T) {
	ts := time.Date(2026, 7, 6, 2, 0, 0, 0, time.UTC) // 02:00 bucket (hour-of-day 2)
	seasonal := make([]EWMA, HoursPerDay)

	// Global not yet warm (count < minGlobal) → not confident.
	global := EWMA{Mean: 40, Variance: 4, Count: 10}
	if _, _, confident := Expected(global, seasonal, ts, HoursPerDay, 5, 20); confident {
		t.Fatal("global below warmup: want confident=false")
	}

	// Global warm, bucket sparse → confident, scores against the global level.
	global.Count = 30
	mean, _, confident := Expected(global, seasonal, ts, HoursPerDay, 5, 20)
	if !confident || mean != 40 {
		t.Fatalf("global fallback: got mean=%v confident=%v, want 40/true", mean, confident)
	}

	// Bucket now confident → scores against the bucket, not the global.
	seasonal[2] = EWMA{Mean: 120, Variance: 9, Count: 8}
	mean, std, confident := Expected(global, seasonal, ts, HoursPerDay, 5, 20)
	if !confident || mean != 120 || math.Abs(std-3) > 1e-9 {
		t.Fatalf("bucket scoring: got mean=%v std=%v confident=%v, want 120/3/true", mean, std, confident)
	}
}
