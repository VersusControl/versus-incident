package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/stats"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Pattern is one entry in the on-disk catalog (`patterns.json`).
//
// The catalog is the agent's long-term memory. During training we add
// patterns; during shadow / detect we look them up to decide whether a
// signal is "known". Operators curate it via the admin REST endpoints.
type Pattern struct {
	ID string `json:"id"`
	// OrgID scopes the pattern to one organization. Defaults to
	// storage.DefaultOrgID ("default") so single-tenant OSS catalogs are
	// unaffected; enterprise multi-tenant routing reads it to isolate
	// per-org catalogs.
	OrgID     string    `json:"org_id,omitempty"`
	Template  string    `json:"template"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Count     int       `json:"count"`
	// BaselineFrequency is the EWMA of the pattern's per-second match RATE
	// (tick matches ÷ poll seconds). Folding a rate rather than the raw per-tick
	// count makes the number intuitive (~38/s, not ~1151/30s-tick) and
	// independent of the poll interval. Computed during training; consumed by
	// the spike detector in detect mode. (When no fold config is wired — the
	// pre-rate default, exercised by unit tests — it folds the raw per-tick
	// count, unchanged.)
	BaselineFrequency float64 `json:"baseline_frequency"`
	// BaselineVariance is the EWMA variance folded alongside BaselineFrequency
	// (West's incremental form), giving the spike detector the dispersion its
	// z-score needs. 0 until the pattern re-learns after the variance migration
	// (`omitempty` keeps a pre-feature catalog byte-identical on read-back).
	BaselineVariance float64 `json:"baseline_variance,omitempty"`
	// BaselineAvg is the cumulative arithmetic mean of the per-second match RATE
	// (total ÷ number of folded ticks), updated incrementally each fold. It is
	// the CENTER the "average" baseline mode scores against; that mode reuses
	// the global EWMA spread (BaselineVariance) as its scale, so there is one
	// dispersion source and only the center differs from the default mode. 0
	// until the pattern re-learns after the baseline-mode migration
	// (`omitempty` keeps a pre-feature catalog byte-identical on read-back).
	BaselineAvg float64 `json:"baseline_avg,omitempty"`
	// Seasonal is the per-time-bucket EWMA rate (hour-of-day = 24 buckets, or
	// hour-of-week = 168, UTC), letting the detector score a tick against the
	// normal-for-this-hour rate. Empty unless seasonal spike detection is
	// enabled; length is the configured period. `omitempty` keeps a pre-feature
	// catalog byte-identical on read-back.
	Seasonal []stats.EWMA `json:"seasonal,omitempty"`
	// SpikeBaselineMode is the operator's per-pattern override of which learned
	// baseline the spike z-score is scored against ("default" | "average" |
	// "time_of_day"). Empty means inherit the agent.catalog.spike_baseline_mode
	// config default. `omitempty` keeps a pre-feature catalog byte-identical on
	// read-back.
	SpikeBaselineMode string `json:"spike_baseline_mode,omitempty"`
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
	// Service is the service name extracted from the pattern's first
	// matching log message (via the agent.service_pattern regex). Empty
	// when service detection is disabled or the regex did not match. The
	// agent uses this to gate detect-mode AI analysis behind the
	// new-service grace window.
	Service string `json:"service,omitempty"`
	// Tags are arbitrary operator-supplied markers.
	Tags []string `json:"tags,omitempty"`
	// Samples is a bounded ring (≤ SampleRingCap) of the most recent
	// POST-REDACTION example log lines this pattern was learned from, ordered
	// oldest→newest so the LATEST is Samples[len-1]. Populated inside
	// Catalog.RecordSample via PushSample (re-scrubbed at the storage boundary);
	// Signal.Raw is NEVER a source — the log brain passes the already-redacted
	// Observation sample message. It rides the whole-blob catalog Persist with
	// no new persistence wiring, is STRIPPED from the /api/agent/patterns list
	// rows (surfaced only on the pattern detail read), and is `omitempty` so a
	// pre-feature catalog reads back byte-identical.
	Samples []string `json:"samples,omitempty"`
}

// ServiceInfo tracks when a service was first seen by the agent. Stored in
// the same patterns.json file alongside patterns — one data store, no Redis
// dependency for this feature.
type ServiceInfo struct {
	// OrgID scopes the service entry to one organization. Defaults to
	// storage.DefaultOrgID ("default"); see Pattern.OrgID.
	OrgID     string    `json:"org_id,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	// Manual is true when an operator created the service by hand via the admin
	// API (so it is selectable as an override target BEFORE any signal is
	// attributed to it). Auto-discovered services leave it false. A manual
	// service coexists with auto-discovery: RegisterService never clobbers an
	// existing entry, so a name seen in a real signal keeps its manual flag.
	//
	// Serialized WITHOUT omitempty so the origin is ALWAYS determinable in the
	// services API: an auto-discovered row returns "manual":false explicitly
	// rather than dropping the key, letting the UI render an "Auto vs Manual"
	// origin column for every service.
	Manual bool `json:"manual"`
}

