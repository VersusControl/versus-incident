package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// worstCaseDatabase creates a fresh qa_ database for one test on the isolated
// test server and drops it when the test ends. Tests that kill connections or
// change database settings never touch a database another test uses.
func worstCaseDatabase(t *testing.T) string {
	t.Helper()
	server := os.Getenv("TEST_POSTGRES_DSN")
	if server == "" {
		t.Skip("isolated Postgres required")
	}
	target, err := url.Parse(server)
	if err != nil {
		t.Fatal("test server address is not a URL")
	}
	name := fmt.Sprintf("qa_worst_case_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(t.Context(), server)
	if err != nil {
		t.Fatal("cannot reach the test server")
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
			t.Errorf("drop %s: cannot reach the test server", name)
			return
		}
		defer conn.Close(ctx)
		if _, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	target.Path = "/" + name
	return target.String()
}

// worstCaseProvider opens a store on the database with background maintenance
// stopped and the journal on the partitioned layout long-running deployments use.
func worstCaseProvider(t *testing.T, dsn string, options PostgresOptions) *postgresProvider {
	t.Helper()
	options.DSN = dsn
	if options.ObservationHeartbeat == "" {
		options.ObservationHeartbeat = "5m"
	}
	// History is kept in full unless a test sets a retention.
	if options.EvidenceRetention == "" {
		options.EvidenceRetention = "0"
	}
	provider, err := NewPostgres(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	p := provider.(*postgresProvider)
	p.maintenanceCancel()
	<-p.maintenanceDone
	settlePartitionCopy(t, p)
	var kind string
	if err := p.db.QueryRow(`SELECT relkind::text FROM pg_class WHERE oid='kubernetes_evidence_events'::regclass`).Scan(&kind); err != nil || kind != "p" {
		t.Fatalf("events table not partitioned: %q %v", kind, err)
	}
	return p
}

// worstChange is one change record whose content is value, padded to roughly
// pad payload bytes.
func worstChange(id, value string, at, observed time.Time, pad int) EvidenceRecord {
	fields := map[string]any{"cluster": "worst", "kind": "Deployment", "name": id, "type": "created", "value": value, "at": at}
	if pad > 0 {
		fields["pad"] = strings.Repeat("p", pad)
	}
	payload, _ := json.Marshal(fields)
	return EvidenceRecord{ID: id, Type: "change", Rule: "created", Priority: 1, At: at, ObservedAt: observed, Namespace: "shop", Name: id, Kind: "Deployment", AuthorizationKind: "Deployment", UID: id, RootUID: id, Epoch: "stable", Payload: payload}
}

func worstCommit(ctx context.Context, store EvidenceStore, scope EvidenceScope, id string, at time.Time, records []EvidenceRecord, issues ...EvidenceIssue) error {
	cp, err := store.EvidenceCheckpoint(ctx, scope)
	if err != nil {
		return err
	}
	return store.CommitEvidence(ctx, scope, EvidenceBatch{ID: id, Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: "stable", Records: records, Issues: issues})
}

type worstRowCounts struct{ rows, current, heads, distinct int }

func worstCounts(t *testing.T, p *postgresProvider, scope EvidenceScope) worstRowCounts {
	t.Helper()
	key, _ := evidenceScopeKey(scope)
	var counts worstRowCounts
	if err := p.db.QueryRow(`SELECT (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1),
	 (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND superseded_seq IS NULL),
	 (SELECT COUNT(*) FROM kubernetes_evidence_event_heads WHERE scope_key=$1),
	 (SELECT COUNT(DISTINCT event_id) FROM kubernetes_evidence_events WHERE scope_key=$1)`, key).Scan(&counts.rows, &counts.current, &counts.heads, &counts.distinct); err != nil {
		t.Fatal(err)
	}
	return counts
}

// worstJournalConsistent checks the journal shape every save must leave: one
// current row per event, no repeated version, and a head on each current row.
func worstJournalConsistent(t *testing.T, p *postgresProvider, scope EvidenceScope) {
	t.Helper()
	key, _ := evidenceScopeKey(scope)
	var manyCurrent, repeated, strayHeads, headless int
	if err := p.db.QueryRow(`SELECT
	 (SELECT COUNT(*) FROM (SELECT event_id FROM kubernetes_evidence_events WHERE scope_key=$1 AND superseded_seq IS NULL GROUP BY event_id HAVING COUNT(*)>1) x),
	 (SELECT COUNT(*) FROM (SELECT event_id,event_version FROM kubernetes_evidence_events WHERE scope_key=$1 GROUP BY event_id,event_version HAVING COUNT(*)>1) y),
	 (SELECT COUNT(*) FROM kubernetes_evidence_event_heads h WHERE h.scope_key=$1 AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_events e WHERE e.scope_key=h.scope_key AND e.sequence=h.sequence AND e.event_at=h.event_at AND e.superseded_seq IS NULL)),
	 (SELECT COUNT(*) FROM kubernetes_evidence_events e WHERE e.scope_key=$1 AND e.superseded_seq IS NULL AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_event_heads h WHERE h.scope_key=e.scope_key AND h.event_id=e.event_id))`, key).Scan(&manyCurrent, &repeated, &strayHeads, &headless); err != nil {
		t.Fatal(err)
	}
	if manyCurrent != 0 || repeated != 0 || strayHeads != 0 || headless != 0 {
		t.Fatalf("journal inconsistent: events with several current rows=%d repeated versions=%d heads off a current row=%d current rows without head=%d", manyCurrent, repeated, strayHeads, headless)
	}
}

// A store answer the caller cannot act on is an untyped error: the capture
// layer then treats it as an outage and retries the same batch forever. A
// refusal must be typed so the batch can be dropped once with a gap.
func typedRefusal(err error) bool {
	return errors.Is(err, ErrInvalidEvidence) || errors.Is(err, ErrEvidenceLimit)
}

// worstPoisonText returns the saved text that began as the poison: the first
// string in the stored record's JSON that starts with "start".
func worstPoisonText(t *testing.T, store EvidenceStore, scope EvidenceScope, at time.Time) (string, bool) {
	t.Helper()
	page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var find func(value any) (string, bool)
	find = func(value any) (string, bool) {
		switch typed := value.(type) {
		case string:
			if strings.HasPrefix(typed, "start") {
				return typed, true
			}
			var nested any
			if json.Unmarshal([]byte(typed), &nested) == nil {
				return find(nested)
			}
		case []any:
			for _, item := range typed {
				if text, ok := find(item); ok {
					return text, true
				}
			}
		case map[string]any:
			for _, item := range typed {
				if text, ok := find(item); ok {
					return text, true
				}
			}
		}
		return "", false
	}
	for _, item := range page.Items {
		var value any
		if json.Unmarshal(item.Payload, &value) != nil {
			continue
		}
		if text, ok := find(value); ok {
			return text, true
		}
	}
	return "", false
}

// TestEvidenceUnstorableTextGetsOneTypedAnswerFromEveryStore offers a batch of
// good records with one record or issue whose JSON text Postgres jsonb cannot
// hold: an escaped NUL, invalid UTF-8, a lone surrogate. Every store must give
// the same answer, either saving the batch or refusing it as invalid evidence,
// and the next batch without the poison must save. A store that saves it keeps
// the same normalized text as every other store: the unstorable parts become
// U+FFFD and nothing a store cannot hold remains.
func TestEvidenceUnstorableTextGetsOneTypedAnswerFromEveryStore(t *testing.T) {
	poisons := []struct{ name, text string }{
		{"escaped NUL", `"start\u0000end"`},
		{"invalid UTF-8", "\"start\xff\xfeend\""},
		{"lone surrogate", `"start\ud800end"`},
	}
	backends := []string{"memory", "file", "postgres"}
	var p *postgresProvider
	if os.Getenv("TEST_POSTGRES_DSN") != "" {
		p = worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for _, poison := range poisons {
		for _, place := range []string{"record", "issue"} {
			answers := map[string]error{}
			texts := map[string]string{}
			for _, backend := range backends {
				var store EvidenceStore
				if backend == "postgres" {
					if p == nil {
						continue
					}
					store = p
				} else {
					provider, _ := localEvidenceTestProvider(t, backend)
					store = provider.(EvidenceStore)
				}
				scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("poison-%s-%d", place, time.Now().UnixNano()), ClusterUID: "physical"}
				good := make([]EvidenceRecord, 0, 51)
				for i := 0; i < 50; i++ {
					good = append(good, worstChange(fmt.Sprintf("change:good-%d", i), "1", at, at, 0))
				}
				records := append([]EvidenceRecord(nil), good...)
				var issues []EvidenceIssue
				payload := json.RawMessage(`{"cluster":"worst","type":"created","value":` + poison.text + `}`)
				if place == "record" {
					bad := worstChange("change:poison", "1", at, at, 0)
					bad.Payload = payload
					records = append(records, bad)
				} else {
					issues = append(issues, EvidenceIssue{Key: "poison-issue", Namespace: "shop", Kind: "Pod", UID: "pod-uid", Rule: "pod.crash_loop", Payload: json.RawMessage(`{"rule":"pod.crash_loop","message":` + poison.text + `}`)})
				}
				batch := EvidenceBatch{ID: "poisoned", At: at, Epoch: "stable", Records: records, Issues: issues}
				validated := batch.Validate()
				err := store.CommitEvidence(t.Context(), scope, batch)
				answers[backend] = err
				t.Logf("%s in %s on %s: validate=%v commit=%v", poison.name, place, backend, validated, err)
				if err != nil && !typedRefusal(err) {
					t.Errorf("%s in %s on %s: untyped refusal %q; the capture layer retries it as an outage on every cycle", poison.name, place, backend, err)
				}
				if validated == nil && err != nil {
					t.Errorf("%s in %s on %s: the batch passed Validate but the store refused it", poison.name, place, backend)
				}
				if err == nil {
					text, found := worstPoisonText(t, store, scope, at)
					if !found {
						t.Errorf("%s in %s on %s: the saved batch has no trace of the poisoned text", poison.name, place, backend)
					} else if !utf8.ValidString(text) || strings.ContainsRune(text, 0) || !strings.ContainsRune(text, utf8.RuneError) || !strings.HasSuffix(text, "end") {
						t.Errorf("%s in %s on %s: saved text %q is not the normalized text", poison.name, place, backend, text)
					}
					texts[backend] = text
				}
				// The poison must not stick: the next capture without it saves.
				clean := append([]EvidenceRecord(nil), good...)
				clean = append(clean, worstChange("change:after", "1", at.Add(time.Minute), at.Add(time.Minute), 0))
				if err := worstCommit(t.Context(), store, scope, "clean", at.Add(time.Minute), clean); err != nil {
					t.Errorf("%s in %s on %s: the next clean save failed: %v", poison.name, place, backend, err)
				}
			}
			saved := map[bool][]string{}
			for backend, err := range answers {
				saved[err == nil] = append(saved[err == nil], backend)
			}
			if len(saved) > 1 {
				t.Errorf("%s in %s: stores disagree, saved by %v and refused by %v", poison.name, place, saved[true], saved[false])
			}
			distinct := map[string][]string{}
			for backend, text := range texts {
				distinct[text] = append(distinct[text], backend)
			}
			if len(distinct) > 1 {
				t.Errorf("%s in %s: stores saved different text: %q", poison.name, place, distinct)
			}
		}
	}
}

// TestPostgresLargestAcceptedSaveFitsTheLeaseOnDeepHistory saves the largest
// batch a store accepts, by record count and by payload bytes, onto events that
// already have deep version chains, and checks it completes well inside the
// 20-second lease. One record or one byte more is refused as a typed limit and
// writes nothing.
func TestPostgresLargestAcceptedSaveFitsTheLeaseOnDeepHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("deep history: 100k journal rows")
	}
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "largest-save", ClusterUID: "physical"}
	const events, depth = 10000, 10
	origin := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	// Pad every record so the batch sits just under the 8 MiB payload limit.
	probe := worstChange("change:deep-00000", "10", origin, origin, 1)
	pad := (8<<20-64<<10)/events - len(probe.Payload) + 1
	batchOf := func(round int, at time.Time) []EvidenceRecord {
		records := make([]EvidenceRecord, events)
		for i := range records {
			records[i] = worstChange(fmt.Sprintf("change:deep-%05d", i), fmt.Sprint(round), at, at, pad)
		}
		return records
	}
	size := func(records []EvidenceRecord) int {
		total := 0
		for _, record := range records {
			total += len(record.Payload)
		}
		return total
	}
	for round := 0; round < depth; round++ {
		at := origin.Add(time.Duration(round) * time.Minute)
		if err := worstCommit(t.Context(), p, scope, fmt.Sprintf("seed-%d", round), at, batchOf(round, at)); err != nil {
			t.Fatalf("seeding round %d: %v", round, err)
		}
	}
	seeded := worstCounts(t, p, scope)
	if seeded.rows != events*depth || seeded.heads != events {
		t.Fatalf("seed: %+v", seeded)
	}
	if _, err := p.db.Exec(`ANALYZE kubernetes_evidence_events; ANALYZE kubernetes_evidence_event_heads`); err != nil {
		t.Fatal(err)
	}

	// The largest record count, every record a real change.
	at := origin.Add(time.Hour)
	full := batchOf(depth, at)
	if bytes := size(full); bytes > 8<<20 {
		t.Fatalf("test batch is over the byte limit: %d", bytes)
	}
	started := time.Now()
	if err := worstCommit(t.Context(), p, scope, "largest-changed", at, full); err != nil {
		t.Fatalf("largest changed save: %v", err)
	}
	changed := time.Since(started)
	// The same records again, unchanged.
	again := batchOf(depth, at.Add(time.Minute))
	started = time.Now()
	if err := worstCommit(t.Context(), p, scope, "largest-unchanged", at.Add(time.Minute), again); err != nil {
		t.Fatalf("largest unchanged save: %v", err)
	}
	unchanged := time.Since(started)
	after := worstCounts(t, p, scope)
	if after.rows != seeded.rows+events || after.heads != events {
		t.Fatalf("largest saves wrote %d rows, want %d: %+v", after.rows-seeded.rows, events, after)
	}

	// The largest payload: 128 records of 64 KiB, exactly the byte limit.
	heavy := make([]EvidenceRecord, 128)
	heavyAt := at.Add(2 * time.Minute)
	for i := range heavy {
		record := worstChange(fmt.Sprintf("change:heavy-%03d", i), "1", heavyAt, heavyAt, 0)
		head := string(record.Payload[:len(record.Payload)-1])
		filler := (64 << 10) - len(head) - len(`,"fill":""}`)
		record.Payload = json.RawMessage(head + `,"fill":"` + strings.Repeat("h", filler) + `"}`)
		heavy[i] = record
	}
	if total := size(heavy); total != 8<<20 {
		t.Fatalf("heavy batch is %d bytes, want exactly %d", total, 8<<20)
	}
	started = time.Now()
	if err := worstCommit(t.Context(), p, scope, "largest-bytes", heavyAt, heavy); err != nil {
		t.Fatalf("largest byte save: %v", err)
	}
	heavySave := time.Since(started)
	t.Logf("largest saves on %d rows: %d changed records %s; %d unchanged records %s; 128 x 64 KiB %s", seeded.rows, events, changed, events, unchanged, heavySave)
	for name, took := range map[string]time.Duration{"changed": changed, "unchanged": unchanged, "bytes": heavySave} {
		if took > 10*time.Second {
			t.Errorf("largest %s save took %s, more than half the 20 s lease", name, took)
		}
	}

	// One over either capture limit is a typed refusal that writes nothing.
	// The capture limits bound memory; saves are split into bounded parts.
	before := worstCounts(t, p, scope)
	checkpoint, _ := p.EvidenceCheckpoint(t.Context(), scope)
	overAt := heavyAt.Add(time.Minute)
	overCount := make([]EvidenceRecord, 0, MaxEvidenceBatchRecords+1)
	for i := 0; i <= MaxEvidenceBatchRecords; i++ {
		overCount = append(overCount, worstChange(fmt.Sprintf("change:count-%05d", i), "1", overAt, overAt, 0))
	}
	overBytes := []EvidenceRecord{}
	for i := 0; i < MaxEvidenceBatchBytes/(64<<10); i++ {
		record := heavy[0]
		record.ID = fmt.Sprintf("change:bytes-%03d", i)
		record.UID, record.RootUID, record.Name = record.ID, record.ID, record.ID
		record.At, record.ObservedAt = overAt, overAt
		overBytes = append(overBytes, record)
	}
	overBytes = append(overBytes, worstChange("change:bytes-more", "1", overAt, overAt, 0))
	for name, records := range map[string][]EvidenceRecord{"one record over the capture limit": overCount, "the byte limit plus one small record": overBytes} {
		err := worstCommit(t.Context(), p, scope, "over-"+name, overAt, records)
		if !errors.Is(err, ErrEvidenceLimit) {
			t.Errorf("%s: %v, want the typed evidence limit", name, err)
		}
	}
	if later := worstCounts(t, p, scope); later != before {
		t.Fatalf("a refused batch wrote rows: %+v -> %+v", before, later)
	}
	if latest, _ := p.EvidenceCheckpoint(t.Context(), scope); latest.ID != checkpoint.ID {
		t.Fatalf("a refused batch moved the checkpoint: %s -> %s", checkpoint.ID, latest.ID)
	}
	worstJournalConsistent(t, p, scope)
}

