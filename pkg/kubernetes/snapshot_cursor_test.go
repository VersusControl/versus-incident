package kubernetes

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// A snapshot cursor survives a republished snapshot with the same rows, is
// refused as expired when the rows, the filters or the list differ, and is
// refused as an invalid argument, not as expired, when it does not decode.
func TestSnapshotListCursorsExpireOnlyWhenTheirPageChanged(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	service := issueHistoryService(t, storage.NewMemory())
	records := []kubeindex.Record{}
	for position := 1; position <= 4; position++ {
		name := fmt.Sprintf("cursor-%d", position)
		records = append(records, brokenDeploymentRecord(name, name, base.Add(-time.Duration(position)*time.Minute)))
	}
	seedInventory(t, service, records)
	one, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2})
	if err != nil || one.Next == "" {
		t.Fatalf("page 1: next=%q err=%v", one.Next, err)
	}
	// The same rows published again, as every inventory cycle does.
	seedInventory(t, service, records)
	two, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: one.Next})
	if err != nil || len(two.Items) != 2 || two.Items[0].Root.Name != "cursor-3" {
		t.Fatalf("a republished, unchanged snapshot expired the cursor: names=%v err=%v", issueRootNames(two), err)
	}
	for name, options := range map[string]IssueOptions{
		"other severity": {Namespace: "shop", Severity: "critical", Limit: 2, Cursor: one.Next},
		"broken only":    {Namespace: "shop", Broken: true, Limit: 2, Cursor: one.Next},
		"all namespaces": {Limit: 2, Cursor: one.Next},
	} {
		if _, err := service.Issues(t.Context(), options); !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("%s: a cursor bound to other filters answered %v, want expired", name, err)
		}
	}
	workloads, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 2})
	if err != nil || workloads.Next == "" {
		t.Fatalf("workloads page 1: next=%q err=%v", workloads.Next, err)
	}
	if _, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: workloads.Next}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("a Workloads cursor read as an Issues cursor answered %v, want expired", err)
	}
	digest := strings.Repeat("a", 64)
	for _, malformed := range []string{
		base64.RawURLEncoding.EncodeToString([]byte(digest + ":-1")),
		base64.RawURLEncoding.EncodeToString([]byte(digest + ":01")),
		base64.RawURLEncoding.EncodeToString([]byte(strings.ToUpper(digest) + ":1")),
		base64.RawURLEncoding.EncodeToString([]byte(digest[:63] + ":1")),
		one.Next + "x",
	} {
		_, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 2, Cursor: malformed})
		if !errors.Is(err, ErrInvalidArguments) || errors.Is(err, ErrCursorExpired) {
			t.Fatalf("malformed cursor %q answered %v, want a plain invalid argument", malformed, err)
		}
	}
	// A new row before the reader's position expires every list's cursor.
	nodes := []kubeindex.Record{{UID: "node-b", Kind: "Node", Name: "node-b", NodeReady: true}, {UID: "node-c", Kind: "Node", Name: "node-c", NodeReady: true}}
	seedInventory(t, service, append(append([]kubeindex.Record{}, records...), nodes...))
	nodePage, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 1})
	if err != nil || nodePage.Continue == "" {
		t.Fatalf("nodes page 1: continue=%q err=%v", nodePage.Continue, err)
	}
	issuePage, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2})
	if err != nil || issuePage.Next == "" {
		t.Fatalf("issues page 1: next=%q err=%v", issuePage.Next, err)
	}
	newer := brokenDeploymentRecord("cursor-0", "cursor-0", base)
	seedInventory(t, service, append(append([]kubeindex.Record{newer, {UID: "node-a", Kind: "Node", Name: "node-a", NodeReady: true}}, records...), nodes...))
	if _, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 1, Continue: nodePage.Continue}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("nodes cursor over a changed snapshot answered %v, want expired", err)
	}
	if _, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: issuePage.Next}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("issues cursor over a changed snapshot answered %v, want expired", err)
	}
}

