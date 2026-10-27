package kubernetes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func TestInventoryRejectsNonProgressingAndOversizedCursors(t *testing.T) {
	for _, test := range []struct {
		name  string
		token string
		pages int32
	}{
		{name: "repeated", token: "same-page", pages: 2},
		{name: "oversized", token: strings.Repeat("x", 4097), pages: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var pages atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages.Add(1)
				writeJSON(w, map[string]any{
					"items":    []any{},
					"metadata": map[string]string{"continue": test.token},
				})
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			ctx = context.WithValue(ctx, discoveryContextKey{}, Discovery{Resources: []ResourceDefinition{
				{ID: indexedResourceIDs["Pod"], Kind: "Pod", Version: "v1", Resource: "pods", Namespaced: true, Available: true},
			}})
			_, complete, err := service.loadIndexKind(ctx, "Pod")
			if err == nil || complete {
				t.Errorf("invalid continuation must fail closed: complete=%v err=%v", complete, err)
			}
			if ctx.Err() != nil {
				t.Error("invalid continuation stopped only at the caller deadline")
			}
			if got := pages.Load(); got != test.pages {
				t.Errorf("read %d pages; invalid continuation must stop after %d", got, test.pages)
			}
		})
	}
}

func TestRelatedSharedSecretExcludesTenOtherWorkloads(t *testing.T) {
	records := []kubeindex.Record{
		{Kind: "Deployment", UID: "selected", Namespace: "shop", Name: "selected"},
		{Kind: "ReplicaSet", UID: "selected-rs", Namespace: "shop", Name: "selected-rs", Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: "selected"}}},
		{Kind: "Pod", UID: "selected-pod", Namespace: "shop", Name: "selected-pod", Owners: []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "selected-rs"}}, References: map[string][]string{"secrets": {"shared"}}},
		{Kind: "Secret", UID: "shared", Namespace: "shop", Name: "shared"},
	}
	for i := 0; i < 10; i++ {
		uid := "other-" + strconv.Itoa(i)
		records = append(records,
			kubeindex.Record{Kind: "Deployment", UID: uid, Namespace: "shop", Name: uid},
			kubeindex.Record{Kind: "Pod", UID: uid + "-pod", Namespace: "shop", Name: uid + "-pod", Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: uid}}, References: map[string][]string{"secrets": {"shared"}}},
		)
	}
	graph := directedRelated(kubeindex.Snapshot{Records: records}, "Deployment", "shop", "selected")
	got := make(map[string]bool)
	for _, node := range graph.Nodes {
		got[node.ID] = true
	}
	for _, uid := range []string{"selected", "selected-rs", "selected-pod", "shared"} {
		if !got[uid] {
			t.Errorf("missing selected workload relation %s", uid)
		}
	}
	if len(got) != 4 || graph.Truncated || graph.Next != "" {
		t.Errorf("shared Secret expanded unrelated workloads or capped results: nodes=%d truncated=%v next=%q", len(got), graph.Truncated, graph.Next)
	}
}
