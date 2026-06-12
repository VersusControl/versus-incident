package runbook

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/runbook/vectorindex"
)

// UploadFile is one operator-uploaded runbook: the original filename and
// its raw `.md` bytes. The filename (basename only) becomes the runbook's
// stable ID and Source, so re-uploading a file with the same name
// replaces the existing runbook.
type UploadFile struct {
	Name    string
	Content []byte
}

// Manager owns the runbook corpus write path plus a live, swappable
// search index for the find_runbook tool. It is the single seam the
// admin runbooks UI (CRUD via the controller) and the agent's read path
// (find_runbook) share: uploads and deletes mutate the corpus and
// atomically swap in a freshly-built index, so edits are searchable
// immediately without a server restart.
//
// The Manager lives in pkg/runbook (the write path), NOT in
// pkg/agent/ai/analyze/tools — the tool only ever sees the read-only
// vectorindex.Index returned by Index(), keeping the analyze read-only
// import-graph guard green.
type Manager struct {
	mu       sync.Mutex // serializes mutations (upload/delete/ingest)
	store    *Store
	embedder core.Embedder // may be nil when embeddings are not configured
	index    atomic.Pointer[vectorindex.Memory]
}

// NewManager builds a manager over an existing corpus store and an
// optional embedder. It snapshots the current corpus into the live
// index so reads work immediately. A nil embedder is allowed: CRUD still
// works, but uploaded runbooks are stored without vectors (not
// searchable until embeddings are configured and the corpus re-ingested).
func NewManager(store *Store, embedder core.Embedder) *Manager {
	m := &Manager{store: store, embedder: embedder}
	m.index.Store(store.BuildIndex(0))
	return m
}

// Index returns the read-only search seam over the manager's live index.
// The find_runbook tool consumes this; every mutation atomically swaps a
// freshly-built index underneath it, so searches always see the latest
// corpus.
func (m *Manager) Index() vectorindex.Index {
	return &liveIndex{m: m}
}

// HasEmbedder reports whether the manager can embed uploads. When false,
// uploads persist without vectors (not searchable yet).
func (m *Manager) HasEmbedder() bool {
	return m != nil && m.embedder != nil
}

// Embedder returns the manager's query embedder (may be nil). The
// find_runbook tool uses it to embed search queries against the same
// model the corpus was embedded with.
func (m *Manager) Embedder() core.Embedder {
	if m == nil {
		return nil
	}
	return m.embedder
}

// List returns every runbook record, ordered by ID.
func (m *Manager) List() []Record {
	if m == nil {
		return nil
	}
	return m.store.All()
}

// Get returns the runbook stored under id, or ErrNotFound.
func (m *Manager) Get(id string) (Record, error) {
	if m == nil {
		return Record{}, ErrNotFound
	}
	rec, ok := m.store.Get(id)
	if !ok {
		return Record{}, ErrNotFound
	}
	return rec, nil
}

// Delete removes the runbook stored under id, persists the corpus, and
// rebuilds the live index. Returns ErrNotFound when no such runbook
// exists.
func (m *Manager) Delete(id string) error {
	if m == nil {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.store.Delete(id) {
		return ErrNotFound
	}
	if err := m.store.Persist(); err != nil {
		return err
	}
	m.rebuildIndexLocked()
	return nil
}

// Upload ingests one or more uploaded `.md` runbook files: each is
// parsed (honoring optional YAML front-matter), embedded (when an
// embedder is configured), upserted, and persisted, then the live index
// is rebuilt so the new runbooks are immediately searchable. Re-uploading
// a file with the same basename replaces the existing runbook. Returns
// the number of runbooks written.
func (m *Manager) Upload(ctx context.Context, files []UploadFile, orgID string) (int, error) {
	if m == nil {
		return 0, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	items := make([]ingestItem, 0, len(files))
	for _, f := range files {
		rel := uploadName(f.Name)
		rec := parseRunbook(f.Content, rel, orgID)
		rec.Source = rel
		items = append(items, ingestItem{rec: rec, text: rec.Title + "\n\n" + rec.Body})
	}

	n, err := ingestItems(ctx, m.store, m.embedder, items)
	if err != nil {
		return 0, err
	}
	m.rebuildIndexLocked()
	return n, nil
}

// IngestDir scans dir for `*.md` runbooks and ingests them through the
// shared write path, then rebuilds the live index. It is the boot-time
// auto-ingest entry point. With no embedder configured it is a no-op
// (find_runbook is disabled, so there is nothing to embed). A missing
// dir is treated as an empty corpus.
func (m *Manager) IngestDir(ctx context.Context, dir, orgID string) (int, error) {
	if m == nil {
		return 0, ErrNotFound
	}
	if m.embedder == nil {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := IngestDir(ctx, m.store, m.embedder, dir, orgID)
	m.rebuildIndexLocked()
	return n, err
}

// rebuildIndexLocked rebuilds the in-memory cosine index from the current
// corpus and atomically swaps it in. Callers must hold m.mu.
func (m *Manager) rebuildIndexLocked() {
	m.index.Store(m.store.BuildIndex(0))
}

// uploadName reduces an uploaded filename to a safe, stable runbook ID:
// the slash-normalized basename with a guaranteed `.md` extension and no
// directory components (defeats path traversal in the upload name).
func uploadName(name string) string {
	base := filepath.Base(filepath.ToSlash(name))
	if base == "" || base == "." || base == "/" {
		base = "runbook"
	}
	if !hasMarkdownExt(base) {
		base += ".md"
	}
	return base
}

func hasMarkdownExt(name string) bool {
	ext := filepath.Ext(name)
	return ext == ".md" || ext == ".MD" || ext == ".Md" || ext == ".mD"
}

// liveIndex is a thin read-only view over the manager's atomically-
// swapped index. It implements vectorindex.Index so the find_runbook
// tool always searches the latest corpus snapshot.
type liveIndex struct{ m *Manager }

// Search implements vectorindex.Index by delegating to the current index
// snapshot. A nil snapshot (never built) yields no results.
func (l *liveIndex) Search(query []float32, service string, limit int) []vectorindex.Result {
	idx := l.m.index.Load()
	if idx == nil {
		return nil
	}
	return idx.Search(query, service, limit)
}
