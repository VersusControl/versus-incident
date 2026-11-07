package kubernetes

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func timelineHistoryFixture(t *testing.T) (*Service, storage.Provider, storage.EvidenceStore, storage.EvidenceScope, *atomic.Int32) {
	t.Helper()
	reads := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.URL.Path == "/apis/authentication.k8s.io/v1/selfsubjectreviews" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			writeJSON(w, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"uid": "physical", "name": "kube-system"}})
			return
		}
		items := []any{}
		if r.URL.Path == "/api/v1/namespaces" {
			items = append(items, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"uid": "physical", "name": "kube-system"}})
		}
		writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": items})
	}))
	t.Cleanup(server.Close)
	provider := storage.NewMemory()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "epoch"})
	service.SetChangeStorage(provider)
	service.changes.evidenceEnabled = true
	store := reviewProvedHistoryStore{EvidenceStore: provider.(storage.EvidenceStore), t: t, service: service, reads: reads}
	return service, provider, store, storage.EvidenceScope{OrgID: "default", ClusterID: "cluster", ClusterUID: "physical"}, reads
}

func commitTimelineHistory(t *testing.T, store storage.EvidenceStore, scope storage.EvidenceScope, batch storage.EvidenceBatch) {
	t.Helper()
	batch.ID = "capture:" + batch.ID
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal("commit proved timeline fixture:", err)
	}
}

func timelineChangeRecord(change Change) storage.EvidenceRecord {
	if change.Cluster == "" {
		change.Cluster = "cluster"
	}
	if change.Name == "" {
		change.Name = change.UID
	}
	raw, _ := json.Marshal(change)
	return storage.EvidenceRecord{ID: "change:" + change.ID, Type: "change", Kind: change.Kind, Name: change.Name, Namespace: change.Namespace, UID: change.UID, RootUID: change.RootUID, Rule: string(change.Type), At: change.At, ObservedAt: change.At, Epoch: "fixture", Payload: raw}
}

func timelineCrashRecord(crash kubechanges.CrashEvent) storage.EvidenceRecord {
	raw, _ := json.Marshal(crash)
	return storage.EvidenceRecord{ID: "crash:" + crash.ID, Type: "crash", Kind: crash.RootKind, Namespace: crash.Namespace, UID: crash.PodUID, RootUID: crash.RootUID, PodUID: crash.PodUID, At: crash.At, ObservedAt: crash.ObservedAt, Epoch: "fixture", Payload: raw}
}

func timelineIssueObservation(name string, at time.Time) storage.EvidenceIssue {
	issue := Issue{Root: ObjectRef{UID: name, Kind: "Pod", Name: name, Namespace: "shop"}, Rule: "pod.crash_loop", Severity: "warning", Count: 1, Broken: true, FirstSeen: at, LastSeen: at}
	raw, _ := json.Marshal(issue)
	return storage.EvidenceIssue{AuthorizationKind: "Pod", Key: name + ":pod.crash_loop", UID: name, Kind: "Pod", Namespace: "shop", Rule: issue.Rule, Broken: true, FirstSeen: at, LastSeen: at, Epoch: "fixture", Payload: raw}
}

