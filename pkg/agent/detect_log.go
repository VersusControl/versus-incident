package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// DetectEvent is the audit record for a single detect-mode handling
// of a pattern: what was sent to the model, what came back, what the
// final structured finding looked like, and what the worker did with
// it. Surfaced to the UI so operators can validate AI behavior end to
// end.
//
// One DetectEvent is recorded per worker decision — including
// "cached", "quota", "ai_error", and "send_error" outcomes — so the
// log doubles as a debugging aid when alerts disappear.
type DetectEvent struct {
	ID        string    `json:"id"`        // 16-byte hex; stable over restarts
	Timestamp time.Time `json:"timestamp"` // worker decision time

	// Pattern context
	Source    string   `json:"source"`
	PatternID string   `json:"pattern_id"`
	Template  string   `json:"template"`
	Service   string   `json:"service,omitempty"`
	Verdict   string   `json:"verdict"` // unknown | spike
	Frequency int      `json:"frequency"`
	Baseline  float64  `json:"baseline"`
	Samples   []string `json:"samples,omitempty"` // up to 3, redacted

	// AI call (empty when outcome != "emitted" — cached/dry/quota/etc.
	// did not invoke the model; ai_error fills RawResponse only when
	// available).
	Model       string `json:"model,omitempty"`
	UserPrompt  string `json:"user_prompt,omitempty"`
	RawResponse string `json:"raw_response,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`

	// Final structured finding (nil when no finding was produced).
	Finding *core.AIFinding `json:"finding,omitempty"`

	// Outcome label — see Worker.emitDetect: emitted | cached | dry |
	// quota | ai_error | send_error.
	Outcome string `json:"outcome"`
	// Error message when Outcome is ai_error / send_error.
	Error string `json:"error,omitempty"`
}

// DetectLog is the in-memory + on-disk store of detect-mode AI calls.
// Bounded ring of the most-recent N events; older entries are evicted
// FIFO by Timestamp.
//
// All public methods are safe for concurrent use. Disk persistence is
// debounced — Record sets a dirty flag that the worker flushes at most
// once per persist tick.
type DetectLog struct {
	mu       sync.RWMutex
	store    storage.Provider
	blobName string
	events   []*DetectEvent // newest last; capped at max
	max      int
	dirty    bool
}

type detectFile struct {
	Version   int            `json:"version"`
	UpdatedAt time.Time      `json:"updated_at"`
	Events    []*DetectEvent `json:"events"`
}

const (
	detectFileVersion = 1
	// detectDefaultMax bounds total stored events. ~5 KB per event with
	// prompt + response, so 500 keeps the JSON file under ~3 MB.
	detectDefaultMax = 500
	// detectSampleMaxBytes bounds each sample line.
	detectSampleMaxBytes = 512
	// detectPromptMaxBytes / detectResponseMaxBytes bound the audit
	// payload so a runaway model doesn't bloat the file.
	detectPromptMaxBytes   = 8 * 1024
	detectResponseMaxBytes = 8 * 1024
)

// LoadDetectLog opens an existing detect log from the storage
// provider or returns an empty one when no blob is present. `max` caps
// retained events; pass 0 for the default. A nil store disables
// persistence (in-memory only).
func LoadDetectLog(store storage.Provider, max int) (*DetectLog, error) {
	if max <= 0 {
		max = detectDefaultMax
	}
	d := &DetectLog{
		store:    store,
		blobName: "detect",
		events:   make([]*DetectEvent, 0, 64),
		max:      max,
	}
	if store == nil {
		return d, nil
	}
	data, err := store.ReadBlob(d.blobName)
	if err != nil {
		return d, fmt.Errorf("read detect blob: %w", err)
	}
	if len(data) == 0 {
		return d, nil
	}
	var f detectFile
	if err := json.Unmarshal(data, &f); err != nil {
		return d, fmt.Errorf("unmarshal detect blob: %w", err)
	}
	// Defensive cap on load.
	if len(f.Events) > max {
		f.Events = f.Events[len(f.Events)-max:]
	}
	d.events = f.Events
	return d, nil
}

// Record appends a DetectEvent. The caller passes a partially-filled
// event; ID and Timestamp are assigned here if zero.
//
// Field-size caps are applied defensively so a misbehaving model
// can't bloat the on-disk file.
func (d *DetectLog) Record(e *DetectEvent) {
	if d == nil || e == nil {
		return
	}
	if e.ID == "" {
		e.ID = newDetectEventID()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	} else {
		e.Timestamp = e.Timestamp.UTC()
	}
	for i, s := range e.Samples {
		if len(s) > detectSampleMaxBytes {
			e.Samples[i] = s[:detectSampleMaxBytes] + "…"
		}
	}
	if len(e.UserPrompt) > detectPromptMaxBytes {
		e.UserPrompt = e.UserPrompt[:detectPromptMaxBytes] + "…"
	}
	if len(e.RawResponse) > detectResponseMaxBytes {
		e.RawResponse = e.RawResponse[:detectResponseMaxBytes] + "…"
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, e)
	if len(d.events) > d.max {
		// FIFO eviction — drop oldest.
		drop := len(d.events) - d.max
		d.events = d.events[drop:]
	}
	d.dirty = true
}

// All returns a snapshot of every event, sorted by Timestamp
// descending (newest first). Returned events are shallow copies; the
// embedded *AIFinding pointer is shared (read-only by convention).
func (d *DetectLog) All() []*DetectEvent {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*DetectEvent, 0, len(d.events))
	for _, e := range d.events {
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out
}

// Get returns a copy of the event with the given ID, or nil.
func (d *DetectLog) Get(id string) *DetectEvent {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, e := range d.events {
		if e.ID == id {
			cp := *e
			return &cp
		}
	}
	return nil
}

// Len returns the number of stored events.
func (d *DetectLog) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.events)
}

// Stats returns aggregate counts useful for /api/agent/detect/stats.
func (d *DetectLog) Stats() map[string]int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	stats := map[string]int{"events": len(d.events)}
	for _, e := range d.events {
		if e.Outcome != "" {
			stats["outcome_"+e.Outcome]++
		}
		switch e.Verdict {
		case "unknown":
			stats["verdict_unknown"]++
		case "spike":
			stats["verdict_spike"]++
		}
		if e.Finding != nil && e.Finding.Severity != "" {
			stats["severity_"+e.Finding.Severity]++
		}
	}
	return stats
}

// Clear removes every event.
func (d *DetectLog) Clear() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.events)
	if n == 0 {
		return 0
	}
	d.events = d.events[:0]
	d.dirty = true
	return n
}

// Dirty reports whether there are unflushed changes.
func (d *DetectLog) Dirty() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.dirty
}

// Persist atomically writes the detect log via the storage backend.
// No-op when store is nil or there are no unflushed changes.
func (d *DetectLog) Persist() error {
	if d == nil || d.store == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.dirty {
		return nil
	}
	f := detectFile{
		Version:   detectFileVersion,
		UpdatedAt: time.Now().UTC(),
		Events:    append([]*DetectEvent(nil), d.events...),
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal detect log: %w", err)
	}
	if err := d.store.WriteBlob(d.blobName, data); err != nil {
		return err
	}
	d.dirty = false
	return nil
}

// newDetectEventID returns a 16-character hex string. Crypto-strong
// random, with a deterministic time-based fallback for the (extremely
// rare) case where rand.Read fails.
func newDetectEventID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	return fmt.Sprintf("%016x", time.Now().UnixNano())
}
