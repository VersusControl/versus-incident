package breaklogs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type windowScriptReader struct {
	read func(LinkedQuery) Result
}

func TestLogPressureReservesPendingDeletedIdentity(t *testing.T) {
	now := time.Now().UTC()
	pod := ManifestPod{Namespace: "shop", RootUID: "root", Name: "api-0", UID: "deleted", CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-time.Minute), DeletedAt: now.Add(-time.Minute), Containers: []ManifestContainer{{Name: "app"}}}
	manifest := Manifest{ID: "pending", Namespace: "shop", RootUID: "root", BreakAt: now.Add(-time.Minute), From: now.Add(-5 * time.Minute), To: now.Add(time.Minute), CapturedAt: now, SettleAt: now.Add(90 * time.Second), Pods: []ManifestPod{pod}}
	state := State{Manifests: []Manifest{manifest}, PodHistory: []ManifestPod{pod}}
	incoming := []Snapshot{}
	for i := 0; i < 137; i++ {
		at := now.Add(time.Duration(i-137) * time.Second)
		item := Snapshot{Cluster: "cluster", Namespace: "shop", RootUID: "root", Pod: fmt.Sprint(i), PodUID: fmt.Sprint(i), Container: "app", Slot: "previous", CapturedAt: at, From: at.Add(-time.Minute), To: at}
		for j := 0; j < 15; j++ {
			item.Lines = append(item.Lines, Line{At: at, Text: strings.Repeat("x", 4096)})
		}
		item.ID = Identity(item)
		incoming = append(incoming, item)
	}
	if err := Merge(&state, incoming, nil, "", now); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if len(raw) > MaxBytes || len(state.Manifests) != 1 || state.Manifests[0].ID != "pending" || len(state.PodHistory) != 1 {
		t.Fatalf("log pressure destroyed reserved identity: bytes=%d manifests=%d history=%d", len(raw), len(state.Manifests), len(state.PodHistory))
	}
	if len(state.Snapshots) >= len(incoming) || state.Snapshots[len(state.Snapshots)-1].ID != incoming[len(incoming)-1].ID {
		t.Fatal("oldest log evidence was not pruned before newest excerpt")
	}
}

func TestLiveHistorySamplingRetainsScopedCoverageWithoutInventingUIDLifetime(t *testing.T) {
	now := time.Now().UTC()
	state := State{}
	for pass := 0; pass < 3; pass++ {
		pods := []ManifestPod{}
		for i := 0; i < 60+pass; i++ {
			pod := ManifestPod{Namespace: "shop", RootUID: "root", Name: fmt.Sprint(i), UID: fmt.Sprintf("uid%02d", i), CreatedAt: now.Add(-time.Hour), Containers: []ManifestContainer{{Name: "app"}}}
			if i == 59 {
				pod.Containers[0].Restarts = 4
			}

			pods = append(pods, pod)
		}
		at := now.Add(time.Duration(pass) * time.Second)
		ObservePods(&state, pods, at)
		if err := Merge(&state, nil, nil, "", at); err != nil {
			t.Fatal(err)
		}
		broken := false
		for _, pod := range state.PodHistory {
			broken = broken || pod.UID == "uid59"
		}
		if !broken || len(state.PodHistory) > MaxHistoryPodsPerRoot {
			t.Fatalf("broken candidate displaced by healthy sampling: broken=%v count=%d", broken, len(state.PodHistory))
		}
		for _, gap := range state.PodHistoryGaps {
			if (gap.Reason != "sampled" && gap.Reason != "budget") || !gap.From.Equal(now.Add(-time.Hour)) || !gap.To.Equal(at) || gap.PodUID != "" {
				t.Fatalf("sampling lost scoped coverage or invented UID lifetime: %+v", gap)
			}
		}
		before, _ := json.Marshal(state)
		if err := Merge(&state, nil, nil, "", at); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(state)
		if string(before) != string(after) {
			t.Fatal("repeated bounding changed committed sampling coverage")
		}
	}
}

