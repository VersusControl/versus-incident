package agent

import (
	"context"
	"testing"
	"time"
)

// TestLoadCursor_HealsFutureCursor proves the worker never starts a tick from a
// cursor ahead of the wall clock. A cursor persisted by a pre-fix build could
// be poisoned into the future by a single future-dated document (observed live:
// docs dated 2048); left as-is, every `>= cursor` query matches nothing until
// that time arrives and the source silently stops learning. loadCursor treats a
// future cursor like no cursor at all — resuming from the lookback window — so
// the source (now `lte: now`-bounded) recovers on the next tick without a clear.
func TestLoadCursor_HealsFutureCursor(t *testing.T) {
	cs := NewCursorStore(nil) // in-memory
	ctx := context.Background()
	lookback := 5 * time.Minute
	w := &Worker{cursors: cs, lookback: lookback}

	// A healthy past cursor is returned verbatim.
	past := time.Now().UTC().Add(-2 * time.Minute)
	if err := cs.Set(ctx, "es:healthy", past); err != nil {
		t.Fatalf("set healthy: %v", err)
	}
	if got := w.loadCursor(ctx, "es:healthy"); !got.Equal(past) {
		t.Errorf("healthy cursor = %v, want %v (unchanged)", got, past)
	}

	// A poisoned future cursor is discarded in favour of the lookback window.
	future := time.Date(2048, 1, 6, 1, 2, 6, 0, time.UTC)
	if err := cs.Set(ctx, "es:poisoned", future); err != nil {
		t.Fatalf("set poisoned: %v", err)
	}
	got := w.loadCursor(ctx, "es:poisoned")
	if got.After(time.Now().UTC()) {
		t.Fatalf("poisoned cursor %v was returned as-is — tick would query an empty future window", got)
	}
	// Should be ~ now - lookback (fresh-start fallback), well before the future.
	wantApprox := time.Now().UTC().Add(-lookback)
	if got.Before(wantApprox.Add(-time.Minute)) || got.After(wantApprox.Add(time.Minute)) {
		t.Errorf("poisoned cursor healed to %v, want ~now-lookback (%v)", got, wantApprox)
	}
}
