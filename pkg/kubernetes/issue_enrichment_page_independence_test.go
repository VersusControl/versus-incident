package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// historyReadFaultStore lets a test fail the history read while the rest of the
// evidence store keeps working. Embedding the interfaces keeps the provider's
// advertised method set (Provider, EvidenceStore, BlobCAS) intact for the
// service's type assertions.
type historyReadFaultStore struct {
	storage.Provider
	storage.EvidenceStore
	storage.BlobCAS
	failReads atomic.Bool
}

func (store *historyReadFaultStore) QueryEvidence(ctx context.Context, scope storage.EvidenceScope, query storage.EvidenceQuery) (storage.EvidencePage, error) {
	if store.failReads.Load() && query.Type == "change" {
		return storage.EvidencePage{}, errors.New("synthetic history read failure")
	}
	return store.EvidenceStore.QueryEvidence(ctx, scope, query)
}

func issueHistoryService(t *testing.T, provider storage.Provider) *Service {
	t.Helper()
	service := newTestService(t, inventoryProjectionEndpoint(t), Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetChangeStorage(provider)
	return service
}

// commitCappedChangeWindow establishes history authority and stores 501 changes in
// one break window. Every enriched issue that reads this window hits the
// 500-change page cap.
func commitCappedChangeWindow(t *testing.T, service *Service, brokeAt time.Time) {
	t.Helper()
	now := time.Now().UTC()
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:fixture-base", At: now, Epoch: "fixture"})
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	checkpoint, err := service.evidenceStore().EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]storage.EvidenceRecord, 0, 501)
	for position := 0; position < 501; position++ {
		change := Change{
			ID:        fmt.Sprintf("cap-%03d", position),
			Cluster:   "cluster",
			Kind:      "Deployment",
			Namespace: "shop",
			Name:      "target",
			UID:       "target",
			RootUID:   "target",
			Type:      kubechanges.ImageChanged,
			At:        brokeAt.Add(-time.Minute),
			Source:    "reconstructed",
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
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:fixture-cap", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: now, Epoch: "fixture", Records: records})
}

func brokenDeploymentRecord(uid, name string, brokeAt time.Time) kubeindex.Record {
	return kubeindex.Record{
		UID: uid, Kind: "Deployment", Namespace: "shop", Name: name,
		Replicas: 1, Available: 0,
		AvailableStatus:       "False",
		AvailableTransitionAt: brokeAt,
		Summary:               map[string]string{"desired_replicas": "1", "available_replicas": "0"},
	}
}

func issueNamed(t *testing.T, page IssuePage, name string) Issue {
	t.Helper()
	for _, issue := range page.Items {
		if issue.Root.Name == name {
			return issue
		}
	}
	t.Fatalf("issue %q absent from %d items", name, len(page.Items))
	return Issue{}
}

func requireNoPageHistoryPartial(t *testing.T, page IssuePage) {
	t.Helper()
	if page.Sync.Partial {
		t.Fatal("a bounded per-issue history read marked the page partial")
	}
	for _, partial := range page.Partial {
		if partial.ResourceID == "kubernetes.history" {
			t.Fatalf("a per-issue history cap leaked into a page-level partial: class=%s", partial.Class)
		}
	}
}

// the same issue is enriched identically whether it lands on page 1 or
// page 2. Its >500-change window marks only that issue; the page stays clean.
func TestIssuesEnrichmentIsPageIndependent(t *testing.T) {
	now := time.Now().UTC()
	brokeAt := now.Add(-6 * time.Hour)
	provider := storage.NewMemory()
	service := issueHistoryService(t, provider)
	commitCappedChangeWindow(t, service, brokeAt)

	newer := func(count int) []kubeindex.Record {
		records := make([]kubeindex.Record, 0, count+1)
		for position := 1; position <= count; position++ {
			uid := fmt.Sprintf("newer-%d", position)
			records = append(records, brokenDeploymentRecord(uid, uid, brokeAt.Add(time.Duration(position)*time.Hour)))
		}
		return append(records, brokenDeploymentRecord("target", "target", brokeAt))
	}

	// Five issues: the target is the fifth, so it is enriched from page 1.
	seedInventory(t, service, newer(4))
	page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true, IncludeChanges: true, Limit: 5})
	if err != nil || len(page.Items) != 5 || page.Next != "" {
		t.Fatalf("single-page issues = %+v err=%v", page, err)
	}
	requireNoPageHistoryPartial(t, page)
	first := issueNamed(t, page, "target")
	if !first.Partial || !first.HistoryTruncated {
		t.Fatalf("page-1 target did not warn about its capped window: partial=%t truncated=%t", first.Partial, first.HistoryTruncated)
	}
	// The other enriched issues read their own, disjoint windows and must not
	// inherit the target's cap.
	for _, issue := range page.Items {
		if issue.Root.Name != "target" && (issue.Partial || issue.HistoryTruncated) {
			t.Fatalf("issue %q inherited another issue's capped window: partial=%t truncated=%t", issue.Root.Name, issue.Partial, issue.HistoryTruncated)
		}
	}

	// Six issues: the same target is the oldest item, now on page 2.
	seedInventory(t, service, newer(5))
	page1, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true, IncludeChanges: true, Limit: 5})
	if err != nil || len(page1.Items) != 5 || page1.Next == "" {
		t.Fatalf("first paged issues = %+v err=%v", page1, err)
	}
	requireNoPageHistoryPartial(t, page1)
	for _, issue := range page1.Items {
		if issue.Root.Name == "target" {
			t.Fatal("fixture did not move the target to page 2")
		}
		if issue.Partial || issue.HistoryTruncated {
			t.Fatalf("unread window %q was marked truncated", issue.Root.Name)
		}
	}
	page2, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true, IncludeChanges: true, Limit: 5, Cursor: page1.Next})
	if err != nil || len(page2.Items) != 1 {
		t.Fatalf("second paged issues = %+v err=%v", page2, err)
	}
	requireNoPageHistoryPartial(t, page2)
	second := issueNamed(t, page2, "target")

	if first.Partial != second.Partial || first.HistoryTruncated != second.HistoryTruncated {
		t.Fatalf("partial flags changed across pages: page1 partial=%t truncated=%t page2 partial=%t truncated=%t", first.Partial, first.HistoryTruncated, second.Partial, second.HistoryTruncated)
	}
	if !reflect.DeepEqual(first.Changes, second.Changes) {
		t.Fatalf("same issue got different changes on page 1 and page 2: page1=%d page2=%d", len(first.Changes), len(second.Changes))
	}
	if len(second.Changes) == 0 {
		t.Fatal("capped window returned no linked change, so page independence was not exercised")
	}
}

