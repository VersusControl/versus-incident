package act

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ledger"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type fakeAdapter struct {
	executions int
	verified   bool
}

func (*fakeAdapter) Type() ActionType                                 { return "k8s.rollout_restart" }
func (*fakeAdapter) Destructive() bool                                { return false }
func (*fakeAdapter) Schema() map[string]any                           { return map[string]any{"type": "object"} }
func (*fakeAdapter) Validate(context.Context, Proposal) error         { return nil }
func (*fakeAdapter) DryRun(context.Context, Proposal) (string, error) { return "restart workload", nil }
func (adapter *fakeAdapter) Execute(context.Context, Proposal) (Result, error) {
	adapter.executions++
	return Result{Summary: "restart requested"}, nil
}
func (adapter *fakeAdapter) Verify(context.Context, Proposal, Result) (Verification, error) {
	return Verification{Verified: adapter.verified, Summary: "rollout checked"}, nil
}

type failingIntentWriter struct{ ledger.Writer }

func (writer failingIntentWriter) Append(ctx context.Context, entry ledger.Entry) (ledger.Entry, error) {
	if entry.Kind == ledger.EntryActionExecuted && entry.State == "intent" {
		return ledger.Entry{}, ledger.ErrLedgerUnavailable
	}
	return writer.Writer.Append(ctx, entry)
}

func newTestService(t *testing.T, adapter Adapter, writer ledger.Writer) (*Service, storage.Provider) {
	t.Helper()
	provider := storage.NewMemory()
	if writer == nil {
		writer = ledger.NewBlobWriter(provider, "org-a")
	}
	service, err := NewService(provider, "org-a", writer, nil, adapter)
	if err != nil {
		t.Fatal(err)
	}
	return service, provider
}

func TestProposeRequiresExplicitApprovalPermission(t *testing.T) {
	service, _ := newTestService(t, &fakeAdapter{}, nil)
	input := ProposalInput{Type: "k8s.rollout_restart", Target: TargetRef{Kind: "Deployment", Name: "api", Namespace: "shop"}, Params: json.RawMessage(`{}`)}
	if _, err := service.Propose(context.Background(), input); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("unauthorized proposal error=%v", err)
	}
	withoutActor := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionAgentApprove: true}})
	if _, err := service.Propose(withoutActor, input); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("proposal without authenticated actor error=%v", err)
	}
	if result, err := service.Propose(authorizedContext(true), input); err != nil || result.Approval == nil {
		t.Fatalf("authorized proposal=%+v err=%v", result, err)
	}
}

func authorizedContext(allowed bool) context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{
		Authenticated: true,
		Actor:         "operator-1",
		Permissions:   map[core.Permission]bool{core.PermissionAgentApprove: allowed},
	})
}

func TestRegistryRejectsDestructiveTypesAndScaleZeroSchemas(t *testing.T) {
	for _, actionType := range []ActionType{"k8s.delete", "k8s.node_drain", "k8s.apply_manifest", "k8s.patch", "k8s.rollout_undo", "gitops.sync", "argo.rollout_promote", "argo.rollout_abort", "argo.rollout_retry"} {
		adapter := &fakeAdapter{}
		adapterType := actionType
		registry := NewRegistry()
		if err := registry.Register(adapterTypeOverride{Adapter: adapter, actionType: adapterType}); !errors.Is(err, ErrActionDenied) {
			t.Errorf("Register(%q) error=%v", actionType, err)
		}
	}
	registry := NewRegistry()
	scaleSchema := &fakeAdapterWithSchema{fakeAdapter: fakeAdapter{}, schema: map[string]any{"properties": map[string]any{"replicas": map[string]any{"type": "integer", "minimum": 0}}}}
	if err := registry.Register(scaleSchema); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("zero-permitting schema error=%v", err)
	}
}

type adapterTypeOverride struct {
	Adapter
	actionType ActionType
}

func (adapter adapterTypeOverride) Type() ActionType { return adapter.actionType }

