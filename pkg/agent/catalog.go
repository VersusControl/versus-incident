package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

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
	// Service is the service name extracted from the pattern's first
	// matching log message (via the agent.service_pattern regex). Empty
	// when service detection is disabled or the regex did not match. The
	// agent uses this to gate detect-mode AI analysis behind the
	// new-service grace window.
	Service string `json:"service,omitempty"`
	// Tags are arbitrary operator-supplied markers.
	Tags []string `json:"tags,omitempty"`
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
	Manual bool `json:"manual,omitempty"`
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
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditMarkKnown, PatternID: patternID}) == nil
	}
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
// agent.service_patterns. It is stamped on the pattern only on first sighting
// (subsequent observations preserve the original attribution to keep the
// catalog stable). Pass "" when service detection is disabled or the
// pattern's regexes did not match.
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
	if s := catalogStore(); s != nil {
		return s.Curate(CatalogEdit{Kind: CatalogEditLabel, PatternID: patternID, Verdict: verdict, Tags: tags}) == nil
	}
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

// Reset wipes the entire catalog — every learned pattern AND every discovered
// service — and persists the empty catalog so training restarts from scratch
// on the next tick. It returns the number of patterns and services that were
// removed (the pre-reset view: fleet-wide when a CatalogStore is installed,
// otherwise this instance's in-memory set).
//
// This is the whole-catalog counterpart to Delete/EndServiceGrace: when a
// CatalogStore is installed the empty state routes through it (so a fleet-wide
// read view is cleared, not just this instance's working set); otherwise the
// inline whole-blob path writes an empty "patterns" blob.
func (c *Catalog) Reset() (patterns int, services int, err error) {
	// Snapshot the pre-reset counts through the same read view callers see
	// (store-aware when installed) before clearing.
	patterns = len(c.All())
	services = len(c.AllServices())

	c.mu.Lock()
	c.patterns = make(map[string]*Pattern)
	c.services = make(map[string]*ServiceInfo)
	c.dirty = true
	c.mu.Unlock()

	if s := catalogStore(); s != nil {
		if err := s.Curate(CatalogEdit{Kind: CatalogEditReset}); err != nil {
			return patterns, services, err
		}
		c.mu.Lock()
		c.dirty = false
		c.mu.Unlock()
		return patterns, services, nil
	}
	if err := c.Persist(); err != nil {
		return patterns, services, err
	}
	return patterns, services, nil
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
