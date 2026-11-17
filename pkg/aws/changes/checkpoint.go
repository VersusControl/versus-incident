// Package changes turns CloudTrail write events into plain-language changes
// and tracks which time ranges have actually been read. A change list is only
// complete when its window has no gap, so a reader can say "no changes" only
// when the checkpoint proves it looked.
package changes

import (
	"sort"
	"time"
)

// Interval is a half-open time range [From, To).
type Interval struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Checkpoint records, for one account and region, every interval of
// CloudTrail history that was read to the end. Anything outside Covered is a
// gap.
type Checkpoint struct {
	Account       string     `json:"account"`
	Region        string     `json:"region"`
	Covered       []Interval `json:"covered,omitempty"`
	LastAttemptAt time.Time  `json:"last_attempt_at,omitzero"`
	LastSuccessAt time.Time  `json:"last_success_at,omitzero"`
	ErrorCode     string     `json:"error_code,omitempty"`
}

// Cover marks [from, to) as read and merges it with touching intervals.
func (checkpoint *Checkpoint) Cover(from, to time.Time) {
	if !to.After(from) {
		return
	}
	checkpoint.Covered = append(checkpoint.Covered, Interval{From: from.UTC(), To: to.UTC()})
	checkpoint.Covered = merge(checkpoint.Covered)
}

// Trim drops coverage before cutoff, so a bounded store does not claim it
// still holds history it has pruned.
func (checkpoint *Checkpoint) Trim(cutoff time.Time) {
	kept := checkpoint.Covered[:0]
	for _, interval := range checkpoint.Covered {
		if !interval.To.After(cutoff) {
			continue
		}
		if interval.From.Before(cutoff) {
			interval.From = cutoff
		}
		kept = append(kept, interval)
	}
	checkpoint.Covered = kept
}

// CoveredUntil is the end of the most recent covered interval.
func (checkpoint Checkpoint) CoveredUntil() time.Time {
	if len(checkpoint.Covered) == 0 {
		return time.Time{}
	}
	return checkpoint.Covered[len(checkpoint.Covered)-1].To
}

// Gaps returns every part of [since, until) that is not covered, in time
// order. An empty result proves the window was read completely.
func (checkpoint Checkpoint) Gaps(since, until time.Time) []Interval {
	if !until.After(since) {
		return nil
	}
	gaps := make([]Interval, 0)
	cursor := since
	for _, interval := range merge(append([]Interval(nil), checkpoint.Covered...)) {
		if !interval.To.After(cursor) {
			continue
		}
		if !interval.From.Before(until) {
			break
		}
		if interval.From.After(cursor) {
			gaps = append(gaps, Interval{From: cursor, To: minTime(interval.From, until)})
		}
		if interval.To.After(cursor) {
			cursor = interval.To
		}
		if !cursor.Before(until) {
			return gaps
		}
	}
	if cursor.Before(until) {
		gaps = append(gaps, Interval{From: cursor, To: until})
	}
	return gaps
}

// Windows plans the next reads. The first window runs forward from the end
// of coverage, re-reading overlap to catch events CloudTrail delivers late;
// the second, when present, back-fills the most recent gap inside lookback.
func (checkpoint Checkpoint) Windows(now time.Time, lookback, overlap time.Duration) []Interval {
	floor := now.Add(-lookback)
	start := floor
	if until := checkpoint.CoveredUntil(); !until.IsZero() {
		start = until.Add(-overlap)
		if start.Before(floor) {
			start = floor
		}
	}
	windows := []Interval{{From: start, To: now}}
	gaps := checkpoint.Gaps(floor, start)
	if len(gaps) > 0 {
		windows = append(windows, gaps[len(gaps)-1])
	}
	return windows
}

func merge(intervals []Interval) []Interval {
	if len(intervals) < 2 {
		return intervals
	}
	sort.Slice(intervals, func(left, right int) bool { return intervals[left].From.Before(intervals[right].From) })
	merged := []Interval{intervals[0]}
	for _, interval := range intervals[1:] {
		last := &merged[len(merged)-1]
		if !interval.From.After(last.To) {
			if interval.To.After(last.To) {
				last.To = interval.To
			}
			continue
		}
		merged = append(merged, interval)
	}
	return merged
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
