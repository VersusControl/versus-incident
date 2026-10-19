package breaklogs

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func testSnapshot(now time.Time, pod string) Snapshot {
	item := Snapshot{Cluster: "cluster", Namespace: "shop", RootUID: "root", Pod: pod, PodUID: pod, Container: "app", Slot: "previous", TerminationAt: now, CapturedAt: now, From: now.Add(-time.Minute), To: now, Lines: []Line{{At: now, Text: "error safe"}}}
	item.ID = Identity(item)
	return item
}

func TestMergeDedupRetentionAndEviction(t *testing.T) {
	now := time.Now().UTC()
	item := testSnapshot(now, "pod")
	state := State{}
	if err := Merge(&state, []Snapshot{item, item}, nil, "job", now); err != nil || len(state.Snapshots) != 1 || state.Jobs["job"] != now {
		t.Fatalf("dedup: %+v %v", state, err)
	}
	rotated := item
	rotated.CapturedAt = now.Add(time.Second)
	rotated.Lines = []Line{{At: now, Text: "later rotated"}}
	if err := Merge(&state, []Snapshot{rotated}, nil, "job", now.Add(time.Second)); err != nil || state.Snapshots[0].Lines[0].Text != "error safe" {
		t.Fatal("retry replaced captured evidence", err)
	}
	if err := Merge(&state, nil, nil, "", now.Add(25*time.Hour)); err != nil || len(state.Snapshots) != 0 || len(state.Jobs) != 0 {
		t.Fatal("retention", err)
	}
	items := []Snapshot{}
	for position := 0; position < MaxSnapshots+1; position++ {
		items = append(items, testSnapshot(now, fmt.Sprint(position)))
	}
	if err := Merge(&state, items, nil, "", now); err != nil || len(state.Snapshots) != MaxSnapshots || len(state.Gaps) == 0 {
		t.Fatal("count cap", err)
	}
	state = State{}
	items = nil
	for position := 0; position < 130; position++ {
		value := testSnapshot(now, fmt.Sprint(position))
		value.Lines = nil
		for range 16 {
			value.Lines = append(value.Lines, Line{At: now, Text: strings.Repeat("x", 4096)})
		}
		items = append(items, value)
	}
	if err := Merge(&state, items, nil, "", now); err != nil || len(state.Snapshots) >= 130 || len(state.Gaps) == 0 {
		t.Fatal("byte cap", err)
	}
	bad := item
	bad.Lines = []Line{{At: now, Text: strings.Repeat("x", 4097)}}
	if Merge(&State{}, []Snapshot{bad}, nil, "", now) == nil {
		t.Fatal("oversized line accepted")
	}
	bad = item
	bad.ID = "invalid"
	if Merge(&State{}, []Snapshot{bad}, nil, "", now) == nil {
		t.Fatal("bad identity accepted")
	}
}

func TestMergeSuccessfulRetryOnlySupersedesMatchingBudgetInterval(t *testing.T) {
	now := time.Now().UTC()
	item := testSnapshot(now, "pod")
	gap := Gap{SnapshotID: item.ID, From: item.From.Add(-time.Minute), To: item.To.Add(time.Minute), Namespace: item.Namespace, RootUID: item.RootUID, PodUID: item.PodUID, Container: item.Container, Reason: "budget"}
	other := gap
	other.SnapshotID = "other-slot"
	cap := gap
	cap.SnapshotID = ""
	state := State{Gaps: []Gap{gap, other, cap}}
	if err := Merge(&state, []Snapshot{item}, nil, "", now); err != nil {
		t.Fatal(err)
	}
	if len(state.Gaps) != 4 || !state.Gaps[0].To.Equal(item.From) || !state.Gaps[1].From.Equal(item.To) || state.Gaps[2] != other || state.Gaps[3] != cap {
		t.Fatalf("retry removed unrelated or uncovered intervals: %+v", state.Gaps)
	}
	for _, test := range []struct {
		name       string
		truncated  bool
		observedAt time.Time
		namespace  string
	}{
		{name: "truncated", truncated: true},
		{name: "older_success", observedAt: now.Add(time.Second)},
		{name: "unrelated_namespace", namespace: "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := item
			observation.Truncated = test.truncated
			failure := gap
			failure.ObservedAt = test.observedAt
			if test.namespace != "" {
				failure.Namespace = test.namespace
			}
			got := ResolveBudgetGaps([]Gap{failure}, []Snapshot{observation})
			if len(got) != 1 || got[0] != failure {
				t.Fatalf("non-covering observation erased failure: %+v", got)
			}
		})
	}
}

func TestCursorScopeFiltersAndObservationBound(t *testing.T) {
	now := time.Now().UTC()
	query := Query{Namespace: "shop", RootUID: "root", From: now.Add(-time.Hour), To: now, Limit: 10}
	query.Cursor = NextCursor("org/cluster/credential", query, "digest", 10, now)
	if offset, observed, err := CursorOffset("org/cluster/credential", query, "digest", now); err != nil || offset != 10 || observed != now {
		t.Fatal("cursor", offset, err)
	}
	for _, scope := range []string{"other/cluster/credential", "org/other/credential", "org/cluster/other"} {
		if _, _, err := CursorOffset(scope, query, "digest", now); err == nil {
			t.Fatal("cross-scope cursor accepted")
		}
	}
	query.Mode = "previous"
	if _, _, err := CursorOffset("org/cluster/credential", query, "digest", now); err == nil {
		t.Fatal("cross-filter cursor accepted")
	}
	query.Mode = ""
	if _, _, err := CursorOffset("org/cluster/credential", query, "changed", now); err == nil {
		t.Fatal("changed result accepted")
	}
	query.Cursor = ""
	query.To = query.From.Add(2*time.Hour + time.Second)
	if query.Validate() == nil {
		t.Fatal("unbounded window")
	}
}

func TestCurrentSlotMergesLaterBreakWithoutLosingFirstEvidence(t *testing.T) {
	now := time.Now().UTC()
	first := testSnapshot(now.Add(-time.Hour), "pod")
	first.Slot = "current"
	first.TerminationAt = time.Time{}
	first.ID = Identity(first)
	state := State{}
	if err := Merge(&state, []Snapshot{first}, nil, "first", now); err != nil {
		t.Fatal(err)
	}
	later := first
	later.CapturedAt = now
	later.From = now.Add(-time.Minute)
	later.To = now
	later.Lines = []Line{{At: now, Text: "second break"}}
	if err := Merge(&state, []Snapshot{later}, nil, "later", now); err != nil {
		t.Fatal(err)
	}
	if len(state.Snapshots) != 1 || len(state.Snapshots[0].Lines) != 2 || state.Snapshots[0].Lines[0].Text != "error safe" || len(state.Gaps) != 1 || state.Gaps[0].Reason != "capture_late" {
		t.Fatal("current merge lost evidence or gap", state)
	}
}