type fakeAdapterWithSchema struct {
	fakeAdapter
	schema map[string]any
}

func (adapter *fakeAdapterWithSchema) Schema() map[string]any { return adapter.schema }

func TestGuideOnlyForDestructiveAndScaleZero(t *testing.T) {
	adapter := &fakeAdapter{}
	service, _ := newTestService(t, adapter, nil)
	for _, input := range []ProposalInput{
		{Type: "k8s.node_drain", Target: TargetRef{Kind: "node", Name: "worker-1"}},
		{Type: "k8s.scale", Target: TargetRef{Kind: "deployment", Name: "api", Namespace: "prod"}, Params: json.RawMessage(`{"replicas":0}`)},
	} {
		result, err := service.Propose(context.Background(), input)
		if err != nil || result.Guide == nil || result.Proposal != nil || result.Approval != nil {
			t.Fatalf("guide result=%+v err=%v", result, err)
		}
	}
	if adapter.executions != 0 {
		t.Fatalf("executions=%d, want no execution", adapter.executions)
	}
}

func TestUnregisteredEffectSensitiveActionOnlyIssuesGuide(t *testing.T) {
	adapter := &fakeAdapter{}
	service, _ := newTestService(t, adapter, nil)
	result, err := service.Propose(authorizedContext(true), ProposalInput{
		Type: "gitops.sync", Target: TargetRef{Kind: "Application", Namespace: "shop", Name: "checkout"}, Params: json.RawMessage(`{}`),
	})
	if err != nil || result.Guide == nil || result.Proposal != nil || result.Approval != nil {
		t.Fatalf("proposal result=%+v err=%v", result, err)
	}
	if adapter.executions != 0 {
		t.Fatalf("unregistered action executed %d times", adapter.executions)
	}
}

func TestProposalToolDoesNotExposeApprovalNonce(t *testing.T) {
	service, _ := newTestService(t, &fakeAdapter{verified: true}, nil)
	tool := ProposalTool{Service: service}
	observer := &approvalEventCapture{}
	ctx := core.WithChatObserver(authorizedContext(true), observer)
	result, err := tool.Invoke(ctx, json.RawMessage(`{"type":"k8s.rollout_restart","target":{"kind":"Deployment","namespace":"shop","name":"checkout"},"params":{},"reason":"restore service"}`))
	if err != nil || result == nil {
		t.Fatalf("proposal result=%+v err=%v", result, err)
	}
	if _, exposed := result.Data["nonce"]; exposed {
		t.Fatal("approval nonce was exposed in model-visible tool data")
	}
	if result.Data["approval"] == nil {
		t.Fatalf("proposal data lacks approval details: %+v", result.Data)
	}
	if observer.event.Kind != core.ChatEventApproval || observer.event.Approval == nil || observer.event.ApprovalNonce == "" {
		t.Fatalf("approval event = %+v", observer.event)
	}
}

type approvalEventCapture struct{ event core.ChatEvent }

func (capture *approvalEventCapture) OnChatEvent(event core.ChatEvent) { capture.event = event }

