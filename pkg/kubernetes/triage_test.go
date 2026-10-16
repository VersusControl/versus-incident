package kubernetes

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestClassifyIssueRootsPodFindingAtOwner(t *testing.T) {
	findings := classifyIssue(ProjectedResource{
		Kind: "Pod", Namespace: "checkout", Name: "api-0", UID: "pod-uid",
		Owners:  []ObjectRef{{Kind: "Deployment", Name: "api", UID: "deployment-uid"}},
		Summary: map[string]any{"phase": "Failed"},
	})
	if len(findings) != 1 || findings[0].rule != "pod.failed" || findings[0].severity != "critical" || findings[0].root.Kind != "Deployment" || findings[0].root.Namespace != "checkout" || findings[0].example.Name != "api-0" {
		t.Fatalf("findings = %#v", findings)
	}
}

func TestClassifyIssueWarningEventUsesInvolvedObject(t *testing.T) {
	findings := classifyIssue(ProjectedResource{
		Kind: "Event", Namespace: "checkout", Name: "api-warning",
		Summary: map[string]any{
			"type": "Warning", "count": float64(3),
			"involved_object": ObjectRef{Kind: "Pod", Name: "api-0", UID: "pod-uid"},
		},
	})
	if len(findings) != 1 || findings[0].rule != "event.warning" || findings[0].count != 3 || findings[0].root.Kind != "Pod" || findings[0].root.Name != "api-0" || findings[0].root.Namespace != "checkout" {
		t.Fatalf("findings = %#v", findings)
	}
}

func TestClassifyIssueUnavailableWorkloadCountsReplicas(t *testing.T) {
	findings := classifyIssue(ProjectedResource{
		Kind: "Deployment", Namespace: "checkout", Name: "api",
		Summary: map[string]any{"replicas": float64(5), "availableReplicas": float64(2)},
	})
	if len(findings) != 1 || findings[0].rule != "workload.unavailable" || findings[0].count != 3 || findings[0].root.Name != "api" {
		t.Fatalf("findings = %#v", findings)
	}
}

func TestClassifyIssueCountBounds(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "negative", value: "-1", want: 0},
		{name: "zero", value: "0", want: 0},
		{name: "positive", value: "3", want: 3},
		{name: "platform maximum", value: strconv.FormatInt(int64(math.MaxInt), 10), want: math.MaxInt},
		{name: "signed maximum", value: "9223372036854775807", want: math.MaxInt},
	}
	if strconv.IntSize == 32 {
		tests = append(tests, struct {
			name  string
			value string
			want  int
		}{name: "above platform maximum", value: "2147483648", want: math.MaxInt})
	}
	for _, kind := range []string{"Job", "Event"} {
		for _, test := range tests {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				value, err := strconv.ParseInt(test.value, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				summary := map[string]any{"failed": value}
				want := test.want
				if kind == "Event" {
					summary = map[string]any{"type": "Warning", "count": value}
					if want == 0 {
						want = 1
					}
				}
				findings := classifyIssue(ProjectedResource{Kind: kind, Name: "bounded", Summary: summary})
				if want == 0 {
					if len(findings) != 0 {
						t.Fatalf("unexpected findings: %#v", findings)
					}
					return
				}
				if len(findings) != 1 || findings[0].count != want {
					t.Fatalf("findings = %#v, want count %d", findings, want)
				}
			})
		}
	}
}

func TestClassifyIssueReplicaCountBounds(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet"} {
		for _, test := range []struct {
			name      string
			desired   int64
			available int64
			want      int
		}{
			{name: "large deficit", desired: math.MaxInt64, available: 0, want: math.MaxInt},
			{name: "small deficit at maximum", desired: math.MaxInt64, available: math.MaxInt64 - 1, want: 1},
			{name: "negative available", desired: 5, available: -1},
			{name: "subtraction overflow", desired: math.MaxInt64, available: math.MinInt64},
			{name: "negative desired", desired: -1, available: math.MinInt64},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				desiredKey := "desired_replicas"
				if kind == "DaemonSet" {
					desiredKey = "desiredNumberScheduled"
				}
				findings := classifyIssue(ProjectedResource{Kind: kind, Name: "bounded", Summary: map[string]any{desiredKey: test.desired, "availableReplicas": test.available}})
				if test.want == 0 {
					if len(findings) != 0 {
						t.Fatalf("unexpected findings: %#v", findings)
					}
					return
				}
				if len(findings) != 1 || findings[0].count != test.want {
					t.Fatalf("findings = %#v, want count %d", findings, test.want)
				}
			})
		}
	}
}

