package agent

import (
	"context"
	"testing"
	"time"
)

func TestDedupStore_AllowAndRelease(t *testing.T) {
	ctx := context.Background()
	s := NewDedupStore(nil, "1h")

	if !s.Allow(ctx, "svc:p1") {
		t.Fatal("first Allow should be true")
	}
	if s.Allow(ctx, "svc:p1") {
		t.Error("second Allow within window should be false (deduped)")
	}
	if !s.Allow(ctx, "svc:p2") {
		t.Error("different key should be allowed independently")
	}

	// Release clears the mark so the next emit is allowed again (the failed-
	// send recovery path).
	s.Release(ctx, "svc:p1")
	if !s.Allow(ctx, "svc:p1") {
		t.Error("Allow after Release should be true")
	}
}

func TestDedupStore_WindowExpiry(t *testing.T) {
	ctx := context.Background()
	s := NewDedupStore(nil, "30ms")

	if !s.Allow(ctx, "k") {
		t.Fatal("first Allow should be true")
	}
	if s.Allow(ctx, "k") {
		t.Fatal("within window should be false")
	}
	time.Sleep(45 * time.Millisecond)
	if !s.Allow(ctx, "k") {
		t.Error("after window expiry Allow should be true again")
	}
}

func TestDedupStore_ZeroWindowDisables(t *testing.T) {
	ctx := context.Background()
	s := NewDedupStore(nil, "0")
	for i := 0; i < 3; i++ {
		if !s.Allow(ctx, "k") {
			t.Fatalf("window=0 must never dedup (call %d)", i)
		}
	}
}

func TestNewDedupStore_DefaultsWindow(t *testing.T) {
	if got := NewDedupStore(nil, "").window; got != defaultEmitDedupWindow {
		t.Errorf("empty window = %v, want default %v", got, defaultEmitDedupWindow)
	}
	if got := NewDedupStore(nil, "garbage").window; got != defaultEmitDedupWindow {
		t.Errorf("unparseable window = %v, want default %v", got, defaultEmitDedupWindow)
	}
	if got := NewDedupStore(nil, "15m").window; got != 15*time.Minute {
		t.Errorf("window = %v, want 15m", got)
	}
}
