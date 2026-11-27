package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// seedThinningMonth stores, in one scope, occurrences sampled six times in
// one hour ten days ago, and many more read-model versions with no samples,
// so a pass that joined every version would read far more than it thins.
func seedThinningMonth(t *testing.T, p *postgresProvider, occurrences, idle int) (string, time.Time) {
	t.Helper()
	scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("thinning-%d", time.Now().UnixNano()), ClusterUID: "physical"}
	key, err := evidenceScopeKey(scope)
	if err != nil {
		t.Fatal(err)
	}
	hour := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Hour)
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid,read_model_ready,heads_backfill,observed_lag_scan) VALUES($1,'default',$2,'physical',TRUE,NULL,NULL)`, []any{key, scope.ClusterID}},
		{`SELECT versus_ensure_evidence_partition(to_regclass('kubernetes_evidence_occurrence_observations'),$1)`, []any{hour}},
		{`INSERT INTO kubernetes_evidence_occurrence_versions(scope_key,payload_version,occurrence_id,payload) SELECT $1,'v-'||i,'o-'||i,'{}'::jsonb FROM generate_series(1,$2::int) i`, []any{key, occurrences + idle}},
		{`INSERT INTO kubernetes_evidence_occurrence_read_versions(scope_key,occurrence_id,payload_version,revision,observed_at,first_seen,root_uid,namespace,kind,rule,severity,authorization_kind,broken,observation_only,payload)
 SELECT $1,'o-'||i,'v-'||i,i*100,$3::timestamptz,$3::timestamptz,'root-'||(i%50),'ns','Pod','pod.crash_loop','warning','Pod',FALSE,FALSE,'{}'::jsonb FROM generate_series(1,$2::int) i`, []any{key, occurrences + idle, hour}},
		{`INSERT INTO kubernetes_evidence_occurrence_observations(scope_key,occurrence_id,revision,observed_at,last_seen,payload_version)
 SELECT $1,'o-'||i,i*100+k,$3::timestamptz+make_interval(mins=>k*5),$3::timestamptz+make_interval(mins=>k*5),'v-'||i FROM generate_series(1,$2::int) i,generate_series(0,5) k`, []any{key, occurrences, hour}},
		{`ANALYZE kubernetes_evidence_occurrence_read_versions`, nil},
		{`ANALYZE kubernetes_evidence_occurrence_observations`, nil},
	} {
		if _, err = p.db.ExecContext(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatalf("%v\n%s", err, statement.sql)
		}
	}
	return key, hour
}

// thinningPass runs one thinning pass in its own transaction under the
// compaction statement limit. It returns how many of the scope's samples the
// pass removed, whether the pass was in that scope, and whether the walk has
// more to examine.
func thinningPass(t *testing.T, p *postgresProvider, key string, expired time.Time) (int, bool, bool) {
	t.Helper()
	count := func() int {
		var n int
		if err := p.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1`, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	p.thinning.mu.Lock()
	inScope := p.thinning.scope == key
	p.thinning.mu.Unlock()
	tx, err := p.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), `SET LOCAL statement_timeout='2s'`); err != nil {
		t.Fatal(err)
	}
	_, more, err := p.thinObservations(t.Context(), tx, expired)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	p.thinning.mu.Lock()
	inScope = inScope || p.thinning.scope == key
	p.thinning.mu.Unlock()
	return before - count(), inScope, more
}

