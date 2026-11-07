package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Worst-case checks for list order, page totals and cursors. Each test states
// the contract it attacks; a failure here is a contract violation, not noise.

// pagingWindowCounter counts the bounded change-window reads that Issues
// enrichment issues, while the wrapped store keeps working.
type pagingWindowCounter struct {
	storage.Provider
	storage.EvidenceStore
	storage.BlobCAS
	windowReads atomic.Int64
}

func (store *pagingWindowCounter) QueryEvidence(ctx context.Context, scope storage.EvidenceScope, query storage.EvidenceQuery) (storage.EvidencePage, error) {
	if query.Type == "change" && query.Limit == 500 && query.Cursor == "" {
		store.windowReads.Add(1)
	}
	return store.EvidenceStore.QueryEvidence(ctx, scope, query)
}

func newPagingWindowCounter() *pagingWindowCounter {
	provider := storage.NewMemory()
	return &pagingWindowCounter{Provider: provider, EvidenceStore: provider.(storage.EvidenceStore), BlobCAS: provider.(storage.BlobCAS)}
}

func pagingChangeRecord(t *testing.T, id, namespace, root string, at, observed time.Time) storage.EvidenceRecord {
	t.Helper()
	change := Change{ID: id, Cluster: "cluster", Kind: "Deployment", Namespace: namespace, Name: root, UID: root, RootUID: root, Type: kubechanges.ImageChanged, At: at, Source: "reconstructed"}
	raw, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	return storage.EvidenceRecord{AuthorizationKind: "Deployment", ID: "change:" + id, Type: "change", Kind: "Deployment", Name: root, Namespace: namespace, UID: root, RootUID: root, Rule: string(change.Type), At: at, ObservedAt: observed, Epoch: "fixture", Payload: raw}
}

func pagingCommitHistory(t *testing.T, service *Service, id string, at time.Time, records []storage.EvidenceRecord) {
	t.Helper()
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	checkpoint, err := service.evidenceStore().EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: id, Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at, Epoch: "fixture", Records: records})
}

func pagingIssueKey(issue Issue) string {
	return issue.Root.Kind + "/" + issue.Root.Namespace + "/" + issue.Root.Name + "/" + issue.Rule
}

func failedPodOwnedBy(uid, ownerKind, ownerUID, ownerName string) kubeindex.Record {
	return kubeindex.Record{UID: uid, Kind: "Pod", Namespace: "shop", Name: uid, Phase: "Failed",
		Summary: map[string]string{"phase": "Failed"},
		Owners:  []kubeindex.OwnerRef{{UID: ownerUID, Kind: ownerKind, Name: ownerName, Namespace: "shop"}}}
}

// Two issue roots that share namespace, name and rule but differ in kind (a
// Deployment and a StatefulSet both named "web") tie on every sort key. The
// list order, and so the offset pages, must still be the same on every read.
func TestIssuesOrderIsDeterministicForSameNamedRootsOfDifferentKinds(t *testing.T) {
	t.Parallel()
	service := issueHistoryService(t, storage.NewMemory())
	seedInventory(t, service, []kubeindex.Record{
		healthyDeploymentRecord("deploy-web", "web"),
		{UID: "sts-web", Kind: "StatefulSet", Namespace: "shop", Name: "web", Replicas: 1, Available: 1,
			Summary: map[string]string{"desired_replicas": "1", "available_replicas": "1"}},
		failedPodOwnedBy("web-deploy-pod", "Deployment", "deploy-web", "web"),
		failedPodOwnedBy("web-0", "StatefulSet", "sts-web", "web"),
	})
	first := ""
	for read := 0; read < 40; read++ {
		page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		keys := []string{}
		for _, issue := range page.Items {
			keys = append(keys, pagingIssueKey(issue))
		}
		order := strings.Join(keys, ",")
		if read == 0 {
			first = order
			if len(keys) != 2 {
				t.Fatalf("expected two tied issues, got %v", keys)
			}
			continue
		}
		if order != first {
			t.Fatalf("read %d returned a different order for the same snapshot:\n first %s\n now   %s", read, first, order)
		}
	}
	// Offset paging over a nondeterministic order shows one row twice and
	// hides the other.
	for walk := 0; walk < 40; walk++ {
		one, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 1})
		if err != nil || one.Next == "" {
			t.Fatalf("page 1: next=%q err=%v", one.Next, err)
		}
		two, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 1, Cursor: one.Next})
		if err != nil || len(two.Items) != 1 {
			t.Fatalf("page 2: items=%d err=%v", len(two.Items), err)
		}
		if pagingIssueKey(one.Items[0]) == pagingIssueKey(two.Items[0]) {
			t.Fatalf("walk %d: page 1 and page 2 both show %s; the other tied issue was never shown", walk, pagingIssueKey(one.Items[0]))
		}
	}
}

