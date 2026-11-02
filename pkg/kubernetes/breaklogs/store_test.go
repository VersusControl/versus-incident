package breaklogs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type delayedReader struct {
	calls     int
	ready     time.Time
	now       time.Time
	truncated bool
}

func TestTerminationEndPreservesPreciseAndUnknownBounds(t *testing.T) {
	second := time.Now().UTC().Truncate(time.Second)
	for _, test := range []struct{ at, want time.Time }{
		{time.Time{}, time.Time{}},
		{second, second.Add(time.Second - time.Nanosecond)},
		{second.Add(time.Nanosecond), second.Add(time.Nanosecond)},
	} {
		if got := TerminationEnd(test.at); !got.Equal(test.want) {
			t.Fatalf("termination end %v: got %v want %v", test.at, got, test.want)
		}
	}
}

func TestObservedPreviousStartOverridesInferredHistory(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-time.Minute)
	old := ManifestPod{Namespace: "shop", Name: "api-0", UID: "pod", CreatedAt: now.Add(-time.Hour), Containers: []ManifestContainer{{Name: "app", Restarts: 8, PreviousStartedAt: start.Add(-time.Minute)}}}
	for _, advance := range []int32{0, 1} {
		next := old
		next.Containers = []ManifestContainer{{Name: "app", Restarts: 8 + advance, PreviousStartedAt: start, TerminationAt: now}}
		state := State{PodHistory: []ManifestPod{old}}
		ObservePods(&state, []ManifestPod{next}, now)
		if len(state.PodHistory) != 1 || !state.PodHistory[0].Containers[0].PreviousStartedAt.Equal(start) {
			t.Fatalf("native previous start discarded: %+v", state.PodHistory)
		}
	}
}

func TestHistoryBudgetProtectsRetiredPodsAndScopesLoss(t *testing.T) {
	now := time.Now().UTC()
	state := State{}
	pods := []ManifestPod{}
	for root := 0; root < 85; root++ {
		for i := 0; i < 25; i++ {
			pods = append(pods, ManifestPod{Namespace: "shop", RootUID: fmt.Sprint(root), UID: fmt.Sprintf("%d-%d", root, i), Name: fmt.Sprintf("api-%d-%d", root, i), CreatedAt: now.Add(-time.Hour), Containers: []ManifestContainer{{Name: "app"}}})
		}
	}
	retired := ManifestPod{Namespace: "shop", RootUID: "0", UID: "retired", Name: "api-old", CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-time.Minute), DeletedAt: now.Add(-time.Minute), Containers: []ManifestContainer{{Name: "app"}}}
	state.PodHistory = []ManifestPod{retired}
	ObservePods(&state, pods, now)
	if err := Merge(&state, nil, nil, "", now); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pod := range state.PodHistory {
		found = found || pod.UID == retired.UID && pod.DeletedAt.Equal(retired.DeletedAt)
	}
	if !found || len(state.PodHistoryGaps) != 0 || len(state.PodHistory) <= MaxSnapshots {
		t.Fatal("cluster-wide cap erased retired identity or marked unaffected roots", len(state.PodHistory), state.PodHistoryGaps)
	}
	for i := 0; i < 60; i++ {
		pods = append(pods, ManifestPod{Namespace: "shop", RootUID: "large", UID: fmt.Sprintf("large-%d", i), Name: fmt.Sprintf("large-%d", i), CreatedAt: now.Add(-time.Hour), Containers: []ManifestContainer{{Name: "app"}}})
	}
	ObservePods(&state, pods, now.Add(time.Second))
	if err := Merge(&state, nil, nil, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, pod := range state.PodHistory {
		if pod.RootUID == "large" {
			count++
		}
	}
	if count != MaxHistoryPodsPerRoot || len(state.PodHistoryGaps) != 1 || state.PodHistoryGaps[0].RootUID != "large" {
		t.Fatal("history loss not root-scoped", count, state.PodHistoryGaps)
	}
}

func TestHistoryRootByteBudgetCannotDisplaceOtherRetiredIdentity(t *testing.T) {
	now := time.Now().UTC()
	pods := []ManifestPod{}
	for i := 0; i < MaxHistoryPodsPerRoot; i++ {
		pods = append(pods, ManifestPod{Namespace: "shop", RootUID: "large", UID: fmt.Sprint(i), Name: fmt.Sprint(i), Node: strings.Repeat("n", 32<<10), CreatedAt: now.Add(-time.Hour), LastSeen: now, DeletedAt: now})
	}
	pods = append(pods, ManifestPod{Namespace: "shop", RootUID: "small", UID: "retired", Name: "old", CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-time.Minute), DeletedAt: now.Add(-time.Minute)})
	state := State{PodHistory: pods}
	if err := Merge(&state, nil, nil, "", now); err != nil {
		t.Fatal(err)
	}
	found, bytes := false, 0
	for _, pod := range state.PodHistory {
		found = found || pod.UID == "retired"
		if pod.RootUID == "large" {
			raw, _ := json.Marshal(pod)
			bytes += len(raw) + 1
		}
	}
	if !found || bytes > MaxHistoryBytesPerRoot || len(state.PodHistoryGaps) != 1 || state.PodHistoryGaps[0].RootUID != "large" {
		t.Fatal("one root exhausted cluster history reservation", found, bytes, state.PodHistoryGaps)
	}
}

