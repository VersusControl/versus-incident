package storage

import (
	"strings"
	"sync"
	"time"
)

// memoryProvider is an in-memory backend used by tests. It is concurrency-safe
// and never returns transport errors. Not exposed as a config option.
type memoryProvider struct {
	mu        sync.RWMutex
	blobs     map[string][]byte
	blobAt    map[string]time.Time // per-blob updated_at, for Lifecycle purge
	incidents []*IncidentRecord
	analyses  []*AnalysisRecord
}

// NewMemory returns a Provider that keeps all state in memory. Intended
// for tests; never select via the config factory.
func NewMemory() Provider {
	return &memoryProvider{
		blobs:  make(map[string][]byte),
		blobAt: make(map[string]time.Time),
	}
}

func (m *memoryProvider) ReadBlob(name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if data, ok := m.blobs[name]; ok {
		// return a copy so callers can't mutate state through the slice
		out := make([]byte, len(data))
		copy(out, data)
		return out, nil
	}
	return nil, nil
}

func (m *memoryProvider) WriteBlob(name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.blobs[name] = cp
	m.blobAt[name] = time.Now().UTC()
	return nil
}

// CreateBlobIfAbsent implements the optional storage.BlobCreator capability.
// The write is atomic under the provider mutex, so N concurrent
// creators serialize and exactly one observes written==true; every other
// caller observes written==false and reads the survivor's bytes via
// ReadBlob. An existing key (created here or via WriteBlob) is left
// untouched.
func (m *memoryProvider) CreateBlobIfAbsent(name string, data []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.blobs[name]; ok {
		return false, nil
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.blobs[name] = cp
	m.blobAt[name] = time.Now().UTC()
	return true, nil
}

func (m *memoryProvider) ListBlobs(prefix string) ([]Blob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Blob, 0)
	for name, data := range m.blobs {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Return a copy so callers can't mutate state through the slice.
		cp := make([]byte, len(data))
		copy(cp, data)
		out = append(out, Blob{Name: name, Data: cp})
	}
	return out, nil
}

func (m *memoryProvider) SaveIncident(rec *IncidentRecord) error {
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, existing := range m.incidents {
		if existing.ID == rec.ID {
			m.incidents[i] = rec
			return nil
		}
	}
	m.incidents = append(m.incidents, rec)
	return nil
}

func (m *memoryProvider) UpdateIncidentAck(id string, ackedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.incidents {
		if rec.ID == id {
			t := ackedAt.UTC()
			rec.AckedAt = &t
			return nil
		}
	}
	return ErrNotFound
}

