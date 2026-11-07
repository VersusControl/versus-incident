package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// headsTestProvider opens Postgres with background maintenance stopped, so a
// test controls every write.
func headsTestProvider(t *testing.T) (*postgresProvider, EvidenceScope) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn, ObservationHeartbeat: "5m", EvidenceRetention: "0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	p := provider.(*postgresProvider)
	p.maintenanceCancel()
	<-p.maintenanceDone
	return p, EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("heads-%d", time.Now().UnixNano()), ClusterUID: "physical"}
}

func headsCommit(t *testing.T, p *postgresProvider, scope EvidenceScope, id string, at time.Time, records ...EvidenceRecord) {
	t.Helper()
	cp, err := p.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	batch := EvidenceBatch{ID: id, Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: "stable", Records: records}
	if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
}

func headsStatus(id string, restarts int, at time.Time) EvidenceRecord {
	payload, _ := json.Marshal(map[string]any{"cluster": "heads", "namespace": "shop", "pod": "api", "container": "app", "type": "status", "waiting_reason": "CrashLoopBackOff", "restart_count": restarts, "at": at, "observed_at": at})
	return EvidenceRecord{ID: id, Type: "crash", At: at, ObservedAt: at, Namespace: "shop", Name: "api", Kind: "Deployment", AuthorizationKind: "Pod", UID: "pod-uid", RootUID: "root-uid", PodUID: "pod-uid", Epoch: "stable", Payload: payload}
}

func headsChange(id, value string, at time.Time) EvidenceRecord {
	payload, _ := json.Marshal(map[string]any{"cluster": "heads", "kind": "Deployment", "name": id, "type": "created", "value": value, "at": at})
	return EvidenceRecord{ID: id, Type: "change", At: at, ObservedAt: at, Namespace: "shop", Name: id, Kind: "Deployment", AuthorizationKind: "Deployment", UID: id, RootUID: id, Epoch: "stable", Payload: payload}
}

func TestPostgresCrashStatusRefreshesInPlaceOncePerHeartbeat(t *testing.T) {
	p, scope := headsTestProvider(t)
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	headsCommit(t, p, scope, "c0", origin, headsStatus("crash:status", 7, origin))
	observed := func() (int, time.Time, time.Time) {
		t.Helper()
		var rows int
		var column, payload time.Time
		if err := p.db.QueryRow(`SELECT COUNT(*),MAX(observed_at),MAX((payload->>'observed_at')::timestamptz) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id='crash:status'`, key).Scan(&rows, &column, &payload); err != nil {
			t.Fatal(err)
		}
		return rows, column, payload
	}
	// Re-observed inside the heartbeat: nothing is written.
	headsCommit(t, p, scope, "c1", origin.Add(time.Minute), headsStatus("crash:status", 7, origin.Add(time.Minute)))
	if rows, column, _ := observed(); rows != 1 || !column.Equal(origin) {
		t.Fatalf("re-observation inside heartbeat wrote: rows=%d observed=%s", rows, column)
	}
	// Re-observed after the heartbeat: the same row now says it was still seen.
	later := origin.Add(6 * time.Minute)
	headsCommit(t, p, scope, "c2", later, headsStatus("crash:status", 7, later))
	rows, column, payload := observed()
	if rows != 1 || !column.Equal(later) || !payload.Equal(later) {
		t.Fatalf("heartbeat refresh: rows=%d column=%s payload=%s want %s", rows, column, payload, later)
	}
	var at time.Time
	if err := p.db.QueryRow(`SELECT event_at FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id='crash:status'`, key).Scan(&at); err != nil || !at.Equal(origin) {
		t.Fatalf("first observation time moved: %s %v", at, err)
	}
	page, err := p.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "crash", ByObservation: true, Since: later.Add(-time.Minute), Until: later.Add(time.Minute), Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("still-crashing record missing from the recent window: %d %v", len(page.Items), err)
	}
}

