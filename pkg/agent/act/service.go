package act

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ledger"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/google/uuid"
)

const (
	proposalTTL              = 10 * time.Minute
	approvalExecutionTimeout = 3 * time.Minute
	approvalUnknownGrace     = 30 * time.Second
)

type PermissionAuthorizer struct{}

func (PermissionAuthorizer) Authorize(ctx context.Context, approver string, proposal Proposal) error {
	if !core.CallerAuthorized(ctx, core.PermissionAgentApprove) {
		return core.NewToolError(core.ToolErrorInternal, "approval not authorized", ErrActionDenied)
	}
	if proposal.Risk == RiskHigh && approver == proposal.ProposedBy {
		return core.NewToolError(core.ToolErrorInternal, "self-approval is not permitted", ErrActionDenied)
	}
	return nil
}

type proposalRecord struct {
	Approval  Approval `json:"approval"`
	NonceHash string   `json:"nonce_hash"`
}

type Service struct {
	provider storage.Provider
	org      string
	writer   ledger.Writer
	registry *Registry
	auth     Authorizer
	now      func() time.Time
	mu       sync.Mutex
}

func NewService(provider storage.Provider, org string, writer ledger.Writer, authorizer Authorizer, adapters ...Adapter) (*Service, error) {
	if provider == nil || writer == nil {
		return nil, ledger.ErrLedgerUnavailable
	}
	if authorizer == nil {
		authorizer = PermissionAuthorizer{}
	}
	registry := NewRegistry()
	for _, adapter := range adapters {
		if err := registry.Register(adapter); err != nil {
			return nil, err
		}
	}
	return &Service{provider: provider, org: storage.NormalizeOrgID(org), writer: writer, registry: registry, auth: authorizer, now: time.Now}, nil
}

