package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	versustools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/versus"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type fixedChangeFeed struct {
	changes []commontools.ChangeRecord
	err     error
}

func (feed fixedChangeFeed) Changes(context.Context, time.Time) ([]commontools.ChangeRecord, error) {
	return feed.changes, feed.err
}

type fixedActionApprovals []act.Approval

func (approvals fixedActionApprovals) Approvals(context.Context, bool) ([]act.Approval, error) {
	return approvals, nil
}

type countingActionApprovals struct {
	approvals []act.Approval
	calls     atomic.Int32
}

func (source *countingActionApprovals) Approvals(context.Context, bool) ([]act.Approval, error) {
	source.calls.Add(1)
	return source.approvals, nil
}

func infrastructureViewContext() context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{
		Authenticated: true,
		Actor:         "operator-1",
		Permissions:   map[core.Permission]bool{core.PermissionInfrastructureView: true},
	})
}

func TestKubernetesChangeFeedMapsTimelineRecords(t *testing.T) {
	provider := storage.NewMemory()
	scope := kubeindex.Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}
	now := time.Now().UTC()
	store := kubechanges.NewStore(provider, scope)
	if err := store.Append([]kubechanges.Change{{ID: "change-a", Cluster: "cluster-a", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid-a", Type: kubechanges.ImageChanged, Service: "checkout", Fields: []kubechanges.FieldChange{{Path: "containers.images", From: "checkout:v1", To: "checkout:v2"}}, At: now}}, now); err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(nil, kubernetes.Scope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, CredentialID: scope.CredentialID}, 0)
	service.SetChangeStorage(provider)
	changes, err := newKubernetesChangeFeed(service).Changes(infrastructureViewContext(), now.Add(-time.Minute))
	if err != nil || len(changes) != 1 || changes[0].Kind != "k8s_image_changed" || changes[0].Service != "checkout" || changes[0].Ref != "k8s://cluster-a/shop/Deployment/checkout" || !strings.Contains(changes[0].Summary, "checkout:v1 -> checkout:v2") {
		t.Fatalf("feed records=%#v err=%v", changes, err)
	}
}

func TestActionChangeFeedIncludesExecutedActionsWithFailedVerification(t *testing.T) {
	now := time.Now().UTC()
	feed := newActionChangeFeed(fixedActionApprovals{
		{State: "verification_failed", ExecutedAt: now, Proposal: act.Proposal{ID: "proposal-1", Type: "k8s.scale", Target: act.TargetRef{Name: "checkout"}, DryRun: "Set replicas to 3"}},
		{State: "pending", Proposal: act.Proposal{ID: "proposal-2", Type: "k8s.scale", Target: act.TargetRef{Name: "other"}}},
	})
	changes, err := feed.Changes(infrastructureViewContext(), now.Add(-time.Minute))
	if err != nil || len(changes) != 1 || changes[0].Kind != "agent_action" || changes[0].Ref != "proposal-1" || changes[0].Service != "checkout" || !strings.Contains(changes[0].Summary, "verification failed") {
		t.Fatalf("action changes=%#v err=%v", changes, err)
	}
}

func TestChangeFeedsRequireInfrastructurePermissionBeforeReading(t *testing.T) {
	provider := storage.NewMemory()
	scope := kubeindex.Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}
	now := time.Now().UTC()
	store := kubechanges.NewStore(provider, scope)
	record := kubeindex.Record{UID: "uid-a", Kind: "Deployment", Namespace: "shop", Name: "checkout", Labels: map[string]string{"app.kubernetes.io/name": "checkout"}}
	change, ok := kubechanges.Detect(scope.ClusterID, kubeindex.Delta{Kind: "Deployment", Op: "upsert", New: &record}, now)
	if !ok {
		t.Fatal("expected the labeled Kubernetes record to produce a change")
	}
	if err := store.Append([]kubechanges.Change{change}, now); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.NotFound(writer, request)
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	kubeService := kubernetes.NewService(client, kubernetes.Scope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, CredentialID: scope.CredentialID}, 0)
	kubeService.SetChangeStorage(provider)
	kubeFeed := newKubernetesChangeFeed(kubeService)
	actionSource := &countingActionApprovals{approvals: []act.Approval{{
		State: "verified", ExecutedAt: now, Proposal: act.Proposal{ID: "proposal-a", Type: "k8s.scale", Target: act.TargetRef{Name: "checkout"}, DryRun: "replicas changed"},
	}}}
	actionFeed := newActionChangeFeed(actionSource)

	for _, contextName := range []string{"absent", "chat-denied", "analyze-denied"} {
		ctx := context.Background()
		if contextName != "absent" {
			ctx = core.WithCallerAuthorization(ctx, core.CallerAuthorization{Authenticated: true, Actor: "operator-1"})
		}
		for _, feed := range []commontools.ChangeFeed{kubeFeed, actionFeed} {
			changes, err := feed.Changes(ctx, now.Add(-time.Minute))
			if err != nil || len(changes) != 0 {
				t.Fatalf("%s unauthorized changes=%#v err=%v", contextName, changes, err)
			}
		}
	}
	if requests.Load() != 0 || actionSource.calls.Load() != 0 {
		t.Fatalf("unauthorized feed reads: Kubernetes requests=%d action reads=%d", requests.Load(), actionSource.calls.Load())
	}

	for _, contextName := range []string{"chat-authorized", "analyze-authorized"} {
		changes, err := kubeFeed.Changes(infrastructureViewContext(), now.Add(-time.Minute))
		if err != nil || len(changes) != 1 || changes[0].Service != "checkout" {
			t.Fatalf("%s Kubernetes changes=%#v err=%v", contextName, changes, err)
		}
		changes, err = actionFeed.Changes(infrastructureViewContext(), now.Add(-time.Minute))
		if err != nil || len(changes) != 1 || changes[0].Kind != "agent_action" {
			t.Fatalf("%s action changes=%#v err=%v", contextName, changes, err)
		}
	}
}

