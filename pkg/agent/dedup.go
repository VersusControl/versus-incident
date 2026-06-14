package agent

import (
	"context"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// defaultEmitDedupWindow is used when agent.emit_dedup_window is unset.
const defaultEmitDedupWindow = time.Hour

// DedupStore suppresses duplicate incident emissions for the same anomaly
// within a time window. A sustained anomaly re-clusters into the same
// pattern every tick, so without dedup the worker re-emits (and re-notifies,
// and re-spends on the cached AI finding) every poll interval. Redis SETNX
// makes the "first in the window" check atomic across replicas; an
// in-memory map is the fallback when Redis is unavailable (single-replica
// dev / training), mirroring CursorStore's degrade-don't-crash behavior.
type DedupStore struct {
	rdb       *redis.Client
	keyPrefix string
	window    time.Duration

	mu  sync.Mutex
	mem map[string]time.Time // key -> expiry (in-memory fallback)
}

// NewDedupStore parses the configured window — empty falls back to the 1h
// default; "0" disables dedup (every emit allowed). Pass rdb=nil for
// in-memory only. An unparseable window logs nothing and uses the default
// (config validation belongs at load time, not here).
func NewDedupStore(rdb *redis.Client, window string) *DedupStore {
	w := defaultEmitDedupWindow
	if window != "" {
		if d, err := time.ParseDuration(window); err == nil {
			w = d
		}
	}
	return &DedupStore{
		rdb:       rdb,
		keyPrefix: "versus:agent:emit:",
		window:    w,
		mem:       make(map[string]time.Time),
	}
}

// Allow reports whether an emit for key is the first within the window and
// atomically marks it. false means a prior emit is still within the window
// and the caller should suppress this one. A non-positive window disables
// dedup (always true).
func (s *DedupStore) Allow(ctx context.Context, key string) bool {
	if s.window <= 0 {
		return true
	}
	if s.rdb != nil {
		ok, err := s.rdb.SetNX(ctx, s.keyPrefix+key, "1", s.window).Result()
		if err == nil {
			return ok
		}
		// Redis error → fall through to in-memory; never block emission on
		// a transient Redis blip.
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	if exp, ok := s.mem[key]; ok && exp.After(now) {
		return false
	}
	s.mem[key] = now.Add(s.window)
	return true
}

// Release clears the dedup mark for key so the next tick may emit again.
// Called when a send fails — a failed emit must not consume the window.
func (s *DedupStore) Release(ctx context.Context, key string) {
	if s.window <= 0 {
		return
	}
	if s.rdb != nil {
		_ = s.rdb.Del(ctx, s.keyPrefix+key).Err()
	}
	s.mu.Lock()
	delete(s.mem, key)
	s.mu.Unlock()
}

// sweepLocked drops expired in-memory entries so the map stays bounded by
// the number of currently-active patterns. Caller holds s.mu.
func (s *DedupStore) sweepLocked(now time.Time) {
	for k, exp := range s.mem {
		if !exp.After(now) {
			delete(s.mem, k)
		}
	}
}
