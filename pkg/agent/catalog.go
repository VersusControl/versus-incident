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
	// Severity is operator-assigned ("known", "info", "warning", ...).
	// Empty means "not yet labeled".
	Severity string `json:"severity"`
	// Label is a short operator-assigned name (e.g. "db-retry-expected").
	Label string `json:"label"`
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
// none exists. Backups (`.1`..`.5`) are tried in order on parse failure.
func LoadCatalog(path string) (*Catalog, error) {
	c := &Catalog{
		path:     path,
		patterns: make(map[string]*Pattern),
	}
	if path == "" {
		return c, nil
	}

	candidates := []string{path}
	for i := 1; i <= 5; i++ {
		candidates = append(candidates, fmt.Sprintf("%s.%d", path, i))
	}

	var lastErr error
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			lastErr = err
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		var f catalogFile
		if err := json.Unmarshal(data, &f); err != nil {
			lastErr = fmt.Errorf("parse %s: %w", p, err)
			continue
		}
		if f.Patterns != nil {
			c.patterns = f.Patterns
		}
		return c, nil
	}

	if lastErr != nil && os.IsNotExist(lastErr) {
		return c, nil // fresh start
	}
	return c, lastErr
}

// Get returns a pattern by ID (nil when not found).
func (c *Catalog) Get(id string) *Pattern {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.patterns[id]
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
// ruleName / severity come from the regex pre-filter and are applied:
//   - on first-seen: always
//   - subsequently: only if currently empty, OR if a non-default named rule
//     supersedes a previous default tag
func (c *Catalog) Upsert(patternID, template, source string, tickCount int, alpha float64, ruleName, severity string) *Pattern {
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
			Severity:  severity,
			Label:     ruleName,
		}
		c.patterns[patternID] = p
	} else {
		// Promote tag if we now have a more specific (non-default) hit, or
		// fill in if it was previously empty.
		if ruleName != "" && ruleName != "default" && p.Label != ruleName {
			p.Label = ruleName
			p.Severity = severity
		} else if p.Severity == "" && severity != "" {
			p.Severity = severity
			if p.Label == "" {
				p.Label = ruleName
			}
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

// Label updates operator-curated metadata for a pattern.
// Returns false when the pattern doesn't exist.
func (c *Catalog) Label(patternID, severity, label string, tags []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.patterns[patternID]
	if !ok {
		return false
	}
	if severity != "" {
		p.Severity = severity
	}
	if label != "" {
		p.Label = label
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

// Persist flushes the in-memory catalog to disk atomically and rotates
// up to 5 backups. Safe to call concurrently with Upsert/Label/Delete.
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

	// rotate backups: .4 → .5, .3 → .4, ..., current → .1
	for i := 4; i >= 1; i-- {
		oldP := fmt.Sprintf("%s.%d", c.path, i)
		newP := fmt.Sprintf("%s.%d", c.path, i+1)
		_ = os.Rename(oldP, newP) // ignore: file may not exist yet
	}
	if _, err := os.Stat(c.path); err == nil {
		_ = os.Rename(c.path, c.path+".1")
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
