package tools

import (
	"testing"
	"time"
)

func TestResolveTimeRange(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		args      TimeRangeArgs
		wantStart time.Time
		wantEnd   time.Time
		wantMins  int
		wantErr   bool
	}{
		{name: "default", wantStart: now.Add(-30 * time.Minute), wantEnd: now, wantMins: 30},
		{name: "canonical wins", args: TimeRangeArgs{TimeRangeMinutes: 15, WindowMinutes: 60}, wantStart: now.Add(-15 * time.Minute), wantEnd: now, wantMins: 15},
		{name: "legacy alias", args: TimeRangeArgs{WindowMinutes: 20}, wantStart: now.Add(-20 * time.Minute), wantEnd: now, wantMins: 20},
		{name: "relative to end", args: TimeRangeArgs{TimeRangeMinutes: 15, End: "2026-08-26T10:00:00Z"}, wantStart: time.Date(2026, 8, 26, 9, 45, 0, 0, time.UTC), wantEnd: time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC), wantMins: 15},
		{name: "absolute", args: TimeRangeArgs{Start: "2026-08-26T09:00:00Z", End: "2026-08-26T10:00:00Z"}, wantStart: time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC), wantEnd: time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC), wantMins: 60},
		{name: "partial minute rounds up", args: TimeRangeArgs{Start: "2026-08-26T09:58:30Z", End: "2026-08-26T10:00:00Z"}, wantStart: time.Date(2026, 8, 26, 9, 58, 30, 0, time.UTC), wantEnd: time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC), wantMins: 2},
		{name: "clamped", args: TimeRangeArgs{Start: "2026-08-20T10:00:00Z", End: "2026-08-26T10:00:00Z"}, wantStart: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC), wantEnd: time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC), wantMins: 1440},
		{name: "invalid start", args: TimeRangeArgs{Start: "yesterday"}, wantErr: true},
		{name: "reversed", args: TimeRangeArgs{Start: "2026-08-26T11:00:00Z", End: "2026-08-26T10:00:00Z"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveTimeRange(test.args, now, 30, 1440)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTimeRange: %v", err)
			}
			if !got.Start.Equal(test.wantStart) || !got.End.Equal(test.wantEnd) || got.Minutes != test.wantMins {
				t.Fatalf("range = %+v, want start=%s end=%s minutes=%d", got, test.wantStart, test.wantEnd, test.wantMins)
			}
		})
	}
}

func TestAddTimeRangeProperties(t *testing.T) {
	properties := AddTimeRangeProperties(map[string]any{"service": map[string]any{"type": "string"}}, 15, 1440)
	for _, name := range []string{"service", "time_range_minutes", "window_minutes", "start", "end"} {
		if _, ok := properties[name]; !ok {
			t.Errorf("property %q missing", name)
		}
	}
}
