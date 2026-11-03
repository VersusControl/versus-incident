package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestPostgresSingleConnectionMigrationAndFirstWriteCutover(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated Postgres required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	p := &postgresProvider{db: db}
	done := make(chan error, 1)
	go func() { done <- p.migrate() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("pool-one migration deadlocked")
	}
	if db.Stats().MaxOpenConnections != 1 {
		t.Fatal("migration changed runtime pool budget")
	}
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errorsOut := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 10; i++ {
			_, err := p.partitionEvidenceBatch(ctx)
			if err != nil {
				var state interface{ SQLState() string }
				if errors.As(err, &state) && state.SQLState() == "55P03" {
					continue
				}
				errorsOut <- err
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		at := time.Now().UTC()
		scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprint(at.UnixNano()), ClusterUID: "cutover"}
		errorsOut <- p.CommitEvidence(ctx, scope, evidenceTestBatch("first", "", at, true, true))
	}()
	workers.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatalf("first write/cutover race: %v", err)
		}
	}
	var markerIndex bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_index WHERE indrelid=to_regclass('kubernetes_evidence_events') AND indisvalid AND pg_get_expr(indpred,indrelid) LIKE '%termination%')`).Scan(&markerIndex); err != nil || !markerIndex {
		t.Fatalf("online termination index did not survive parent cutover: exists=%v err=%v", markerIndex, err)
	}
}

func TestPostgresRetainedSamplesAndCursorOrderingRemainExact(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn, EvidenceRetention: "720h"})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	p := provider.(*postgresProvider)
	at := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
	scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprint(time.Now().UnixNano()), ClusterUID: "pins"}
	previous := ""
	for i, minute := range []int{0, 6, 12} {
		batch := evidenceTestBatch(fmt.Sprint(i), previous, at.Add(time.Duration(minute)*time.Minute), true, true)
		previous = batch.ID
		for n := 0; n < 3; n++ {
			issue := batch.Issues[0]
			issue.Key = fmt.Sprint(n)
			issue.UID = fmt.Sprint(n)
			batch.Issues = append(batch.Issues, issue)
		}
		if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
	}
	query := EvidenceQuery{Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Type: "occurrence", Limit: 1}
	page, err := p.QueryEvidence(t.Context(), scope, query)
	if err != nil || page.Next == "" {
		t.Fatal(err)
	}
	query.Cursor = page.Next
	before, err := p.QueryEvidence(t.Context(), scope, query)
	if err != nil {
		t.Fatal(err)
	}
	last := evidenceTestBatch("latest", previous, at.Add(14*time.Minute), true, true)
	for n := 0; n < 3; n++ {
		issue := last.Issues[0]
		issue.Key = fmt.Sprint(n)
		issue.UID = fmt.Sprint(n)
		last.Issues = append(last.Issues, issue)
	}
	if err = p.CommitEvidence(t.Context(), scope, last); err != nil {
		t.Fatal(err)
	}
	after, err := p.QueryEvidence(t.Context(), scope, query)
	if err != nil || !reflect.DeepEqual(before.Items, after.Items) {
		t.Fatalf("pinned sample regressed: %v", err)
	}
	if _, err = p.compactEvidenceBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	still, err := p.QueryEvidence(t.Context(), scope, query)
	if err != nil || !reflect.DeepEqual(before.Items, still.Items) {
		t.Fatalf("retention compacted live pins: %v", err)
	}
	key, _ := evidenceScopeKey(scope)
	if _, err = p.db.Exec(`UPDATE kubernetes_evidence_scopes SET read_model_ready=FALSE WHERE scope_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	for _, pending := range []EvidenceQuery{{Since: query.Since, Until: query.Until, Type: "occurrence", Limit: 1}, query, {Since: query.Since, Until: query.Until, Type: "issue", Limit: 1}} {
		if _, err = p.QueryEvidence(t.Context(), scope, pending); !errors.Is(err, ErrEvidenceNotReady) {
			t.Fatalf("converting scope served %q: %v", pending.Type, err)
		}
	}
	if _, err = p.db.Exec(`UPDATE kubernetes_evidence_scopes SET read_model_ready=TRUE WHERE scope_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	resumed, err := p.QueryEvidence(t.Context(), scope, query)
	if err != nil || !reflect.DeepEqual(before.Items, resumed.Items) {
		t.Fatalf("cursor did not resume after conversion: %v", err)
	}
	if _, err = p.db.Exec(`UPDATE kubernetes_evidence_scopes SET checkpoint=jsonb_set(checkpoint,'{prune_revision}','99') WHERE scope_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err = p.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("pruned occurrence cursor survived: %v", err)
	}
}

