package kubernetes

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func degradedDeploymentRecord(uid, name string, brokeAt time.Time) kubeindex.Record {
	return kubeindex.Record{
		UID: uid, Kind: "Deployment", Namespace: "shop", Name: name,
		Replicas: 4, Available: 3,
		AvailableStatus:       "False",
		AvailableTransitionAt: brokeAt,
		Summary:               map[string]string{"desired_replicas": "4", "available_replicas": "3"},
	}
}

func healthyDeploymentRecord(uid, name string) kubeindex.Record {
	return kubeindex.Record{
		UID: uid, Kind: "Deployment", Namespace: "shop", Name: name,
		Replicas: 1, Available: 1,
		Summary: map[string]string{"desired_replicas": "1", "available_replicas": "1"},
	}
}

func issueRootNames(page IssuePage) []string {
	names := make([]string, 0, len(page.Items))
	for _, issue := range page.Items {
		names = append(names, issue.Root.Name)
	}
	return names
}

func sameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for position := range got {
		if got[position] != want[position] {
			return false
		}
	}
	return true
}

// severity groups stay ordered, broken issues precede non-broken ones,
// later BrokeAt wins, and issues with no break time sort last in their group.
func TestIssuesNewestBreakFirstStableAcrossPages(t *testing.T) {
	base := time.Now().UTC()
	records := []kubeindex.Record{
		brokenDeploymentRecord("crit-new", "crit-new", base.Add(-10*time.Minute)),
		brokenDeploymentRecord("crit-old", "crit-old", base.Add(-70*time.Minute)),
		healthyDeploymentRecord("healthy", "healthy"),
		{UID: "healthy-pod", Kind: "Pod", Namespace: "shop", Name: "healthy-pod", Phase: "Failed",
			Summary: map[string]string{"phase": "Failed"},
			Owners:  []kubeindex.OwnerRef{{UID: "healthy", Kind: "Deployment", Name: "healthy", Namespace: "shop"}}},
		degradedDeploymentRecord("warn-new", "warn-new", base.Add(-5*time.Minute)),
		degradedDeploymentRecord("warn-old", "warn-old", base.Add(-40*time.Minute)),
	}
	service := issueHistoryService(t, storage.NewMemory())
	seedInventory(t, service, records)

	page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"crit-new", "crit-old", "healthy", "warn-new", "warn-old"}
	if got := issueRootNames(page); !sameNames(got, want) {
		t.Fatalf("issue order = %v want %v", got, want)
	}

	// Pages 1 and 2 must keep the same order and the same totals.
	paged := []string{}
	cursor := ""
	for {
		next, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if next.Total != len(want) || next.Limit != 2 {
			t.Fatalf("page total=%d limit=%d want %d/2", next.Total, next.Limit, len(want))
		}
		paged = append(paged, issueRootNames(next)...)
		if next.Next == "" {
			break
		}
		cursor = next.Next
	}
	if !sameNames(paged, want) {
		t.Fatalf("paged issue order = %v want %v", paged, want)
	}
}

// snapshot lists report the filtered length and the page size on every
// page, and a RootUID-selected Issues page has a total of one.
func TestIssuesTotalsAndRootUIDSelection(t *testing.T) {
	base := time.Now().UTC()
	service := issueHistoryService(t, storage.NewMemory())
	seedInventory(t, service, []kubeindex.Record{
		brokenDeploymentRecord("one", "one", base.Add(-time.Minute)),
		brokenDeploymentRecord("two", "two", base.Add(-2*time.Minute)),
		brokenDeploymentRecord("three", "three", base.Add(-3*time.Minute)),
		brokenDeploymentRecord("four", "four", base.Add(-4*time.Minute)),
		brokenDeploymentRecord("five", "five", base.Add(-5*time.Minute)),
	})
	cursor := ""
	for number, wantItems := range []int{2, 2, 1} {
		page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: cursor})
		if err != nil || len(page.Items) != wantItems || page.Total != 5 || page.Limit != 2 {
			t.Fatalf("issues page %d: items=%d total=%d limit=%d err=%v", number+1, len(page.Items), page.Total, page.Limit, err)
		}
		cursor = page.Next
	}
	if cursor != "" {
		t.Fatalf("the last issues page issued another cursor %q", cursor)
	}
	root, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", RootUID: "three", IncludeChanges: true, Limit: 2})
	if err != nil || len(root.Items) != 1 || root.Total != 1 || root.Limit != 2 || root.Items[0].Root.Name != "three" {
		t.Fatalf("root-scoped issues: items=%d total=%d limit=%d err=%v", len(root.Items), root.Total, root.Limit, err)
	}
}