// Sentinel errors for manual-service CRUD, so the admin controller can map them
// to precise HTTP statuses without string matching.
var (
	// ErrServiceExists is returned when creating/renaming to a name that is
	// already tracked (auto-discovered or manual).
	ErrServiceExists = fmt.Errorf("service already exists")
	// ErrServiceNotFound is returned when renaming/deleting a name that is not
	// tracked.
	ErrServiceNotFound = fmt.Errorf("service not found")
)

// Catalog is the in-memory + on-disk pattern store.
//
// All public methods are safe for concurrent use. Disk persistence is
// debounced — calls to MarkDirty() set a flag that the agent worker flushes
// at most once per `persist_interval`.
type Catalog struct {
	mu       sync.RWMutex
	store    storage.Provider
	blobName string
	patterns map[string]*Pattern
	services map[string]*ServiceInfo
	dirty    bool
	fold     BaselineFold
}

// BaselineFold configures how Catalog.Upsert folds a tick into a pattern's
// baseline. The zero value is the legacy mean-only fold of the raw per-tick
// count (so a catalog with no fold wired — every unit test, and any pre-rate
// caller — behaves exactly as before). The agent worker wires the real config
// from the poll interval + spike settings via SetBaselineFold.
type BaselineFold struct {
	// PollSeconds is the tick duration; the folded value is tickCount /
	// PollSeconds, a per-second rate. <= 0 folds the raw per-tick count (legacy).
	PollSeconds float64
	// RejectZ holds out a tick whose rate sits >= this many σ from the pre-fold
	// baseline once the estimator is confident, so a burst can't drag the mean.
	// <= 0 disables the hold-out (every tick folds).
	RejectZ float64
	// SeasonalPeriod selects the seasonal buckets: 0 = off (global only),
	// 24 = hour-of-day, 168 = hour-of-week.
	SeasonalPeriod int
	// MinBaselineCount is the confidence gate for the global outlier hold-out:
	// the estimator is confident once the pattern's total sighting count reaches
	// this. <= 0 leaves the global fold always cold (never rejects).
	MinBaselineCount int
	// MinBucketSamples is the confidence gate for a seasonal bucket's own
	// hold-out (mirrors the detector's bucket-fallback threshold).
	MinBucketSamples int
}

// catalogFile is the on-disk schema. Versioned so we can evolve the
// in-memory struct without breaking existing files.
type catalogFile struct {
	Version   int                     `json:"version"`
	UpdatedAt time.Time               `json:"updated_at"`
	Patterns  map[string]*Pattern     `json:"patterns"`
	Services  map[string]*ServiceInfo `json:"services,omitempty"`
}

const catalogFileVersion = 1

// LoadCatalog opens an existing patterns blob from the storage provider
// or returns an empty catalog if none exists. The blob name is
// config.CatalogBlobName ("patterns").
func LoadCatalog(store storage.Provider) (*Catalog, error) {
	c := &Catalog{
		store:    store,
		blobName: "patterns",
		patterns: make(map[string]*Pattern),
		services: make(map[string]*ServiceInfo),
	}
	// When a CatalogStore is installed the boot load routes through it; the
	// inline ReadBlob("patterns") path below is the default (nil store).
	if s := catalogStore(); s != nil {
		patterns, services, err := s.Load()
		if patterns != nil {
			c.patterns = patterns
		}
		if services != nil {
			c.services = services
		}
		return c, err
	}
	if store == nil {
		return c, nil
	}

	data, err := store.ReadBlob(c.blobName)
	if err != nil {
		return c, err
	}
	if len(data) == 0 {
		return c, nil // fresh start
	}
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return c, fmt.Errorf("parse catalog: %w", err)
	}
	if f.Patterns != nil {
		c.patterns = f.Patterns
	}
	if f.Services != nil {
		c.services = f.Services
	}
	return c, nil
}

