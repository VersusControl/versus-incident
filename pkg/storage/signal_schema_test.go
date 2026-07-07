package storage_test

// signal_schema_test.go — Verifies migration 003 lands the typed
// signal schema: the FK from vs_logs to the
// vs_patterns root, the tuned indexes, the instance_index write-shard default,
// and the security invariant that NO signal table carries a secret/cipher
// column. Postgres-gated on TEST_POSTGRES_DSN.

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	// NewPostgres runs migrations 001..003 at construction.
	p, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	acc, ok := p.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres provider must implement storage.SQLAccessor")
	}
	t.Cleanup(func() { _ = p.Close() })
	return acc.DB()
}

// TestSignalSchema_ForeignKey proves vs_logs FK-references vs_patterns with
// ON DELETE CASCADE (so a root delete cleans up its learned rows).
func TestSignalSchema_ForeignKey(t *testing.T) {
	db := migratedDB(t)
	var n int
	err := db.QueryRow(`
		SELECT count(*)
		FROM information_schema.table_constraints tc
		JOIN information_schema.referential_constraints rc
		  ON tc.constraint_name = rc.constraint_name
		 AND tc.constraint_schema = rc.constraint_schema
		WHERE tc.constraint_type = 'FOREIGN KEY'
		  AND tc.table_name = 'vs_logs'
		  AND rc.delete_rule = 'CASCADE'`).Scan(&n)
	if err != nil {
		t.Fatalf("query FK: %v", err)
	}
	if n < 1 {
		t.Fatal("vs_logs is missing an ON DELETE CASCADE FK to vs_patterns")
	}
}

