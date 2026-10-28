package tools

import (
	"fmt"
	"math"
	"time"
)

// TimeRangeArgs is the shared model-facing time-range contract.
type TimeRangeArgs struct {
	TimeRangeMinutes int    `json:"time_range_minutes"`
	WindowMinutes    int    `json:"window_minutes"`
	Start            string `json:"start"`
	End              string `json:"end"`
}

// TimeRange is an absolute, bounded query interval.
type TimeRange struct {
	Start   time.Time
	End     time.Time
	Minutes int
}

// ResolveTimeRange resolves absolute bounds and relative aliases into one interval.
func ResolveTimeRange(args TimeRangeArgs, now time.Time, defaultMinutes, maxMinutes int) (TimeRange, error) {
	end := now.UTC()
	if args.End != "" {
		parsed, err := time.Parse(time.RFC3339, args.End)
		if err != nil {
			return TimeRange{}, fmt.Errorf("end must be RFC3339: %w", err)
		}
		end = parsed.UTC()
	}

	minutes := args.TimeRangeMinutes
	if minutes <= 0 {
		minutes = args.WindowMinutes
	}
	if minutes <= 0 {
		minutes = defaultMinutes
	}
	if minutes > maxMinutes {
		minutes = maxMinutes
	}

	start := end.Add(-time.Duration(minutes) * time.Minute)
	if args.Start != "" {
		parsed, err := time.Parse(time.RFC3339, args.Start)
		if err != nil {
			return TimeRange{}, fmt.Errorf("start must be RFC3339: %w", err)
		}
		start = parsed.UTC()
		if start.After(end) {
			return TimeRange{}, fmt.Errorf("start must not be after end")
		}
		if maxMinutes > 0 && end.Sub(start) > time.Duration(maxMinutes)*time.Minute {
			start = end.Add(-time.Duration(maxMinutes) * time.Minute)
		}
		minutes = int(math.Ceil(end.Sub(start).Minutes()))
	}

	return TimeRange{Start: start, End: end, Minutes: minutes}, nil
}

// TimeRangeProperties returns JSON-schema properties for the shared time range.
func TimeRangeProperties(defaultMinutes, maxMinutes int) map[string]any {
	return map[string]any{
		"time_range_minutes": map[string]any{
			"type":        "integer",
			"description": fmt.Sprintf("Look back this many minutes from end. Default %d, max %d.", defaultMinutes, maxMinutes),
		},
		"window_minutes": map[string]any{
			"type":        "integer",
			"description": "Deprecated alias for time_range_minutes.",
		},
		"start": map[string]any{
			"type":        "string",
			"format":      "date-time",
			"description": "Optional absolute RFC3339 start time. When set, it takes precedence over relative minute fields.",
		},
		"end": map[string]any{
			"type":        "string",
			"format":      "date-time",
			"description": "Optional absolute RFC3339 end time. Defaults to now.",
		},
	}
}

// AddTimeRangeProperties adds the shared time-range schema to properties.
func AddTimeRangeProperties(properties map[string]any, defaultMinutes, maxMinutes int) map[string]any {
	for name, property := range TimeRangeProperties(defaultMinutes, maxMinutes) {
		properties[name] = property
	}
	return properties
}
