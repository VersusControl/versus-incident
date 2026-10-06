package kubernetes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitOpsApplicationProjectionRemovesURLCredentialsAndScrubsMessage(t *testing.T) {
	finishedAt := "2026-10-05T12:00:00Z"
	summary := safeSummary("Application", map[string]any{
		"metadata": map[string]any{"name": "checkout", "namespace": "apps", "creationTimestamp": "2026-10-01T00:00:00Z"},
		"spec":     map[string]any{"source": map[string]any{"repoURL": "https://user:secret@example.test/repo?token=value#branch"}},
		"status": map[string]any{
			"sync":           map[string]any{"status": "OutOfSync", "revision": "abc123"},
			"health":         map[string]any{"status": "Degraded"},
			"operationState": map[string]any{"message": "secret token", "finishedAt": finishedAt},
		},
	}, replacingScrubber{})
	app := gitOpsAppFromResource("argocd", ProjectedResource{Namespace: "apps", Name: "checkout", Summary: summary})
	if app.Sync != "OutOfSync" || app.Health != "Degraded" || app.Revision != "abc123" || app.Source != "https://example.test/repo" || app.Message != "[redacted] token" {
		t.Fatalf("app = %#v", app)
	}
	if !app.LastSyncAt.Equal(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("last sync = %s", app.LastSyncAt)
	}
}

func TestRolloutProjectionMapsStepsAndReplicaSets(t *testing.T) {
	summary := safeSummary("Rollout", map[string]any{
		"spec":   map[string]any{"strategy": map[string]any{"canary": map[string]any{"steps": []any{map[string]any{"setWeight": float64(25)}, map[string]any{"pause": map[string]any{}}}}}},
		"status": map[string]any{"phase": "Progressing", "currentStepIndex": float64(0), "stableRS": "stable", "canaryRS": "canary", "message": "waiting"},
	}, nil)
	rollout := rolloutFromResource(ProjectedResource{Namespace: "apps", Name: "checkout", Summary: summary})
	if rollout.Phase != "Progressing" || rollout.Strategy != "canary" || rollout.Step != 0 || rollout.TotalSteps != 2 || rollout.Weight != 25 || rollout.StableRS != "stable" || rollout.CanaryRS != "canary" {
		t.Fatalf("rollout = %#v", rollout)
	}
}

func TestGitOpsSourceRequiresSafeAbsoluteURL(t *testing.T) {
	if got := sanitizeGitOpsSource("//example.test/repo"); got != "" {
		t.Fatalf("relative source = %q", got)
	}
}

func TestGitOpsAppsAndRolloutUseDiscoveredResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "argoproj.io", "versions": []any{map[string]any{"groupVersion": "argoproj.io/v1alpha1", "version": "v1alpha1"}}, "preferredVersion": map[string]any{"groupVersion": "argoproj.io/v1alpha1", "version": "v1alpha1"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis/argoproj.io/v1alpha1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "applications", "kind": "Application", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "rollouts", "kind": "Rollout", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/apis/argoproj.io/v1alpha1/namespaces/apps/rollouts":
			if request.URL.Query().Get("continue") == "page-2" {
				writeJSON(writer, map[string]any{"items": []any{map[string]any{
					"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout",
					"metadata": map[string]any{"name": "checkout-2", "namespace": "apps"},
					"status":   map[string]any{"phase": "Healthy"},
				}}})
				return
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "page-2"}, "items": []any{map[string]any{
				"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout",
				"metadata": map[string]any{"name": "checkout", "namespace": "apps"},
				"status":   map[string]any{"phase": "Progressing"},
			}}})
		case "/apis/argoproj.io/v1alpha1/applications":
			if request.URL.Query().Get("continue") == "apps-page-2" {
				writeJSON(writer, map[string]any{"items": []any{map[string]any{
					"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
					"metadata": map[string]any{"name": "api", "namespace": "apps"},
					"status":   map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": "Healthy"}},
				}}})
				return
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "apps-page-2"}, "items": []any{map[string]any{
				"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
				"metadata": map[string]any{"name": "checkout", "namespace": "apps"},
				"spec":     map[string]any{"source": map[string]any{"repoURL": "https://user:secret@example.test/repo?token=hide"}},
				"status":   map[string]any{"sync": map[string]any{"status": "OutOfSync", "revision": "abc"}, "health": map[string]any{"status": "Degraded"}},
			}}})
		case "/apis/argoproj.io/v1alpha1/namespaces/apps/rollouts/checkout":
			writeJSON(writer, map[string]any{
				"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout",
				"metadata": map[string]any{"name": "checkout", "namespace": "apps"},
				"spec":     map[string]any{"strategy": map[string]any{"canary": map[string]any{"steps": []any{map[string]any{"setWeight": float64(20)}}}}},
				"status":   map[string]any{"phase": "Progressing", "currentStepIndex": float64(0), "stableRS": "stable", "canaryRS": "canary"},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	service.SetScrubber(replacingScrubber{})
	apps, err := service.GitOpsApps(t.Context(), GitOpsAppOptions{Tool: "argocd"})
	if err != nil || !apps.Available || len(apps.Items) != 2 || apps.Items[0].Kind != "Application" || apps.Items[0].Source != "https://example.test/repo" || strings.Contains(apps.Items[0].Source, "secret") {
		t.Fatalf("apps = %#v err=%v", apps, err)
	}
	appPage, err := service.GitOpsApps(t.Context(), GitOpsAppOptions{Tool: "argocd", Limit: 1})
	if err != nil || len(appPage.Items) != 1 || appPage.Items[0].Name != "checkout" || appPage.Next != "1" || !appPage.Truncated {
		t.Fatalf("first GitOps page = %#v err=%v", appPage, err)
	}
	appPage, err = service.GitOpsApps(t.Context(), GitOpsAppOptions{Tool: "argocd", Limit: 1, Cursor: appPage.Next})
	if err != nil || len(appPage.Items) != 1 || appPage.Items[0].Name != "api" || appPage.Next != "" || appPage.Truncated {
		t.Fatalf("second GitOps page = %#v err=%v", appPage, err)
	}
	rollout, err := service.Rollout(t.Context(), "apps", "checkout")
	if err != nil || rollout.Phase != "Progressing" || rollout.Strategy != "canary" || rollout.StableRS != "stable" || rollout.CanaryRS != "canary" {
		t.Fatalf("rollout = %#v err=%v", rollout, err)
	}
	rollouts, err := service.Rollouts(t.Context(), RolloutListOptions{Namespace: "apps", Limit: 1})
	if err != nil || !rollouts.Available || len(rollouts.Items) != 1 || rollouts.Items[0].Name != "checkout" || rollouts.Next != "page-2" || !rollouts.Truncated {
		t.Fatalf("rollouts first page = %#v err=%v", rollouts, err)
	}
	rollouts, err = service.Rollouts(t.Context(), RolloutListOptions{Namespace: "apps", Limit: 1, Cursor: rollouts.Next})
	if err != nil || len(rollouts.Items) != 1 || rollouts.Items[0].Name != "checkout-2" || rollouts.Next != "" || rollouts.Truncated {
		t.Fatalf("rollouts second page = %#v err=%v", rollouts, err)
	}
}

func TestRolloutsReportsMissingCRD(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	page, err := service.Rollouts(t.Context(), RolloutListOptions{})
	if err != nil || page.Available || page.Reason == "" || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("missing Rollouts CRD page = %#v err=%v", page, err)
	}
}
