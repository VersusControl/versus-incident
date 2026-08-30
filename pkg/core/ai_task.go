package core

import (
	"context"
	"time"
)

// AITaskKind identifies what an AIAgent is being asked to do.
//
// The dispatcher uses the kind to pick the right agent, cache, and
// rate limiter. New kinds are added by extending this enum and the
// router's wiring.
type AITaskKind string

const (
	// AITaskDetect is a cheap, tool-free, single-call classification of
	// an unknown or spiking log pattern. The output is an AIFinding
	// emitted as an incident through services.CreateIncidentFromFinding.
	AITaskDetect AITaskKind = "detect"

	// AITaskAnalyze is an operator-triggered, tool-using investigation
	// of a single incident. The output is an AIFinding persisted to the
	// analyses storage blob. Analyze NEVER fans out to notification
	// channels.
	AITaskAnalyze AITaskKind = "analyze"

	// AITaskChat is an operator-triggered conversational DevOps/SRE turn.
	// Its markdown result is persisted in a chat session and is never cached.
	AITaskChat AITaskKind = "chat"
)

// AITask is the input to an AIAgent.Run call. Each concrete task type
// carries the inputs needed by its kind and exposes a CacheKey() the
// router uses for memoisation. An empty CacheKey disables caching for
// that call.
type AITask interface {
	Kind() AITaskKind
	CacheKey() string
}

// DetectTask wraps an AgentResult for detect-mode classification.
type DetectTask struct {
	Result AgentResult
}

// Kind implements AITask.
func (DetectTask) Kind() AITaskKind { return AITaskDetect }

// CacheKey implements AITask. Detect memoisation is keyed by pattern
// id; an empty pattern id (unknown pattern not yet stored) disables
// caching for that call.
func (t DetectTask) CacheKey() string { return t.Result.PatternID }

// AnalyzeTask wraps an on-demand analysis request. The snapshot
// carries the incident payload the agent inspects; richer context
// (related logs, pattern history) is fetched via the agent's
// read-only tools at run time, not pre-loaded here.
type AnalyzeTask struct {
	Snapshot AnalyzeIncidentSnapshot
}

// Kind implements AITask.
func (AnalyzeTask) Kind() AITaskKind { return AITaskAnalyze }

// CacheKey implements AITask. Empty disables caching — operators
// expect a fresh tool walk on every analyze request.
func (t AnalyzeTask) CacheKey() string { return "" }

// ChatTimeRange is an absolute half-open interval attached to a chat turn.
// Natural-language date arithmetic is resolved before the model is called.
type ChatTimeRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// ChatIncidentContext is the bounded, redacted incident context that may be
// attached to a turn. Raw incident payloads are deliberately absent.
type ChatIncidentContext struct {
	ID       string    `json:"id"`
	Title    string    `json:"title,omitempty"`
	Service  string    `json:"service,omitempty"`
	Severity string    `json:"severity,omitempty"`
	Status   string    `json:"status,omitempty"`
	Created  time.Time `json:"created,omitempty"`
}

// ChatAttachment grounds a turn without restricting the conversation to that
// context. Each field is optional and validated by the chat service.
type ChatAttachment struct {
	Incident *ChatIncidentContext `json:"incident,omitempty"`
	Service  string               `json:"service,omitempty"`
	Time     *ChatTimeRange       `json:"time_range,omitempty"`
}

// ChatTask is one user turn in a durable chat session.
type ChatTask struct {
	SessionID  string          `json:"session_id"`
	Message    string          `json:"message"`
	Attachment *ChatAttachment `json:"attachment,omitempty"`
}

// Kind implements AITask.
func (ChatTask) Kind() AITaskKind { return AITaskChat }

// CacheKey implements AITask. Conversational turns are never replayable.
func (ChatTask) CacheKey() string { return "" }

// ChatCitation identifies evidence used by a conversational answer.
type ChatCitation struct {
	Tool    string `json:"tool"`
	Label   string `json:"label,omitempty"`
	Locator string `json:"locator,omitempty"`
}

// ChatTurnResult is the markdown-native result of one chat turn. It is
// intentionally separate from AICallResult so chat is never coerced into an
// incident finding.
type ChatTurnResult struct {
	Markdown   string          `json:"markdown"`
	Citations  []ChatCitation  `json:"citations,omitempty"`
	ToolCalls  []ToolCallTrace `json:"tool_calls,omitempty"`
	DurationMs int64           `json:"duration_ms,omitempty"`
	Model      string          `json:"model,omitempty"`
}

// AIAgent is one concrete model + prompt + (optional) tool wiring,
// dedicated to a single AITaskKind. Implementations live under
// pkg/agent/ai (e.g. detect, analyze).
//
// Run is expected to be self-contained: it owns the model call, but
// the router handles cache / rate-limit / persistence around it.
type AIAgent interface {
	Name() string
	Kind() AITaskKind
	Run(ctx context.Context, task AITask) (*AICallResult, error)
}

// ChatTurnAgent is the narrow execution contract implemented by the ADK chat
// agent. Its streaming, markdown-native result does not fit AIAgent.Run.
type ChatTurnAgent interface {
	Name() string
	Kind() AITaskKind
	RunChatTurn(ctx context.Context, task ChatTask) (*ChatTurnResult, error)
}
