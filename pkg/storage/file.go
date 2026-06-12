package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// MaxIncidentsDefault is the default rolling cap for the file backend.
// Older records are dropped on SaveIncident when the cap is exceeded.
const MaxIncidentsDefault = 1000

// FileOptions configures the file backend. Empty fields fall back to
// sensible defaults.
type FileOptions struct {
	DataDir      string // default DefaultDataDir
	MaxIncidents int    // default MaxIncidentsDefault
}

// fileProvider stores blobs as <DataDir>/<name>.json and incidents as
// a single <DataDir>/incidents.json file kept in memory for fast list
// queries. All writes are atomic (tmp + rename).
type fileProvider struct {
	dir          string
	maxIncidents int

	mu        sync.RWMutex
	incidents []*IncidentRecord // newest last; persisted as is
	loaded    bool

	analysesMu     sync.RWMutex
	analyses       []*AnalysisRecord // newest last
	analysesLoaded bool
}

// NewFile returns a Provider backed by the local filesystem.
func NewFile(opts FileOptions) (Provider, error) {
	dir := opts.DataDir
	if dir == "" {
		dir = DefaultDataDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: mkdir %s: %w", dir, err)
	}
	max := opts.MaxIncidents
	if max <= 0 {
		max = MaxIncidentsDefault
	}
	p := &fileProvider{dir: dir, maxIncidents: max}
	if err := p.loadIncidents(); err != nil {
		return nil, err
	}
	if err := p.loadAnalyses(); err != nil {
		return nil, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Blobs
// ---------------------------------------------------------------------------

func (p *fileProvider) blobPath(name string) string {
	return filepath.Join(p.dir, name+".json")
}

func (p *fileProvider) ReadBlob(name string) ([]byte, error) {
	data, err := os.ReadFile(p.blobPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read blob %s: %w", name, err)
	}
	return data, nil
}

func (p *fileProvider) WriteBlob(name string, data []byte) error {
	target := p.blobPath(name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("storage: mkdir blob dir: %w", err)
	}
	if err := writeFileAtomicSync(target, data, 0o644); err != nil {
		return fmt.Errorf("storage: write blob %s: %w", name, err)
	}
	return nil
}

// writeFileAtomicSync writes data to a sibling tmp file, fsyncs it,
// then renames over the target. The fsync between write and rename is
// the load-bearing line: without it, a power loss between rename and
// fs commit can replace a previous good file with a zero-length one.
// (The rename itself is journaled by ext4/xfs; the tmp file's *data*
// isn't unless we sync.)
func writeFileAtomicSync(target string, data []byte, mode os.FileMode) error {
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	// Cleanup the tmp file on any error path before rename.
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Incidents
// ---------------------------------------------------------------------------

const incidentsFile = "incidents.json"

type incidentsFileSchema struct {
	Version   int               `json:"version"`
	UpdatedAt time.Time         `json:"updated_at"`
	Incidents []*IncidentRecord `json:"incidents"`
}

const incidentsFileVersion = 1

func (p *fileProvider) loadIncidents() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		return nil
	}
	p.loaded = true

	path := filepath.Join(p.dir, incidentsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("storage: read incidents: %w", err)
	}
	var f incidentsFileSchema
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("storage: parse incidents: %w", err)
	}
	p.incidents = f.Incidents
	// Defensive: sort by CreatedAt ascending so newest is last.
	sort.SliceStable(p.incidents, func(i, j int) bool {
		return p.incidents[i].CreatedAt.Before(p.incidents[j].CreatedAt)
	})
	return nil
}

func (p *fileProvider) persistIncidentsLocked() error {
	f := incidentsFileSchema{
		Version:   incidentsFileVersion,
		UpdatedAt: time.Now().UTC(),
		Incidents: p.incidents,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("storage: marshal incidents: %w", err)
	}
	target := filepath.Join(p.dir, incidentsFile)
	if err := writeFileAtomicSync(target, data, 0o644); err != nil {
		return fmt.Errorf("storage: write incidents: %w", err)
	}
	return nil
}

func (p *fileProvider) SaveIncident(rec *IncidentRecord) error {
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("storage: SaveIncident: missing id")
	}
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	p.mu.Lock()
	defer p.mu.Unlock()

	// Update in place if the id already exists (e.g. ack flow).
	for i, existing := range p.incidents {
		if existing.ID == rec.ID {
			p.incidents[i] = rec
			return p.persistIncidentsLocked()
		}
	}

	// Append + trim.
	p.incidents = append(p.incidents, rec)
	if over := len(p.incidents) - p.maxIncidents; over > 0 {
		p.incidents = append([]*IncidentRecord(nil), p.incidents[over:]...)
	}
	return p.persistIncidentsLocked()
}

