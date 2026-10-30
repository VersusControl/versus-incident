package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// This matrix separates a selected-namespace proof's transport/envelope from
// the same journal's all-namespace query. No live cluster objects are fixtures.
func TestEvidenceAuthorizationQueryScopeMatrix(t *testing.T) {
	for _, shape := range []string{"complete-empty", "complete-object", "complete-null", "complete-null-all-scopes", "null-items-unproven", "wrong-negotiated-kind", "forbidden", "unserved"} {
		t.Run(shape, func(t *testing.T) {
			var mu sync.Mutex
			requests := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.URL.Path)
				mu.Unlock()
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "scope-physical"}})
					return
				}
				metadata := strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList")
				selected := r.URL.Path == "/api/v1/namespaces/sample/pods"
				if selected && shape == "forbidden" {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				if selected && shape == "unserved" {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				kind, version := "PodList", "v1"
				if metadata {
					kind, version = "PartialObjectMetadataList", "meta.k8s.io/v1"
				}
				listMeta := map[string]any{"resourceVersion": "42"}
				var items any = []any{}
				emptyWorkload := strings.HasSuffix(r.URL.Path, "/statefulsets") || strings.HasSuffix(r.URL.Path, "/daemonsets")
				if emptyWorkload && (shape == "complete-null-all-scopes" || shape == "complete-null" && strings.Contains(r.URL.Path, "/namespaces/sample/")) {
					items = nil
				}
				if selected {
					switch shape {
					case "complete-object":
						items = []any{map[string]any{"kind": "PartialObjectMetadata", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{"name": "synthetic", "namespace": "sample", "uid": "synthetic-pod", "resourceVersion": "42"}}}
					case "null-items-unproven":
						items = nil
						listMeta = map[string]any{}
					case "wrong-negotiated-kind":
						kind, version = "PodList", "v1"
					}
				}
				writeJSON(w, map[string]any{"kind": kind, "apiVersion": version, "metadata": listMeta, "items": items})
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "scope-matrix"})
			provider := storage.NewMemory()
			service.SetChangeStorage(provider)
			service.changes.evidenceEnabled = true
			scope := storage.EvidenceScope{OrgID: "org", ClusterID: "scope-matrix", ClusterUID: "scope-physical"}
			now := time.Now().UTC()
			batch := storage.EvidenceBatch{ID: "capture:scope-matrix", At: now, Epoch: "synthetic", Coverage: map[string]storage.EvidenceInterval{
				"Pod": {From: now.Add(-time.Minute), To: now}, "Namespace": {From: now.Add(-time.Minute), To: now}, "Node": {From: now.Add(-time.Minute), To: now},
			}}
			if err := provider.(storage.EvidenceStore).CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
			for _, namespace := range []string{"", "sample"} {
				for _, kind := range []string{"timeline", "change", "crash", "occurrence"} {
					ctx, cancel := context.WithCancel(t.Context())
					page, err := service.queryEvidence(ctx, storage.EvidenceQuery{Namespace: namespace, Type: kind, Since: now.Add(-time.Hour), Until: now.Add(time.Minute), Limit: 20})
					cancel()
					var want error
					if namespace != "" {
						switch shape {
						case "null-items-unproven", "wrong-negotiated-kind":
							want = ErrIncompleteInventory
						case "forbidden":
							want = ErrForbidden
						case "unserved":
							want = ErrNotFound
						}
					}
					if (want == nil && err != nil) || (want != nil && !errors.Is(err, want)) || len(page.Items) != 0 {
						t.Fatalf("scope=%q type=%s shape=%s: items=%d error=%v want=%v", namespace, kind, shape, len(page.Items), err, want)
					}
					if err != nil {
						t.Logf("closed synthetic discriminator scopeSelected=%t type=%s shape=%s code=%s", namespace != "", kind, shape, DiagnoseError(err).Code)
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for _, path := range requests {
				if path == "/api/v1/namespaces/sample/nodes" || path == "/api/v1/namespaces/sample/namespaces" {
					t.Fatalf("cluster-scoped checkpoint kind escaped into a namespaced proof: %s", path)
				}
			}
		})
	}
}

