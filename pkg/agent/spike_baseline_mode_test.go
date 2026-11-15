package agent

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/stats"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// twoAM is a fixed 02:00 UTC instant so the hour-of-day bucket the detector
// scores against is deterministic (bucket index 2).
var twoAM = time.Date(2026, 1, 2, 2, 0, 0, 0, time.UTC)

// modeTestPattern builds a pattern whose three baselines each read a distinct,
// clean number so a mode selection is unambiguous:
//
//	global rate    = 10/s ± 2   (default mode)
//	average rate   = 20/s       (average mode; spread reuses the global ± 2)
//	02:00 bucket   = 44/s ± 2   (time_of_day mode at 02:00)
func modeTestPattern() *Pattern {
	seasonal := make([]stats.EWMA, stats.HoursPerDay)
	seasonal[2] = stats.EWMA{Mean: 44, Variance: 4, Count: 100}
	return &Pattern{
		ID:                "k",
		BaselineFrequency: 10,
		BaselineVariance:  4,
		BaselineAvg:       20,
		Seasonal:          seasonal,
		Count:             200,
	}
}

// TestResolveBaselineModes proves each mode selects the baseline the founder's
// delta specifies — global for default, the arithmetic average (with the global
// spread) for average, and the current hour-of-day bucket for time_of_day — and
// that the explanation label names the baseline used.
func TestResolveBaselineModes(t *testing.T) {
	tests := []struct {
		mode      string
		wantMean  float64
		wantStd   float64
		wantLabel string
	}{
		{"default", 10, 2, ""},
		{"average", 20, 2, "the average baseline"},
		{"time_of_day", 44, 2, "the 02:00 baseline"},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			b, c := newLogBrainForTest(t, config.AgentCatalogConfig{})
			setGlobalBaselineMode(t, c, tc.mode)
			mean, std, label := b.resolveBaseline(modeTestPattern(), twoAM)
			if mean != tc.wantMean || std != tc.wantStd || label != tc.wantLabel {
				t.Fatalf("resolveBaseline(%s) = (%.1f, %.1f, %q), want (%.1f, %.1f, %q)",
					tc.mode, mean, std, label, tc.wantMean, tc.wantStd, tc.wantLabel)
			}
		})
	}
}

// TestResolveBaselinePatternOverride proves a per-pattern spike_baseline_mode
// wins over the stored global default, and that an unset pattern property
// inherits it.
func TestResolveBaselinePatternOverride(t *testing.T) {
	b, c := newLogBrainForTest(t, config.AgentCatalogConfig{})
	setGlobalBaselineMode(t, c, "default")

	p := modeTestPattern()
	p.SpikeBaselineMode = "average"
	if mean, _, label := b.resolveBaseline(p, twoAM); mean != 20 || label != "the average baseline" {
		t.Fatalf("pattern override average = (%.1f, %q), want (20, the average baseline)", mean, label)
	}

	p.SpikeBaselineMode = "" // inherit the stored global default (global)
	if mean, _, label := b.resolveBaseline(p, twoAM); mean != 10 || label != "" {
		t.Fatalf("inherited default = (%.1f, %q), want (10, \"\")", mean, label)
	}
}