// A snapshot list cursor used after the snapshot changed must be rejected or
// must not repeat a row the reader has already seen. Workloads binds its
// cursor to the committed rows; the other snapshot lists must be as safe.
func TestSnapshotListCursorAfterTheSnapshotChanged(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	t.Run("issues", func(t *testing.T) {
		t.Parallel()
		service := issueHistoryService(t, storage.NewMemory())
		records := []kubeindex.Record{}
		for position := 1; position <= 5; position++ {
			name := fmt.Sprintf("issue-%d", position)
			records = append(records, brokenDeploymentRecord(name, name, base.Add(-time.Duration(position)*time.Minute)))
		}
		seedInventory(t, service, records)
		one, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2})
		if err != nil || len(one.Items) != 2 || one.Total != 5 || one.Next == "" {
			t.Fatalf("page 1: items=%d total=%d next=%q err=%v", len(one.Items), one.Total, one.Next, err)
		}
		// A newer break appears before the reader asks for page 2.
		seedInventory(t, service, append([]kubeindex.Record{brokenDeploymentRecord("issue-0", "issue-0", base)}, records...))
		two, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: one.Next})
		if err != nil {
			// The new break sorts before the reader's position, so the
			// cursor must be refused as expired, not with any other error.
			if !errors.Is(err, ErrCursorExpired) {
				t.Fatalf("a stale Issues cursor answered %v, want expired", err)
			}
			return
		}
		seen := map[string]bool{}
		for _, issue := range one.Items {
			seen[issue.Root.Name] = true
		}
		for _, issue := range two.Items {
			if seen[issue.Root.Name] {
				t.Fatalf("page 2 repeated %s from page 1 after the snapshot changed (page 1 %v, page 2 %v, total %d -> %d)", issue.Root.Name, issueRootNames(one), issueRootNames(two), one.Total, two.Total)
			}
		}
	})
	t.Run("nodes", func(t *testing.T) {
		t.Parallel()
		service := issueHistoryService(t, storage.NewMemory())
		nodes := []kubeindex.Record{}
		for _, name := range []string{"node-b", "node-c", "node-d", "node-e"} {
			nodes = append(nodes, kubeindex.Record{UID: name, Kind: "Node", Name: name, NodeReady: true})
		}
		seedInventory(t, service, nodes)
		one, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 2})
		if err != nil || len(one.Items) != 2 || one.Continue == "" {
			t.Fatalf("page 1: items=%d continue=%q err=%v", len(one.Items), one.Continue, err)
		}
		seedInventory(t, service, append([]kubeindex.Record{{UID: "node-a", Kind: "Node", Name: "node-a", NodeReady: true}}, nodes...))
		two, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 2, Continue: one.Continue})
		if err != nil {
			if !errors.Is(err, ErrCursorExpired) {
				t.Fatalf("a stale Nodes cursor answered %v, want expired", err)
			}
			return
		}
		seen := map[string]bool{}
		for _, item := range one.Items {
			seen[item.Name] = true
		}
		for _, item := range two.Items {
			if seen[item.Name] {
				t.Fatalf("Nodes page 2 repeated %s from page 1 after the snapshot changed", item.Name)
			}
		}
	})
	t.Run("workloads", func(t *testing.T) {
		t.Parallel()
		service := issueHistoryService(t, storage.NewMemory())
		records := []kubeindex.Record{}
		for _, name := range []string{"w-b", "w-c", "w-d", "w-e"} {
			records = append(records, healthyDeploymentRecord(name, name))
		}
		seedInventory(t, service, records)
		one, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 2})
		if err != nil || one.Next == "" {
			t.Fatalf("page 1: next=%q err=%v", one.Next, err)
		}
		seedInventory(t, service, append([]kubeindex.Record{healthyDeploymentRecord("w-a", "w-a")}, records...))
		if _, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 2, Cursor: one.Next}); !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("a Workloads cursor over a replaced snapshot answered %v, want expired", err)
		}
	})
}

