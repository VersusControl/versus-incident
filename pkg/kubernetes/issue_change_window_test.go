package kubernetes

import (
	"testing"
	"time"

	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
)

// Each enriched issue reads exactly its own [BrokeAt-30m, BrokeAt+2m] window.
// Overlapping windows stay separate, so a capped read for one issue cannot mark
// or crowd out another, and every break maps to its own window.
func TestIssueChangeWindowsKeepOverlapsSeparateAndMapIndexes(t *testing.T) {
	base := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	breaks := []kubebreaks.Break{
		{BrokeAt: base.Add(70 * time.Minute)},
		{BrokeAt: base},
		{BrokeAt: base.Add(20 * time.Minute)},
		{BrokeAt: base.Add(32 * time.Minute)},
	}
	windows, windowOf := issueChangeWindows(breaks)
	if len(windows) != len(breaks) || len(windowOf) != len(breaks) {
		t.Fatalf("overlapping breaks did not each get a window: windows=%+v windowOf=%v", windows, windowOf)
	}
	seen := map[int]bool{}
	for position, item := range breaks {
		index := windowOf[position]
		if index < 0 || index >= len(windows) || seen[index] {
			t.Fatalf("break %d maps to window %d; windowOf=%v", position, index, windowOf)
		}
		seen[index] = true
		window := windows[index]
		if want := item.BrokeAt.Add(-30 * time.Minute); !window.from.Equal(want) {
			t.Fatalf("break %d window start = %s want %s", position, window.from, want)
		}
		if want := item.BrokeAt.Add(2 * time.Minute); !window.to.Equal(want) {
			t.Fatalf("break %d window end = %s want %s", position, window.to, want)
		}
	}
}

func TestIssueChangeWindowsKeepDisjointBreaksSeparate(t *testing.T) {
	base := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	windows, windowOf := issueChangeWindows([]kubebreaks.Break{
		{BrokeAt: base.Add(60 * time.Minute)},
		{BrokeAt: base},
	})
	if len(windows) != 2 || windowOf[0] == windowOf[1] {
		t.Fatalf("disjoint windows=%+v windowOf=%v", windows, windowOf)
	}
	if first := windows[windowOf[1]]; !first.from.Equal(base.Add(-30*time.Minute)) || !first.to.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("earlier window = [%s, %s]", first.from, first.to)
	}
	if second := windows[windowOf[0]]; !second.from.Equal(base.Add(30*time.Minute)) || !second.to.Equal(base.Add(62*time.Minute)) {
		t.Fatalf("later window = [%s, %s]", second.from, second.to)
	}
}

func TestIssueChangeWindowsEmptyInput(t *testing.T) {
	windows, windowOf := issueChangeWindows(nil)
	if len(windows) != 0 || len(windowOf) != 0 {
		t.Fatalf("empty input produced windows=%+v windowOf=%v", windows, windowOf)
	}
}

// Windows that only touch, or that are one second apart, stay separate and
// keep their own exact ranges.
func TestIssueChangeWindowsTouchingBoundaryStaysSeparate(t *testing.T) {
	base := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	for _, gap := range []time.Duration{32 * time.Minute, 32*time.Minute + time.Second} {
		windows, windowOf := issueChangeWindows([]kubebreaks.Break{{BrokeAt: base}, {BrokeAt: base.Add(gap)}})
		if len(windows) != 2 || windowOf[0] == windowOf[1] {
			t.Fatalf("gap %s: windows were shared: windows=%+v windowOf=%v", gap, windows, windowOf)
		}
		if first := windows[windowOf[0]]; !first.from.Equal(base.Add(-30*time.Minute)) || !first.to.Equal(base.Add(2*time.Minute)) {
			t.Fatalf("gap %s: first window = [%s, %s]", gap, first.from, first.to)
		}
		if second := windows[windowOf[1]]; !second.from.Equal(base.Add(gap-30*time.Minute)) || !second.to.Equal(base.Add(gap+2*time.Minute)) {
			t.Fatalf("gap %s: second window = [%s, %s]", gap, second.from, second.to)
		}
	}
}

// Breaks at the same instant share one window, and a single break gets
// exactly its own range.
func TestIssueChangeWindowsSameInstantAndSingleBreak(t *testing.T) {
	base := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	windows, windowOf := issueChangeWindows([]kubebreaks.Break{{BrokeAt: base}, {BrokeAt: base.Add(10 * time.Minute)}, {BrokeAt: base}})
	if len(windows) != 2 || windowOf[0] != windowOf[2] || windowOf[0] == windowOf[1] {
		t.Fatalf("same-instant windows=%+v windowOf=%v", windows, windowOf)
	}
	if shared := windows[windowOf[0]]; !shared.from.Equal(base.Add(-30*time.Minute)) || !shared.to.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("shared window = [%s, %s]", shared.from, shared.to)
	}
	windows, windowOf = issueChangeWindows([]kubebreaks.Break{{BrokeAt: base}})
	if len(windows) != 1 || windowOf[0] != 0 || !windows[0].from.Equal(base.Add(-30*time.Minute)) || !windows[0].to.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("single window=%+v windowOf=%v", windows, windowOf)
	}
}
