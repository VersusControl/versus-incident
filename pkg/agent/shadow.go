package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ShadowEvent is one "we would have alerted on this" record produced by the
// worker while running in shadow mode. Events are coalesced per
// (source, pattern_id): repeat hits update Count / LastSeen / Occurrences
// rather than appending new rows, so the on-disk log stays small and
// operator-reviewable.
type ShadowEvent struct {
	PatternID     string    `json:"pattern_id"`
	Template      string    `json:"template"`
	Source        string    `json:"source"`
	RuleName      string    `json:"rule_name,omitempty"`
	Verdict       string    `json:"verdict"` // "unknown" | "spike"
	SampleMessage string    `json:"sample_message"`
	Count         int       `json:"count"`       // total signals across all ticks
	Occurrences   int       `json:"occurrences"` // number of ticks that flagged this
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
}

// ShadowLog is the in-memory + on-disk store of shadow-mode verdicts.
//
// All public methods are safe for concurrent use. Disk persistence is
// debounced — Record sets a dirty flag that the worker flushes at most once
// per `persist_interval`.
type ShadowLog struct {
	mu     sync.RWMutex
	path   string
	events map[string]*ShadowEvent // key = source + "\x00" + pattern_id
	max    int
	dirty  bool
}

// shadowFile is the on-disk schema. Versioned for future evolution.
type shadowFile struct {
	Version   int            `json:"version"`
	UpdatedAt time.Time      `json:"updated_at"`
	Events    []*ShadowEvent `json:"events"`
}

const (
	shadowFileVersion = 1
	// shadowDefaultMax bounds the number of distinct (source, pattern)
	// events kept in the log. When exceeded, the oldest by LastSeen is
	// evicted on the next Record. 1000 is comfortably large for review
	// while keeping the JSON file under a few hundred KB.
	shadowDefaultMax = 1000
	// shadowSampleMaxBytes bounds the SampleMessage field so a giant log
	// line can't blow up the shadow file.
	shadowSampleMaxBytes = 512
)

// LoadShadowLog opens an existing shadow log at `path` or returns an empty
// one when no file is present. `max` caps distinct events; pass 0 for the
// default. An empty path disables persistence (in-memory only).
func LoadShadowLog(path string, max int) (*ShadowLog, error) {
	if max <= 0 {
		max = shadowDefaultMax
	}
	s := &ShadowLog{
		path:   path,
		events: make(map[string]*ShadowEvent),
		max:    max,
	}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read shadow log: %w", err)
	}
	var f shadowFile
	if err := json.Unmarshal(data, &f); err != nil {
		// Don't hard-fail on a corrupt shadow log: it's a debug artifact, not
		// the source of truth. Start fresh and let the worker rebuild it.
		return s, fmt.Errorf("parse shadow log %s: %w (starting fresh)", path, err)
	}
	for _, e := range f.Events {
		if e == nil || e.PatternID == "" {
			continue
		}
		s.events[shadowKey(e.Source, e.PatternID)] = e
	}
	return s, nil
}

func shadowKey(source, patternID string) string {
	return source + "\x00" + patternID
}

// Record merges a shadow-mode hit into the log. The record is coalesced per
// (source, pattern_id); repeat hits bump Count and Occurrences instead of
// appending a new entry. `freq` is the number of signals observed in the
// current worker tick.
func (s *ShadowLog) Record(source, patternID, template, sample, rule, verdict string, freq int) {
	if patternID == "" {
		return
	}
	if freq <= 0 {
		freq = 1
	}
	if len(sample) > shadowSampleMaxBytes {
		sample = sample[:shadowSampleMaxBytes] + "…"
	}
	now := time.Now().UTC()
	k := shadowKey(source, patternID)

	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.events[k]; ok {
		e.Template = template // keep refreshed as the miner refines it
		e.Verdict = verdict
		if rule != "" {
			e.RuleName = rule
		}
		if sample != "" {
			e.SampleMessage = sample
		}
		e.Count += freq
		e.Occurrences++
		e.LastSeen = now
		s.dirty = true
		return
	}

	// New entry — evict oldest if we're at capacity.
	if len(s.events) >= s.max {
		s.evictOldestLocked()
	}
	s.events[k] = &ShadowEvent{
		PatternID:     patternID,
		Template:      template,
		Source:        source,
		RuleName:      rule,
		Verdict:       verdict,
		SampleMessage: sample,
		Count:         freq,
		Occurrences:   1,
		FirstSeen:     now,
		LastSeen:      now,
	}
	s.dirty = true
}

// evictOldestLocked drops the event with the oldest LastSeen.
// Caller must hold s.mu (write lock).
func (s *ShadowLog) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, e := range s.events {
		if first || e.LastSeen.Before(oldestTime) {
			oldestKey = k
			oldestTime = e.LastSeen
			first = false
		}
	}
	if oldestKey != "" {
		delete(s.events, oldestKey)
	}
}

// All returns a snapshot of every shadow event, sorted by LastSeen
// descending (most recent first). Returned events are copies — callers
// cannot mutate the log.
func (s *ShadowLog) All() []*ShadowEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ShadowEvent, 0, len(s.events))
	for _, e := range s.events {
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// Len returns the number of distinct (source, pattern) events.
func (s *ShadowLog) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events)
}

// Stats returns aggregate counts useful for /api/agent/shadow/stats.
func (s *ShadowLog) Stats() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stats := map[string]int{
		"events":            len(s.events),
		"total_signals":     0,
		"verdict_unknown":   0,
		"verdict_spike":     0,
		"total_occurrences": 0,
	}
	for _, e := range s.events {
		stats["total_signals"] += e.Count
		stats["total_occurrences"] += e.Occurrences
		switch e.Verdict {
		case "unknown":
			stats["verdict_unknown"]++
		case "spike":
			stats["verdict_spike"]++
		}
	}
	return stats
}

// Clear removes every event. The change is persisted on the next Persist.
func (s *ShadowLog) Clear() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.events)
	if n == 0 {
		return 0
	}
	s.events = make(map[string]*ShadowEvent)
	s.dirty = true
	return n
}

// Dirty reports whether there are unflushed changes.
func (s *ShadowLog) Dirty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
}

// Persist atomically writes the shadow log to disk. Safe to call concurrently
// with Record / Clear. No-op when path is empty (in-memory mode).
func (s *ShadowLog) Persist() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir shadow log: %w", err)
	}

	// Materialize a stable, sorted slice (most-recent first) so the file
	// diffs cleanly across runs.
	out := make([]*ShadowEvent, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })

	f := shadowFile{
		Version:   shadowFileVersion,
		UpdatedAt: time.Now().UTC(),
		Events:    out,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal shadow log: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp shadow log: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename tmp shadow log: %w", err)
	}
	s.dirty = false
	return nil
}
