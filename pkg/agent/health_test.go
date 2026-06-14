package agent

import (
	"errors"
	"testing"
	"time"
)

func TestHealthTracker_BreakerBackoff(t *testing.T) {
	h := NewHealthTracker(time.Minute, 15*time.Minute)
	t0 := time.Now()

	if !h.Allow("s", t0) {
		t.Fatal("a fresh source should be allowed")
	}

	h.RecordFailure("s", t0, errors.New("boom")) // 1st failure → 1m backoff
	if h.Allow("s", t0.Add(59*time.Second)) {
		t.Error("should back off within the window after a failure")
	}
	if !h.Allow("s", t0.Add(61*time.Second)) {
		t.Error("should be eligible once the backoff window passes")
	}

	h.RecordFailure("s", t0, errors.New("boom")) // 2nd failure → 2m backoff
	if h.Allow("s", t0.Add(119*time.Second)) {
		t.Error("second consecutive failure should double the backoff")
	}
	if !h.Allow("s", t0.Add(121*time.Second)) {
		t.Error("should be eligible after the 2m window")
	}

	h.RecordSuccess("s", t0)
	if !h.Allow("s", t0) {
		t.Error("a success must reset the breaker")
	}
}

func TestHealthTracker_BackoffCap(t *testing.T) {
	h := NewHealthTracker(time.Minute, 5*time.Minute)
	t0 := time.Now()
	for i := 0; i < 12; i++ {
		h.RecordFailure("s", t0, errors.New("x"))
	}
	if h.Allow("s", t0.Add(4*time.Minute+59*time.Second)) {
		t.Error("backoff should still hold just under the cap")
	}
	if !h.Allow("s", t0.Add(5*time.Minute+time.Second)) {
		t.Error("backoff must be capped at backoffMax (5m)")
	}
}

func TestHealthTracker_PauseResume(t *testing.T) {
	h := NewHealthTracker(time.Minute, time.Hour)
	t0 := time.Now()
	h.Register("s")

	if !h.Pause("s") {
		t.Error("Pause of a known source should return true")
	}
	if h.Allow("s", t0) {
		t.Error("a paused source must not be allowed")
	}
	if !h.Resume("s") {
		t.Error("Resume of a known source should return true")
	}
	if !h.Allow("s", t0) {
		t.Error("a resumed source should be allowed again")
	}

	// Unknown names are rejected, not auto-created (no phantom sources).
	if h.Pause("typo") {
		t.Error("Pause of an unknown source should return false")
	}
	for _, s := range h.Snapshot(t0) {
		if s.Name == "typo" {
			t.Error("a rejected Pause must not create a phantom source")
		}
	}
}

func TestHealthTracker_Snapshot(t *testing.T) {
	h := NewHealthTracker(time.Minute, time.Hour)
	t0 := time.Now()
	h.RecordSuccess("ok", t0)
	h.RecordFailure("bad", t0, errors.New("down"))
	h.Register("held")
	h.Pause("held")

	states := map[string]string{}
	for _, s := range h.Snapshot(t0) {
		states[s.Name] = s.State
	}
	if states["ok"] != "ok" {
		t.Errorf("ok.State = %q, want ok", states["ok"])
	}
	if states["bad"] != "backing_off" {
		t.Errorf("bad.State = %q, want backing_off", states["bad"])
	}
	if states["held"] != "paused" {
		t.Errorf("held.State = %q, want paused", states["held"])
	}
}
