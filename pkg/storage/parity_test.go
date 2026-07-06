package storage_test

// parity_test.go — runs the same incident and analysis CRUD suite against
// every backend so they stay behaviorally identical.
//
// Postgres tests are gated on the TEST_POSTGRES_DSN environment variable.
// If the variable is unset or empty the tests are skipped, not failed, so
// the standard CI loop (no live Postgres) stays green.
//
//   TEST_POSTGRES_DSN="postgres://user:pass@localhost:5432/testdb?sslmode=disable" \
//       go test ./pkg/storage/...

import (
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// ---------------------------------------------------------------------------
// Shared incident CRUD helper
// ---------------------------------------------------------------------------

func runIncidentCRUD(t *testing.T, p storage.Provider) {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Millisecond)

	i1 := &storage.IncidentRecord{
		ID:           "inc-1",
		Title:        "DB down",
		Source:       "webhook",
		Service:      "api",
		NotifyStatus: "sent",
		CreatedAt:    now.Add(-2 * time.Minute),
	}
	i2 := &storage.IncidentRecord{
		ID:           "inc-2",
		Title:        "High latency",
		Source:       "sns",
		NotifyStatus: "pending",
		CreatedAt:    now.Add(-time.Minute),
	}

	// Save two incidents.
	for _, inc := range []*storage.IncidentRecord{i1, i2} {
		if err := p.SaveIncident(inc); err != nil {
			t.Fatalf("SaveIncident(%s): %v", inc.ID, err)
		}
	}

	// GetIncident — known id.
	got, err := p.GetIncident("inc-1")
	if err != nil {
		t.Fatalf("GetIncident inc-1: %v", err)
	}
	if got.Title != "DB down" {
		t.Fatalf("title mismatch: %q", got.Title)
	}

	// GetIncident — unknown id.
	if _, err := p.GetIncident("does-not-exist"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing id, got %v", err)
	}

	// ListIncidents — newest first.
	list, err := p.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListIncidents len=%d, want 2", len(list))
	}
	if list[0].ID != "inc-2" {
		t.Fatalf("expected newest first, got %s", list[0].ID)
	}

	// ListIncidents — with limit.
	limited, err := p.ListIncidents(1)
	if err != nil {
		t.Fatalf("ListIncidents limit=1: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limited len=%d, want 1", len(limited))
	}

	// UpdateIncidentAck.
	ackAt := now
	if err := p.UpdateIncidentAck("inc-1", ackAt); err != nil {
		t.Fatalf("UpdateIncidentAck: %v", err)
	}
	acked, err := p.GetIncident("inc-1")
	if err != nil {
		t.Fatalf("GetIncident after ack: %v", err)
	}
	if acked.AckedAt == nil {
		t.Fatal("AckedAt should be set after ack")
	}

	// UpdateIncidentAck — unknown id.
	if err := p.UpdateIncidentAck("ghost", ackAt); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown ack, got %v", err)
	}

	// SaveIncident — upsert (overwrite i2).
	i2.NotifyStatus = "sent"
	if err := p.SaveIncident(i2); err != nil {
		t.Fatalf("SaveIncident upsert: %v", err)
	}
	updated, err := p.GetIncident("inc-2")
	if err != nil {
		t.Fatalf("GetIncident after upsert: %v", err)
	}
	if updated.NotifyStatus != "sent" {
		t.Fatalf("NotifyStatus after upsert=%q, want sent", updated.NotifyStatus)
	}
}

// ---------------------------------------------------------------------------
// Memory backend — incidents
// ---------------------------------------------------------------------------

func TestMemoryIncidentCRUD(t *testing.T) {
	runIncidentCRUD(t, storage.NewMemory())
}

// ---------------------------------------------------------------------------
// File backend — incidents
// ---------------------------------------------------------------------------

func TestFileIncidentCRUD(t *testing.T) {
	dir := t.TempDir()
	p, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer p.Close()
	runIncidentCRUD(t, p)
}

// ---------------------------------------------------------------------------
// Postgres backend helpers
// ---------------------------------------------------------------------------

