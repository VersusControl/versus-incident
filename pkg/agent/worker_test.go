package agent

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// TestEvalSpike covers the z-score volume-spike decision: the founder's
// high-baseline burst FIRES, low-count noise stays floored, and the absolute
// ceiling is a warmup-independent safety net.
func TestEvalSpike(t *testing.T) {
	// 30s tick, defaults (z=3, min_freq=5, min_baseline=20, ceiling off).
	p := resolveSpikeParams(config.AgentCatalogConfig{}, 30)

	tests := []struct {
		name      string
		mean, std float64 // pre-fold baseline (per-second rate)
		prevCount int
		tickFreq  int
		params    spikeParams
		want      bool
	}{
		{
			// The founder's case: a chatty pattern normally ~38/s (± ~1) gets a
			// burst of 2000 matches in a 30s tick → ~67/s → many σ above → FIRES,
			// even though the mean is large (the old mean × 5 ratio went blind
			// here).
			name: "founder high-baseline burst fires", mean: 38.4, std: 1.0,
			prevCount: 10000, tickFreq: 2000, params: p, want: true,
		},
		{
			// Low-count noise: baseline ~0.02/s, a tick of 3 is many σ but below
			// the absolute min-frequency floor → stays quiet.
			name: "low-count noise stays floored", mean: 0.02, std: 0.01,
			prevCount: 500, tickFreq: 3, params: p, want: false,
		},
		{
			// A steady high-volume pattern with a normal-sized tick is not a
			// spike: ~38.6/s vs a 38.4/s ± 2 baseline is well within band.
			name: "steady tick within band", mean: 38.4, std: 2.0,
			prevCount: 10000, tickFreq: 1160, params: p, want: false,
		},
		{
			// Warmup: not enough total sightings yet → the z-score is not
			// trusted, so even a big jump is held (the ceiling is its cover).
			name: "under warmup gate does not fire on z", mean: 1.0, std: 0.1,
			prevCount: 10, tickFreq: 100, params: p, want: false,
		},
		{
			// Absolute ceiling: fires regardless of warmup or z-score.
			name: "absolute ceiling fires during warmup", mean: 1.0, std: 0.1,
			prevCount: 2, tickFreq: 1000,
			params: resolveSpikeParams(config.AgentCatalogConfig{SpikeAbsCeiling: 500}, 30),
			want:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := evalSpike(tc.mean, tc.std, tc.prevCount, tc.tickFreq, tc.params).Spike
			if got != tc.want {
				t.Errorf("evalSpike(mean=%v std=%v prevCount=%d tickFreq=%d) = %v, want %v",
					tc.mean, tc.std, tc.prevCount, tc.tickFreq, got, tc.want)
			}
		})
	}
}

// TestEvalSpikePollIndependence proves the same log stream trips the same way
// regardless of poll interval, because the score is a per-second rate. A
// pattern normally ~10/s that jumps to ~40/s is a spike whether polled at 10s,
// 30s, or 60s, and the scored rate is the same.
func TestEvalSpikePollIndependence(t *testing.T) {
	mean, std := 10.0, 1.0 // 10/s baseline
	for _, poll := range []float64{10, 30, 60} {
		p := resolveSpikeParams(config.AgentCatalogConfig{}, poll)
		tickFreq := int(40 * poll) // 40/s for this tick = 40 * poll matches
		res := evalSpike(mean, std, 10000, tickFreq, p)
		if !res.Spike {
			t.Errorf("poll=%vs: 40/s burst should fire (z=%.1f)", poll, res.Z)
		}
		if res.Rate < 39 || res.Rate > 41 {
			t.Errorf("poll=%vs: rate = %.2f/s, want ~40 (poll-independent)", poll, res.Rate)
		}
	}
}

// TestConfirmSpikeSustain proves the sustained-tick debounce holds a single
// noisy tick and fires only once the streak reaches spike_sustain_ticks, and
// that an absolute-ceiling hit bypasses the debounce entirely.
func TestConfirmSpikeSustain(t *testing.T) {
	b, _ := newLogBrainForTest(t, config.AgentCatalogConfig{SpikeSustainTicks: 3})
	hit := spikeResult{Spike: true}
	if b.confirmSpike("k", hit) {
		t.Fatal("tick 1 of 3: should not fire yet")
	}
	if b.confirmSpike("k", hit) {
		t.Fatal("tick 2 of 3: should not fire yet")
	}
	if !b.confirmSpike("k", hit) {
		t.Fatal("tick 3 of 3: should fire (streak reached sustain)")
	}
	if b.confirmSpike("k", spikeResult{Spike: false}) {
		t.Fatal("non-spike should not fire and should reset the streak")
	}
	if b.confirmSpike("k", hit) {
		t.Fatal("after reset, tick 1 of 3 again: should not fire")
	}

	ceil, _ := newLogBrainForTest(t, config.AgentCatalogConfig{SpikeSustainTicks: 5})
	if !ceil.confirmSpike("k", spikeResult{Spike: true, Ceiling: true}) {
		t.Fatal("ceiling hit should fire on the first tick despite sustain=5")
	}
}