func TestClassifyIssueUsesDesiredReplicasNotStatusReplicas(t *testing.T) {
	summary := safeSummary("Deployment", map[string]any{
		"spec":   map[string]any{"replicas": float64(5)},
		"status": map[string]any{"replicas": float64(4), "availableReplicas": float64(2)},
	}, nil)
	if summary["desired_replicas"] != float64(5) {
		t.Fatalf("summary = %#v", summary)
	}
	findings := classifyIssue(ProjectedResource{Kind: "Deployment", Name: "api", Summary: summary})
	if len(findings) != 1 || findings[0].count != 3 {
		t.Fatalf("findings = %#v", findings)
	}
	if got := classifyIssue(ProjectedResource{Kind: "Deployment", Name: "starting", Summary: map[string]any{"desired_replicas": float64(5)}}); len(got) != 0 {
		t.Fatalf("incomplete status findings = %#v", got)
	}
}

func TestIssuesCursorRoundTripReturnsStableNonNullItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/shop/pods", "/api/v1/pods":
			writeJSON(writer, map[string]any{"items": []any{
				map[string]any{"kind": "Pod", "metadata": map[string]any{"uid": "pod-a", "name": "api-a", "namespace": "shop"}, "status": map[string]any{"phase": "Failed"}},
				map[string]any{"kind": "Pod", "metadata": map[string]any{"uid": "pod-b", "name": "api-b", "namespace": "shop"}, "status": map[string]any{"phase": "Failed"}},
			}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 1})
	if err != nil || page.Items == nil || len(page.Items) != 1 || page.Items[0].Root.Name != "api-a" || page.Next != "1" || !page.Truncated {
		t.Fatalf("first issue page = %#v err=%v", page, err)
	}
	page, err = service.Issues(t.Context(), IssueOptions{Namespace: "shop", Limit: 1, Cursor: page.Next})
	if err != nil || page.Items == nil || len(page.Items) != 1 || page.Items[0].Root.Name != "api-b" || page.Next != "" {
		t.Fatalf("second issue page = %#v err=%v", page, err)
	}
}

func TestIssuesNamespaceOnlyReadScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/pods":
			http.Error(writer, "forbidden cluster list", http.StatusForbidden)
		case "/api/v1/namespaces/shop/pods":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "api", "namespace": "shop", "uid": "pod"}, "status": map[string]any{"phase": "Failed"}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	page, err := service.Issues(context.Background(), IssueOptions{Namespace: "shop", Broken: true})
	if err != nil || len(page.Items) != 1 || page.Items[0].Root.Name != "api" || page.Broken.Total != 1 || !page.Sync.Partial {
		t.Fatalf("namespace-only findings: %+v %v", page, err)
	}
}

