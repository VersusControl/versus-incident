package kubernetes

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
		case "/api/v1/namespaces/shop/pods":
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