func (service *Service) Propose(ctx context.Context, input ProposalInput) (ProposalResult, error) {
	if service == nil || ctx == nil {
		return ProposalResult{}, ErrActionDenied
	}
	adapter, ok := service.registry.Lookup(input.Type)
	if ok {
		if resolver, resolves := adapter.(TargetResolver); resolves {
			target, err := resolver.ResolveTarget(input.Target)
			if err != nil {
				return ProposalResult{}, ErrActionDenied
			}
			input.Target = target
		}
	}
	if input.Target.Cluster != "" && !core.CallerClusterAllowed(ctx, input.Target.Cluster) {
		return ProposalResult{}, ErrActionDenied
	}
	if isForbiddenType(input.Type) || scaleToZero(input) {
		return service.issueGuide(ctx, input)
	}
	if !core.CallerAuthorized(ctx, core.PermissionAgentApprove) {
		return ProposalResult{}, ErrActionDenied
	}
	actor := core.CallerActor(ctx)
	if actor == "" {
		return ProposalResult{}, ErrActionDenied
	}
	if !ok {
		return service.issueGuide(ctx, input)
	}
	if !json.Valid(input.Params) {
		return ProposalResult{}, core.NewToolError(core.ToolErrorInvalidArguments, "invalid action parameters", ErrActionDenied)
	}
	run, err := service.ensureRun(ctx)
	if err != nil {
		return ProposalResult{}, err
	}
	paramsHash, params, err := canonicalParams(input.Params)
	if err != nil {
		return ProposalResult{}, core.NewToolError(core.ToolErrorInvalidArguments, "invalid action parameters", err)
	}
	proposal := Proposal{
		ID: uuid.NewString(), RunID: run.RunID, Type: input.Type, Target: input.Target,
		Params: params, ParamsHash: paramsHash, Risk: RiskMedium,
		Reason: bounded(input.Reason, 512), ProposedBy: actor, CreatedAt: service.now().UTC(),
	}
	proposal.ExpiresAt = proposal.CreatedAt.Add(proposalTTL)
	if binder, ok := adapter.(ProposalBinder); ok {
		boundParams, err := binder.Bind(ctx, proposal)
		if err != nil {
			return ProposalResult{}, core.NewToolError(core.ToolErrorBackend, "action proposal binding failed", err)
		}
		paramsHash, params, err = canonicalParams(boundParams)
		if err != nil {
			return ProposalResult{}, core.NewToolError(core.ToolErrorInvalidArguments, "action proposal binding is invalid", err)
		}
		proposal.Params = params
		proposal.ParamsHash = paramsHash
	}
	if err := adapter.Validate(ctx, proposal); err != nil {
		return ProposalResult{}, core.NewToolError(core.ToolErrorInvalidArguments, "action proposal is invalid", err)
	}
	diff, err := adapter.DryRun(ctx, proposal)
	if err != nil {
		return ProposalResult{}, core.NewToolError(core.ToolErrorBackend, "action dry-run failed", err)
	}
	proposal.DryRun = bounded(diff, 2048)
	proposal.BindingHash = proposalBindingHash(proposal)
	payload, err := json.Marshal(struct {
		Type       ActionType `json:"type"`
		Target     TargetRef  `json:"target"`
		ParamsHash string     `json:"params_hash"`
		DryRun     string     `json:"dry_run"`
	}{proposal.Type, proposal.Target, proposal.ParamsHash, proposal.DryRun})
	if err != nil {
		return ProposalResult{}, ErrActionDenied
	}
	if _, err := run.Writer.Append(ctx, ledger.Entry{RunID: run.RunID, Kind: ledger.EntryActionProposed, State: "intent", Ref: proposal.ID, Payload: payload}); err != nil {
		return ProposalResult{}, ledger.ErrLedgerUnavailable
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return ProposalResult{}, ErrActionDenied
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	approval := Approval{ID: uuid.NewString(), Proposal: proposal, State: "pending", ExpiresAt: proposal.ExpiresAt}
	record := proposalRecord{Approval: approval, NonceHash: digest([]byte(nonce + proposal.BindingHash))}
	service.mu.Lock()
	err = service.save(record)
	service.mu.Unlock()
	if err != nil {
		return ProposalResult{}, err
	}
	return ProposalResult{Proposal: &proposal, Approval: &approval, Nonce: nonce}, nil
}

func (service *Service) Approvals(ctx context.Context, pendingOnly bool) ([]Approval, error) {
	if service == nil || service.provider == nil || ctx == nil {
		return nil, ErrActionDenied
	}
	blobs, err := service.provider.ListBlobs(service.prefix())
	if err != nil {
		return nil, ledger.ErrLedgerUnavailable
	}
	result := make([]Approval, 0, len(blobs))
	for _, blob := range blobs {
		if ctx.Err() != nil || len(result) >= 1000 {
			break
		}
		var record proposalRecord
		if json.Unmarshal(blob.Data, &record) != nil {
			return nil, ledger.ErrLedgerUnavailable
		}
		if !service.targetAllowed(ctx, record.Approval.Proposal) {
			if !service.legacyTargetReadable(ctx, record.Approval.Proposal) {
				continue
			}
		} else {
			record = service.reconcileExecutionUnknown(record)
		}
		if pendingOnly && (record.Approval.State != "pending" || !service.now().Before(record.Approval.ExpiresAt)) {
			continue
		}
		result = append(result, record.Approval)
	}
	return result, nil
}

func (service *Service) Approve(ctx context.Context, approvalID, nonce, approver string) (Approval, error) {
	actor := core.CallerActor(ctx)
	if actor == "" || approver != actor {
		return Approval{}, ErrActionDenied
	}
	record, err := service.load(approvalID)
	if err != nil {
		return Approval{}, err
	}
	if !service.targetAllowed(ctx, record.Approval.Proposal) {
		return Approval{}, ErrActionDenied
	}
	record = service.reconcileExecutionUnknown(record)
	bindingHash := proposalBindingHash(record.Approval.Proposal)
	if record.Approval.State != "pending" || !service.now().Before(record.Approval.ExpiresAt) || !validNonce(nonce, record.NonceHash, bindingHash) {
		return Approval{}, ErrActionDenied
	}
	paramsHash, _, err := canonicalParams(record.Approval.Proposal.Params)
	if err != nil || paramsHash != record.Approval.Proposal.ParamsHash || bindingHash != record.Approval.Proposal.BindingHash {
		return Approval{}, ErrActionDenied
	}
	if err := service.auth.Authorize(ctx, approver, record.Approval.Proposal); err != nil {
		return Approval{}, err
	}
	adapter, ok := service.registry.Lookup(record.Approval.Proposal.Type)
	if !ok {
		return Approval{}, ErrActionDenied
	}
	if err := adapter.Validate(ctx, record.Approval.Proposal); err != nil {
		return Approval{}, ErrActionDenied
	}
	record.Approval.State = "approving"
	record.Approval.Approver = approver
	record.Approval.ClaimedAt = service.now().UTC()
	if err := service.claimApproval(record, nonce); err != nil {
		return Approval{}, err
	}
	executionContext, cancelExecution := context.WithTimeout(context.WithoutCancel(ctx), approvalExecutionTimeout)
	defer cancelExecution()
	runID := record.Approval.Proposal.RunID
	approvalPayload, _ := json.Marshal(struct {
		ProposalID string `json:"proposal_id"`
		ParamsHash string `json:"params_hash"`
		Approver   string `json:"approver"`
	}{record.Approval.Proposal.ID, paramsHash, approver})
	if _, err := service.writer.Append(executionContext, ledger.Entry{RunID: runID, Kind: ledger.EntryApproval, State: "approved", Ref: record.Approval.ID, Payload: approvalPayload}); err != nil {
		return service.failLedger(record)
	}
	intentPayload, _ := json.Marshal(struct {
		Type       ActionType `json:"type"`
		Target     TargetRef  `json:"target"`
		ParamsHash string     `json:"params_hash"`
	}{record.Approval.Proposal.Type, record.Approval.Proposal.Target, paramsHash})
	if _, err := service.writer.Append(executionContext, ledger.Entry{RunID: runID, Kind: ledger.EntryActionExecuted, State: "intent", Ref: record.Approval.Proposal.ID, Payload: intentPayload}); err != nil {
		return service.failLedger(record)
	}
	result, executeErr := adapter.Execute(executionContext, record.Approval.Proposal)
	if executeErr != nil {
		record.Approval.State = "failed"
		if err := service.save(record); err != nil {
			return Approval{}, errors.Join(executeErr, err)
		}
		_, ledgerErr := service.writer.Append(executionContext, ledger.Entry{RunID: runID, Kind: ledger.EntryActionExecuted, State: "failed", Ref: record.Approval.Proposal.ID, Payload: intentPayload})
		return record.Approval, errors.Join(executeErr, ledgerErr)
	}
	record.Approval.ExecutedAt = service.now().UTC()
	record.Approval.Result = &result
	resultPayload, _ := json.Marshal(struct {
		Summary string `json:"summary"`
	}{bounded(result.Summary, 1024)})
	if _, err := service.writer.Append(executionContext, ledger.Entry{RunID: runID, Kind: ledger.EntryActionExecuted, State: "done", Ref: record.Approval.Proposal.ID, Payload: resultPayload}); err != nil {
		return service.failLedger(record)
	}
	record.Approval.State = "executed"
	if err := service.save(record); err != nil {
		return Approval{}, err
	}
	verification, verifyErr := adapter.Verify(executionContext, record.Approval.Proposal, result)
	if verifyErr == nil {
		verifyPayload, _ := json.Marshal(verification)
		_, verifyErr = service.writer.Append(executionContext, ledger.Entry{RunID: runID, Kind: ledger.EntryActionVerified, State: verificationState(verification), Ref: record.Approval.Proposal.ID, Payload: verifyPayload})
	}
	if verifyErr != nil {
		record.Approval.State = "verification_failed"
		_ = service.save(record)
		return record.Approval, verifyErr
	}
	record.Approval.Verified = &verification
	if !verification.Verified {
		record.Approval.State = "verification_failed"
		if err := service.save(record); err != nil {
			return Approval{}, err
		}
		return record.Approval, nil
	}
	record.Approval.State = "verified"
	record.Approval.VerifiedAt = service.now().UTC()
	if err := service.save(record); err != nil {
		return Approval{}, err
	}
	return record.Approval, nil
}

func (service *Service) Reject(ctx context.Context, approvalID, reason, approver string) (Approval, error) {
	if core.CallerActor(ctx) == "" || approver != core.CallerActor(ctx) {
		return Approval{}, ErrActionDenied
	}
	record, err := service.load(approvalID)
	if err != nil || record.Approval.State != "pending" || !service.now().Before(record.Approval.ExpiresAt) {
		return Approval{}, ErrActionDenied
	}
	if !service.targetAllowed(ctx, record.Approval.Proposal) {
		return Approval{}, ErrActionDenied
	}
	if err := service.auth.Authorize(ctx, approver, record.Approval.Proposal); err != nil {
		return Approval{}, err
	}
	record.Approval.State = "rejecting"
	record.Approval.Approver = approver
	if err := service.claimPending(record, ""); err != nil {
		return Approval{}, err
	}
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
	}{bounded(reason, 512), approver})
	if _, err := service.writer.Append(ctx, ledger.Entry{RunID: record.Approval.Proposal.RunID, Kind: ledger.EntryApproval, State: "rejected", Ref: record.Approval.ID, Payload: payload}); err != nil {
		record.Approval.State = "rejection_failed"
		_ = service.save(record)
		return Approval{}, ledger.ErrLedgerUnavailable
	}
	record.Approval.State = "rejected"
	record.Approval.Approver = approver
	if err := service.save(record); err != nil {
		return Approval{}, err
	}
	return record.Approval, nil
}

