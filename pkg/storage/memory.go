package storage

import (
	"sync"
	"time"
)

// memoryProvider is an in-memory backend used by tests. It is concurrency-safe
// and never returns transport errors. Not exposed as a config option.
type memoryProvider struct {
	mu        sync.RWMutex
	blobs     map[string][]byte
	incidents []*IncidentRecord
	analyses  []*AnalysisRecord
}

// NewMemory returns a Provider that keeps all state in memory. Intended
// for tests; never select via the config factory.
func NewMemory() Provider {
	return &memoryProvider{blobs: make(map[string][]byte)}
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
	return nil
}

func (m *memoryProvider) SaveIncident(rec *IncidentRecord) error {
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

func (m *memoryProvider) Close() error { return nil }

func (m *memoryProvider) SaveAnalysis(rec *AnalysisRecord) error {
	if rec == nil || rec.ID == "" {
		return ErrNotFound
	}
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
