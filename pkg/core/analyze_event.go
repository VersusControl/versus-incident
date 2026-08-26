package core

import (
	"context"
	"time"
)

// Analyze run event kinds. These are the wire values the UI switches on, so
// they are part of the API surface — renaming one breaks a client.
const (
	AnalyzeEventRunStarted    = "run_started"
	AnalyzeEventModelStarted  = "model_started"
	AnalyzeEventModelDelta    = "model_delta"
	AnalyzeEventModelFinished = "model_finished"
	AnalyzeEventToolStarted   = "tool_started"
	AnalyzeEventToolFinished  = "tool_finished"
	AnalyzeEventToolError     = "tool_error"
	AnalyzeEventRunFinished   = "run_finished"
	AnalyzeEventRunFailed     = "run_failed"
)

// AnalyzeEvent is one observable step of an analyze run, emitted live while
// the ReAct loop is still working.
//
// It carries the SAME already-capped, already-redacted strings that end up on
// the persisted ToolCallTrace — a live viewer never sees more than the stored
// record, so streaming opens no new egress path.
type AnalyzeEvent struct {
	Seq         int64     `json:"seq"`
	At          time.Time `json:"at"`
	Kind        string    `json:"kind"`
	Tool        string    `json:"tool,omitempty"`
	ToolDisplay string    `json:"tool_display,omitempty"`
	Args        string    `json:"args,omitempty"`
	Output      string    `json:"output,omitempty"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
	Error       string    `json:"error,omitempty"`
	// Turn is the model round-trip this event belongs to, starting at 1. The
	// ReAct loop alternates model turn -> tool calls -> model turn, so the UI
	// uses it to group tools under the turn that requested them.
	Turn int `json:"turn,omitempty"`
	// AnalysisID is set on the terminal event so the client can fetch the
	// persisted record without guessing which analysis it just watched.
	AnalysisID string `json:"analysis_id,omitempty"`
}

// AnalyzeObserver receives run events as they happen.
//
// Implementations MUST NOT block: an observer is called from inside the
// agent's tool callbacks, so a slow or disconnected consumer would otherwise
// stall the model run itself.
type AnalyzeObserver interface {
	OnAnalyzeEvent(AnalyzeEvent)
}

type analyzeObserverKey struct{}

// WithAnalyzeObserver attaches an observer to ctx. Passing it through the
// context rather than the task keeps AnalyzeTask a plain serialisable value.
func WithAnalyzeObserver(ctx context.Context, obs AnalyzeObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, analyzeObserverKey{}, obs)
}

// AnalyzeObserverFrom returns the observer on ctx, or nil when the run is not
// being watched. Callers emit unconditionally via EmitAnalyzeEvent instead of
// nil-checking at every call site.
func AnalyzeObserverFrom(ctx context.Context) AnalyzeObserver {
	obs, _ := ctx.Value(analyzeObserverKey{}).(AnalyzeObserver)
	return obs
}

// EmitAnalyzeEvent delivers ev to the observer on ctx, if any. It stamps At
// when the caller left it zero.
func EmitAnalyzeEvent(ctx context.Context, ev AnalyzeEvent) {
	obs := AnalyzeObserverFrom(ctx)
	if obs == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	obs.OnAnalyzeEvent(ev)
}