// A caller that shows only some rows of a page (the assistant tools trim pages
// to a byte budget) resumes the same list right after the rows it showed.
func TestSnapshotPageResumesAfterTheRowsACallerShowed(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	service := issueHistoryService(t, storage.NewMemory())
	records := []kubeindex.Record{}
	for position := 1; position <= 5; position++ {
		name := fmt.Sprintf("resume-%d", position)
		records = append(records, brokenDeploymentRecord(name, name, base.Add(-time.Duration(position)*time.Minute)))
	}
	seedInventory(t, service, records)
	all, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 5})
	if err != nil || len(all.Items) != 5 {
		t.Fatalf("full list: items=%d err=%v", len(all.Items), err)
	}
	one, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 3})
	if err != nil || len(one.Items) != 3 {
		t.Fatalf("page 1: items=%d err=%v", len(one.Items), err)
	}
	resumed, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 3, Cursor: one.ResumeAfter(1)})
	if err != nil || len(resumed.Items) != 3 || resumed.Items[0].Root.Name != all.Items[1].Root.Name {
		t.Fatalf("resume after one shown row: names=%v err=%v, want to start at %s", issueRootNames(resumed), err, all.Items[1].Root.Name)
	}
	workloads, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 3})
	if err != nil || len(workloads.Items) != 3 {
		t.Fatalf("workloads page 1: items=%d err=%v", len(workloads.Items), err)
	}
	rest, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 3, Cursor: workloads.ResumeAfter(2)})
	if err != nil || len(rest.Items) == 0 || rest.Items[0].UID != workloads.Items[2].UID {
		t.Fatalf("workloads resume after two shown rows: err=%v", err)
	}
	if (IssuePage{}).ResumeAfter(1) != "" {
		t.Fatal("a page that was not served from a snapshot minted a cursor")
	}
}