func TestMetadataBudgetTrimsBeforePreviousEvidenceAndOmitsUnsetTimes(t *testing.T) {
	now := time.Now().UTC()
	item := testSnapshot(now, "pod")
	state := State{}
	for i := 0; i < 600; i++ {
		state.Manifests = append(state.Manifests, Manifest{ID: fmt.Sprint(i), Namespace: "shop", RootUID: fmt.Sprint(i), BreakAt: now, From: now.Add(-time.Minute), To: now, CapturedAt: now,
			Pods: []ManifestPod{{Namespace: "shop", RootUID: fmt.Sprint(i), UID: fmt.Sprint(i), Name: "api", CreatedAt: now.Add(-time.Hour), LastSeen: now, Node: strings.Repeat("n", 4096)}}})
	}
	if err := Merge(&state, []Snapshot{item}, nil, "", now); err != nil {
		t.Fatal(err)
	}
	next := item
	next.CapturedAt = now.Add(time.Second)
	next.Lines = []Line{{At: now, Text: "newer previous slot is not the original excerpt"}}
	if err := Merge(&state, []Snapshot{next}, nil, "", next.CapturedAt); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if len(state.Manifests) == 600 || len(state.Snapshots) != 1 || state.Snapshots[0].Lines[0].Text != item.Lines[0].Text || len(raw) > MaxMetadataBytes {
		t.Fatal("metadata displaced prior evidence", len(state.Manifests), len(state.Snapshots), len(raw))
	}
	if strings.Contains(string(raw), "0001-01-01") {
		t.Fatal("unset optional timestamps inflate metadata")
	}
}

func TestReplicaClockTolerancePreservesTimeAndRejectsFutureSpoofs(t *testing.T) {
	now := time.Now().UTC()
	item := testSnapshot(now, "pod")
	item.CapturedAt = now.Add(ClockTolerance)
	state := State{}
	if err := Merge(&state, []Snapshot{item}, nil, "", now); err != nil || !state.Snapshots[0].CapturedAt.Equal(item.CapturedAt) {
		t.Fatal("bounded skew rejected or evidence timestamp rewritten", err)
	}
	item.CapturedAt = now.Add(ClockTolerance + time.Nanosecond)
	if err := Merge(&State{}, []Snapshot{item}, nil, "", now); err == nil {
		t.Fatal("future snapshot accepted")
	}
	manifest := Manifest{ID: "future", Namespace: "shop", RootUID: "root", BreakAt: now, From: now.Add(-time.Minute), To: now, CapturedAt: item.CapturedAt}
	if err := MergeManifests(&State{}, []Manifest{manifest}, now); err == nil {
		t.Fatal("future manifest accepted")
	}
}

func TestDelayedInventoryCannotResurrectDeletedLifetime(t *testing.T) {
	now := time.Now().UTC()
	pod := ManifestPod{Namespace: "shop", RootUID: "root", Name: "api-0", UID: "old", CreatedAt: now.Add(-time.Hour)}
	state := State{}
	ObservePods(&state, []ManifestPod{pod}, now.Add(-time.Minute))
	ObservePods(&state, nil, now)
	ObservePods(&state, []ManifestPod{pod}, now.Add(-30*time.Second))
	if len(state.PodHistory) != 1 || state.PodHistory[0].DeletedAt.IsZero() || state.PodHistory[0].LastSeen.After(now.Add(-time.Minute)) {
		t.Fatal("delayed inventory resurrected vanished UID", state.PodHistory)
	}
}

func TestLinkedCompleteRetryClearsTruncationAndEmptyPreservesExcerpt(t *testing.T) {
	now := time.Now().UTC()
	old := testSnapshot(now, "pod")
	old.Source, old.OccurrenceID, old.Truncated = "linked", "selected", true
	old.ID = Identity(old)
	state := State{}
	if err := Merge(&state, []Snapshot{old}, nil, "", now); err != nil {
		t.Fatal(err)
	}
	next := old
	next.Truncated = false
	next.CapturedAt = now.Add(time.Second)
	next.Lines = append(append([]Line(nil), old.Lines...), Line{At: now.Add(-time.Second), Text: "late ingestion"})
	if err := Merge(&state, []Snapshot{next}, nil, "", next.CapturedAt); err != nil {
		t.Fatal(err)
	}
	if state.Snapshots[0].Truncated || len(state.Snapshots[0].Lines) != 2 {
		t.Fatal("complete retry kept stale truncation", state.Snapshots[0])
	}
	next.CapturedAt = now.Add(2 * time.Second)
	next.Lines = nil
	if err := Merge(&state, []Snapshot{next}, nil, "", next.CapturedAt); err != nil || len(state.Snapshots[0].Lines) != 2 {
		t.Fatal("empty retry erased excerpt", state, err)
	}
}

