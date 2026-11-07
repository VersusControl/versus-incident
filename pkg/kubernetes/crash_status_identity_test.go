package kubernetes

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestStableCrashLoopIsOneJournalRowPerRestart polls a Pod stuck in
// CrashLoopBackOff 1,000 times through the capture's crash projection and
// saves every poll the way a capture does. The Pod restarts every 250 polls.
// A stable state is one status row however often it is observed, and each
// restart adds exactly one status row and one termination row.
func TestStableCrashLoopIsOneJournalRowPerRestart(t *testing.T) {
	t.Parallel()
	const (
		polls        = 1000
		restartEvery = 250
	)
	provider := storage.NewMemory()
	t.Cleanup(func() { _ = provider.Close() })
	store := provider.(storage.EvidenceStore)
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "crashloop", ClusterUID: "physical"}
	pod := kubeindex.Record{UID: "pod-uid", Kind: "Pod", Name: "api", Namespace: "shop"}
	snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}
	origin := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Microsecond)
	restarts, finished := 3, origin.Add(-time.Minute)
	statusIDs, terminationIDs := map[string]int{}, map[string]int{}
	var high uint64
	for poll := 0; poll < polls; poll++ {
		observed := origin.Add(time.Duration(poll) * 30 * time.Second)
		restarted := poll > 0 && poll%restartEvery == 0
		if restarted {
			restarts++
			finished = observed.Add(-time.Second)
		}
		batch := &crashCaptureBatch{}
		batch.add("crashloop", pod, map[string]any{"containerStatuses": []any{map[string]any{
			"name": "app", "restartCount": float64(restarts),
			"state":     map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
			"lastState": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": float64(1), "finishedAt": finished.Format(time.RFC3339Nano)}},
		}}}, observed)
		events, gaps, read := batch.resolve(snapshot, observed)
		if len(events) != 2 || len(gaps) != 0 || !read {
			t.Fatalf("poll %d projected %d events, %d gaps", poll, len(events), len(gaps))
		}
		records := make([]storage.EvidenceRecord, 0, len(events))
		for _, crash := range events {
			switch crash.Type {
			case "status":
				statusIDs[crash.ID]++
			case "termination":
				terminationIDs[crash.ID]++
			}
			raw, err := json.Marshal(crash)
			if err != nil {
				t.Fatal(err)
			}
			// The same record a capture saves for a crash event.
			records = append(records, storage.EvidenceRecord{AuthorizationKind: "Pod", ID: "crash:" + crash.ID, Type: "crash", At: crash.At, ObservedAt: crash.ObservedAt, Namespace: crash.Namespace, Kind: crash.RootKind, UID: crash.PodUID, RootUID: crash.RootUID, PodUID: crash.PodUID, Epoch: "stable", Payload: raw})
		}
		cp, err := store.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.CommitEvidence(t.Context(), scope, storage.EvidenceBatch{ID: fmt.Sprintf("poll-%d", poll), Expected: cp.ID, Generation: cp.Generation, At: observed, Epoch: "stable", Records: records}); err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
		if cp, err = store.EvidenceCheckpoint(t.Context(), scope); err != nil {
			t.Fatal(err)
		}
		switch {
		case poll == 0 && cp.HighSequence != 2:
			t.Fatalf("first poll stored %d rows, want a status and a termination", cp.HighSequence)
		case restarted && cp.HighSequence != high+2:
			t.Fatalf("poll %d: a restart stored %d rows, want one status and one termination", poll, cp.HighSequence-high)
		case poll > 0 && !restarted && cp.HighSequence != high:
			t.Fatalf("poll %d: an unchanged crash loop stored %d rows", poll, cp.HighSequence-high)
		}
		high = cp.HighSequence
	}
	states := 1 + (polls-1)/restartEvery
	if len(statusIDs) != states || len(terminationIDs) != states {
		t.Fatalf("distinct status ids=%d termination ids=%d, want one per state (%d)", len(statusIDs), len(terminationIDs), states)
	}
	for id, seen := range statusIDs {
		if seen != restartEvery && seen != polls-restartEvery*(states-1) {
			t.Fatalf("status %s seen %d times: its identity changed inside a stable state", id, seen)
		}
	}
	if high != uint64(2*states) {
		t.Fatalf("journal holds %d rows, want %d", high, 2*states)
	}
}

// A termination whose reported finish time is ahead of the observation (the
// node clock runs ahead) keeps the identity of its finish time: it is the same
// termination before and after the observation catches up, a restart count
// change does not split it, and two terminations with the same restart count
// but different finish times stay apart. Only the shown time is clamped.
func TestTerminationIdentityFollowsFinishTimeUnderClockSkew(t *testing.T) {
	t.Parallel()
	pod := kubeindex.Record{UID: "skew-pod", Kind: "Pod", Namespace: "shop", Name: "api-0"}
	snapshot := kubeindex.Snapshot{Records: []kubeindex.Record{pod}, Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	termination := func(restarts int, finished, observed time.Time) (string, time.Time) {
		t.Helper()
		status := map[string]any{}
		raw := fmt.Sprintf(`{"containerStatuses":[{"name":"app","restartCount":%d,"state":{"running":{}},"lastState":{"terminated":{"exitCode":1,"reason":"Error","finishedAt":%q}}}]}`, restarts, finished.Format(time.RFC3339))
		if err := json.Unmarshal([]byte(raw), &status); err != nil {
			t.Fatal(err)
		}
		batch := &crashCaptureBatch{}
		batch.add("skew-cluster", pod, status, observed)
		events, _, _ := batch.resolve(snapshot, observed)
		for _, event := range events {
			if event.Type == "termination" {
				if !event.Valid() || event.At.After(observed) {
					t.Fatalf("termination %+v is invalid or shown after its observation", event)
				}
				return event.ID, event.At
			}
		}
		t.Fatal("no termination produced")
		return "", time.Time{}
	}
	finished := origin.Add(2 * time.Minute)
	ahead, shown := termination(3, finished, origin)
	if !shown.Equal(origin) {
		t.Fatalf("a finish time ahead of the observation was shown as %s, want the observation %s", shown, origin)
	}
	if restarted, _ := termination(4, finished, origin.Add(30*time.Second)); restarted != ahead {
		t.Fatal("a restart count change split one termination into two identities")
	}
	if caughtUp, _ := termination(4, finished, finished.Add(time.Minute)); caughtUp != ahead {
		t.Fatal("the termination changed identity once the observation passed its finish time")
	}
	if other, _ := termination(3, finished.Add(time.Minute), origin); other == ahead {
		t.Fatal("two terminations with different finish times merged into one identity")
	}
}
