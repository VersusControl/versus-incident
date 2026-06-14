package agent

import (
	"context"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// CursorStore persists the last-seen timestamp for each SignalSource. Redis
// is preferred so cursors survive crashes and replica restarts; the worker
// falls back to in-memory storage when Redis is unavailable so that
// development setups don't require Redis just to try training mode.
type CursorStore struct {
	rdb       *redis.Client
	keyPrefix string

	mu  sync.RWMutex
	mem map[string]time.Time
}

// NewCursorStore returns a CursorStore. Pass `rdb=nil` for in-memory only.
func NewCursorStore(rdb *redis.Client) *CursorStore {
	return &CursorStore{
		rdb:       rdb,
		keyPrefix: "versus:agent:cursor:",
		mem:       make(map[string]time.Time),
	}
}

// Get returns the cursor for a source. The bool is false when no cursor has
// been recorded yet (caller should fall back to AgentConfig.Lookback).
func (s *CursorStore) Get(ctx context.Context, source string) (time.Time, bool) {
	if s.rdb != nil {
		v, err := s.rdb.Get(ctx, s.keyPrefix+source).Result()
		if err == nil {
			if t, perr := time.Parse(time.RFC3339Nano, v); perr == nil {
				return t.UTC(), true
			}
		}
		// On Redis error fall through to mem (don't crash the worker).
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.mem[source]
	return t, ok
}

// Set records a cursor. Redis errors are returned to the caller (the worker
// logs them and continues — mem cache always succeeds).
func (s *CursorStore) Set(ctx context.Context, source string, t time.Time) error {
	s.mu.Lock()
	s.mem[source] = t.UTC()
	s.mu.Unlock()
	if s.rdb == nil {
		return nil
	}
	return s.rdb.Set(ctx, s.keyPrefix+source, t.UTC().Format(time.RFC3339Nano), 0).Err()
}

// Delete drops a source's cursor so the next pull re-backfills from the
// lookback window. Admin use: recover a source stuck behind a cursor that
// points past a log-retention boundary. Redis errors are returned; the
// in-memory entry is always removed.
func (s *CursorStore) Delete(ctx context.Context, source string) error {
	s.mu.Lock()
	delete(s.mem, source)
	s.mu.Unlock()
	if s.rdb == nil {
		return nil
	}
	return s.rdb.Del(ctx, s.keyPrefix+source).Err()
}