func TestTimelineOneSnapshotOrdersAllSourcesAndSurvivesReplicaAndBackfill(t *testing.T) {
	service, provider, store, scope, reads := timelineHistoryFixture(t)
	now := time.Now().UTC()
	change := Change{ID: "old-change", Cluster: "cluster", UID: "pod", RootUID: "pod", Kind: "Pod", Name: "pod", Namespace: "shop", Type: kubechanges.Created, At: now.Add(-2 * time.Minute)}
	crash := kubechanges.CrashEvent{Cluster: "cluster", Namespace: "shop", Pod: "pod", PodUID: "pod", RootUID: "pod", RootKind: "Pod", RootName: "pod", Container: "app", Type: "termination", Reason: "Error", ExitCode: 1, RestartCount: 1, At: now.Add(-time.Minute), ObservedAt: now, TimeSource: "finished_at"}
	crash.ID = kubechanges.CrashID(crash)
	observedAt := now.Add(-3 * time.Minute)
	commitTimelineHistory(t, store, scope, storage.EvidenceBatch{ID: "sources", At: now, Epoch: "fixture", Records: []storage.EvidenceRecord{timelineChangeRecord(change), timelineCrashRecord(crash)}, Issues: []storage.EvidenceIssue{timelineIssueObservation("pod", observedAt)}})
	query := TimelineQuery{Since: now.Add(-time.Hour), Until: now.Add(time.Second), Namespace: "shop", Limit: 1}
	before := reads.Load()
	first, err := service.Timeline(t.Context(), query)
	if err != nil || first.CountCapped || first.Total != 3 || first.CrashCount != 1 || first.Counts["break"] != 1 || first.Counts["change"] != 1 || first.Next == "" {
		t.Fatalf("first=%+v %v", first, err)
	}
	if reads.Load() != before {
		t.Fatal("first history page called the cluster")
	}
	backfill := change
	backfill.ID, backfill.At = "late-backfill", now.Add(-90*time.Second)
	commitTimelineHistory(t, store, scope, storage.EvidenceBatch{ID: "backfill", At: now.Add(time.Millisecond), CoveredThrough: now.Add(time.Millisecond), Epoch: "fixture", Records: []storage.EvidenceRecord{timelineChangeRecord(backfill)}})
	seedInventory(t, service, nil)
	replica := newTestService(t, service.client.base.String(), service.Scope())
	replica.SetChangeStorage(provider)
	replica.changes.evidenceEnabled = true
	before = reads.Load()
	items := append([]TimelineItem{}, first.Items...)
	query.Cursor = first.Next
	for {
		page, err := replica.Timeline(t.Context(), query)
		if err != nil || page.Total != 0 || page.CrashCount != 0 || page.CountCapped || page.Counts["break"] != 0 || page.Counts["change"] != 0 {
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
	types := map[string]int{}
	ids := map[string]bool{}
	for i, item := range items {
		types[item.Type]++
		if ids[item.ID] {
			t.Fatal("snapshot repeated an item across pages")
		}
		ids[item.ID] = true
		if item.ID == "change:late-backfill" || item.Type == "crash" && item.Crash.Type != "termination" {
			t.Fatal("snapshot admitted new/status row")
		}
		if item.Type == "break" && (item.Issue == nil || !item.Issue.FirstSeen.Equal(observedAt) || !item.Issue.LastSeen.Equal(observedAt) || item.Issue.ClosedAt != nil || item.Issue.State != "open") {
			t.Fatal("pinned break lost its retained observation sample")
		}
		if i > 0 && items[i-1].At.Before(item.At) {
			t.Fatal("page boundary is not globally chronological")
		}
	}
	if types["change"] != 1 || types["crash"] != 1 || types["break"] != 1 || !ids["change:old-change"] || !ids["crash:"+crash.ID] {
		t.Fatalf("snapshot lost source membership: %v", types)
	}
	freshQuery := query
	freshQuery.Cursor, freshQuery.Limit = "", 200
	fresh, err := replica.Timeline(t.Context(), freshQuery)
	if err != nil || fresh.Total != 5 || fresh.Counts["break"] != 2 || fresh.Counts["change"] != 2 {
		t.Fatalf("new first page did not observe committed backfill and recovery: %+v %v", fresh, err)
	}
	if reads.Load() != before {
		t.Fatal("replica continuation called the cluster")
	}
	query.Cursor = first.Next
	query.Namespace = "other"
	if _, err := replica.Timeline(t.Context(), query); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("changed filter accepted cursor: %v", err)
	}
	foreign := newTestService(t, service.client.base.String(), Scope{OrgID: "foreign", ClusterID: "cluster", CredentialID: "epoch"})
	foreign.SetChangeStorage(provider)
	foreign.changes.evidenceEnabled = true
	foreignScope := scope
	foreignScope.OrgID = "foreign"
	commitTimelineHistory(t, reviewProvedHistoryStore{EvidenceStore: provider.(storage.EvidenceStore), t: t, service: foreign}, foreignScope, storage.EvidenceBatch{ID: "foreign", At: now, Epoch: "fixture"})
	query.Namespace = "shop"
	if _, err := foreign.Timeline(t.Context(), query); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("foreign scope accepted cursor: %v", err)
	}
}

func TestTimelineStatusObservationsDoNotBecomeCrashMarkers(t *testing.T) {
	service, _, store, scope, reads := timelineHistoryFixture(t)
	now := time.Now().UTC()
	crash := kubechanges.CrashEvent{Cluster: "cluster", Namespace: "shop", Pod: "pod", PodUID: "pod", RootUID: "pod", RootKind: "Pod", RootName: "pod", Container: "app", Type: "termination", Reason: "Error", ExitCode: 1, RestartCount: 1, At: now.Add(-time.Minute), ObservedAt: now, TimeSource: "finished_at"}
	crash.ID = kubechanges.CrashID(crash)
	status := crash
	status.PodUID, status.RootUID, status.Type, status.Reason, status.ExitCode, status.TimeSource, status.At = "other", "other", "status", "", 0, "observed_at", now
	status.ID = kubechanges.CrashID(status)
	commitTimelineHistory(t, store, scope, storage.EvidenceBatch{ID: "crash-types", At: now, Epoch: "fixture", Records: []storage.EvidenceRecord{timelineCrashRecord(crash), timelineCrashRecord(status)}})
	before := reads.Load()
	page, err := service.Timeline(t.Context(), TimelineQuery{Since: now.Add(-time.Hour), Until: now, Namespace: "shop"})
	if err != nil || page.Total != 1 || page.CrashCount != 1 || len(page.Items) != 1 || page.Items[0].ID != "crash:"+crash.ID {
		t.Fatalf("status observation became a timeline marker: %+v %v", page, err)
	}
	if reads.Load() != before {
		t.Fatal("crash history called the cluster")
	}
}