func (reader *delayedReader) Preview(LinkedQuery) string { return "" }
func (reader *delayedReader) Read(_ context.Context, query LinkedQuery) (Result, error) {
	reader.calls++
	if reader.now.Before(reader.ready) {
		return Result{}, nil
	}
	return Result{Lines: []LinkedLine{{At: query.To, Pod: "api-0", Container: "app", Text: "failure"}}, Truncated: reader.truncated}, nil
}

func TestLinkedDeletedLifetimeDelayAndBoundedSplits(t *testing.T) {
	now := time.Now().UTC()
	old := ManifestPod{Name: "api-0", UID: "old", CreatedAt: now.Add(-time.Hour), DeletedAt: now.Add(-time.Minute), Containers: []ManifestContainer{{Name: "app", StartedAt: now.Add(-time.Hour)}}}
	next := ManifestPod{Name: "api-0", UID: "next", CreatedAt: now, Containers: []ManifestContainer{{Name: "app", StartedAt: now}}}
	for _, test := range []struct {
		at  time.Time
		uid string
	}{
		{now.Add(-2 * time.Minute), "old"}, {now.Add(-30 * time.Second), ""}, {now, "next"},
	} {
		pod, _, ok := ResolveLinkedLine([]ManifestPod{old, next}, LinkedLine{At: test.at, Pod: "api-0", Container: "app"})
		if (test.uid != "") != ok || ok && pod.UID != test.uid {
			t.Fatalf("lifetime at %s: %+v %v", test.at, pod, ok)
		}
	}
	overlap := next
	overlap.CreatedAt = old.CreatedAt
	if _, _, ok := ResolveLinkedLine([]ManifestPod{old, overlap}, LinkedLine{At: now.Add(-2 * time.Minute), Pod: "api-0", Container: "app"}); ok {
		t.Fatal("overlap attributed")
	}
	old.Containers[0].Restarts = 3
	old.Containers[0].StartedAt = now.Add(-3 * time.Minute)
	old.Containers[0].TerminationAt = now.Add(-4 * time.Minute)
	if _, _, ok := ResolveLinkedLine([]ManifestPod{old}, LinkedLine{At: now.Add(-5 * time.Minute), Pod: "api-0", Container: "app"}); ok {
		t.Fatal("unknown previous restart attributed")
	}
	reader := &delayedReader{now: now, ready: now.Add(90 * time.Second)}
	query := LinkedQuery{Namespace: "shop", Pods: []string{"api-0"}, From: now.Add(-5 * time.Minute), To: now, Limit: 2000}
	result, err := ReadWindow(context.Background(), reader, query)
	if err != nil || len(result.Lines) != 0 || reader.calls != 1 {
		t.Fatal(result, err, reader.calls)
	}
	reader.now = reader.ready
	reader.truncated = true
	reader.calls = 0
	result, err = ReadWindow(context.Background(), reader, query)
	if err != nil || reader.calls != 3 || !result.Truncated || len(result.Lines) > 6000 {
		t.Fatal(result, err, reader.calls)
	}
}

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

func TestManifestAcquiresLateVerifiedPodsWithoutResettingProgress(t *testing.T) {
	at := time.Now().UTC()
	root := "root"
	original := Manifest{ID: "occurrence", OccurrenceID: "occurrence", Namespace: "shop", RootUID: root, BreakAt: at.Add(-time.Minute), From: at.Add(-6 * time.Minute), To: at.Add(time.Minute), CapturedAt: at, Attempts: 1, Done: true, SettleAt: at.Add(time.Minute)}
	state := State{Manifests: []Manifest{original}}
	incoming := original
	incoming.Attempts = 0
	incoming.Done = false
	incoming.Pods = []ManifestPod{{Namespace: "shop", RootUID: root, Name: "api-0", UID: "pod", CreatedAt: at.Add(-time.Hour), LastSeen: at, Containers: []ManifestContainer{{Name: "app"}}}}
	if err := MergeManifests(&state, []Manifest{incoming}, at); err != nil {
		t.Fatal(err)
	}
	got := state.Manifests[0]
	if len(got.Pods) != 1 || got.Attempts != 1 || got.Done || !got.BreakAt.Equal(original.BreakAt) || !got.CapturedAt.Equal(original.CapturedAt) {
		t.Fatalf("late proof lost or rewound immutable progress: %+v", got)
	}
	incoming.Pods[0].UID = "replacement"
	if err := MergeManifests(&state, []Manifest{incoming}, at); err != nil {
		t.Fatal(err)
	}
	if state.Manifests[0].Pods[0].UID != "pod" {
		t.Fatal("existing pinned membership changed")
	}
}
