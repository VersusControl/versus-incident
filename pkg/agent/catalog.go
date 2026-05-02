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

// Pattern is one entry in the on-disk catalog (`patterns.json`).
//
// The catalog is the agent's long-term memory. During training we add
// patterns; during shadow / detect we look them up to decide whether a
// signal is "known". Operators curate it via the admin REST endpoints.
type Pattern struct {
	ID        string    `json:"id"`
	Template  string    `json:"template"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Count     int       `json:"count"`
	// BaselineFrequency is the EWMA of per-tick counts. Computed during
	// training; consumed by the spike detector in detect mode.
	BaselineFrequency float64 `json:"baseline_frequency"`
	// Verdict is the agent's classification of this pattern: "known" once
	// it is part of baseline (auto-promoted by count or set explicitly via
	// the admin API), otherwise empty. Operators flip a pattern to
	// "known" by POSTing {"verdict":"known"} to /api/agent/patterns/:id.
	Verdict string `json:"verdict"`
	// RuleName is the regex tag attached on first sighting ("default" when
	// only the default pattern matched, or the named rule otherwise).
	RuleName string `json:"rule_name"`
	// Source is the SignalSource name where the pattern was first observed.
	Source string `json:"source"`
	// Tags are arbitrary operator-supplied markers.
	Tags []string `json:"tags,omitempty"`
}

// Catalog is the in-memory + on-disk pattern store.
//
// All public methods are safe for concurrent use. Disk persistence is
// debounced — calls to MarkDirty() set a flag that the agent worker flushes
// at most once per `persist_interval`.
type Catalog struct {
	mu       sync.RWMutex
	path     string
	patterns map[string]*Pattern
	dirty    bool
}

// catalogFile is the on-disk schema. Versioned so we can evolve the
// in-memory struct without breaking existing files.
type catalogFile struct {
	Version   int                 `json:"version"`
	UpdatedAt time.Time           `json:"updated_at"`
	Patterns  map[string]*Pattern `json:"patterns"`
}

const catalogFileVersion = 1

// LoadCatalog opens an existing patterns file or returns an empty catalog if
// none exists.
func LoadCatalog(path string) (*Catalog, error) {
	c := &Catalog{
		path:     path,
		patterns: make(map[string]*Pattern),
	}
	if path == "" {
		return c, nil
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil // fresh start
	}
	if err != nil {
		return c, err
	}
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Patterns != nil {
		c.patterns = f.Patterns
	}
	return c, nil
}

// Get returns a pattern by ID (nil when not found).
func (c *Catalog) Get(id string) *Pattern {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.patterns[id]
}

// MarkKnown stamps a pattern as auto-promoted ("known") in the catalog.
func (c *Catalog) MarkKnown(patternID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.patterns[patternID]
	if !ok {
		return false
	}
	if p.Verdict == "known" {
		return false
	}
	p.Verdict = "known"
	c.dirty = true
	return true
}

// All returns a stable, sorted snapshot of every pattern (sorted by Count
// descending so the most-frequent patterns appear first in admin views).
func (c *Catalog) All() []*Pattern {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*Pattern, 0, len(c.patterns))
	for _, p := range c.patterns {
		// return copies so callers can't mutate the catalog
		cp := *p
		if p.Tags != nil {
			cp.Tags = append([]string(nil), p.Tags...)
		}
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// Len returns the number of patterns currently in the catalog.
func (c *Catalog) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.patterns)
}

// Upsert records an observation against patternID. If the pattern is new it
// is created with FirstSeen=now; otherwise Count is incremented and LastSeen
// is updated. tickCount is the number of matches observed in the current
// worker tick — used to update the EWMA baseline.
//
// ruleName comes from the regex pre-filter and is applied:
//   - on first-seen: always
//   - subsequently: only when a non-default named rule supersedes a previous
//     default tag, or when the previous tag was empty
func (c *Catalog) Upsert(patternID, template, source string, tickCount int, alpha float64, ruleName string) *Pattern {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	p, ok := c.patterns[patternID]
	if !ok {
		p = &Pattern{
			ID:        patternID,
			Template:  template,
			FirstSeen: now,
			LastSeen:  now,
			Count:     0,
			Source:    source,
			RuleName:  ruleName,
		}
		c.patterns[patternID] = p
	} else {
		// Promote tag if we now have a more specific (non-default) hit, or
		// fill in if it was previously empty.
		if ruleName != "" && ruleName != "default" && p.RuleName != ruleName {
			p.RuleName = ruleName
		} else if p.RuleName == "" && ruleName != "" {
			p.RuleName = ruleName
		}
	}
	p.Template = template // keep template fresh as miner refines it
	p.LastSeen = now
	p.Count += tickCount
	if alpha <= 0 {
		alpha = 0.2
	}
	if p.BaselineFrequency == 0 {
		p.BaselineFrequency = float64(tickCount)
	} else {
		p.BaselineFrequency = alpha*float64(tickCount) + (1-alpha)*p.BaselineFrequency
	}
	c.dirty = true
	return p
}

// Label updates operator-curated metadata for a pattern. Empty fields are
// left unchanged. Returns false when the pattern doesn't exist.
func (c *Catalog) Label(patternID, verdict string, tags []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.patterns[patternID]
	if !ok {
		return false
	}
	if verdict != "" {
		p.Verdict = verdict
	}
	if tags != nil {
		p.Tags = append([]string(nil), tags...)
	}
	c.dirty = true
	return true
}

// Delete removes a pattern (e.g. operator marks a false-positive cluster).
func (c *Catalog) Delete(patternID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.patterns[patternID]; !ok {
		return false
	}
	delete(c.patterns, patternID)
	c.dirty = true
	return true
}

// Dirty reports whether there are unflushed changes.
func (c *Catalog) Dirty() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dirty
}

// Persist flushes the in-memory catalog to disk atomically. Safe to call
// concurrently with Upsert/Label/Delete.
func (c *Catalog) Persist() error {
	if c.path == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("mkdir catalog: %w", err)
	}

	f := catalogFile{
		Version:   catalogFileVersion,
		UpdatedAt: time.Now().UTC(),
		Patterns:  c.patterns,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal catalog: %w", err)
	}

	// atomic write: temp + rename
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp catalog: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("rename tmp catalog: %w", err)
	}
	c.dirty = false
	return nil
}