// worstAdmin opens a direct connection to the test database.
func worstAdmin(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal("cannot reach the test database")
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// holdLastSaveStatement makes every save wait, after all of its writes, at the
// capture marker it inserts last, for as long as the returned connection holds
// the hold. It installs a trigger in the test's own throwaway database only.
func holdLastSaveStatement(t *testing.T, dsn string) (release func()) {
	t.Helper()
	admin := worstAdmin(t, dsn)
	for _, statement := range []string{
		`CREATE OR REPLACE FUNCTION qa_hold_capture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(4242); RETURN NEW; END $$`,
		`CREATE TRIGGER qa_hold_capture BEFORE INSERT ON kubernetes_evidence_captures FOR EACH ROW EXECUTE FUNCTION qa_hold_capture()`,
	} {
		if _, err := admin.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	holder := worstAdmin(t, dsn)
	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_lock(4242)`); err != nil {
		t.Fatal(err)
	}
	released := false
	return func() {
		if !released {
			released = true
			if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_unlock(4242)`); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// heldSavePID waits for a save held at its last statement and returns its
// backend.
func heldSavePID(t *testing.T, admin *pgx.Conn) int {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var pid int
		if admin.QueryRow(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory' AND query LIKE 'INSERT INTO kubernetes_evidence_captures%' LIMIT 1`).Scan(&pid) == nil {
			return pid
		}
	}
	t.Fatal("the save never reached its last statement")
	return 0
}

// TestPostgresSaveKilledBeforeCommitLeavesNoTraceAndRetriesOnce holds a save
// after it has written its journal rows, heads and checkpoint but before it
// commits, then kills its connection. A capture is saved in bounded parts, so
// the parts committed before the kill stay as saved progress, but readers stay
// on the old checkpoint and see none of it. The answer must not look like a
// lease loss or a refusal, and retrying the same capture must append every
// row exactly once.
func TestPostgresSaveKilledBeforeCommitLeavesNoTraceAndRetriesOnce(t *testing.T) {
	dsn := worstCaseDatabase(t)
	p := worstCaseProvider(t, dsn, PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "killed-save", ClusterUID: "physical"}
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	base := make([]EvidenceRecord, 1000)
	for i := range base {
		base[i] = worstChange(fmt.Sprintf("change:object-%04d", i), "1", origin, origin, 0)
	}
	if err := worstCommit(t.Context(), p, scope, "c0", origin, base); err != nil {
		t.Fatal(err)
	}
	before := worstCounts(t, p, scope)
	checkpoint, _ := p.EvidenceCheckpoint(t.Context(), scope)
	at := origin.Add(time.Minute)
	next := make([]EvidenceRecord, 0, 2000)
	for i := 0; i < 1000; i++ {
		next = append(next, worstChange(fmt.Sprintf("change:object-%04d", i), "2", at, at, 0))
		next = append(next, worstChange(fmt.Sprintf("change:new-%04d", i), "1", at, at, 0))
	}
	batch := EvidenceBatch{ID: "c1", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at, Epoch: "stable", Records: next}
	visibleBefore := savePartsVisible(t, p, scope, at)

	release := holdLastSaveStatement(t, dsn)
	saved := make(chan error, 1)
	go func() { saved <- p.CommitEvidence(context.Background(), scope, batch) }()
	admin := worstAdmin(t, dsn)
	savePID := heldSavePID(t, admin)
	var killed bool
	if err := admin.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, savePID).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate save connection: %v", err)
	}
	var saveErr error
	select {
	case saveErr = <-saved:
	case <-time.After(30 * time.Second):
		t.Fatal("the killed save never returned")
	}
	release()
	t.Logf("killed save answered: %v", saveErr)
	if saveErr == nil {
		t.Fatal("a save whose connection was killed before commit reported success")
	}
	if errors.Is(saveErr, ErrEvidenceConflict) || typedRefusal(saveErr) {
		t.Fatalf("a killed connection was answered as a lease loss or refusal: %v", saveErr)
	}
	if visible := savePartsVisible(t, p, scope, at); visible != visibleBefore {
		t.Fatalf("readers see %d rows after the killed save, want the %d they saw before", visible, visibleBefore)
	}
	worstJournalConsistent(t, p, scope)
	if latest, _ := p.EvidenceCheckpoint(t.Context(), scope); latest.ID != checkpoint.ID {
		t.Fatalf("the killed save advanced the checkpoint to %s", latest.ID)
	}
	// The owner retries the same capture: every row lands exactly once.
	var err error
	if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatalf("retry after the killed save: %v", err)
	}
	if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatalf("a repeated retry of a saved capture: %v", err)
	}
	after := worstCounts(t, p, scope)
	if after.rows != before.rows+2000 || after.current != 2000 || after.heads != 2000 {
		t.Fatalf("retry wrote %+v, want %d rows with 2000 current", after, before.rows+2000)
	}
	worstJournalConsistent(t, p, scope)
}

