package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
)

type apiFunc func(context.Context, string, string, url.Values, []byte) ([]byte, error)

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