func TestNegotiatedMetadataNullRequiresCompleteListMetadata(t *testing.T) {
	meta := func(m map[string]any) map[string]any { return m["metadata"].(map[string]any) }
	cases := []struct {
		name     string
		mutate   func(map[string]any)
		ordinary bool
		valid    bool
	}{
		{name: "complete-null", valid: true},
		{name: "complete-null-empty-continue", mutate: func(m map[string]any) { meta(m)["continue"] = "" }, valid: true},
		{name: "complete-null-zero-remaining", mutate: func(m map[string]any) { meta(m)["remainingItemCount"] = 0 }, valid: true},
		{name: "ordinary-null", ordinary: true},
		{name: "missing-items", mutate: func(m map[string]any) { delete(m, "items") }},
		{name: "wrong-items", mutate: func(m map[string]any) { m["items"] = map[string]any{} }},
		{name: "missing-kind", mutate: func(m map[string]any) { delete(m, "kind") }},
		{name: "wrong-kind", mutate: func(m map[string]any) { m["kind"] = "StatefulSetList" }},
		{name: "missing-version", mutate: func(m map[string]any) { delete(m, "apiVersion") }},
		{name: "wrong-version", mutate: func(m map[string]any) { m["apiVersion"] = "v1" }},
		{name: "missing-list-meta", mutate: func(m map[string]any) { delete(m, "metadata") }},
		{name: "null-list-meta", mutate: func(m map[string]any) { m["metadata"] = nil }},
		{name: "wrong-list-meta", mutate: func(m map[string]any) { m["metadata"] = []any{} }},
		{name: "missing-resource-version", mutate: func(m map[string]any) { delete(meta(m), "resourceVersion") }},
		{name: "empty-resource-version", mutate: func(m map[string]any) { meta(m)["resourceVersion"] = "" }},
		{name: "null-resource-version", mutate: func(m map[string]any) { meta(m)["resourceVersion"] = nil }},
		{name: "numeric-resource-version", mutate: func(m map[string]any) { meta(m)["resourceVersion"] = 42 }},
		{name: "more-pages", mutate: func(m map[string]any) { meta(m)["continue"] = "next" }},
		{name: "null-continue", mutate: func(m map[string]any) { meta(m)["continue"] = nil }},
		{name: "numeric-continue", mutate: func(m map[string]any) { meta(m)["continue"] = 2 }},
		{name: "positive-remaining", mutate: func(m map[string]any) { meta(m)["remainingItemCount"] = 1 }},
		{name: "negative-remaining", mutate: func(m map[string]any) { meta(m)["remainingItemCount"] = -1 }},
		{name: "null-remaining", mutate: func(m map[string]any) { meta(m)["remainingItemCount"] = nil }},
		{name: "wrong-remaining", mutate: func(m map[string]any) { meta(m)["remainingItemCount"] = "0" }},
		{name: "existing-array-contract", mutate: func(m map[string]any) { m["items"] = []any{}; delete(m, "metadata") }, valid: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{"resourceVersion": "opaque-version"}, "items": nil}
			if test.mutate != nil {
				test.mutate(body)
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			err = validateInventoryListEnvelope(raw, !test.ordinary)
			if (err == nil) != test.valid {
				t.Fatalf("qualified metadata empty validation: err=%v wantValid=%t", err, test.valid)
			}
			if err != nil && !errors.Is(err, ErrIncompleteInventory) {
				t.Fatalf("invalid envelope lost its typed integrity cause: %v", err)
			}
		})
	}
}