// TestPostgresSaveTimeoutsLeaveNoTraceAndRetryOnce times a large save out two
// ways, by the server's statement timeout and by the capture lease deadline.
// Neither may show readers anything or advance the checkpoint (parts saved
// before the timeout stay as progress), neither may read as a lease loss or a
// refusal, and the retry appends every row exactly once.
func TestPostgresSaveTimeoutsLeaveNoTraceAndRetryOnce(t *testing.T) {
	dsn := worstCaseDatabase(t)
	p := worstCaseProvider(t, dsn, PostgresOptions{})
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	records := func(at time.Time) []EvidenceRecord {
		out := make([]EvidenceRecord, 10000)
		for i := range out {
			out[i] = worstChange(fmt.Sprintf("change:timeout-%05d", i), "1", at, at, 200)
		}
		return out
	}
	for _, way := range []string{"statement timeout", "lease deadline"} {
		t.Run(way, func(t *testing.T) {
			scope := EvidenceScope{OrgID: "default", ClusterID: "timeout-" + strings.ReplaceAll(way, " ", "-"), ClusterUID: "physical"}
			if err := worstCommit(t.Context(), p, scope, "c0", origin, records(origin)[:10]); err != nil {
				t.Fatal(err)
			}
			checkpoint, _ := p.EvidenceCheckpoint(t.Context(), scope)
			at := origin.Add(time.Minute)
			batch := EvidenceBatch{ID: "c1", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at, Epoch: "stable", Records: records(at)}
			visibleBefore := savePartsVisible(t, p, scope, at)
			var err error
			if way == "statement timeout" {
				// Every save transaction is timed out by the server. A bounded
				// save retries smaller parts, so this fails only once the
				// smallest part times out too.
				p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
					return savePartsStatementTimeout(ctx, tx)
				}
				err = p.CommitEvidence(t.Context(), scope, batch)
				p.evidenceChunkFault = nil
			} else {
				ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
				err = p.CommitEvidence(ctx, scope, batch)
				cancel()
			}
			t.Logf("%s answered: %v", way, err)
			if err == nil {
				t.Skip("the save finished before the timeout; nothing to observe")
			}
			if errors.Is(err, ErrEvidenceConflict) || typedRefusal(err) {
				t.Fatalf("a %s was answered as a lease loss or refusal: %v", way, err)
			}
			if visible := savePartsVisible(t, p, scope, at); visible != visibleBefore {
				t.Fatalf("readers see %d rows after the timed-out save, want the %d they saw before", visible, visibleBefore)
			}
			worstJournalConsistent(t, p, scope)
			if latest, _ := p.EvidenceCheckpoint(t.Context(), scope); latest.ID != checkpoint.ID {
				t.Fatalf("the timed-out save advanced the checkpoint to %s", latest.ID)
			}
			if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatalf("retry after %s: %v", way, err)
			}
			if after := worstCounts(t, p, scope); after.rows != 10000 || after.heads != 10000 {
				t.Fatalf("retry after %s: %+v, want 10000 rows", way, after)
			}
			worstJournalConsistent(t, p, scope)
		})
	}
}