// A cursor binds only the rows the reader has already been shown. Rows added
// after the reader's position keep the cursor valid and the next page repeats
// nothing; a row inserted or removed before the position expires it.
func TestSnapshotCursorBindsOnlyTheRowsBeforeItsPosition(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	nodeRecord := func(name string) kubeindex.Record {
		return kubeindex.Record{UID: name, Kind: "Node", Name: name, NodeReady: true}
	}
	nodeNames := func(page ResourcePage) []string {
		names := []string{}
		for _, item := range page.Items {
			names = append(names, item.Name)
		}
		return names
	}
	nodes := func(names ...string) []kubeindex.Record {
		records := []kubeindex.Record{}
		for _, name := range names {
			records = append(records, nodeRecord(name))
		}
		return records
	}
	firstNodePage := func(t *testing.T, service *Service) ResourcePage {
		t.Helper()
		page, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 2})
		if err != nil || len(page.Items) != 2 || page.Continue == "" {
			t.Fatalf("nodes page 1: names=%v continue=%q err=%v", nodeNames(page), page.Continue, err)
		}
		return page
	}
	t.Run("nodes appended after the position", func(t *testing.T) {
		t.Parallel()
		service := issueHistoryService(t, storage.NewMemory())
		seedInventory(t, service, nodes("node-b", "node-c", "node-d"))
		one := firstNodePage(t, service)
		seedInventory(t, service, nodes("node-b", "node-c", "node-d", "node-e", "node-f"))
		two, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 2, Continue: one.Continue})
		if err != nil {
			t.Fatalf("a row added after the position expired the cursor: %v", err)
		}
		if got := strings.Join(nodeNames(two), ","); got != "node-d,node-e" || two.Total != 5 {
			t.Fatalf("page 2 = %s (total %d), want node-d,node-e of 5", got, two.Total)
		}
		three, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 2, Continue: two.Continue})
		if err != nil || strings.Join(nodeNames(three), ",") != "node-f" || three.Continue != "" {
			t.Fatalf("page 3 = %v continue=%q err=%v, want node-f and the end of the list", nodeNames(three), three.Continue, err)
		}
	})
	t.Run("node inserted before the position", func(t *testing.T) {
		t.Parallel()
		service := issueHistoryService(t, storage.NewMemory())
		seedInventory(t, service, nodes("node-b", "node-d", "node-e"))
		one := firstNodePage(t, service)
		seedInventory(t, service, nodes("node-b", "node-c", "node-d", "node-e"))
		if _, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 2, Continue: one.Continue}); !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("a row inserted before the position answered %v, want expired", err)
		}
	})
	t.Run("node removed before the position", func(t *testing.T) {
		t.Parallel()
		service := issueHistoryService(t, storage.NewMemory())
		seedInventory(t, service, nodes("node-b", "node-c", "node-d", "node-e"))
		one := firstNodePage(t, service)
		seedInventory(t, service, nodes("node-c", "node-d", "node-e"))
		if _, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 2, Continue: one.Continue}); !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("a row removed before the position answered %v, want expired", err)
		}
		seedInventory(t, service, nodes("node-b"))
		if _, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Node"], Limit: 2, Continue: one.Continue}); !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("a list now shorter than the position answered %v, want expired", err)
		}
	})
	t.Run("workloads", func(t *testing.T) {
		t.Parallel()
		service := issueHistoryService(t, storage.NewMemory())
		workloads := func(names ...string) []kubeindex.Record {
			records := []kubeindex.Record{}
			for _, name := range names {
				records = append(records, healthyDeploymentRecord(name, name))
			}
			return records
		}
		seedInventory(t, service, workloads("w-b", "w-c", "w-d"))
		one, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 2})
		if err != nil || one.Next == "" {
			t.Fatalf("workloads page 1: next=%q err=%v", one.Next, err)
		}
		seedInventory(t, service, workloads("w-b", "w-c", "w-d", "w-e"))
		two, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 2, Cursor: one.Next})
		if err != nil || len(two.Items) != 2 || two.Items[0].Name != "w-d" || two.Items[1].Name != "w-e" {
			t.Fatalf("workloads appended after the position: items=%v err=%v, want w-d,w-e", two.Items, err)
		}
		seedInventory(t, service, workloads("w-a", "w-b", "w-c", "w-d", "w-e"))
		if _, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop", Limit: 2, Cursor: one.Next}); !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("a workload inserted before the position answered %v, want expired", err)
		}
	})
	t.Run("issues", func(t *testing.T) {
		t.Parallel()
		service := issueHistoryService(t, storage.NewMemory())
		records := []kubeindex.Record{}
		for position := 1; position <= 3; position++ {
			name := fmt.Sprintf("append-%d", position)
			records = append(records, brokenDeploymentRecord(name, name, base.Add(-time.Duration(position)*time.Minute)))
		}
		seedInventory(t, service, records)
		one, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2})
		if err != nil || one.Next == "" {
			t.Fatalf("issues page 1: next=%q err=%v", one.Next, err)
		}
		// An older break sorts after every row the reader has seen.
		older := brokenDeploymentRecord("append-9", "append-9", base.Add(-time.Hour))
		seedInventory(t, service, append(append([]kubeindex.Record{}, records...), older))
		two, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 2, Cursor: one.Next})
		if err != nil {
			t.Fatalf("an issue added after the position expired the cursor: %v", err)
		}
		seen := map[string]bool{}
		for _, name := range issueRootNames(one) {
			seen[name] = true
		}
		for _, name := range issueRootNames(two) {
			if seen[name] {
				t.Fatalf("page 2 repeated %s from page 1 (page 1 %v, page 2 %v)", name, issueRootNames(one), issueRootNames(two))
			}
		}
		if got := strings.Join(issueRootNames(two), ","); got != "append-3,append-9" {
			t.Fatalf("page 2 = %s, want append-3,append-9", got)
		}
	})
}
