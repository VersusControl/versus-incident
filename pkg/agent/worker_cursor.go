package agent

import (
	"context"
	"log"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// readRange is the cursor range one pull covered: the cursor the read started
// from and the cursor it advanced to.
type readRange struct{ since, cursor time.Time }

type readRangeKey struct{}

// withReadRange attaches the pull's cursor range to ctx for the learner's
// Group, which has every line of the batch and measures per-pattern spans.
func withReadRange(ctx context.Context, since, cursor time.Time) context.Context {
	return context.WithValue(ctx, readRangeKey{}, readRange{since: since, cursor: cursor})
}

// readRangeFrom returns the pull's cursor range, if the worker attached one.
func readRangeFrom(ctx context.Context) (readRange, bool) {
	r, ok := ctx.Value(readRangeKey{}).(readRange)
	return r, ok
}

// readSpan is the source time a set of lines spans: from the earliest to the
// latest timestamp, clipped to [since, newCursor] so a straggler older than
// the cursor cannot stretch it. The log brain measures it per pattern, on that
// pattern's own lines, so an unrelated old line in the batch, or a quiet gap
// before a burst, never dilutes the burst's rate. Lines without a timestamp
// are ignored; fewer than two timestamps, or a cursor that did not move, give
// zero (unknown: rate per poll).
func readSpan(signals []core.Signal, since, newCursor time.Time) time.Duration {
	var lo, hi time.Time
	for _, sig := range signals {
		ts := sig.Timestamp
		if ts.IsZero() {
			continue
		}
		if lo.IsZero() || ts.Before(lo) {
			lo = ts
		}
		if hi.IsZero() || ts.After(hi) {
			hi = ts
		}
	}
	if lo.IsZero() || !newCursor.After(since) {
		return 0
	}
	if lo.Before(since) {
		lo = since
	}
	if hi.After(newCursor) {
		hi = newCursor
	}
	if !hi.After(lo) {
		return 0
	}
	return hi.Sub(lo)
}

// -----------------------------------------------------------------------------
// cursor helpers
// -----------------------------------------------------------------------------

func (w *Worker) loadCursor(ctx context.Context, name string) time.Time {
	now := time.Now().UTC()
	if w.cursors != nil {
		if t, ok := w.cursors.Get(ctx, name); ok {
			// Never start a tick from a cursor ahead of the wall clock. A cursor
			// persisted before the tailing sources bounded their scan at `now`
			// could be poisoned into the future by a single future-dated
			// document (observed live: docs dated 2048). Left as-is it makes
			// every `>= cursor` query match nothing until that time arrives —
			// the source silently stops learning. Treating a future cursor like
			// no cursor at all heals it in one tick: we resume from the lookback
			// window and the source (now `lte: now`-bounded) advances normally.
			if !t.After(now) {
				return t
			}
			log.Printf("agent: cursor for %s is in the future (%s); resetting to lookback window", name, t.Format(time.RFC3339))
		}
	}
	return now.Add(-w.lookback)
}

func (w *Worker) saveCursor(ctx context.Context, name string, t time.Time) {
	if w.cursors == nil {
		return
	}
	if err := w.cursors.Set(ctx, name, t); err != nil {
		log.Printf("agent: failed to persist cursor for %s: %v", name, err)
	}
}
