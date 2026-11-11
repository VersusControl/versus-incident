package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
	"time"
)

// ErrSchemaOutOfDate reports a database built by an older version of the
// table schema: the tables exist, so startup would otherwise succeed, but
// they lack columns the running code writes, leaving a store that half
// works. The only safe recovery is to recreate the database.
var ErrSchemaOutOfDate = errors.New("storage: database schema is out of date")

// SchemaOutOfDateError names the table and column a database is missing.
// It unwraps to ErrSchemaOutOfDate so a caller can classify it.
type SchemaOutOfDateError struct {
	Table  string
	Column string
}

func (e *SchemaOutOfDateError) Error() string {
	return fmt.Sprintf("%s: table %s has no column %s; the database was created by an older schema and cannot be upgraded in place — recreate it", ErrSchemaOutOfDate, e.Table, e.Column)
}

func (e *SchemaOutOfDateError) Unwrap() error { return ErrSchemaOutOfDate }

// requiredTableColumns is the startup assertion over the AWS and FinOps key
// columns. Each table was created with CREATE TABLE IF NOT EXISTS, so an
// older edition of a migration file leaves an existing table unchanged while
// the code starts writing columns it does not have.
var requiredTableColumns = []struct {
	table   string
	columns []string
}{
	{"aws_sync_units", []string{"account", "region", "service", "generation", "state", "gaps"}},
	{"aws_unit_generations", []string{"service", "seq", "digest", "resource_count", "gaps", "valid_from", "valid_to"}},
	{"aws_resources", []string{"service", "arn", "digest", "valid_from", "valid_to"}},
	{"aws_health", []string{"service", "arn", "kind", "digest", "valid_from", "valid_to"}},
	{"aws_security_flags", []string{"service", "arn", "flag", "digest", "valid_from", "valid_to"}},
	{"aws_changes", []string{"event_id", "service", "event_time", "detail"}},
	{"aws_change_coverage", []string{"account", "region", "covered_from", "covered_until"}},
	{"finops_cost_lines", []string{"service_name", "net_amortized_micros", "estimated_team", "estimated_region"}},
	{"finops_cost_daily", []string{"usage_group", "net_amortized_micros", "lines", "overflowed"}},
	{"finops_cost_monthly", []string{"min_day", "net_amortized_micros", "lines", "overflowed"}},
	{"finops_increases", []string{"suspects", "detail"}},
	{"finops_increase_events", []string{"until_day"}},
	{"finops_saving_decisions", []string{"payer", "account", "kind", "title", "orphaned_at"}},
}

// assertTableSchema fails closed when the database lacks a column the code
// reads or writes, instead of starting a store whose totals work and whose
// writes fail with an unclassified SQL error.
func assertTableSchema(ctx context.Context, db *sql.DB, postgres bool) error {
	if db == nil {
		return nil
	}
	for _, required := range requiredTableColumns {
		have, err := tableColumns(ctx, db, required.table, postgres)
		if err != nil {
			return err
		}
		for _, column := range required.columns {
			if !have[column] {
				return &SchemaOutOfDateError{Table: required.table, Column: column}
			}
		}
	}
	return nil
}

// tableColumns lists one table's columns on either backend.
func tableColumns(ctx context.Context, db *sql.DB, table string, postgres bool) (map[string]bool, error) {
	var rows *sql.Rows
	var err error
	if postgres {
		rows, err = db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1`, table)
	} else {
		// The table names come from the fixed list above, never from input.
		rows, err = db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		if postgres {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			have[name] = true
			continue
		}
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

// migrate.go — RunSQLMigrations: a fully FS- and table-agnostic
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
// Guarantees:
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
		// Operators see how long each first-start migration takes.
		started := time.Now()
		logApplied := func() {
			log.Printf("storage: migration %s applied in %.2fs", name, time.Since(started).Seconds())
		}

		data, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return fmt.Errorf("storage: RunSQLMigrations: read %q: %w", name, err)
		}
		if strings.HasPrefix(string(data), "-- versus:nontransactional\n") {
			// Online index files contain only independently idempotent index
			// statements. A failed concurrent build can leave an invalid index;
			// fail closed instead of ledgering a silently unusable index.
			for _, statement := range strings.Split(strings.TrimPrefix(string(data), "-- versus:nontransactional\n"), ";") {
				statement = strings.TrimSpace(statement)
				optionalTable := strings.HasPrefix(statement, "-- versus:optional-table\n")
				if optionalTable {
					statement = strings.TrimSpace(strings.TrimPrefix(statement, "-- versus:optional-table\n"))
				}
				// A statement for a partitioned table only is skipped where the
				// table does not exist or is not (yet) partitioned.
				partitionedOnly := strings.HasPrefix(statement, "-- versus:partitioned-table\n")
				if partitionedOnly {
					statement = strings.TrimSpace(strings.TrimPrefix(statement, "-- versus:partitioned-table\n"))
				}
				if statement == "" {
					continue
				}
				if !strings.HasPrefix(statement, "CREATE INDEX CONCURRENTLY IF NOT EXISTS ") {
					return fmt.Errorf("storage: online migration %q contains unsupported statement", name)
				}
				fields := strings.Fields(statement)
				if len(fields) < 9 {
					return fmt.Errorf("storage: malformed online index statement in %q", name)
				}
				index := fields[6]
				if !isSafeSQLIdentifier(index) {
					return fmt.Errorf("storage: online migration %q has invalid index", name)
				}
				table := strings.Split(fields[8], "(")[0]
				if optionalTable {
					var exists bool
					if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
						return err
					}
					if !exists {
						continue
					}
				}
				var partitioned bool
				if err := db.QueryRowContext(ctx, `SELECT COALESCE((SELECT relkind='p' FROM pg_class WHERE oid=to_regclass($1)),FALSE)`, table).Scan(&partitioned); err != nil {
					return err
				}
				if partitionedOnly && !partitioned {
					continue
				}
				if partitioned {
					if err := runPartitionedOnlineIndex(ctx, db, statement, index, table); err != nil {
						return err
					}
					continue
				}
				var valid bool
				checkErr := db.QueryRowContext(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid=to_regclass($1)`, index).Scan(&valid)
				if checkErr == nil && !valid {
					if _, err := db.ExecContext(ctx, "DROP INDEX CONCURRENTLY "+index); err != nil {
						return fmt.Errorf("storage: repair online index %q: %w", index, err)
					}
				} else if checkErr != nil && checkErr != sql.ErrNoRows {
					return checkErr
				}
				if _, err := db.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("storage: online migration %q: %w", name, err)
				}
			}
			if _, err := db.ExecContext(ctx, recordApplied, name); err != nil {
				return fmt.Errorf("storage: record online migration %q: %w", name, err)
			}
			logApplied()
			continue
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
		logApplied()
	}
	return nil
}
