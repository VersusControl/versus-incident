package agent

// catalog_pg_store_test.go — X28-A4. Two layers:
//
//  1. SQLi-safety / query-construction unit tests that run EVERYWHERE (no DB):
//     every query is a static constant that names only the fixed signal
//     tables and binds values as $N parameters — never fmt-interpolated.
//  2. A full catalog-lifecycle round-trip against a live Postgres, gated on
//     TEST_POSTGRES_DSN, that drives the PUBLIC Catalog API with the store
//     installed (Upsert/Persist/Snapshot, Label/MarkKnown/Delete, the samples
//     ring, RegisterService/grace, manual-service CRUD, and both resets).

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// allCatalogQueries is every SQL string the Postgres catalog store issues.
var allCatalogQueries = []string{
	sqlCatalogLoadLogs, sqlCatalogSelectServices, sqlCatalogUpsertRoot,
	sqlCatalogUpsertLog, sqlCatalogInsertServiceIfAbsent, sqlCatalogSnapshotLogs,
	sqlCurateVerdict, sqlCurateTags, sqlCurateMarkKnown, sqlCurateRepointService,
	sqlCurateDelete, sqlCurateResetPatterns, sqlCurateResetServices,
	sqlCurateEndGrace, sqlCurateRestartGrace, sqlCurateCreateService,
	sqlCurateDeleteService, sqlRenameSelectService, sqlRenameTombstoneOld,
	sqlRenameUpsertNewSvc,
}

// TestCatalogQueries_NoFormatVerbs proves no query carries a printf verb — the
// tables are Go constants embedded literally, never interpolated, so there is
// no dynamic-SQL surface (A03).
func TestCatalogQueries_NoFormatVerbs(t *testing.T) {
	for _, q := range allCatalogQueries {
		for _, verb := range []string{"%s", "%d", "%v", "%q", "%w"} {
			if strings.Contains(q, verb) {
				t.Fatalf("query contains format verb %q (dynamic SQL): %s", verb, q)
			}
		}
	}
}

// TestCatalogQueries_OnlyKnownTables proves every query touches ONLY the three
// signal tables — no stray table name, no enterprise vs_metrics/vs_traces.
func TestCatalogQueries_OnlyKnownTables(t *testing.T) {
	for _, q := range allCatalogQueries {
		lower := strings.ToLower(q)
		if strings.Contains(lower, "vs_metrics") || strings.Contains(lower, "vs_traces") {
			t.Fatalf("OSS catalog query must not name an enterprise table: %s", q)
		}
		if !strings.Contains(lower, "vs_patterns") &&
			!strings.Contains(lower, "vs_logs") &&
			!strings.Contains(lower, "vs_services") {
			t.Fatalf("query names no known signal table: %s", q)
		}
	}
}

// TestCatalogQueries_ParameterizedOrgScope proves every org-scoped statement
// binds org_id as a parameter ($1), so tenant isolation is a bound value and
// the id/service/name are never concatenated (A03 + tenant isolation).
func TestCatalogQueries_ParameterizedOrgScope(t *testing.T) {
	for _, q := range allCatalogQueries {
		if strings.Contains(q, "org_id") && !strings.Contains(q, "$1") {
			t.Fatalf("org-scoped query missing a bound $1 parameter: %s", q)
		}
	}
}

// TestNewPostgresCatalogStore_OrgNormalized proves an empty org is normalized
// to the default deployment org (never a blank org_id on the write path).
func TestNewPostgresCatalogStore_OrgNormalized(t *testing.T) {
	s := NewPostgresCatalogStore(nil, "", 0).(*pgCatalogStore)
	if s.orgID != storage.DefaultOrgID {
		t.Fatalf("orgID = %q, want %q", s.orgID, storage.DefaultOrgID)
	}
}

// ---------------------------------------------------------------------------
// Live-Postgres lifecycle round-trip (gated on TEST_POSTGRES_DSN)
// ---------------------------------------------------------------------------

