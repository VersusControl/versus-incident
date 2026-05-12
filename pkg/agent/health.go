package agent

import (
	"sync"
	"time"
)

// HealthTracker records per-source health and applies exponential
// backoff cooldown after consecutive failures. It is safe for
// concurrent use — the worker fans out tickSource across goroutines.
//
// The tracker is intentionally lightweight: a mutex-guarded map of
// SourceHealth keyed by source name. It is owned by the worker but
// also read by the admin status endpoint, which is why we expose a
// Snapshot method that returns a deep copy.
type HealthTracker struct {
	initial    time.Duration
	max        time.Duration
	multiplier float64
	clock      func() time.Time // injectable for tests

	mu  sync.Mutex
	all map[string]*SourceHealth
}

// SourceHealth captures the rolling state of one signal source.
// All time fields use UTC. zero-value times mean "never".
type SourceHealth struct {
	Name                string
	ConsecutiveFailures int
	LastError           string
	LastErrorAt         time.Time
	LastSuccessAt       time.Time
	InCooldownUntil     time.Time
	TotalPullsOK        int64
	TotalPullsFailed    int64
	TotalSignalsPulled  int64
	TotalSignalsDropped int64 // truncated by batch_max
	LastPullDurationMs  int64
	LastSignalsPulled   int
}

// NewHealthTracker constructs a tracker. When initial <= 0 backoff is
// disabled (cooldown is never set). When max <= 0 cooldown grows
// unbounded.
func NewHealthTracker(initial, max time.Duration, multiplier float64) *HealthTracker {
	if multiplier < 1 {
		multiplier = 2
	}
	return &HealthTracker{
		initial:    initial,
		max:        max,
		multiplier: multiplier,
		clock:      time.Now,
		all:        make(map[string]*SourceHealth),
	}
}

// Register seeds an entry for the given source so it shows up in
// /api/agent/status before its first pull. Idempotent.
func (h *HealthTracker) Register(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.all[name]; !ok {
		h.all[name] = &SourceHealth{Name: name}
	}
}

// ShouldSkip reports whether the named source is currently in cooldown
// and therefore the worker should NOT call Pull this tick.
func (h *HealthTracker) ShouldSkip(name string) (bool, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.all[name]
	if !ok {
		return false, time.Time{}
	}
	if s.InCooldownUntil.IsZero() {
		return false, time.Time{}
	}
	now := h.clock()
	if now.Before(s.InCooldownUntil) {
		return true, s.InCooldownUntil
	}
	return false, time.Time{}
}

// RecordSuccess clears the consecutive failure counter and cooldown.
// pulledN is the number of signals returned (post-cap), droppedN is
// how many were truncated by batch_max in this tick.
func (h *HealthTracker) RecordSuccess(name string, pulledN, droppedN int, dur time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.getOrCreate(name)
	s.ConsecutiveFailures = 0
	s.LastError = ""
	s.InCooldownUntil = time.Time{}
	s.LastSuccessAt = h.clock().UTC()
	s.TotalPullsOK++
	s.TotalSignalsPulled += int64(pulledN)
	s.TotalSignalsDropped += int64(droppedN)
	s.LastSignalsPulled = pulledN
	s.LastPullDurationMs = dur.Milliseconds()
}

// RecordFailure increments the consecutive failure counter and sets
// the cooldown using exponential backoff. Returns the cooldown end
// time (zero if backoff is disabled).
func (h *HealthTracker) RecordFailure(name string, err error, dur time.Duration) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.getOrCreate(name)
	s.ConsecutiveFailures++
	if err != nil {
		s.LastError = err.Error()
	}
	s.LastErrorAt = h.clock().UTC()
	s.TotalPullsFailed++
	s.LastPullDurationMs = dur.Milliseconds()

	if h.initial <= 0 {
		return time.Time{}
	}
	cooldown := h.initial
	for i := 1; i < s.ConsecutiveFailures; i++ {
		cooldown = time.Duration(float64(cooldown) * h.multiplier)
		if h.max > 0 && cooldown > h.max {
			cooldown = h.max
			break
		}
	}
	s.InCooldownUntil = h.clock().Add(cooldown)
	return s.InCooldownUntil
}

// Snapshot returns a deep copy of every tracked source. Order is not
// guaranteed — callers that want a stable order should sort by Name.
func (h *HealthTracker) Snapshot() []SourceHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]SourceHealth, 0, len(h.all))
	for _, s := range h.all {
		out = append(out, *s)
	}
	return out
}

func (h *HealthTracker) getOrCreate(name string) *SourceHealth {
	s, ok := h.all[name]
	if !ok {
		s = &SourceHealth{Name: name}
		h.all[name] = s
	}
	return s
}
