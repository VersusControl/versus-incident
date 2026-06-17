package agent

import (
	"sort"
	"sync"
	"time"
)

// SourceHealth is a point-in-time snapshot of one source's breaker, returned
// by GET /api/agent/sources/health.
type SourceHealth struct {
	Name                string     `json:"name"`
	State               string     `json:"state"` // "ok" | "backing_off" | "paused"
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastError           string     `json:"last_error,omitempty"`
	LastErrorAt         *time.Time `json:"last_error_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	NextEligibleAt      *time.Time `json:"next_eligible_at,omitempty"`
	Paused              bool       `json:"paused"`
}

type srcHealth struct {
	failures      int
	lastErr       string
	lastErrAt     time.Time
	lastSuccessAt time.Time
	nextEligible  time.Time
	paused        bool
}

// HealthTracker is a per-source circuit breaker. A source that keeps failing
// is pulled progressively less often (exponential backoff, base·2^(n-1)
// capped at backoffMax) instead of being hammered every tick, and recovers
// on its own once a pull succeeds — no restart required. State is exposed so
// operators can see why a source went quiet, and pause/resume it manually.
// Safe for concurrent use.
type HealthTracker struct {
	mu         sync.Mutex
	base       time.Duration // first backoff step (≈ the poll interval)
	backoffMax time.Duration
	sources    map[string]*srcHealth
}

// NewHealthTracker builds a tracker. base defaults to 30s, backoffMax to 15m.
func NewHealthTracker(base, backoffMax time.Duration) *HealthTracker {
	if base <= 0 {
		base = 30 * time.Second
	}
	if backoffMax <= 0 {
		backoffMax = 15 * time.Minute
	}
	return &HealthTracker{base: base, backoffMax: backoffMax, sources: make(map[string]*srcHealth)}
}

// caller holds h.mu.
func (h *HealthTracker) get(name string) *srcHealth {
	s := h.sources[name]
	if s == nil {
		s = &srcHealth{}
		h.sources[name] = s
	}
	return s
}

// Allow reports whether the source may be pulled now: not paused and past
// its next-eligible time. A healthy source is always allowed.
func (h *HealthTracker) Allow(name string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(name)
	if s.paused {
		return false
	}
	return !now.Before(s.nextEligible)
}

// RecordSuccess clears the breaker for a source.
func (h *HealthTracker) RecordSuccess(name string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(name)
	s.failures = 0
	s.lastErr = ""
	s.lastSuccessAt = now
	s.nextEligible = time.Time{}
}

// RecordFailure increments the failure count and pushes next-eligible out by
// an exponential backoff capped at backoffMax.
func (h *HealthTracker) RecordFailure(name string, now time.Time, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(name)
	s.failures++
	if err != nil {
		s.lastErr = err.Error()
	}
	s.lastErrAt = now

	shift := s.failures - 1
	if shift > 20 {
		shift = 20 // cap before the shift to avoid int64 overflow
	}
	backoff := h.base << uint(shift)
	if backoff <= 0 || backoff > h.backoffMax {
		backoff = h.backoffMax
	}
	s.nextEligible = now.Add(backoff)
}

// Register seeds a source so it appears in Snapshot from boot (before its
// first pull) and is addressable by Pause/Resume. Idempotent. The worker
// calls this for every configured source at startup, keyed by the same
// name (SignalSource.Name()) used by the breaker and the cursor store.
func (h *HealthTracker) Register(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.get(name)
}

// Pause holds a known source open until Resume. Returns false when the name
// is unknown — Pause does NOT create an entry, so a typo'd name can't
// conjure a phantom source into the health view. Use the name exactly as
// shown by Snapshot (SignalSource.Name(), e.g. "file:app-file").
func (h *HealthTracker) Pause(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sources[name]
	if !ok {
		return false
	}
	s.paused = true
	return true
}

// Resume clears a manual pause and resets the breaker so the next tick
// pulls. Returns false when the name is unknown.
func (h *HealthTracker) Resume(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sources[name]
	if !ok {
		return false
	}
	s.paused = false
	s.failures = 0
	s.nextEligible = time.Time{}
	return true
}

// Snapshot returns the health of every known source, sorted by name.
func (h *HealthTracker) Snapshot(now time.Time) []SourceHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]SourceHealth, 0, len(h.sources))
	for name, s := range h.sources {
		sh := SourceHealth{
			Name:                name,
			ConsecutiveFailures: s.failures,
			LastError:           s.lastErr,
			Paused:              s.paused,
		}
		switch {
		case s.paused:
			sh.State = "paused"
		case now.Before(s.nextEligible):
			sh.State = "backing_off"
		default:
			sh.State = "ok"
		}
		if !s.lastErrAt.IsZero() {
			t := s.lastErrAt
			sh.LastErrorAt = &t
		}
		if !s.lastSuccessAt.IsZero() {
			t := s.lastSuccessAt
			sh.LastSuccessAt = &t
		}
		if now.Before(s.nextEligible) {
			t := s.nextEligible
			sh.NextEligibleAt = &t
		}
		out = append(out, sh)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
