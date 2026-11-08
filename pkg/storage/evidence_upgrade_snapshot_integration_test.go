package storage

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// snapshotTables are the exported evidence tables in foreign-key order. The
// monthly partition files duplicate their parents' rows and are only used to
// check that every restored row landed in its month.
var snapshotTables = []string{
	"kubernetes_evidence_scopes",
	"kubernetes_evidence_captures",
	"kubernetes_evidence_events",
	"kubernetes_evidence_occurrence_versions",
	"kubernetes_evidence_occurrence_observations",
	"kubernetes_evidence_issues",
	"kubernetes_evidence_occurrence_read_versions",
}

func snapshotHeader(t *testing.T, path string) ([]string, int) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	var columns []string
	lines := 0
	for scanner.Scan() {
		if lines == 0 {
			for _, name := range strings.Split(scanner.Text(), "\t") {
				columns = append(columns, strings.Trim(name, `"`))
			}
		}
		lines++
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return columns, lines - 1
}

// snapshotRestore loads a tab-separated, quoted, day-first table export into
// a pre-upgrade schema. It fails, never skips, when a file is unreadable.
func snapshotRestore(t *testing.T, dsn, dir string) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err = conn.Exec(ctx, `SET DateStyle='ISO, DMY'`); err != nil {
		t.Fatal(err)
	}
	partitions, err := filepath.Glob(filepath.Join(dir, "evidence_*_month_*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range partitions {
		name := strings.TrimSuffix(filepath.Base(path), ".txt")
		parent := "kubernetes_evidence_events"
		if strings.HasPrefix(name, "evidence_observation_month_") {
			parent = "kubernetes_evidence_occurrence_observations"
		}
		month, err := time.Parse("200601", name[len(name)-6:])
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Exec(ctx, `SELECT versus_ensure_evidence_partition(to_regclass($1::text),$2)`, parent, month); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range snapshotTables {
		path := filepath.Join(dir, table+".txt")
		columns, rows := snapshotHeader(t, path)
		quoted := make([]string, len(columns))
		for i, column := range columns {
			quoted[i] = pgx.Identifier{column}.Sanitize()
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		tag, err := conn.PgConn().CopyFrom(ctx, file, fmt.Sprintf(`COPY %s(%s) FROM STDIN WITH (FORMAT csv, DELIMITER E'\t', QUOTE '"', HEADER true)`, pgx.Identifier{table}.Sanitize(), strings.Join(quoted, ",")))
		file.Close()
		if err != nil {
			t.Fatalf("RESTORE BLOCKED: %s: %v", table, err)
		}
		if int(tag.RowsAffected()) != rows {
			t.Fatalf("RESTORE BLOCKED: %s restored %d of %d rows", table, tag.RowsAffected(), rows)
		}
		t.Logf("RESTORE table=%s rows=%d seconds=%.1f", table, rows, time.Since(started).Seconds())
	}
	for _, path := range partitions {
		name := strings.TrimSuffix(filepath.Base(path), ".txt")
		_, want := snapshotHeader(t, path)
		var got int
		if err = conn.QueryRow(ctx, "SELECT COUNT(*) FROM "+pgx.Identifier{name}.Sanitize()).Scan(&got); err != nil || got != want {
			t.Fatalf("RESTORE BLOCKED: partition %s holds %d rows, export has %d (%v)", name, got, want, err)
		}
	}
	for _, name := range []string{"kubernetes_evidence_events_retired", "kubernetes_evidence_observations_retired"} {
		if _, rows := snapshotHeader(t, filepath.Join(dir, name+".txt")); rows != 0 {
			t.Fatalf("RESTORE BLOCKED: %s has %d rows; this restore expects the completed partition layout", name, rows)
		}
	}
	if _, err = conn.Exec(ctx, `SELECT setval('kubernetes_evidence_events_sequence_seq',(SELECT MAX(sequence) FROM kubernetes_evidence_events))`); err != nil {
		t.Fatal(err)
	}
	for _, table := range snapshotTables {
		if _, err = conn.Exec(ctx, "ANALYZE "+pgx.Identifier{table}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshotOwnDatabase creates a fresh qa_ database on the test server for
// this run alone, points TEST_POSTGRES_DSN at it, and drops it when the test
// ends, so the schema rewind below never touches a shared database.
func snapshotOwnDatabase(t *testing.T) string {
	t.Helper()
	server := os.Getenv("TEST_POSTGRES_DSN")
	if server == "" {
		t.Fatal("TEST_POSTGRES_DSN is required")
	}
	target, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("qa_upgrade_snapshot_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(t.Context(), server)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	// Registered first, so it runs after every provider on it has closed.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		conn, err := pgx.Connect(ctx, server)
		if err != nil {
			t.Errorf("drop %s: %v", name, err)
			return
		}
		defer conn.Close(ctx)
		if _, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	target.Path = "/" + name
	own := target.String()
	conn, err := pgx.Connect(t.Context(), own)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var current string
	var tables int
	if err = conn.QueryRow(t.Context(), `SELECT current_database(),(SELECT COUNT(*) FROM pg_tables WHERE schemaname='public')`).Scan(&current, &tables); err != nil {
		t.Fatal(err)
	}
	if current != name || !strings.HasPrefix(current, "qa_") || tables != 0 {
		t.Fatalf("refusing to rewind %q: not the fresh qa_ database %q (public tables %d)", current, name, tables)
	}
	t.Setenv("TEST_POSTGRES_DSN", own)
	return own
}

// TestPostgresEvidenceUpgradeFromProductionSnapshot restores a production
// evidence export taken before the heads table existed, starts the store so
// the heads migration runs without a backfill, and re-sends the cluster's
// current records as a capture would. The save must finish inside the 20 s
// save lease and must not append a version for any record whose stored
// version already matches its content; a change hashed by the old rule may
// gain one row, once. A second identical save must append nothing at all.
func TestPostgresEvidenceUpgradeFromProductionSnapshot(t *testing.T) {
	dir := os.Getenv("EVIDENCE_UPGRADE_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("set EVIDENCE_UPGRADE_SNAPSHOT_DIR to a production evidence table export")
	}
	dsn := snapshotOwnDatabase(t)
	// Build the pre-upgrade schema: every earlier migration, the partition
	// cutover the export was taken after, and no heads table.
	old, _ := headsPartitionedProvider(t)
	// Everything from the heads table on is undone: heads and their fence,
	// the scope column, and the occurrence span and object indexes.
	if _, err := old.db.Exec(`DROP TABLE kubernetes_evidence_event_heads;
DROP FUNCTION IF EXISTS versus_evidence_heads_fence() CASCADE;
DROP FUNCTION IF EXISTS versus_evidence_heads_guard() CASCADE;
DROP FUNCTION IF EXISTS versus_evidence_gap_reach() CASCADE;
DROP FUNCTION IF EXISTS versus_evidence_observed_lag() CASCADE;
ALTER TABLE kubernetes_evidence_scopes DROP COLUMN IF EXISTS heads_backfill, DROP COLUMN IF EXISTS gap_reach, DROP COLUMN IF EXISTS observed_lag, DROP COLUMN IF EXISTS observed_lag_scan;
DROP INDEX IF EXISTS kubernetes_occurrence_read_open_span, kubernetes_occurrence_read_visible_span, kubernetes_occurrence_read_replaced, kubernetes_history_object_time;
DELETE FROM versus_schema_migrations WHERE filename>='025'`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	snapshotRestore(t, dsn, dir)

	started := time.Now()
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("store did not start on the restored snapshot: %v", err)
	}
	defer provider.Close()
	t.Logf("UPGRADE start_seconds=%.2f", time.Since(started).Seconds())
	p := provider.(*postgresProvider)
	// The background heads pass is driven by hand below, so the saves are
	// timed with every restored scope still incomplete.
	p.maintenanceCancel()
	<-p.maintenanceDone
	var applied, complete, heads int
	if err = p.db.QueryRow(`SELECT (SELECT COUNT(*) FROM versus_schema_migrations WHERE filename>='025'),
 (SELECT COUNT(*) FROM kubernetes_evidence_scopes WHERE heads_backfill IS NULL),(SELECT COUNT(*) FROM kubernetes_evidence_event_heads)`).Scan(&applied, &complete, &heads); err != nil || applied < 5 || complete != 0 || heads != 0 {
		t.Fatalf("upgrade migrations: applied=%d complete scopes=%d heads=%d err=%v (want 025 onwards applied, every restored scope incomplete, no backfill)", applied, complete, heads, err)
	}

	var key, orgID, clusterID, clusterUID string
	if err = p.db.QueryRow(`SELECT s.scope_key,s.org_id,s.cluster_id,s.cluster_uid FROM kubernetes_evidence_scopes s ORDER BY (SELECT COUNT(*) FROM kubernetes_evidence_events e WHERE e.scope_key=s.scope_key) DESC LIMIT 1`).Scan(&key, &orgID, &clusterID, &clusterUID); err != nil {
		t.Fatal(err)
	}
	scope := EvidenceScope{OrgID: orgID, ClusterID: clusterID, ClusterUID: clusterUID}
	if derived, _ := evidenceScopeKey(scope); derived != key {
		t.Fatalf("restored scope key %s does not match its identity (%s)", key, derived)
	}
	// The cluster's current change and crash records, most recently
	// observed first, as many as one save accepts.
	rows, err := p.db.Query(`SELECT event_id,event_version,event_type,event_at,observed_at,namespace,resource_name,kind,authorization_kind,resource_uid,root_uid,pod_uid,rule,epoch,payload,is_break,priority
 FROM kubernetes_evidence_events WHERE scope_key=$1 AND superseded_seq IS NULL AND event_type IN('change','crash') ORDER BY observed_at DESC,sequence DESC LIMIT 10000`, key)
	if err != nil {
		t.Fatal(err)
	}
	var records []EvidenceRecord
	sameVersion := map[string]bool{}
	byType := map[string]int{}
	for rows.Next() {
		var r EvidenceRecord
		var version string
		var payload []byte
		if err = rows.Scan(&r.ID, &version, &r.Type, &r.At, &r.ObservedAt, &r.Namespace, &r.Name, &r.Kind, &r.AuthorizationKind, &r.UID, &r.RootUID, &r.PodUID, &r.Rule, &r.Epoch, &payload, &r.Broken, &r.Priority); err != nil {
			t.Fatal(err)
		}
		r.Payload = payload
		sameVersion[r.ID] = evidenceVersion(r) == version
		byType[r.Type]++
		records = append(records, r)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	identical := 0
	for _, same := range sameVersion {
		if same {
			identical++
		}
	}
	t.Logf("BATCH records=%d types=%v stored_version_matches_current_identity=%d differs=%d", len(records), byType, identical, len(records)-identical)

	journal := func() (count int, high int64) {
		t.Helper()
		if err := p.db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(sequence),0) FROM kubernetes_evidence_events WHERE scope_key=$1`, key).Scan(&count, &high); err != nil {
			t.Fatal(err)
		}
		return count, high
	}
	save := func(id string) (time.Duration, []string) {
		t.Helper()
		cp, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		if at.Before(cp.At) {
			at = cp.At.Add(time.Second)
		}
		batch := EvidenceBatch{ID: id, Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: records[0].Epoch, Principal: cp.Principal, Records: make([]EvidenceRecord, len(records))}
		for i, r := range records {
			// A re-detection: same evidence, observed now.
			r.ObservedAt = at
			batch.Records[i] = r
		}
		_, before := journal()
		started := time.Now()
		err = p.CommitEvidence(t.Context(), scope, batch)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("save %s failed after %s: %v", id, elapsed, err)
		}
		appended, err := p.db.Query(`SELECT event_id FROM kubernetes_evidence_events WHERE scope_key=$1 AND sequence>$2`, key, before)
		if err != nil {
			t.Fatal(err)
		}
		defer appended.Close()
		var ids []string
		for appended.Next() {
			var id string
			if err = appended.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err = appended.Err(); err != nil {
			t.Fatal(err)
		}
		return elapsed, ids
	}
	countBefore, _ := journal()
	first, appended := save("upgrade-proof-first")
	// A change stored before the upgrade carries a hash that still covered
	// observation times, so it may gain exactly one row on this save; nothing
	// else may grow.
	var unchangedAppended, reversioned []string
	seen := map[string]bool{}
	for _, id := range appended {
		if sameVersion[id] {
			unchangedAppended = append(unchangedAppended, id)
		} else {
			reversioned = append(reversioned, id)
		}
		if seen[id] {
			t.Errorf("first save appended %s more than once", id)
		}
		seen[id] = true
	}
	for id, same := range sameVersion {
		if !same && !seen[id] {
			t.Logf("old-rule change %s was not re-versioned", id)
		}
	}
	if len(reversioned) > len(records)-identical {
		t.Errorf("re-versioned %d rows for %d old-rule changes", len(reversioned), len(records)-identical)
	}
	if err = p.db.QueryRow(`SELECT COUNT(*) FROM kubernetes_evidence_event_heads WHERE scope_key=$1`, key).Scan(&heads); err != nil {
		t.Fatal(err)
	}
	t.Logf("SAVE first seconds=%.2f appended=%d (identity unchanged=%d, re-versioned under the new identity=%d) heads=%d journal_before=%d", first.Seconds(), len(appended), len(unchangedAppended), len(reversioned), heads, countBefore)
	if first >= 20*time.Second {
		t.Errorf("first save after upgrade took %s, beyond the 20 s lease", first)
	}
	if len(unchangedAppended) > 0 {
		sort.Strings(unchangedAppended)
		t.Errorf("first save appended %d versions for unchanged records, e.g. %v", len(unchangedAppended), unchangedAppended[:min(5, len(unchangedAppended))])
	}
	if heads != len(records) {
		t.Errorf("first save left %d heads for %d records", heads, len(records))
	}
	second, appended := save("upgrade-proof-second")
	countAfter, _ := journal()
	t.Logf("SAVE second seconds=%.2f appended=%d journal_after=%d", second.Seconds(), len(appended), countAfter)
	if second >= 20*time.Second {
		t.Errorf("second save took %s, beyond the 20 s lease", second)
	}
	if len(appended) != 0 {
		t.Errorf("identical second save appended %d rows", len(appended))
	}
	if results := os.Getenv("HARNESS_RESULTS_DIR"); results != "" {
		scaleWriteJSON(t, filepath.Join(results, "upgrade-snapshot.json"), map[string]any{
			"scope_key": key, "records": len(records), "types": byType, "stored_version_matches": identical,
			"first_save_seconds": first.Seconds(), "first_appended": len(reversioned) + len(unchangedAppended),
			"first_appended_unchanged": len(unchangedAppended), "first_reversioned": len(reversioned),
			"second_save_seconds": second.Seconds(), "second_appended": len(appended), "heads_after_first": heads,
			"journal_before": countBefore, "journal_after": countAfter,
		})
	}
	// The background pass then gives every restored current row its head and
	// marks the scope complete, in batches that fit the statement timeout.
	started = time.Now()
	batches := 0
	for {
		var pending int
		if err = p.db.QueryRow(`SELECT COUNT(*) FROM kubernetes_evidence_scopes WHERE heads_backfill IS NOT NULL`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		ctx, cancel := context.WithTimeout(t.Context(), evidenceBackgroundBackfill.batchTimeout)
		_, err = p.backfillEvidenceHeads(ctx, evidenceBackgroundBackfill.events)
		cancel()
		if err != nil {
			t.Fatalf("heads pass batch %d: %v", batches, err)
		}
		batches++
	}
	var mismatched int
	if err = p.db.QueryRow(`SELECT COUNT(*) FROM kubernetes_evidence_events e WHERE e.superseded_seq IS NULL AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_event_heads h WHERE h.scope_key=e.scope_key AND h.event_id=e.event_id AND h.sequence>=e.sequence)`).Scan(&mismatched); err != nil || mismatched != 0 {
		t.Errorf("after the heads pass %d current rows have no head (%v)", mismatched, err)
	}
	t.Logf("HEADS backfill seconds=%.1f batches=%d", time.Since(started).Seconds(), batches)
}
