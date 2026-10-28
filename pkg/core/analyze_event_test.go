package core

import (
	"context"
	"testing"
	"time"
)

type captureObserver struct{ got []AnalyzeEvent }

func (c *captureObserver) OnAnalyzeEvent(ev AnalyzeEvent) { c.got = append(c.got, ev) }

// TestAnalyzeObserverFrom_BareContext asserts an unwatched run reports no
// observer, which is what makes EmitAnalyzeEvent safe to call unconditionally.
func TestAnalyzeObserverFrom_BareContext(t *testing.T) {
	if obs := AnalyzeObserverFrom(context.Background()); obs != nil {
		t.Fatalf("bare context yielded observer %#v, want nil", obs)
	}
}

// TestWithAnalyzeObserver_NilIsIgnored asserts attaching a nil observer leaves
// the context unwatched rather than storing a typed-nil that later panics.
func TestWithAnalyzeObserver_NilIsIgnored(t *testing.T) {
	ctx := WithAnalyzeObserver(context.Background(), nil)
	if obs := AnalyzeObserverFrom(ctx); obs != nil {
		t.Fatalf("nil observer was stored: %#v", obs)
	}
	// Must not panic.
	EmitAnalyzeEvent(ctx, AnalyzeEvent{Kind: AnalyzeEventRunStarted})
}

// TestEmitAnalyzeEvent_NoObserverIsNoOp asserts the emit helper is inert on an
// unwatched run: an observer attached to one context must not receive events
// emitted on another, and a bare context must not panic.
func TestEmitAnalyzeEvent_NoObserverIsNoOp(t *testing.T) {
	obs := &captureObserver{}
	watched := WithAnalyzeObserver(context.Background(), obs)

	EmitAnalyzeEvent(context.Background(), AnalyzeEvent{Kind: AnalyzeEventRunStarted})
	if len(obs.got) != 0 {
		t.Fatalf("observer received %d events from an unwatched context", len(obs.got))
	}

	// The same observer still works on its own context, proving the emit
	// above was silent because of the context, not a broken observer.
	EmitAnalyzeEvent(watched, AnalyzeEvent{Kind: AnalyzeEventRunStarted})
	if len(obs.got) != 1 {
		t.Fatalf("watched context delivered %d events, want 1", len(obs.got))
	}
}

func TestEmitAnalyzeEvent_DeliversAndStampsAt(t *testing.T) {
	obs := &captureObserver{}
	ctx := WithAnalyzeObserver(context.Background(), obs)

	before := time.Now().UTC()
	EmitAnalyzeEvent(ctx, AnalyzeEvent{Seq: 7, Kind: AnalyzeEventToolStarted, Tool: "logs"})
	after := time.Now().UTC()

	if len(obs.got) != 1 {
		t.Fatalf("got %d events, want 1", len(obs.got))
	}
	ev := obs.got[0]
	if ev.Seq != 7 || ev.Kind != AnalyzeEventToolStarted || ev.Tool != "logs" {
		t.Fatalf("event mangled in transit: %+v", ev)
	}
	if ev.At.IsZero() {
		t.Fatalf("At was left zero; the client has no timestamp to order by")
	}
	if ev.At.Before(before) || ev.At.After(after) {
		t.Fatalf("At = %s, want within [%s, %s]", ev.At, before, after)
	}
}

// TestEmitAnalyzeEvent_KeepsCallerTimestamp asserts a caller-supplied At wins,
// so an event timed at the moment it happened is not re-stamped at delivery.
func TestEmitAnalyzeEvent_KeepsCallerTimestamp(t *testing.T) {
	obs := &captureObserver{}
	ctx := WithAnalyzeObserver(context.Background(), obs)

	want := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	EmitAnalyzeEvent(ctx, AnalyzeEvent{Kind: AnalyzeEventRunFinished, At: want})

	if len(obs.got) != 1 {
		t.Fatalf("got %d events, want 1", len(obs.got))
	}
	if !obs.got[0].At.Equal(want) {
		t.Fatalf("At = %s, want %s", obs.got[0].At, want)
	}
}
