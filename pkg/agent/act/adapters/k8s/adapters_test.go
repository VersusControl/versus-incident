package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	"github.com/VersusControl/versus-incident/pkg/agent/ledger"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type apiFunc func(context.Context, string, string, url.Values, []byte) ([]byte, error)

func TestProposalClusterAuthorizationPrecedesActorRequests(t *testing.T) {
	calls := 0
	api := apiFunc(func(context.Context, string, string, url.Values, []byte) ([]byte, error) {
		calls++
		return []byte(`{}`), nil
	})
	provider := storage.NewMemory()
	service, err := act.NewService(provider, "default", ledger.NewBlobWriter(provider, "default"), nil, NewAdapters(api, Options{Cluster: "one"})...)
	if err != nil {
		t.Fatal(err)
	}
	input := act.ProposalInput{Type: RolloutRestart, Target: act.TargetRef{Kind: "Deployment", Name: "api", Namespace: "shop"}, Params: json.RawMessage(`{}`)}
	authorization := core.CallerAuthorization{Authenticated: true, Actor: "operator", Permissions: map[core.Permission]bool{core.PermissionAgentApprove: true}, Clusters: &core.ClusterScope{IDs: []string{"two"}}}
	if _, err := service.Propose(core.WithCallerAuthorization(context.Background(), authorization), input); !errors.Is(err, act.ErrActionDenied) || calls != 0 {
		t.Fatalf("denied implicit cluster err=%v actor requests=%d", err, calls)
	}
	authorization.Clusters.IDs = []string{"one"}
	result, err := service.Propose(core.WithCallerAuthorization(context.Background(), authorization), input)
	if err != nil || result.Proposal.Target.Cluster != "one" || result.Approval.Proposal.Target.Cluster != "one" || calls != 1 {
		t.Fatalf("bound proposal=%+v err=%v actor requests=%d", result, err, calls)
	}
	service, err = act.NewService(provider, "default", ledger.NewBlobWriter(provider, "default"), nil, NewClusterAdapters(map[string]API{"one": api, "two": api}, Options{})...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Propose(core.WithCallerAuthorization(context.Background(), authorization), input); !errors.Is(err, act.ErrActionDenied) || calls != 1 {
		t.Fatalf("multi-cluster default err=%v actor requests=%d", err, calls)
	}
}

func TestLegacyApprovalListingIsReadOnlyAndClusterScoped(t *testing.T) {
	for _, test := range []struct {
		name         string
		clusters     []string
		direct       bool
		scope        *core.ClusterScope
		unauthorized bool
		visible      bool
	}{
		{name: "single OSS", clusters: []string{"one"}, direct: true, visible: true},
		{name: "single registry", clusters: []string{"one"}, visible: true},
		{name: "authorized restricted caller", clusters: []string{"one"}, scope: &core.ClusterScope{IDs: []string{"one"}}, visible: true},
		{name: "restricted other cluster", clusters: []string{"one"}, scope: &core.ClusterScope{IDs: []string{"two"}}},
		{name: "restricted empty scope", clusters: []string{"one"}, scope: &core.ClusterScope{}},
		{name: "unauthenticated", clusters: []string{"one"}, unauthorized: true},
		{name: "multiple unrestricted", clusters: []string{"one", "two"}},
		{name: "multiple hidden", clusters: []string{"one", "two"}, scope: &core.ClusterScope{IDs: []string{"one"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, state := range []string{"executed", "rejected", "pending", "approving"} {
				t.Run(state, func(t *testing.T) {
					calls := 0
					api := apiFunc(func(context.Context, string, string, url.Values, []byte) ([]byte, error) {
						calls++
						return []byte(`{}`), nil
					})
					apis := map[string]API{}
					for _, id := range test.clusters {
						apis[id] = api
					}
					adapters := NewClusterAdapters(apis, Options{})
					if test.direct {
						adapters = NewAdapters(api, Options{Cluster: "one"})
					}
					provider := storage.NewMemory()
					service, err := act.NewService(provider, "default", ledger.NewBlobWriter(provider, "default"), nil, adapters...)
					if err != nil {
						t.Fatal(err)
					}
					authorization := core.CallerAuthorization{Authenticated: true, Actor: "operator", Permissions: map[core.Permission]bool{core.PermissionAgentApprove: true}}
					mutationContext := core.WithCallerAuthorization(context.Background(), authorization)
					result, err := service.Propose(mutationContext, act.ProposalInput{Type: RolloutRestart, Target: act.TargetRef{Cluster: "one", Kind: "Deployment", Name: "api", Namespace: "shop"}, Params: json.RawMessage(`{}`)})
					if err != nil {
						t.Fatal(err)
					}
					blobs, err := provider.ListBlobs("")
					if err != nil {
						t.Fatal(err)
					}
					key := ""
					var legacy []byte
					for _, blob := range blobs {
						var record struct {
							Approval  act.Approval `json:"approval"`
							NonceHash string       `json:"nonce_hash"`
						}
						if json.Unmarshal(blob.Data, &record) != nil || record.Approval.ID != result.Approval.ID {
							continue
						}
						record.Approval.Proposal.Target.Cluster = ""
						record.Approval.State = state
						legacy, err = json.Marshal(record)
						if err != nil {
							t.Fatal(err)
						}
						key = blob.Name
						if err := provider.WriteBlob(key, legacy); err != nil {
							t.Fatal(err)
						}
					}
					if key == "" {
						t.Fatal("approval record not found")
					}
					authorization.Clusters = test.scope
					authorization.Authenticated = !test.unauthorized
					ctx := core.WithCallerAuthorization(context.Background(), authorization)
					for _, pendingOnly := range []bool{false, true} {
						listed, err := service.Approvals(ctx, pendingOnly)
						want := 0
						if test.visible && (!pendingOnly || state == "pending") {
							want = 1
						}
						if err != nil || len(listed) != want {
							t.Fatalf("pendingOnly=%v listing=%+v err=%v want=%d", pendingOnly, listed, err, want)
						}
						if want == 1 && (listed[0].Proposal.Target.Cluster != "" || listed[0].State != state || listed[0].Proposal.BindingHash != result.Proposal.BindingHash) {
							t.Fatalf("listing changed legacy record=%+v", listed[0])
						}
					}
					if _, err := service.Approve(mutationContext, result.Approval.ID, result.Nonce, "operator"); !errors.Is(err, act.ErrActionDenied) {
						t.Fatalf("legacy approval error=%v", err)
					}
					if _, err := service.Reject(mutationContext, result.Approval.ID, "not needed", "operator"); !errors.Is(err, act.ErrActionDenied) {
						t.Fatalf("legacy rejection error=%v", err)
					}
					after, err := provider.ReadBlob(key)
					if err != nil || !bytes.Equal(after, legacy) || calls != 1 {
						t.Fatalf("legacy record or actor changed: err=%v calls=%d", err, calls)
					}
					afterBlobs, err := provider.ListBlobs("")
					if err != nil || len(afterBlobs) != len(blobs) {
						t.Fatalf("durable entries changed: err=%v", err)
					}
					before := map[string][]byte{}
					for _, blob := range blobs {
						before[blob.Name] = blob.Data
					}
					for _, blob := range afterBlobs {
						if blob.Name != key && !bytes.Equal(blob.Data, before[blob.Name]) {
							t.Fatal("ledger changed during legacy read or denied mutation")
						}
					}
				})
			}
		})
	}
}

func TestClusterActionDispatch(t *testing.T) {
	calls := map[string]int{}
	apis := map[string]API{}
	for _, id := range []string{"one", "two"} {
		apis[id] = apiFunc(func(context.Context, string, string, url.Values, []byte) ([]byte, error) {
			calls[id]++
			return []byte(`{}`), nil
		})
	}
	var adapter act.Adapter
	for _, candidate := range NewClusterAdaptersWithOptions(apis, map[string]Options{"one": {MaxReplicas: 2}, "two": {MaxReplicas: 5}}) {
		if candidate.Type() == Scale {
			adapter = candidate
		}
	}
	proposal := act.Proposal{Type: Scale, Target: act.TargetRef{Cluster: "two", Namespace: "shop", Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{"replicas":3}`)}
	if maximum := adapter.Schema()["properties"].(map[string]any)["replicas"].(map[string]any)["maximum"]; maximum != 5 {
		t.Fatalf("shared replica maximum=%v", maximum)
	}
	if _, err := adapter.(act.TargetResolver).ResolveTarget(act.TargetRef{}); err == nil {
		t.Fatal("multi-cluster resolver defaulted an empty target")
	}
	if _, err := adapter.DryRun(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Execute(context.Background(), proposal); err != nil || calls["one"] != 0 || calls["two"] != 2 {
		t.Fatalf("dispatch %v %v", calls, err)
	}
	for _, id := range []string{"", "unknown", "one"} {
		proposal.Target.Cluster = id
		if _, err := adapter.Execute(context.Background(), proposal); err == nil {
			t.Fatalf("invalid cluster/limit %q accepted", id)
		}
	}
	if calls["one"] != 0 || calls["two"] != 2 {
		t.Fatal("invalid target reached actor")
	}
}

func TestSingleClusterTargetResolution(t *testing.T) {
	adapter := NewAdapters(apiFunc(func(context.Context, string, string, url.Values, []byte) ([]byte, error) {
		t.Fatal("target resolution reached actor")
		return nil, nil
	}), Options{Cluster: "one"})[0].(act.TargetResolver)
	target, err := adapter.ResolveTarget(act.TargetRef{Name: "api"})
	if err != nil || target.Cluster != "one" || target.Name != "api" {
		t.Fatalf("resolved target=%+v err=%v", target, err)
	}
	if _, err := adapter.ResolveTarget(act.TargetRef{Cluster: "two"}); err == nil {
		t.Fatal("mismatched cluster accepted")
	}
	for _, candidate := range NewClusterAdapters(map[string]API{"one": apiFunc(func(context.Context, string, string, url.Values, []byte) ([]byte, error) { return nil, nil })}, Options{MaxReplicas: 7}) {
		if candidate.Type() == Scale && candidate.Schema()["properties"].(map[string]any)["replicas"].(map[string]any)["maximum"] != 7 {
			t.Fatal("uniform replica limit missing from cluster schema")
		}
	}
}

func (function apiFunc) Do(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	return function(ctx, method, path, query, body)
}

func TestScaleDryRunAndExecutionUseTypedScaleSubresource(t *testing.T) {
	var calls int
	api := apiFunc(func(_ context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
		calls++
		if method != http.MethodPatch || path != "/apis/apps/v1/namespaces/shop/deployments/checkout/scale" {
			t.Fatalf("request = %s %s", method, path)
		}
		var patch struct {
			Spec struct {
				Replicas int `json:"replicas"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(body, &patch); err != nil || patch.Spec.Replicas != 3 {
			t.Fatalf("scale patch=%s err=%v", body, err)
		}
		if calls == 1 && query.Get("dryRun") != "All" || calls == 2 && query.Get("dryRun") != "" {
			t.Fatalf("request %d query=%v", calls, query)
		}
		return []byte(`{}`), nil
	})
	var adapter act.Adapter
	for _, candidate := range NewAdapters(api, Options{Cluster: "cluster-a", MaxReplicas: 5}) {
		if candidate.Type() == Scale {
			adapter = candidate
		}
	}
	proposal := act.Proposal{Type: Scale, Target: act.TargetRef{Cluster: "cluster-a", Namespace: "shop", Kind: "Deployment", Name: "checkout"}, Params: json.RawMessage(`{"replicas":3}`)}
	if err := adapter.Validate(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.DryRun(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Execute(context.Background(), proposal); err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestScaleRejectsZeroOverLimitAndUnknownFields(t *testing.T) {
	var adapter act.Adapter
	for _, candidate := range NewAdapters(apiFunc(func(context.Context, string, string, url.Values, []byte) ([]byte, error) { return nil, nil }), Options{MaxReplicas: 4}) {
		if candidate.Type() == Scale {
			adapter = candidate
		}
	}
	for _, params := range []string{`{"replicas":0}`, `{"replicas":5}`, `{"replicas":2,"force":true}`} {
		proposal := act.Proposal{Type: Scale, Target: act.TargetRef{Namespace: "shop", Kind: "Deployment", Name: "api"}, Params: json.RawMessage(params)}
		if err := adapter.Validate(context.Background(), proposal); err == nil {
			t.Errorf("Validate(%s) unexpectedly succeeded", params)
		}
	}
}

func TestRestartDryRunBindsTheTimestampAndVerifiesReadiness(t *testing.T) {
	createdAt := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	calls := 0
	api := apiFunc(func(_ context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
		calls++
		if path != "/apis/apps/v1/namespaces/shop/deployments/api" {
			t.Fatalf("path=%q", path)
		}
		if method == http.MethodPatch {
			var patch map[string]any
			if err := json.Unmarshal(body, &patch); err != nil {
				t.Fatal(err)
			}
			template := patch["spec"].(map[string]any)["template"].(map[string]any)
			metadata := template["metadata"].(map[string]any)
			if metadata["annotations"].(map[string]any)["versus.dev/restartedAt"] != createdAt.Format(time.RFC3339Nano) {
				t.Fatalf("restart patch=%v", patch)
			}
			if query.Get("dryRun") != "All" {
				t.Fatalf("dry-run query=%v", query)
			}
			return []byte(`{}`), nil
		}
		if method != http.MethodGet {
			t.Fatalf("verify method=%q", method)
		}
		return []byte(`{"metadata":{"generation":4},"spec":{"replicas":2},"status":{"observedGeneration":4,"updatedReplicas":2,"readyReplicas":2}}`), nil
	})
	var adapter act.Adapter
	for _, candidate := range NewAdapters(api, Options{Cluster: "cluster-a", VerificationTimeout: time.Second, PollInterval: time.Millisecond}) {
		if candidate.Type() == RolloutRestart {
			adapter = candidate
		}
	}
	proposal := act.Proposal{Type: RolloutRestart, Target: act.TargetRef{Namespace: "shop", Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`), CreatedAt: createdAt}
	if _, err := adapter.DryRun(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	verification, err := adapter.Verify(context.Background(), proposal, act.Result{})
	if err != nil || !verification.Verified || calls != 2 {
		t.Fatalf("verification=%+v calls=%d err=%v", verification, calls, err)
	}
}

func TestActionTargetAndOperationAllowList(t *testing.T) {
	var restart act.Adapter
	for _, candidate := range NewAdapters(apiFunc(func(context.Context, string, string, url.Values, []byte) ([]byte, error) { return nil, nil }), Options{Cluster: "cluster-a"}) {
		if candidate.Type() == RolloutRestart {
			restart = candidate
		}
	}
	if restart.Destructive() {
		t.Fatal("bounded rollout restart was classified as destructive")
	}
	proposal := act.Proposal{Type: RolloutRestart, Target: act.TargetRef{Cluster: "other", Namespace: "shop", Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`)}
	if err := restart.Validate(context.Background(), proposal); err == nil {
		t.Fatal("mismatched cluster was accepted")
	}
	proposal.Target.Cluster = "cluster-a"
	proposal.Target.Name = "../../secrets"
	if err := restart.Validate(context.Background(), proposal); err == nil {
		t.Fatal("non-resource name was accepted")
	}
	proposal.Target.Name = "checkout"
	proposal.Params = json.RawMessage(`null`)
	if err := restart.Validate(context.Background(), proposal); err == nil {
		t.Fatal("null parameters were accepted as an empty object")
	}
	proposal.Params = json.RawMessage(`{}`)
	proposal.Target.Namespace = "shop.prod"
	if err := restart.Validate(context.Background(), proposal); err == nil {
		t.Fatal("namespace with a dot was accepted")
	}
	allowed := map[act.ActionType]bool{RolloutRestart: true, Scale: true, CronJobSuspend: true, CronJobResume: true, NodeCordon: true, NodeUncordon: true, CronJobTrigger: true}
	for _, candidate := range NewAdapters(apiFunc(func(context.Context, string, string, url.Values, []byte) ([]byte, error) { return nil, nil }), Options{}) {
		if !allowed[candidate.Type()] {
			t.Errorf("unexpected registered action type %q", candidate.Type())
		}
		delete(allowed, candidate.Type())
	}
	if len(allowed) != 0 {
		t.Fatalf("missing allow-listed operations: %v", allowed)
	}
}

func TestCronJobTriggerBindsDryRunsExecutesAndVerifiesOneJob(t *testing.T) {
	proposalID := "123e4567-e89b-12d3-a456-426614174000"
	cronJobPath := "/apis/batch/v1/namespaces/shop/cronjobs/nightly"
	jobCollectionPath := "/apis/batch/v1/namespaces/shop/jobs"
	jobPath := jobCollectionPath + "/versus-123e4567e89b"
	calls := 0
	api := apiFunc(func(_ context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
		calls++
		switch {
		case method == http.MethodGet && path == cronJobPath:
			return []byte(`{"metadata":{"uid":"uid-1","resourceVersion":"12"},"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"run","image":"worker:v1"}],"restartPolicy":"Never"}}}}}}`), nil
		case method == http.MethodPost && path == jobCollectionPath:
			if calls == 3 && query.Get("dryRun") != "All" || calls == 5 && query.Get("dryRun") != "" {
				t.Fatalf("post call %d query=%v", calls, query)
			}
			var job map[string]any
			if err := json.Unmarshal(body, &job); err != nil {
				t.Fatal(err)
			}
			if job["kind"] != "Job" || job["apiVersion"] != "batch/v1" || job["metadata"].(map[string]any)["name"] != "versus-123e4567e89b" {
				t.Fatalf("job manifest=%v", job)
			}
			owner := job["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)
			if _, present := owner["blockOwnerDeletion"]; present {
				t.Fatalf("job owner reference requests blockOwnerDeletion: %v", owner)
			}
			return []byte(`{}`), nil
		case method == http.MethodGet && path == jobPath:
			return []byte(`{"status":{"active":1}}`), nil
		default:
			t.Fatalf("unexpected request %s %s", method, path)
			return nil, nil
		}
	})
	var adapter act.Adapter
	for _, candidate := range NewAdapters(api, Options{Cluster: "cluster-a", VerificationTimeout: time.Second, PollInterval: time.Millisecond}) {
		if candidate.Type() == CronJobTrigger {
			adapter = candidate
		}
	}
	proposal := act.Proposal{ID: proposalID, RunID: "run-1", Type: CronJobTrigger, Target: act.TargetRef{Cluster: "cluster-a", Namespace: "shop", Kind: "CronJob", Name: "nightly"}, Params: json.RawMessage(`{}`)}
	bound, err := adapter.(act.ProposalBinder).Bind(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	proposal.Params = bound
	if err := adapter.Validate(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.DryRun(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Execute(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	verification, err := adapter.Verify(context.Background(), proposal, act.Result{})
	if err != nil || !verification.Verified || calls != 6 {
		t.Fatalf("verification=%+v calls=%d err=%v", verification, calls, err)
	}
}

func TestCronJobTriggerRejectsTemplateChangesAfterProposal(t *testing.T) {
	reads := 0
	api := apiFunc(func(_ context.Context, method, path string, _ url.Values, _ []byte) ([]byte, error) {
		if method != http.MethodGet || path != "/apis/batch/v1/namespaces/shop/cronjobs/nightly" {
			t.Fatalf("unexpected request %s %s", method, path)
		}
		reads++
		version := "12"
		if reads > 1 {
			version = "13"
		}
		return []byte(`{"metadata":{"uid":"uid-1","resourceVersion":"` + version + `"},"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[]}}}}}}`), nil
	})
	var adapter act.Adapter
	for _, candidate := range NewAdapters(api, Options{}) {
		if candidate.Type() == CronJobTrigger {
			adapter = candidate
		}
	}
	proposal := act.Proposal{ID: "123e4567-e89b-12d3-a456-426614174000", Type: CronJobTrigger, Target: act.TargetRef{Namespace: "shop", Kind: "CronJob", Name: "nightly"}, Params: json.RawMessage(`{}`)}
	bound, err := adapter.(act.ProposalBinder).Bind(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	proposal.Params = bound
	if _, err := adapter.DryRun(context.Background(), proposal); err == nil || reads != 2 {
		t.Fatalf("dry-run err=%v source reads=%d; changed CronJob must fail closed", err, reads)
	}
}
