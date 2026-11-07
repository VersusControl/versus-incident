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

// Every part is bounded by records and by payload bytes, the final one too:
// lifecycle records derived from issues are saved in parts like any other
// record, and heavy payloads end a part early.
func TestPostgresEveryPartIsBoundedByRecordsAndBytes(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	p.evidenceSaveChunk = 100
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	t.Run("issues", func(t *testing.T) {
		scope := EvidenceScope{OrgID: "default", ClusterID: "parts-issues", ClusterUID: "physical"}
		batch := savePartsBatch(t, p, scope, "issues", at, 50)
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
		if len(log.sizes) < 4 || log.sizes[len(log.sizes)-1] != 0 {
			t.Fatalf("transaction sizes %v: want the issue lifecycle records saved in parts and a final transaction without journal records", log.sizes)
		}
		for _, size := range log.sizes {
			if size > 100 {
				t.Fatalf("transaction sizes %v: a part exceeded the chunk", log.sizes)
			}
		}
		if latest, _ := p.EvidenceCheckpoint(t.Context(), scope); latest.ID != batch.ID {
			t.Fatalf("checkpoint %q after the save", latest.ID)
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
// statement timeout, is saved again in parts of half the size; parts after it
// stay small, and every row lands once.
func TestPostgresTimedOutSingleTransactionSaveIsSplit(t *testing.T) {
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
	if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(answer, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("the injected timeout answered %v, want SQLSTATE 57014", answer)
	}
	t.Logf("transaction sizes %v", log.sizes)
	if len(log.sizes) < 3 || log.sizes[0] != 400 || log.sizes[1] != defaultEvidenceSaveChunk/2 || log.sizes[len(log.sizes)-1] != 0 {
		t.Fatalf("transaction sizes %v: the timed-out transaction was not saved again in parts of half the size", log.sizes)
	}
	if counts := worstCounts(t, p, scope); counts.rows != 400 || counts.distinct != 400 {
		t.Fatalf("journal after the split save: %+v", counts)
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
