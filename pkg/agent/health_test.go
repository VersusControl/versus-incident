package agent

import (
	"errors"
	"testing"
	"time"
)

func TestHealthTracker_BackoffGrowsExponentially(t *testing.T) {
	now := time.Unix(1_000_000_000, 0).UTC()
	h := NewHealthTracker(1*time.Second, 8*time.Second, 2)
	h.clock = func() time.Time { return now }

	cases := []struct {
		failures int
		want     time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 8 * time.Second}, // capped
		{6, 8 * time.Second},
	}
	for _, tc := range cases {
		until := h.RecordFailure("es", errors.New("boom"), 0)
		want := now.Add(tc.want)
		if !until.Equal(want) {
			t.Fatalf("after %d failures: cooldown until %s, want %s", tc.failures, until, want)
		}
	}
}

func TestHealthTracker_ShouldSkipRespectsCooldown(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	h := NewHealthTracker(10*time.Second, 0, 2)
	h.clock = func() time.Time { return now }

	if skip, _ := h.ShouldSkip("es"); skip {
		t.Fatal("brand-new source should not be skipped")
	}
	h.RecordFailure("es", errors.New("boom"), 0)

	skip, until := h.ShouldSkip("es")
	if !skip {
		t.Fatal("should skip during cooldown")
	}
	if !until.Equal(now.Add(10 * time.Second)) {
		t.Fatalf("until=%s want %s", until, now.Add(10*time.Second))
	}

	// Advance past cooldown.
	h.clock = func() time.Time { return now.Add(11 * time.Second) }
	if skip, _ := h.ShouldSkip("es"); skip {
		t.Fatal("should not skip after cooldown elapses")
	}
}

func TestHealthTracker_SuccessResets(t *testing.T) {
	h := NewHealthTracker(1*time.Second, 1*time.Minute, 2)
	h.RecordFailure("es", errors.New("boom"), 0)
	h.RecordFailure("es", errors.New("boom"), 0)
	h.RecordSuccess("es", 42, 3, 5*time.Millisecond)

	snap := h.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len=%d want 1", len(snap))
	}
	s := snap[0]
	if s.ConsecutiveFailures != 0 {
		t.Fatalf("failures not reset: %d", s.ConsecutiveFailures)
	}
	if s.LastError != "" {
		t.Fatalf("last_error not cleared: %q", s.LastError)
	}
	if !s.InCooldownUntil.IsZero() {
		t.Fatal("cooldown not cleared")
	}
	if s.TotalSignalsPulled != 42 || s.TotalSignalsDropped != 3 {
		t.Fatalf("counters off: pulled=%d dropped=%d", s.TotalSignalsPulled, s.TotalSignalsDropped)
	}
	if s.LastPullDurationMs != 5 {
		t.Fatalf("duration ms=%d want 5", s.LastPullDurationMs)
	}
}

func TestHealthTracker_RegisterIsIdempotent(t *testing.T) {
	h := NewHealthTracker(0, 0, 2)
	h.Register("a")
	h.Register("a")
	h.Register("b")
	if got := len(h.Snapshot()); got != 2 {
		t.Fatalf("snapshot len=%d want 2", got)
	}
}

func TestHealthTracker_DisabledBackoffNeverCools(t *testing.T) {
	h := NewHealthTracker(0, 0, 2)
	h.RecordFailure("es", errors.New("boom"), 0)
	if skip, _ := h.ShouldSkip("es"); skip {
		t.Fatal("backoff disabled (initial=0) must never set cooldown")
	}
}

func TestHealthTracker_SnapshotIsDeepCopy(t *testing.T) {
	h := NewHealthTracker(1*time.Second, 0, 2)
	h.RecordFailure("es", errors.New("boom"), 0)
	snap := h.Snapshot()
	snap[0].ConsecutiveFailures = 999
	again := h.Snapshot()
	if again[0].ConsecutiveFailures != 1 {
		t.Fatal("Snapshot should return a copy; mutation leaked into tracker")
	}
}