// TestSignalSchema_Indexes proves the plan's tuned indexes exist.
func TestSignalSchema_Indexes(t *testing.T) {
	db := migratedDB(t)
	want := []string{
		"idx_patterns_kind", "idx_patterns_service", "idx_patterns_verdict",
		"idx_logs_last_seen", "idx_services_deleted",
	}
	for _, idx := range want {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname=$1`, idx,
		).Scan(&n); err != nil {
			t.Fatalf("query index %s: %v", idx, err)
		}
		if n != 1 {
			t.Fatalf("index %s present %d times, want 1", idx, n)
		}
	}
}

// TestSignalSchema_InstanceIndexDefaultZero proves the HA write-shard ordinal
// defaults to 0 — so the OSS single-instance path always writes one row per
// pattern without naming any HA policy.
func TestSignalSchema_InstanceIndexDefaultZero(t *testing.T) {
	db := migratedDB(t)
	var def sql.NullString
	if err := db.QueryRow(`
		SELECT column_default
		FROM information_schema.columns
		WHERE table_name='vs_logs' AND column_name='instance_index'`).Scan(&def); err != nil {
		t.Fatalf("query instance_index default: %v", err)
	}
	if !def.Valid || def.String != "0" {
		t.Fatalf("instance_index default = %v, want 0", def)
	}
}

// TestSignalSchema_NoSecretColumns is the security pre-check: signal data
// is post-redaction and non-secret, so NO column on the three signal tables
// may look like a secret/cipher/token/key store.
func TestSignalSchema_NoSecretColumns(t *testing.T) {
	db := migratedDB(t)
	rows, err := db.Query(`
		SELECT table_name, column_name
		FROM information_schema.columns
		WHERE table_name IN ('vs_patterns','vs_logs','vs_services')
		  AND (column_name ILIKE '%secret%'
		    OR column_name ILIKE '%cipher%'
		    OR column_name ILIKE '%password%'
		    OR column_name ILIKE '%token%'
		    OR column_name ILIKE '%encrypt%'
		    OR column_name ILIKE '%private_key%')`)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Fatalf("signal table %s carries a secret-shaped column %q — signal data must be non-secret", tbl, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// TestSignalSchema_BaselineModeColumns proves migration 007 landed the two
// remaining spike-baseline columns on vs_logs additively and default-safe:
// baseline_avg is a NOT NULL double defaulting to 0, and spike_baseline_mode is
// a NOT NULL text defaulting to ” — so every pre-migration row reads back
// byte-identically and simply re-learns.
func TestSignalSchema_BaselineModeColumns(t *testing.T) {
	db := migratedDB(t)
	for _, tc := range []struct {
		column, dataType, defaultLike string
	}{
		{"baseline_avg", "double precision", "0"},
		{"spike_baseline_mode", "text", "''"},
	} {
		var dataType, isNullable, colDefault string
		if err := db.QueryRow(`
			SELECT data_type, is_nullable, COALESCE(column_default, '')
			FROM information_schema.columns
			WHERE table_name='vs_logs' AND column_name=$1`, tc.column,
		).Scan(&dataType, &isNullable, &colDefault); err != nil {
			t.Fatalf("vs_logs.%s missing (migration 007 not applied?): %v", tc.column, err)
		}
		if dataType != tc.dataType {
			t.Fatalf("vs_logs.%s data_type = %q, want %q", tc.column, dataType, tc.dataType)
		}
		if isNullable != "NO" {
			t.Fatalf("vs_logs.%s is_nullable = %q, want NO", tc.column, isNullable)
		}
		if !strings.Contains(colDefault, tc.defaultLike) {
			t.Fatalf("vs_logs.%s default = %q, want it to contain %q", tc.column, colDefault, tc.defaultLike)
		}
	}
}

// TestSignalSchema_KindInForeignKey proves migration 004 folded `kind`
// into the child→root FK: vs_patterns carries a UNIQUE (org_id, id, kind) key,
// vs_logs pins kind='log' via a CHECK, and vs_logs FK-references
// vs_patterns (org_id, id, kind) — so a vs_logs row can only attach to a
// kind='log' root. The original (org_id, id) cascade FK is left intact.
func TestSignalSchema_KindInForeignKey(t *testing.T) {
	db := migratedDB(t)

	// The three new named constraints each exist exactly once.
	for name, ctype := range map[string]string{
		"vs_patterns_org_id_kind_key": "u", // UNIQUE (org_id, id, kind)
		"vs_logs_kind_is_log":         "c", // CHECK (kind = 'log')
		"vs_logs_patterns_kind_fk":    "f", // FK (org_id, pattern_id, kind)
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pg_constraint WHERE conname=$1 AND contype=$2`, name, ctype,
		).Scan(&n); err != nil {
			t.Fatalf("query constraint %s: %v", name, err)
		}
		if n != 1 {
			t.Fatalf("constraint %s (type %s) present %d times, want 1", name, ctype, n)
		}
	}

	// The composite FK definition pins kind and targets the vs_patterns root.
	var def string
	if err := db.QueryRow(
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='vs_logs_patterns_kind_fk'`,
	).Scan(&def); err != nil {
		t.Fatalf("get FK def: %v", err)
	}
	for _, want := range []string{"kind", "vs_patterns", "ON DELETE CASCADE"} {
		if !strings.Contains(def, want) {
			t.Fatalf("kind FK def %q missing %q", def, want)
		}
	}

	// The original (org_id, pattern_id) → (org_id, id) cascade FK is untouched:
	// vs_logs still has (at least) two ON DELETE CASCADE FKs to vs_patterns.
	var casc int
	if err := db.QueryRow(`
		SELECT count(*)
		FROM information_schema.table_constraints tc
		JOIN information_schema.referential_constraints rc
		  ON tc.constraint_name = rc.constraint_name
		 AND tc.constraint_schema = rc.constraint_schema
		WHERE tc.constraint_type = 'FOREIGN KEY'
		  AND tc.table_name = 'vs_logs'
		  AND rc.delete_rule = 'CASCADE'`).Scan(&casc); err != nil {
		t.Fatalf("query cascade FKs: %v", err)
	}
	if casc < 2 {
		t.Fatalf("vs_logs has %d ON DELETE CASCADE FKs, want >= 2 (original + kind-composite)", casc)
	}
}

// TestSignalSchema_KindFKRejectsWrongKind proves the DB rejects a vs_logs
// row FK'd to a non-'log' root and accepts one FK'd to a 'log' root — the
// wrong-kind link is impossible at the storage layer, not merely unlikely by
// the id-scheme.
func TestSignalSchema_KindFKRejectsWrongKind(t *testing.T) {
	db := migratedDB(t)
	const org = "b58-fk-test"
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM vs_patterns WHERE org_id=$1`, org) })
	_, _ = db.Exec(`DELETE FROM vs_patterns WHERE org_id=$1`, org)

	// Seed a metric-kind root and a log-kind root under the same id namespace.
	if _, err := db.Exec(
		`INSERT INTO vs_patterns (org_id, id, kind) VALUES ($1, 'wrongkind', 'metric')`, org,
	); err != nil {
		t.Fatalf("seed metric root: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO vs_patterns (org_id, id, kind) VALUES ($1, 'rightkind', 'log')`, org,
	); err != nil {
		t.Fatalf("seed log root: %v", err)
	}

	// A vs_logs row (kind defaults to 'log') FK'd to the metric root: REJECTED.
	// The old (org_id, pattern_id) FK is satisfied (the root exists), so only
	// the kind-composite FK can reject it — proving kind is enforced.
	if _, err := db.Exec(
		`INSERT INTO vs_logs (org_id, pattern_id, template, first_seen, last_seen)
		 VALUES ($1, 'wrongkind', 't', NOW(), NOW())`, org,
	); err == nil {
		t.Fatal("expected FK rejection linking a vs_logs row to a non-'log' root")
	}

	// The same row FK'd to the log root: ACCEPTED.
	if _, err := db.Exec(
		`INSERT INTO vs_logs (org_id, pattern_id, template, first_seen, last_seen)
		 VALUES ($1, 'rightkind', 't', NOW(), NOW())`, org,
	); err != nil {
		t.Fatalf("correct-kind link rejected: %v", err)
	}
}

// TestSignalSchema_Migration004Idempotent proves re-running the OSS
// migration set is a no-op: the ledger skips the already-applied 004 (if it
// re-applied, its named ADD CONSTRAINTs would error on the duplicate), and
// each new constraint exists exactly once.
func TestSignalSchema_Migration004Idempotent(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	// First construction applies migrations 001..004 (each exactly once).
	p1, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres (first): %v", err)
	}
	defer func() { _ = p1.Close() }()

	// Second construction must be a clean no-op — the ledger skips 004.
	p2, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("re-running migrations must be a no-op, got: %v", err)
	}
	defer func() { _ = p2.Close() }()

	db := p2.(storage.SQLAccessor).DB()
	for _, name := range []string{
		"vs_patterns_org_id_kind_key", "vs_logs_kind_is_log", "vs_logs_patterns_kind_fk",
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname=$1`, name).Scan(&n); err != nil {
			t.Fatalf("query constraint %s: %v", name, err)
		}
		if n != 1 {
			t.Fatalf("constraint %s present %d times after re-run, want 1", name, n)
		}
	}
}