// a failed history read still makes the page partial, unlike a cap.
func TestIssuesHistoryReadErrorMarksOnlyThePagePartial(t *testing.T) {
	brokeAt := time.Now().UTC().Add(-time.Hour)
	memory := storage.NewMemory()
	fault := &historyReadFaultStore{Provider: memory, EvidenceStore: memory.(storage.EvidenceStore), BlobCAS: memory.(storage.BlobCAS)}
	service := issueHistoryService(t, fault)
	commitCappedChangeWindow(t, service, brokeAt)
	seedInventory(t, service, []kubeindex.Record{brokenDeploymentRecord("target", "target", brokeAt)})

	fault.failReads.Store(true)
	page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true, IncludeChanges: true})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("issues with failed history read = %+v err=%v", page, err)
	}
	if !page.Sync.Partial {
		t.Fatal("a failed history read did not mark the page partial")
	}
	found := false
	for _, partial := range page.Partial {
		found = found || partial.ResourceID == "kubernetes.history"
	}
	if !found {
		t.Fatalf("failed history read was not reported as a page-level partial: %+v", page.Partial)
	}
	// A read error is not a cap: the issue is partial because the page is, but
	// it must not claim that some changes were left unloaded.
	target := issueNamed(t, page, "target")
	if !target.Partial || target.HistoryTruncated {
		t.Fatalf("read error flags on the issue: partial=%t truncated=%t", target.Partial, target.HistoryTruncated)
	}
}

// An issue whose own correlation window is small must be enriched the same
// whether or not an older issue with a capped window is enriched beside it.
// The two windows overlap by one minute, so they are read together when both
// issues are on the page.
func TestIssuesEnrichmentIgnoresANeighboursCappedWindow(t *testing.T) {
	now := time.Now().UTC()
	brokeAt := now.Add(-6 * time.Hour)
	neighbourBrokeAt := brokeAt.Add(31 * time.Minute)
	provider := storage.NewMemory()
	service := issueHistoryService(t, provider)
	commitCappedChangeWindow(t, service, brokeAt)

	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	checkpoint, err := service.evidenceStore().EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	change := Change{
		ID: "neighbour-image", Cluster: "cluster", Kind: "Deployment", Namespace: "shop",
		Name: "neighbour", UID: "neighbour", RootUID: "neighbour",
		Type: kubechanges.ImageChanged, At: neighbourBrokeAt.Add(-time.Minute), Source: "reconstructed",
	}
	raw, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{
		ID: "capture:fixture-neighbour", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: now, Epoch: "fixture",
		Records: []storage.EvidenceRecord{{
			AuthorizationKind: "Deployment", ID: "change:" + change.ID, Type: "change",
			Kind: change.Kind, Name: change.Name, Namespace: change.Namespace, UID: change.UID,
			RootUID: change.RootUID, Rule: string(change.Type), At: change.At, ObservedAt: now,
			Epoch: "fixture", Payload: raw,
		}},
	})
	seedInventory(t, service, []kubeindex.Record{
		brokenDeploymentRecord("target", "target", brokeAt),
		brokenDeploymentRecord("neighbour", "neighbour", neighbourBrokeAt),
	})

	// Limit 1: the newer neighbour is alone on page 1 and reads only its window.
	alonePage, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true, IncludeChanges: true, Limit: 1})
	if err != nil || len(alonePage.Items) != 1 || alonePage.Items[0].Root.Name != "neighbour" {
		t.Fatalf("neighbour-only page = %+v err=%v", alonePage, err)
	}
	alone := alonePage.Items[0]
	if alone.HistoryTruncated || len(alone.Changes) == 0 {
		t.Fatalf("fixture: neighbour alone truncated=%t changes=%d, want an untruncated linked change", alone.HistoryTruncated, len(alone.Changes))
	}

	// Limit 5: both issues are enriched on the same page.
	sharedPage, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true, IncludeChanges: true, Limit: 5})
	if err != nil || len(sharedPage.Items) != 2 {
		t.Fatalf("shared page = %+v err=%v", sharedPage, err)
	}
	requireNoPageHistoryPartial(t, sharedPage)
	if target := issueNamed(t, sharedPage, "target"); !target.HistoryTruncated {
		t.Fatal("fixture: target window was not capped")
	}
	shared := issueNamed(t, sharedPage, "neighbour")
	if shared.Partial != alone.Partial || shared.HistoryTruncated != alone.HistoryTruncated {
		t.Fatalf("neighbour warning depends on the page: alone partial=%t truncated=%t changes=%d, beside capped issue partial=%t truncated=%t changes=%d", alone.Partial, alone.HistoryTruncated, len(alone.Changes), shared.Partial, shared.HistoryTruncated, len(shared.Changes))
	}
	if !reflect.DeepEqual(alone.Changes, shared.Changes) {
		t.Fatalf("neighbour changes depend on the page: alone=%d beside capped issue=%d", len(alone.Changes), len(shared.Changes))
	}
}

