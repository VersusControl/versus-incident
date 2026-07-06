package storage

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// migrate.go — RunSQLMigrations (X28-A2): a fully FS- and table-agnostic
// Postgres migration runner.
//
// It is the ONE seam both the OSS backend and the out-of-tree enterprise
// module use to apply their own `*.sql` migration sets against a shared
// pool obtained via storage.SQLAccessor. It knows nothing about which
// tables a migration creates: the caller supplies the embedded filesystem,
// the directory to scan, and the name of a per-set ledger table. OSS runs
// it over its own `migrations/` under the `versus_schema_migrations` ledger;
// the enterprise module runs its own set under a different ledger on the
// same pool, so the two track independently and never re-apply each other's
// files.
//
// Guarantees (the A2 acceptance):
//   - each `*.sql` file is applied AT MOST ONCE — its filename is recorded
//     in the ledger inside the SAME transaction that applies it, so a crash
//     mid-file rolls back both the DDL and the ledger row (tx-per-file);
//   - a re-run is a NO-OP — files already in the ledger are skipped, which
//     is REQUIRED for destructive migrations (e.g. a DROP-and-recreate that
//     would wipe data if it re-ran every boot);
//   - files apply in lexical (filename) order — the numeric prefix
//     convention (001_, 002_, …) gives the ordering.
//
// SQL surface: the ledger table name is a Go constant supplied by the
// caller (never end-user input) and is additionally validated as a plain
// identifier here as defence-in-depth; the recorded filename is always a
// bound parameter. No value is ever interpolated into SQL.

// isSafeSQLIdentifier reports whether s is a conservative, unquoted SQL
// identifier: a leading letter/underscore followed by letters, digits, or
// underscores. Callers pass Go constants for the ledger table, so this is
// belt-and-suspenders against a future non-constant creeping in; a value
// that fails is rejected rather than interpolated.
func isSafeSQLIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			// always allowed
		case r >= '0' && r <= '9':
			if i == 0 {
				return false // may not start with a digit
			}
		default:
			return false
		}
	}
	return true
}

// RunSQLMigrations applies every `*.sql` file in fsys under dir, in lexical
// filename order, exactly once each, tracking applied filenames in
// ledgerTable so a re-run is a no-op. It is FS- and table-agnostic (see the
// file header). db must be a Postgres *sql.DB (obtained via
// storage.SQLAccessor.DB()); the ledger DDL and bookkeeping use Postgres
// syntax.
//
// Concurrency: RunSQLMigrations does NOT take the migration advisory lock
// itself — the caller wraps it in WithMigrationLock when multiple processes
// may migrate the same pool at once (the OSS backend does; the enterprise
// runner does the same on the shared key). This keeps the runner a pure,
// lock-agnostic primitive.
func RunSQLMigrations(db *sql.DB, fsys fs.FS, dir, ledgerTable string) error {
	if db == nil {
		return fmt.Errorf("storage: RunSQLMigrations: nil db")
	}
	if !isSafeSQLIdentifier(ledgerTable) {
		return fmt.Errorf("storage: RunSQLMigrations: invalid ledger table %q", ledgerTable)
	}
	ctx := context.Background()

	// Ledger: one row per applied filename. Idempotent create so a fresh
	// pool and an existing one converge. The table name is a validated Go
	// constant (never caller-derived), so interpolating it is safe.
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			filename   TEXT        PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`, ledgerTable)); err != nil {
		return fmt.Errorf("storage: RunSQLMigrations: create ledger %q: %w", ledgerTable, err)
	}

	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("storage: RunSQLMigrations: read dir %q: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	selectApplied := fmt.Sprintf(`SELECT 1 FROM %s WHERE filename = $1`, ledgerTable)
	recordApplied := fmt.Sprintf(`INSERT INTO %s (filename) VALUES ($1)`, ledgerTable)

	for _, name := range names {
		var one int
		err := db.QueryRowContext(ctx, selectApplied, name).Scan(&one)
		if err == nil {
			continue // already applied — skip (re-run no-op)
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("storage: RunSQLMigrations: check ledger for %q: %w", name, err)
		}

		data, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return fmt.Errorf("storage: RunSQLMigrations: read %q: %w", name, err)
		}

		// tx-per-file: the DDL and the ledger row commit together, so a
		// crash mid-apply leaves the file NEITHER applied nor recorded.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("storage: RunSQLMigrations: begin tx for %q: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(data)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("storage: RunSQLMigrations: apply %q: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, recordApplied, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("storage: RunSQLMigrations: record %q: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("storage: RunSQLMigrations: commit %q: %w", name, err)
		}
	}
	return nil
}