func TestTimelineBreaksOnlyPagesAndCountsOnlyBreaks(t *testing.T) {
	service, _, store, scope, reads := timelineHistoryFixture(t)
	now := time.Now().UTC()
	observations := []storage.EvidenceIssue{}
	for _, name := range []string{"pod-a", "pod-b", "pod-c"} {
		observations = append(observations, timelineIssueObservation(name, now.Add(-30*time.Second)))
	}
	records := []storage.EvidenceRecord{}
	for index := 0; index < 4; index++ {
		records = append(records, timelineChangeRecord(Change{ID: "change-" + string(rune('a'+index)), Cluster: "cluster", UID: "healthy", RootUID: "healthy", Kind: "Pod", Name: "healthy", Namespace: "shop", Type: kubechanges.Created, At: now.Add(-time.Duration(index+1) * time.Minute)}))
	}
	crash := kubechanges.CrashEvent{Cluster: "cluster", Namespace: "shop", Pod: "pod-a", PodUID: "pod-a", RootUID: "pod-a", RootKind: "Pod", RootName: "pod-a", Container: "app", Type: "termination", Reason: "Error", ExitCode: 1, RestartCount: 1, At: now.Add(-time.Minute), ObservedAt: now, TimeSource: "finished_at"}
	crash.ID = kubechanges.CrashID(crash)
	records = append(records, timelineCrashRecord(crash))
	commitTimelineHistory(t, store, scope, storage.EvidenceBatch{ID: "breaks", At: now, Epoch: "fixture", Records: records, Issues: observations})
	before := reads.Load()
	all, err := service.Timeline(t.Context(), TimelineQuery{Since: now.Add(-time.Hour), Until: now.Add(time.Second), Namespace: "shop", Limit: 50})
	if err != nil || all.CountCapped || all.Counts["change"] != 4 || all.Counts["crash"] != 1 || all.Counts["break"] != 3 || all.BreaksOnly {
		t.Fatalf("unfiltered timeline=%+v %v", all, err)
	}
	query := TimelineQuery{Since: all.Since, Until: all.Until, Namespace: "shop", Limit: 2, BreaksOnly: true}
	first, err := service.Timeline(t.Context(), query)
	if err != nil || first.CountCapped || !first.BreaksOnly || first.Total != 3 || first.CrashCount != 0 || first.Counts["break"] != 3 || first.Counts["change"] != 0 || first.Counts["crash"] != 0 || len(first.Items) != 2 || first.Next == "" {
		t.Fatalf("breaks-only first page=%+v %v", first, err)
	}
	items := append([]TimelineItem{}, first.Items...)
	query.Cursor = first.Next
	second, err := service.Timeline(t.Context(), query)
	if err != nil || second.Total != 0 || second.Counts["break"] != 0 || second.CrashCount != 0 || second.Next != "" {
		t.Fatalf("breaks-only second page=%+v %v", second, err)
	}
	items = append(items, second.Items...)
	if len(items) != 3 {
		t.Fatalf("breaks-only paging returned %d items", len(items))
	}
	roots := map[string]bool{}
	for _, item := range items {
		if item.Type != "break" {
			t.Fatalf("breaks-only page carried a %s item", item.Type)
		}
		if item.Issue == nil || roots[item.Issue.Root.UID] {
			t.Fatal("break pages repeated or lost an occurrence")
		}
		roots[item.Issue.Root.UID] = true
	}
	if !roots["pod-a"] || !roots["pod-b"] || !roots["pod-c"] {
		t.Fatalf("break membership changed: %v", roots)
	}
	query.BreaksOnly = false
	if _, err := service.Timeline(t.Context(), query); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("breaks-only cursor was accepted for the unfiltered timeline: %v", err)
	}
	if reads.Load() != before {
		t.Fatal("break history called the cluster")
	}
}

