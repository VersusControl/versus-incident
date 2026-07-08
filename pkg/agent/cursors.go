package agent

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// CursorStore persists the last-seen timestamp for each SignalSource. Redis
// is preferred so cursors survive crashes and replica restarts; the worker
// falls back to in-memory storage when Redis is unavailable so that
// development setups don't require Redis just to try training mode.
type CursorStore struct {
	rdb       redis.UniversalClient
	keyPrefix string

	mu  sync.RWMutex
	mem map[string]time.Time
}

// NewCursorStore returns a CursorStore. Pass `rdb=nil` for in-memory only.
func NewCursorStore(rdb redis.UniversalClient) *CursorStore {
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

// Reset forgets every recorded cursor so the next Pull for each source starts
// from the worker's lookback window again (loadCursor falls back to
// now-lookback when no cursor exists). It is the in-place equivalent of a
// fresh process start with empty in-memory cursors: after an operator wipes
// the learned catalog, the SAME running worker re-reads the available history
// and relearns immediately, instead of sitting idle until brand-new-timestamp
// signals arrive because its cursor is still pinned past the consumed window.
//
// The in-memory map is always cleared; when Redis backs the store every
// persisted cursor key under the prefix is deleted too. Redis errors are
// returned so the caller can decide whether the rewind fully succeeded.
func (s *CursorStore) Reset(ctx context.Context) error {
	s.mu.Lock()
	s.mem = make(map[string]time.Time)
	s.mu.Unlock()
	if s.rdb == nil {
		return nil
	}
	// In cluster mode a plain SCAN only walks the node the call routes to,
	// so sweep every master shard; in single-node mode there is exactly one
	// backend and the single deleteCursorKeys pass covers it.
	if cc, ok := s.rdb.(*redis.ClusterClient); ok {
		return cc.ForEachMaster(ctx, func(ctx context.Context, m *redis.Client) error {
			return s.deleteCursorKeys(ctx, m)
		})
	}
	return s.deleteCursorKeys(ctx, s.rdb)
}

// deleteCursorKeys scans one Redis backend for every cursor key under the
// prefix and deletes them ONE AT A TIME. Single-key Del always routes to the
// key's owning slot, so it is correct on a *redis.ClusterClient too — a
// multi-key Del across different hash slots would fail with CROSSSLOT.
func (s *CursorStore) deleteCursorKeys(ctx context.Context, c redis.Cmdable) error {
	var cursor uint64
	for {
		keys, next, err := c.Scan(ctx, cursor, s.keyPrefix+"*", 100).Result()
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := c.Del(ctx, k).Err(); err != nil {
				return err
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return nil
}
