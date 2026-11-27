package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// savePartsBatch is a capture of count brand-new change records.
func savePartsBatch(t *testing.T, store EvidenceStore, scope EvidenceScope, id string, at time.Time, count int) EvidenceBatch {
	t.Helper()
	records := make([]EvidenceRecord, count)
	for i := range records {
		records[i] = worstChange(fmt.Sprintf("change:%s-%05d", id, i), "created", at, at, 0)
	}
	cp, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	return EvidenceBatch{ID: id, Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: "stable", Records: records}
}

// savePartsVisible counts the change rows a reader sees for the scope.
func savePartsVisible(t *testing.T, store EvidenceStore, scope EvidenceScope, at time.Time) int {
	t.Helper()
	page, err := store.QueryEvidence(context.Background(), scope, EvidenceQuery{Type: "change", Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	return page.Total
}

// savePartsStatementTimeout makes the current save transaction time a
// statement out on the server, as an overloaded database does.
func savePartsStatementTimeout(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '50ms'`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_sleep(1)`)
	return err
}

// savePartsLog records the size of every save transaction and the size of the
// final one, which holds the read models and the checkpoint.
type savePartsLog struct {
	mu    sync.Mutex
	sizes []int
}

func (log *savePartsLog) add(size int) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.sizes = append(log.sizes, size)
}

// A capture at the record limit is saved in parts of at most the save chunk,
// each its own short transaction, with the lease confirmed before each. The
// final transaction writes no journal records. Until it lands, readers see
// none of the capture.
func TestPostgresLargeCaptureSavesInBoundedParts(t *testing.T) {
	if testing.Short() {
		t.Skip("saves a capture at the record limit")
	}
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "save-parts", ClusterUID: "physical"}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	const total = MaxEvidenceBatchRecords
	batch := savePartsBatch(t, p, scope, "large", at, total)
	var mu sync.Mutex
	var starts []time.Time
	var sizes []int
	visibleMidway := -1
	p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
		mu.Lock()
		defer mu.Unlock()
		starts, sizes = append(starts, time.Now()), append(sizes, size)
		if part == total/defaultEvidenceSaveChunk/2 {
			visibleMidway = savePartsVisible(t, p, scope, at)
		}
		return nil
	}
	leaseChecks, fenced := 0, 0
	batch.KeepLease = func(context.Context) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		leaseChecks++
		return nil, nil
	}
	batch.Fence = func(ctx context.Context, transaction func() error) error {
		mu.Lock()
		fenced++
		mu.Unlock()
		return transaction()
	}
	started := time.Now()
	if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	starts = append(starts, time.Now())
	longest := time.Duration(0)
	for i := 1; i < len(starts); i++ {
		longest = max(longest, starts[i].Sub(starts[i-1]))
	}
	t.Logf("%d records in %d writing transactions, %s in total, longest transaction %s", total, len(sizes), elapsed, longest)
	parts := total / defaultEvidenceSaveChunk
	if len(sizes) != parts+1 || sizes[len(sizes)-1] != 0 {
		t.Fatalf("transaction sizes %v: want %d parts and a final transaction without journal records", sizes, parts)
	}
	for _, size := range sizes {
		if size > defaultEvidenceSaveChunk {
			t.Fatalf("a save transaction wrote %d records, more than the chunk", size)
		}
	}
	// One attempt to save everything at once that finds it too large, every
	// part, and the final transaction: each confirms the lease and is fenced.
	if want := parts + 2; leaseChecks != want || fenced != want {
		t.Fatalf("the lease was confirmed %d times and the fence taken %d times for %d transactions", leaseChecks, fenced, want)
	}
	if longest > 5*time.Second {
		t.Fatalf("the longest save transaction took %s, not well inside the 20 s save bound", longest)
	}
	if visibleMidway != 0 {
		t.Fatalf("readers saw %d rows of a capture saved halfway", visibleMidway)
	}
	if visible := savePartsVisible(t, p, scope, at); visible <= 1000 {
		t.Fatalf("after the save readers see %d rows", visible)
	}
	if counts := worstCounts(t, p, scope); counts.rows != total || counts.heads != total {
		t.Fatalf("journal after the save: %+v", counts)
	}
	if latest, _ := p.EvidenceCheckpoint(t.Context(), scope); latest.ID != batch.ID {
		t.Fatalf("checkpoint %q after the save, want %q", latest.ID, batch.ID)
	}
	worstJournalConsistent(t, p, scope)
}

