package agent

import (
	"context"
	"testing"
	"time"
)

// TestCursorStore_Reset proves that Reset forgets every recorded cursor so a
// subsequent Get reports "no cursor" — which drives loadCursor back to the
// lookback window. This is the seam that lets an operator wipe the catalog and
// have the SAME running worker re-read available history, matching a fresh
// process start with empty in-memory cursors.
func TestCursorStore_Reset(t *testing.T) {
	cs := NewCursorStore(nil) // in-memory
	ctx := context.Background()

	now := time.Now().UTC()
	if err := cs.Set(ctx, "es:prod", now); err != nil {
		t.Fatalf("Set es:prod: %v", err)
	}
	if err := cs.Set(ctx, "loki:staging", now.Add(-time.Minute)); err != nil {
		t.Fatalf("Set loki:staging: %v", err)
	}

	if _, ok := cs.Get(ctx, "es:prod"); !ok {
		t.Fatal("es:prod cursor should be recorded before reset")
	}
	if _, ok := cs.Get(ctx, "loki:staging"); !ok {
		t.Fatal("loki:staging cursor should be recorded before reset")
	}

	if err := cs.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if _, ok := cs.Get(ctx, "es:prod"); ok {
		t.Error("es:prod cursor still present after reset; worker will not re-read the window")
	}
	if _, ok := cs.Get(ctx, "loki:staging"); ok {
		t.Error("loki:staging cursor still present after reset; worker will not re-read the window")
	}
}