// Hostile paging parameters on the snapshot Issues list are rejected or
// normalized; none panics or returns rows outside the list.
func TestIssuesHostilePagingParameters(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	service := issueHistoryService(t, storage.NewMemory())
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:hostile-base", At: base, Epoch: "fixture"})
	records := []kubeindex.Record{}
	for position := 1; position <= 5; position++ {
		name := fmt.Sprintf("h-%d", position)
		records = append(records, brokenDeploymentRecord(name, name, base.Add(-time.Duration(position)*time.Minute)))
	}
	seedInventory(t, service, records)
	for _, limit := range []int{0, -1, -1 << 31, 1 << 40} {
		page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: limit})
		want := 50
		if limit > 0 {
			want = 200
		}
		if err != nil || page.Limit != want || page.Total != 5 || len(page.Items) != 5 || page.Next != "" {
			t.Fatalf("limit %d: limit=%d total=%d items=%d next=%q err=%v", limit, page.Limit, page.Total, len(page.Items), page.Next, err)
		}
	}
	// The last issued cursor serves the final partial page and ends the walk.
	cursor, walked := "", 0
	for pages := 0; pages < 3; pages++ {
		page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: cursor})
		if err != nil || page.Total != 5 {
			t.Fatalf("walk page %d: total=%d err=%v", pages+1, page.Total, err)
		}
		walked += len(page.Items)
		cursor = page.Next
	}
	if walked != 5 || cursor != "" {
		t.Fatalf("walk showed %d issues and ended with next=%q", walked, cursor)
	}
	// A bare offset names no rows. Even the offset at the end is refused as an
	// expired cursor, so the reader starts the list again.
	if _, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: "5"}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("a bare offset at the end answered %v, want an expired cursor", err)
	}
	for _, cursor := range []string{"6", "99999", strings.Repeat("9", 400), "-1", "+2", "2.0", "1e3", "abc", "%00", "eyJ", "eyJhIjoxfQ", "٣"} {
		page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: cursor})
		if err == nil {
			t.Fatalf("hostile cursor %q was served: items=%d", cursor, len(page.Items))
		}
		if !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("hostile cursor %q answered %v, want an invalid-arguments rejection", cursor, err)
		}
	}
}

