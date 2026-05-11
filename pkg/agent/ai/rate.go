package ai

import (
	"sync"
	"time"
)

// RateLimiter caps the number of AI calls per rolling hour. Pure
// in-memory: a process restart resets the window, which is acceptable
// because the goal is to bound cost per running process — not to enforce
// a quota across a fleet (we run one agent worker per replica).
//
// max <= 0 disables the limit (Allow always returns true).
type RateLimiter struct {
	max int

	mu        sync.Mutex
	bucket    string // current "yyyymmddhh" key
	hourCount int
}

// NewRateLimiter constructs a limiter with the configured per-hour cap.
func NewRateLimiter(maxPerHour int) *RateLimiter {
	return &RateLimiter{max: maxPerHour}
}

// Allow returns true and records a use when the caller is under the
// per-hour cap, false otherwise. Thread-safe.
func (r *RateLimiter) Allow() bool {
	if r == nil || r.max <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := bucketKey(time.Now())
	if now != r.bucket {
		r.bucket = now
		r.hourCount = 0
	}
	if r.hourCount >= r.max {
		return false
	}
	r.hourCount++
	return true
}

// Stats returns the current hour bucket and how many calls have been
// allowed in it. Used by the admin status endpoint.
func (r *RateLimiter) Stats() (bucket string, used, max int) {
	if r == nil {
		return "", 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bucket, r.hourCount, r.max
}

func bucketKey(t time.Time) string {
	return t.UTC().Format("2006010215")
}
