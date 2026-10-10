package act

import (
	"context"
	"encoding/json"
	"time"
)

type ActionType string
type Risk string
type Decision string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"

	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

type TargetRef struct {
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

type Proposal struct {
	ID          string          `json:"id"`
	RunID       string          `json:"run_id"`
	Type        ActionType      `json:"type"`
	Target      TargetRef       `json:"target"`
	Params      json.RawMessage `json:"params"`
	ParamsHash  string          `json:"params_hash"`
	BindingHash string          `json:"binding_hash"`
	DryRun      string          `json:"dry_run"`
	Risk        Risk            `json:"risk"`
	Reason      string          `json:"reason"`
	ProposedBy  string          `json:"proposed_by"`
	CreatedAt   time.Time       `json:"created_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
}

type ProposalInput struct {
	Type   ActionType      `json:"type"`
	Target TargetRef       `json:"target"`
	Params json.RawMessage `json:"params"`
	Reason string          `json:"reason"`
}

type Result struct {
	Summary  string          `json:"summary,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

type Verification struct {
	Verified bool   `json:"verified"`
	Summary  string `json:"summary"`
}

type Adapter interface {
	Type() ActionType
	Destructive() bool
	Schema() map[string]any
	Validate(context.Context, Proposal) error
	DryRun(context.Context, Proposal) (string, error)
	Execute(context.Context, Proposal) (Result, error)
	Verify(context.Context, Proposal, Result) (Verification, error)
}

type ProposalBinder interface {
	Bind(context.Context, Proposal) (json.RawMessage, error)
}

type TargetResolver interface {
	ResolveTarget(TargetRef) (TargetRef, error)
}

type ReadTargetResolver interface {
	ResolveReadTarget(TargetRef) (TargetRef, error)
}

type Authorizer interface {
	Authorize(context.Context, string, Proposal) error
}

type Approval struct {
	ID                     string        `json:"id"`
	Proposal               Proposal      `json:"proposal"`
	State                  string        `json:"state"`
	Approver               string        `json:"approver,omitempty"`
	ExpiresAt              time.Time     `json:"expires_at"`
	ClaimedAt              time.Time     `json:"claimed_at,omitempty"`
	ReconciliationRequired bool          `json:"reconciliation_required,omitempty"`
	ExecutedAt             time.Time     `json:"executed_at,omitempty"`
	VerifiedAt             time.Time     `json:"verified_at,omitempty"`
	Result                 *Result       `json:"result,omitempty"`
	Verified               *Verification `json:"verification,omitempty"`
}

type Guide struct {
	Action  string `json:"action"`
	Command string `json:"command"`
	Risk    string `json:"risk"`
}

type ProposalResult struct {
	Proposal *Proposal `json:"proposal,omitempty"`
	Approval *Approval `json:"approval,omitempty"`
	Nonce    string    `json:"nonce,omitempty"`
	Guide    *Guide    `json:"guide,omitempty"`
}