// A history cursor is bound to its list, filters and cluster. Tampered,
// truncated or reused cursors are rejected and never return rows.
func TestHistoryCursorTamperingAndReuseAreRejected(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	at := now.Add(-20 * time.Minute)
	provider := storage.NewMemory()
	service := issueHistoryService(t, provider)
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:tamper-base", At: now, Epoch: "fixture"})
	records := []storage.EvidenceRecord{}
	for position := 0; position < 12; position++ {
		namespace := "shop"
		if position%2 == 1 {
			namespace = "ops"
		}
		records = append(records, pagingChangeRecord(t, fmt.Sprintf("tamper-%02d", position), namespace, "root-t", at.Add(time.Duration(position)*time.Second), now))
	}
	pagingCommitHistory(t, service, "capture:tamper", now, records)
	page, err := service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Limit: 2})
	if err != nil || page.Next == "" || len(page.Items) != 2 {
		t.Fatalf("page 1: items=%d next=%q err=%v", len(page.Items), page.Next, err)
	}
	cursor := page.Next
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	rewrite := func(key string, value any) string {
		copied := map[string]any{}
		for name, field := range fields {
			copied[name] = field
		}
		copied[key] = value
		encoded, _ := json.Marshal(copied)
		return base64.RawURLEncoding.EncodeToString(encoded)
	}
	flipped := []byte(cursor)
	flipped[len(flipped)/2] ^= 0x01
	hostile := map[string]string{
		"flipped":           string(flipped),
		"truncated":         cursor[:len(cursor)-7],
		"base64 garbage":    base64.RawURLEncoding.EncodeToString([]byte("\xff\xfe not json")),
		"standard base64":   strings.NewReplacer("-", "+", "_", "/").Replace(cursor) + "==",
		"empty json":        base64.RawURLEncoding.EncodeToString([]byte("{}")),
		"other binding":     rewrite("Binding", strings.Repeat("0", 64)),
		"other scope":       rewrite("CaptureScope", "kubernetes/captured-evidence/"+strings.Repeat("a", 64)),
		"null pins":         rewrite("Highs", nil),
		"overflowing pin":   rewrite("Highs", []any{json.Number("18446744073709551615")}),
		"negative pin":      rewrite("Highs", []any{-1}),
		"zero time":         rewrite("At", time.Time{}),
		"oversized":         strings.Repeat("A", 5000),
		"snapshot offset":   "2",
		"occurrence layout": rewrite("Ordering", "first_seen"),
	}
	for name, value := range hostile {
		next, err := service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Limit: 2, Cursor: value})
		if err == nil {
			// The ordering tag is not read by a change list, so that cursor
			// still names the same position and the same rows.
			if name == "occurrence layout" {
				continue
			}
			t.Fatalf("%s cursor was served: items=%d", name, len(next.Items))
		}
		if !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("%s cursor answered %v, want an invalid-arguments rejection", name, err)
		}
	}
	reuse := map[string]func(string) error{
		"other namespace": func(value string) error {
			_, err := service.Changes(t.Context(), ChangeQuery{Namespace: "ops", Limit: 2, Cursor: value})
			return err
		},
		"all namespaces": func(value string) error {
			_, err := service.Changes(t.Context(), ChangeQuery{Limit: 2, Cursor: value})
			return err
		},
		"other kind": func(value string) error {
			_, err := service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Kind: "Pod", Limit: 2, Cursor: value})
			return err
		},
		"other window": func(value string) error {
			_, err := service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Since: at.Add(-time.Hour), Until: now, Limit: 2, Cursor: value})
			return err
		},
		"crashes list": func(value string) error {
			_, err := service.Crashes(t.Context(), kubechanges.CrashQuery{Namespace: "shop", Limit: 2, Cursor: value})
			return err
		},
		"timeline list": func(value string) error {
			_, err := service.Timeline(t.Context(), TimelineQuery{Namespace: "shop", Limit: 2, Cursor: value})
			return err
		},
		"history issues": func(value string) error {
			_, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: value})
			return err
		},
	}
	for name, read := range reuse {
		if err := read(cursor); !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("cursor reused on %s answered %v, want an invalid-arguments rejection", name, err)
		}
	}
	// The same storage seen from another cluster's service: the cursor names
	// a different capture scope and must not reach its rows.
	other := newTestService(t, inventoryProjectionEndpoint(t), Scope{OrgID: "org", ClusterID: "other"})
	other.SetChangeStorage(provider)
	commitInventoryFixtureHistory(t, other, storage.EvidenceBatch{ID: "capture:other-base", At: now, Epoch: "fixture"})
	if leaked, err := other.Changes(t.Context(), ChangeQuery{Namespace: "shop", Limit: 2, Cursor: cursor}); err == nil {
		t.Fatalf("another cluster served this cluster's cursor: items=%d", len(leaked.Items))
	}
	// The issued cursor still works after all of the above.
	if resumed, err := service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Limit: 2, Cursor: cursor}); err != nil || len(resumed.Items) != 2 {
		t.Fatalf("the issued cursor stopped working: items=%d err=%v", len(resumed.Items), err)
	}
}

