package ai

import (
	"sort"
	"sync"
	"time"
)

// BreakerState is the public state of the circuit breaker.
type BreakerState int

const (
	BreakerClosed   BreakerState = iota // calls flow through normally
	BreakerOpen                         // calls short-circuit until cooldown elapses
	BreakerHalfOpen                     // one probe is in flight
)

// String returns the lowercase state name (used in JSON/status output).
func (s BreakerState) String() string {
	switch s {
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// Breaker is a minimal three-state circuit breaker for the AI client.
//
// Closed: every Allow() returns true. A consecutive run of `threshold`
// failures flips state to Open.
//
// Open: Allow() returns false until `cooldown` elapses, then the next
// Allow() returns true once with state HalfOpen — that single call is
// the probe.
//
// HalfOpen: Allow() returns false for every caller except the one
// holding the probe. On success the breaker re-closes (counters
// reset); on failure it re-opens with a fresh cooldown.
//
// threshold == 0 disables the breaker (always-closed).
type Breaker struct {
	threshold int
	cooldown  time.Duration
	clock     func() time.Time // injectable for tests

	mu                  sync.Mutex
	state               BreakerState
	consecutiveFailures int
	openedAt            time.Time
	totalOpens          int
	totalTrips          int
	totalProbes         int
	probeInFlight       bool

	// Rolling latency window of last N successful calls.
	latencies    []int64
	latencyMax   int
	latencyIndex int
	latencyFull  bool

	// Success / failure totals so the admin status can report
	// "X% success rate over the lifetime of the process".
	totalSuccess int64
	totalFailure int64
	lastError    string
	lastErrorAt  time.Time
	lastOKAt     time.Time
}

// NewBreaker constructs a Breaker. cooldown <= 0 falls back to 1
// minute. latencyWindow is the size of the rolling buffer (0 disables
// latency tracking).
func NewBreaker(threshold int, cooldown time.Duration, latencyWindow int) *Breaker {
	if cooldown <= 0 {
		cooldown = time.Minute
	}
	if latencyWindow < 0 {
		latencyWindow = 0
	}
	return &Breaker{
		threshold:  threshold,
		cooldown:   cooldown,
		clock:      time.Now,
		state:      BreakerClosed,
		latencies:  make([]int64, latencyWindow),
		latencyMax: latencyWindow,
	}
}

// Allow returns true when a call may proceed. The caller MUST follow
// with exactly one RecordSuccess or RecordFailure for the call it was
// granted. When threshold == 0 the breaker is permanently closed.
func (b *Breaker) Allow() bool {
	if b == nil || b.threshold == 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerClosed:
		return true
	case BreakerOpen:
		if b.clock().Sub(b.openedAt) < b.cooldown {
			return false
		}
		// Cooldown elapsed: promote to half-open and hand out a probe.
		b.state = BreakerHalfOpen
		b.probeInFlight = true
		b.totalProbes++
		return true
	case BreakerHalfOpen:
		// Only one in-flight probe at a time.
		if b.probeInFlight {
			return false
		}
		b.probeInFlight = true
		return true
	}
	return false
}

// RecordSuccess registers a successful call. dur is the wall-clock
// latency of the call (passed in by the caller because the breaker
// doesn't time the call itself).
func (b *Breaker) RecordSuccess(dur time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.totalSuccess++
	b.lastOKAt = b.clock().UTC()
	b.consecutiveFailures = 0
	if b.state != BreakerClosed {
		b.state = BreakerClosed
	}
	b.probeInFlight = false
	if b.latencyMax > 0 {
		b.latencies[b.latencyIndex] = dur.Milliseconds()
		b.latencyIndex = (b.latencyIndex + 1) % b.latencyMax
		if b.latencyIndex == 0 {
			b.latencyFull = true
		}
	}
}

// RecordFailure registers a failed call. err drives the LastError
// surfaced in stats.
func (b *Breaker) RecordFailure(err error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.totalFailure++
	b.consecutiveFailures++
	b.lastErrorAt = b.clock().UTC()
	if err != nil {
		b.lastError = err.Error()
	}
	switch b.state {
	case BreakerClosed:
		if b.threshold > 0 && b.consecutiveFailures >= b.threshold {
			b.state = BreakerOpen
			b.openedAt = b.clock()
			b.totalOpens++
			b.totalTrips++
		}
	case BreakerHalfOpen:
		// Probe failed: re-open with a fresh cooldown.
		b.state = BreakerOpen
		b.openedAt = b.clock()
		b.totalOpens++
	}
	b.probeInFlight = false
}

// BreakerStats is what the admin status endpoint surfaces.
type BreakerStats struct {
	State               string `json:"state"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	TotalSuccess        int64  `json:"total_success"`
	TotalFailure        int64  `json:"total_failure"`
	TotalOpens          int    `json:"total_opens"`
	TotalProbes         int    `json:"total_probes"`
	LastError           string `json:"last_error,omitempty"`
	LastErrorAt         string `json:"last_error_at,omitempty"`
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	OpenedAt            string `json:"opened_at,omitempty"`
	LatencyP50Ms        int64  `json:"latency_p50_ms"`
	LatencyP95Ms        int64  `json:"latency_p95_ms"`
}

// Stats returns a snapshot suitable for JSON marshalling.
func (b *Breaker) Stats() BreakerStats {
	if b == nil {
		return BreakerStats{State: BreakerClosed.String()}
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	s := BreakerStats{
		State:               b.state.String(),
		ConsecutiveFailures: b.consecutiveFailures,
		TotalSuccess:        b.totalSuccess,
		TotalFailure:        b.totalFailure,
		TotalOpens:          b.totalOpens,
		TotalProbes:         b.totalProbes,
		LastError:           b.lastError,
	}
	if !b.lastErrorAt.IsZero() {
		s.LastErrorAt = b.lastErrorAt.Format(time.RFC3339)
	}
	if !b.lastOKAt.IsZero() {
		s.LastSuccessAt = b.lastOKAt.Format(time.RFC3339)
	}
	if !b.openedAt.IsZero() && b.state == BreakerOpen {
		s.OpenedAt = b.openedAt.Format(time.RFC3339)
	}
	s.LatencyP50Ms, s.LatencyP95Ms = b.percentilesLocked()
	return s
}

// percentilesLocked computes p50/p95 over the rolling window. Caller
// must hold b.mu.
func (b *Breaker) percentilesLocked() (p50, p95 int64) {
	if b.latencyMax == 0 {
		return 0, 0
	}
	n := b.latencyMax
	if !b.latencyFull {
		n = b.latencyIndex
	}
	if n == 0 {
		return 0, 0
	}
	buf := make([]int64, n)
	copy(buf, b.latencies[:n])
	sort.Slice(buf, func(i, j int) bool { return buf[i] < buf[j] })
	return buf[idx(n, 0.50)], buf[idx(n, 0.95)]
}

func idx(n int, q float64) int {
	i := int(float64(n-1) * q)
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return i
}
