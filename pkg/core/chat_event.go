package core

import (
	"context"
	"time"
)

const (
	ChatEventRunStarted   = "run_started"
	ChatEventModelDelta   = "model_delta"
	ChatEventToolStarted  = "tool_started"
	ChatEventToolFinished = "tool_finished"
	ChatEventCompacted    = "compacted"
	ChatEventRunFinished  = "run_finished"
	ChatEventRunFailed    = "run_failed"
	ChatEventRunCancelled = "run_cancelled"
)

// ChatEvent is one observable step of a chat turn. Error contains only a safe
// classification; backend and model errors must never cross this boundary.
type ChatEvent struct {
	Seq         int64          `json:"seq"`
	At          time.Time      `json:"at"`
	Kind        string         `json:"kind"`
	Delta       string         `json:"delta,omitempty"`
	Tool        string         `json:"tool,omitempty"`
	CallID      string         `json:"call_id,omitempty"`
	ToolDisplay string         `json:"tool_display,omitempty"`
	Args        string         `json:"args,omitempty"`
	Output      string         `json:"output,omitempty"`
	DurationMs  int64          `json:"duration_ms,omitempty"`
	Error       string         `json:"error,omitempty"`
	Citations   []ChatCitation `json:"citations,omitempty"`
}

// ChatObserver receives events synchronously. Implementations must return
// immediately, normally by using a bounded non-blocking channel send.
type ChatObserver interface {
	OnChatEvent(ChatEvent)
}

type chatObserverKey struct{}

// WithChatObserver attaches an observer to a chat run context.
func WithChatObserver(ctx context.Context, observer ChatObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, chatObserverKey{}, observer)
}

// ChatObserverFrom returns the observer attached to ctx, if any.
func ChatObserverFrom(ctx context.Context) ChatObserver {
	observer, _ := ctx.Value(chatObserverKey{}).(ChatObserver)
	return observer
}

// EmitChatEvent stamps and delivers an event when an observer is attached.
func EmitChatEvent(ctx context.Context, event ChatEvent) {
	observer := ChatObserverFrom(ctx)
	if observer == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	observer.OnChatEvent(event)
}