func newTestPostgres(t *testing.T) storage.Provider {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	p, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	// The backend reuses one shared database, so every test must start from a
	// clean slate — truncate all vs_* tables the migrations created. Without
	// this, records from one test (e.g. "High latency") leak into another's
	// search/list assertions.
	truncateAllTables(t, dsn)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// truncateAllTables empties every vs_* table so each Postgres test runs in
// isolation against the shared test database.
func truncateAllTables(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'vs\_%'`)
	if err != nil {
		t.Fatalf("list vs_ tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	for _, tbl := range tables {
		// CASCADE: vs_logs FK-references vs_patterns (X28 typed signal tables),
		// so a plain per-table TRUNCATE of a referenced table is rejected.
		if _, err := db.Exec("TRUNCATE TABLE " + tbl + " CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Shared blob CRUD helper
// ---------------------------------------------------------------------------

// runBlobCRUD exercises the opaque-blob side of the Provider. Every blob
// consumer (agent catalog "patterns", shadow log "shadow", detect log
// "detect", AI cache "ai_cache", teams "members"/"teams") rides these two
// methods, so one round-trip per backend keeps them behaviorally identical.
func runBlobCRUD(t *testing.T, p storage.Provider) {
	t.Helper()

	// Missing blob MUST return (nil, nil) — not ErrNotFound — so the
	// agent's "fresh start" path stays a single line.
	got, err := p.ReadBlob("patterns")
	if err != nil {
		t.Fatalf("ReadBlob(missing): %v", err)
	}
	if got != nil {
		t.Fatalf("ReadBlob(missing) = %q, want nil", got)
	}

	// Write then read back the same bytes.
	payload := []byte(`{"version":1,"services":["api","db"]}`)
	if err := p.WriteBlob("patterns", payload); err != nil {
		t.Fatalf("WriteBlob(patterns): %v", err)
	}
	got, err = p.ReadBlob("patterns")
	if err != nil {
		t.Fatalf("ReadBlob(patterns): %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("ReadBlob(patterns) = %q, want %q", got, payload)
	}

	// Overwrite the same key (upsert) — last write wins.
	payload2 := []byte(`{"version":2,"services":["api"]}`)
	if err := p.WriteBlob("patterns", payload2); err != nil {
		t.Fatalf("WriteBlob(patterns) overwrite: %v", err)
	}
	got, err = p.ReadBlob("patterns")
	if err != nil {
		t.Fatalf("ReadBlob(patterns) after overwrite: %v", err)
	}
	if string(got) != string(payload2) {
		t.Fatalf("ReadBlob(patterns) after overwrite = %q, want %q", got, payload2)
	}

	// Distinct keys are isolated — writing "shadow" must not disturb
	// "patterns". This is the row-per-name contract of vs_blobs.
	if err := p.WriteBlob("shadow", []byte("shadow-data")); err != nil {
		t.Fatalf("WriteBlob(shadow): %v", err)
	}
	got, err = p.ReadBlob("patterns")
	if err != nil {
		t.Fatalf("ReadBlob(patterns) after shadow write: %v", err)
	}
	if string(got) != string(payload2) {
		t.Fatalf("patterns clobbered by shadow write: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Postgres backend — incidents
// ---------------------------------------------------------------------------

func TestPostgresIncidentCRUD(t *testing.T) {
	runIncidentCRUD(t, newTestPostgres(t))
}

// ---------------------------------------------------------------------------
// Blob round-trip — every backend (the 5 catalog/shadow/detect/teams
// JSON files collapse into the single vs_blobs table on Postgres)
// ---------------------------------------------------------------------------

func TestMemoryBlobCRUD(t *testing.T) {
	runBlobCRUD(t, storage.NewMemory())
}

func TestFileBlobCRUD(t *testing.T) {
	dir := t.TempDir()
	p, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer p.Close()
	runBlobCRUD(t, p)
}

func TestPostgresBlobCRUD(t *testing.T) {
	runBlobCRUD(t, newTestPostgres(t))
}

// ---------------------------------------------------------------------------
// Shared blob-listing helper — the namespaced enumeration the model-state
// seam (ModelStore.List) rides. Every backend must list a prefixed
// namespace identically: a model-state name like
// models/<org>/<agent>/<key> is one nested blob, and ListBlobs(prefix)
// must return exactly the blobs under that prefix and nothing else.
// ---------------------------------------------------------------------------

func runBlobListing(t *testing.T, p storage.Provider) {
	t.Helper()

	// An empty store lists nothing under any prefix — not an error.
	got, err := p.ListBlobs("models/")
	if err != nil {
		t.Fatalf("ListBlobs(empty store): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListBlobs(empty store) len = %d, want 0", len(got))
	}

	// Lay down a model-state namespace (two orgs, two agents) plus an
	// unrelated top-level blob that must never leak into a namespace list.
	blobs := map[string][]byte{
		"models/acme/intel-baseline/svcA~rate":   []byte(`{"k":"a1"}`),
		"models/acme/intel-baseline/svcB~rate":   []byte(`{"k":"a2"}`),
		"models/acme/intel-trace/svcA~op~p99":    []byte(`{"k":"t1"}`),
		"models/globex/intel-baseline/svcA~rate": []byte(`{"k":"g1"}`),
		"patterns":                               []byte(`{"doc":"patterns"}`),
	}
	for name, data := range blobs {
		if err := p.WriteBlob(name, data); err != nil {
			t.Fatalf("WriteBlob(%s): %v", name, err)
		}
	}

	// List one org+agent namespace: exactly the two baseline artifacts.
	got, err = p.ListBlobs("models/acme/intel-baseline/")
	if err != nil {
		t.Fatalf("ListBlobs(acme baseline): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListBlobs(acme baseline) len = %d, want 2", len(got))
	}
	for _, b := range got {
		want, ok := blobs[b.Name]
		if !ok {
			t.Fatalf("ListBlobs returned an unexpected name %q", b.Name)
		}
		if string(b.Data) != string(want) {
			t.Fatalf("ListBlobs[%s] = %q, want %q", b.Name, b.Data, want)
		}
	}

	// Listing the whole org spans both agents (2 baseline + 1 trace = 3).
	got, err = p.ListBlobs("models/acme/")
	if err != nil {
		t.Fatalf("ListBlobs(acme): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListBlobs(acme) len = %d, want 3", len(got))
	}

	// A different org's namespace is isolated.
	got, err = p.ListBlobs("models/globex/intel-baseline/")
	if err != nil {
		t.Fatalf("ListBlobs(globex baseline): %v", err)
	}
	if len(got) != 1 || got[0].Name != "models/globex/intel-baseline/svcA~rate" {
		t.Fatalf("ListBlobs(globex baseline) = %v, want one globex artifact", got)
	}

	// An org that never wrote anything lists nothing.
	got, err = p.ListBlobs("models/nobody/")
	if err != nil {
		t.Fatalf("ListBlobs(unknown org): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListBlobs(unknown org) len = %d, want 0", len(got))
	}

	// Mutating a returned Data slice must not corrupt stored state.
	got, err = p.ListBlobs("models/globex/intel-baseline/")
	if err != nil {
		t.Fatalf("ListBlobs(globex re-read): %v", err)
	}
	if len(got) == 1 {
		got[0].Data[0] = 'X'
	}
	reread, err := p.ReadBlob("models/globex/intel-baseline/svcA~rate")
	if err != nil {
		t.Fatalf("ReadBlob after mutation: %v", err)
	}
	if string(reread) != `{"k":"g1"}` {
		t.Fatalf("stored blob corrupted by caller mutation: %q", reread)
	}
}

func TestMemoryBlobListing(t *testing.T) {
	runBlobListing(t, storage.NewMemory())
}

func TestFileBlobListing(t *testing.T) {
	dir := t.TempDir()
	p, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer p.Close()
	runBlobListing(t, p)
}

func TestPostgresBlobListing(t *testing.T) {
	runBlobListing(t, newTestPostgres(t))
}

// ---------------------------------------------------------------------------
// Postgres backend — analyses (mirrors TestMemoryAnalysisCRUD /
// TestFileAnalysisCRUD from analyses_test.go)
// ---------------------------------------------------------------------------

func TestPostgresAnalysisCRUD(t *testing.T) {
	runAnalysisCRUD(t, newTestPostgres(t))
}

// ---------------------------------------------------------------------------
// Postgres backend — per-document blob tables
//
// Each remaining agent JSON document lands in its own table (shadow→vs_shadow,
// …). Writing one must not touch another, and an unknown name falls back to
// vs_blobs. "patterns" is NO LONGER a whole-blob table (X28: the log catalog
// moved to the typed vs_patterns/vs_logs/vs_services tables via the Postgres
// catalog store), so it now falls back to vs_blobs like any other name.
// ---------------------------------------------------------------------------

func TestPostgresBlobPerTable(t *testing.T) {
	p := newTestPostgres(t)

	cases := map[string][]byte{
		"shadow":   []byte(`{"doc":"shadow"}`),
		"detect":   []byte(`{"doc":"detect"}`),
		"members":  []byte(`{"doc":"members"}`),
		"teams":    []byte(`{"doc":"teams"}`),
		"patterns": []byte(`{"doc":"fallback-patterns"}`), // no dedicated table now → vs_blobs
		"ai_cache": []byte(`{"doc":"fallback"}`),          // no dedicated table → vs_blobs
	}
	for name, data := range cases {
		if err := p.WriteBlob(name, data); err != nil {
			t.Fatalf("WriteBlob(%s): %v", name, err)
		}
	}
	// Every document reads back exactly, proving the per-table routing
	// keeps them isolated.
	for name, want := range cases {
		got, err := p.ReadBlob(name)
		if err != nil {
			t.Fatalf("ReadBlob(%s): %v", name, err)
		}
		if string(got) != string(want) {
			t.Fatalf("ReadBlob(%s) = %q, want %q", name, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Postgres backend — search (the optional storage.Searcher capability)
// ---------------------------------------------------------------------------

func TestPostgresSearch(t *testing.T) {
	p := newTestPostgres(t)

	searcher, ok := p.(storage.Searcher)
	if !ok {
		t.Fatal("postgres backend must implement storage.Searcher")
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	incidents := []*storage.IncidentRecord{
		{ID: "s-1", Title: "Database connection pool exhausted", Service: "payments-api", Source: "webhook", CreatedAt: now.Add(-3 * time.Minute)},
		{ID: "s-2", Title: "Elevated p99 latency", Service: "checkout", Source: "sns", CreatedAt: now.Add(-2 * time.Minute)},
		{ID: "s-3", Title: "OOMKilled pod restart", Service: "payments-worker", Source: "webhook", CreatedAt: now.Add(-time.Minute)},
	}
	for _, inc := range incidents {
		if err := p.SaveIncident(inc); err != nil {
			t.Fatalf("SaveIncident(%s): %v", inc.ID, err)
		}
	}

	// Match by service substring — two payments-* services.
	got, err := searcher.SearchIncidents("payments", 0)
	if err != nil {
		t.Fatalf("SearchIncidents(payments): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("SearchIncidents(payments) len=%d, want 2", len(got))
	}
	// Newest first: s-3 before s-1.
	if got[0].ID != "s-3" || got[1].ID != "s-1" {
		t.Fatalf("SearchIncidents order = [%s %s], want [s-3 s-1]", got[0].ID, got[1].ID)
	}

	// Match by title (case-insensitive).
	got, err = searcher.SearchIncidents("LATENCY", 0)
	if err != nil {
		t.Fatalf("SearchIncidents(LATENCY): %v", err)
	}
	if len(got) != 1 || got[0].ID != "s-2" {
		t.Fatalf("SearchIncidents(LATENCY) = %v, want [s-2]", got)
	}

	// Limit is honoured.
	got, err = searcher.SearchIncidents("payments", 1)
	if err != nil {
		t.Fatalf("SearchIncidents(payments, limit=1): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("SearchIncidents limit=1 len=%d, want 1", len(got))
	}

	// No match → empty, no error.
	got, err = searcher.SearchIncidents("nonexistent-zzz", 0)
	if err != nil {
		t.Fatalf("SearchIncidents(nonexistent): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("SearchIncidents(nonexistent) len=%d, want 0", len(got))
	}

	// Empty query degrades to ListIncidents (most recent first).
	got, err = searcher.SearchIncidents("", 0)
	if err != nil {
		t.Fatalf("SearchIncidents(empty): %v", err)
	}
	if len(got) != 3 || got[0].ID != "s-3" {
		t.Fatalf("SearchIncidents(empty) = %d records, newest %s; want 3 / s-3", len(got), got[0].ID)
	}
}