// TestResolveBaselineUnknownNormalizes proves an unrecognized stored global mode
// (or per-pattern value) folds to the global default rather than silently
// disabling scoring. The bogus value is written as a raw blob to bypass the
// save-side sanitize and exercise the read-side normalization.
func TestResolveBaselineUnknownNormalizes(t *testing.T) {
	b, c := newLogBrainForTest(t, config.AgentCatalogConfig{})
	if err := c.store.WriteBlob(SpikeSettingsBlobName, []byte(`{"baseline_mode":"wat"}`)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if mean, _, label := b.resolveBaseline(modeTestPattern(), twoAM); mean != 10 || label != "" {
		t.Fatalf("unknown stored mode = (%.1f, %q), want (10, \"\") (default)", mean, label)
	}
}

// TestResolveBaselineTimeOfDayFallback proves a sparse hour-of-day bucket (below
// the confidence gate) falls back to the global baseline, so switching a
// still-warming pattern to time_of_day never fires a spurious cold-bucket spike.
func TestResolveBaselineTimeOfDayFallback(t *testing.T) {
	b, c := newLogBrainForTest(t, config.AgentCatalogConfig{})
	setGlobalBaselineMode(t, c, "time_of_day")
	p := modeTestPattern()
	p.Seasonal[2] = stats.EWMA{Mean: 44, Variance: 4, Count: 2} // below MinBucketSamples (5)
	if mean, _, label := b.resolveBaseline(p, twoAM); mean != 10 || label != "the baseline" {
		t.Fatalf("sparse bucket fallback = (%.1f, %q), want (10, the baseline)", mean, label)
	}
}

// TestSpikeBaselineModeFireHold proves the SAME burst is judged differently per
// mode: a tick of 47/s at 02:00 fires against the global and average baselines
// but is normal-for-2am against the hour-of-day bucket (44/s ± 2), so
// time_of_day holds. The explanation names the baseline that fired.
func TestSpikeBaselineModeFireHold(t *testing.T) {
	const poll = 30.0
	tickFreq := int(47 * poll) // 47/s

	tests := []struct {
		mode       string
		wantSpike  bool
		wantReason string // substring the explanation must contain when it fires
	}{
		{"default", true, "above 10.0/s"},
		{"average", true, "above the average baseline 20.0/s"},
		{"time_of_day", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			b, c := newLogBrainForTest(t, config.AgentCatalogConfig{})
			setGlobalBaselineMode(t, c, tc.mode)
			p := modeTestPattern()
			mean, std, label := b.resolveBaseline(p, twoAM)
			res := evalSpike(mean, std, p.Count, tickFreq, b.params)
			if res.Spike != tc.wantSpike {
				t.Fatalf("mode %s: spike = %v (z=%.1f), want %v", tc.mode, res.Spike, res.Z, tc.wantSpike)
			}
			if res.Spike {
				reason := spikeReason(res, mean, std, tickFreq, poll, label)
				if !strings.Contains(reason, tc.wantReason) {
					t.Fatalf("mode %s reason = %q, want it to contain %q", tc.mode, reason, tc.wantReason)
				}
			}
		})
	}
}

// TestFoldBaselineAvg proves the cumulative arithmetic mean is folded correctly
// and incrementally: after a first tick at 1/s, ticks at 2/s, 3/s, 4/s leave
// baseline_avg at their arithmetic mean (3.0) — the first tick only stands in
// until a second arrives — and the fold count equals the total seasonal
// observation count.
func TestFoldBaselineAvg(t *testing.T) {
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	c.SetBaselineFold(resolveSpikeParams(config.AgentCatalogConfig{}, 30).fold())

	c.Upsert("k", "tmpl", "src", 30, 0.2, "default", "") // 1/s at a 30s poll
	if got := c.Get("k").BaselineAvg; math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("BaselineAvg after the first tick = %v, want 1.0", got)
	}
	for _, tickCount := range []int{60, 90, 120} { // rates 2/s, 3/s, 4/s
		c.Upsert("k", "tmpl", "src", tickCount, 0.2, "default", "")
	}

	p := c.Get("k")
	if got := p.BaselineAvg; math.Abs(got-3.0) > 1e-9 {
		t.Fatalf("BaselineAvg = %v, want 3.0 (mean of 2,3,4)", got)
	}
	if n := seasonalCount(p.Seasonal); n != 4 {
		t.Fatalf("seasonal fold count = %d, want 4 (one per accepted tick)", n)
	}
}