// TestPostgresSavesResumeAfterEveryConnectionIsKilled kills every connection
// the store holds, as a database restart does, between two saves. The store
// must recover on its own within one retry, without a restart of Versus.
func TestPostgresSavesResumeAfterEveryConnectionIsKilled(t *testing.T) {
	dsn := worstCaseDatabase(t)
	p := worstCaseProvider(t, dsn, PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "restarted", ClusterUID: "physical"}
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	admin := worstAdmin(t, dsn)
	for cycle := 0; cycle < 3; cycle++ {
		at := origin.Add(time.Duration(cycle) * time.Minute)
		records := []EvidenceRecord{worstChange("change:restart", fmt.Sprint(cycle), at, at, 0)}
		if err := worstCommit(t.Context(), p, scope, fmt.Sprintf("before-%d", cycle), at, records); err != nil {
			t.Fatal(err)
		}
	}
	var killed int
	if err := admin.QueryRow(t.Context(), `SELECT COUNT(*) FILTER(WHERE pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()`).Scan(&killed); err != nil || killed == 0 {
		t.Fatalf("killed %d store connections: %v", killed, err)
	}
	// Each pooled connection the restart killed may fail one attempt; the
	// capture layer retries a failed save on the next cycle.
	failures := 0
	for cycle := 3; cycle < 6; cycle++ {
		at := origin.Add(time.Duration(cycle) * time.Minute)
		records := []EvidenceRecord{worstChange("change:restart", fmt.Sprint(cycle), at, at, 0)}
		for attempt := 0; ; attempt++ {
			err := worstCommit(t.Context(), p, scope, fmt.Sprintf("after-%d", cycle), at, records)
			if err == nil {
				break
			}
			failures++
			t.Logf("save attempt after the restart failed: %v", err)
			if errors.Is(err, ErrEvidenceConflict) || typedRefusal(err) {
				t.Fatalf("a dead connection was answered as a lease loss or refusal: %v", err)
			}
			if attempt == defaultPostgresMaxIdleConns+1 {
				t.Fatalf("the store did not recover after %d attempts", attempt+1)
			}
		}
	}
	t.Logf("failed attempts after the restart: %d (idle pool %d)", failures, defaultPostgresMaxIdleConns)
	if failures > defaultPostgresMaxIdleConns {
		t.Fatalf("%d attempts failed after one restart, more than the %d idle connections it killed", failures, defaultPostgresMaxIdleConns)
	}
	if counts := worstCounts(t, p, scope); counts.rows != 6 || counts.current != 1 {
		t.Fatalf("history after the restart: %+v", counts)
	}
	worstJournalConsistent(t, p, scope)
}

