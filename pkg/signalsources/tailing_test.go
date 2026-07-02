package signalsources

import (
	"testing"
	"time"
)

// TestClampCursor table-drives the shared tailing-cursor invariant every
// cursor-driven source now routes its returned cursor through: never below the
// lower bound the worker asked for, never ahead of the wall clock. The
// future-timestamp cases are the regression bite for the live stall — a
// document dated in the future must not push the cursor past `now`.
func TestClampCursor(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	now := base.Add(5 * time.Minute) // 10:05

	cases := []struct {
		name      string
		candidate time.Time
		since     time.Time
		want      time.Time
	}{
		{
			name:      "past candidate within range is kept",
			candidate: base.Add(2 * time.Minute), // 10:02
			since:     base,                      // 10:00
			want:      base.Add(2 * time.Minute),
		},
		{
			name:      "candidate below since is raised to since",
			candidate: base.Add(-time.Hour),
			since:     base,
			want:      base,
		},
		{
			name:      "candidate at now is kept",
			candidate: now,
			since:     base,
			want:      now,
		},
		{
			name:      "future candidate is clamped to now (the stall guard)",
			candidate: base.Add(100 * 365 * 24 * time.Hour), // ~year 2126
			since:     base,
			want:      now,
		},
		{
			name:      "far-future candidate (2048-style) is clamped to now",
			candidate: time.Date(2048, 1, 6, 1, 2, 6, 0, time.UTC),
			since:     base,
			want:      now,
		},
		{
			name:      "future since collapses the whole range to now",
			candidate: base.Add(200 * 365 * 24 * time.Hour),
			since:     now.Add(time.Hour), // since itself already ahead of now
			want:      now,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClampCursor(tc.candidate, tc.since, now)
			if !got.Equal(tc.want) {
				t.Errorf("ClampCursor(%v, %v, %v) = %v, want %v",
					tc.candidate, tc.since, now, got, tc.want)
			}
		})
	}
}