// Every part is bounded by records and by payload bytes and holds only the
// producer's records; heavy payloads end a part early. The issue records a
// save derives are named after the capture, so only the final transaction
// that commits it writes them, never more than the batch's change limit.
func TestPostgresEveryPartIsBoundedByRecordsAndBytes(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	p.evidenceSaveChunk = 100
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	t.Run("issues", func(t *testing.T) {
		scope := EvidenceScope{OrgID: "default", ClusterID: "parts-issues", ClusterUID: "physical"}
		const producer, limit = 50, 120
		batch := savePartsBatch(t, p, scope, "issues", at, producer)
		batch.ChangeLimit = limit
		for i := 0; i < 300; i++ {
			raw, _ := json.Marshal(map[string]any{"rule": "pod.crash_loop", "severity": "warning", "index": i})
			batch.Issues = append(batch.Issues, EvidenceIssue{Key: fmt.Sprintf("issue-%03d", i), Namespace: "shop", Kind: "Pod", UID: fmt.Sprintf("pod-%03d", i), Rule: "pod.crash_loop", AuthorizationKind: "Pod", LastSeen: at, Payload: raw})
		}
		log := &savePartsLog{}
		p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
			log.add(size)
			return nil
		}
		defer func() { p.evidenceChunkFault = nil }()
		if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
		t.Logf("transaction sizes %v", log.sizes)
		if len(log.sizes) < 2 {
			t.Fatalf("transaction sizes %v: want parts and a final transaction", log.sizes)
		}
		parts, final := log.sizes[:len(log.sizes)-1], log.sizes[len(log.sizes)-1]
		inParts := 0
		for _, size := range parts {
			if size > 100 {
				t.Fatalf("transaction sizes %v: a part exceeded the chunk", log.sizes)
			}
			inParts += size
		}
		if inParts != producer {
			t.Fatalf("transaction sizes %v: the parts wrote %d records, want only the producer's %d", log.sizes, inParts, producer)
		}
		if final != limit {
			t.Fatalf("transaction sizes %v: the final wrote %d issue records, want the change limit %d of the 300 openings", log.sizes, final, limit)
		}
		if latest, _ := p.EvidenceCheckpoint(t.Context(), scope); latest.ID != batch.ID {
			t.Fatalf("checkpoint %q after the save", latest.ID)
		}
		if counts := worstCounts(t, p, scope); counts.rows != producer+limit {
			t.Fatalf("journal after the save: %+v, want the %d producer records and %d openings", counts, producer, limit)
		}
		worstJournalConsistent(t, p, scope)
	})

	t.Run("bytes", func(t *testing.T) {
		scope := EvidenceScope{OrgID: "default", ClusterID: "parts-bytes", ClusterUID: "physical"}
		batch := savePartsBatch(t, p, scope, "bytes", at, 0)
		for i := 0; i < 90; i++ {
			batch.Records = append(batch.Records, worstChange(fmt.Sprintf("change:heavy-%03d", i), "1", at, at, 60<<10))
		}
		log := &savePartsLog{}
		p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
			log.add(size)
			return nil
		}
		defer func() { p.evidenceChunkFault = nil }()
		if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
		t.Logf("transaction sizes %v", log.sizes)
		perPart := maxEvidenceSavePartBytes / (60 << 10)
		for _, size := range log.sizes {
			if size > perPart {
				t.Fatalf("transaction sizes %v: a part held more than %d records of 60 KiB", log.sizes, perPart)
			}
		}
		if counts := worstCounts(t, p, scope); counts.rows != 90 {
			t.Fatalf("journal after the save: %+v", counts)
		}
	})
}