// replicaSave follows the capture layer's conflict protocol: on a conflict it
// re-reads the checkpoint and saves again, as a historical observation when
// another replica already covered a later time.
func replicaSave(ctx context.Context, store EvidenceStore, scope EvidenceScope, batch EvidenceBatch) error {
	err := store.CommitEvidence(ctx, scope, batch)
	if !errors.Is(err, ErrEvidenceConflict) {
		return err
	}
	checkpoint, err := store.EvidenceCheckpoint(ctx, scope)
	if err != nil {
		return err
	}
	if checkpoint.Generation != batch.Generation {
		return nil
	}
	batch.Expected = checkpoint.ID
	if !batch.At.After(checkpoint.At) {
		batch.At, batch.HistoricalOnly = checkpoint.At, true
		batch.CoveredThrough, batch.Coverage = time.Time{}, nil
	}
	return store.CommitEvidence(ctx, scope, batch)
}

// TestPostgresReplicasSavingConcurrentlyKeepOneRowPerChange runs two replicas
// on two connection pools against one scope. One replica's clock is 90 s
// behind. They save the same cluster concurrently, then the skewed replica
// takes over alone, then the other returns. Every new event is saved exactly
// once, no version is stored twice, and every event ends at its latest
// content.
func TestPostgresReplicasSavingConcurrentlyKeepOneRowPerChange(t *testing.T) {
	dsn := worstCaseDatabase(t)
	replicas := []*postgresProvider{worstCaseProvider(t, dsn, PostgresOptions{}), worstCaseProvider(t, dsn, PostgresOptions{})}
	skews := []time.Duration{0, -90 * time.Second}
	scope := EvidenceScope{OrgID: "default", ClusterID: "two-replicas", ClusterUID: "physical"}
	const objects, bursts, cycles = 400, 40, 60
	origin := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	values := make([]int, objects)
	counts := make([]int, bursts)
	created := []string{}
	latest := map[string]EvidenceRecord{}
	conflicts := 0
	for cycle := 0; cycle < cycles; cycle++ {
		at := origin.Add(time.Duration(cycle) * 30 * time.Second)
		if cycle > 0 {
			for i := 0; i < 5; i++ {
				values[(cycle*7+i)%objects]++
			}
			for i := 0; i < 2; i++ {
				created = append(created, fmt.Sprintf("change:created-%d-%d", cycle, i))
			}
			counts[cycle%bursts]++
		}
		build := func(skew time.Duration) []EvidenceRecord {
			observed := at.Add(skew)
			records := make([]EvidenceRecord, 0, objects+bursts+len(created))
			for i := 0; i < objects; i++ {
				records = append(records, worstChange(fmt.Sprintf("change:object-%03d", i), fmt.Sprint(values[i]), observed, observed, 0))
			}
			// A warning burst keeps its identity while its count grows.
			for i := 0; i < bursts; i++ {
				records = append(records, worstChange(fmt.Sprintf("change:burst-%02d", i), fmt.Sprint(counts[i]), origin.Add(-time.Hour), observed, 0))
			}
			// A created object is reported once, by whoever sees it first.
			for _, id := range created[max(0, len(created)-2):] {
				records = append(records, worstChange(id, "1", observed, observed, 0))
			}
			return records
		}
		for _, record := range build(0) {
			latest[record.ID] = record
		}
		active := []int{0, 1}
		switch {
		case cycle >= 20 && cycle < 40:
			active = []int{1}
		case cycle >= 40:
			active = []int{0}
		}
		var wg sync.WaitGroup
		errs := make([]error, len(replicas))
		for _, r := range active {
			wg.Add(1)
			go func(r int) {
				defer wg.Done()
				store := replicas[r]
				checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
				if err != nil {
					errs[r] = err
					return
				}
				batch := EvidenceBatch{ID: fmt.Sprintf("r%d-c%d", r, cycle), Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at.Add(skews[r]), Epoch: "stable", Records: build(skews[r])}
				errs[r] = replicaSave(t.Context(), store, scope, batch)
			}(r)
		}
		wg.Wait()
		for r, err := range errs {
			// Two savers racing past each other's retry is a lease answer; the
			// capture layer reports lease_lost and saves on its next cycle.
			if errors.Is(err, ErrEvidenceConflict) {
				conflicts++
				continue
			}
			if err != nil {
				t.Fatalf("cycle %d replica %d: %v", cycle, r, err)
			}
		}
	}
	worstJournalConsistent(t, replicas[0], scope)
	key, _ := evidenceScopeKey(scope)
	rows, err := replicas[0].db.Query(`SELECT event_id,event_version FROM kubernetes_evidence_event_heads WHERE scope_key=$1`, key)
	if err != nil {
		t.Fatal(err)
	}
	heads := map[string]string{}
	for rows.Next() {
		var id, version string
		if err = rows.Scan(&id, &version); err != nil {
			t.Fatal(err)
		}
		heads[id] = version
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	stale := []string{}
	for id, record := range latest {
		if heads[id] != evidenceVersion(record) {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	if len(heads) != len(latest) || len(stale) > 0 {
		t.Fatalf("events=%d heads=%d; not at their latest content: %v", len(latest), len(heads), stale)
	}
	counted := worstCounts(t, replicas[0], scope)
	realChanges := 0
	for _, value := range values {
		realChanges += value
	}
	for _, count := range counts {
		realChanges += count
	}
	if limit := objects + bursts + len(created) + realChanges; counted.rows > limit {
		t.Fatalf("rows=%d exceed one per real change (%d)", counted.rows, limit)
	}
	t.Logf("two replicas, %d cycles: rows=%d events=%d real changes=%d lease conflicts=%d", cycles, counted.rows, counted.distinct, realChanges, conflicts)
}

// TestPostgresPrincipalFlapEveryCycleAppendsOnlyGaps alternates the history
// principal and credential epoch on every save of a 2,000-object cluster, the
// way a real role change retires the epoch. Objects are offered again under
// each new epoch. Only the coverage gap each rebuild records may be appended.
func TestPostgresPrincipalFlapEveryCycleAppendsOnlyGaps(t *testing.T) {
	if testing.Short() {
		t.Skip("2,000 objects over 40 identity changes")
	}
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "identity-flap", ClusterUID: "physical"}
	const objects, crashing, cycles = 2000, 200, 40
	kinds := []string{"Pod", "Deployment", "ReplicaSet"}
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	var prior time.Time
	for cycle := 0; cycle < cycles; cycle++ {
		at := origin.Add(time.Duration(cycle) * time.Minute)
		principal, epoch := "role-a", fmt.Sprintf("epoch-a-%d", cycle)
		if cycle%2 == 1 {
			principal, epoch = "role-b", fmt.Sprintf("epoch-b-%d", cycle)
		}
		records := make([]EvidenceRecord, 0, objects+crashing+len(kinds))
		for i := 0; i < objects; i++ {
			// After a rebuild the object is first seen at this cycle.
			record := worstChange(fmt.Sprintf("change:object-%04d", i), "1", at, at, 0)
			record.Epoch = epoch
			records = append(records, record)
		}
		for i := 0; i < crashing; i++ {
			record := headsStatus(fmt.Sprintf("crash:status-%03d", i), 4, at)
			record.Epoch = epoch
			records = append(records, record)
		}
		if !prior.IsZero() {
			for _, kind := range kinds {
				payload, _ := json.Marshal(map[string]any{"from": prior, "to": at})
				records = append(records, EvidenceRecord{ID: fmt.Sprintf("gap:cycle-%d:%s", cycle, kind), Type: "coverage_gap", GapFrom: prior, GapTo: at, Kind: kind, At: at, ObservedAt: at, Epoch: epoch, Payload: payload})
			}
		}
		prior = at
		checkpoint, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		authority := map[string]EvidenceAuthority{}
		for _, kind := range kinds {
			authority[kind] = EvidenceAuthority{State: "allowed", ProvedAt: at}
		}
		batch := EvidenceBatch{ID: fmt.Sprintf("flap-%d", cycle), Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at, Epoch: epoch, Principal: principal, Authority: authority, Records: records}
		if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatalf("cycle %d as %s: %v", cycle, principal, err)
		}
	}
	counts := worstCounts(t, p, scope)
	gaps := (cycles - 1) * len(kinds)
	if want := objects + crashing + gaps; counts.rows != want {
		t.Fatalf("identity flaps appended %d rows beyond the cluster and its %d gaps", counts.rows-want, gaps)
	}
	checkpoint, _ := p.EvidenceCheckpoint(t.Context(), scope)
	if checkpoint.Principal != "role-b" || len(checkpoint.Authority) != len(kinds) {
		t.Fatalf("checkpoint after the last flap: principal=%q authority=%d", checkpoint.Principal, len(checkpoint.Authority))
	}
	worstJournalConsistent(t, p, scope)
}

// TestPostgresSaveAcrossAMonthBoundary saves batches that straddle a month
// partition boundary: records on both sides in one save, a change superseding
// a row in the previous month, and a crash refreshed in place across it.
func TestPostgresSaveAcrossAMonthBoundary(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "month-boundary", ClusterUID: "physical"}
	key, _ := evidenceScopeKey(scope)
	now := time.Now().UTC()
	boundary := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	before, after := boundary.Add(-time.Second), boundary.Add(time.Second)
	if err := worstCommit(t.Context(), p, scope, "c0", before, []EvidenceRecord{worstChange("change:straddle", "1", before, before, 0), headsStatus("crash:straddle", 3, before)}); err != nil {
		t.Fatal(err)
	}
	records := []EvidenceRecord{
		worstChange("change:straddle", "2", after, after, 0),
		headsStatus("crash:straddle", 3, after),
		worstChange("change:last-microsecond", "1", boundary.Add(-time.Microsecond), after, 0),
		worstChange("change:first-instant", "1", boundary, after, 0),
	}
	if err := worstCommit(t.Context(), p, scope, "c1", after, records); err != nil {
		t.Fatalf("straddling save: %v", err)
	}
	refresh := boundary.Add(6 * time.Minute)
	if err := worstCommit(t.Context(), p, scope, "c2", refresh, []EvidenceRecord{headsStatus("crash:straddle", 3, refresh)}); err != nil {
		t.Fatal(err)
	}
	month := func(at time.Time) string { return "evidence_event_month_" + at.UTC().Format("200601") }
	rows, err := p.db.Query(`SELECT event_id,tableoid::regclass::text,superseded_seq IS NULL,observed_at FROM kubernetes_evidence_events WHERE scope_key=$1 ORDER BY event_id,sequence`, key)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id, partition string
		current       bool
		observed      time.Time
	}
	var stored []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.id, &r.partition, &r.current, &r.observed); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, r)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	want := []row{
		{"change:first-instant", month(after), true, after},
		{"change:last-microsecond", month(before), true, after},
		{"change:straddle", month(before), false, before},
		{"change:straddle", month(after), true, after},
		{"crash:straddle", month(before), true, refresh},
	}
	if len(stored) != len(want) {
		t.Fatalf("rows across the boundary: %+v", stored)
	}
	for i := range want {
		got := stored[i]
		if got.id != want[i].id || got.partition != want[i].partition || got.current != want[i].current || !got.observed.Equal(want[i].observed) {
			t.Fatalf("row %d = %+v, want %+v", i, got, want[i])
		}
	}
	worstJournalConsistent(t, p, scope)
}