// A first tick that reads a whole lookback window (a burst of history in one
// tick) must not freeze the learned rate: later steady ticks have to pull the
// baseline down to the real rate instead of being held out as outliers.
func TestFoldBaselineConvergesAfterBackfillFirstTick(t *testing.T) {
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	c.SetBaselineFold(resolveSpikeParams(config.AgentCatalogConfig{}, 30).fold())

	c.Upsert("k", "tmpl", "src", 100, 0.2, "default", "") // 5 minutes of history: 3.33/s
	for i := 0; i < 30; i++ {
		c.Upsert("k", "tmpl", "src", 15, 0.2, "default", "") // steady 0.5/s
	}

	p := c.Get("k")
	if got := p.BaselineFrequency; math.Abs(got-0.5) > 0.05 {
		t.Fatalf("BaselineFrequency = %v after 30 steady ticks, want ≈ 0.5 (the steady rate)", got)
	}
	if got := p.BaselineAvg; math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("BaselineAvg = %v after 30 steady ticks, want 0.5 (the burst left out)", got)
	}
}

// The warm-up before the hold-out applies without seasonal buckets too.
func TestFoldBaselineConvergesAfterBackfillFirstTickWithoutSeasonal(t *testing.T) {
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	c.SetBaselineFold(BaselineFold{PollSeconds: 30, RejectZ: 3, MinBaselineCount: 20, MinBucketSamples: 5})

	c.Upsert("k", "tmpl", "src", 100, 0.2, "default", "")
	for i := 0; i < 30; i++ {
		c.Upsert("k", "tmpl", "src", 15, 0.2, "default", "")
	}
	if got := c.Get("k").BaselineFrequency; math.Abs(got-0.5) > 0.05 {
		t.Fatalf("BaselineFrequency = %v after 30 steady ticks, want ≈ 0.5", got)
	}

	before := c.Get("k").BaselineFrequency
	c.Upsert("k", "tmpl", "src", 3000, 0.2, "default", "")
	if after := c.Get("k").BaselineFrequency; after != before {
		t.Fatalf("BaselineFrequency moved %v → %v on a burst after warm-up; want it held out", before, after)
	}
}

// Once the baseline has warmed up on steady ticks, a real burst is still held
// out of the global fold.
func TestFoldBaselineStillHoldsOutBurstAfterWarmUp(t *testing.T) {
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	c.SetBaselineFold(resolveSpikeParams(config.AgentCatalogConfig{}, 30).fold())

	for i := 0; i < 20; i++ {
		c.Upsert("k", "tmpl", "src", 14+i%3, 0.2, "default", "") // ≈ 0.5/s with a little wobble
	}
	before := c.Get("k").BaselineFrequency
	c.Upsert("k", "tmpl", "src", 3000, 0.2, "default", "") // 100/s burst
	if after := c.Get("k").BaselineFrequency; after != before {
		t.Fatalf("BaselineFrequency moved %v → %v on a burst after warm-up; want it held out", before, after)
	}
}

// Deleting or resetting a pattern clears its warm-up, so relearning it under
// the same ID after a backfill first tick still converges instead of holding
// every steady tick out against the burst seed.
func TestFoldBaselineRelearnAfterDeleteRestartsWarmUp(t *testing.T) {
	for _, clear := range []struct {
		name string
		run  func(c *Catalog)
	}{
		{"delete", func(c *Catalog) { c.Delete("k") }},
		{"reset", func(c *Catalog) {
			if _, err := c.ResetPatterns(); err != nil {
				t.Fatalf("ResetPatterns: %v", err)
			}
		}},
	} {
		t.Run(clear.name, func(t *testing.T) {
			c, err := LoadCatalog(storage.NewMemory())
			if err != nil {
				t.Fatalf("LoadCatalog: %v", err)
			}
			c.SetBaselineFold(BaselineFold{PollSeconds: 30, RejectZ: 3, MinBaselineCount: 20, MinBucketSamples: 5})
			for i := 0; i < 10; i++ {
				c.Upsert("k", "tmpl", "src", 15, 0.2, "default", "")
			}
			clear.run(c)

			c.Upsert("k", "tmpl", "src", 100, 0.2, "default", "") // backfill burst: 3.33/s
			for i := 0; i < 30; i++ {
				c.Upsert("k", "tmpl", "src", 15, 0.2, "default", "")
			}
			if got := c.Get("k").BaselineFrequency; math.Abs(got-0.5) > 0.05 {
				t.Fatalf("BaselineFrequency = %v after relearning, want ≈ 0.5", got)
			}
		})
	}
}