func TestPostgresEvidencePartitionLifecyclePreservesDisabledRetention(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated harness Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	p := provider.(*postgresProvider)
	p.maintenanceCancel()
	<-p.maintenanceDone
	at := time.Now().UTC().Truncate(time.Microsecond).AddDate(0, -2, 0)
	scope := EvidenceScope{OrgID: "partition-lifecycle", ClusterID: fmt.Sprint(time.Now().UnixNano()), ClusterUID: "physical"}
	if err = p.CommitEvidence(t.Context(), scope, evidenceTestBatch("old", "", at, true, true)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		more, err := p.backfillEvidenceBatch(t.Context(), evidenceBackgroundBackfill)
		if err != nil {
			t.Fatal(err)
		}
		copyMore, err := p.partitionEvidenceBatch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more && !copyMore {
			break
		}
	}
	var complete bool
	if err = p.db.QueryRow(`SELECT complete FROM kubernetes_evidence_partition_migration WHERE singleton`).Scan(&complete); err != nil || !complete {
		t.Fatalf("copy not complete: %t %v", complete, err)
	}
	for _, name := range []string{"kubernetes_evidence_events", "kubernetes_evidence_occurrence_observations"} {
		var kind string
		if err = p.db.QueryRow(`SELECT relkind::text FROM pg_class WHERE oid=$1::regclass`, name).Scan(&kind); err != nil || kind != "p" {
			t.Fatalf("not partitioned: %s %s %v", name, kind, err)
		}
	}
	if more, err := p.compactEvidenceBatch(t.Context()); err != nil || more {
		t.Fatalf("disabled retention performed maintenance: %t %v", more, err)
	}
	page, err := p.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: at.Add(-time.Minute), Until: time.Now().UTC(), Type: "issue"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("upgrade expired old evidence: %+v %v", page, err)
	}
	if err = p.DeleteEvidence(t.Context(), scope); err != nil {
		t.Fatalf("explicit deletion including rollback originals: %v", err)
	}
	page, err = p.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: at.Add(-time.Minute), Until: time.Now().UTC(), Type: "issue"})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("explicit delete retained live evidence: %+v %v", page, err)
	}
}

