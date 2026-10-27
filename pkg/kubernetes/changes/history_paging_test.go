package changes

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestHistoryRootCursorSurvivesInsertAndRemovedRoot(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore(storage.NewMemory(), index.Scope{OrgID: "default", ClusterID: "cluster"})
	makeChange := func(id, uid, root string, at time.Time) Change {
		return Change{ID: id, UID: uid, RootUID: root, Cluster: "cluster", Kind: "Pod", Name: uid, Namespace: "shop", Type: Created, At: at}
	}
	items := []Change{
		makeChange("one", "pod-one", "removed-root", now.Add(-3*time.Minute)),
		makeChange("two", "pod-two", "removed-root", now.Add(-2*time.Minute)),
		makeChange("foreign", "pod-foreign", "other-root", now.Add(-time.Minute)),
	}
	if err := store.Append(items, now); err != nil {
		t.Fatal(err)
	}
	query := Query{RootUID: "removed-root", Namespace: "shop", Limit: 1}
	first, err := store.Query(query, now)
	if err != nil || len(first.Items) != 1 || first.Items[0].ID != "two" || first.Next == "" {
		t.Fatalf("first=%+v %v", first, err)
	}
	if err := store.Append([]Change{makeChange("inserted", "new", "removed-root", now.Add(time.Second))}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	query.Cursor = first.Next
	second, err := store.Query(query, now.Add(2*time.Second))
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != "one" || second.Next != "" {
		t.Fatalf("second=%+v %v", second, err)
	}
	query.RootUID = "other-root"
	if _, err := store.Query(query, now); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("cursor accepted changed root")
	}
}

func TestCrashCountCountsOnlyTerminationAcrossPages(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore(storage.NewMemory(), index.Scope{OrgID: "default", ClusterID: "cluster"})
	events := []CrashEvent{}
	for i, kind := range []string{"status", "termination", "status", "termination"} {
		event := CrashEvent{Cluster: "cluster", Namespace: "shop", Pod: "pod", PodUID: "pod-" + strconv.Itoa(i), RootUID: "root", RootKind: "Deployment", RootName: "api", Container: "app", Type: kind, Reason: "Error", ExitCode: 1, RestartCount: 1, At: now.Add(-time.Minute), ObservedAt: now, TimeSource: "finished_at"}
		if kind == "status" {
			event.Reason, event.ExitCode, event.TimeSource, event.At = "", 0, "observed_at", now
		}
		event.ID = CrashID(event)
		events = append(events, event)
	}
	if err := store.CommitCapture(nil, nil, nil, "", now, events, nil, true); err != nil {
		t.Fatal(err)
	}
	query := CrashQuery{RootUID: "root", Limit: 1}
	for pageNumber := 0; ; pageNumber++ {
		page, err := store.QueryCrashes(query, now)
		if err != nil || page.CrashCount != 2 || len(page.Items) != 1 {
			t.Fatalf("page %d: %+v %v", pageNumber, page, err)
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
}