func TestUnauthorizedRecentChangesKeepsGitFeedUsable(t *testing.T) {
	now := time.Now().UTC()
	gitRecord := commontools.ChangeRecord{Timestamp: now, Service: "checkout", Kind: "commit", Summary: "git change"}
	feed := mergeChangeFeeds(fixedChangeFeed{changes: []commontools.ChangeRecord{gitRecord}}, newKubernetesChangeFeed(nil))
	result, err := (commontools.RecentChanges{Feed: feed}).Invoke(context.Background(), nil)
	if err != nil || result == nil || !result.Found || result.Data["count"] != 1 {
		t.Fatalf("unauthorized Git recent changes=%+v err=%v", result, err)
	}
}

func TestMergeChangeFeedsIsNilSafeAndKeepsSuccessfulSources(t *testing.T) {
	if mergeChangeFeeds(nil, nil) != nil {
		t.Fatal("empty feed list should remain nil")
	}
	now := time.Now().UTC()
	older := commontools.ChangeRecord{Timestamp: now.Add(-time.Minute), Kind: "git"}
	newer := commontools.ChangeRecord{Timestamp: now, Kind: "k8s_created"}
	merged := mergeChangeFeeds(fixedChangeFeed{changes: []commontools.ChangeRecord{older}}, fixedChangeFeed{changes: []commontools.ChangeRecord{newer}}, fixedChangeFeed{err: errors.New("source unavailable")})
	changes, err := merged.Changes(context.Background(), now.Add(-time.Hour))
	if err != nil || len(changes) != 2 || changes[0].Kind != newer.Kind || changes[1].Kind != older.Kind {
		t.Fatalf("merged records=%#v err=%v", changes, err)
	}
	failed := mergeChangeFeeds(fixedChangeFeed{err: errors.New("offline")})
	if _, err := failed.Changes(context.Background(), now); err == nil {
		t.Fatal("all-feed failure was suppressed")
	}
}

func TestRecentChangesAvailableWithChangeFeedAndNoGitHub(t *testing.T) {
	configured := aitools.Snapshot{
		Integrations: map[string]aitools.DependencyStatus{"github": {Name: "GitHub"}},
		Capabilities: map[string]aitools.DependencyStatus{"change_feed": {Configured: true, Name: "Change feed"}},
	}
	snapshot := buildToolAvailabilitySnapshot(configured, nil, nil, fixedChangeFeed{}, nil, nil, versustools.DetectionHealthSnapshot{})
	resolved := aitools.Resolve(aitools.Requirement{Kind: aitools.RequirementCapability, Capabilities: []string{"change_feed"}}, snapshot, true)
	if resolved.State != aitools.StateAvailable || snapshot.Integrations["github"].Healthy {
		t.Fatalf("change feed resolution=%+v GitHub=%+v", resolved, snapshot.Integrations["github"])
	}
}

func TestKubernetesOnlyChangeFeedIsVisibleToChatAndAnalyze(t *testing.T) {
	cfg := config.AgentConfig{}
	cfg.Tools.Kubernetes.Endpoint = "https://cluster.example"
	configured := configuredToolAvailabilitySnapshot(cfg, storage.NewMemory())
	if !configured.Capabilities["change_feed"].Configured || configured.Integrations["github"].Configured {
		t.Fatalf("Kubernetes-only configuration=%+v", configured)
	}
	feed := fixedChangeFeed{}
	snapshot := buildToolAvailabilitySnapshot(configured, nil, nil, feed, nil, nil, versustools.DetectionHealthSnapshot{})
	if !snapshot.Capabilities["change_feed"].Healthy || snapshot.Integrations["github"].Healthy {
		t.Fatalf("Kubernetes-only availability=%+v", snapshot)
	}
	runtime := []core.Tool{commontools.RecentChanges{Feed: feed}}
	scope := tenancy.NewOrgScope("org-a")
	manager := aitools.NewManager(storage.NewMemory())
	for _, agentKind := range []aitools.AgentKind{aitools.AgentChat, aitools.AgentAnalyze} {
		filtered, err := manager.Filter(scope, agentKind, runtime, snapshot)
		if err != nil || len(filtered) != 1 || filtered[0].Name() != "recent_changes" {
			t.Fatalf("%s change-feed tools=%v err=%v", agentKind, toolNamesForTest(filtered), err)
		}
	}
	if _, err := manager.SetToolsetEnabled(scope, aitools.AgentAnalyze, "source-control", false); err != nil {
		t.Fatal(err)
	}
	chat, err := manager.Filter(scope, aitools.AgentChat, runtime, snapshot)
	if err != nil || len(chat) != 1 {
		t.Fatalf("Chat source-control policy filtered=%v err=%v", toolNamesForTest(chat), err)
	}
	analyze, err := manager.Filter(scope, aitools.AgentAnalyze, runtime, snapshot)
	if err != nil || len(analyze) != 0 {
		t.Fatalf("Analyze source-control policy filtered=%v err=%v", toolNamesForTest(analyze), err)
	}
}
