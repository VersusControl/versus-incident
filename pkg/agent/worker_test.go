package agent

import "testing"

func TestIsSpike(t *testing.T) {
	tests := []struct {
		name             string
		prevBaseline     float64
		prevCount        int
		tickFreq         int
		multiplier       float64
		minFreq          int
		minBaselineCount int
		want             bool
	}{
		{
			name:             "clear spike: 6x baseline well above floors",
			prevBaseline:     2.0,
			prevCount:        100,
			tickFreq:         15,
			multiplier:       5.0,
			minFreq:          5,
			minBaselineCount: 20,
			want:             true,
		},
		{
			name:             "exactly at multiplier is not a spike (strict >)",
			prevBaseline:     2.0,
			prevCount:        100,
			tickFreq:         10,
			multiplier:       5.0,
			minFreq:          5,
			minBaselineCount: 20,
			want:             false,
		},
		{
			name:             "below multiplier is not a spike",
			prevBaseline:     2.0,
			prevCount:        100,
			tickFreq:         9,
			multiplier:       5.0,
			minFreq:          5,
			minBaselineCount: 20,
			want:             false,
		},
		{
			name:             "tick frequency below minFreq is not a spike",
			prevBaseline:     0.5,
			prevCount:        100,
			tickFreq:         4, // 8x baseline but below minFreq=5
			multiplier:       5.0,
			minFreq:          5,
			minBaselineCount: 20,
			want:             false,
		},
		{
			name:             "barely-seen pattern is not a spike (prevCount below floor)",
			prevBaseline:     1.0,
			prevCount:        10, // < minBaselineCount=20
			tickFreq:         100,
			multiplier:       5.0,
			minFreq:          5,
			minBaselineCount: 20,
			want:             false,
		},
		{
			name:             "zero baseline is not a spike",
			prevBaseline:     0.0,
			prevCount:        100,
			tickFreq:         50,
			multiplier:       5.0,
			minFreq:          5,
			minBaselineCount: 20,
			want:             false,
		},
		{
			name:             "multiplier=0 disables spike detection",
			prevBaseline:     1.0,
			prevCount:        500,
			tickFreq:         500,
			multiplier:       0,
			minFreq:          5,
			minBaselineCount: 20,
			want:             false,
		},
		{
			name:             "negative multiplier disables spike detection",
			prevBaseline:     1.0,
			prevCount:        500,
			tickFreq:         500,
			multiplier:       -1,
			minFreq:          5,
			minBaselineCount: 20,
			want:             false,
		},
		{
			name:             "zero floors fall back to defaults (5 / 20)",
			prevBaseline:     1.0,
			prevCount:        50,
			tickFreq:         10, // 10x baseline, above default minFreq=5
			multiplier:       5.0,
			minFreq:          0,
			minBaselineCount: 0,
			want:             true,
		},
		{
			name:             "default-floor fallback still rejects under-trained pattern",
			prevBaseline:     1.0,
			prevCount:        15, // below default minBaselineCount=20
			tickFreq:         100,
			multiplier:       5.0,
			minFreq:          0,
			minBaselineCount: 0,
			want:             false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isSpike(tc.prevBaseline, tc.prevCount, tc.tickFreq,
				tc.multiplier, tc.minFreq, tc.minBaselineCount)
			if got != tc.want {
				t.Errorf("isSpike(prevBaseline=%v, prevCount=%d, tickFreq=%d, mult=%v, minFreq=%d, minBaseline=%d) = %v, want %v",
					tc.prevBaseline, tc.prevCount, tc.tickFreq,
					tc.multiplier, tc.minFreq, tc.minBaselineCount,
					got, tc.want)
			}
		})
	}
}