func TestIssuesEvidencePartialIsRootLocalAndTotalsRemainKnown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("fresh seeded index unexpectedly read upstream: %s", request.URL.Path)
		http.NotFound(writer, request)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	now := time.Now().UTC()
	statuses := []any{}
	for position := range 9 {
		state := map[string]any{}
		if position == 8 {
			state["waiting"] = map[string]any{"reason": "CrashLoopBackOff", "message": "CANARY_MESSAGE"}
		}
		statuses = append(statuses, map[string]any{"name": "app-" + strconv.Itoa(position), "state": state})
	}
	projected := service.projectResource(ResourceDefinition{Kind: "Pod"}, map[string]any{"metadata": map[string]any{"name": "wide", "namespace": "shop", "uid": "wide"}, "status": map[string]any{"phase": "Running", "containerStatuses": statuses}})
	wide := indexRecord(projected)
	wide.Owners = []kubeindex.OwnerRef{{UID: "wide-root", Name: "wide-root", Kind: "Deployment", Namespace: "shop"}}
	if len(wide.Containers) != 8 || wide.Containers[0].WaitingReason != "CrashLoopBackOff" || wide.Summary["break_evidence_partial"] != "true" {
		t.Fatalf("ninth container failure lost: %+v", wide)
	}
	root := kubeindex.Record{UID: "wide-root", Name: "wide-root", Kind: "Deployment", Namespace: "shop", Replicas: 1, Available: 1, Summary: map[string]string{"available_replicas": "1"}}
	otherRoot := root
	otherRoot.UID, otherRoot.Name, otherRoot.Namespace = "other-root", "other-root", "payments"
	otherRoot.Available, otherRoot.Summary = 0, map[string]string{"available_replicas": "0"}
	otherPod := kubeindex.Record{UID: "other-pod", Name: "other-pod", Kind: "Pod", Namespace: "payments", Phase: "Failed", Owners: []kubeindex.OwnerRef{{UID: "other-root", Name: "other-root", Kind: "Deployment", Namespace: "payments"}}}
	thirdRoot := root
	thirdRoot.UID, thirdRoot.Name = "third-root", "third-root"
	thirdRoot.Available, thirdRoot.Summary = 0, map[string]string{"available_replicas": "0"}
	thirdPod := otherPod
	thirdPod.UID, thirdPod.Name, thirdPod.Namespace = "third-pod", "third-pod", "shop"
	thirdPod.Owners = []kubeindex.OwnerRef{{UID: "third-root", Name: "third-root", Kind: "Deployment", Namespace: "shop"}}
	records := []kubeindex.Record{root, otherRoot, thirdRoot, wide, otherPod, thirdPod}
	index, err := service.indexes.ForScope(service.indexScope())
	if err != nil {
		t.Fatal(err)
	}
	for kind := range indexedResourceIDs {
		items := []kubeindex.Record{}
		for _, record := range records {
			if record.Kind == kind {
				items = append(items, record)
			}
		}
		if err := index.Ingest(kind, items, true, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, namespace := range []string{"", "shop", "payments"} {
		page, err := service.Issues(t.Context(), IssueOptions{Broken: true, Namespace: namespace, Limit: 1})
		want := 3
		switch namespace {
		case "shop":
			want = 2
		case "payments":
			want = 1
		}
		if err != nil || page.Sync.Partial || page.Broken.Total != want || len(page.Items) != 1 || page.Items[0].Partial != (page.Items[0].Root.UID == "wide-root") {
			t.Fatalf("scope %q: root-local partial/counting: %+v %v", namespace, page, err)
		}
	}
	coverage := map[string]kubechanges.Gap{}
	for kind := range captureCadences {
		coverage[kind] = kubechanges.Gap{From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	}
	if err := service.changeStore().Commit(nil, nil, coverage, "", now); err != nil {
		t.Fatal(err)
	}
	thirdRoot.References = map[string][]string{"persistent_volume_claims": {"data"}, "service_accounts": {"runner"}}
	if err := index.Ingest("Deployment", []kubeindex.Record{root, otherRoot, thirdRoot}, true, now); err != nil {
		t.Fatal(err)
	}
	page, err := service.Issues(t.Context(), IssueOptions{Broken: true, IncludeChanges: true, RootUID: "third-root"})
	if err != nil || len(page.Items) != 1 || page.Sync.Partial || page.Items[0].Partial || page.Items[0].History != "complete" || len(page.Items[0].Gaps) != 0 || len(page.Items[0].Untracked) != 2 || !slices.Contains(page.Items[0].Untracked, "PersistentVolumeClaim") || !slices.Contains(page.Items[0].Untracked, "ServiceAccount") {
		t.Fatalf("untracked response semantics: %+v %v", page, err)
	}
	service.SetChangeStorage(storage.NewMemory())
	page, err = service.Issues(t.Context(), IssueOptions{Broken: true, IncludeChanges: true, RootUID: "third-root"})
	if err != nil || len(page.Items) != 1 || page.Sync.Partial || page.Items[0].Partial || page.Items[0].History != "gap" || len(page.Items[0].Gaps) == 0 || len(page.Items[0].Untracked) != 2 || !slices.Contains(page.Items[0].Untracked, "PersistentVolumeClaim") || !slices.Contains(page.Items[0].Untracked, "ServiceAccount") {
		t.Fatalf("missing tracked coverage response semantics: %+v %v", page, err)
	}
}