func newPGCatalog(t *testing.T) (*Catalog, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	store, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	acc, ok := store.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres provider must implement storage.SQLAccessor")
	}
	db := acc.DB()
	// Fresh slate: the typed signal tables only (CASCADE clears vs_logs too).
	if _, err := db.Exec(`TRUNCATE TABLE vs_patterns, vs_logs, vs_services CASCADE`); err != nil {
		t.Fatalf("truncate signal tables: %v", err)
	}

	SetCatalogStore(NewPostgresCatalogStore(db, storage.DefaultOrgID, 0))
	t.Cleanup(func() {
		SetCatalogStore(nil)
		_ = store.Close()
	})

	cat, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	return cat, db
}

func patternByID(ps []*Pattern, id string) *Pattern {
	for _, p := range ps {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// TestPGCatalog_PatternLifecycle exercises the log-pattern half end to end.
func TestPGCatalog_PatternLifecycle(t *testing.T) {
	cat, db := newPGCatalog(t)

	// Learn two patterns across two ticks, with a sample on one.
	cat.Upsert("p1", "template one", "src-a", 3, 0.2, "default", "checkout")
	cat.Upsert("p2", "template two", "src-b", 1, 0.2, "rule-x", "")
	cat.RecordSample("p1", "GET /checkout 500 error", nil)
	cat.Upsert("p1", "template one", "src-a", 2, 0.2, "default", "checkout")
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Snapshot (fleet read) via All(): both patterns, summed counts, sample.
	all := cat.All()
	if len(all) != 2 {
		t.Fatalf("All() len = %d, want 2", len(all))
	}
	p1 := patternByID(all, "p1")
	if p1 == nil || p1.Count != 5 {
		t.Fatalf("p1 count = %v, want 5", p1)
	}
	if p1.Service != "checkout" {
		t.Fatalf("p1 service = %q, want checkout", p1.Service)
	}
	if len(p1.Samples) != 1 || p1.Samples[0] != "GET /checkout 500 error" {
		t.Fatalf("p1 samples = %v, want one redacted sample", p1.Samples)
	}

	// instance_index defaults to 0 on the single-instance OSS write path.
	var idx int
	if err := db.QueryRow(
		`SELECT instance_index FROM vs_logs WHERE org_id=$1 AND pattern_id='p1'`,
		storage.DefaultOrgID,
	).Scan(&idx); err != nil {
		t.Fatalf("read instance_index: %v", err)
	}
	if idx != 0 {
		t.Fatalf("instance_index = %d, want 0", idx)
	}

	// Label: set verdict + tags (curated root columns).
	known := "known"
	if !cat.Label("p2", &known, []string{"noise"}) {
		t.Fatal("Label p2 returned false")
	}
	p2 := patternByID(cat.All(), "p2")
	if p2 == nil || p2.Verdict != "known" || len(p2.Tags) != 1 || p2.Tags[0] != "noise" {
		t.Fatalf("p2 after label = %+v", p2)
	}

	// Clear verdict (tri-state &""): p2 verdict blanks fleet-wide.
	clear := ""
	if !cat.Label("p2", &clear, nil) {
		t.Fatal("Label clear returned false")
	}
	if got := patternByID(cat.All(), "p2"); got.Verdict != "" {
		t.Fatalf("p2 verdict after clear = %q, want empty", got.Verdict)
	}

	// MarkKnown twice: the second is a churn-cached no-op (still verdict known).
	if !cat.MarkKnown("p1") {
		t.Fatal("MarkKnown p1 returned false")
	}
	_ = cat.MarkKnown("p1") // no-op, must not error
	if got := patternByID(cat.All(), "p1"); got.Verdict != "known" {
		t.Fatalf("p1 verdict = %q, want known", got.Verdict)
	}

	// Delete (tombstone): p2 disappears from the read view.
	if !cat.Delete("p2") {
		t.Fatal("Delete p2 returned false")
	}
	if patternByID(cat.All(), "p2") != nil {
		t.Fatal("p2 still present after delete")
	}
	if len(cat.All()) != 1 {
		t.Fatalf("All() len after delete = %d, want 1", len(cat.All()))
	}

	// ResetPatterns wipes the log half (FK cascade clears vs_logs).
	n, err := cat.ResetPatterns()
	if err != nil {
		t.Fatalf("ResetPatterns: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetPatterns removed %d, want 1 (the pre-reset visible count)", n)
	}
	if len(cat.All()) != 0 {
		t.Fatalf("All() after reset = %d, want 0", len(cat.All()))
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM vs_logs WHERE org_id=$1`, storage.DefaultOrgID).Scan(&rows); err != nil {
		t.Fatalf("count vs_logs: %v", err)
	}
	if rows != 0 {
		t.Fatalf("vs_logs rows after reset = %d, want 0 (FK cascade)", rows)
	}
}

// TestPGCatalog_ServiceLifecycle exercises the discovered/manual service half.
func TestPGCatalog_ServiceLifecycle(t *testing.T) {
	cat, _ := newPGCatalog(t)

	// Discovery rides Persist.
	if !cat.RegisterService("payments") {
		t.Fatal("RegisterService payments returned false (want newly registered)")
	}
	if cat.RegisterService("payments") {
		t.Fatal("second RegisterService payments returned true (want already-known)")
	}
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if _, ok := cat.AllServices()["payments"]; !ok {
		t.Fatal("payments not in AllServices after persist")
	}

	// Grace: in window, then end it. IsServiceInGrace reads the in-memory
	// working set; grace edits route through Curate (DB) and are
	// eventually-consistent via the read view / next Load — the shared
	// CatalogStore contract (the enterprise partition store behaves the same).
	// So assert grace-in-window on the in-memory anchor, and grace-ended on the
	// fleet read view (AllServices → Snapshot → DB).
	if !cat.IsServiceInGrace("payments", time.Hour) {
		t.Fatal("payments should be within a 1h grace window")
	}
	if !cat.EndServiceGrace("payments") {
		t.Fatal("EndServiceGrace payments returned false")
	}
	ended := cat.AllServices()["payments"]
	if time.Now().UTC().Before(ended.FirstSeen.Add(time.Hour)) {
		t.Fatalf("payments grace anchor %v still within a 1h window after EndServiceGrace", ended.FirstSeen)
	}

	// Manual create — selectable before any signal, origin preserved.
	if err := cat.CreateService("billing"); err != nil {
		t.Fatalf("CreateService billing: %v", err)
	}
	if info, ok := cat.AllServices()["billing"]; !ok || !info.Manual {
		t.Fatalf("billing manual service missing/not manual: %+v ok=%v", info, ok)
	}

	// Rename manual service: old gone, new present + still manual.
	if err := cat.RenameService("billing", "billing-v2"); err != nil {
		t.Fatalf("RenameService: %v", err)
	}
	svcs := cat.AllServices()
	if _, ok := svcs["billing"]; ok {
		t.Fatal("old service name still present after rename")
	}
	if info, ok := svcs["billing-v2"]; !ok || !info.Manual {
		t.Fatalf("renamed service missing/not manual: %+v ok=%v", info, ok)
	}

	// Delete manual service (tombstone) — dropped from the read view.
	if !cat.DeleteService("billing-v2") {
		t.Fatal("DeleteService returned false")
	}
	if _, ok := cat.AllServices()["billing-v2"]; ok {
		t.Fatal("deleted service still present")
	}

	// ResetServices wipes them all, leaving patterns untouched.
	if _, err := cat.ResetServices(); err != nil {
		t.Fatalf("ResetServices: %v", err)
	}
	if len(cat.AllServices()) != 0 {
		t.Fatalf("AllServices after reset = %d, want 0", len(cat.AllServices()))
	}
}

// TestPGCatalog_ReloadRoundTrip proves persisted learned + curated state
// survives a fresh Load (a process restart) — the boot read is the same view.
func TestPGCatalog_ReloadRoundTrip(t *testing.T) {
	cat, db := newPGCatalog(t)

	cat.Upsert("keep", "kept template", "src", 4, 0.2, "default", "orders")
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	known := "known"
	cat.Label("keep", &known, []string{"routine"})

	// Fresh store + Load against the same DB simulates a restart.
	reloaded := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	patterns, _, err := reloaded.Load()
	if err != nil {
		t.Fatalf("reload Load: %v", err)
	}
	got := patterns["keep"]
	if got == nil {
		t.Fatal("pattern 'keep' missing after reload")
	}
	if got.Count != 4 || got.Service != "orders" || got.Verdict != "known" {
		t.Fatalf("reloaded pattern = %+v, want count=4 service=orders verdict=known", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "routine" {
		t.Fatalf("reloaded tags = %v, want [routine]", got.Tags)
	}
}

// redactScrubber redacts a fixed secret token so the storage-boundary re-scrub
// is observable (mirrors the enterprise store_pg_test.go scrubber).
type redactScrubber struct{ secret string }

func (r redactScrubber) Scrub(s string) string {
	return strings.ReplaceAll(s, r.secret, "<REDACTED>")
}

// TestPGCatalog_RedactionAtStorageBoundary (B57) proves a secret planted
// directly in a pattern's samples ring — bypassing the learn-boundary scrub —
// is re-scrubbed at the STORAGE boundary before it reaches vs_logs, so no
// secret ever lands in a signal table. It mirrors the enterprise
// TestPGBaseline_RedactionAtStorageBoundary so both signal-table write paths
// are defence-in-depth-equal.
func TestPGCatalog_RedactionAtStorageBoundary(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	store, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	acc, ok := store.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres provider must implement storage.SQLAccessor")
	}
	db := acc.DB()
	if _, err := db.Exec(`TRUNCATE TABLE vs_patterns, vs_logs, vs_services CASCADE`); err != nil {
		t.Fatalf("truncate signal tables: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cs := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0).(*pgCatalogStore)
	cs.SetSampleScrubber(redactScrubber{secret: "hunter2"})

	// Plant a raw secret directly in the ring (NOT via RecordSample), so the
	// only thing that can scrub it is the storage-boundary re-scrub on Persist.
	patterns := map[string]*Pattern{
		"p-secret": {
			ID:        "p-secret",
			OrgID:     storage.DefaultOrgID,
			Template:  "boom <*>",
			Count:     1,
			FirstSeen: time.Now().UTC(),
			LastSeen:  time.Now().UTC(),
			Samples:   []string{"password=hunter2 boom 500"},
		},
	}
	if err := cs.Persist(patterns, nil); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Belt: the raw samples column bytes carry no secret.
	var raw string
	if err := db.QueryRow(
		`SELECT samples::text FROM vs_logs WHERE org_id=$1 AND pattern_id='p-secret'`,
		storage.DefaultOrgID,
	).Scan(&raw); err != nil {
		t.Fatalf("read samples column: %v", err)
	}
	if strings.Contains(raw, "hunter2") {
		t.Fatalf("secret present in the vs_logs.samples column: %q", raw)
	}
	if !strings.Contains(raw, "<REDACTED>") {
		t.Fatalf("expected the redacted placeholder in the persisted ring, got: %q", raw)
	}

	// And the read view (Snapshot) surfaces only the scrubbed sample.
	snap, _, err := cs.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := patternByID(snap, "p-secret")
	if got == nil {
		t.Fatal("pattern 'p-secret' missing from snapshot")
	}
	if len(got.Samples) != 1 || strings.Contains(got.Samples[0], "hunter2") {
		t.Fatalf("secret survived the storage-boundary re-scrub: %v", got.Samples)
	}
}