func TestWorkloadAndCachedResourceTotals(t *testing.T) {
	base := time.Now().UTC()
	service := issueHistoryService(t, storage.NewMemory())
	seedInventory(t, service, []kubeindex.Record{
		brokenDeploymentRecord("w-1", "w-1", base.Add(-time.Minute)),
		brokenDeploymentRecord("w-2", "w-2", base.Add(-2*time.Minute)),
		brokenDeploymentRecord("w-3", "w-3", base.Add(-3*time.Minute)),
		brokenDeploymentRecord("w-4", "w-4", base.Add(-4*time.Minute)),
	})
	cursor := ""
	for pageNumber, wantItems := range []int{2, 2} {
		page, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 2, Cursor: cursor})
		if err != nil || len(page.Items) != wantItems || page.Total != 4 || page.Limit != 2 {
			t.Fatalf("workloads page %d: items=%d total=%d limit=%d err=%v", pageNumber, len(page.Items), page.Total, page.Limit, err)
		}
		cursor = page.Next
	}

	nodeService := issueHistoryService(t, storage.NewMemory())
	seedInventory(t, nodeService, []kubeindex.Record{
		{UID: "node-1", Kind: "Node", Name: "node-1", NodeReady: true},
		{UID: "node-2", Kind: "Node", Name: "node-2", NodeReady: true},
		{UID: "node-3", Kind: "Node", Name: "node-3", NodeReady: true},
	})
	cursor = ""
	for pageNumber, wantItems := range []int{2, 1} {
		page, err := nodeService.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 2, Continue: cursor})
		if err != nil || len(page.Items) != wantItems || page.Total != 3 || page.Limit != 2 {
			t.Fatalf("node page %d: items=%d total=%d limit=%d err=%v", pageNumber, len(page.Items), page.Total, page.Limit, err)
		}
		cursor = page.Continue
	}
}

func historyTotalCrashEvent(position int, at, observed time.Time) kubechanges.CrashEvent {
	event := kubechanges.CrashEvent{
		Cluster: "cluster", Namespace: "shop", Pod: "pod-x", PodUID: "pod-x",
		RootUID: "root-x", RootKind: "Deployment", RootName: "root-x", Container: "app",
		Type: "status", WaitingReason: "CrashLoopBackOff", RestartCount: int32(position),
		At: at, TimeSource: "observed_at", ObservedAt: observed,
	}
	event.ID = kubechanges.CrashID(event)
	return event
}

// history lists keep a bounded first-page total and mark it capped.
func TestHistoryTotalsStayBoundedAndCapped(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-30 * time.Minute)
	service := issueHistoryService(t, storage.NewMemory())
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:fixture-totals-base", At: now, Epoch: "fixture"})
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	checkpoint, err := service.evidenceStore().EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]storage.EvidenceRecord, 0, 2002)
	for position := 0; position < 1001; position++ {
		change := Change{
			ID: fmt.Sprintf("total-%04d", position), Cluster: "cluster", Kind: "Deployment",
			Namespace: "shop", Name: "root-x", UID: "root-x", RootUID: "root-x",
			Type: kubechanges.ImageChanged, At: at.Add(time.Duration(position) * time.Microsecond), Source: "reconstructed",
		}
		raw, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, storage.EvidenceRecord{
			AuthorizationKind: "Deployment", ID: "change:" + change.ID, Type: "change",
			Kind: change.Kind, Name: change.Name, Namespace: change.Namespace, UID: change.UID,
			RootUID: change.RootUID, Rule: string(change.Type), At: change.At, ObservedAt: now,
			Epoch: "fixture", Payload: raw,
		})
		crash := historyTotalCrashEvent(position, at.Add(time.Duration(position)*time.Microsecond), now)
		crashRaw, err := json.Marshal(crash)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, storage.EvidenceRecord{
			AuthorizationKind: "Pod", ID: "crash:" + crash.ID, Type: "crash",
			Kind: crash.RootKind, Name: crash.Pod, Namespace: crash.Namespace, UID: crash.PodUID,
			RootUID: crash.RootUID, PodUID: crash.PodUID, At: crash.At, ObservedAt: crash.ObservedAt,
			Epoch: "fixture", Payload: crashRaw,
		})
	}
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:fixture-totals", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: now, Epoch: "fixture", Records: records})

	changes, err := service.Changes(t.Context(), ChangeQuery{Limit: 5})
	if err != nil || changes.Total != 1001 || !changes.CountCapped || changes.Limit != 5 || len(changes.Items) != 5 || changes.Next == "" {
		t.Fatalf("changes total=%d capped=%t limit=%d items=%d next=%t err=%v", changes.Total, changes.CountCapped, changes.Limit, len(changes.Items), changes.Next != "", err)
	}
	crashes, err := service.Crashes(t.Context(), kubechanges.CrashQuery{Limit: 5})
	if err != nil || crashes.Total != 1001 || !crashes.CountCapped || crashes.Limit != 5 || len(crashes.Items) != 5 || crashes.Next == "" {
		t.Fatalf("crashes total=%d capped=%t limit=%d items=%d next=%t err=%v", crashes.Total, crashes.CountCapped, crashes.Limit, len(crashes.Items), crashes.Next != "", err)
	}
}