func TestRetiredIdentityLossIsScopedToRecordedExpiredLifetime(t *testing.T) {
	now := time.Now().UTC()
	state := State{}
	for i := 0; i < 60; i++ {
		state.PodHistory = append(state.PodHistory, ManifestPod{Namespace: "shop", RootUID: "root", Name: fmt.Sprint(i), UID: fmt.Sprint(i), CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-time.Minute), DeletedAt: now.Add(-time.Minute), Containers: []ManifestContainer{{Name: "app"}}})
	}
	for pass := 0; pass < 3; pass++ {
		at := now.Add(time.Duration(pass) * time.Second)
		if err := Merge(&state, nil, nil, "", at); err != nil {
			t.Fatal(err)
		}
		if len(state.PodHistoryGaps) != 1 {
			t.Fatalf("actual expired identity loss missing: %+v", state.PodHistoryGaps)
		}
		gap := state.PodHistoryGaps[0]
		if gap.Reason != "budget" || gap.Namespace != "shop" || gap.RootUID != "root" || !gap.From.Equal(now.Add(-time.Hour)) || !gap.To.Equal(now.Add(-time.Minute)) {
			t.Fatalf("expired gap widened with each pass: %+v", gap)
		}
	}
}

func TestHealthyEvictionCoverageClosesAtNextInventoryAndExpiresAtHorizon(t *testing.T) {
	now := time.Now().UTC()
	state := State{}
	pods := []ManifestPod{}
	for i := 0; i <= MaxHistoryPodsPerRoot; i++ {
		pods = append(pods, ManifestPod{Namespace: "shop", RootUID: "root", Name: fmt.Sprint(i), UID: fmt.Sprintf("uid%02d", i), CreatedAt: now.Add(-time.Hour), Containers: []ManifestContainer{{Name: "app"}}})
	}
	ObservePods(&state, pods, now)
	if err := Merge(&state, nil, nil, "", now); err != nil {
		t.Fatal(err)
	}
	next := now.Add(time.Minute)
	ObservePods(&state, nil, next)
	if err := Merge(&state, nil, nil, "", next); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{next.Add(time.Second), next.Add(Retention)} {
		ObservePods(&state, nil, at)
		if err := Merge(&state, nil, nil, "", at); err != nil {
			t.Fatal(err)
		}
		if len(state.PodHistoryGaps) != 1 || state.PodHistoryGaps[0].Reason != "budget" || !state.PodHistoryGaps[0].To.Equal(next) || state.PodHistoryGaps[0].PodUID != "" {
			t.Fatalf("loss expired early or grew beyond next committed inventory: %+v", state.PodHistoryGaps)
		}
		for _, pod := range state.PodHistory {
			if !pod.LastSeen.Equal(now) || !pod.DeletedAt.Equal(now) {
				t.Fatalf("coverage extension fabricated UID lifetime: %+v", pod)
			}
		}
	}
	if err := Merge(&state, nil, nil, "", next.Add(Retention+time.Nanosecond)); err != nil || len(state.PodHistoryGaps) != 0 {
		t.Fatalf("coverage did not expire beyond the 24h horizon: %+v err=%v", state.PodHistoryGaps, err)
	}
}

func TestHistoryLossMarkerPressureCoarsensOnlyAffectedNamespacesIdempotently(t *testing.T) {
	now := time.Now().UTC()
	state := State{}
	for i := 0; i < 5000; i++ {
		state.PodHistoryGaps = append(state.PodHistoryGaps, Gap{Namespace: fmt.Sprintf("shop-%d", i%2), RootUID: fmt.Sprintf("root-%06d", i), From: now.Add(-time.Minute), To: now, Reason: "budget"})
	}
	if err := Merge(&state, nil, nil, "", now); err != nil {
		t.Fatal(err)
	}
	if len(state.PodHistoryGaps) != 2 {
		t.Fatalf("marker pressure was not compacted: %d", len(state.PodHistoryGaps))
	}
	for _, gap := range state.PodHistoryGaps {
		if gap.RootUID != "" || gap.PodUID != "" || !strings.HasPrefix(gap.Namespace, "shop-") || !gap.From.Equal(now.Add(-time.Minute)) || !gap.To.Equal(now) {
			t.Fatalf("unsafe coarsening: %+v", gap)
		}
	}
	before, _ := json.Marshal(state)
	if err := Merge(&state, nil, nil, "", now); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) || len(after) > MaxMetadataBytes {
		t.Fatal("coarsened loss metadata not idempotent or bounded")
	}
}

