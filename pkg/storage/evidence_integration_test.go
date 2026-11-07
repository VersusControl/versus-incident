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
	if _, err = p.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceCursorExpired) {
		t.Fatalf("pruned occurrence cursor survived: %v", err)
	}
}

func TestPostgresEvidencePartitionLifecyclePreservesDisabledRetention(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated harness Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn, EvidenceRetention: "0"})
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

func TestPostgresMonthDropInvalidatesOnlyScopesItPruned(t *testing.T) {
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
	// The test drives retention itself, so background maintenance cannot drop
	// a month between a commit and the cursor that pins it.
	p.maintenanceCancel()
	<-p.maintenanceDone
	settlePartitionCopy(t, p)
	now := time.Now().UTC()
	expiredAt := time.Date(now.Year(), now.Month()-3, 10, 12, 0, 0, 0, time.UTC)
	recentAt := now.Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
	run := fmt.Sprint(now.UnixNano())
	open := func(scope EvidenceScope, at time.Time) EvidenceQuery {
		t.Helper()
		checkpoint, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		previous := checkpoint.ID
		for i, minute := range []int{0, 6} {
			batch := evidenceTestBatch(scope.ClusterID+at.Format("150405")+fmt.Sprint(i), previous, at.Add(time.Duration(minute)*time.Minute), true, true)
			previous = batch.ID
			for n := 0; n < 3; n++ {
				issue := batch.Issues[0]
				issue.Key, issue.UID = fmt.Sprint(n), fmt.Sprint(n)
				batch.Issues = append(batch.Issues, issue)
			}
			if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
		}
		query := EvidenceQuery{Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Type: "occurrence", Limit: 1}
		page, err := p.QueryEvidence(t.Context(), scope, query)
		if err != nil || page.Next == "" {
			t.Fatalf("opening a cursor: %v", err)
		}
		query.Cursor = page.Next
		return query
	}
	pruned := EvidenceScope{OrgID: "default", ClusterID: "pruned-" + run, ClusterUID: "pruned"}
	kept := EvidenceScope{OrgID: "default", ClusterID: "kept-" + run, ClusterUID: "kept"}
	prunedQuery, keptQuery := open(pruned, expiredAt), open(kept, recentAt)
	before, err := p.QueryEvidence(t.Context(), kept, keptQuery)
	if err != nil {
		t.Fatal(err)
	}
	settle := func() {
		t.Helper()
		for i := 0; ; i++ {
			if i == 1000 {
				t.Fatal("retention never settled")
			}
			more, err := p.compactEvidenceBatch(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !more {
				return
			}
		}
	}
	settle()
	after, err := p.QueryEvidence(t.Context(), kept, keptQuery)
	if err != nil || !reflect.DeepEqual(before.Items, after.Items) {
		t.Fatalf("dropping another scope's month invalidated this cursor: %v", err)
	}
	if _, err = p.QueryEvidence(t.Context(), pruned, prunedQuery); !errors.Is(err, ErrEvidenceCursorExpired) {
		t.Fatalf("a cursor over dropped rows was still served: %v", err)
	}
	// Rows that arrive late in a month already pruned are dropped again, and
	// the scope's cursors over them are invalidated again.
	lateQuery := open(pruned, expiredAt.Add(time.Hour))
	settle()
	if _, err = p.QueryEvidence(t.Context(), pruned, lateQuery); !errors.Is(err, ErrEvidenceCursorExpired) {
		t.Fatalf("a cursor over late rows in a dropped month was still served: %v", err)
	}
	if still, err := p.QueryEvidence(t.Context(), kept, keptQuery); err != nil || !reflect.DeepEqual(before.Items, still.Items) {
		t.Fatalf("a second drop invalidated another scope's cursor: %v", err)
	}
}

// settlePartitionCopy finishes the move to the partitioned layout, which is
// the only layout retention drops months from.
func settlePartitionCopy(t *testing.T, p *postgresProvider) {
	t.Helper()
	for i := 0; ; i++ {
		if i == 64 {
			t.Fatal("partition copy never settled")
		}
		more, err := p.backfillEvidenceBatch(t.Context(), evidenceBackgroundBackfill)
		if err != nil {
			t.Fatal(err)
		}
		copyMore, err := p.partitionEvidenceBatch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more && !copyMore {
			return
		}
	}
}

// The probe that finds which scopes a dropped month held runs under the
// retention statement timeout, so it must probe an index per scope rather
// than read the month. EVIDENCE_PROBE_SCALE=1 fills the month with a million
// rows first.
func TestPostgresMonthScopeProbeUsesTheScopeIndex(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated Postgres required")
	}
	scale := os.Getenv("EVIDENCE_PROBE_SCALE") == "1"
	if scale && testing.Short() {
		t.Skip("million-row month is not a short test")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn, EvidenceRetention: "720h"})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	p := provider.(*postgresProvider)
	p.maintenanceCancel()
	<-p.maintenanceDone
	settlePartitionCopy(t, p)
	now := time.Now().UTC()
	at := time.Date(now.Year(), now.Month()-4, 3, 12, 0, 0, 0, time.UTC)
	scope := EvidenceScope{OrgID: "default", ClusterID: "probe-" + fmt.Sprint(now.UnixNano()), ClusterUID: "probe"}
	// A store with retention never writes into a month retention has passed,
	// so the month is seeded the way a deployment that enables retention later
	// holds it: through a store without retention.
	seed, err := NewPostgres(PostgresOptions{DSN: dsn, EvidenceRetention: "0"})
	if err != nil {
		t.Fatal(err)
	}
	err = seed.(EvidenceStore).CommitEvidence(t.Context(), scope, evidenceTestBatch("probe", "", at, true, true))
	_ = seed.Close()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := evidenceScopeKey(scope)
	partition := "evidence_event_month_" + at.Format("200601")
	if scale {
		started := time.Now()
		if _, err = p.db.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_events(scope_key,event_id,event_version,event_type,event_at,observed_at,epoch,payload)
		 SELECT $1,'scale-'||g,'v','change',$2::timestamptz+g*INTERVAL '1 millisecond',$2::timestamptz,'scale','{}'::jsonb FROM generate_series(1,1000000) g`, key, at); err != nil {
			t.Fatal(err)
		}
		t.Logf("seeded a million rows in %s in %s", partition, time.Since(started).Round(time.Second))
	}
	tx, err := p.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), `SET LOCAL statement_timeout='2s'`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), evidenceScopesProbeSettings); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = tx.QueryRowContext(t.Context(), "EXPLAIN (FORMAT JSON) "+evidenceScopesProbe(partition)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err = json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("plan: %v", err)
	}
	probed := false
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == partition {
			switch node["Node Type"] {
			case "Index Scan", "Index Only Scan":
				probed = true
			default:
				t.Fatalf("the month probe reads %s by %v, not an index probe: %s", partition, node["Node Type"], raw)
			}
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if next, ok := child.(map[string]any); ok {
				walk(next)
			}
		}
	}
	walk(plans[0].Plan)
	if !probed {
		t.Fatalf("the month probe never reached %s: %s", partition, raw)
	}
	started := time.Now()
	scopes, err := evidenceScopesIn(t.Context(), tx, partition)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("month probe: %v", err)
	}
	found := false
	for _, candidate := range scopes {
		found = found || candidate == key
	}
	if !found || elapsed > 500*time.Millisecond {
		t.Fatalf("month probe found=%t in %s", found, elapsed)
	}
	t.Logf("month probe over %s took %s", partition, elapsed)
}

// longOpenIssueCapture saves one capture of issues that stay open, with the
// given change records, as the capture layer does every heartbeat.
func longOpenIssueCapture(t *testing.T, store EvidenceStore, scope EvidenceScope, id string, at time.Time, issues int, records []EvidenceRecord) {
	t.Helper()
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	batch := EvidenceBatch{ID: id, Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at, CoveredThrough: at, Epoch: "paging", Records: records}
	for i := 0; i < issues; i++ {
		issue := pagingIssue(fmt.Sprintf("pod-%d", i), "shop", at)
		issue.Broken = true
		batch.Issues = append(batch.Issues, issue)
	}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatalf("commit %s: %v", id, err)
	}
}

// seedLongOpenIssues saves issues opened at opened and sampled every five
// minutes for sampled, then recent captures with changes, through a store
// without retention: retention never saves a sample it would already thin.
func seedLongOpenIssues(t *testing.T, dsn string, scope EvidenceScope, opened time.Time, sampled time.Duration, issues int, now time.Time) {
	t.Helper()
	seed := worstCaseProvider(t, dsn, PostgresOptions{EvidenceRetention: "0"})
	n := 0
	for at := opened; !at.After(opened.Add(sampled)); at = at.Add(5 * time.Minute) {
		longOpenIssueCapture(t, seed, scope, fmt.Sprintf("old-%d", n), at, issues, nil)
		n++
	}
	for i := 0; i < 6; i++ {
		at := now.Add(-time.Duration(60-i*5) * time.Minute)
		changes := []EvidenceRecord{pagingChange(fmt.Sprintf("change-%d-a", i), "shop", at, at, "a"), pagingChange(fmt.Sprintf("change-%d-b", i), "shop", at, at, "b")}
		longOpenIssueCapture(t, seed, scope, fmt.Sprintf("recent-%d", i), at, issues, changes)
	}
}

// drainEvidencePages reads the pages after a cursor and returns every row
// id, failing on a repeated row.
func drainEvidencePages(t *testing.T, store EvidenceStore, scope EvidenceScope, query EvidenceQuery, seen []string) []string {
	t.Helper()
	ids := map[string]bool{}
	for _, id := range seen {
		ids[id] = true
	}
	for query.Cursor != "" {
		page, err := store.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatalf("%s page after %d rows: %v", query.Type, len(seen), err)
		}
		for _, item := range page.Items {
			if ids[item.ID] {
				t.Fatalf("%s row %s repeated", query.Type, item.ID)
			}
			ids[item.ID] = true
			seen = append(seen, item.ID)
		}
		query.Cursor = page.Next
	}
	return seen
}

func evidencePageIDs(page EvidencePage) []string {
	ids := []string{}
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// thinUntilSettled runs retention passes until there is nothing left to
// remove and returns the scope's checkpoint.
func thinUntilSettled(t *testing.T, p *postgresProvider, scope EvidenceScope) EvidenceCheckpoint {
	t.Helper()
	for i := 0; ; i++ {
		if i == 1000 {
			t.Fatal("retention never settled")
		}
		more, err := p.compactEvidenceBatch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	checkpoint, err := p.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

// Thinning removes only issue samples. A Changes, Timeline or break list never
// reads them, and an issue list reads them only near the end of its window, so
// each keeps its page links unless the samples it reads were thinned. The rows
// read across the thinning match a fresh read, none skipped or repeated.
func TestPostgresThinningKeepsPageLinksOfListsItCannotChange(t *testing.T) {
	dsn := worstCaseDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := EvidenceScope{OrgID: "default", ClusterID: "long-open", ClusterUID: "long-open"}
	opened := now.Add(-8 * 24 * time.Hour)
	seedLongOpenIssues(t, dsn, scope, opened, 2*time.Hour, 3, now)
	p := worstCaseProvider(t, dsn, PostgresOptions{EvidenceRetention: "168h"})

	lists := []EvidenceQuery{
		{Type: "change", Since: now.Add(-2 * time.Hour), Until: now, Limit: 2},
		{Type: "timeline", GapsApart: true, Since: now.Add(-9 * 24 * time.Hour), Until: now, Limit: 2},
		{Type: "break", GapsApart: true, Since: now.Add(-9 * 24 * time.Hour), Until: now, Limit: 1},
		{Type: "occurrence", Since: now.Add(-9 * 24 * time.Hour), Until: now, Limit: 1},
	}
	firsts := make([]EvidencePage, len(lists))
	for i, query := range lists {
		page, err := p.QueryEvidence(t.Context(), scope, query)
		if err != nil || page.Next == "" {
			t.Fatalf("%s first page: next=%q err=%v", query.Type, page.Next, err)
		}
		firsts[i] = page
	}
	// An issue list that ends inside the thinned hours reads the samples
	// thinning removes.
	historic := EvidenceQuery{Type: "occurrence", Since: opened.Add(-time.Hour), Until: opened.Add(90 * time.Minute), Limit: 1}
	historicPage, err := p.QueryEvidence(t.Context(), scope, historic)
	if err != nil || historicPage.Next == "" {
		t.Fatalf("historic issue page: next=%q err=%v", historicPage.Next, err)
	}

	before, err := p.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	after := thinUntilSettled(t, p, scope)

	for i, query := range lists {
		next := query
		next.Cursor = firsts[i].Next
		got := drainEvidencePages(t, p, scope, next, evidencePageIDs(firsts[i]))
		fresh := query
		fresh.Limit = 500
		want, err := p.QueryEvidence(t.Context(), scope, fresh)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, evidencePageIDs(want)) {
			t.Fatalf("%s rows across thinning = %v, want %v", query.Type, got, evidencePageIDs(want))
		}
	}
	historic.Cursor = historicPage.Next
	if _, err = p.QueryEvidence(t.Context(), scope, historic); !errors.Is(err, ErrEvidenceCursorExpired) {
		t.Fatalf("issue page over thinned samples: %v, want the list restarted from its first page", err)
	}
	if after.ThinRevision == before.ThinRevision || after.PruneRevision != before.PruneRevision || !after.PrunedThrough.Equal(before.PrunedThrough) {
		t.Fatalf("thinning recorded thin %d->%d prune %d->%d pruned through %s->%s; want samples thinned and nothing pruned",
			before.ThinRevision, after.ThinRevision, before.PruneRevision, after.PruneRevision, before.PrunedThrough, after.PrunedThrough)
	}
}

// A steady cluster under the default retention, with issues open longer than
// it, keeps its page links while maintenance runs and captures keep arriving.
func TestPostgresDefaultRetentionKeepsPageLinksOfASteadyCluster(t *testing.T) {
	t.Setenv("KUBERNETES_EVIDENCE_RETENTION", "")
	dsn := worstCaseDatabase(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := EvidenceScope{OrgID: "default", ClusterID: "steady", ClusterUID: "steady"}
	// Enough old samples that retention thins them over several passes.
	seedLongOpenIssues(t, dsn, scope, now.Add(-10*24*time.Hour), 12*time.Hour, 3, now)
	provider, err := NewPostgres(PostgresOptions{DSN: dsn, ObservationHeartbeat: "5m"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	p := provider.(*postgresProvider)
	p.maintenanceCancel()
	<-p.maintenanceDone
	settlePartitionCopy(t, p)
	if p.evidenceRetention != 7*24*time.Hour {
		t.Fatalf("default retention = %s, want 168h", p.evidenceRetention)
	}

	lists := []EvidenceQuery{
		{Type: "change", Since: now.Add(-2 * time.Hour), Until: now, Limit: 2},
		{Type: "timeline", GapsApart: true, Since: now.Add(-2 * time.Hour), Until: now, Limit: 2},
		{Type: "occurrence", Since: now.Add(-11 * 24 * time.Hour), Until: now, Limit: 1},
	}
	cursors := make([]EvidenceQuery, len(lists))
	seen := make([][]string, len(lists))
	for i, query := range lists {
		page, err := p.QueryEvidence(t.Context(), scope, query)
		if err != nil || page.Next == "" {
			t.Fatalf("%s first page: next=%q err=%v", query.Type, page.Next, err)
		}
		cursors[i], seen[i] = query, evidencePageIDs(page)
		cursors[i].Cursor = page.Next
	}
	start, err := p.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	thinned := 0
	for pass := 0; pass < 4; pass++ {
		more, err := p.compactEvidenceBatch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if more {
			thinned++
		}
		longOpenIssueCapture(t, p, scope, fmt.Sprintf("steady-%d", pass), now.Add(time.Duration(pass*5-30)*time.Minute), 3, nil)
		for i := range cursors {
			if cursors[i].Cursor == "" {
				continue
			}
			page, err := p.QueryEvidence(t.Context(), scope, cursors[i])
			if err != nil {
				t.Fatalf("pass %d: %s next page: %v", pass, cursors[i].Type, err)
			}
			seen[i] = append(seen[i], evidencePageIDs(page)...)
			cursors[i].Cursor = page.Next
		}
	}
	end, err := p.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if thinned < 2 || end.ThinRevision < start.ThinRevision+2 || end.PruneRevision != start.PruneRevision {
		t.Fatalf("maintenance thinned in %d passes (thin %d->%d, prune %d->%d), want several thinning passes and no pruning",
			thinned, start.ThinRevision, end.ThinRevision, start.PruneRevision, end.PruneRevision)
	}
	for i := range cursors {
		got := drainEvidencePages(t, p, scope, cursors[i], seen[i])
		if len(got) < 2 {
			t.Fatalf("%s read %d rows", lists[i].Type, len(got))
		}
	}
}