// A capture that fits one transaction and times out there, by a real server
// statement timeout, saves its journal records in parts of half the size and
// answers the timeout without running its final transaction again, so the
// caller backs off. The next attempt's final transaction adopts the parts,
// and every row lands once.
func TestPostgresTimedOutSingleTransactionSaveKeepsItsPartsAndAnswersTheTimeout(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "single-timeout", ClusterUID: "physical"}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	batch := savePartsBatch(t, p, scope, "single", at, 400)
	log := &savePartsLog{}
	failed := false
	var answer error
	p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
		log.add(size)
		if !failed {
			failed = true
			answer = savePartsStatementTimeout(ctx, tx)
			return answer
		}
		return nil
	}
	err := p.CommitEvidence(t.Context(), scope, batch)
	var pgErr *pgconn.PgError
	if !errors.As(answer, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("the injected timeout answered %v, want SQLSTATE 57014", answer)
	}
	if !evidenceSaveTimedOut(err) {
		t.Fatalf("the save answered %v, want the final transaction's timeout", err)
	}
	t.Logf("transaction sizes %v", log.sizes)
	if len(log.sizes) != 3 || log.sizes[0] != 400 || log.sizes[1] != defaultEvidenceSaveChunk/2 || log.sizes[2] != 400-defaultEvidenceSaveChunk/2 {
		t.Fatalf("transaction sizes %v: want the timed-out final, then the journal in parts of half the save chunk, and no second final", log.sizes)
	}
	if counts := worstCounts(t, p, scope); counts.rows != 400 || counts.distinct != 400 {
		t.Fatalf("journal after the timed-out save: %+v", counts)
	}
	if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	if counts := worstCounts(t, p, scope); counts.rows != 400 || counts.distinct != 400 {
		t.Fatalf("journal after the next attempt: %+v", counts)
	}
	worstJournalConsistent(t, p, scope)
}

// A part that times out (a real server statement timeout) is retried alone at
// half the size; parts already saved are not written again. A part that fails
// otherwise fails the save, leaves the checkpoint and readers where they were,
// and the retried capture resumes without a duplicate row. A save whose own
// deadline has passed stops instead of retrying smaller.
func TestPostgresTimedOutSavePartResumesWithoutDuplicates(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	p.evidenceSaveChunk = 100
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	const records = 1000

	t.Run("statement timeout halves the part", func(t *testing.T) {
		scope := EvidenceScope{OrgID: "default", ClusterID: "save-timeout", ClusterUID: "physical"}
		batch := savePartsBatch(t, p, scope, "timeout", at, records)
		log := &savePartsLog{}
		failed := false
		p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
			log.add(size)
			if part == 3 && !failed {
				failed = true
				return savePartsStatementTimeout(ctx, tx)
			}
			return nil
		}
		defer func() { p.evidenceChunkFault = nil }()
		if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
		t.Logf("transaction sizes %v", log.sizes)
		if len(log.sizes) < 5 || log.sizes[2] != 100 || log.sizes[3] != 50 {
			t.Fatalf("transaction sizes %v: the timed-out part was not retried alone at half the size", log.sizes)
		}
		for _, size := range log.sizes[3:] {
			if size > 50 {
				t.Fatalf("transaction sizes %v: a part after the timeout went back to full size", log.sizes)
			}
		}
		if counts := worstCounts(t, p, scope); counts.rows != records || counts.distinct != records {
			t.Fatalf("journal after a timed-out part: %+v", counts)
		}
		worstJournalConsistent(t, p, scope)
	})

	t.Run("failed part resumes on retry", func(t *testing.T) {
		scope := EvidenceScope{OrgID: "default", ClusterID: "save-resume", ClusterUID: "physical"}
		batch := savePartsBatch(t, p, scope, "resume", at, records)
		broken := errors.New("connection reset by peer")
		p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
			if part == 5 {
				return broken
			}
			return nil
		}
		if err := p.CommitEvidence(t.Context(), scope, batch); !errors.Is(err, broken) {
			t.Fatalf("a failed part answered %v", err)
		}
		if visible := savePartsVisible(t, p, scope, at); visible != 0 {
			t.Fatalf("readers saw %d rows of a capture whose save failed", visible)
		}
		if latest, _ := p.EvidenceCheckpoint(t.Context(), scope); latest.ID == batch.ID {
			t.Fatal("the checkpoint advanced past a capture whose save failed")
		}
		if partial := worstCounts(t, p, scope); partial.rows != 400 {
			t.Fatalf("parts before the failed one wrote %d rows, want 400", partial.rows)
		}
		p.evidenceChunkFault = nil
		if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
		if counts := worstCounts(t, p, scope); counts.rows != records || counts.distinct != records || counts.heads != records {
			t.Fatalf("journal after the retried capture: %+v", counts)
		}
		if latest, _ := p.EvidenceCheckpoint(t.Context(), scope); latest.ID != batch.ID {
			t.Fatalf("checkpoint %q after the retry, want %q", latest.ID, batch.ID)
		}
		if visible := savePartsVisible(t, p, scope, at); visible != records {
			t.Fatalf("after the retry readers see %d rows, want %d", visible, records)
		}
		worstJournalConsistent(t, p, scope)
		// The same capture offered again is already saved and writes nothing.
		before := worstCounts(t, p, scope)
		if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
		if after := worstCounts(t, p, scope); after != before {
			t.Fatalf("a saved capture offered again wrote rows: %+v -> %+v", before, after)
		}
	})

	t.Run("only the final transaction fails", func(t *testing.T) {
		scope := EvidenceScope{OrgID: "default", ClusterID: "save-final", ClusterUID: "physical"}
		batch := savePartsBatch(t, p, scope, "final", at, records)
		broken := errors.New("connection reset by peer")
		p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
			if size == 0 {
				return broken
			}
			return nil
		}
		if err := p.CommitEvidence(t.Context(), scope, batch); !errors.Is(err, broken) {
			t.Fatalf("a failed final transaction answered %v", err)
		}
		if visible := savePartsVisible(t, p, scope, at); visible != 0 {
			t.Fatalf("readers saw %d rows before the checkpoint landed", visible)
		}
		p.evidenceChunkFault = nil
		// Every part was saved by the failed attempt, so the retry writes no
		// journal record; the checkpoint must still cover all of them.
		if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
		if visible := savePartsVisible(t, p, scope, at); visible != records {
			t.Fatalf("after the retry readers see %d rows, want %d", visible, records)
		}
		if counts := worstCounts(t, p, scope); counts.rows != records {
			t.Fatalf("journal after the retry: %+v", counts)
		}
	})

	t.Run("a save past its deadline stops", func(t *testing.T) {
		scope := EvidenceScope{OrgID: "default", ClusterID: "save-deadline", ClusterUID: "physical"}
		batch := savePartsBatch(t, p, scope, "deadline", at, records)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		log := &savePartsLog{}
		p.evidenceChunkFault = func(partCtx context.Context, tx *sql.Tx, part, size int) error {
			log.add(size)
			if part == 2 {
				// The save's own deadline passes while this part runs.
				cancel()
				return fmt.Errorf("save part: %w", context.DeadlineExceeded)
			}
			return nil
		}
		defer func() { p.evidenceChunkFault = nil }()
		if err := p.CommitEvidence(ctx, scope, batch); err == nil {
			t.Fatal("a save past its deadline reported success")
		}
		if len(log.sizes) != 2 {
			t.Fatalf("transaction sizes %v: a save past its deadline kept retrying smaller", log.sizes)
		}
	})
}