func TestApprovalBindsParametersExpiresReauthorizesAndIsSingleUse(t *testing.T) {
	adapter := &fakeAdapter{verified: true}
	service, provider := newTestService(t, adapter, nil)
	ctx := authorizedContext(true)
	proposed, err := service.Propose(ctx, ProposalInput{Type: adapter.Type(), Target: TargetRef{Kind: "Deployment", Name: "api", Namespace: "prod"}, Params: json.RawMessage(`{}`), Reason: "restore capacity"})
	if err != nil || proposed.Approval == nil || proposed.Nonce == "" {
		t.Fatalf("Propose=%+v err=%v", proposed, err)
	}
	if proposed.Proposal.ProposedBy != "operator-1" {
		t.Fatalf("proposal actor=%q, want authenticated actor", proposed.Proposal.ProposedBy)
	}
	missingActor := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{
		Authenticated: true,
		Permissions:   map[core.Permission]bool{core.PermissionAgentApprove: true},
	})
	if _, err := service.Approve(missingActor, proposed.Approval.ID, proposed.Nonce, "gateway"); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("approval with missing principal error=%v", err)
	}
	if _, err := service.Reject(missingActor, proposed.Approval.ID, "not authorized", "gateway"); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("rejection with missing principal error=%v", err)
	}
	untrustedActor := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{
		Authenticated: true,
		Actor:         "operator-2",
		Permissions:   map[core.Permission]bool{core.PermissionAgentApprove: true},
	})
	if _, err := service.Approve(untrustedActor, proposed.Approval.ID, proposed.Nonce, "operator-1"); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("approval with mismatched principal error=%v", err)
	}
	if _, err := service.Approve(authorizedContext(false), proposed.Approval.ID, proposed.Nonce, "operator-1"); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("revoked authorization error=%v", err)
	}
	key := service.key(proposed.Approval.ID)
	data, err := provider.ReadBlob(key)
	if err != nil {
		t.Fatal(err)
	}
	var stored proposalRecord
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Approval.Proposal.Params = json.RawMessage(`{"changed":true}`)
	tampered, _ := json.Marshal(stored)
	if err := provider.WriteBlob(key, tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Approve(ctx, proposed.Approval.ID, proposed.Nonce, "operator-1"); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("parameter tamper error=%v", err)
	}
	stored.Approval.Proposal.Params = json.RawMessage(`{}`)
	stored.Approval.Proposal.Target.Name = "other"
	tampered, _ = json.Marshal(stored)
	if err := provider.WriteBlob(key, tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Approve(ctx, proposed.Approval.ID, proposed.Nonce, "operator-1"); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("target tamper error=%v", err)
	}
	stored.Approval.Proposal.Target.Name = "api"
	stored.Approval.Proposal.ExpiresAt = time.Now().Add(-time.Second)
	stored.Approval.ExpiresAt = stored.Approval.Proposal.ExpiresAt
	expired, _ := json.Marshal(stored)
	if err := provider.WriteBlob(key, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Approve(ctx, proposed.Approval.ID, proposed.Nonce, "operator-1"); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("expired approval error=%v", err)
	}
}

