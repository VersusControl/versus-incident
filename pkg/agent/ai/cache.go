package ai

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// ResultCache memoises AI findings by pattern_id so a recurring pattern
// inside one cache window does not pay for a repeat LLM call.
//
// State is held in-memory and (best effort) mirrored to storage.Provider
// under config.AICacheBlobName so it survives restarts. A storage write
// failure logs but does not block the caller — the cache still works for
// the lifetime of the process.
type ResultCache struct {
	ttl   time.Duration
	store storage.Provider

	mu      sync.RWMutex
	entries map[string]cacheEntry
	dirty   bool
}

type cacheEntry struct {
	Finding   core.AIFinding `json:"finding"`
	StoredAt  time.Time      `json:"stored_at"`
	PatternID string         `json:"pattern_id"`
}

// cacheBlob is the on-disk representation. Versioned so we can evolve
// the entry shape later.
type cacheBlob struct {
	Version int                   `json:"version"`
	Entries map[string]cacheEntry `json:"entries"`
}

// NewResultCache builds a cache with the configured TTL. ttl <= 0
// disables caching (Get always misses, Put is a no-op).
//
// store may be nil — in that case the cache is in-memory only.
func NewResultCache(ttl time.Duration, store storage.Provider) *ResultCache {
	c := &ResultCache{
		ttl:     ttl,
		store:   store,
		entries: make(map[string]cacheEntry),
	}
	c.load()
	return c
}

// Get returns a cached finding when one exists and has not expired.
func (c *ResultCache) Get(patternID string) (*core.AIFinding, bool) {
	if c == nil || c.ttl <= 0 || patternID == "" {
		return nil, false
	}
	c.mu.RLock()
	e, ok := c.entries[patternID]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(e.StoredAt) > c.ttl {
		// Expired — drop lazily on the next put / persist.
		return nil, false
	}
	f := e.Finding // value copy so callers can mutate without racing the cache
	return &f, true
}

// Put inserts or replaces a finding for patternID. Safe to call with a
// nil receiver or zero-TTL cache (becomes a no-op).
func (c *ResultCache) Put(patternID string, f *core.AIFinding) {
	if c == nil || c.ttl <= 0 || patternID == "" || f == nil {
		return
	}
	c.mu.Lock()
	c.entries[patternID] = cacheEntry{
		Finding:   *f,
		StoredAt:  time.Now().UTC(),
		PatternID: patternID,
	}
	c.dirty = true
	c.mu.Unlock()
}

// Flush wipes every entry and persists. Used by the admin endpoint.
func (c *ResultCache) Flush() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = make(map[string]cacheEntry)
	c.dirty = true
	c.mu.Unlock()
	_ = c.Persist()
}

// Len returns the number of live entries (including expired-but-not-evicted).
func (c *ResultCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Persist writes the cache to storage. No-op when store is nil or the
// cache hasn't changed since the last persist.
func (c *ResultCache) Persist() error {
	if c == nil || c.store == nil {
		return nil
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	// Drop expired entries before writing so the blob doesn't grow
	// unbounded with stale data.
	if c.ttl > 0 {
		cutoff := time.Now().Add(-c.ttl)
		for id, e := range c.entries {
			if e.StoredAt.Before(cutoff) {
				delete(c.entries, id)
			}
		}
	}
	blob := cacheBlob{Version: 1, Entries: c.entries}
	c.dirty = false
	c.mu.Unlock()

	data, err := json.Marshal(blob)
	if err != nil {
		return err
	}
	return c.store.WriteBlob(config.AICacheBlobName, data)
}

func (c *ResultCache) load() {
	if c.store == nil {
		return
	}
	data, err := c.store.ReadBlob(config.AICacheBlobName)
	if err != nil || len(data) == 0 {
		return
	}
	var blob cacheBlob
	if err := json.Unmarshal(data, &blob); err != nil {
		return
	}
	if blob.Entries == nil {
		return
	}
	cutoff := time.Now().Add(-c.ttl)
	for id, e := range blob.Entries {
		if c.ttl > 0 && e.StoredAt.Before(cutoff) {
			continue
		}
		c.entries[id] = e
	}
}
