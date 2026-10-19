package breaklogs

import (
	"testing"
	"time"
)

func TestCoverageRequiresAffirmativeIntervalsAndPreservesReasons(t *testing.T) {
	from := time.Now().UTC().Add(-time.Hour)
	to := from.Add(time.Hour)
	if got := Cover(from, to, nil, nil, false); got.State != "none" || !got.Tip || len(got.Gaps) != 1 {
		t.Fatalf("empty: %+v", got)
	}
	if got := Cover(from, to, []Gap{{From: from, To: to}}, nil, false); got.State != "complete" || got.Tip {
		t.Fatalf("full: %+v", got)
	}
	for _, reason := range []string{"restarted_since", "pod_deleted", "capture_late", "rotated", "forbidden", "budget"} {
		got := Cover(from, to, []Gap{{From: from, To: to}}, []Gap{{From: from, To: from.Add(time.Minute), Reason: reason}}, false)
		if got.State != "partial" || got.Reason != reason || !got.Tip {
			t.Fatalf("reason: %+v", got)
		}
		got = Cover(from, to, nil, got.Gaps, true)
		if got.State != "complete" || got.Tip {
			t.Fatalf("linked: %+v", got)
		}
	}
	intervals := []Gap{{From: to.Add(-time.Minute), To: to}, {From: from, To: from.Add(time.Minute)}}
	before := intervals[0]
	Cover(from, to, intervals, nil, false)
	if intervals[0] != before {
		t.Fatal("coverage mutated input")
	}
}

func TestCoverObservedFutureIsPendingWithoutUnavailableTip(t *testing.T) {
	now := time.Now().UTC()
	from, to := now.Add(-time.Minute), now.Add(2*time.Minute)
	got := CoverObserved(from, to, now, []Gap{{From: from, To: now}}, nil)
	if got.State != "pending" || got.Tip || got.Reason != "pending" || len(got.Gaps) != 1 || !got.Gaps[0].From.Equal(now) || got.Gaps[0].Reason != "pending" {
		t.Fatalf("future became unavailable: %+v", got)
	}
	got = CoverObserved(now.Add(time.Minute), to, now, nil, nil)
	if got.State != "pending" || got.Tip || len(got.Gaps) != 1 || !got.Gaps[0].From.Equal(now.Add(time.Minute)) {
		t.Fatalf("wholly future window became unavailable: %+v", got)
	}
	for _, reason := range []string{"budget", "pod_deleted", "forbidden", "capture_late", "rotated", "restarted_since"} {
		got = CoverObserved(from, to, now, []Gap{{From: from, To: now}}, []Gap{{From: from, To: now, Reason: reason}})
		if !got.Tip || got.Reason != reason || len(got.Gaps) != 2 || got.Gaps[1].Reason != "pending" {
			t.Fatalf("future hid completed interval %s: %+v", reason, got)
		}
	}
	got = CoverObserved(from, to, now, []Gap{{From: from, To: now}}, []Gap{{From: from, To: from, Reason: "rotated"}})
	if !got.Tip || got.Reason != "rotated" {
		t.Fatalf("future hid completed boundary truncation: %+v", got)
	}
}

func TestAssessInfersContainerCoverageReasons(t *testing.T) {
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	item := testSnapshot(now, "pod")
	item.From = from
	item.Restarts = 1
	base := ContainerObservation{PodUID: "pod", Container: "app", Slot: "previous", Restarts: 1}
	if got := Assess(from, now, []Snapshot{item}, base); got.State != "complete" {
		t.Fatal("complete", got)
	}
	for _, reason := range []string{"restarted_since", "pod_deleted", "capture_late", "rotated", "forbidden", "budget"} {
		observation := base
		samples := []Snapshot{item}
		switch reason {
		case "restarted_since":
			observation.Restarts = 3
		case "pod_deleted":
			samples = nil
			observation.Deleted = true
		case "capture_late":
			samples[0].From = from.Add(time.Minute)
		case "rotated":
			samples[0].Truncated = true
		case "forbidden":
			observation.Forbidden = true
		case "budget":
			observation.Deferred = true
		}
		got := Assess(from, now, samples, observation)
		if got.State == "complete" || got.Reason != reason || !got.Tip {
			t.Fatalf("%s: %+v", reason, got)
		}
	}
}