// Thousands of breaks at the same instant share one bounded change read per
// request, keep a stable order across pages and report the same total on
// every page.
func TestIssuesThousandsOfSameInstantBreaksShareOneRead(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 2,000-issue snapshot")
	}
	t.Parallel()
	brokeAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	store := newPagingWindowCounter()
	service := issueHistoryService(t, store)
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:instant-base", At: time.Now().UTC(), Epoch: "fixture"})
	records := make([]kubeindex.Record, 0, 2000)
	for position := 0; position < 2000; position++ {
		name := fmt.Sprintf("same-%04d", position)
		records = append(records, brokenDeploymentRecord(name, name, brokeAt))
	}
	seedInventory(t, service, records)
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		before := store.windowReads.Load()
		page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true, IncludeChanges: true, Limit: 200, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if reads := store.windowReads.Load() - before; reads != 1 {
			t.Fatalf("page %d issued %d change-window reads for same-instant breaks, want 1", pages, reads)
		}
		if page.Total != 2000 || page.Limit != 200 {
			t.Fatalf("page %d: total=%d limit=%d", pages, page.Total, page.Limit)
		}
		requireNoPageHistoryPartial(t, page)
		previous := ""
		for _, issue := range page.Items {
			if seen[issue.Root.Name] {
				t.Fatalf("page %d repeated %s", pages, issue.Root.Name)
			}
			seen[issue.Root.Name] = true
			if issue.HistoryTruncated {
				t.Fatalf("%s marked truncated by an empty window", issue.Root.Name)
			}
			if previous != "" && issue.Root.Name < previous {
				t.Fatalf("tied breaks are not in name order: %s after %s", issue.Root.Name, previous)
			}
			previous = issue.Root.Name
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if pages != 10 || len(seen) != 2000 {
		t.Fatalf("walked %d pages and %d issues, want 10 and 2000", pages, len(seen))
	}
}

// Break times in the future, far in the past and at the same instant sort
// newest first and enrich without a page-level failure.
func TestIssuesExtremeBreakTimesOrderAndEnrich(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	service := issueHistoryService(t, storage.NewMemory())
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:extreme-base", At: now, Epoch: "fixture"})
	seedInventory(t, service, []kubeindex.Record{
		brokenDeploymentRecord("ancient", "ancient", time.Date(1971, 1, 1, 0, 0, 0, 0, time.UTC)),
		brokenDeploymentRecord("future", "future", now.Add(90*time.Minute)),
		brokenDeploymentRecord("recent", "recent", now.Add(-time.Minute)),
	})
	page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true, IncludeChanges: true, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	requireNoPageHistoryPartial(t, page)
	names := issueRootNames(page)
	for position := 1; position < len(page.Items); position++ {
		left, right := page.Items[position-1].BrokeAt, page.Items[position].BrokeAt
		if left == nil || right == nil || left.Before(*right) {
			t.Fatalf("issues are not newest break first: %v", names)
		}
	}
	if len(names) != 3 {
		t.Fatalf("issues = %v", names)
	}
}

