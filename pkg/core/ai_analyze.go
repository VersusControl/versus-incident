package core

import (
	"context"
	"encoding/json"
	"time"
)

// AnalyzeIncidentSnapshot is the input payload handed to the
// analyze-kind AIAgent. It captures the minimal incident metadata the
// agent needs to plan its tool calls; richer context (related logs,
// nearby incidents, pattern history) is fetched on demand via the
// agent's read-only tools, not pre-loaded into the snapshot.
//
// The struct deliberately does not import pkg/storage — `pkg/core`
// must stay leaf-level. Callers (the admin controller, the worker)
// flatten storage records into this shape.
type AnalyzeIncidentSnapshot struct {
	IncidentID string     `json:"incident_id"`
	Title      string     `json:"title,omitempty"`
	Service    string     `json:"service,omitempty"`
	Source     string     `json:"source,omitempty"`
	Severity   string     `json:"severity,omitempty"`
	Resolved   bool       `json:"resolved,omitempty"`
	CreatedAt  time.Time  `json:"created_at,omitempty"`
	AckedAt    *time.Time `json:"acked_at,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`

	// Content is the alert payload (the same map persisted on the
	// incident record). Operators see this verbatim in the UI.
	Content map[string]any `json:"content,omitempty"`

	// RequestedBy identifies the operator that triggered the analysis
	// (gateway-authenticated, so today this is just a free-form label
	// like "admin"). Stored on the AnalysisRecord for audit.
	RequestedBy string `json:"requested_by,omitempty"`
}

// AnalyzeTool is the contract every analyze-side read-only tool
// satisfies. Tools are registered with the analyze agent at
// construction time; the agent surfaces them to the model as Eino
// ToolInfo and dispatches model-requested calls back to this
// interface.
//
// Implementations MUST be read-only. The compile-time guard in
// pkg/agent/ai/analyze rejects any import of services.CreateIncident
// transitively.
type AnalyzeTool interface {
	// Name is the model-visible tool name. Must be a stable identifier
	// (snake_case is the convention).
	Name() string
	// Description is the one-line model-visible doc. The model uses it
	// to decide when to call the tool.
	Description() string
	// ArgsSchema returns a JSON schema (drafted as a generic map) for
	// the tool's argument object. Eino converts this into the model's
	// tool-call schema.
	ArgsSchema() map[string]any
	// Invoke runs the tool with the model-provided JSON args. The
	// returned ToolResult is serialised to JSON and fed back to the
	// model as the tool message. Errors are surfaced to the model as
	// a tool error so it can adapt.
	Invoke(ctx context.Context, args json.RawMessage) (*ToolResult, error)
}

// ToolResult is the uniform envelope every AnalyzeTool returns. The
// shape is stable so the model sees a predictable schema across tools;
// the per-tool payload lives in Data as JSON-encodable values.
//
// Tool — the tool name (mirrors AnalyzeTool.Name) so a model parsing
// multiple tool responses can disambiguate without relying on call
// ordering.
//
// Found — optional flag for lookup-style tools (pattern_history,
// describe_service) to signal "no such entity" without an error.
// Defaults to true; lookups that miss should set it to false and
// leave Data empty (or populated with just the query echo).
//
// Data — the typed payload. Keys are tool-specific; values must be
// JSON-marshalable (no channels, funcs, or unexported structs).
type ToolResult struct {
	Tool  string         `json:"tool"`
	Found bool           `json:"found"`
	Data  map[string]any `json:"data,omitempty"`
}
