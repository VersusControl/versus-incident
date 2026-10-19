package breaklogs

import (
	"sort"
	"time"
)

type Coverage struct {
	State  string `json:"state"`
	Gaps   []Gap  `json:"gaps"`
	Reason string `json:"reason,omitempty"`
	Tip    bool   `json:"tip"`
}

type ContainerObservation struct {
	PodUID    string
	Container string
	Init      bool
	Slot      string
	Restarts  int32
	Deleted   bool
	Forbidden bool
	Deferred  bool
}

func Assess(from, to time.Time, snapshots []Snapshot, container ContainerObservation) Coverage {
	intervals := []Gap{}
	failures := []Gap{}
	found := false
	for _, item := range snapshots {
		if item.PodUID != container.PodUID || item.Container != container.Container || item.Init != container.Init || container.Slot != "" && item.Slot != container.Slot {
			continue
		}
		found = true
		intervals = append(intervals, Gap{From: item.From, To: item.To})
		if item.Truncated {
			failures = append(failures, Gap{From: from, To: maxTime(from, item.From), Reason: "rotated"})
		}
		if int64(container.Restarts) > int64(item.Restarts)+1 {
			failures = append(failures, Gap{From: maxTime(from, item.To), To: to, Reason: "restarted_since"})
		}
	}
	reason := ""
	switch {
	case container.Forbidden:
		reason = "forbidden"
	case container.Deferred:
		reason = "budget"
	case container.Deleted && !found:
		reason = "pod_deleted"
	}
	if reason != "" {
		failures = append(failures, Gap{From: from, To: to, Reason: reason})
	}
	result := Cover(from, to, intervals, failures, false)
	for position := range result.Gaps {
		result.Gaps[position].PodUID = container.PodUID
		result.Gaps[position].Container = container.Container
	}
	return result
}

func Cover(from, to time.Time, intervals []Gap, failures []Gap, linkedClosed bool) Coverage {
	result := Coverage{State: "none", Gaps: []Gap{}}
	intervals = append([]Gap(nil), intervals...)
	sort.Slice(intervals, func(left, right int) bool { return intervals[left].From.Before(intervals[right].From) })
	position := from
	covered := false
	for _, interval := range intervals {
		if !interval.To.After(interval.From) {
			continue
		}
		if interval.To.Before(from) || interval.From.After(to) {
			continue
		}
		if interval.From.After(position) {
			result.Gaps = append(result.Gaps, Gap{From: position, To: minTime(interval.From, to)})
		}
		if interval.To.After(position) {
			covered = true
			position = interval.To
		}
		if !position.Before(to) {
			break
		}
	}
	if position.Before(to) {
		result.Gaps = append(result.Gaps, Gap{From: position, To: to})
	}
	for _, failure := range failures {
		if failure.From.After(to) || failure.To.Before(from) {
			continue
		}
		failure.From = maxTime(failure.From, from)
		failure.To = minTime(failure.To, to)
		result.Gaps = append(result.Gaps, failure)
		if result.Reason == "" {
			result.Reason = failure.Reason
		}
	}
	if linkedClosed {
		result.Gaps = []Gap{}
	}
	if len(result.Gaps) == 0 {
		result.State = "complete"
	} else if covered {
		result.State = "partial"
	}
	if result.State == "complete" {
		result.Reason = ""
	}
	if result.State != "complete" && result.Reason == "" {
		result.Reason = "capture_late"
	}
	result.Tip = result.State != "complete"
	return result
}

func CoverObserved(from, to, now time.Time, intervals, failures []Gap) Coverage {
	observedTo := minTime(to, now)
	result := Coverage{State: "pending", Gaps: []Gap{}}
	if observedTo.After(from) {
		observedFailures := []Gap{}
		for _, gap := range failures {
			if gap.Reason != "pending" && gap.From.Before(observedTo) && !gap.To.Before(from) {
				observedFailures = append(observedFailures, gap)
			}
		}
		result = Cover(from, observedTo, intervals, observedFailures, false)
	}
	if to.After(now) {
		result.Gaps = append(result.Gaps, Gap{From: maxTime(from, now), To: to, Reason: "pending"})
		if !result.Tip {
			result.State = "pending"
			result.Reason = "pending"
		}
	}
	return result
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
func maxTime(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}
