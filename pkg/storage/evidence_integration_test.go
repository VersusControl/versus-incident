package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

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