// Without the evidence store, Issues reads the local change store. A
// cluster-scoped root still reads its own window there and sees only changes
// of its own kind; a namespaced root keeps its own changes and its Pods' Node
// changes exactly once.
func TestIssueChangesFromTheLocalChangeStoreIncludeClusterScopedRoots(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	brokeAt := now.Add(-10 * time.Minute)
	service := newTestService(t, inventoryProjectionEndpoint(t), Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	if service.evidenceStore() != nil || service.changeStore() == nil {
		t.Fatal("the fixture must read the local change store")
	}
	node := kubeindex.Record{UID: "node-a", Kind: "Node", Name: "node-a", ReadyStatus: "False", ReadyTransitionAt: brokeAt}
	deployment := brokenDeploymentRecord("deploy-web", "web", brokeAt)
	pod := kubeindex.Record{UID: "web-pod", Kind: "Pod", Namespace: "shop", Name: "web-pod", NodeName: "node-a", Phase: "Running",
		Owners: []kubeindex.OwnerRef{{UID: "deploy-web", Kind: "Deployment", Name: "web", Namespace: "shop"}}}
	seedInventory(t, service, []kubeindex.Record{node, deployment, pod})
	coverage := map[string]kubechanges.Gap{}
	for kind := range captureCadences {
		coverage[kind] = kubechanges.Gap{From: now.Add(-2 * time.Hour), To: now.Add(time.Hour)}
	}
	changed := brokeAt.Add(-5 * time.Minute)
	changes := []Change{
		{ID: "node-change", Cluster: "cluster", Kind: "Node", Name: "node-a", UID: "node-a", RootUID: "node-a", Type: kubechanges.SpecChanged, At: changed, Source: "observed"},
		{ID: "deploy-change", Cluster: "cluster", Kind: "Deployment", Namespace: "shop", Name: "web", UID: "deploy-web", RootUID: "deploy-web", Type: kubechanges.ImageChanged, At: changed, Source: "observed"},
	}
	if err := service.changeStore().Commit(changes, nil, coverage, "", now); err != nil {
		t.Fatal(err)
	}
	linked := func(issue Issue) map[string]int {
		ids := map[string]int{}
		for _, change := range issue.Changes {
			ids[change.Change.ID]++
		}
		return ids
	}
	for _, read := range []struct {
		root, want, absent string
	}{
		{root: "node-a", want: "node-change", absent: "deploy-change"},
		{root: "deploy-web", want: "deploy-change"},
	} {
		page, err := service.Issues(t.Context(), IssueOptions{Broken: true, IncludeChanges: true, RootUID: read.root})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("%s: items=%d err=%v", read.root, len(page.Items), err)
		}
		ids := linked(page.Items[0])
		if ids[read.want] != 1 {
			t.Fatalf("%s: linked changes %v, want %s once", read.root, ids, read.want)
		}
		if read.absent != "" && ids[read.absent] != 0 {
			t.Fatalf("%s: linked changes %v include %s from outside its own window", read.root, ids, read.absent)
		}
	}
	page, err := service.Issues(t.Context(), IssueOptions{Broken: true, IncludeChanges: true, Limit: 10})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("both roots: items=%d err=%v", len(page.Items), err)
	}
	for _, issue := range page.Items {
		for id, count := range linked(issue) {
			if count != 1 {
				t.Fatalf("%s linked %s %d times", issue.Root.Name, id, count)
			}
		}
		if issue.Root.Kind == "Node" && linked(issue)["node-change"] != 1 {
			t.Fatalf("the Node issue lost its change when listed beside a namespaced issue: %v", linked(issue))
		}
	}
}