func (m *memoryProvider) GetIncident(id string) (*IncidentRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rec := range m.incidents {
		if rec.ID == id {
			cp := *rec
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *memoryProvider) ListIncidents(limit int) ([]*IncidentRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := len(m.incidents)
	if n == 0 {
		return nil, nil
	}
	out := make([]*IncidentRecord, 0, n)
	for i := n - 1; i >= 0; i-- {
		cp := *m.incidents[i]
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CountIncidents implements the optional storage.IncidentPager capability.
// The in-memory history is already capped, so a linear tally is cheap.
// Counts reflect open work: resolved incidents are skipped so the tally
// matches the unresolved-only counts the SQL backend returns. Legacy rows
// are classified via EffectiveOrigin, matching the SQL backend.
func (m *memoryProvider) CountIncidents() (IncidentCounts, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var c IncidentCounts
	for _, rec := range m.incidents {
		if rec.Resolved {
			continue
		}
		if rec.EffectiveOrigin() == OriginAIDetect {
			c.AIDetect++
		} else {
			c.Webhook++
		}
	}
	c.Total = c.AIDetect + c.Webhook
	return c, nil
}

// CountIncidentsByStatus implements the optional storage.IncidentPager
// capability. The in-memory history is already capped, so a single pass over
// the slice is cheap; the shared StatusCountsOf helper classifies each row via
// EffectiveOrigin and buckets it by stored status, matching the SQL backend
// exactly.
func (m *memoryProvider) CountIncidentsByStatus() (IncidentStatusCounts, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return StatusCountsOf(m.incidents), nil
}

// ListIncidentsPage implements the optional storage.IncidentPager
// capability: one bounded, newest-first page over the in-memory slice,
// skipping offset matches and returning at most limit rows. The origin
// filter is applied while walking so a filtered page stays bounded.
func (m *memoryProvider) ListIncidentsPage(origin string, offset, limit int) ([]*IncidentRecord, error) {
	if limit <= 0 {
		limit = DefaultIncidentPageSize
	}
	if offset < 0 {
		offset = 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	filtered := origin == OriginAIDetect || origin == OriginWebhook
	out := make([]*IncidentRecord, 0, limit)
	skipped := 0
	for i := len(m.incidents) - 1; i >= 0; i-- {
		rec := m.incidents[i]
		if filtered && rec.EffectiveOrigin() != origin {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		cp := *rec
		out = append(out, &cp)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memoryProvider) Close() error { return nil }

func (m *memoryProvider) SaveAnalysis(rec *AnalysisRecord) error {
	if rec == nil || rec.ID == "" {
		return ErrNotFound
	}
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, existing := range m.analyses {
		if existing.ID == rec.ID {
			m.analyses[i] = rec
			return nil
		}
	}
	m.analyses = append(m.analyses, rec)
	return nil
}

func (m *memoryProvider) GetAnalysis(id string) (*AnalysisRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rec := range m.analyses {
		if rec.ID == id {
			cp := *rec
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *memoryProvider) ListAnalysesByIncident(incidentID string, limit int) ([]*AnalysisRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := len(m.analyses)
	if n == 0 {
		return nil, nil
	}
	out := make([]*AnalysisRecord, 0)
	for i := n - 1; i >= 0; i-- {
		if m.analyses[i].IncidentID != incidentID {
			continue
		}
		cp := *m.analyses[i]
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memoryProvider) ListAnalyses(limit int) ([]*AnalysisRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := len(m.analyses)
	if n == 0 {
		return nil, nil
	}
	out := make([]*AnalysisRecord, 0, n)
	for i := n - 1; i >= 0; i-- {
		cp := *m.analyses[i]
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CountAnalyses implements the optional storage.AnalysisPager capability. The
// in-memory history is small, so a len() is the whole count.
func (m *memoryProvider) CountAnalyses() (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.analyses), nil
}

// ListAnalysesPage implements the optional storage.AnalysisPager capability:
// one bounded, newest-first page over the in-memory slice, skipping offset
// rows and returning at most limit rows.
func (m *memoryProvider) ListAnalysesPage(offset, limit int) ([]*AnalysisRecord, error) {
	if limit <= 0 {
		limit = DefaultAnalysisPageSize
	}
	if offset < 0 {
		offset = 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*AnalysisRecord, 0, limit)
	skipped := 0
	for i := len(m.analyses) - 1; i >= 0; i-- {
		if skipped < offset {
			skipped++
			continue
		}
		cp := *m.analyses[i]
		out = append(out, &cp)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memoryProvider) DeleteAnalysis(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, rec := range m.analyses {
		if rec.ID == id {
			m.analyses = append(m.analyses[:i], m.analyses[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// ---------------------------------------------------------------------------
// Lifecycle (implements the optional storage.Lifecycle capability)
// ---------------------------------------------------------------------------

func (m *memoryProvider) PurgeOlderThan(domain string, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch domain {
	case DomainIncidents:
		kept := make([]*IncidentRecord, 0, len(m.incidents))
		n := 0
		for _, rec := range m.incidents {
			if rec.CreatedAt.Before(cutoff) {
				n++
				continue
			}
			kept = append(kept, rec)
		}
		m.incidents = kept
		return n, nil
	case DomainAnalyses:
		kept := make([]*AnalysisRecord, 0, len(m.analyses))
		n := 0
		for _, rec := range m.analyses {
			if rec.RequestedAt.Before(cutoff) {
				n++
				continue
			}
			kept = append(kept, rec)
		}
		m.analyses = kept
		return n, nil
	case DomainBlobs:
		n := 0
		for name, at := range m.blobAt {
			if at.Before(cutoff) {
				delete(m.blobs, name)
				delete(m.blobAt, name)
				n++
			}
		}
		return n, nil
	default:
		return 0, ErrUnknownDomain
	}
}

func (m *memoryProvider) DeleteByID(domain, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch domain {
	case DomainIncidents:
		for i, rec := range m.incidents {
			if rec.ID == id {
				m.incidents = append(m.incidents[:i], m.incidents[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	case DomainAnalyses:
		for i, rec := range m.analyses {
			if rec.ID == id {
				m.analyses = append(m.analyses[:i], m.analyses[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	case DomainBlobs:
		if _, ok := m.blobs[id]; !ok {
			return ErrNotFound
		}
		delete(m.blobs, id)
		delete(m.blobAt, id)
		return nil
	default:
		return ErrUnknownDomain
	}
}