// An issue's change window is the issue's own. Changes in another namespace
// that fall in the same 32 minutes must not cut the window and hide the
// issue's own probable cause; the issue must read the same whether the list
// is filtered to its namespace or shows every namespace.
func TestIssuesWindowIsNotCutByAnotherNamespacesChanges(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	brokeAt := now.Add(-6 * time.Hour).Truncate(time.Second)
	service := issueHistoryService(t, storage.NewMemory())
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:noise-base", At: now, Epoch: "fixture"})
	records := []storage.EvidenceRecord{pagingChangeRecord(t, "pay-cause", "pay", "pay-api", brokeAt.Add(-10*time.Minute), now)}
	for position := 0; position < 501; position++ {
		records = append(records, pagingChangeRecord(t, fmt.Sprintf("ci-noise-%03d", position), "ci", "ci-runner", brokeAt.Add(-time.Minute), now))
	}
	pagingCommitHistory(t, service, "capture:noise", now, records)
	payBroken := brokenDeploymentRecord("pay-api", "pay-api", brokeAt)
	payBroken.Namespace = "pay"
	seedInventory(t, service, []kubeindex.Record{payBroken, {UID: "ci-runner", Kind: "Deployment", Namespace: "ci", Name: "ci-runner", Replicas: 1, Available: 1, Summary: map[string]string{"desired_replicas": "1", "available_replicas": "1"}}})
	read := func(namespace string) Issue {
		t.Helper()
		page, err := service.Issues(t.Context(), IssueOptions{Namespace: namespace, Broken: true, IncludeChanges: true, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		return issueNamed(t, page, "pay-api")
	}
	causes := func(issue Issue) []string {
		ids := []string{}
		for _, linked := range issue.Changes {
			ids = append(ids, linked.Change.ID)
		}
		return ids
	}
	scoped, all := read("pay"), read("")
	if scoped.HistoryTruncated || len(causes(scoped)) == 0 {
		t.Fatalf("namespace view: truncated=%t causes=%v", scoped.HistoryTruncated, causes(scoped))
	}
	if all.HistoryTruncated != scoped.HistoryTruncated || strings.Join(causes(all), ",") != strings.Join(causes(scoped), ",") {
		t.Fatalf("the same issue reads differently by list filter: namespace view truncated=%t causes=%v; all-namespaces view truncated=%t causes=%v",
			scoped.HistoryTruncated, causes(scoped), all.HistoryTruncated, causes(all))
	}
}

// Severity filters: wrong case and unknown values are rejected on both the
// current and the history read; a filter with no matches is an empty, exact
// list.
func TestIssuesSeverityFilterEdgeCases(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	service := issueHistoryService(t, storage.NewMemory())
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:severity-base", At: now, Epoch: "fixture"})
	seedInventory(t, service, []kubeindex.Record{brokenDeploymentRecord("crit", "crit", now.Add(-time.Minute))})
	for _, severity := range []string{"Critical", "CRITICAL", "fatal", " critical", "critical ", "warning,info"} {
		if _, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Severity: severity}); !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("current read accepted severity %q: %v", severity, err)
		}
		if _, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Severity: severity, Since: now.Add(-time.Hour), Until: now}); !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("history read accepted severity %q: %v", severity, err)
		}
	}
	empty, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Severity: "info", Limit: 2})
	if err != nil || len(empty.Items) != 0 || empty.Total != 0 || empty.Next != "" || empty.Limit != 2 {
		t.Fatalf("no-match filter: items=%d total=%d next=%q limit=%d err=%v", len(empty.Items), empty.Total, empty.Next, empty.Limit, err)
	}
	for severity, count := range empty.Totals {
		if count != 0 {
			t.Fatalf("a severity-filtered read counted another severity: %s=%d", severity, count)
		}
	}
}