func (p *fileProvider) UpdateIncidentAck(id string, ackedAt time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, rec := range p.incidents {
		if rec.ID == id {
			t := ackedAt.UTC()
			rec.AckedAt = &t
			return p.persistIncidentsLocked()
		}
	}
	return ErrNotFound
}

func (p *fileProvider) GetIncident(id string) (*IncidentRecord, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, rec := range p.incidents {
		if rec.ID == id {
			cp := *rec
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (p *fileProvider) ListIncidents(limit int) ([]*IncidentRecord, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := len(p.incidents)
	if n == 0 {
		return nil, nil
	}
	// Newest first.
	out := make([]*IncidentRecord, 0, n)
	for i := n - 1; i >= 0; i-- {
		cp := *p.incidents[i]
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (p *fileProvider) Close() error { return nil }

// ---------------------------------------------------------------------------
// Analyses
// ---------------------------------------------------------------------------

const (
	analysesFile        = "analyses.json"
	analysesFileVersion = 1
	maxAnalysesDefault  = 500
)

type analysesFileSchema struct {
	Version   int               `json:"version"`
	UpdatedAt time.Time         `json:"updated_at"`
	Analyses  []*AnalysisRecord `json:"analyses"`
}

func (p *fileProvider) loadAnalyses() error {
	p.analysesMu.Lock()
	defer p.analysesMu.Unlock()
	if p.analysesLoaded {
		return nil
	}
	p.analysesLoaded = true

	path := filepath.Join(p.dir, analysesFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("storage: read analyses: %w", err)
	}
	var f analysesFileSchema
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("storage: parse analyses: %w", err)
	}
	p.analyses = f.Analyses
	sort.SliceStable(p.analyses, func(i, j int) bool {
		return p.analyses[i].RequestedAt.Before(p.analyses[j].RequestedAt)
	})
	return nil
}

func (p *fileProvider) persistAnalysesLocked() error {
	f := analysesFileSchema{
		Version:   analysesFileVersion,
		UpdatedAt: time.Now().UTC(),
		Analyses:  p.analyses,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("storage: marshal analyses: %w", err)
	}
	target := filepath.Join(p.dir, analysesFile)
	if err := writeFileAtomicSync(target, data, 0o644); err != nil {
		return fmt.Errorf("storage: write analyses: %w", err)
	}
	return nil
}

func (p *fileProvider) SaveAnalysis(rec *AnalysisRecord) error {
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("storage: SaveAnalysis: missing id")
	}
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	p.analysesMu.Lock()
	defer p.analysesMu.Unlock()
	for i, existing := range p.analyses {
		if existing.ID == rec.ID {
			p.analyses[i] = rec
			return p.persistAnalysesLocked()
		}
	}
	p.analyses = append(p.analyses, rec)
	if over := len(p.analyses) - maxAnalysesDefault; over > 0 {
		p.analyses = append([]*AnalysisRecord(nil), p.analyses[over:]...)
	}
	return p.persistAnalysesLocked()
}

func (p *fileProvider) GetAnalysis(id string) (*AnalysisRecord, error) {
	p.analysesMu.RLock()
	defer p.analysesMu.RUnlock()
	for _, rec := range p.analyses {
		if rec.ID == id {
			cp := *rec
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (p *fileProvider) ListAnalysesByIncident(incidentID string, limit int) ([]*AnalysisRecord, error) {
	p.analysesMu.RLock()
	defer p.analysesMu.RUnlock()
	n := len(p.analyses)
	if n == 0 {
		return nil, nil
	}
	out := make([]*AnalysisRecord, 0)
	for i := n - 1; i >= 0; i-- {
		if p.analyses[i].IncidentID != incidentID {
			continue
		}
		cp := *p.analyses[i]
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (p *fileProvider) ListAnalyses(limit int) ([]*AnalysisRecord, error) {
	p.analysesMu.RLock()
	defer p.analysesMu.RUnlock()
	n := len(p.analyses)
	if n == 0 {
		return nil, nil
	}
	out := make([]*AnalysisRecord, 0, n)
	for i := n - 1; i >= 0; i-- {
		cp := *p.analyses[i]
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (p *fileProvider) DeleteAnalysis(id string) error {
	p.analysesMu.Lock()
	defer p.analysesMu.Unlock()
	for i, rec := range p.analyses {
		if rec.ID == id {
			p.analyses = append(p.analyses[:i], p.analyses[i+1:]...)
			return p.persistAnalysesLocked()
		}
	}
	return ErrNotFound
}