func TestApprovalExecutesOnlyAfterLedgerIntentAndCannotRepeat(t *testing.T) {
	adapter := &fakeAdapter{verified: true}
	service, _ := newTestService(t, adapter, nil)
	ctx := authorizedContext(true)
	proposed, err := service.Propose(ctx, ProposalInput{Type: adapter.Type(), Target: TargetRef{Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := service.Approve(ctx, proposed.Approval.ID, proposed.Nonce, "operator-1")
	if err != nil || approved.State != "verified" || approved.Approver != "operator-1" || adapter.executions != 1 {
		t.Fatalf("approval=%+v executions=%d err=%v", approved, adapter.executions, err)
	}
	if _, err := service.Approve(ctx, proposed.Approval.ID, proposed.Nonce, "operator-1"); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("second approval error=%v", err)
	}
	if adapter.executions != 1 {
		t.Fatalf("execution repeated: %d", adapter.executions)
	}
}

func TestApprovalDoesNotReportUnverifiedExecutionAsVerified(t *testing.T) {
	service, _ := newTestService(t, &fakeAdapter{verified: false}, nil)
	ctx := authorizedContext(true)
	proposed, err := service.Propose(ctx, ProposalInput{Type: "k8s.rollout_restart", Target: TargetRef{Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := service.Approve(ctx, proposed.Approval.ID, proposed.Nonce, "operator-1")
	if err != nil || approved.State != "verification_failed" || approved.Verified == nil || approved.Verified.Verified {
		t.Fatalf("approval=%+v err=%v", approved, err)
	}
}

type blockingAdapter struct {
	entered    chan struct{}
	release    chan struct{}
	executions atomic.Int32
}

func (*blockingAdapter) Type() ActionType                         { return "k8s.rollout_restart" }
func (*blockingAdapter) Destructive() bool                        { return false }
func (*blockingAdapter) Schema() map[string]any                   { return map[string]any{"type": "object"} }
func (*blockingAdapter) Validate(context.Context, Proposal) error { return nil }
func (*blockingAdapter) DryRun(context.Context, Proposal) (string, error) {
	return "restart workload", nil
}
func (adapter *blockingAdapter) Execute(ctx context.Context, _ Proposal) (Result, error) {
	adapter.executions.Add(1)
	close(adapter.entered)
	select {
	case <-adapter.release:
		return Result{Summary: "restart requested"}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}
func (*blockingAdapter) Verify(context.Context, Proposal, Result) (Verification, error) {
	return Verification{Verified: true}, nil
}

func TestApprovalClaimDoesNotHoldServiceLockDuringExecution(t *testing.T) {
	adapter := &blockingAdapter{entered: make(chan struct{}), release: make(chan struct{})}
	service, _ := newTestService(t, adapter, nil)
	proposal, err := service.Propose(authorizedContext(true), ProposalInput{
		Type: adapter.Type(), Target: TargetRef{Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() {
		_, approveErr := service.Approve(authorizedContext(true), proposal.Approval.ID, proposal.Nonce, "operator-1")
		first <- approveErr
	}()
	<-adapter.entered
	second := make(chan error, 1)
	go func() {
		_, approveErr := service.Approve(authorizedContext(true), proposal.Approval.ID, proposal.Nonce, "operator-1")
		second <- approveErr
	}()
	select {
	case err := <-second:
		if !errors.Is(err, ErrActionDenied) {
			t.Fatalf("duplicate approval error=%v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("duplicate approval blocked behind action execution")
	}
	close(adapter.release)
	if err := <-first; err != nil || adapter.executions.Load() != 1 {
		t.Fatalf("first approval err=%v executions=%d", err, adapter.executions.Load())
	}
}

type approvalReadBarrier struct {
	storage.Provider
	key     string
	loaded  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (provider *approvalReadBarrier) ReadBlob(name string) ([]byte, error) {
	data, err := provider.Provider.ReadBlob(name)
	if name == provider.key {
		provider.once.Do(func() {
			close(provider.loaded)
			<-provider.release
		})
	}
	return data, err
}

func (provider *approvalReadBarrier) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	cas, ok := provider.Provider.(storage.BlobCAS)
	if !ok {
		return false, errors.New("provider does not support compare-and-swap")
	}
	return cas.CompareAndSwapBlob(name, expected, replacement)
}

func TestRejectLosesToApprovalClaimAcrossServices(t *testing.T) {
	adapter := &fakeAdapter{verified: true}
	provider := storage.NewMemory()
	writer := ledger.NewBlobWriter(provider, "org-a")
	approver, err := NewService(provider, "org-a", writer, nil, adapter)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := approver.Propose(authorizedContext(true), ProposalInput{
		Type: adapter.Type(), Target: TargetRef{Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	barrier := &approvalReadBarrier{
		Provider: provider, key: approver.key(proposal.Approval.ID),
		loaded: make(chan struct{}), release: make(chan struct{}),
	}
	rejector, err := NewService(barrier, "org-a", writer, nil, adapter)
	if err != nil {
		t.Fatal(err)
	}
	rejection := make(chan error, 1)
	go func() {
		_, rejectErr := rejector.Reject(authorizedContext(true), proposal.Approval.ID, "not needed", "operator-1")
		rejection <- rejectErr
	}()
	<-barrier.loaded

	approved, err := approver.Approve(authorizedContext(true), proposal.Approval.ID, proposal.Nonce, "operator-1")
	if err != nil || approved.State != "verified" || adapter.executions != 1 {
		t.Fatalf("approval=%+v executions=%d err=%v", approved, adapter.executions, err)
	}
	close(barrier.release)
	if err := <-rejection; !errors.Is(err, ErrActionDenied) {
		t.Fatalf("stale rejection error=%v, want denied", err)
	}
	approvals, err := approver.Approvals(context.Background(), false)
	if err != nil || len(approvals) != 1 || approvals[0].State != "verified" || adapter.executions != 1 {
		t.Fatalf("approvals=%+v executions=%d err=%v", approvals, adapter.executions, err)
	}
}

func TestStaleApprovalClaimRequiresReconciliationAndCannotExecuteAgain(t *testing.T) {
	adapter := &fakeAdapter{verified: true}
	service, provider := newTestService(t, adapter, nil)
	proposal, err := service.Propose(authorizedContext(true), ProposalInput{
		Type: adapter.Type(), Target: TargetRef{Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := service.key(proposal.Approval.ID)
	data, err := provider.ReadBlob(key)
	if err != nil {
		t.Fatal(err)
	}
	var record proposalRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.Approval.State = "approving"
	record.Approval.ClaimedAt = time.Now().Add(-approvalExecutionTimeout - approvalUnknownGrace - time.Second).UTC()
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.WriteBlob(key, data); err != nil {
		t.Fatal(err)
	}

	approvals, err := service.Approvals(context.Background(), false)
	if err != nil || len(approvals) != 1 || approvals[0].State != "execution_unknown" || !approvals[0].ReconciliationRequired {
		t.Fatalf("approvals=%+v err=%v, want reconciliation-required unknown execution", approvals, err)
	}
	if _, err := service.Approve(authorizedContext(true), proposal.Approval.ID, proposal.Nonce, "operator-1"); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("approval retry error=%v, want denied", err)
	}
	if adapter.executions != 0 {
		t.Fatalf("unknown execution was repeated: executions=%d", adapter.executions)
	}
}

func TestApprovalExecutionSurvivesRequestCancellation(t *testing.T) {
	adapter := &fakeAdapter{verified: true}
	service, _ := newTestService(t, adapter, nil)
	proposal, err := service.Propose(authorizedContext(true), ProposalInput{
		Type: adapter.Type(), Target: TargetRef{Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestContext, cancel := context.WithCancel(authorizedContext(true))
	cancel()
	approved, err := service.Approve(requestContext, proposal.Approval.ID, proposal.Nonce, "operator-1")
	if err != nil || approved.State != "verified" || adapter.executions != 1 {
		t.Fatalf("approval=%+v executions=%d err=%v", approved, adapter.executions, err)
	}
}

func TestPendingApprovalsExcludesExpiredRecords(t *testing.T) {
	service, _ := newTestService(t, &fakeAdapter{}, nil)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	proposal, err := service.Propose(authorizedContext(true), ProposalInput{
		Type: "k8s.rollout_restart", Target: TargetRef{Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	now = proposal.Approval.ExpiresAt
	pending, err := service.Approvals(context.Background(), true)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending approvals=%+v err=%v, want expired approval excluded", pending, err)
	}
	all, err := service.Approvals(context.Background(), false)
	if err != nil || len(all) != 1 || all[0].State != "pending" {
		t.Fatalf("all approvals=%+v err=%v", all, err)
	}
}

func TestLedgerFailureBlocksActionExecution(t *testing.T) {
	adapter := &fakeAdapter{verified: true}
	provider := storage.NewMemory()
	baseWriter := ledger.NewBlobWriter(provider, "org-a")
	service, err := NewService(provider, "org-a", failingIntentWriter{Writer: baseWriter}, nil, adapter)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authorizedContext(true)
	proposed, err := service.Propose(ctx, ProposalInput{Type: adapter.Type(), Target: TargetRef{Kind: "Deployment", Name: "api"}, Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Approve(ctx, proposed.Approval.ID, proposed.Nonce, "operator-1"); !errors.Is(err, ledger.ErrLedgerUnavailable) {
		t.Fatalf("Approve error=%v, want ledger unavailable", err)
	}
	if adapter.executions != 0 {
		t.Fatalf("adapter executed %d times after ledger failure", adapter.executions)
	}
}