func TestPostgresEventSavedBeforeHeadsIsSupersededOnce(t *testing.T) {
	p, scope := headsTestProvider(t)
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	headsCommit(t, p, scope, "c0", origin, headsChange("change:legacy", "1", origin))
	// Simulate history written before the heads table existed: the upgrade
	// marks such a scope for the background heads pass.
	if _, err := p.db.Exec(`DELETE FROM kubernetes_evidence_event_heads WHERE scope_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.Exec(`UPDATE kubernetes_evidence_scopes SET heads_backfill=0 WHERE scope_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	headsCommit(t, p, scope, "c1", origin.Add(time.Minute), headsChange("change:legacy", "1", origin.Add(time.Minute)))
	var rows, heads int
	if err := p.db.QueryRow(`SELECT (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1),(SELECT COUNT(*) FROM kubernetes_evidence_event_heads WHERE scope_key=$1)`, key).Scan(&rows, &heads); err != nil || rows != 1 || heads != 1 {
		t.Fatalf("unchanged legacy event: rows=%d heads=%d err=%v", rows, heads, err)
	}
	headsCommit(t, p, scope, "c2", origin.Add(2*time.Minute), headsChange("change:legacy", "2", origin.Add(2*time.Minute)))
	var current, superseded int
	if err := p.db.QueryRow(`SELECT COUNT(*) FILTER(WHERE superseded_seq IS NULL),COUNT(*) FILTER(WHERE superseded_seq IS NOT NULL) FROM kubernetes_evidence_events WHERE scope_key=$1`, key).Scan(&current, &superseded); err != nil || current != 1 || superseded != 1 {
		t.Fatalf("real change after upgrade: current=%d superseded=%d err=%v", current, superseded, err)
	}
	var headSequence, liveSequence int64
	if err := p.db.QueryRow(`SELECT h.sequence,e.sequence FROM kubernetes_evidence_event_heads h JOIN kubernetes_evidence_events e ON e.scope_key=h.scope_key AND e.event_id=h.event_id AND e.superseded_seq IS NULL WHERE h.scope_key=$1`, key).Scan(&headSequence, &liveSequence); err != nil || headSequence != liveSequence {
		t.Fatalf("head does not point at the current row: %d %d %v", headSequence, liveSequence, err)
	}
}

func TestPostgresDeleteEvidenceRemovesHeads(t *testing.T) {
	p, scope := headsTestProvider(t)
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	headsCommit(t, p, scope, "c0", origin, headsChange("change:deleted", "1", origin))
	if err := p.DeleteEvidence(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	var heads int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM kubernetes_evidence_event_heads WHERE scope_key=$1`, key).Scan(&heads); err != nil || heads != 0 {
		t.Fatalf("deleted history kept heads: %d %v", heads, err)
	}
}

// TestPostgresEvidenceSoakStaysBoundedOverTime replays many sync cycles over a
// cluster with crash-looping containers and occasional real changes. History
// must grow only with real changes, and a save must cost the same at the end
// as at the start, however long the deployment has run.
func TestPostgresEvidenceSoakStaysBoundedOverTime(t *testing.T) {
	if testing.Short() {
		t.Skip("soak: hundreds of sync cycles against Postgres")
	}
	p, scope := headsTestProvider(t)
	key, _ := evidenceScopeKey(scope)
	const objects, crashing, cycles = 2000, 200, 240
	origin := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	values := make([]int, objects)
	restarts := make([]int, crashing)
	realChanges := 0
	durations := make([]time.Duration, 0, cycles)
	for cycle := 0; cycle < cycles; cycle++ {
		// Twelve simulated minutes per cycle covers 48 hours.
		at := origin.Add(time.Duration(cycle) * 12 * time.Minute)
		if cycle > 0 {
			values[cycle%objects]++
			realChanges++
			if cycle%10 == 0 {
				restarts[cycle%crashing]++
				realChanges++
			}
		}
		records := make([]EvidenceRecord, 0, objects+crashing)
		for i := 0; i < objects; i++ {
			// Every cycle re-detects every object, as a rebuilt epoch would.
			records = append(records, headsChange(fmt.Sprintf("change:object-%d", i), fmt.Sprint(values[i]), at))
		}
		for i := 0; i < crashing; i++ {
			records = append(records, headsStatus(fmt.Sprintf("crash:status-%d-%d", i, restarts[i]), restarts[i], at))
		}
		started := time.Now()
		headsCommit(t, p, scope, fmt.Sprintf("cycle-%d", cycle), at, records...)
		durations = append(durations, time.Since(started))
	}
	var rows int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1`, key).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if want := objects + crashing + realChanges; rows != want {
		t.Fatalf("history grew with time, not change: rows=%d want=%d (real changes %d)", rows, want, realChanges)
	}
	median := func(values []time.Duration) time.Duration {
		sorted := append([]time.Duration(nil), values...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		return sorted[len(sorted)/2]
	}
	early, late := median(durations[5:45]), median(durations[len(durations)-40:])
	t.Logf("save median early=%s late=%s rows=%d", early, late, rows)
	if late > 3*early+50*time.Millisecond {
		t.Fatalf("save cost grew with uptime: early=%s late=%s", early, late)
	}
}

// A scope that holds history from before heads existed is given its heads by
// the background pass, slice by slice, and then marked complete; later saves
// still recognize its unchanged and changed events.
func TestPostgresHeadsBackfillCompletesAScopeFromBeforeHeads(t *testing.T) {
	p, scope := headsTestProvider(t)
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	records := []EvidenceRecord{}
	for i := 0; i < 40; i++ {
		records = append(records, headsChange(fmt.Sprintf("change:old-%02d", i), "1", origin))
	}
	headsCommit(t, p, scope, "c0", origin, records...)
	headsCommit(t, p, scope, "c1", origin.Add(time.Minute), headsChange("change:old-00", "2", origin.Add(time.Minute)))
	if _, err := p.db.Exec(`DELETE FROM kubernetes_evidence_event_heads WHERE scope_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.Exec(`UPDATE kubernetes_evidence_scopes SET heads_backfill=0 WHERE scope_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	passes := 0
	for {
		var pending bool
		if err := p.db.QueryRow(`SELECT heads_backfill IS NOT NULL FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if !pending {
			break
		}
		if passes++; passes > 10 {
			t.Fatal("heads backfill did not finish")
		}
		if _, err := p.backfillEvidenceHeads(t.Context(), 16); err != nil {
			t.Fatal(err)
		}
	}
	if passes < 3 {
		t.Fatalf("41 rows in slices of 16 finished in %d passes", passes)
	}
	var heads, mismatched int
	if err := p.db.QueryRow(`SELECT COUNT(*),COUNT(*) FILTER(WHERE e.sequence IS NULL) FROM kubernetes_evidence_event_heads h
 LEFT JOIN kubernetes_evidence_events e ON e.scope_key=h.scope_key AND e.event_id=h.event_id AND e.sequence=h.sequence AND e.superseded_seq IS NULL WHERE h.scope_key=$1`, key).Scan(&heads, &mismatched); err != nil {
		t.Fatal(err)
	}
	if heads != 40 || mismatched != 0 {
		t.Fatalf("heads=%d, %d not on the current row; want 40 on current rows", heads, mismatched)
	}
	before := 0
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1`, key).Scan(&before); err != nil {
		t.Fatal(err)
	}
	headsCommit(t, p, scope, "c2", origin.Add(2*time.Minute), headsChange("change:old-00", "2", origin.Add(2*time.Minute)), headsChange("change:old-01", "2", origin.Add(2*time.Minute)), headsChange("change:new", "1", origin.Add(2*time.Minute)))
	var after, current int
	if err := p.db.QueryRow(`SELECT COUNT(*),COUNT(*) FILTER(WHERE superseded_seq IS NULL) FROM kubernetes_evidence_events WHERE scope_key=$1`, key).Scan(&after, &current); err != nil {
		t.Fatal(err)
	}
	if after-before != 2 || current != 41 {
		t.Fatalf("save after the pass appended %d rows with %d current, want 2 and 41", after-before, current)
	}
}

// Once heads are complete, a save of events never seen before reads no
// journal partition to look for their history; while a scope still holds
// rows from before heads, it does.
func TestPostgresNewEventsReadNoJournalOnceHeadsAreComplete(t *testing.T) {
	p, scope := headsTestProvider(t)
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	headsCommit(t, p, scope, "c0", origin, headsChange("change:seen", "1", origin))
	cfg, err := pgx.ParseConfig(os.Getenv("TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	trace := &scaleQueryTrace{}
	cfg.Tracer = trace
	traced := stdlib.OpenDB(*cfg)
	defer traced.Close()
	statement := func() string {
		t.Helper()
		trace.mu.Lock()
		trace.queries = nil
		trace.mu.Unlock()
		tx, err := traced.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		trace.enabled.Store(true)
		_, err = writeEvidenceEvents(t.Context(), tx, key, []EvidenceRecord{headsChange("change:brand-new", "1", origin)}, 5*time.Minute)
		trace.enabled.Store(false)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range trace.queries {
			if strings.Contains(q.SQL, "kubernetes_evidence_event_heads") && strings.Contains(q.SQL, "INSERT") {
				return q.SQL
			}
		}
		t.Fatal("write statement not captured")
		return ""
	}
	if sql := statement(); strings.Contains(sql, "ORDER BY p.sequence DESC") {
		t.Fatal("a scope created with heads still searches its journal for new events")
	}
	if _, err = p.db.Exec(`UPDATE kubernetes_evidence_scopes SET heads_backfill=0 WHERE scope_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if sql := statement(); !strings.Contains(sql, "ORDER BY p.sequence DESC") {
		t.Fatal("a scope with rows from before heads must look new events up in its journal")
	}
}

// headsWriterExec runs one statement as a writer that keeps heads current, as
// the store's own saves and maintenance do, so test seeding that writes
// matching journal rows and heads does not mark the scope incomplete.
func headsWriterExec(ctx context.Context, db *sql.DB, query string, args ...any) (sql.Result, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, evidenceHeadsWriter); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

// headsScopeState reads what a scope's journal and heads hold for one event.
func headsScopeState(t *testing.T, p *postgresProvider, key, eventID string) (rows, current int, headOnCurrent, complete bool) {
	t.Helper()
	if err := p.db.QueryRow(`SELECT (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id=$2),
 (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id=$2 AND superseded_seq IS NULL),
 EXISTS(SELECT 1 FROM kubernetes_evidence_event_heads h JOIN kubernetes_evidence_events e ON e.scope_key=h.scope_key AND e.event_id=h.event_id AND e.sequence=h.sequence AND e.superseded_seq IS NULL WHERE h.scope_key=$1 AND h.event_id=$2),
 (SELECT heads_backfill IS NULL FROM kubernetes_evidence_scopes WHERE scope_key=$1)`, key, eventID).Scan(&rows, &current, &headOnCurrent, &complete); err != nil {
		t.Fatal(err)
	}
	return rows, current, headOnCurrent, complete
}

// headsPageTotal is the number of change rows a filtered history read counts
// and lists for the scope, which the journal itself answers.
func headsPageTotal(t *testing.T, p *postgresProvider, scope EvidenceScope, at time.Time) (int, int) {
	t.Helper()
	page, err := p.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "change", Namespace: "shop", Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return page.Total, len(page.Items)
}

// A head removed by anything but the store (here a plain DELETE, as a restore
// of an older dump or an operator would) makes the scope incomplete, so the
// next saves compare with the journal: an unchanged record appends nothing and
// a real change supersedes the old version instead of adding a second current
// one.
func TestPostgresHeadRemovedOutsideTheStoreReopensTheScope(t *testing.T) {
	p, scope := headsPartitionedProvider(t)
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	headsCommit(t, p, scope, "c0", origin, headsChange("change:probe", "1", origin))
	if _, _, _, complete := headsScopeState(t, p, key, "change:probe"); !complete {
		t.Fatal("a scope the store created is not complete")
	}
	if _, err := p.db.Exec(`DELETE FROM kubernetes_evidence_event_heads WHERE scope_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, _, _, complete := headsScopeState(t, p, key, "change:probe"); complete {
		t.Fatal("removing heads outside the store left the scope complete")
	}
	headsCommit(t, p, scope, "c1", origin.Add(time.Minute), headsChange("change:probe", "1", origin.Add(time.Minute)))
	headsCommit(t, p, scope, "c2", origin.Add(2*time.Minute), headsChange("change:probe", "2", origin.Add(2*time.Minute)))
	rows, current, headOnCurrent, _ := headsScopeState(t, p, key, "change:probe")
	if rows != 2 || current != 1 || !headOnCurrent {
		t.Fatalf("after an unchanged resave and a real change: rows=%d current=%d head on current=%v, want 2, 1, true", rows, current, headOnCurrent)
	}
	if total, items := headsPageTotal(t, p, scope, origin); total != 1 || items != 1 {
		t.Fatalf("history read counts %d and lists %d versions, want 1 and 1", total, items)
	}
}

// A writer that keeps no heads, such as a replica of an earlier release still
// running during a rolling update, writes a new version with a plain INSERT
// and supersedes the old one with a plain UPDATE. The database marks the scope
// incomplete and drops the stale head, so the store's next saves neither add a
// second current version nor miss a change; the background pass then
// completes the scope again, and a later write of the same kind reopens it.
func TestPostgresWriteWithoutHeadsReopensTheScope(t *testing.T) {
	p, scope := headsPartitionedProvider(t)
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	headsCommit(t, p, scope, "c0", origin, headsChange("change:shared", "1", origin), headsChange("change:other", "1", origin))
	oldWriter := func(value string, at time.Time) {
		t.Helper()
		record := headsChange("change:shared", value, at)
		var sequence int64
		if err := p.db.QueryRow(`INSERT INTO kubernetes_evidence_events(scope_key,event_id,event_version,event_type,event_at,observed_at,namespace,resource_name,kind,authorization_kind,resource_uid,root_uid,epoch,payload)
 VALUES($1,$2,$3,'change',$4,$4,'shop',$2,'Deployment','Deployment',$2,$2,'stable',$5) RETURNING sequence`, key, record.ID, evidenceVersion(record), at, []byte(record.Payload)).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		if _, err := p.db.Exec(`UPDATE kubernetes_evidence_events SET superseded_seq=$3 WHERE scope_key=$1 AND event_id=$2 AND superseded_seq IS NULL AND sequence<>$3`, key, record.ID, sequence); err != nil {
			t.Fatal(err)
		}
	}
	oldWriter("2", origin.Add(time.Minute))
	rows, current, headOnCurrent, complete := headsScopeState(t, p, key, "change:shared")
	if complete || headOnCurrent || rows != 2 || current != 1 {
		t.Fatalf("after the old writer: rows=%d current=%d head on current=%v complete=%v, want 2, 1, false, false", rows, current, headOnCurrent, complete)
	}
	// The store resends what the old writer wrote, then a real change.
	headsCommit(t, p, scope, "c1", origin.Add(2*time.Minute), headsChange("change:shared", "2", origin.Add(2*time.Minute)), headsChange("change:other", "1", origin.Add(2*time.Minute)))
	if rows, current, headOnCurrent, _ = headsScopeState(t, p, key, "change:shared"); rows != 2 || current != 1 || !headOnCurrent {
		t.Fatalf("unchanged resave: rows=%d current=%d head on current=%v, want 2, 1, true", rows, current, headOnCurrent)
	}
	headsCommit(t, p, scope, "c2", origin.Add(3*time.Minute), headsChange("change:shared", "3", origin.Add(3*time.Minute)))
	if rows, current, headOnCurrent, _ = headsScopeState(t, p, key, "change:shared"); rows != 3 || current != 1 || !headOnCurrent {
		t.Fatalf("real change: rows=%d current=%d head on current=%v, want 3, 1, true", rows, current, headOnCurrent)
	}
	if total, items := headsPageTotal(t, p, scope, origin); total != 2 || items != 2 {
		t.Fatalf("history read counts %d and lists %d current events, want 2 and 2", total, items)
	}
	for i := 0; ; i++ {
		if _, _, _, complete = headsScopeState(t, p, key, "change:shared"); complete {
			break
		}
		if i > 10 {
			t.Fatal("the heads pass did not complete the scope")
		}
		if _, err := p.backfillEvidenceHeads(t.Context(), 512); err != nil {
			t.Fatal(err)
		}
	}
	oldWriter("4", origin.Add(4*time.Minute))
	if _, _, _, complete = headsScopeState(t, p, key, "change:shared"); complete {
		t.Fatal("a write without heads after the pass left the scope complete")
	}
	headsCommit(t, p, scope, "c3", origin.Add(5*time.Minute), headsChange("change:shared", "5", origin.Add(5*time.Minute)))
	if rows, current, headOnCurrent, _ = headsScopeState(t, p, key, "change:shared"); rows != 5 || current != 1 || !headOnCurrent {
		t.Fatalf("change after a second old write: rows=%d current=%d head on current=%v, want 5, 1, true", rows, current, headOnCurrent)
	}
}

// A scope row created by a writer that does not know about heads starts
// incomplete; the store marks the scopes it creates complete.
func TestPostgresScopeCreatedWithoutHeadsStartsIncomplete(t *testing.T) {
	p, scope := headsTestProvider(t)
	key, _ := evidenceScopeKey(scope)
	if _, err := p.db.Exec(`INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid,read_model_ready) VALUES($1,'default',$2,'physical',TRUE)`, key, scope.ClusterID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.db.Exec(`DELETE FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key) })
	var complete bool
	if err := p.db.QueryRow(`SELECT heads_backfill IS NULL FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key).Scan(&complete); err != nil || complete {
		t.Fatalf("a scope created without heads is complete: %v %v", complete, err)
	}
}

// Truncating the journal outside the store, then restoring an older version
// of an event (as a restore of an older dump does), drops every head and
// reopens every scope: an unchanged resave of the restored version appends
// nothing, a real change supersedes it, and the heads pass leaves the head on
// the current row, not on the newer version the truncate removed.
func TestPostgresJournalTruncateAndRestoreReopensTheScope(t *testing.T) {
	if os.Getenv("TEST_POSTGRES_DSN") == "" {
		t.Skip("isolated Postgres required")
	}
	// The truncate empties the whole journal, so it runs in a database of its own.
	snapshotOwnDatabase(t)
	p, scope := headsPartitionedProvider(t)
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	headsCommit(t, p, scope, "c0", origin, headsChange("change:restored", "1", origin))
	old := headsChange("change:restored", "1", origin)
	headsCommit(t, p, scope, "c1", origin.Add(time.Minute), headsChange("change:restored", "2", origin.Add(time.Minute)))
	if _, err := p.db.Exec(`TRUNCATE kubernetes_evidence_events`); err != nil {
		t.Fatal(err)
	}
	var heads int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM kubernetes_evidence_event_heads WHERE scope_key=$1`, key).Scan(&heads); err != nil || heads != 0 {
		t.Fatalf("truncate left %d heads (%v)", heads, err)
	}
	if _, err := p.db.Exec(`INSERT INTO kubernetes_evidence_events(scope_key,event_id,event_version,event_type,event_at,observed_at,namespace,resource_name,kind,authorization_kind,resource_uid,root_uid,epoch,payload)
 VALUES($1,$2,$3,'change',$4,$4,'shop',$2,'Deployment','Deployment',$2,$2,'stable',$5)`, key, old.ID, evidenceVersion(old), origin, []byte(old.Payload)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, complete := headsScopeState(t, p, key, old.ID); complete {
		t.Fatal("a truncate and restore outside the store left the scope complete")
	}
	headsCommit(t, p, scope, "c2", origin.Add(2*time.Minute), headsChange("change:restored", "1", origin.Add(2*time.Minute)))
	if rows, current, headOnCurrent, _ := headsScopeState(t, p, key, old.ID); rows != 1 || current != 1 || !headOnCurrent {
		t.Fatalf("unchanged resave of the restored version: rows=%d current=%d head on current=%v, want 1, 1, true", rows, current, headOnCurrent)
	}
	headsCommit(t, p, scope, "c3", origin.Add(3*time.Minute), headsChange("change:restored", "3", origin.Add(3*time.Minute)))
	for i := 0; ; i++ {
		if _, _, _, complete := headsScopeState(t, p, key, old.ID); complete {
			break
		}
		if i > 10 {
			t.Fatal("the heads pass did not complete the scope")
		}
		if _, err := p.backfillEvidenceHeads(t.Context(), 512); err != nil {
			t.Fatal(err)
		}
	}
	if rows, current, headOnCurrent, _ := headsScopeState(t, p, key, old.ID); rows != 2 || current != 1 || !headOnCurrent {
		t.Fatalf("real change after the restore: rows=%d current=%d head on current=%v, want 2, 1, true", rows, current, headOnCurrent)
	}
	if total, items := headsPageTotal(t, p, scope, origin); total != 1 || items != 1 {
		t.Fatalf("history read counts %d and lists %d versions, want 1 and 1", total, items)
	}
}

// An in-place update of a current row outside the store drops its head and
// reopens the scope, so the store compares the next save with the journal.
func TestPostgresUndeclaredInPlaceUpdateReopensTheScope(t *testing.T) {
	p, scope := headsPartitionedProvider(t)
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	headsCommit(t, p, scope, "c0", origin, headsChange("change:edited", "1", origin))
	if _, err := p.db.Exec(`UPDATE kubernetes_evidence_events SET event_version='edited' WHERE scope_key=$1 AND event_id='change:edited'`, key); err != nil {
		t.Fatal(err)
	}
	if rows, current, headOnCurrent, complete := headsScopeState(t, p, key, "change:edited"); complete || headOnCurrent || rows != 1 || current != 1 {
		t.Fatalf("after the edit: rows=%d current=%d head on current=%v complete=%v, want 1, 1, false, false", rows, current, headOnCurrent, complete)
	}
	headsCommit(t, p, scope, "c1", origin.Add(time.Minute), headsChange("change:edited", "2", origin.Add(time.Minute)))
	if rows, current, headOnCurrent, _ := headsScopeState(t, p, key, "change:edited"); rows != 2 || current != 1 || !headOnCurrent {
		t.Fatalf("real change after the edit: rows=%d current=%d head on current=%v, want 2, 1, true", rows, current, headOnCurrent)
	}
}

// A crash status refreshed in place while the journal is still being copied
// into monthly partitions reaches the copy, so after the swap the journal and
// a read by observation time show when it was last seen.
func TestPostgresCrashRefreshDuringThePartitionCopySurvivesTheSwap(t *testing.T) {
	if os.Getenv("TEST_POSTGRES_DSN") == "" {
		t.Skip("isolated Postgres required")
	}
	// The journal must start unpartitioned, so it runs in a database of its own.
	snapshotOwnDatabase(t)
	p, scope := headsTestProvider(t)
	key, _ := evidenceScopeKey(scope)
	var kind string
	if err := p.db.QueryRow(`SELECT relkind::text FROM pg_class WHERE oid='kubernetes_evidence_events'::regclass`).Scan(&kind); err != nil || kind != "r" {
		t.Fatalf("journal is %q before the copy (%v), want a plain table", kind, err)
	}
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	later := origin.Add(6 * time.Minute)
	headsCommit(t, p, scope, "c0", origin, headsStatus("crash:status", 7, origin))
	headsCommit(t, p, scope, "c1", later, headsStatus("crash:status", 7, later))
	type row struct {
		observed time.Time
		payload  string
		rows     int
	}
	read := func(table string) row {
		t.Helper()
		var r row
		if err := p.db.QueryRow(`SELECT MAX(observed_at),MAX(payload->>'observed_at'),COUNT(*) FROM `+table+` WHERE scope_key=$1`, key).Scan(&r.observed, &r.payload, &r.rows); err != nil {
			t.Fatal(err)
		}
		return r
	}
	same := func(a, b row) bool { return a.observed.Equal(b.observed) && a.payload == b.payload && a.rows == b.rows }
	journal, shadow := read("kubernetes_evidence_events"), read("kubernetes_evidence_events_partitioned")
	if journal.rows != 1 || !journal.observed.Equal(later) {
		t.Fatalf("the status was not refreshed in place: rows=%d observed=+%s", journal.rows, journal.observed.Sub(origin))
	}
	if !same(shadow, journal) {
		t.Fatalf("copy before the swap: %+v, journal: %+v", shadow, journal)
	}
	headsConvert(t, p)
	if after := read("kubernetes_evidence_events"); !same(after, journal) {
		t.Fatalf("journal after the swap: %+v, before: %+v", after, journal)
	}
	page, err := p.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "crash", ByObservation: true, Since: later.Add(-time.Minute), Until: later.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.Items[0].ObservedAt.Equal(later) {
		t.Fatalf("crashes observed around the refresh: %d items %+v", len(page.Items), page.Items)
	}
}
