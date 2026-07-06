package storage_test

// runmigrations_test.go — Proves storage.RunSQLMigrations is
// FS/table-agnostic, tx-per-file, ledger-tracked and idempotent.
//
// The behavioural (DB) cases are Postgres-gated on TEST_POSTGRES_DSN exactly
// like the parity/migrate suites; the pure-validation cases run everywhere.

import (
	"database/sql"
	"os"
	"testing"
	"testing/fstest"

	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestRunSQLMigrations_NilDB rejects a nil pool without panicking.
func TestRunSQLMigrations_NilDB(t *testing.T) {
	err := storage.RunSQLMigrations(nil, fstest.MapFS{}, "m", "vs_ledger_x")
	if err == nil {
		t.Fatal("expected error for nil db")
	}
}

// TestRunSQLMigrations_InvalidLedger rejects a ledger name that is not a plain
// identifier — defence-in-depth against a non-constant ever reaching the
// interpolated ledger DDL. sql.Open does not connect, so no live DB is needed:
// the identifier is validated before any query runs.
func TestRunSQLMigrations_InvalidLedger(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://localhost/none")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	for _, bad := range []string{"", "1abc", "drop table x", "a;b", "a-b", `a"b`} {
		if err := storage.RunSQLMigrations(db, fstest.MapFS{}, "m", bad); err == nil {
			t.Fatalf("expected error for invalid ledger %q", bad)
		}
	}
}

// TestRunSQLMigrations_IdempotentAndLedger runs a two-file migration set
// twice and asserts: both files apply, the ledger records exactly those two
// filenames, and a re-run is a no-op (nothing re-applies). Postgres-gated.
func TestRunSQLMigrations_IdempotentAndLedger(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	const ledger = "zz_test_ledger"
	cleanup := func() {
		_, _ = db.Exec("DROP TABLE IF EXISTS zz_mig_a CASCADE")
		_, _ = db.Exec("DROP TABLE IF EXISTS zz_mig_b CASCADE")
		_, _ = db.Exec("DROP TABLE IF EXISTS " + ledger + " CASCADE")
	}
	cleanup()
	t.Cleanup(cleanup)

	fsys := fstest.MapFS{
		"m/001_a.sql": {Data: []byte(`CREATE TABLE zz_mig_a (id TEXT PRIMARY KEY);`)},
		"m/002_b.sql": {Data: []byte(`CREATE TABLE zz_mig_b (id TEXT PRIMARY KEY);`)},
		"m/notes.txt": {Data: []byte("ignored — not a .sql file")},
	}

	if err := storage.RunSQLMigrations(db, fsys, "m", ledger); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Both tables exist.
	for _, tbl := range []string{"zz_mig_a", "zz_mig_b"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename=$1`, tbl,
		).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s present %d times (err=%v), want 1", tbl, n, err)
		}
	}
	// The ledger records exactly the two .sql filenames (the .txt is skipped).
	assertLedger := func(want int) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + ledger).Scan(&n); err != nil {
			t.Fatalf("count ledger: %v", err)
		}
		if n != want {
			t.Fatalf("ledger rows = %d, want %d", n, want)
		}
	}
	assertLedger(2)

	// Re-run: a full no-op. If a file re-applied, the CREATE TABLE (no IF NOT
	// EXISTS) would error — so a clean second run PROVES the skip.
	if err := storage.RunSQLMigrations(db, fsys, "m", ledger); err != nil {
		t.Fatalf("second run must be a no-op, got: %v", err)
	}
	assertLedger(2)
}

// TestRunSQLMigrations_TxPerFileRollback proves a file that fails partway
// applies NEITHER its DDL NOR a ledger row — the tx-per-file guarantee that
// makes destructive migrations (e.g. 003's DROP+recreate) safe to retry.
func TestRunSQLMigrations_TxPerFileRollback(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	const ledger = "zz_test_ledger_rb"
	cleanup := func() {
		_, _ = db.Exec("DROP TABLE IF EXISTS zz_mig_rb CASCADE")
		_, _ = db.Exec("DROP TABLE IF EXISTS " + ledger + " CASCADE")
	}
	cleanup()
	t.Cleanup(cleanup)

	// A single file whose 2nd statement is invalid: the whole file must roll
	// back, so zz_mig_rb (created by the 1st statement) must NOT survive.
	fsys := fstest.MapFS{
		"m/001_bad.sql": {Data: []byte(
			`CREATE TABLE zz_mig_rb (id TEXT PRIMARY KEY); THIS IS NOT SQL;`)},
	}
	if err := storage.RunSQLMigrations(db, fsys, "m", ledger); err == nil {
		t.Fatal("expected error from the invalid statement")
	}
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename='zz_mig_rb'`,
	).Scan(&n); err != nil {
		t.Fatalf("count zz_mig_rb: %v", err)
	}
	if n != 0 {
		t.Fatalf("zz_mig_rb present %d times, want 0 (tx-per-file must roll back)", n)
	}
	// The ledger exists (created before the loop) but records no applied file.
	var recorded int
	if err := db.QueryRow(`SELECT count(*) FROM ` + ledger).Scan(&recorded); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("ledger recorded %d files, want 0", recorded)
	}
}