func (service *Service) ensureRun(ctx context.Context) (ledger.Run, error) {
	if run, ok := ledger.RunFromContext(ctx); ok {
		return run, nil
	}
	runID := uuid.NewString()
	if err := service.writer.Begin(ctx, ledger.Trigger{RunID: runID, Kind: ledger.TriggerUser, Principal: core.CallerActor(ctx), Org: service.org, Surface: "api", PolicyVersion: "oss-default"}); err != nil {
		return ledger.Run{}, ledger.ErrLedgerUnavailable
	}
	return ledger.Run{Writer: service.writer, RunID: runID}, nil
}

func (service *Service) legacyTargetReadable(ctx context.Context, proposal Proposal) bool {
	if ctx == nil || proposal.Target.Cluster != "" {
		return false
	}
	adapter, ok := service.registry.Lookup(proposal.Type)
	if !ok {
		return false
	}
	var target TargetRef
	var err error
	if resolver, ok := adapter.(ReadTargetResolver); ok {
		target, err = resolver.ResolveReadTarget(proposal.Target)
	} else if resolver, ok := adapter.(TargetResolver); ok {
		target, err = resolver.ResolveTarget(proposal.Target)
	} else {
		return false
	}
	if err != nil || target.Cluster == "" || !core.CallerClusterAllowed(ctx, target.Cluster) {
		return false
	}
	target.Cluster = ""
	return target == proposal.Target
}