func seedScaleOccurrences(t *testing.T, p *postgresProvider, scope EvidenceScope, at time.Time, tier string) string {
	t.Helper()
	corpora := map[string]struct{ occurrences, observations int }{"S": {100, 10000}, "M": {2000, 1000000}, "L": {20000, 10000000}}
	corpus := corpora[tier]
	origin := at.Add(-23 * time.Hour)
	for offset := 0; offset < corpus.occurrences; offset += 1000 {
		cp, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		batch := EvidenceBatch{ID: fmt.Sprintf("roots-%d", offset), Expected: cp.ID, Generation: cp.Generation, At: origin.Add(time.Duration(offset/1000) * time.Second), Epoch: "synthetic"}
		for i := offset; i < min(offset+1000, corpus.occurrences); i++ {
			uid := fmt.Sprintf("root-%d", i)
			raw, _ := json.Marshal(map[string]any{"root": map[string]string{"uid": uid, "kind": "Pod", "namespace": "shop"}, "rule": "pod.crash_loop", "severity": "warning", "count": 1, "broke_at": origin})
			batch.Issues = append(batch.Issues, EvidenceIssue{Key: uid, UID: uid, Kind: "Pod", Namespace: "shop", Rule: "pod.crash_loop", AuthorizationKind: "Pod", Broken: true, FirstSeen: origin, LastSeen: batch.At, Payload: raw})
		}
		if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := evidenceScopeKey(scope)
	if _, err := p.db.ExecContext(t.Context(), `SELECT versus_ensure_evidence_partition('kubernetes_evidence_occurrence_observations',$1),versus_ensure_evidence_partition('kubernetes_evidence_occurrence_observations',$2)`, origin, at); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecContext(t.Context(), `WITH models AS MATERIALIZED(
 SELECT occurrence_id,payload_version,row_number() OVER(ORDER BY occurrence_id) AS n FROM kubernetes_evidence_issues WHERE scope_key=$1)
 INSERT INTO kubernetes_evidence_occurrence_observations(scope_key,occurrence_id,revision,observed_at,last_seen,payload_version)
 SELECT $1,m.occurrence_id,1000+(g.n/$3::bigint),$2::timestamptz+(g.n/$3::bigint)*INTERVAL '2 minutes',$2::timestamptz+(g.n/$3::bigint)*INTERVAL '2 minutes',m.payload_version
 FROM generate_series(0,$4::bigint-1)g(n) JOIN models m ON m.n=(g.n%$3::bigint)+1 ON CONFLICT DO NOTHING`, key, origin, corpus.occurrences, corpus.observations); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_occurrence_observations(scope_key,occurrence_id,revision,observed_at,last_seen,payload_version)
 SELECT scope_key,occurrence_id,2000,$2,$2,payload_version FROM kubernetes_evidence_issues WHERE scope_key=$1 ON CONFLICT DO NOTHING`, key, at); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecContext(t.Context(), `UPDATE kubernetes_evidence_issues SET last_seen=$2,payload=jsonb_set(payload,'{last_seen}',to_jsonb($2::timestamptz)) WHERE scope_key=$1`, key, at); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecContext(t.Context(), `UPDATE kubernetes_evidence_scopes SET checkpoint=jsonb_set(jsonb_set(checkpoint,'{occurrence_revision}','2000'),'{at}',to_jsonb($2::timestamptz)) WHERE scope_key=$1`, key, at); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := p.db.QueryRowContext(t.Context(), `SELECT occurrence_id FROM kubernetes_evidence_issues WHERE scope_key=$1 AND root_uid='root-0'`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPostgresEvidenceScaleReadModels(t *testing.T) {
	tier := os.Getenv("EVIDENCE_SCALE_TIER")
	tiers := map[string]int{"S": 10000, "M": 1000000, "L": 20000000}
	count, ok := tiers[tier]
	if !ok {
		t.Skip("set EVIDENCE_SCALE_TIER=S, M or L in an isolated harness run")
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated harness Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	p := provider.(*postgresProvider)
	at := time.Now().UTC().Truncate(time.Microsecond)
	scope := EvidenceScope{OrgID: "scale", ClusterID: fmt.Sprint(time.Now().UnixNano()), ClusterUID: "physical"}
	if err = p.CommitEvidence(t.Context(), scope, EvidenceBatch{ID: "scope", At: at.Add(-24 * time.Hour), Epoch: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	occurrenceID := seedScaleOccurrences(t, p, scope, at, tier)
	key, _ := evidenceScopeKey(scope)
	// Generate the retained journal in SQL so corpus size does not consume Go
	// request memory. This is a deliberately opt-in performance fixture.
	if _, err = p.db.ExecContext(t.Context(), `SELECT versus_ensure_evidence_partition('kubernetes_evidence_events',$1),versus_ensure_evidence_partition('kubernetes_evidence_events',$2)`, at.Add(-24*time.Hour), at); err != nil {
		t.Fatal(err)
	}
	if _, err = p.db.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_events(scope_key,event_id,event_version,event_type,event_at,observed_at,namespace,kind,authorization_kind,resource_uid,root_uid,rule,epoch,payload)
 SELECT $1,'event-'||n,'immutable','change',$2::timestamptz-(n%1440)*INTERVAL '1 minute',$2,
 CASE WHEN n%2=0 THEN 'shop' ELSE 'ops' END,'Pod','Pod','pod-'||(n%20000),'root-'||(n%20000),'changed','synthetic','{}'::jsonb FROM generate_series(1,$3::bigint)n`, key, at, count); err != nil {
		t.Fatal(err)
	}
	if _, err = p.db.ExecContext(t.Context(), `UPDATE kubernetes_evidence_scopes SET high_sequence=(SELECT MAX(sequence) FROM kubernetes_evidence_events WHERE scope_key=$1),checkpoint=jsonb_set(checkpoint,'{at}',to_jsonb($2::timestamptz)) WHERE scope_key=$1`, key, at); err != nil {
		t.Fatal(err)
	}
	for _, query := range []EvidenceQuery{
		{Type: "timeline"}, {Type: "change", Namespace: "shop"}, {Type: "change", RootUID: "root-12"}, {Type: "change", Kind: "Pod"},
	} {
		query.Since, query.Until, query.Limit = at.Add(-24*time.Hour), at, 50
		query.EnforceAuthority, query.AllowedKinds = true, []string{"Pod"}
		start := time.Now()
		page, err := p.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, query := range []EvidenceQuery{
			{Type: "occurrence", Limit: 50},
			{Type: "occurrence", Lookup: true, OccurrenceID: occurrenceID, Limit: 1},
			{Type: "occurrence", Lookup: true, RootUID: "root-0", Limit: 1},
			{Type: "occurrence", Lookup: true, RootUID: "root-0", Center: at.Add(-23 * time.Hour), Limit: 2},
		} {
			query.Since, query.Until = at.Add(-24*time.Hour), at
			query.EnforceAuthority, query.AllowedKinds = true, []string{"Pod"}
			start := time.Now()
			page, err := p.QueryEvidence(t.Context(), scope, query)
			if err != nil || len(page.Items) == 0 {
				t.Fatalf("tier=%s occurrence=%+v err=%v", tier, query, err)
			}
			t.Logf("tier=%s occurrence_lookup=%t root=%s center=%t elapsed=%s", tier, query.Lookup, query.RootUID, !query.Center.IsZero(), time.Since(start))
		}
		if len(page.Items) > 50 || page.Total > 1001 {
			t.Fatal("unbounded read model page")
		}
		t.Logf("tier=%s type=%s namespace=%s root=%s first_page=%s capped=%t", tier, query.Type, query.Namespace, query.RootUID, time.Since(start), page.CountCapped)
		if page.Next != "" {
			query.Cursor = page.Next
			start = time.Now()
			if _, err = p.QueryEvidence(t.Context(), scope, query); err != nil {
				t.Fatal(err)
			}
			t.Logf("tier=%s next_page=%s", tier, time.Since(start))
		}
	}
	var plan []byte
	if err = p.db.QueryRowContext(t.Context(), `EXPLAIN(ANALYZE,BUFFERS,FORMAT JSON) SELECT event_id FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_at>=$2 AND event_at<=$3 AND superseded_seq IS NULL ORDER BY event_at DESC,event_id DESC LIMIT 51`, key, at.Add(-24*time.Hour), at).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	t.Logf("tier=%s event_plan=%s", tier, plan)
}

