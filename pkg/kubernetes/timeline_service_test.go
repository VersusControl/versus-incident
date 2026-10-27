package kubernetes

import (
	"errors"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestTimelineOneSnapshotOrdersAllSourcesAndSurvivesReplicaAndBackfill(t *testing.T) {
	provider := storage.NewMemory()
	scope := Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "epoch"}
	service := NewService(nil, scope, 0)
	service.SetChangeStorage(provider)
	now := time.Now().UTC()
	seedInventory(t, service, []kubeindex.Record{{UID: "pod", Kind: "Pod", Name: "pod", Namespace: "shop", Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 1}}}})
	store := service.changeStore()
	change := Change{ID: "old-change", Cluster: "cluster", UID: "pod", RootUID: "pod", Kind: "Pod", Name: "pod", Namespace: "shop", Type: kubechanges.Created, At: now.Add(-2 * time.Minute)}
	crash := kubechanges.CrashEvent{Cluster: "cluster", Namespace: "shop", Pod: "pod", PodUID: "pod", RootUID: "pod", RootKind: "Pod", RootName: "pod", Container: "app", Type: "termination", Reason: "Error", ExitCode: 1, RestartCount: 1, At: now.Add(-time.Minute), ObservedAt: now, TimeSource: "finished_at"}
	crash.ID = kubechanges.CrashID(crash)
	status := crash
	status.PodUID, status.RootUID, status.Type, status.Reason, status.ExitCode, status.TimeSource, status.At = "other", "other", "status", "", 0, "observed_at", now
	status.ID = kubechanges.CrashID(status)
	if err := store.CommitCapture([]Change{change}, nil, nil, "", now, []kubechanges.CrashEvent{crash, status}, nil, true); err != nil {
		t.Fatal(err)
	}
	query := TimelineQuery{Since: now.Add(-time.Hour), Until: now.Add(time.Second), Namespace: "shop", Limit: 1}
	first, err := service.Timeline(t.Context(), query)
	if err != nil || first.Total != 3 || first.CrashCount != 1 || first.Counts["break"] != 1 || first.Next == "" {
		t.Fatalf("first=%+v %v", first, err)
	}
	backfill := change
	backfill.ID, backfill.At = "late-backfill", now.Add(-90*time.Second)
	if err := store.Append([]Change{backfill}, now); err != nil {
		t.Fatal(err)
	}
	seedInventory(t, service, nil)
	replica := NewService(nil, scope, 0)
	replica.SetChangeStorage(provider)
	items := append([]TimelineItem{}, first.Items...)
	query.Cursor = first.Next
	for {
		page, err := replica.Timeline(t.Context(), query)
		if err != nil || page.Total != 3 || page.CrashCount != 1 {
			t.Fatalf("continuation=%+v %v", page, err)
		}
		items = append(items, page.Items...)
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	if len(items) != 3 {
		t.Fatalf("source-local paging lost rows: %d", len(items))
	}
	for i, item := range items {
		if item.ID == "change:late-backfill" || item.Type == "crash" && item.Crash.Type != "termination" {
			t.Fatal("snapshot admitted new/status row")
		}
		if i > 0 && items[i-1].At.Before(item.At) {
			t.Fatal("page boundary is not globally chronological")
		}
	}
	query.Cursor = first.Next
	query.Namespace = "other"
	if _, err := replica.Timeline(t.Context(), query); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("changed filter accepted cursor: %v", err)
	}
	foreign := NewService(nil, Scope{OrgID: "foreign", ClusterID: "cluster", CredentialID: "epoch"}, 0)
	foreign.SetChangeStorage(provider)
	query.Namespace = "shop"
	if _, err := foreign.Timeline(t.Context(), query); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("foreign scope accepted cursor: %v", err)
	}
}