func (service *Service) targetAllowed(ctx context.Context, proposal Proposal) bool {
	if ctx == nil || !core.CallerClusterAllowed(ctx, proposal.Target.Cluster) {
		return false
	}
	if adapter, ok := service.registry.Lookup(proposal.Type); ok {
		if resolver, resolves := adapter.(TargetResolver); resolves {
			target, err := resolver.ResolveTarget(proposal.Target)
			return err == nil && target == proposal.Target
		}
	}
	return true
}

func (service *Service) claimApproval(record proposalRecord, nonce string) error {
	return service.claimPending(record, nonce)
}

func (service *Service) claimPending(record proposalRecord, nonce string) error {
	cas, ok := service.provider.(storage.BlobCAS)
	if !ok {
		return ledger.ErrLedgerUnavailable
	}
	if record.Approval.State != "approving" && record.Approval.State != "rejecting" {
		return ErrActionDenied
	}
	key := service.key(record.Approval.ID)
	current, err := service.provider.ReadBlob(key)
	if err != nil || len(current) == 0 {
		return ErrActionDenied
	}
	var stored proposalRecord
	if json.Unmarshal(current, &stored) != nil {
		return ledger.ErrLedgerUnavailable
	}
	bindingHash := proposalBindingHash(stored.Approval.Proposal)
	paramsHash, _, paramsErr := canonicalParams(stored.Approval.Proposal.Params)
	if stored.Approval.State != "pending" || !service.now().Before(stored.Approval.ExpiresAt) || bindingHash != stored.Approval.Proposal.BindingHash || paramsErr != nil || paramsHash != stored.Approval.Proposal.ParamsHash {
		return ErrActionDenied
	}
	if nonce != "" && !validNonce(nonce, stored.NonceHash, bindingHash) {
		return ErrActionDenied
	}
	replacement, err := json.Marshal(record)
	if err != nil || len(replacement) > 16*1024 {
		return ledger.ErrLedgerUnavailable
	}
	swapped, err := cas.CompareAndSwapBlob(key, current, replacement)
	if err != nil {
		return ledger.ErrLedgerUnavailable
	}
	if !swapped {
		return ErrActionDenied
	}
	return nil
}