func TestPostgresEvidenceTransactionsFencesAndRestartIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("explicit harness Postgres required")
	}
	open := func() Provider {
		p, err := NewPostgres(PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal("isolated Postgres initialization failed")
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	first, second := open(), open()
	store := first.(EvidenceStore)
	db := first.(SQLAccessor).DB()
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('kubernetes_evidence_scopes','kubernetes_evidence_captures','kubernetes_evidence_events','kubernetes_evidence_issues')").Scan(&tables); err != nil || tables != 4 {
		t.Fatalf("schema tables=%d err=%v", tables, err)
	}
	var indexes int
	if err := db.QueryRow("SELECT count(*) FROM pg_indexes WHERE tablename='kubernetes_evidence_events'").Scan(&indexes); err != nil || indexes < 10 {
		t.Fatalf("indexed schema indexes=%d err=%v", indexes, err)
	}
	scope := EvidenceScope{OrgID: "synthetic-atomic", ClusterID: fmt.Sprintf("cluster-%d", time.Now().UnixNano()), ClusterUID: "physical-a"}
	origin := time.Now().UTC().Add(-time.Hour)
	initial := evidenceTestBatch("initial", "", origin, true, true)
	if err := store.CommitEvidence(t.Context(), scope, initial); err != nil {
		t.Fatal(err)
	}
	before, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour), Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	// Raise a real database write error after the transaction has begun. This
	// proves rollback for SQLSTATE disk-full, not physical volume exhaustion.
	_, err = db.Exec(`CREATE OR REPLACE FUNCTION synthetic_evidence_write_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.payload->>'synthetic_disk_full'='true' THEN RAISE EXCEPTION 'synthetic capacity fault' USING ERRCODE='53100'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER synthetic_evidence_write_fault BEFORE INSERT ON kubernetes_evidence_events FOR EACH ROW EXECUTE FUNCTION synthetic_evidence_write_fault()`)
	if err != nil {
		t.Fatal(err)
	}
	fault := evidenceTestBatch("fault", "initial", origin.Add(time.Minute), false, true)
	fault.Records = []EvidenceRecord{{ID: "synthetic-capacity-record", Type: "change", At: fault.At, ObservedAt: fault.At, Epoch: fault.Epoch, Payload: json.RawMessage(`{"synthetic_disk_full":true}`)}}
	if err = store.CommitEvidence(t.Context(), scope, fault); err == nil {
		t.Fatal("server storage fault accepted")
	}
	after, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour), Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if after.Checkpoint.ID != "initial" || len(after.Items) != len(before.Items) {
		t.Fatal("failed transaction changed checkpoint or history")
	}
	var closed, receipts int
	key, _ := evidenceScopeKey(scope)
	if err = db.QueryRow("SELECT count(*) FROM kubernetes_evidence_issues WHERE scope_key=$1 AND closed_at IS NOT NULL", key).Scan(&closed); err != nil || closed != 0 {
		t.Fatal("failed transaction falsely closed issue")
	}
	if err = db.QueryRow("SELECT count(*) FROM kubernetes_evidence_captures WHERE scope_key=$1 AND capture_id='fault'", key).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("failed transaction retained receipt")
	}
	results := make(chan error, 16)
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			p := first
			if i%2 == 1 {
				p = second
			}
			results <- p.(EvidenceStore).CommitEvidence(t.Context(), scope, evidenceTestBatch(fmt.Sprintf("contender-%d", i), "initial", origin.Add(2*time.Minute), true, true))
		}(i)
	}
	wait.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result == nil {
			winners++
		} else if !errors.Is(result, ErrEvidenceConflict) {
			t.Fatal(result)
		}
	}
	if winners != 1 {
		t.Fatalf("transaction winners=%d", winners)
	}
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	guardKey := "kubernetes/inventory/synthetic-atomic-guard"
	oldGuard, newGuard := []byte(`{"owner":"old"}`), []byte(`{"owner":"new"}`)
	if err = first.WriteBlob(guardKey, oldGuard); err != nil {
		t.Fatal(err)
	}
	won, err := second.(BlobCAS).CompareAndSwapBlob(guardKey, oldGuard, newGuard)
	if err != nil || !won {
		t.Fatal("takeover CAS failed")
	}
	retired := evidenceTestBatch("retired", checkpoint.ID, origin.Add(3*time.Minute), false, true)
	retired.GuardKey, retired.GuardExpected = guardKey, oldGuard
	if err = store.CommitEvidence(t.Context(), scope, retired); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("retired writer not fenced: %v", err)
	}
	retired.ID = "successor"
	retired.GuardExpected = newGuard
	if err = second.(EvidenceStore).CommitEvidence(t.Context(), scope, retired); err != nil {
		t.Fatal(err)
	}
	if err = second.(EvidenceStore).CommitEvidence(t.Context(), scope, retired); err != nil {
		t.Fatal("duplicate successor not idempotent")
	}
	reopened := open().(EvidenceStore)
	restored, err := reopened.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour), Limit: 500})
	if err != nil || restored.Checkpoint.ID != "successor" {
		t.Fatal("restart did not restore committed journal")
	}
	for _, foreign := range []EvidenceScope{{OrgID: "foreign", ClusterID: scope.ClusterID, ClusterUID: scope.ClusterUID}, {OrgID: scope.OrgID, ClusterID: "foreign", ClusterUID: scope.ClusterUID}, {OrgID: scope.OrgID, ClusterID: scope.ClusterID, ClusterUID: "physical-b"}} {
		page, err := reopened.QueryEvidence(t.Context(), foreign, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour)})
		if err != nil || len(page.Items) != 0 {
			t.Fatal("foreign scope read evidence")
		}
	}
	narrow, err := reopened.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour), Namespace: "forbidden"})
	if err != nil || len(narrow.Items) != 0 {
		t.Fatal("namespace filter leaked evidence")
	}
	t.Logf("real Postgres: schema=%d tables indexes=%d; rollback preserved %d records; CAS winners=%d/16; fenced retired writer; reopened checkpoint=%s records=%d; four foreign scopes denied", tables, indexes, len(before.Items), winners, restored.Checkpoint.ID, len(restored.Items))
}

