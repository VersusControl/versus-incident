package storage_test

// migrate_test.go — proves the Postgres schema migration is concurrency-safe
// when N replicas boot at once against ONE fresh shared Postgres
// (e.g. a StatefulSet with podManagementPolicy: Parallel) the migration runner
// must serialize on the shared advisory lock so exactly one migrator creates
// the schema while the rest wait then no-op — no `duplicate key …
// pg_type_typname_nsp_index` catalog race, no crashed replica.
//
// Postgres-backed, so gated on TEST_POSTGRES_DSN exactly like the parity and
// blob-create suites; the standard CI loop (no live Postgres) skips it and
// stays green. Run against a real database with:
//
//   TEST_POSTGRES_DSN="postgres://user:pass@localhost:5432/testdb?sslmode=disable" \
//       go test -race ./pkg/storage/...

import (
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// expectedSchemaTables is every table the OSS migrations create. After any
// migrate run — single or concurrent — each must exist exactly once. The
// migration 003 drops the old whole-blob vs_patterns and recreates it as the
// typed catalog root alongside vs_logs / vs_services.
var expectedSchemaTables = []string{
	"vs_blobs", "vs_incidents", "vs_analyses",
	"vs_patterns", "vs_logs", "vs_services",
	"vs_shadow", "vs_detect", "vs_members", "vs_teams",
}

// dropAllVersusTables drops every vs_* table so the next migrate runs against
// a FRESH database — i.e. CREATE TABLE actually attempts creation (and would
// race the catalog without the advisory lock), reproducing the first-boot
// scenario rather than a warm-DB no-op.
func dropAllVersusTables(t *testing.T, dsn string) {
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
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration error: %v", err)
	}
	for _, tbl := range tables {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	// The migration ledger is NOT a vs_* table, so drop it explicitly:
	// leaving it would make RunSQLMigrations treat every file as already
	// applied and skip re-creating the just-dropped schema, defeating the
	// fresh-boot repro this helper sets up.
	if _, err := db.Exec("DROP TABLE IF EXISTS versus_schema_migrations CASCADE"); err != nil {
		t.Fatalf("drop migration ledger: %v", err)
	}
}

// assertSchemaComplete confirms every expected table exists exactly once.
func assertSchemaComplete(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	for _, tbl := range expectedSchemaTables {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename=$1`, tbl,
		).Scan(&n); err != nil {
			t.Fatalf("count table %s: %v", tbl, err)
		}
		if n != 1 {
			t.Fatalf("table %s present %d times, want exactly 1", tbl, n)
		}
	}
}

// TestPostgresConcurrentMigrate fires N replicas' first boots at once against
// the SAME fresh database. With the advisory lock every NewPostgres (which
// runs migrate) must succeed — no duplicate-key / catalog error — and the
// resulting schema is complete and consistent. This is the concurrency repro.
func TestPostgresConcurrentMigrate(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}

	// Fresh DB: drop the schema so each goroutine's migrate genuinely races to
	// CREATE the tables.
	dropAllVersusTables(t, dsn)

	const n = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		provs    []storage.Provider
	)
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // line every booter up so they hit migrate together
			p, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			provs = append(provs, p)
		}()
	}
	close(start)
	wg.Wait()

	for _, p := range provs {
		defer p.Close()
	}

	if firstErr != nil {
		t.Fatalf("a concurrent NewPostgres/migrate failed (expected all to succeed under the advisory lock): %v", firstErr)
	}
	if len(provs) != n {
		t.Fatalf("only %d of %d concurrent booters succeeded", len(provs), n)
	}
	assertSchemaComplete(t, dsn)
}

// TestPostgresSingleMigrate is the single-caller (single-instance) path: one
// booter against a fresh DB migrates cleanly and yields the full schema. It
// guards that wrapping migrate in the advisory lock left the one-instance
// behavior unchanged (the lone booter takes + releases the lock with no
// contention).
func TestPostgresSingleMigrate(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}

	dropAllVersusTables(t, dsn)

	p, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres (single, fresh DB): %v", err)
	}
	defer p.Close()
	assertSchemaComplete(t, dsn)

	// Re-running migrate over the now-warm DB stays a no-op (idempotent), and
	// takes + releases the lock cleanly without deadlocking on the prior run.
	p2, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres (single, warm DB re-run): %v", err)
	}
	defer p2.Close()
	assertSchemaComplete(t, dsn)
}