// A save part whose lease is gone writes nothing, and the save stops there.
func TestPostgresSavePartsStopWhenTheLeaseIsLost(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	p.evidenceSaveChunk = 100
	scope := EvidenceScope{OrgID: "default", ClusterID: "save-lease", ClusterUID: "physical"}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	batch := savePartsBatch(t, p, scope, "lease", at, 1000)
	lost := errors.New("lease lost")
	calls := 0
	batch.KeepLease = func(context.Context) ([]byte, error) {
		calls++
		// The first call is the attempt to save everything at once.
		if calls == 4 {
			return nil, lost
		}
		return nil, nil
	}
	if err := p.CommitEvidence(t.Context(), scope, batch); !errors.Is(err, lost) {
		t.Fatalf("a save without its lease answered %v", err)
	}
	if counts := worstCounts(t, p, scope); counts.rows != 200 {
		t.Fatalf("a save stopped by its lease wrote %d rows, want the 200 written before", counts.rows)
	}
}

// Another replica takes the lease between two parts of a save and saves its
// own capture. The old owner's next part is refused by the lease guard; the
// journal holds every change once and the checkpoint never moves back.
func TestPostgresLeaseTakeoverBetweenSavePartsFencesTheOldOwner(t *testing.T) {
	dsn := worstCaseDatabase(t)
	old := worstCaseProvider(t, dsn, PostgresOptions{})
	successor := worstCaseProvider(t, dsn, PostgresOptions{})
	old.evidenceSaveChunk = 100
	scope := EvidenceScope{OrgID: "default", ClusterID: "save-takeover", ClusterUID: "physical"}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	guardKey := "kubernetes/inventory/save-takeover"
	oldLease, newLease := []byte(`{"owner":"old"}`), []byte(`{"owner":"successor"}`)
	if err := old.WriteBlob(guardKey, oldLease); err != nil {
		t.Fatal(err)
	}
	stale := savePartsBatch(t, old, scope, "stale", at, 1000)
	stale.GuardKey, stale.GuardExpected = guardKey, oldLease
	var takeoverErr error
	took := false
	old.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
		if part != 3 || took {
			return nil
		}
		took = true
		// The successor claims the lease and saves a newer capture that
		// re-offers the same changes, as the next capture does.
		won, err := successor.CompareAndSwapBlob(guardKey, oldLease, newLease)
		if err != nil || !won {
			takeoverErr = fmt.Errorf("takeover: won=%t err=%v", won, err)
			return nil
		}
		next := stale
		next.ID, next.At, next.GuardExpected = "successor", at.Add(time.Minute), newLease
		takeoverErr = successor.CommitEvidence(context.Background(), scope, next)
		return nil
	}
	err := old.CommitEvidence(t.Context(), scope, stale)
	if takeoverErr != nil {
		t.Fatalf("the successor's save failed: %v", takeoverErr)
	}
	if !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("the old owner's part after the takeover answered %v, want the lease conflict", err)
	}
	checkpoint, _ := old.EvidenceCheckpoint(t.Context(), scope)
	if checkpoint.ID != "successor" || checkpoint.At.Before(at.Add(time.Minute)) {
		t.Fatalf("checkpoint %q at %s after the takeover, want the successor's", checkpoint.ID, checkpoint.At)
	}
	if counts := worstCounts(t, old, scope); counts.rows != 1000 || counts.distinct != 1000 {
		t.Fatalf("journal after the takeover: %+v, want each of the 1000 changes once", counts)
	}
	worstJournalConsistent(t, old, scope)
	// The old owner offers its capture again: refused, nothing written, and
	// the checkpoint stays the successor's.
	before := worstCounts(t, old, scope)
	if err := old.CommitEvidence(t.Context(), scope, stale); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("the old owner's retry answered %v", err)
	}
	if after := worstCounts(t, old, scope); after != before {
		t.Fatalf("the old owner's retry wrote rows: %+v -> %+v", before, after)
	}
	if latest, _ := old.EvidenceCheckpoint(t.Context(), scope); latest.ID != "successor" {
		t.Fatalf("checkpoint moved back to %q", latest.ID)
	}
}

