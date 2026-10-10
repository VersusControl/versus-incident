package agentapi

import (
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
)

const (
	APIVersion = "v1"
	MinClient  = "v1"
)

type Bootstrap struct {
	APIVersion string             `json:"api_version"`
	MinClient  string             `json:"min_client"`
	Server     ServerInfo         `json:"server"`
	Principal  Principal          `json:"principal"`
	Profiles   []Profile          `json:"profiles"`
	Toolsets   []Toolset          `json:"toolsets"`
	Features   Features           `json:"features"`
	Kubernetes *KubernetesCatalog `json:"kubernetes,omitempty"`
}

type KubernetesCatalog struct {
	Multiple bool                        `json:"multiple"`
	Clusters []kubernetes.ClusterSummary `json:"clusters"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type Principal struct {
	Subject string `json:"subject,omitempty"`
	Org     string `json:"org"`
}

type Profile struct {
	Name string `json:"name"`
}

type Toolset struct {
	Name  string   `json:"name"`
	Tools []string `json:"tools,omitempty"`
}

type Features struct {
	Approvals bool `json:"approvals"`
	Ledger    bool `json:"ledger"`
	Decider   bool `json:"decider"`
	MCP       bool `json:"mcp"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type TurnMode string

const (
	ModeInteractive TurnMode = "interactive"
	ModeHeadless    TurnMode = "headless"
)

type Attachment struct {
	Incident *IncidentAttachment `json:"incident,omitempty"`
	Service  string              `json:"service,omitempty"`
	Time     *TimeRange          `json:"time_range,omitempty"`
	Resource *ResourceRef        `json:"resource,omitempty"`
}

type IncidentAttachment struct {
	ID      string    `json:"id"`
	Title   string    `json:"title,omitempty"`
	Service string    `json:"service,omitempty"`
	Status  string    `json:"status,omitempty"`
	Created time.Time `json:"created,omitempty"`
}

type TimeRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type ResourceRef struct {
	Provider   string `json:"provider"`
	Cluster    string `json:"cluster"`
	ResourceID string `json:"resource_id"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

type TurnRequest struct {
	Message    string      `json:"message"`
	Attachment *Attachment `json:"attachment,omitempty"`
	Mode       TurnMode    `json:"mode,omitempty"`
	Profile    string      `json:"profile,omitempty"`
}

type Session struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Seeded    bool      `json:"seeded"`
	Turns     []Turn    `json:"turns"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Turn struct {
	ID         string      `json:"id"`
	Role       string      `json:"role"`
	Content    string      `json:"content"`
	CreatedAt  time.Time   `json:"created_at"`
	Attachment *Attachment `json:"attachment,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	Citations  []Citation  `json:"citations,omitempty"`
	Events     []Event     `json:"events,omitempty"`
}

type ToolCall struct {
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name"`
	Args   string `json:"args,omitempty"`
	Output string `json:"output,omitempty"`
}

type Citation struct {
	Tool    string `json:"tool"`
	Label   string `json:"label,omitempty"`
	Locator string `json:"locator,omitempty"`
}

type Event struct {
	Seq           int64     `json:"seq"`
	At            time.Time `json:"at"`
	Kind          string    `json:"kind"`
	RunID         string    `json:"run_id,omitempty"`
	Delta         string    `json:"delta,omitempty"`
	Tool          string    `json:"tool,omitempty"`
	CallID        string    `json:"call_id,omitempty"`
	Approval      *Approval `json:"approval,omitempty"`
	ApprovalNonce string    `json:"approval_nonce,omitempty"`
	Error         *Error    `json:"error,omitempty"`
	Args          string    `json:"args,omitempty"`
	Output        string    `json:"output,omitempty"`
	Duration      int64     `json:"duration_ms,omitempty"`
}

type Replay struct {
	SessionID string  `json:"session_id"`
	After     int64   `json:"after"`
	Events    []Event `json:"events"`
}

type TurnResponse struct {
	SessionID string     `json:"session_id"`
	Assistant TurnResult `json:"assistant"`
}

type TurnResult struct {
	Content   string     `json:"content"`
	Citations []Citation `json:"citations,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Events    []Event    `json:"events,omitempty"`
	Duration  int64      `json:"duration_ms,omitempty"`
}

type Approval struct {
	ID         string    `json:"id"`
	ProposalID string    `json:"proposal_id"`
	RunID      string    `json:"run_id"`
	Type       string    `json:"type"`
	Target     string    `json:"target"`
	Effect     string    `json:"effect"`
	Risk       string    `json:"risk"`
	State      string    `json:"state"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type TriggerKind string

const (
	TriggerUser     TriggerKind = "user"
	TriggerIncident TriggerKind = "incident"
	TriggerSchedule TriggerKind = "schedule"
	TriggerCI       TriggerKind = "ci"
	TriggerMCP      TriggerKind = "mcp"
)

type RunRef struct {
	RunID string `json:"run_id"`
}