// Get returns a deep copy of the pattern keyed by id, or nil when not
// found. Returning a copy (rather than the live pointer) prevents
// callers from observing torn reads while a concurrent Upsert mutates
// the same struct.
func (c *Catalog) Get(id string) *Pattern {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.patterns[id]
	if !ok {
		return nil
	}
	cp := *p
	if p.Tags != nil {
		cp.Tags = append([]string(nil), p.Tags...)
	}
	return &cp
}

// MarkKnown stamps a pattern as auto-promoted ("known") in the catalog.
func (c *Catalog) MarkKnown(patternID string) bool {
	// Always reflect the promotion in the in-memory working set so the
	// single-pattern read (catalog.Get → the pattern detail page), the brain's
	// verdict guard, and the Promote idempotency check see "known" immediately.
	// When a store is installed its Snapshot backs the LIST view but NOT Get, so
	// without this the detail page and the every-tick Promote guard would keep
	// reading a stale empty verdict.
	c.mu.Lock()
	p, ok := c.patterns[patternID]
	changed := ok && p.Verdict != "known"
	if changed {
		p.Verdict = "known"
		c.dirty = true
	}
	c.mu.Unlock()

	// When a durable store is installed, route the curation so the promotion is
	// persisted (and applied fleet-wide). The store's MarkKnown creates the
	// pattern's identity row if it does not exist yet, so a pattern that crossed
	// auto_promote_after on its very first tick — before the debounced Persist has
	// written it — is still promoted instead of silently lost.
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditMarkKnown, PatternID: patternID}) == nil
	}
	return changed
}

// RepointService immediately sets an EXISTING pattern's Service to service —
// the retroactive half of an operator "Reassign to service" correction. Unlike
// Upsert's re-observation re-point (which only lands when a fresh matching log
// line re-clusters the pattern on a later tick), this takes effect on the very
// next catalog read, so reassigning a pattern that is not currently receiving
// traffic is reflected immediately instead of after an unbounded delay.
//
// service MUST be a real service (non-"" and not the "_unknown" sentinel); a
// blank/"_unknown" target is rejected so a reassignment can never blank out or
// unknown-out a pattern's attribution. The caller (createServiceOverride) also
// validates the target exists in the catalog first.
//
// Returns true when a pattern's Service was changed; false when the guard
// fails, the pattern does not exist (e.g. the override match was a message
// substring, not a pattern id — the lazy re-observation path still applies it),
// or the pattern already points at service.
//
// Store-aware: when a CatalogStore is installed the re-point routes through
// Curate(CatalogEditRepointService) so a fleet-wide read view (the enterprise
// partition store) re-points too; otherwise it mutates the in-memory pattern
// under lock and marks the catalog dirty for the next Persist flush.
func (c *Catalog) RepointService(patternID, service string) bool {
	if service == "" || service == "_unknown" {
		return false
	}
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditRepointService, PatternID: patternID, Service: service}) == nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.patterns[patternID]
	if !ok {
		return false
	}
	if p.Service == service {
		return false
	}
	p.Service = service
	c.dirty = true
	return true
}