func TestNextInventoryCannotResurrectExpiredHealthySamplingLoss(t *testing.T) {
	now := time.Now().UTC()
	state := State{PodHistoryAt: now.Add(-Retention - time.Second), PodHistoryGaps: []Gap{{Namespace: "shop", RootUID: "root", From: now.Add(-2 * Retention), To: now.Add(-Retention - time.Second), Reason: "sampled"}}}
	ObservePods(&state, nil, now)
	if err := Merge(&state, nil, nil, "", now); err != nil || len(state.PodHistoryGaps) != 0 {
		t.Fatalf("expired sampling was resurrected by later inventory: %+v err=%v", state.PodHistoryGaps, err)
	}
}

func TestPendingManifestPriorityAndScopedLossUnderMetadataPressure(t *testing.T) {
	now := time.Now().UTC()
	state := State{}
	for i := 0; i < 600; i++ {
		root := fmt.Sprint(i)
		state.Manifests = append(state.Manifests, Manifest{ID: root, Namespace: "shop", RootUID: root, BreakAt: now, From: now.Add(-time.Minute), To: now.Add(time.Minute), CapturedAt: now, Done: i != 0,
			Pods: []ManifestPod{{Namespace: "shop", RootUID: root, Name: "api", UID: root, CreatedAt: now.Add(-time.Hour), LastSeen: now, Node: strings.Repeat("n", 4096)}}})
	}
	if err := Merge(&state, nil, nil, "", now); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, manifest := range state.Manifests {
		found = found || manifest.ID == "0"
	}
	if !found {
		t.Fatal("completed metadata displaced pending occurrence")
	}
	// Even a pending manifest that cannot itself fit is lost explicitly.
	pending := state.Manifests[len(state.Manifests)-1]
	pending.ID, pending.Done = "oversized-pending", false
	pending.Pods[0].Node = strings.Repeat("n", MaxMetadataBytes)
	state.Manifests = []Manifest{pending}
	state.Gaps = nil
	if err := Merge(&state, nil, nil, "", now); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if len(raw) > MaxMetadataBytes || len(state.Manifests) != 0 || len(state.Gaps) != 1 || state.Gaps[0].SnapshotID != pending.ID || state.Gaps[0].RootUID != pending.RootUID || !state.Gaps[0].From.Equal(pending.From) || !state.Gaps[0].To.Equal(pending.To) {
		t.Fatalf("unrepresentable pending manifest lost without scoped marker: bytes=%d gaps=%+v", len(raw), state.Gaps)
	}
}

func (reader windowScriptReader) Preview(LinkedQuery) string { return "" }
func (reader windowScriptReader) Read(_ context.Context, query LinkedQuery) (Result, error) {
	return reader.read(query), nil
}