func TestTimelineBreaksOnlyPagesAndCountsOnlyBreaks(t *testing.T) {
	provider := storage.NewMemory()
	scope := Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "epoch"}
	service := NewService(nil, scope, 0)
	service.SetChangeStorage(provider)
	now := time.Now().UTC()
	records := []kubeindex.Record{}
	for _, name := range []string{"pod-a", "pod-b", "pod-c"} {
		records = append(records, kubeindex.Record{UID: name, Kind: "Pod", Name: name, Namespace: "shop", Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 1}}})
	}
	records = append(records, kubeindex.Record{UID: "healthy", Kind: "Pod", Name: "healthy", Namespace: "shop", Phase: "Running", Summary: map[string]string{"phase": "Running"}})
	seedInventory(t, service, records)
	changes := []Change{}
	for index := 0; index < 4; index++ {
		changes = append(changes, Change{ID: "change-" + string(rune('a'+index)), Cluster: "cluster", UID: "healthy", RootUID: "healthy", Kind: "Pod", Name: "healthy", Namespace: "shop", Type: kubechanges.Created, At: now.Add(-time.Duration(index+1) * time.Minute)})
	}
	crash := kubechanges.CrashEvent{Cluster: "cluster", Namespace: "shop", Pod: "pod-a", PodUID: "pod-a", RootUID: "pod-a", RootKind: "Pod", RootName: "pod-a", Container: "app", Type: "termination", Reason: "Error", ExitCode: 1, RestartCount: 1, At: now.Add(-time.Minute), ObservedAt: now, TimeSource: "finished_at"}
	crash.ID = kubechanges.CrashID(crash)
	if err := service.changeStore().CommitCapture(changes, nil, nil, "", now, []kubechanges.CrashEvent{crash}, nil, true); err != nil {
		t.Fatal(err)
	}
	all, err := service.Timeline(t.Context(), TimelineQuery{Since: now.Add(-time.Hour), Until: now.Add(time.Second), Namespace: "shop", Limit: 50})
	if err != nil || all.Counts["change"] != 4 || all.Counts["crash"] != 1 || all.Counts["break"] != 3 || all.BreaksOnly {
		t.Fatalf("unfiltered timeline=%+v %v", all, err)
	}
	query := TimelineQuery{Since: all.Since, Until: all.Until, Namespace: "shop", Limit: 2, BreaksOnly: true}
	first, err := service.Timeline(t.Context(), query)
	if err != nil || !first.BreaksOnly || first.Total != 3 || first.CrashCount != 0 || first.Counts["break"] != 3 || first.Counts["change"] != 0 || first.Counts["crash"] != 0 || len(first.Items) != 2 || first.Next == "" {
		t.Fatalf("breaks-only first page=%+v %v", first, err)
	}
	items := append([]TimelineItem{}, first.Items...)
	query.Cursor = first.Next
	second, err := service.Timeline(t.Context(), query)
	if err != nil || second.Total != 3 || second.Next != "" {
		t.Fatalf("breaks-only second page=%+v %v", second, err)
	}
	items = append(items, second.Items...)
	if len(items) != 3 {
		t.Fatalf("breaks-only paging returned %d items", len(items))
	}
	for _, item := range items {
		if item.Type != "break" {
			t.Fatalf("breaks-only page carried a %s item", item.Type)
		}
	}
	query.BreaksOnly = false
	if _, err := service.Timeline(t.Context(), query); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("breaks-only cursor was accepted for the unfiltered timeline: %v", err)
	}
}

func TestTimelineExactResourceAndPodFiltersKeepTruthfulTotals(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	seedInventory(t, service, nil)
	now := time.Now().UTC()
	events := []kubechanges.CrashEvent{}
	for _, pod := range []string{"selected", "sibling"} {
		event := kubechanges.CrashEvent{Cluster: "cluster", Namespace: "shop", Pod: pod, PodUID: pod, RootUID: "workload", RootKind: "Deployment", RootName: "api", Container: "app", Type: "termination", Reason: "Error", ExitCode: 1, At: now, ObservedAt: now, TimeSource: "finished_at"}
		event.ID = kubechanges.CrashID(event)
		events = append(events, event)
	}
	change := Change{ID: "change", Cluster: "cluster", UID: "selected", RootUID: "workload", Kind: "Pod", Name: "selected", Namespace: "shop", Type: kubechanges.Created, At: now.Add(-time.Minute)}
	if err := service.changeStore().CommitCapture([]Change{change}, nil, nil, "", now, events, nil, true); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		query          TimelineQuery
		total, crashes int
	}{
		{TimelineQuery{ResourceUID: "selected"}, 2, 1},
		{TimelineQuery{ResourceUID: "workload"}, 2, 2},
		{TimelineQuery{PodUID: "selected"}, 2, 1},
		{TimelineQuery{RootUID: "workload"}, 3, 2},
	} {
		test.query.Until = now
		page, err := service.Timeline(t.Context(), test.query)
		if err != nil || page.Total != test.total || page.CrashCount != test.crashes {
			t.Fatalf("query=%+v page=%+v %v", test.query, page, err)
		}
	}
	page, err := service.Crashes(t.Context(), kubechanges.CrashQuery{PodUID: "selected", Until: now, Limit: 1})
	if err != nil || page.CrashCount != 1 || len(page.Items) != 1 || page.Items[0].PodUID != "selected" {
		t.Fatalf("exact Pod crashes=%+v %v", page, err)
	}
}

func TestUsageExplicitLimitAndSampleBoundPagingDoNotCutCachedTotals(t *testing.T) {
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	seedInventory(t, service, nil)
	cache := service.changes.inventories[service.indexScope()]
	now := time.Now().UTC()
	cache.generation.Usage = Usage{ObservedAt: now, Availability: "available", Pods: []ResourceUsage{{Name: "a"}, {Name: "b"}, {Name: "c"}}, PodMetrics: MetricsSourceStatus{Total: 3, Complete: true}}
	first, err := service.Usage(t.Context(), "", 1)
	if err != nil || len(first.Pods) != 1 || !first.Truncated || first.PodMetrics.Total != 3 || first.Next == "" {
		t.Fatalf("limit ignored: %+v %v", first, err)
	}
	second, err := service.UsagePage(t.Context(), "", 1, first.Next)
	if err != nil || len(second.Pods) != 1 || second.Pods[0].Name != "b" || second.PodMetrics.Total != 3 || second.Truncated {
		t.Fatalf("page=%+v %v", second, err)
	}
	cache.generation.Usage.ObservedAt = now.Add(time.Second)
	if _, err := service.UsagePage(t.Context(), "", 1, first.Next); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("changed metric generation accepted cursor: %v", err)
	}
}