func TestTimelineExactResourceAndPodFiltersKeepTruthfulTotals(t *testing.T) {
	service, _, store, scope, reads := timelineHistoryFixture(t)
	now := time.Now().UTC()
	records := []storage.EvidenceRecord{}
	for _, pod := range []string{"selected", "sibling"} {
		event := kubechanges.CrashEvent{Cluster: "cluster", Namespace: "shop", Pod: pod, PodUID: pod, RootUID: "workload", RootKind: "Deployment", RootName: "api", Container: "app", Type: "termination", Reason: "Error", ExitCode: 1, At: now, ObservedAt: now, TimeSource: "finished_at"}
		event.ID = kubechanges.CrashID(event)
		records = append(records, timelineCrashRecord(event))
	}
	change := Change{ID: "change", Cluster: "cluster", UID: "selected", RootUID: "workload", Kind: "Pod", Name: "selected", Namespace: "shop", Type: kubechanges.Created, At: now.Add(-time.Minute)}
	records = append(records, timelineChangeRecord(change))
	commitTimelineHistory(t, store, scope, storage.EvidenceBatch{ID: "filters", At: now, Epoch: "fixture", Records: records})
	before := reads.Load()
	for _, test := range []struct {
		name           string
		query          TimelineQuery
		total, crashes int
		ids            []string
	}{
		{"resource pod", TimelineQuery{ResourceUID: "selected"}, 2, 1, []string{records[0].ID, "change:change"}},
		{"resource workload", TimelineQuery{ResourceUID: "workload"}, 2, 2, []string{records[0].ID, records[1].ID}},
		{"pod", TimelineQuery{PodUID: "selected"}, 2, 1, []string{records[0].ID, "change:change"}},
		{"root", TimelineQuery{RootUID: "workload"}, 3, 2, []string{records[0].ID, records[1].ID, "change:change"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.query.Until = now
			page, err := service.Timeline(t.Context(), test.query)
			if err != nil || page.CountCapped || page.Total != test.total || page.CrashCount != test.crashes {
				t.Errorf("query=%+v page=%+v %v", test.query, page, err)
			}
			ids := map[string]bool{}
			for _, item := range page.Items {
				ids[item.ID] = true
			}
			if len(page.Items) != len(test.ids) || len(ids) != len(test.ids) {
				t.Errorf("filtered membership=%v want=%v", ids, test.ids)
			}
			for _, id := range test.ids {
				if !ids[id] {
					t.Errorf("filtered timeline lost %s", id)
				}
			}
		})
	}
	page, err := service.Crashes(t.Context(), kubechanges.CrashQuery{PodUID: "selected", Until: now, Limit: 1})
	if err != nil || page.CrashCount != 1 || len(page.Items) != 1 || page.Items[0].PodUID != "selected" {
		t.Fatalf("exact Pod crashes=%+v %v", page, err)
	}
	if reads.Load() != before {
		t.Fatal("filtered history called the cluster")
	}
}

func TestTimelineCountsAreCappedAndOnlyReturnedOnFirstPage(t *testing.T) {
	t.Parallel()
	service, _, store, scope, reads := timelineHistoryFixture(t)
	now := time.Now().UTC()
	records := make([]storage.EvidenceRecord, 0, 1002)
	for i := 0; i < 1002; i++ {
		records = append(records, timelineChangeRecord(Change{ID: "change-" + strconv.Itoa(i), Kind: "Pod", UID: "pod-" + strconv.Itoa(i), Type: kubechanges.Created, At: now.Add(-time.Duration(i) * time.Second)}))
	}
	commitTimelineHistory(t, store, scope, storage.EvidenceBatch{ID: "capped", At: now, Epoch: "fixture", Records: records})
	query := TimelineQuery{Since: now.Add(-time.Hour), Until: now, Limit: 200}
	before := reads.Load()
	first, err := service.Timeline(t.Context(), query)
	if err != nil || !first.CountCapped || first.Total != 1001 || first.Counts["change"] != 1001 || first.Next == "" {
		t.Fatalf("first-page lower-bound counts=%+v %v", first, err)
	}
	seen := map[string]bool{}
	page := first
	for {
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("capped count caused duplicate paging")
			}
			seen[item.ID] = true
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
		page, err = service.Timeline(t.Context(), query)
		if err != nil || page.CountCapped || page.Total != 0 || page.Counts["change"] != 0 {
			t.Fatalf("continuation repeated counts: %+v %v", page, err)
		}
	}
	if len(seen) != len(records) || reads.Load() != before {
		t.Fatalf("capped counts truncated history or called cluster: rows=%d reads=%d", len(seen), reads.Load()-before)
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