func (c *Catalog) All() []*Pattern {
	// Bulk/admin read: route through the store's unified read view when one is
	// installed. The inline in-memory snapshot below is the default and the
	// fallback if the store read fails.
	if s := catalogStore(); s != nil {
		out, _, err := s.Snapshot()
		if err != nil {
			log.Printf("agent: catalog snapshot failed, serving local view: %v", err)
		} else {
			sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
			return out
		}
	}
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
// service is the service name extracted from the log via
// agent.service_patterns, then run through the manual-attribution override
// resolver by the caller. It is stamped on a new pattern verbatim; on an
// existing pattern it is REFRESHED whenever the incoming attribution is real
// (non-empty and not "_unknown"), so an operator override resolved on a later
// tick re-points an already-learned pattern's Service instead of leaving the
// stale first-sighting label. A "" or "_unknown" attribution on a later tick
// never clobbers a good stored Service, so a tick where detection/override
// yields nothing keeps the last real attribution. Pass "" when service
// detection is disabled or the pattern's regexes did not match.
//
// ruleName comes from the regex pre-filter and is applied:
//   - on first-seen: always
//   - subsequently: only when a non-default named rule supersedes a previous
//     default tag, or when the previous tag was empty
func (c *Catalog) Upsert(patternID, template, source string, tickCount int, alpha float64, ruleName, service string) *Pattern {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	p, ok := c.patterns[patternID]
	if !ok {
		p = &Pattern{
			ID:        patternID,
			OrgID:     storage.DefaultOrgID,
			Template:  template,
			FirstSeen: now,
			LastSeen:  now,
			Count:     0,
			Source:    source,
			RuleName:  ruleName,
			Service:   service,
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
		// Re-point Service on re-observation when the incoming attribution is
		// real. An operator's manual override resolves on every tick (see
		// brain_log.Group → ResolveServiceOverride), so an already-learned
		// pattern must adopt the corrected service rather than keep its stale
		// first-sighting label. The guard keeps a good service from being
		// clobbered back to "_unknown"/"" on a later tick where detection and
		// override both yield nothing. Verdict/grace are untouched here:
		// Service is only the attribution label, and RegisterService/grace key
		// off the service map independently.
		if service != "" && service != "_unknown" {
			p.Service = service
		}
	}
	p.Template = template // keep template fresh as miner refines it
	p.LastSeen = now
	prevCount := p.Count // pre-fold total sightings — the confidence gate
	p.Count += tickCount
	if alpha <= 0 {
		alpha = 0.2
	}
	c.foldBaseline(p, tickCount, alpha, now, prevCount)
	c.dirty = true
	return p
}

// SetBaselineFold wires the baseline-fold configuration the agent worker
// derives from its poll interval + spike settings. It is set once at
// construction; the zero value (never set) keeps the legacy mean-only
// per-tick-count fold.
func (c *Catalog) SetBaselineFold(f BaselineFold) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fold = f
}

// foldBaseline updates a pattern's learned baseline with this tick. Caller
// holds c.mu.
//
// With no fold config wired it is the original mean-only EWMA of the raw
// per-tick count — byte-identical to the pre-rate behavior. With a fold config
// it folds a per-SECOND rate (poll-independent), tracks the variance the
// z-score needs, holds out a strong outlier once confident so a burst can't
// drag the mean, and (when seasonal is enabled) folds the same rate into the
// current time bucket.
func (c *Catalog) foldBaseline(p *Pattern, tickCount int, alpha float64, now time.Time, prevCount int) {
	f := c.fold

	// Legacy path: no fold config → the original mean-only fold of the raw
	// per-tick count. No variance, no seasonal, so a pre-feature catalog reads
	// back byte-identical.
	if f.PollSeconds <= 0 && f.SeasonalPeriod <= 0 && f.RejectZ <= 0 {
		if p.BaselineFrequency == 0 {
			p.BaselineFrequency = float64(tickCount)
		} else {
			p.BaselineFrequency = alpha*float64(tickCount) + (1-alpha)*p.BaselineFrequency
		}
		return
	}

	// The value we fold: a per-second rate when a poll interval is configured.
	value := float64(tickCount)
	if f.PollSeconds > 0 {
		value = float64(tickCount) / f.PollSeconds
	}

	// Global mean + variance. The first fold seeds the mean at the value (as
	// the pre-rate seed did); later folds run the incremental update unless the
	// tick is a confident outlier, in which case it is held out.
	if p.BaselineFrequency == 0 {
		p.BaselineFrequency = value
		p.BaselineVariance = 0
	} else {
		std := math.Sqrt(math.Max(0, p.BaselineVariance))
		confident := f.MinBaselineCount > 0 && prevCount >= f.MinBaselineCount
		if !stats.ShouldReject(value, p.BaselineFrequency, std, confident, f.RejectZ) {
			g := stats.EWMA{Mean: p.BaselineFrequency, Variance: p.BaselineVariance, Count: 2}
			g.Observe(value, alpha)
			p.BaselineFrequency = g.Mean
			p.BaselineVariance = g.Variance
		}
	}

	// Seasonal buckets: fold the same rate into the current hour's bucket, with
	// the same confident-outlier hold-out per bucket.
	if f.SeasonalPeriod > 0 {
		if len(p.Seasonal) != f.SeasonalPeriod {
			grown := make([]stats.EWMA, f.SeasonalPeriod)
			copy(grown, p.Seasonal)
			p.Seasonal = grown
		}
		idx := stats.SeasonalIndex(now, f.SeasonalPeriod)
		bucket := p.Seasonal[idx]
		bConfident := f.MinBucketSamples > 0 && bucket.Count >= f.MinBucketSamples
		if !stats.ShouldReject(value, bucket.Mean, bucket.Std(), bConfident, f.RejectZ) {
			bucket.Observe(value, alpha)
			p.Seasonal[idx] = bucket
			// Cumulative arithmetic mean of the rate, folded over the SAME
			// accepted ticks the seasonal buckets are. The fold count is exactly
			// the total seasonal observation count (every accepted tick bumps
			// one bucket), so no separate counter column is needed and the
			// backends stay byte-identical: avg += (value − avg) / n, where n is
			// the seasonal total AFTER this tick. A strong outlier held out of
			// the seasonal baseline is held out of the average too.
			if n := seasonalCount(p.Seasonal); n > 0 {
				p.BaselineAvg += (value - p.BaselineAvg) / float64(n)
			}
		}
	}
}

// seasonalCount sums the observation counts across every seasonal bucket — the
// total number of ticks folded into the seasonal baseline, which doubles as the
// fold count for the cumulative arithmetic mean (BaselineAvg).
func seasonalCount(buckets []stats.EWMA) int {
	total := 0
	for _, b := range buckets {
		total += b.Count
	}
	return total
}

// RecordSample appends a redacted example log line to a pattern's bounded
// sample ring (drop-oldest past SampleRingCap) and marks the catalog dirty, so
// the ring rides the existing debounced Persist path (and, under an installed
// CatalogStore, the same per-partition Persist) with NO new persistence wiring
// — exactly like the Service re-point Upsert already carries. It is the log
// brain's post-Upsert companion: the brain holds the pipeline's redactor and
// passes the representative POST-REDACTION Observation message; Signal.Raw is
// never a source. scrub re-scrubs the line at the storage boundary
// (defence-in-depth) and MAY be nil. A missing pattern or empty sample is a
// no-op. It stays OFF the store hot path (never calls Curate), so community and
// single-instance behaviour is unchanged.
func (c *Catalog) RecordSample(patternID, sample string, scrub core.Scrubber) {
	if sample == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.patterns[patternID]
	if !ok {
		return
	}
	p.Samples = PushSample(p.Samples, sample, scrub)
	c.dirty = true
}

// Label updates operator-curated metadata for a pattern.
//
// verdict is a tri-state pointer that lets the admin API distinguish "verdict
// absent (leave it)" from "verdict present and empty (clear it)":
//   - nil            → leave the stored verdict unchanged (a tags-only update)
//   - non-nil, ""    → CLEAR the verdict (operator "Clear verdict")
//   - non-nil, "..." → SET the verdict to that value
//
// tags nil leaves tags unchanged. Returns false when the pattern doesn't exist.
func (c *Catalog) Label(patternID string, verdict *string, tags []string) bool {
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditLabel, PatternID: patternID, Verdict: verdict, Tags: tags}) == nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.patterns[patternID]
	if !ok {
		return false
	}
	if verdict != nil {
		p.Verdict = *verdict
	}
	if tags != nil {
		p.Tags = append([]string(nil), tags...)
	}
	c.dirty = true
	return true
}