// oldTermination is a container termination the kubelet still reports in
// lastState long after it finished. Its event time is the finish time.
func oldTermination(finished, observed time.Time) EvidenceRecord {
	payload, _ := json.Marshal(map[string]any{"cluster": "worst", "namespace": "shop", "pod": "api", "container": "app", "type": "termination", "termination_slot": "previous", "reason": "OOMKilled", "exit_code": 137, "restart_count": 1, "time_source": "finished_at", "at": finished, "observed_at": observed})
	return EvidenceRecord{ID: "crash:old-termination", Type: "crash", At: finished, ObservedAt: observed, Namespace: "shop", Name: "api", Kind: "Deployment", AuthorizationKind: "Pod", UID: "pod-uid", RootUID: "root-uid", PodUID: "pod-uid", Epoch: "stable", Payload: payload}
}

// TestPostgresRetainedTerminationDoesNotResurrectADroppedMonth keeps a Pod whose
// last termination finished three months ago, beyond a 30-day retention. The
// kubelet reports that termination on every read, so every capture offers it
// unchanged. Once retention has dropped its month, re-offering it must not
// append rows, recreate the month, or invalidate the scope's cursors again on
// every maintenance pass. The termination reaches the journal first through a
// store without retention, as it does on a deployment that enables retention
// later, so its month exists before retention drops it.
func TestPostgresRetainedTerminationDoesNotResurrectADroppedMonth(t *testing.T) {
	dsn := worstCaseDatabase(t)
	p := worstCaseProvider(t, dsn, PostgresOptions{EvidenceRetention: "720h"})
	unretained := worstCaseProvider(t, dsn, PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "old-termination", ClusterUID: "physical"}
	key, _ := evidenceScopeKey(scope)
	now := time.Now().UTC().Truncate(time.Microsecond)
	finished := time.Date(now.Year(), now.Month()-3, 10, 12, 0, 0, 0, time.UTC)
	oldMonth := "evidence_event_month_" + finished.Format("200601")
	settle := func() {
		t.Helper()
		for i := 0; ; i++ {
			if i == 100 {
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
	state := func() (rows int, monthExists bool, revision uint64) {
		t.Helper()
		if err := p.db.QueryRow(`SELECT (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id='crash:old-termination'),to_regclass($2) IS NOT NULL`, key, oldMonth).Scan(&rows, &monthExists); err != nil {
			t.Fatal(err)
		}
		checkpoint, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		return rows, monthExists, checkpoint.PruneRevision
	}
	cycle := func(store EvidenceStore, n int) {
		t.Helper()
		at := now.Add(time.Duration(n-10) * time.Minute)
		records := []EvidenceRecord{oldTermination(finished, at), headsStatus("crash:current-1", 2, now.Add(-10*time.Minute)), headsStatus("crash:current-2", 5, now.Add(-10*time.Minute))}
		records[1].ObservedAt, records[2].ObservedAt = at, at
		if err := worstCommit(t.Context(), store, scope, fmt.Sprintf("cycle-%d", n), at, records); err != nil {
			t.Fatalf("cycle %d: %v", n, err)
		}
	}
	cycle(unretained, 0)
	if rows, exists, _ := state(); rows != 1 || !exists {
		t.Fatalf("the termination was not saved before retention: rows=%d month=%t", rows, exists)
	}
	settle()
	rows, exists, revision := state()
	if rows != 0 || exists || revision == 0 {
		t.Fatalf("first drop: rows=%d month=%t revision=%d", rows, exists, revision)
	}
	page, err := p.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "crash", ByObservation: true, Since: now.Add(-time.Hour), Until: now.Add(time.Hour), Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	cursor := page.Next
	appended, recreated := 0, 0
	for n := 1; n <= 5; n++ {
		cycle(p, n)
		rows, exists, _ := state()
		appended += rows
		if exists {
			recreated++
		}
		settle()
	}
	_, _, finalRevision := state()
	t.Logf("five captures after the drop: rows appended=%d month recreated=%d prune revision %d -> %d", appended, recreated, revision, finalRevision)
	if appended != 0 || recreated != 0 || finalRevision != revision {
		t.Errorf("an unchanged termination older than retention was re-saved %d times, recreated its dropped month %d times and invalidated the scope's cursors %d more times", appended, recreated, finalRevision-revision)
	}
	if cursor != "" {
		if _, err := p.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "crash", ByObservation: true, Since: now.Add(-time.Hour), Until: now.Add(time.Hour), Limit: 1, Cursor: cursor}); errors.Is(err, ErrEvidenceConflict) || errors.Is(err, ErrEvidenceCursorExpired) {
			t.Errorf("a cursor over recent crashes, nothing of which was pruned, was invalidated")
		}
	}
}

// TestPostgresRetentionDropDuringAnActiveSave runs retention while a save that
// supersedes a row in the month being dropped is in flight. The two must
// serialize: the save commits, the month is dropped afterwards, and no head
// points at a removed row.
func TestPostgresRetentionDropDuringAnActiveSave(t *testing.T) {
	dsn := worstCaseDatabase(t)
	p := worstCaseProvider(t, dsn, PostgresOptions{EvidenceRetention: "720h"})
	scope := EvidenceScope{OrgID: "default", ClusterID: "drop-during-save", ClusterUID: "physical"}
	key, _ := evidenceScopeKey(scope)
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := time.Date(now.Year(), now.Month()-3, 10, 12, 0, 0, 0, time.UTC)
	if err := worstCommit(t.Context(), p, scope, "c0", old, []EvidenceRecord{worstChange("change:aging", "1", old, old, 0), worstChange("change:expiring", "1", old, old, 0)}); err != nil {
		t.Fatal(err)
	}
	at := now.Add(-time.Minute)
	records := []EvidenceRecord{worstChange("change:aging", "2", at, at, 0)}
	for i := 0; i < 2000; i++ {
		records = append(records, worstChange(fmt.Sprintf("change:fresh-%04d", i), "1", at, at, 0))
	}
	checkpoint, _ := p.EvidenceCheckpoint(t.Context(), scope)
	batch := EvidenceBatch{ID: "c1", Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at, Epoch: "stable", Records: records}
	release := holdLastSaveStatement(t, dsn)
	saved := make(chan error, 1)
	go func() { saved <- p.CommitEvidence(context.Background(), scope, batch) }()
	heldSavePID(t, worstAdmin(t, dsn))
	type result struct {
		more bool
		err  error
	}
	compacted := make(chan result, 1)
	go func() {
		more, err := p.compactEvidenceBatch(context.Background())
		compacted <- result{more, err}
	}()
	select {
	case r := <-compacted:
		t.Fatalf("retention ran while a save held the month: more=%t err=%v", r.more, r.err)
	case <-time.After(time.Second):
	}
	release()
	if err := <-saved; err != nil {
		t.Fatalf("the save lost to retention: %v", err)
	}
	r := <-compacted
	if r.err != nil || !r.more {
		t.Fatalf("retention after the save: more=%t err=%v", r.more, r.err)
	}
	var oldRows, aging int
	if err := p.db.QueryRow(`SELECT (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_at<$2),(SELECT COUNT(*) FROM kubernetes_evidence_event_heads WHERE scope_key=$1 AND event_id='change:aging' AND event_at=$3)`, key, now.AddDate(0, -1, 0), at).Scan(&oldRows, &aging); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 || aging != 1 {
		t.Fatalf("after the drop: old rows=%d, aging head on its new row=%d", oldRows, aging)
	}
	worstJournalConsistent(t, p, scope)
}

// worstLog captures the standard logger for one test.
type worstLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *worstLog) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(data)
}