func TestPostgresStartupConversionRebuildsReadModels(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	p := provider.(*postgresProvider)
	p.maintenanceCancel()
	<-p.maintenanceDone
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprint(time.Now().UnixNano()), ClusterUID: "startup-conversion"}
	previous := ""
	for i := 0; i < 3; i++ {
		batch := evidenceTestBatch(fmt.Sprint(i), previous, at.Add(time.Duration(i)*time.Minute), true, true)
		previous = batch.ID
		if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
	}
	queries := []EvidenceQuery{
		{Since: at.Add(-time.Hour), Until: time.Now().UTC(), Type: "occurrence", Limit: 50},
		{Since: at.Add(-time.Hour), Until: time.Now().UTC(), Type: "issue", Limit: 50},
	}
	want := make([]EvidencePage, len(queries))
	for i, query := range queries {
		if want[i], err = p.QueryEvidence(t.Context(), scope, query); err != nil || len(want[i].Items) == 0 {
			t.Fatalf("baseline %s: %d %v", query.Type, len(want[i].Items), err)
		}
	}
	key, _ := evidenceScopeKey(scope)
	for _, statement := range []string{
		`UPDATE kubernetes_evidence_scopes SET read_model_ready=FALSE,backfill_sequence=0,backfill_occurrence='',backfill_version='',event_counts='{}' WHERE scope_key=$1`,
		`UPDATE kubernetes_evidence_issues SET model_ready=FALSE WHERE scope_key=$1`,
		`UPDATE kubernetes_evidence_events SET superseded_seq=NULL WHERE scope_key=$1`,
		`DELETE FROM kubernetes_evidence_occurrence_read_versions WHERE scope_key=$1`,
	} {
		if _, err = p.db.Exec(statement, key); err != nil {
			t.Fatal(err)
		}
	}

	// A capture commit holding the scope row must not read as finished work.
	lock, err := p.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lock.Exec(`SELECT 1 FROM kubernetes_evidence_scopes WHERE scope_key=$1 FOR UPDATE`, key); err != nil {
		t.Fatal(err)
	}
	if more, err := p.backfillEvidenceBatch(t.Context(), evidenceBackgroundBackfill); err != nil || !more {
		t.Fatalf("locked pending scope reported done: more=%t err=%v", more, err)
	}
	if err = lock.Rollback(); err != nil {
		t.Fatal(err)
	}

	if err = p.completeEvidenceBackfill(t.Context(), time.Minute); err != nil {
		t.Fatal(err)
	}
	var ready bool
	if err = p.db.QueryRow(`SELECT read_model_ready FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key).Scan(&ready); err != nil || !ready {
		t.Fatalf("scope not converted: %t %v", ready, err)
	}
	for i, query := range queries {
		got, err := p.QueryEvidence(t.Context(), scope, query)
		if err != nil || !reflect.DeepEqual(want[i].Items, got.Items) || want[i].Total != got.Total {
			t.Fatalf("converted %s page differs: %v", query.Type, err)
		}
	}
	if err = p.completeEvidenceBackfill(t.Context(), time.Minute); err != nil {
		t.Fatalf("converted deployment repeated conversion: %v", err)
	}
}