// Permissions between pages: when Nodes become forbidden after page 1, page 2
// drops node issues and recounts, never shows a Node field; when every kind
// is forbidden the read is refused.
func TestIssuesPermissionChangesBetweenPages(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	service := issueHistoryService(t, storage.NewMemory())
	records := []kubeindex.Record{
		{UID: "node-x", Kind: "Node", Name: "node-x", NodeReady: false},
		{UID: "pod-x", Kind: "Pod", Namespace: "shop", Name: "pod-x", NodeName: "node-x", Phase: "Failed", Summary: map[string]string{"phase": "Failed"}},
	}
	for position := 1; position <= 3; position++ {
		name := fmt.Sprintf("perm-%d", position)
		records = append(records, brokenDeploymentRecord(name, name, now.Add(-time.Duration(position)*time.Minute)))
	}
	seedInventory(t, service, records)
	one, err := service.Issues(t.Context(), IssueOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	cache := service.changes.inventories[service.indexScope()]
	cache.generation.View.Kinds["Node"] = kubeindex.KindStatus{State: "error", Error: "forbidden", Partial: true, FailingSince: now}
	cursor := one.Next
	for cursor != "" {
		page, err := service.Issues(t.Context(), IssueOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			if errors.Is(err, ErrInvalidArguments) {
				break // a rejected stale cursor is safe
			}
			t.Fatal(err)
		}
		for _, issue := range page.Items {
			raw, _ := json.Marshal(issue)
			if issue.Root.Kind == "Node" || strings.HasPrefix(issue.Rule, "node.") || strings.Contains(string(raw), "node-x") {
				t.Fatalf("a page read after Nodes were denied exposed the Node: %s", raw)
			}
		}
		cursor = page.Next
	}
	for kind := range cache.generation.View.Kinds {
		cache.generation.View.Kinds[kind] = kubeindex.KindStatus{State: "error", Error: "forbidden", Partial: true, FailingSince: now}
	}
	if page, err := service.Issues(t.Context(), IssueOptions{Limit: 2}); err == nil {
		t.Fatalf("an all-forbidden snapshot answered %d issues", len(page.Items))
	} else if !errors.Is(err, ErrForbidden) {
		t.Fatalf("an all-forbidden snapshot answered %v, want forbidden", err)
	}
}

// History totals must describe the rows a reader can page through. Coverage
// gaps share the keyset with changes; the first page's total and the pages it
// implies must agree with what the walk returns.
func TestHistoryTotalsMatchTheRowsThePagesShow(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	at := now.Add(-40 * time.Minute)
	service := issueHistoryService(t, storage.NewMemory())
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:gaps-base", At: now, Epoch: "fixture"})
	records := []storage.EvidenceRecord{}
	for position := 0; position < 6; position++ {
		records = append(records, pagingChangeRecord(t, fmt.Sprintf("gapped-%d", position), "shop", "root-g", at.Add(time.Duration(position)*time.Minute), now))
		from := at.Add(time.Duration(position)*time.Minute + 20*time.Second)
		gap, _ := json.Marshal(kubechanges.Gap{From: from, To: from.Add(10 * time.Second)})
		records = append(records, storage.EvidenceRecord{ID: fmt.Sprintf("gap:%d", position), Type: "coverage_gap", Namespace: "shop", At: from, ObservedAt: now, GapFrom: from, GapTo: from.Add(10 * time.Second), Epoch: "fixture", Payload: gap})
	}
	pagingCommitHistory(t, service, "capture:gaps", now, records)
	for name, walk := range map[string]func(cursor string) (total, items int, next string, err error){
		"changes": func(cursor string) (int, int, string, error) {
			page, err := service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Since: at.Add(-time.Minute), Until: now, Limit: 3, Cursor: cursor})
			if cursor != "" {
				page, err = service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Limit: 3, Cursor: cursor})
			}
			return page.Total, len(page.Items), page.Next, err
		},
		"timeline": func(cursor string) (int, int, string, error) {
			page, err := service.Timeline(t.Context(), TimelineQuery{Namespace: "shop", Since: at.Add(-time.Minute), Until: now, Limit: 3, Cursor: cursor})
			if cursor != "" {
				page, err = service.Timeline(t.Context(), TimelineQuery{Namespace: "shop", Limit: 3, Cursor: cursor})
			}
			return page.Total, len(page.Items), page.Next, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			total, shown, pages := 0, 0, 0
			cursor := ""
			perPage := []int{}
			for {
				pageTotal, items, next, err := walk(cursor)
				if err != nil {
					t.Fatalf("page %d: %v", pages+1, err)
				}
				if pages == 0 {
					total = pageTotal
				}
				pages++
				shown += items
				perPage = append(perPage, items)
				if next == "" {
					break
				}
				cursor = next
			}
			wantPages := (total + 2) / 3
			if shown != total || pages != wantPages {
				t.Fatalf("first-page total %d implies %d pages of 3; the walk showed %d rows over %d pages %v", total, wantPages, shown, pages, perPage)
			}
		})
	}
}