// Delete removes a pattern (e.g. operator marks a false-positive cluster).
func (c *Catalog) Delete(patternID string) bool {
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditDelete, PatternID: patternID}) == nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.patterns[patternID]; !ok {
		return false
	}
	delete(c.patterns, patternID)
	c.dirty = true
	return true
}

// ResetPatterns wipes every learned log pattern — the whole `patterns` map —
// and persists the empty pattern set so log training restarts from scratch on
// the next tick. Discovered/manual services are LEFT INTACT. It returns the
// number of patterns that were removed (the pre-reset view: fleet-wide when a
// CatalogStore is installed, otherwise this instance's in-memory set).
//
// This is the pattern-half counterpart to ResetServices: when a CatalogStore is
// installed the empty pattern state routes through it (so a fleet-wide read
// view is cleared, not just this instance's working set); otherwise the inline
// whole-blob path rewrites the "patterns" blob from the in-memory maps, which
// still carry the untouched services.
func (c *Catalog) ResetPatterns() (patterns int, err error) {
	// Snapshot the pre-reset count through the same read view callers see
	// (store-aware when installed) before clearing.
	patterns = len(c.All())

	c.mu.Lock()
	c.patterns = make(map[string]*Pattern)
	c.dirty = true
	c.mu.Unlock()

	if s := catalogStore(); s != nil {
		if err := s.Curate(CatalogEdit{Kind: CatalogEditResetPatterns}); err != nil {
			return patterns, err
		}
		c.mu.Lock()
		c.dirty = false
		c.mu.Unlock()
		return patterns, nil
	}
	if err := c.Persist(); err != nil {
		return patterns, err
	}
	return patterns, nil
}