func (service *Service) reconcileExecutionUnknown(record proposalRecord) proposalRecord {
	approval := record.Approval
	if approval.State != "approving" || (!approval.ClaimedAt.IsZero() && service.now().Sub(approval.ClaimedAt) <= approvalExecutionTimeout+approvalUnknownGrace) {
		return record
	}
	key := service.key(approval.ID)
	current, err := service.provider.ReadBlob(key)
	if err != nil || len(current) == 0 {
		return record
	}
	var stored proposalRecord
	if json.Unmarshal(current, &stored) != nil || stored.Approval.State != "approving" || !stored.Approval.ClaimedAt.Equal(approval.ClaimedAt) {
		return record
	}
	stored.Approval.State = "execution_unknown"
	stored.Approval.ReconciliationRequired = true
	replacement, err := json.Marshal(stored)
	if err != nil {
		return record
	}
	cas, ok := service.provider.(storage.BlobCAS)
	if !ok {
		return record
	}
	swapped, err := cas.CompareAndSwapBlob(key, current, replacement)
	if err == nil && swapped {
		return stored
	}
	return record
}

func (service *Service) issueGuide(ctx context.Context, input ProposalInput) (ProposalResult, error) {
	run, err := service.ensureRun(ctx)
	if err != nil {
		return ProposalResult{}, err
	}
	guide := manualGuide(input)
	payload, _ := json.Marshal(guide)
	if _, err := run.Writer.Append(ctx, ledger.Entry{RunID: run.RunID, Kind: ledger.EntryGuideIssued, State: "done", Ref: string(input.Type), Payload: payload}); err != nil {
		return ProposalResult{}, ledger.ErrLedgerUnavailable
	}
	return ProposalResult{Guide: &guide}, nil
}

