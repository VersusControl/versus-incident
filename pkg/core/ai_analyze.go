package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
	"unicode"
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

// Tool is the contract every read-only AI tool satisfies. Tools are registered with an agent at
// construction time; the agent surfaces them to the model as Eino
// ToolInfo and dispatches model-requested calls back to this
// interface.
//
// Implementations MUST be read-only. The compile-time guard in
// pkg/agent/ai/analyze rejects any import of services.CreateIncident
// transitively.
type Tool interface {
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

// AnalyzeTool is retained for source compatibility.
// Deprecated: use Tool.
type AnalyzeTool = Tool

// ToolErrorCode classifies a tool failure without exposing its underlying cause.
type ToolErrorCode string

const (
	ToolErrorInvalidArguments ToolErrorCode = "invalid_arguments"
	ToolErrorTimeout          ToolErrorCode = "timeout"
	ToolErrorCancelled        ToolErrorCode = "cancelled"
	ToolErrorBackend          ToolErrorCode = "backend_error"
	ToolErrorInternal         ToolErrorCode = "internal_error"
)

// ToolError carries a model-safe message while retaining the underlying cause
// for trusted server-side handling. Cause is never part of the model envelope.
type ToolError struct {
	Code    ToolErrorCode
	Message string
	Cause   error
}

// Error implements error without exposing the underlying cause.
func (err *ToolError) Error() string {
	if err == nil {
		return ""
	}
	return err.Message
}

// Unwrap retains errors.Is/errors.As support for trusted server-side callers.
func (err *ToolError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

// NewToolError constructs a classified tool error with a model-safe message.
func NewToolError(code ToolErrorCode, message string, cause error) error {
	return &ToolError{Code: code, Message: message, Cause: cause}
}

// ClassifyToolError returns only model-safe fields. Unknown errors are treated
// as backend failures and their text is deliberately discarded.
func ClassifyToolError(err error) (ToolErrorCode, string) {
	var classified *ToolError
	if errors.As(err, &classified) {
		return classified.Code, classified.Message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ToolErrorTimeout, "tool timed out"
	}
	if errors.Is(err, context.Canceled) {
		return ToolErrorCancelled, "tool run was cancelled"
	}
	return ToolErrorBackend, "tool backend failed"
}

// ToolDisplayer optionally gives a tool a human-readable activity name.
type ToolDisplayer interface {
	DisplayName() string
}

// AnalyzeToolDisplayer is retained for source compatibility.
// Deprecated: use ToolDisplayer.
type AnalyzeToolDisplayer = ToolDisplayer

// ToolDisplayName returns an explicit display name or title-cases the stable name.
func ToolDisplayName(tool Tool) string {
	if tool == nil || (reflect.ValueOf(tool).Kind() == reflect.Ptr && reflect.ValueOf(tool).IsNil()) {
		return ""
	}
	if displayer, ok := tool.(ToolDisplayer); ok {
		if display := strings.TrimSpace(displayer.DisplayName()); display != "" {
			return display
		}
	}
	parts := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(tool.Name()))
	for i, part := range parts {
		runes := []rune(part)
		runes[0] = unicode.ToUpper(runes[0])
		parts[i] = string(runes)
	}
	return strings.Join(parts, " ")
}

// ToolResult is the uniform envelope every AnalyzeTool returns. The
// shape is stable so the model sees a predictable schema across tools;
// the per-tool payload lives in Data as JSON-encodable values.
//
// Tool — the tool name (mirrors AnalyzeTool.Name) so a model parsing
// multiple tool responses can disambiguate without relying on call
// ordering.
//
// Found — optional flag for lookup-style tools (get_pattern,
// get_service) to signal "no such entity" without an error.
// Defaults to true; lookups that miss should set it to false and
// leave Data empty (or populated with just the query echo).
//
// Data — the typed payload. Keys are tool-specific; values must be
// JSON-marshalable (no channels, funcs, or unexported structs).
type ToolResult struct {
	Tool      string         `json:"tool"`
	Found     bool           `json:"found"`
	Available *bool          `json:"-"`
	Reason    string         `json:"reason,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// IsAvailable reports availability, defaulting existing results to true.
func (result ToolResult) IsAvailable() bool {
	return result.Available == nil || *result.Available
}

// UnavailableToolResult returns an expected missing-capability result.
// UnavailableToolResult reports missing capability as a successful result so
// the model can adapt without treating expected configuration as a tool error.
func UnavailableToolResult(tool, reason string) *ToolResult {
	available := false
	return &ToolResult{Tool: tool, Found: false, Available: &available, Reason: reason}
}

// MarshalJSON keeps availability explicit on the wire while preserving old constructors.
func (result ToolResult) MarshalJSON() ([]byte, error) {
	type wireResult struct {
		Tool      string         `json:"tool"`
		Found     bool           `json:"found"`
		Available bool           `json:"available"`
		Reason    string         `json:"reason,omitempty"`
		Data      map[string]any `json:"data,omitempty"`
	}
	return json.Marshal(wireResult{
		Tool: result.Tool, Found: result.Found, Available: result.IsAvailable(),
		Reason: result.Reason, Data: result.Data,
	})
}

// UnmarshalJSON preserves explicit availability when reading stored results.
func (result *ToolResult) UnmarshalJSON(data []byte) error {
	type wireResult struct {
		Tool      string         `json:"tool"`
		Found     bool           `json:"found"`
		Available *bool          `json:"available"`
		Reason    string         `json:"reason,omitempty"`
		Data      map[string]any `json:"data,omitempty"`
	}
	var wire wireResult
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	result.Tool = wire.Tool
	result.Found = wire.Found
	result.Available = wire.Available
	result.Reason = wire.Reason
	result.Data = wire.Data
	return nil
}
