// Package runbook is the runbook-RAG corpus store and ingestion (write)
// path for the SRE agent's read-only find_runbook tool. It is
// deliberately SEPARATE from pkg/agent/ai/analyze/tools: the tool reads
// a vector index through a narrow local interface, while this package
// owns the write path (ingest + persist). Keeping the write path out of
// the tools package is what keeps the analyze import-graph guard green
// (a write in that package would trip the read-only guard).
//
// Persistence goes through storage.Provider (ReadBlob/WriteBlob), the
// same seam the pattern catalog and shadow log use — never os.WriteFile.
// Every record carries an OrgID (default storage.DefaultOrgID) so the
// enterprise org-injection seam scopes runbooks per-tenant with zero OSS
// change.
package runbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/runbook/vectorindex"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// ErrNotFound is returned by the manager's CRUD methods when no runbook
// exists under the given ID.
var ErrNotFound = errors.New("runbook: not found")

// BlobName is the storage blob key the runbook corpus is persisted
// under. Backends translate it into a path / redis key / row.
const BlobName = "runbooks"

// SourceSubdir is the subdirectory, under the storage data folder, where
// operators place their *.md runbook files for ingestion. The directory
// is <data folder>/runbooks (e.g. ./data/runbooks; /app/data/runbooks in
// the container image); the server auto-ingests it at boot.
const SourceSubdir = "runbooks"

const storeFileVersion = 1

// excerptMaxRunes bounds how much of a runbook body is surfaced to the
// model as a match excerpt, keeping tool output compact.
const excerptMaxRunes = 600

// Record is the durable shape of one runbook in the corpus. The cached
// Vector is the embedding of the runbook's indexed text, stored
// alongside the record so the corpus never needs re-embedding at boot.
type Record struct {
	ID string `json:"id"`
	// OrgID scopes the runbook to one organization. Defaults to
	// storage.DefaultOrgID ("default") so single-tenant OSS users never
	// see or set it; enterprise multi-tenant routing reads it to isolate
	// per-org corpora.
	OrgID     string    `json:"org_id,omitempty"`
	Title     string    `json:"title"`
	Services  []string  `json:"services,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	Body      string    `json:"body"`
	Source    string    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Vector    []float32 `json:"vector,omitempty"`
	// ContentHash is the SHA-256 of the indexed text (title + body) at
	// embed time. Auto-ingest compares it to skip re-embedding runbooks
	// whose content is unchanged, so a reboot with no edits makes no
	// embedding calls.
	ContentHash string `json:"content_hash,omitempty"`
}

// storeFile is the on-disk schema. Versioned so the record struct can
// evolve without breaking existing blobs.
type storeFile struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Records   []Record  `json:"records"`
}

// Store is the in-memory + blob-backed runbook corpus. All methods are
// safe for concurrent use. The read side (All / BuildIndex) is consumed
// at boot to build the vector index; the write side (Upsert / Persist)
// is the ingestion path.
type Store struct {
	mu      sync.RWMutex
	store   storage.Provider
	records map[string]Record
}

// LoadStore opens the existing runbook blob from the storage provider,
// or returns an empty store when none exists (a fresh corpus). A nil
// provider yields an empty, memory-only store.
func LoadStore(store storage.Provider) (*Store, error) {
	s := &Store{store: store, records: make(map[string]Record)}
	if store == nil {
		return s, nil
	}
	data, err := store.ReadBlob(BlobName)
	if err != nil {
		return s, fmt.Errorf("runbook: read corpus: %w", err)
	}
	if len(data) == 0 {
		return s, nil // fresh start
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return s, fmt.Errorf("runbook: parse corpus: %w", err)
	}
	for _, r := range f.Records {
		r.OrgID = storage.NormalizeOrgID(r.OrgID)
		s.records[r.ID] = r
	}
	return s, nil
}

// Upsert inserts or replaces a record by ID. A blank OrgID is
// normalized to storage.DefaultOrgID. This is the write path; it is
// called by ingestion, never by the read-only tool.
func (s *Store) Upsert(rec Record) {
	if s == nil || rec.ID == "" {
		return
	}
	rec.OrgID = storage.NormalizeOrgID(rec.OrgID)
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	s.records[rec.ID] = rec
	s.mu.Unlock()
}

// Len reports the number of records in the corpus.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records)
}

// lookup returns the record stored under id, if any. Used by ingestion
// to reuse a cached vector when the runbook content is unchanged.
func (s *Store) lookup(id string) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[id]
	return r, ok
}

// Get returns the record stored under id, if any. It is the read side of
// the admin CRUD path (the manager surfaces it to the runbooks UI).
func (s *Store) Get(id string) (Record, bool) {
	return s.lookup(id)
}

// Delete removes the record stored under id and reports whether a record
// was present. It is the delete side of the admin CRUD path; callers
// Persist() afterwards to make the removal durable.
func (s *Store) Delete(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[id]; !ok {
		return false
	}
	delete(s.records, id)
	return true
}

// All returns every record, ordered by ID for determinism.
func (s *Store) All() []Record {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Persist writes the whole corpus to the storage backend via WriteBlob.
// A nil provider is a no-op (memory-only store). This is part of the
// write path.
func (s *Store) Persist() error {
	if s == nil || s.store == nil {
		return nil
	}
	s.mu.RLock()
	recs := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		recs = append(recs, r)
	}
	s.mu.RUnlock()
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })

	f := storeFile{
		Version:   storeFileVersion,
		UpdatedAt: time.Now().UTC(),
		Records:   recs,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("runbook: marshal corpus: %w", err)
	}
	if err := s.store.WriteBlob(BlobName, data); err != nil {
		return fmt.Errorf("runbook: write corpus: %w", err)
	}
	return nil
}

// BuildIndex constructs an in-memory cosine index from the cached
// vectors in the corpus. maxDocs bounds the index size. Records without
// a cached vector are skipped (they were never embedded). The returned
// index is read-only and safe to hand to the find_runbook tool's
// searcher bridge. An empty corpus yields a non-nil, empty index so the
// tool registers and cleanly reports Found:false.
func (s *Store) BuildIndex(maxDocs int) *vectorindex.Memory {
	idx := vectorindex.NewMemory(maxDocs)
	if s == nil {
		return idx
	}
	for _, r := range s.All() {
		if len(r.Vector) == 0 {
			continue
		}
		idx.Add(vectorindex.Doc{
			ID:      r.ID,
			Title:   r.Title,
			Service: primaryService(r.Services),
			Source:  r.Source,
			Excerpt: excerpt(r.Body),
			Vector:  r.Vector,
		})
	}
	return idx
}

// primaryService returns the first non-empty service of a record (the
// vector index keys its optional service filter on a single name).
func primaryService(services []string) string {
	for _, s := range services {
		if s != "" {
			return s
		}
	}
	return ""
}

// excerpt trims a runbook body to a model-friendly length on a rune
// boundary.
func excerpt(body string) string {
	r := []rune(body)
	if len(r) <= excerptMaxRunes {
		return body
	}
	return string(r[:excerptMaxRunes])
}