// An empty Issues list still reports its page size, and the page size follows
// the shared default and maximum.
func TestIssuesTotalsForEmptyListAndLimitBounds(t *testing.T) {
	service := issueHistoryService(t, storage.NewMemory())
	seedInventory(t, service, []kubeindex.Record{healthyDeploymentRecord("ok", "ok")})
	empty, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop"})
	if err != nil || len(empty.Items) != 0 || empty.Total != 0 || empty.Limit != 50 || empty.Next != "" {
		t.Fatalf("empty issues: items=%d total=%d limit=%d next=%q err=%v", len(empty.Items), empty.Total, empty.Limit, empty.Next, err)
	}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["limit"] != float64(50) {
		t.Fatalf("empty page did not carry its limit on the wire: %s", raw)
	}

	base := time.Now().UTC()
	seedInventory(t, service, []kubeindex.Record{
		brokenDeploymentRecord("one", "one", base.Add(-time.Minute)),
		brokenDeploymentRecord("two", "two", base.Add(-2*time.Minute)),
	})
	wide, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 5000})
	if err != nil || wide.Total != 2 || wide.Limit != 200 || len(wide.Items) != 2 {
		t.Fatalf("over-limit issues: items=%d total=%d limit=%d err=%v", len(wide.Items), wide.Total, wide.Limit, err)
	}
}

// Below the count cap a history list reports its exact first-page total.
func TestHistoryTotalIsExactBelowTheCap(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-30 * time.Minute)
	service := issueHistoryService(t, storage.NewMemory())
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:fixture-small-base", At: now, Epoch: "fixture"})
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	checkpoint, err := service.evidenceStore().EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]storage.EvidenceRecord, 0, 3)
	for position := 0; position < 3; position++ {
		change := Change{
			ID: fmt.Sprintf("small-%d", position), Cluster: "cluster", Kind: "Deployment",
			Namespace: "shop", Name: "root-x", UID: "root-x", RootUID: "root-x",
			Type: kubechanges.ImageChanged, At: at.Add(time.Duration(position) * time.Second), Source: "reconstructed",
		}
		raw, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, storage.EvidenceRecord{
			AuthorizationKind: "Deployment", ID: "change:" + change.ID, Type: "change",
			Kind: change.Kind, Name: change.Name, Namespace: change.Namespace, UID: change.UID,
			RootUID: change.RootUID, Rule: string(change.Type), At: change.At, ObservedAt: now,
			Epoch: "fixture", Payload: raw,
		})
	}
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:fixture-small", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: now, Epoch: "fixture", Records: records})

	changes, err := service.Changes(t.Context(), ChangeQuery{Limit: 2})
	if err != nil || changes.Total != 3 || changes.CountCapped || changes.Limit != 2 || len(changes.Items) != 2 || changes.Next == "" {
		t.Fatalf("changes total=%d capped=%t limit=%d items=%d next=%t err=%v", changes.Total, changes.CountCapped, changes.Limit, len(changes.Items), changes.Next != "", err)
	}
}