func manualGuide(input ProposalInput) Guide {
	kind := strings.ToLower(input.Target.Kind)
	name := safeName(input.Target.Name)
	namespace := safeName(input.Target.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	switch {
	case input.Type == "k8s.scale" && scaleToZero(input):
		return Guide{Action: "scale to zero", Command: fmt.Sprintf("kubectl scale %s/%s --replicas=0 -n %s", kind, name, namespace), Risk: "This may make the service unavailable; inspect capacity and workload dependencies first."}
	case strings.Contains(strings.ToLower(string(input.Type)), "drain"):
		return Guide{Action: "node drain", Command: fmt.Sprintf("kubectl drain %s --ignore-daemonsets", name), Risk: "This evicts workloads and can disrupt service; check PDBs and capacity first."}
	case strings.Contains(strings.ToLower(string(input.Type)), "delete") || strings.Contains(strings.ToLower(string(input.Type)), "remove") || strings.Contains(strings.ToLower(string(input.Type)), "destroy"):
		return Guide{Action: "delete resource", Command: fmt.Sprintf("kubectl delete %s/%s -n %s", kind, name, namespace), Risk: "Deletion is not executable through Versus; verify ownership, backups, and dependent workloads."}
	case strings.Contains(strings.ToLower(string(input.Type)), "apply") || strings.Contains(strings.ToLower(string(input.Type)), "patch"):
		return Guide{Action: "manifest change", Command: "kubectl diff -f <reviewed-manifest.yaml>", Risk: "Arbitrary manifests are never executed by Versus; review the complete diff and server-side validation first."}
	default:
		return Guide{Action: string(input.Type), Command: "", Risk: "This action is not supported for execution; use the owning system's reviewed manual workflow."}
	}
}

func (service *Service) failLedger(record proposalRecord) (Approval, error) {
	record.Approval.State = "ledger_failed"
	_ = service.save(record)
	return record.Approval, ledger.ErrLedgerUnavailable
}

func (service *Service) load(id string) (proposalRecord, error) {
	if id == "" || strings.ContainsAny(id, "/\\") {
		return proposalRecord{}, ErrActionDenied
	}
	data, err := service.provider.ReadBlob(service.key(id))
	if err != nil || data == nil {
		return proposalRecord{}, ErrActionDenied
	}
	var record proposalRecord
	if json.Unmarshal(data, &record) != nil {
		return proposalRecord{}, ledger.ErrLedgerUnavailable
	}
	return record, nil
}

func (service *Service) save(record proposalRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil || len(encoded) > 16*1024 || service.provider.WriteBlob(service.key(record.Approval.ID), encoded) != nil {
		return ledger.ErrLedgerUnavailable
	}
	return nil
}

func (service *Service) prefix() string {
	return "agent-actions/" + digest([]byte(service.org)) + "/proposals/"
}
func (service *Service) key(id string) string { return service.prefix() + id }

func canonicalParams(raw json.RawMessage) (string, json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	return digest(encoded), encoded, nil
}

func digest(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func proposalBindingHash(proposal Proposal) string {
	data, _ := json.Marshal(struct {
		ID         string     `json:"id"`
		RunID      string     `json:"run_id"`
		Type       ActionType `json:"type"`
		Target     TargetRef  `json:"target"`
		ParamsHash string     `json:"params_hash"`
		DryRun     string     `json:"dry_run"`
		Reason     string     `json:"reason"`
		ProposedBy string     `json:"proposed_by"`
		CreatedAt  time.Time  `json:"created_at"`
		ExpiresAt  time.Time  `json:"expires_at"`
	}{proposal.ID, proposal.RunID, proposal.Type, proposal.Target, proposal.ParamsHash, proposal.DryRun, proposal.Reason, proposal.ProposedBy, proposal.CreatedAt, proposal.ExpiresAt})
	return digest(data)
}

func validNonce(nonce, expected, bindingHash string) bool {
	actual := digest([]byte(nonce + bindingHash))
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func isForbiddenType(actionType ActionType) bool {
	name := strings.ToLower(string(actionType))
	for _, denied := range []string{"delete", "remove", "destroy", "drain", "prune", "apply", "patch"} {
		if strings.Contains(name, denied) {
			return true
		}
	}
	return false
}

func scaleToZero(input ProposalInput) bool {
	if input.Type != "k8s.scale" {
		return false
	}
	var params struct {
		Replicas int `json:"replicas"`
	}
	return json.Unmarshal(input.Params, &params) == nil && params.Replicas == 0
}

func bounded(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func safeName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 63 {
		return "<name>"
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '.') {
			return "<name>"
		}
	}
	return value
}

func verificationState(verification Verification) string {
	if verification.Verified {
		return "done"
	}
	return "failed"
}
