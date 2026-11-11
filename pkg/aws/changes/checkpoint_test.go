package changes

import (
	"testing"
	"time"
)

var base = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

func TestCoverMergesTouchingIntervals(t *testing.T) {
	var checkpoint Checkpoint
	checkpoint.Cover(at(0), at(10))
	checkpoint.Cover(at(10), at(20))
	checkpoint.Cover(at(30), at(40))
	checkpoint.Cover(at(5), at(5))
	if len(checkpoint.Covered) != 2 || !checkpoint.Covered[0].To.Equal(at(20)) || !checkpoint.CoveredUntil().Equal(at(40)) {
		t.Fatalf("covered = %+v", checkpoint.Covered)
	}
	checkpoint.Cover(at(15), at(35))
	if len(checkpoint.Covered) != 1 {
		t.Fatalf("bridged = %+v", checkpoint.Covered)
	}
}

func TestGapsAreExactlyTheUncoveredStretches(t *testing.T) {
	var checkpoint Checkpoint
	if gaps := checkpoint.Gaps(at(0), at(10)); len(gaps) != 1 || !gaps[0].From.Equal(at(0)) || !gaps[0].To.Equal(at(10)) {
		t.Fatalf("empty checkpoint gaps = %+v", gaps)
	}
	checkpoint.Cover(at(10), at(20))
	checkpoint.Cover(at(30), at(40))
	gaps := checkpoint.Gaps(at(0), at(50))
	want := []Interval{{at(0), at(10)}, {at(20), at(30)}, {at(40), at(50)}}
	if len(gaps) != len(want) {
		t.Fatalf("gaps = %+v", gaps)
	}
	for index := range want {
		if !gaps[index].From.Equal(want[index].From) || !gaps[index].To.Equal(want[index].To) {
			t.Fatalf("gap %d = %+v, want %+v", index, gaps[index], want[index])
		}
	}
	if gaps := checkpoint.Gaps(at(12), at(18)); len(gaps) != 0 {
		t.Fatalf("covered window has gaps: %+v", gaps)
	}
	if gaps := checkpoint.Gaps(at(18), at(12)); gaps != nil {
		t.Fatalf("empty window = %+v", gaps)
	}
}

func TestWindowsResumeWithOverlapAndBackfill(t *testing.T) {
	var checkpoint Checkpoint
	first := checkpoint.Windows(at(600), 24*time.Hour, 15*time.Minute)
	if len(first) != 1 || !first[0].From.Equal(at(600).Add(-24*time.Hour)) {
		t.Fatalf("first windows = %+v", first)
	}
	checkpoint.Cover(at(0), at(100))
	checkpoint.Cover(at(200), at(300))
	windows := checkpoint.Windows(at(400), 24*time.Hour, 15*time.Minute)
	if len(windows) != 2 || !windows[0].From.Equal(at(285)) || !windows[0].To.Equal(at(400)) {
		t.Fatalf("resume window = %+v", windows)
	}
	if !windows[1].From.Equal(at(100)) || !windows[1].To.Equal(at(200)) {
		t.Fatalf("backfill window = %+v", windows[1])
	}
}

func TestTrimDropsPrunedCoverage(t *testing.T) {
	var checkpoint Checkpoint
	checkpoint.Cover(at(0), at(10))
	checkpoint.Cover(at(20), at(40))
	checkpoint.Trim(at(30))
	if len(checkpoint.Covered) != 1 || !checkpoint.Covered[0].From.Equal(at(30)) {
		t.Fatalf("trimmed = %+v", checkpoint.Covered)
	}
}