// ResetServices wipes every discovered/manual service — the whole `services`
// map — and persists the empty service set so service discovery restarts from
// scratch on the next tick. Learned log patterns are LEFT INTACT. It returns the
// number of services that were removed (the pre-reset view: fleet-wide when a
// CatalogStore is installed, otherwise this instance's in-memory set).
//
// This is the service-half counterpart to ResetPatterns: when a CatalogStore is
// installed the empty service state routes through it (so a fleet-wide read
// view is cleared, not just this instance's working set); otherwise the inline
// whole-blob path rewrites the "patterns" blob from the in-memory maps, which
// still carry the untouched patterns.
func (c *Catalog) ResetServices() (services int, err error) {
	// Snapshot the pre-reset count through the same read view callers see
	// (store-aware when installed) before clearing.
	services = len(c.AllServices())

	c.mu.Lock()
	c.services = make(map[string]*ServiceInfo)
	c.dirty = true
	c.mu.Unlock()

	if s := catalogStore(); s != nil {
		if err := s.Curate(CatalogEdit{Kind: CatalogEditResetServices}); err != nil {
			return services, err
		}
		c.mu.Lock()
		c.dirty = false
		c.mu.Unlock()
		return services, nil
	}
	if err := c.Persist(); err != nil {
		return services, err
	}
	return services, nil
}

// Dirty reports whether there are unflushed changes.
func (c *Catalog) Dirty() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dirty
}

// Persist flushes the in-memory catalog to the storage backend. Safe to
// call concurrently with Upsert/Label/Delete.
func (c *Catalog) Persist() error {
	// Route this instance's working-set write through the store when one is
	// installed; the inline WriteBlob("patterns") path below is the default.
	if s := catalogStore(); s != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.dirty {
			return nil
		}
		if err := s.Persist(c.patterns, c.services); err != nil {
			return err
		}
		c.dirty = false
		return nil
	}
	if c.store == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}

	f := catalogFile{
		Version:   catalogFileVersion,
		UpdatedAt: time.Now().UTC(),
		Patterns:  c.patterns,
		Services:  c.services,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal catalog: %w", err)
	}
	if err := c.store.WriteBlob(c.blobName, data); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// ---------------------------------------------------------------------------
// Service tracking — detect-mode new-service grace period
// ---------------------------------------------------------------------------

// RegisterService records a service name the first time it is seen. Returns
// true when the service was newly registered (first sighting), false when it
// was already known. The caller uses this to decide whether to log a
// "new service discovered" message.
func (c *Catalog) RegisterService(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.services[name]; ok {
		return false
	}
	c.services[name] = &ServiceInfo{OrgID: storage.DefaultOrgID, FirstSeen: time.Now().UTC()}
	c.dirty = true
	return true
}

// IsServiceInGrace reports whether the named service is still inside its
// new-service grace window. A zero graceDuration means grace is disabled
// (always returns false). An unknown service is registered on the spot and
// enters grace.
func (c *Catalog) IsServiceInGrace(name string, graceDuration time.Duration) bool {
	if graceDuration <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	svc, ok := c.services[name]
	if !ok {
		svc = &ServiceInfo{OrgID: storage.DefaultOrgID, FirstSeen: time.Now().UTC()}
		c.services[name] = svc
		c.dirty = true
	}
	return time.Now().UTC().Before(svc.FirstSeen.Add(graceDuration))
}