// A thinning pass looks each candidate's opening up through the read model's
// key instead of reading every version, and passes remove at most a bounded
// batch each until only the openings and the last sample of each hour stay.
func TestPostgresObservationThinningUsesKeysAndProgressesInBatches(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	const occurrences, idle = 2000, 40000
	key, _ := seedThinningMonth(t, p, occurrences, idle)
	expired := time.Now().UTC().Add(-7 * 24 * time.Hour)

	tx, err := p.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	err = tx.QueryRowContext(t.Context(), "EXPLAIN (FORMAT JSON) "+observationThinningStatement, expired, expired.Add(time.Hour), key, time.Time{}, time.Unix(0, 0).UTC(), int64(-1), "", observationThinningWalk).Scan(&raw)
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err = json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	versionsByKey, versionsScanned := 0, 0
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == "kubernetes_evidence_occurrence_read_versions" {
			if strings.Contains(fmt.Sprint(node["Node Type"]), "Index") {
				versionsByKey++
			} else {
				versionsScanned++
			}
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			walk(child.(map[string]any))
		}
	}
	walk(plans[0].Plan)
	if versionsScanned != 0 || versionsByKey == 0 {
		t.Fatalf("thinning reads the version read model without its key (scans=%d keyed=%d):\n%s", versionsScanned, versionsByKey, raw)
	}

	// One cycle walks every scope of the database; this scope's samples take
	// one pass per walk of observationThinningWalk, and one more finds the
	// scope done.
	removed, passesHere := 0, 0
	for pass := 0; ; pass++ {
		if pass > 10000 {
			t.Fatal("the thinning cycle did not end")
		}
		n, inScope, more := thinningPass(t, p, key, expired)
		if inScope {
			passesHere++
		}
		if n > observationThinningWalk {
			t.Fatalf("pass %d removed %d samples, more than it may examine", pass, n)
		}
		removed += n
		if !more {
			break
		}
	}
	if want := (occurrences*6+observationThinningWalk-1)/observationThinningWalk + 1; passesHere != want {
		t.Fatalf("the walk took %d passes in this scope, want %d: one per %d samples examined", passesHere, want, observationThinningWalk)
	}
	if removed != occurrences*4 {
		t.Fatalf("thinning removed %d samples, want %d: all but each version's opening and its hour's last sample", removed, occurrences*4)
	}
	var kept int
	if err = p.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_occurrence_observations o JOIN kubernetes_evidence_occurrence_read_versions r ON r.scope_key=o.scope_key AND r.payload_version=o.payload_version
 WHERE o.scope_key=$1 AND (o.revision=r.revision OR o.revision=r.revision+5)`, key).Scan(&kept); err != nil || kept != occurrences*2 {
		t.Fatalf("kept %d openings and hour-last samples, want %d (%v)", kept, occurrences*2, err)
	}
}

// Passes that keep running out of statement time stop for a while with one
// warning, instead of retrying every round; the samples stay, and thinning
// resumes after the pause.
func TestPostgresObservationThinningStopsAfterRepeatedTimeouts(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	seedThinningMonth(t, p, 0, 1)
	now := time.Now().UTC()
	p.evidenceClock = func() time.Time { return now }
	calls := 0
	p.thinning.before = func(context.Context, *sql.Tx) error {
		calls++
		return &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
	}
	buf := captureStorageLog(t, "info")
	pass := func() {
		t.Helper()
		tx, err := p.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		compacted, _, err := p.thinObservations(t.Context(), tx, now.Add(-7*24*time.Hour))
		if err != nil || len(compacted) != 0 {
			t.Fatalf("a pass out of time failed the round: %v %v", compacted, err)
		}
		// The transaction is still usable after the pass was undone.
		var one int
		if err = tx.QueryRowContext(t.Context(), `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("the round's transaction broke after the pass: %v", err)
		}
	}
	for range observationThinningTimeouts {
		pass()
	}
	lines := logLines(buf, "observation thinning")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN ") || !strings.Contains(lines[0], "statement timeout") || !strings.Contains(lines[0], "Check") {
		t.Fatalf("want one warning naming the cause and what to check, got %q", lines)
	}
	for range 5 {
		pass()
	}
	if calls != observationThinningTimeouts {
		t.Fatalf("thinning ran %d passes, want it stopped after %d", calls, observationThinningTimeouts)
	}
	if lines = logLines(buf, "observation thinning"); len(lines) != 0 {
		t.Fatalf("paused rounds logged %q", lines)
	}
	now = now.Add(observationThinningPause)
	p.thinning.before = func(context.Context, *sql.Tx) error { calls++; return nil }
	pass()
	if calls != observationThinningTimeouts+1 {
		t.Fatalf("thinning did not resume after the pause: %d passes", calls)
	}
}

// A pass that the database itself cancels at its statement timeout is undone
// back to its savepoint: the compaction transaction stays usable, and its own
// settings are as they were before the pass.
func TestPostgresObservationThinningTimeoutLeavesTheRoundUsable(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	key, _ := seedThinningMonth(t, p, 10, 0)
	p.thinning.before = func(ctx context.Context, tx *sql.Tx) error {
		// A pass slower than its statement limit: the limit is lowered for
		// the pass alone and the statement outlasts it.
		if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout='50ms'`); err != nil { // testlint:allow the database's own statement limit is what this test triggers
			return err
		}
		_, err := tx.ExecContext(ctx, `SELECT pg_sleep(30)`)
		return err
	}
	tx, err := p.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), `SET LOCAL statement_timeout='20s'`); err != nil {
		t.Fatal(err)
	}
	compacted, more, err := p.thinObservations(t.Context(), tx, time.Now().UTC().Add(-7*24*time.Hour))
	if err != nil || len(compacted) != 0 || more {
		t.Fatalf("a pass cancelled at its statement limit failed the round: %v %v %v", compacted, more, err)
	}
	var limit string
	if err = tx.QueryRowContext(t.Context(), `SHOW statement_timeout`).Scan(&limit); err != nil {
		t.Fatalf("the round's transaction broke after the cancelled pass: %v", err)
	}
	if limit != "20s" {
		t.Fatalf("the round's statement limit is %q after the pass, want the round's own 20s", limit)
	}
	var samples int
	if err = tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1`, key).Scan(&samples); err != nil || samples != 60 {
		t.Fatalf("the cancelled pass changed the samples: %d %v", samples, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("the round could not commit after the cancelled pass: %v", err)
	}
	p.thinning.mu.Lock()
	timeouts := p.thinning.timeouts
	p.thinning.mu.Unlock()
	if timeouts != 1 {
		t.Fatalf("the cancelled pass counted %d timeouts, want 1", timeouts)
	}
}
