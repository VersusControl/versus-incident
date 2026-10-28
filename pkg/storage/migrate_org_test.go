package storage_test

// migrate_org_test.go — drives the one-time operator script
// scripts/postgres/migrate_org_to_enterprise.sql against a live Postgres.
//
// The script re-keys the OSS data plane from the single-tenant "default" org
// to an Enterprise deployment org. These tests seed default-org rows across
// every affected table (including vs_logs children and models/<org>/… blobs,
// plus a models/settings/… blob that must NOT move), run the real script, and
// assert the move, the foreign keys, the untouched settings blob, the
// re-runnable no-op, and the skip-if-exists conflict policy.
//
// Gated on TEST_POSTGRES_DSN like the rest of the Postgres suite.

import (
	"database/sql"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const migrateOrgScriptPath = "../../scripts/postgres/migrate_org_to_enterprise.sql"

var orgIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// orgMigrationScript returns the operator script with psql's client-side work
// already done: backslash meta-commands dropped and every :'target_org'
// reference replaced by the quoted literal psql would have substituted. What
// remains is plain SQL — the exact statements an operator's psql session sends
// to the server — so the test exercises the real script rather than a
// hand-copied duplicate that could drift.
func orgMigrationScript(t *testing.T, targetOrg string) string {
	t.Helper()
	if !orgIDPattern.MatchString(targetOrg) {
		t.Fatalf("test target org %q must match %s", targetOrg, orgIDPattern)
	}
	raw, err := os.ReadFile(migrateOrgScriptPath)
	if err != nil {
		t.Fatalf("read migration script: %v", err)
	}
	var sqlOnly strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `\`) {
			continue
		}
		sqlOnly.WriteString(line)
		sqlOnly.WriteString("\n")
	}
	return strings.ReplaceAll(sqlOnly.String(), `:'target_org'`, "'"+targetOrg+"'")
}

// runOrgMigration executes the whole script — BEGIN … COMMIT included — as one
// batch on a single connection, exactly as psql feeds it.
func runOrgMigration(t *testing.T, db *sql.DB, targetOrg string) error {
	t.Helper()
	_, err := db.Exec(orgMigrationScript(t, targetOrg))
	return err
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %.60q: %v", query, err)
	}
}

func scalar[T any](t *testing.T, db *sql.DB, query string, args ...any) T {
	t.Helper()
	var out T
	if err := db.QueryRow(query, args...).Scan(&out); err != nil {
		t.Fatalf("query %.60q: %v", query, err)
	}
	return out
}

func pgDB(t *testing.T, p storage.Provider) *sql.DB {
	t.Helper()
	accessor, ok := p.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres backend must implement storage.SQLAccessor")
	}
	return accessor.DB()
}

// seedDefaultOrg fills every table the script touches with default-org rows,
// plus the two blobs that must survive untouched.
func seedDefaultOrg(t *testing.T, db *sql.DB) {
	t.Helper()

	mustExec(t, db, `
		INSERT INTO vs_incidents (id, created_at, org_id, title, source, service, origin)
		VALUES ('inc-1', now(), 'default', 'db down', 'agent:detect', 'checkout', 'ai_detect'),
		       ('inc-2', now(), 'default', 'latency', 'webhook',      'cart',     'webhook')`)

	// an-2 predates the org field entirely: no 'org_id' key at all, which the
	// read path normalizes to "default" and the script must therefore rewrite.
	mustExec(t, db, `
		INSERT INTO vs_analyses (id, incident_id, data, requested_at)
		VALUES ('an-1', 'inc-1', jsonb_build_object('id','an-1','incident_id','inc-1','org_id','default'), now()),
		       ('an-2', 'inc-2', jsonb_build_object('id','an-2','incident_id','inc-2'), now())`)

	mustExec(t, db, `
		INSERT INTO vs_patterns (org_id, id, kind, service)
		VALUES ('default', 'p-aaa', 'log', 'checkout'),
		       ('default', 'p-bbb', 'log', 'cart')`)

	// p-aaa has two partitions, so the move must carry every child row.
	mustExec(t, db, `
		INSERT INTO vs_logs (org_id, pattern_id, instance_index, template, first_seen, last_seen, count)
		VALUES ('default', 'p-aaa', 0, 'conn refused <*>', now(), now(), 7),
		       ('default', 'p-aaa', 1, 'conn refused <*>', now(), now(), 3),
		       ('default', 'p-bbb', 0, 'timeout <*>',      now(), now(), 5)`)

	mustExec(t, db, `
		INSERT INTO vs_services (org_id, name, manual, first_seen)
		VALUES ('default', 'checkout', false, now()),
		       ('default', 'cart',     true,  now())`)

	mustExec(t, db, `
		INSERT INTO vs_blobs (name, data) VALUES
		  ('models/default/sre/baseline:checkout', convert_to('{"from":"default"}', 'UTF8')),
		  ('models/default/sre/baseline:cart',     convert_to('{"from":"default"}', 'UTF8')),
		  ('models/settings/report-settings',      convert_to('{"keep":"me"}', 'UTF8')),
		  ('service-overrides', convert_to('{"version":1,"rules":[
		      {"id":"r-1","org_id":"default","source_type":"log","match":"p-aaa","service":"checkout"},
		      {"id":"r-2","source_type":"metric","match":"cpu*","service":"cart"}]}', 'UTF8'))`)
}

// TestPostgresMigrateOrgScript is the happy path: every default-org row lands
// under the deployment org, the deployment-wide settings blob is untouched,
// the foreign keys survive, and a second run changes nothing.
func TestPostgresMigrateOrgScript(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset
	db := pgDB(t, p)
	seedDefaultOrg(t, db)

	if err := runOrgMigration(t, db, "acme"); err != nil {
		t.Fatalf("run migration script: %v", err)
	}

	// Nothing left behind, everything accounted for under the new org.
	for _, tc := range []struct{ table, want string }{
		{"vs_incidents", "acme"},
		{"vs_patterns", "acme"},
		{"vs_logs", "acme"},
		{"vs_services", "acme"},
	} {
		left := scalar[int](t, db, `SELECT count(*) FROM `+tc.table+` WHERE org_id = 'default'`)
		moved := scalar[int](t, db, `SELECT count(*) FROM `+tc.table+` WHERE org_id = $1`, tc.want)
		if left != 0 || moved == 0 {
			t.Errorf("%s: %d rows still under default, %d under %s", tc.table, left, moved, tc.want)
		}
	}

	// Both partitions of p-aaa came across, not just the first.
	if n := scalar[int](t, db,
		`SELECT count(*) FROM vs_logs WHERE org_id = 'acme' AND pattern_id = 'p-aaa'`); n != 2 {
		t.Errorf("vs_logs partitions for p-aaa = %d, want 2", n)
	}

	// Analyses: the org lives inside the JSONB record, including the row that
	// had no org_id key at all.
	if n := scalar[int](t, db,
		`SELECT count(*) FROM vs_analyses WHERE data->>'org_id' = 'acme'`); n != 2 {
		t.Errorf("analyses under acme = %d, want 2", n)
	}

	// Model-state blobs are re-keyed by name; the read path must find them.
	if got, err := p.ReadBlob("models/acme/sre/baseline:checkout"); err != nil || string(got) != `{"from":"default"}` {
		t.Errorf("model blob after migration = %q, %v", got, err)
	}
	if n := scalar[int](t, db,
		`SELECT count(*) FROM vs_blobs WHERE name LIKE 'models/default/%'`); n != 0 {
		t.Errorf("%d model blobs still under models/default/", n)
	}

	// The deployment-wide settings blob shares the models/ namespace but is not
	// org-scoped — a blanket rename would have eaten it.
	settings, err := p.ReadBlob("models/settings/report-settings")
	if err != nil || string(settings) != `{"keep":"me"}` {
		t.Errorf("settings blob = %q, %v — must be untouched", settings, err)
	}

	// Service-attribution rules carry their org inside the blob; both the
	// explicit "default" rule and the org-less one move.
	if n := scalar[int](t, db, `
		SELECT count(*) FROM jsonb_array_elements(
			(SELECT convert_from(data, 'UTF8')::jsonb->'rules' FROM vs_blobs WHERE name = 'service-overrides')
		) AS r WHERE r->>'org_id' = 'acme'`); n != 2 {
		t.Errorf("service-override rules under acme = %d, want 2", n)
	}

	// The foreign keys are back to their original, immediately-checked state.
	if n := scalar[int](t, db, `
		SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'vs_logs'::regclass AND contype = 'f'
		  AND (condeferrable OR condeferred)`); n != 0 {
		t.Errorf("%d vs_logs foreign keys left deferrable", n)
	}
	// …and still enforcing: an orphan child must be rejected.
	if _, err := db.Exec(`
		INSERT INTO vs_logs (org_id, pattern_id, instance_index, template, first_seen, last_seen)
		VALUES ('acme', 'p-nonexistent', 0, 'orphan', now(), now())`); err == nil {
		t.Error("orphan vs_logs insert succeeded; the foreign key is not enforcing")
	}

	// Re-run: a clean no-op, not an error.
	before := scalar[string](t, db, orgSnapshotQuery)
	if err := runOrgMigration(t, db, "acme"); err != nil {
		t.Fatalf("second run must be a no-op, got: %v", err)
	}
	if after := scalar[string](t, db, orgSnapshotQuery); after != before {
		t.Errorf("second run changed state:\n before %s\n after  %s", before, after)
	}
}

// orgSnapshotQuery renders every org-carrying row into one stable string so a
// re-run can be compared byte-for-byte.
const orgSnapshotQuery = `
	SELECT string_agg(line, E'\n' ORDER BY line) FROM (
		SELECT 'incident:'  || id || '=' || org_id AS line FROM vs_incidents
		UNION ALL SELECT 'pattern:' || id || '=' || org_id FROM vs_patterns
		UNION ALL SELECT 'log:' || pattern_id || '/' || instance_index || '=' || org_id FROM vs_logs
		UNION ALL SELECT 'service:' || name || '=' || org_id FROM vs_services
		UNION ALL SELECT 'analysis:' || id || '=' || coalesce(data->>'org_id', '?') FROM vs_analyses
		UNION ALL SELECT 'blob:' || name || '=' || md5(data) FROM vs_blobs
	) s`

// TestPostgresMigrateOrgScriptSkipsConflicts covers the operator who ran
// Enterprise for a while before migrating: keys the target org already owns are
// left alone, and the colliding default-org rows stay where they are rather
// than aborting the run or destroying live data.
func TestPostgresMigrateOrgScriptSkipsConflicts(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset
	db := pgDB(t, p)

	mustExec(t, db, `
		INSERT INTO vs_patterns (org_id, id, kind, service)
		VALUES ('default', 'p-move', 'log', 'checkout'),
		       ('default', 'p-dup',  'log', 'stale'),
		       ('acme',    'p-dup',  'log', 'relearned')`)
	mustExec(t, db, `
		INSERT INTO vs_logs (org_id, pattern_id, instance_index, template, first_seen, last_seen, count)
		VALUES ('default', 'p-move', 0, 'moves <*>',     now(), now(), 9),
		       ('default', 'p-dup',  0, 'stale <*>',     now(), now(), 1),
		       ('acme',    'p-dup',  0, 'relearned <*>', now(), now(), 42)`)
	mustExec(t, db, `
		INSERT INTO vs_services (org_id, name, manual, first_seen)
		VALUES ('default', 'moves-svc', false, now()),
		       ('default', 'dup-svc',   true,  now()),
		       ('acme',    'dup-svc',   false, now())`)
	mustExec(t, db, `
		INSERT INTO vs_blobs (name, data) VALUES
		  ('models/default/sre/moves', convert_to('{"from":"default"}', 'UTF8')),
		  ('models/default/sre/dup',   convert_to('{"from":"default"}', 'UTF8')),
		  ('models/acme/sre/dup',      convert_to('{"from":"acme"}', 'UTF8'))`)

	if err := runOrgMigration(t, db, "acme"); err != nil {
		t.Fatalf("conflicting run must commit, got: %v", err)
	}

	// Target-org data is never overwritten.
	if svc := scalar[string](t, db,
		`SELECT service FROM vs_patterns WHERE org_id = 'acme' AND id = 'p-dup'`); svc != "relearned" {
		t.Errorf("target-org pattern service = %q, want relearned (must not be overwritten)", svc)
	}
	if n := scalar[int64](t, db,
		`SELECT count FROM vs_logs WHERE org_id = 'acme' AND pattern_id = 'p-dup'`); n != 42 {
		t.Errorf("target-org log count = %d, want 42 (must not be overwritten)", n)
	}
	if manual := scalar[bool](t, db,
		`SELECT manual FROM vs_services WHERE org_id = 'acme' AND name = 'dup-svc'`); manual {
		t.Error("target-org service was overwritten by the default-org row")
	}
	if b := scalar[string](t, db,
		`SELECT convert_from(data, 'UTF8') FROM vs_blobs WHERE name = 'models/acme/sre/dup'`); b != `{"from":"acme"}` {
		t.Errorf("target-org blob = %q, must not be overwritten", b)
	}

	// Non-colliding keys still move.
	if n := scalar[int](t, db,
		`SELECT count(*) FROM vs_patterns WHERE org_id = 'acme' AND id = 'p-move'`); n != 1 {
		t.Error("non-colliding pattern did not move")
	}
	if n := scalar[int](t, db,
		`SELECT count(*) FROM vs_blobs WHERE name = 'models/acme/sre/moves'`); n != 1 {
		t.Error("non-colliding model blob did not move")
	}

	// A skipped parent keeps its children: parent and children never split
	// across orgs, which is what keeps the foreign key satisfiable.
	if org := scalar[string](t, db,
		`SELECT org_id FROM vs_logs WHERE pattern_id = 'p-dup' AND template = 'stale <*>'`); org != "default" {
		t.Errorf("child of a skipped pattern moved to %q; parent and children must stay together", org)
	}
	if n := scalar[int](t, db, `
		SELECT count(*) FROM vs_logs l
		WHERE NOT EXISTS (SELECT 1 FROM vs_patterns p WHERE p.org_id = l.org_id AND p.id = l.pattern_id)`); n != 0 {
		t.Errorf("%d orphaned vs_logs rows after a conflicting migration", n)
	}
}

// TestPostgresMigrateOrgScriptRejectsDefaultOrg proves the guard that stops an
// operator from "migrating" to the org the script migrates away from — and
// that the abort rolls everything back.
func TestPostgresMigrateOrgScriptRejectsDefaultOrg(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset
	db := pgDB(t, p)
	seedDefaultOrg(t, db)

	err := runOrgMigration(t, db, "default")
	if err == nil {
		t.Fatal("migrating to the default org must fail")
	}
	if !strings.Contains(err.Error(), "migrates AWAY from") {
		t.Errorf("unexpected error: %v", err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM vs_incidents WHERE org_id = 'default'`); n != 2 {
		t.Errorf("%d default-org incidents after the aborted run, want 2 (nothing may change)", n)
	}
}