func TestManifestEncodingAndCombinedByteBudget(t *testing.T) {
	now := time.Now().UTC()
	manifest := Manifest{ID: "selected", Namespace: "shop", RootUID: "root", BreakAt: now, From: now.Add(-time.Minute), To: now, CapturedAt: now,
		Pods: []ManifestPod{{Namespace: "shop", RootUID: "root", Name: "api-0", UID: "pod", CreatedAt: now.Add(-time.Hour), LastSeen: now, Containers: []ManifestContainer{{Name: "app"}}}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	allowed := strings.Fields("id occurrence_id namespace root_uid break_at from to captured_at pods truncated settle_at attempts done settling_until")
	for key := range object {
		found := false
		for _, candidate := range allowed {
			found = found || candidate == key
		}
		if !found {
			t.Fatalf("non-identity manifest field %q", key)
		}
	}
	for _, forbidden := range []string{`"env"`, `"labels"`, `"text"`, `"message"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("manifest encodes forbidden field %s", forbidden)
		}
	}
	state := State{}
	for i := 0; i < 12; i++ {
		item := manifest
		item.ID = fmt.Sprint(i)
		item.Pods = append([]ManifestPod(nil), manifest.Pods...)
		item.Pods[0].Node = strings.Repeat("n", 1<<20)
		state.Manifests = append(state.Manifests, item)
	}
	state.PodHistory = append([]ManifestPod(nil), state.Manifests[0].Pods...)
	if err := Merge(&state, nil, nil, "", now); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(state)
	if err != nil || len(raw) > MaxBytes || len(state.Manifests) == 12 {
		t.Fatalf("combined manifest/history cap: bytes=%d manifests=%d err=%v", len(raw), len(state.Manifests), err)
	}
	if err := Merge(&state, nil, nil, "", now.Add(Retention+time.Nanosecond)); err != nil || len(state.Manifests) != 0 || len(state.PodHistory) != 0 {
		t.Fatalf("manifest/history retention: %+v %v", state, err)
	}
}

func TestLinkedSplitBoundariesAndSameTimestampExhaustion(t *testing.T) {
	from := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Minute)
	middle := from.Add(30 * time.Second)
	query := LinkedQuery{Namespace: "shop", Pods: []string{"api-0"}, From: from, To: to, Limit: 2000}
	t.Run("disjoint leaves own split timestamp once", func(t *testing.T) {
		calls := []LinkedQuery{}
		reader := windowScriptReader{read: func(next LinkedQuery) Result {
			calls = append(calls, next)
			if len(calls) == 1 {
				return Result{Truncated: true}
			}
			// Deliberately return boundary/out-of-window rows from both leaves.
			return Result{Lines: []LinkedLine{{At: from}, {At: middle.Add(-time.Nanosecond)}, {At: middle}, {At: to}, {At: to.Add(time.Nanosecond)}}}
		}}
		result, err := ReadWindow(t.Context(), reader, query)
		if err != nil || result.Truncated || len(calls) != 3 || len(result.Complete) != 2 || len(result.Lines) != 4 {
			t.Fatalf("split result=%+v calls=%+v err=%v", result, calls, err)
		}
		if !calls[1].From.Equal(middle) || !calls[2].To.Equal(middle.Add(-time.Nanosecond)) {
			t.Fatalf("overlapping or reordered leaves: %+v", calls)
		}
		count := 0
		for _, line := range result.Lines {
			if line.At.Equal(middle) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("split timestamp accepted %d times", count)
		}
	})
	t.Run("same timestamp cannot prove complete", func(t *testing.T) {
		calls := 0
		reader := windowScriptReader{read: func(next LinkedQuery) Result {
			calls++
			lines := make([]LinkedLine, 2001)
			for i := range lines {
				lines[i] = LinkedLine{At: middle, Text: fmt.Sprint(i)}
			}
			return Result{Lines: lines, Truncated: true}
		}}
		result, err := ReadWindow(t.Context(), reader, query)
		if err != nil || calls != 3 || !result.Truncated || len(result.Complete) != 0 || len(result.Lines) > 6000 {
			t.Fatalf("same-time truncation falsely complete: %+v calls=%d err=%v", result, calls, err)
		}
	})
}

func TestLinkedLifetimeRefusesGuessedNamesAndUnknownRestart(t *testing.T) {
	now := time.Now().UTC()
	pod := ManifestPod{Name: "api-0", UID: "old", CreatedAt: now.Add(-time.Hour), LastSeen: now, DeletedAt: now, Containers: []ManifestContainer{{Name: "app", Restarts: 4, StartedAt: now.Add(-time.Minute), TerminationAt: now.Add(-2 * time.Minute)}}}
	for _, test := range []struct {
		name string
		line LinkedLine
	}{
		{"name prefix is not identity", LinkedLine{At: now, Pod: "api-1", Container: "app"}},
		{"before creation", LinkedLine{At: pod.CreatedAt.Add(-time.Nanosecond), Pod: pod.Name, Container: "app"}},
		{"after deletion", LinkedLine{At: now.Add(time.Nanosecond), Pod: pod.Name, Container: "app"}},
		{"unobserved previous restart", LinkedLine{At: now.Add(-3 * time.Minute), Pod: pod.Name, Container: "app"}},
		{"restart gap", LinkedLine{At: now.Add(-90 * time.Second), Pod: pod.Name, Container: "app"}},
		{"unknown container", LinkedLine{At: now, Pod: pod.Name, Container: "sidecar"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, ok := ResolveLinkedLine([]ManifestPod{pod}, test.line); ok {
				t.Fatal("guessed identity/lifetime accepted", test.line)
			}
		})
	}
}
