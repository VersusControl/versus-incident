package ai

import (
	"errors"
	"testing"
	"time"
)

func TestBreaker_DisabledAlwaysAllows(t *testing.T) {
	b := NewBreaker(0, time.Minute, 0)
	for i := 0; i < 100; i++ {
		if !b.Allow() {
			t.Fatal("threshold=0 must allow every call")
		}
		b.RecordFailure(errors.New("nope"))
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	b := NewBreaker(3, time.Minute, 0)

	// 3 failures → open.
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("attempt %d should be allowed before threshold trips", i)
		}
		b.RecordFailure(errors.New("boom"))
	}
	if got := b.Stats().State; got != "open" {
		t.Fatalf("state=%q want open", got)
	}
	if b.Allow() {
		t.Fatal("Allow must reject when open and within cooldown")
	}
}

func TestBreaker_HalfOpenProbeSuccessCloses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := NewBreaker(1, 10*time.Second, 0)
	b.clock = func() time.Time { return now }

	// Trip the breaker.
	b.Allow()
	b.RecordFailure(errors.New("boom"))

	// Inside cooldown — denied.
	if b.Allow() {
		t.Fatal("breaker should reject while in cooldown")
	}

	// Cooldown elapses → exactly one probe is allowed.
	b.clock = func() time.Time { return now.Add(11 * time.Second) }
	if !b.Allow() {
		t.Fatal("first Allow after cooldown should hand out a probe")
	}
	if b.Allow() {
		t.Fatal("second concurrent Allow during probe must be denied")
	}

	// Probe succeeds → breaker closes, subsequent calls flow.
	b.RecordSuccess(50 * time.Millisecond)
	if got := b.Stats().State; got != "closed" {
		t.Fatalf("state=%q want closed", got)
	}
	if !b.Allow() {
		t.Fatal("breaker should be closed after successful probe")
	}
}

func TestBreaker_HalfOpenProbeFailureReopens(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := NewBreaker(1, 10*time.Second, 0)
	b.clock = func() time.Time { return now }

	b.Allow()
	b.RecordFailure(errors.New("boom"))

	// Cooldown elapses, probe is granted, but probe fails.
	b.clock = func() time.Time { return now.Add(11 * time.Second) }
	if !b.Allow() {
		t.Fatal("probe should be granted")
	}
	b.RecordFailure(errors.New("still down"))

	if got := b.Stats().State; got != "open" {
		t.Fatalf("after probe failure state=%q want open", got)
	}
	// Cooldown re-starts: more calls within cooldown are denied.
	b.clock = func() time.Time { return now.Add(15 * time.Second) }
	if b.Allow() {
		t.Fatal("breaker should reject after probe failure, within new cooldown")
	}
}

func TestBreaker_SuccessResetsConsecutiveCount(t *testing.T) {
	b := NewBreaker(3, time.Minute, 0)
	b.Allow()
	b.RecordFailure(errors.New("a"))
	b.Allow()
	b.RecordFailure(errors.New("b"))
	b.Allow()
	b.RecordSuccess(10 * time.Millisecond)
	// 2 failures + success → counter reset; 2 more failures should not open.
	for i := 0; i < 2; i++ {
		b.Allow()
		b.RecordFailure(errors.New("c"))
	}
	if got := b.Stats().State; got != "closed" {
		t.Fatalf("state=%q want closed", got)
	}
}

func TestBreaker_LatencyPercentiles(t *testing.T) {
	b := NewBreaker(5, time.Minute, 100)
	for ms := int64(1); ms <= 100; ms++ {
		b.Allow()
		b.RecordSuccess(time.Duration(ms) * time.Millisecond)
	}
	st := b.Stats()
	// 100-element buffer, p50 ≈ value at idx 49 = 50ms.
	if st.LatencyP50Ms != 50 {
		t.Fatalf("p50=%d want 50", st.LatencyP50Ms)
	}
	if st.LatencyP95Ms != 95 {
		t.Fatalf("p95=%d want 95", st.LatencyP95Ms)
	}
}

func TestBreaker_StatsCountersAccumulate(t *testing.T) {
	b := NewBreaker(2, time.Minute, 0)
	b.Allow()
	b.RecordSuccess(0)
	b.Allow()
	b.RecordFailure(errors.New("x"))
	b.Allow()
	b.RecordFailure(errors.New("y")) // trips
	st := b.Stats()
	if st.TotalSuccess != 1 || st.TotalFailure != 2 || st.TotalOpens != 1 {
		t.Fatalf("counters off: %+v", st)
	}
	if st.LastError != "y" {
		t.Fatalf("last_error=%q want y", st.LastError)
	}
}