func TestPublicEvidenceViewsWithNegotiatedEmptyWorkloads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "public-physical"}})
			return
		}
		metadata := strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList")
		kind, version := "List", "v1"
		var items any = []any{}
		if metadata {
			kind, version = "PartialObjectMetadataList", "meta.k8s.io/v1"
			if strings.HasSuffix(r.URL.Path, "/statefulsets") || strings.HasSuffix(r.URL.Path, "/daemonsets") {
				items = nil
			}
		}
		writeJSON(w, map[string]any{"kind": kind, "apiVersion": version, "metadata": map[string]any{"resourceVersion": "opaque-version"}, "items": items})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "public-empty"})
	service.SetChangeStorage(storage.NewMemory())
	service.changes.evidenceEnabled = true
	seedInventory(t, service, []kubeindex.Record{
		{Kind: "Namespace", UID: "public-physical", Name: "kube-system"},
		{Kind: "Pod", UID: "broken-pod", Name: "synthetic", Namespace: "sample", CreatedAt: time.Now().Add(-time.Hour), Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 3}}},
	})
	cache := service.changes.inventories[service.indexScope()]
	generation := cache.generation
	legacy := syntheticCustomOwnerCrash(t, "public-empty", time.Now().UTC().Add(-time.Minute), 1)
	var crash kubechanges.CrashEvent
	if err := json.Unmarshal(legacy.Payload, &crash); err != nil {
		t.Fatal(err)
	}
	generation.Crashes = []kubechanges.CrashEvent{crash}
	if err := service.prepareEvidence(t.Context(), &generation, inventoryGeneration{}); err != nil {
		t.Fatal(err)
	}
	for _, issue := range generation.EvidenceBatch.Issues {
		if strings.HasPrefix(issue.Rule, "pod.") && issue.AuthorizationKind != "Pod" || strings.HasPrefix(issue.Rule, "event.") && issue.AuthorizationKind != "Event" {
			t.Fatal("issue capture persisted display-owner authority instead of its observation source")
		}
	}
	if err := service.commitEvidence(t.Context(), generation); err != nil {
		t.Fatal(err)
	}
	stored, err := service.evidenceStore().QueryEvidence(t.Context(), generation.EvidenceScope, storage.EvidenceQuery{Type: "crash", Since: crash.At.Add(-time.Minute), Until: time.Now().UTC()})
	if err != nil || len(stored.Items) != 1 || stored.Items[0].AuthorizationKind != "Pod" || containsString(stored.Checkpoint.Kinds, "FluentBit") {
		t.Fatal("new crash capture confused display-owner kind with Pod authorization")
	}
	cache.generation = generation
	for _, namespace := range []string{"", "sample"} {
		plain, err := service.Issues(t.Context(), IssueOptions{Namespace: namespace, Broken: true})
		if err != nil || len(plain.Items) == 0 {
			t.Fatalf("plain current issues scopeSelected=%t: count=%d err=%v", namespace != "", len(plain.Items), err)
		}
		enriched, err := service.Issues(t.Context(), IssueOptions{Namespace: namespace, Broken: true, IncludeChanges: true})
		if err != nil || len(enriched.Items) != len(plain.Items) {
			t.Fatalf("enriched current issues scopeSelected=%t: count=%d err=%v", namespace != "", len(enriched.Items), err)
		}
		for _, gap := range enriched.Partial {
			if gap.ResourceID == "kubernetes.history" {
				t.Fatalf("legitimate metadata empty made issue enrichment unavailable: class=%s", gap.Class)
			}
		}
		now := time.Now().UTC()
		for _, breaks := range []bool{false, true} {
			page, err := service.Timeline(t.Context(), TimelineQuery{Namespace: namespace, Since: now.Add(-2 * time.Hour), Until: now, BreaksOnly: breaks})
			if err != nil || len(page.Items) == 0 {
				t.Fatalf("timeline scopeSelected=%t breaksOnly=%t: count=%d err=%v", namespace != "", breaks, len(page.Items), err)
			}
		}
		if _, err := service.Crashes(t.Context(), kubechanges.CrashQuery{Namespace: namespace, Since: now.Add(-time.Hour), Until: now}); err != nil {
			t.Fatalf("genuine empty crashes: %v", err)
		}
		if _, err := service.Changes(t.Context(), ChangeQuery{Namespace: namespace, Since: now.Add(-time.Hour), Until: now}); err != nil {
			t.Fatalf("changes: %v", err)
		}
		historical, err := service.Issues(t.Context(), IssueOptions{Namespace: namespace, Since: now.Add(-time.Hour), Until: now})
		if err != nil || len(historical.Items) == 0 {
			t.Fatalf("current-authorized occurrence history: count=%d err=%v", len(historical.Items), err)
		}
	}
	overview, err := service.Overview(t.Context())
	if err != nil || overview.Pods != 1 {
		t.Fatalf("overview current inventory corrupted by empty unrelated workloads: pods=%d err=%v", overview.Pods, err)
	}
}