func (w *worstLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestPostgresMassChurnSaveCostStaysFlatAndWarns replaces 4,900 Pods on every
// cycle, just under the per-save record limit: each save carries 9,800 real
// created and deleted changes. Save time must not grow with the journal, and
// the amplification warning must fire as a log line, not as an error.
func TestPostgresMassChurnSaveCostStaysFlatAndWarns(t *testing.T) {
	if testing.Short() {
		t.Skip("mass churn: 20 saves of 9,800 changes")
	}
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "mass-churn", ClusterUID: "physical"}
	captured := &worstLog{}
	log.SetOutput(captured)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	const pods, cycles = 4900, 20
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	durations := make([]time.Duration, 0, cycles)
	failed := 0
	for cycle := 0; cycle < cycles; cycle++ {
		at := origin.Add(time.Duration(cycle) * 30 * time.Second)
		records := make([]EvidenceRecord, 0, 2*pods)
		for i := 0; i < pods; i++ {
			gone := worstChange(fmt.Sprintf("change:pod-%d-%04d-deleted", cycle, i), "deleted", at, at, 120)
			gone.Kind, gone.AuthorizationKind, gone.Rule = "Pod", "Pod", "deleted"
			born := worstChange(fmt.Sprintf("change:pod-%d-%04d-created", cycle+1, i), "created", at, at, 120)
			born.Kind, born.AuthorizationKind = "Pod", "Pod"
			records = append(records, gone, born)
		}
		started := time.Now()
		err := worstCommit(t.Context(), p, scope, fmt.Sprintf("churn-%d", cycle), at, records)
		took := time.Since(started)
		t.Logf("cycle %d: %d new events in %s (%v)", cycle, len(records), took, err)
		if err != nil {
			// The capture layer reports the failure and retries the same
			// capture; a later cycle shows whether the store recovers.
			failed++
			t.Errorf("cycle %d: a save of %d real changes failed on a journal of %d rows: %v", cycle, len(records), worstCounts(t, p, scope).rows, err)
			if err = worstCommit(t.Context(), p, scope, fmt.Sprintf("churn-%d", cycle), at, records); err != nil {
				t.Logf("cycle %d retry: %v", cycle, err)
				continue
			}
		}
		durations = append(durations, took)
	}
	if failed > 0 || len(durations) < cycles {
		t.Fatalf("%d of %d mass-churn saves failed", failed, cycles)
	}
	sorted := func(values []time.Duration) []time.Duration {
		out := append([]time.Duration(nil), values...)
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	early, late := sorted(durations[1:6]), sorted(durations[cycles-5:])
	worst := sorted(durations)[cycles-1]
	counts := worstCounts(t, p, scope)
	warnings := strings.Count(captured.String(), "storage: evidence saves keep appending most records")
	t.Logf("mass churn: early median=%s late median=%s worst=%s rows=%d warnings=%d", early[2], late[2], worst, counts.rows, warnings)
	if late[2] > 2*early[2]+100*time.Millisecond || worst > 10*time.Second {
		t.Errorf("save cost grew with churn: early=%s late=%s worst=%s", early[2], late[2], worst)
	}
	if counts.rows != cycles*2*pods {
		t.Errorf("rows=%d, want one per real change %d", counts.rows, cycles*2*pods)
	}
	if warnings != cycles/amplifiedWriteStreak {
		t.Errorf("amplification warnings=%d, want one per %d amplified saves", warnings, amplifiedWriteStreak)
	}
}

// TestPostgresNewEventsSaveWithoutReadingHistory saves batches of events the
// journal has never seen, as created and deleted changes are, onto a journal
// that already holds tens of thousands of other events. A new event has no
// head, so the save looks for its latest stored row. That lookup must probe
// the event's own key, not walk the scope's history, with and without planner
// statistics and with the planner's custom and generic plans.
func TestPostgresNewEventsSaveWithoutReadingHistory(t *testing.T) {
	dsn := worstCaseDatabase(t)
	p := worstCaseProvider(t, dsn, PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "new-events", ClusterUID: "physical"}
	key, _ := evidenceScopeKey(scope)
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	const perCycle, seeded, probe = 9800, 5, 9800
	newEvents := func(cycle, count int) []EvidenceRecord {
		at := origin.Add(time.Duration(cycle) * 30 * time.Second)
		records := make([]EvidenceRecord, count)
		for i := range records {
			records[i] = worstChange(fmt.Sprintf("change:new-%d-%05d", cycle, i), "created", at, at, 0)
		}
		return records
	}
	for cycle := 0; cycle < seeded; cycle++ {
		started := time.Now()
		if err := worstCommit(t.Context(), p, scope, fmt.Sprintf("seed-%d", cycle), origin.Add(time.Duration(cycle)*30*time.Second), newEvents(cycle, perCycle)); err != nil {
			t.Fatalf("seed %d: %v", cycle, err)
		}
		t.Logf("seed cycle %d: %d new events in %s", cycle, perCycle, time.Since(started))
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	trace := &scaleQueryTrace{}
	cfg.Tracer = trace
	traced := stdlib.OpenDB(*cfg)
	defer traced.Close()
	capture := func(records []EvidenceRecord) scaleStatement {
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
		_, err = writeEvidenceEvents(t.Context(), tx, key, records, 5*time.Minute)
		trace.enabled.Store(false)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range trace.queries {
			if strings.Contains(statement.SQL, "kubernetes_evidence_event_heads") && strings.Contains(statement.SQL, "WITH incoming") {
				return statement
			}
		}
		t.Fatal("write statement not captured")
		return scaleStatement{}
	}
	results := os.Getenv("HARNESS_RESULTS_DIR")
	type finding struct {
		Statistics, Mode string
		Journal          int
		ExecutionMS      float64
		Lookups          []headsPlanNode
		Problems         []string
	}
	var findings []finding
	for _, statistics := range []string{"none", "analyzed"} {
		if statistics == "analyzed" {
			if _, err := p.db.Exec(`ANALYZE kubernetes_evidence_events; ANALYZE kubernetes_evidence_event_heads`); err != nil {
				t.Fatal(err)
			}
		}
		statement := capture(newEvents(1000, probe))
		for _, mode := range []string{"custom", "generic"} {
			raw := headsExplain(t, dsn, statement, mode)
			nodes, err := headsPlanNodes(raw)
			if err != nil {
				t.Fatal(err)
			}
			var timing []struct {
				Execution float64 `json:"Execution Time"`
			}
			_ = json.Unmarshal(raw, &timing)
			f := finding{Statistics: statistics, Mode: mode, Journal: seeded * perCycle}
			if len(timing) > 0 {
				f.ExecutionMS = timing[0].Execution
			}
			for _, n := range nodes {
				// The fallback CTE is referenced once, so the planner inlines it
				// and its nodes carry no CTE name. Every executed journal scan
				// other than the key-based supersede and refresh is a lookup.
				if n.CTE == "superseded" || n.CTE == "refreshed" || !headsJournalRelation(n.Relation) || !strings.Contains(n.NodeType, "Scan") || n.Loops == 0 {
					continue
				}
				f.Lookups = append(f.Lookups, n)
				// Each lookup may touch its own event's rows only: a new event
				// has none, so anything read and filtered away is someone
				// else's history.
				// An index that does not lead with the event's key answers the
				// lookup by reading the scope's rows; the event condition is
				// then applied inside the index and not counted as filtered.
				if strings.Contains(n.NodeType, "Seq Scan") || n.Removed > n.Loops || !strings.Contains(n.Index, "scope_key_event_id") {
					f.Problems = append(f.Problems, fmt.Sprintf("%s on %s via %q: %v lookups not keyed by the event", n.NodeType, n.Relation, n.Index, n.Loops))
				}
			}
			findings = append(findings, f)
			t.Logf("statistics=%s mode=%s: %d journal lookup nodes, execution %.0f ms for %d new events on %d rows", statistics, mode, len(f.Lookups), f.ExecutionMS, probe, f.Journal)
			for _, n := range f.Lookups {
				t.Logf("  %s cte=%q %s via %q loops=%v rows=%v removed=%v", n.NodeType, n.CTE, n.Relation, n.Index, n.Loops, n.Rows, n.Removed)
			}
			for _, problem := range f.Problems {
				t.Errorf("new-event save with statistics %s, %s plan: %s", statistics, mode, problem)
			}
		}
	}
	if results != "" {
		scaleWriteJSON(t, filepath.Join(results, "new-event-lookup-plans.json"), findings)
	}
}