// savePartsHistory reads a scope's whole change history page by page, as an
// unfiltered list does. It answers the first page's total and every item, by
// ID, with its stored value.
func savePartsHistory(t *testing.T, store EvidenceStore, scope EvidenceScope, since time.Time) (int, map[string]string) {
	t.Helper()
	query := EvidenceQuery{Type: "change", Since: since, Until: time.Now().UTC(), Limit: 500}
	total, items := -1, map[string]string{}
	for {
		page, err := store.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatal(err)
		}
		if total < 0 {
			total = page.Total
		}
		for _, item := range page.Items {
			var payload struct{ Value string }
			if err = json.Unmarshal(item.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if _, seen := items[item.ID]; seen {
				t.Fatalf("change %s listed twice", item.ID)
			}
			items[item.ID] = payload.Value
		}
		if page.Next == "" {
			return total, items
		}
		query.Cursor = page.Next
	}
}

// A save abandoned after some of its parts leaves their journal rows and its
// in-progress marker behind. Until the next save lands, readers see none of
// those rows, and the unfiltered total is counted from what they do see, not
// from the scope's counters, which the parts already moved. The next save
// adopts the parts: its checkpoint covers them, so they read as history, each
// once, beside the new capture, and the exact total from the counters matches
// what a reader lists.
func TestPostgresNextSaveAdoptsTheRowsOfAnAbandonedSave(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{EvidenceRetention: "0"})
	p.evidenceSaveChunk = 100
	scope := EvidenceScope{OrgID: "default", ClusterID: "save-adopted", ClusterUID: "physical"}
	key, err := evidenceScopeKey(scope)
	if err != nil {
		t.Fatal(err)
	}
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	since := origin.Add(-time.Hour)

	// The cluster's first capture: 50 changes, saved in one transaction.
	steady := savePartsBatch(t, p, scope, "steady", origin, 50)
	if err = p.CommitEvidence(t.Context(), scope, steady); err != nil {
		t.Fatal(err)
	}

	// The next capture carries new versions of those 50 changes and 950 new
	// ones. It is abandoned when its fourth part is rejected.
	at := origin.Add(time.Minute)
	abandoned := savePartsBatch(t, p, scope, "abandoned", at, 1000)
	for i := range steady.Records {
		abandoned.Records[i] = worstChange(steady.Records[i].ID, "revised", at, at, 0)
	}
	rejected := errors.New("part rejected")
	p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
		if part == 4 {
			return rejected
		}
		return nil
	}
	if err = p.CommitEvidence(t.Context(), scope, abandoned); !errors.Is(err, rejected) {
		t.Fatalf("the abandoned save answered %v", err)
	}
	p.evidenceChunkFault = nil
	var orphans int
	var orphanHigh int64
	if err = p.db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(e.sequence),0) FROM kubernetes_evidence_events e JOIN kubernetes_evidence_scopes s ON s.scope_key=e.scope_key WHERE e.scope_key=$1 AND e.sequence>s.high_sequence`, key).Scan(&orphans, &orphanHigh); err != nil {
		t.Fatal(err)
	}
	if orphans != 300 {
		t.Fatalf("the abandoned save left %d rows past the checkpoint, want its three parts of 100", orphans)
	}
	checkpoint, err := p.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.ID != steady.ID || checkpoint.Saving != abandoned.ID || checkpoint.SavingHigh != uint64(orphanHigh) {
		t.Fatalf("checkpoint after the abandoned save: id %q saving %q up to %d, want %q saving %q up to %d", checkpoint.ID, checkpoint.Saving, checkpoint.SavingHigh, steady.ID, abandoned.ID, orphanHigh)
	}
	var counted int
	if err = p.db.QueryRow(`SELECT COALESCE(SUM(value::bigint),0) FROM kubernetes_evidence_scopes s,jsonb_each_text(s.event_counts) WHERE s.scope_key=$1`, key).Scan(&counted); err != nil {
		t.Fatal(err)
	}
	total, items := savePartsHistory(t, p, scope, since)
	if total != len(steady.Records) || len(items) != len(steady.Records) {
		t.Fatalf("readers see %d changes with a total of %d while the save is abandoned, want the first capture's %d", len(items), total, len(steady.Records))
	}
	for id, value := range items {
		if value != "created" {
			t.Fatalf("change %s reads %q before the next save, want the first capture's version", id, value)
		}
	}
	if counted <= total {
		t.Fatalf("the scope's counters hold %d changes, want more than the %d readers see: the parts moved them", counted, total)
	}

	// The next capture brings 200 other changes and does not re-offer the
	// abandoned ones.
	pinned := checkpoint.HighSequence
	next := savePartsBatch(t, p, scope, "next", at.Add(time.Minute), 200)
	if err = p.CommitEvidence(t.Context(), scope, next); err != nil {
		t.Fatal(err)
	}
	checkpoint, err = p.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.ID != next.ID || checkpoint.Saving != "" || checkpoint.SavingHigh != 0 || checkpoint.HighSequence < uint64(orphanHigh) {
		t.Fatalf("checkpoint after the next save: id %q saving %q up to %d, pinned to %d; want %q pinned past %d", checkpoint.ID, checkpoint.Saving, checkpoint.SavingHigh, checkpoint.HighSequence, next.ID, orphanHigh)
	}
	revised, adoptedNew := 0, 0
	if err = p.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE payload->>'value'='revised'),COUNT(*) FILTER (WHERE payload->>'value'='created') FROM kubernetes_evidence_events WHERE scope_key=$1 AND sequence>$2 AND sequence<=$3`, key, int64(pinned), orphanHigh).Scan(&revised, &adoptedNew); err != nil {
		t.Fatal(err)
	}
	if revised+adoptedNew != orphans {
		t.Fatalf("the abandoned parts hold %d revised and %d new changes, want %d rows", revised, adoptedNew, orphans)
	}
	total, items = savePartsHistory(t, p, scope, since)
	want := len(steady.Records) + adoptedNew + len(next.Records)
	if len(items) != want || total != want {
		t.Fatalf("after the next save readers list %d changes with an exact total of %d, want %d: %d first, %d adopted new, %d next", len(items), total, want, len(steady.Records), adoptedNew, len(next.Records))
	}
	versions := 0
	for _, record := range steady.Records {
		if items[record.ID] == "revised" {
			versions++
		}
	}
	if versions != revised {
		t.Fatalf("%d of the first capture's changes read the abandoned save's version, want the %d it wrote", versions, revised)
	}
	for _, record := range next.Records {
		if _, ok := items[record.ID]; !ok {
			t.Fatalf("the next capture's change %s is missing", record.ID)
		}
	}
	worstJournalConsistent(t, p, scope)
}