// AllServices returns a snapshot of every tracked service, sorted by
// FirstSeen ascending.
func (c *Catalog) AllServices() map[string]ServiceInfo {
	// Bulk/admin read: route through the store's unified read view when one is
	// installed. The inline in-memory snapshot below is the default and the
	// fallback if the store read fails.
	if s := catalogStore(); s != nil {
		_, services, err := s.Snapshot()
		if err != nil {
			log.Printf("agent: catalog snapshot failed, serving local services view: %v", err)
		} else {
			return services
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]ServiceInfo, len(c.services))
	for k, v := range c.services {
		out[k] = *v
	}
	return out
}

// EndServiceGrace forces a service out of its grace period by setting
// FirstSeen to the zero time. Returns false when the service doesn't exist.
func (c *Catalog) EndServiceGrace(name string) bool {
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditEndServiceGrace, Service: name}) == nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	svc, ok := c.services[name]
	if !ok {
		return false
	}
	svc.FirstSeen = time.Time{} // epoch → always past grace
	c.dirty = true
	return true
}

// RestartServiceGrace resets a service's FirstSeen to now, restarting the
// grace window. Returns false when the service doesn't exist.
func (c *Catalog) RestartServiceGrace(name string) bool {
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditRestartServiceGrace, Service: name}) == nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	svc, ok := c.services[name]
	if !ok {
		return false
	}
	svc.FirstSeen = time.Now().UTC()
	c.dirty = true
	return true
}

// ---------------------------------------------------------------------------
// Manual service CRUD — operator-curated services
// ---------------------------------------------------------------------------

// Service returns a copy of one tracked service's info and whether it exists.
// It reads the same unified view AllServices() serves (store-aware when a
// CatalogStore is installed), so a manual service created on any instance is
// visible fleet-wide.
func (c *Catalog) Service(name string) (ServiceInfo, bool) {
	info, ok := c.AllServices()[name]
	return info, ok
}

// CreateService records an operator-created (manual) service so it is
// selectable as an override target before any signal is attributed to it. The
// caller (admin controller) validates non-existence first — this is the write.
// It routes through the CatalogStore when one is installed so the manual
// service is fleet-visible; otherwise it writes the in-memory + blob path.
func (c *Catalog) CreateService(name string) error {
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditCreateService, Service: name})
	}
	c.mu.Lock()
	if _, ok := c.services[name]; ok {
		c.mu.Unlock()
		return ErrServiceExists
	}
	c.services[name] = &ServiceInfo{OrgID: storage.DefaultOrgID, FirstSeen: time.Now().UTC(), Manual: true}
	c.dirty = true
	c.mu.Unlock()
	// Persist immediately: a manual service has no signal to re-create it and
	// its override rules persist synchronously, so the two must not diverge.
	return c.Persist()
}

// RenameService moves a service entry from oldName to newName, preserving its
// FirstSeen and manual flag. The caller validates that oldName exists and
// newName does not. Pattern attribution is not bulk-rewritten (a pattern's
// Service is a historical label; future signals re-attribute); the admin
// controller repoints override rules that target the old name so none dangle.
func (c *Catalog) RenameService(oldName, newName string) error {
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditRenameService, Service: oldName, NewService: newName})
	}
	c.mu.Lock()
	svc, ok := c.services[oldName]
	if !ok {
		c.mu.Unlock()
		return ErrServiceNotFound
	}
	if _, exists := c.services[newName]; exists {
		c.mu.Unlock()
		return ErrServiceExists
	}
	moved := *svc
	delete(c.services, oldName)
	c.services[newName] = &moved
	c.dirty = true
	c.mu.Unlock()
	return c.Persist()
}

// DeleteService removes a tracked service entry. Returns false when it does not
// exist. The admin controller gates this on the service being manual and on
// having no override rules that target it, so a delete never orphans an
// override.
func (c *Catalog) DeleteService(name string) bool {
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditDeleteService, Service: name}) == nil
	}
	c.mu.Lock()
	if _, ok := c.services[name]; !ok {
		c.mu.Unlock()
		return false
	}
	delete(c.services, name)
	c.dirty = true
	c.mu.Unlock()
	return c.Persist() == nil
}
