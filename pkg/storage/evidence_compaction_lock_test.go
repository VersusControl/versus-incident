package storage

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VersusControl/versus-incident/pkg/logging"
)

// holdSharedEvidenceLock holds the partition lock shared on its own
// connection, as a save in progress does, until the returned release runs.
func holdSharedEvidenceLock(t *testing.T, p *postgresProvider) func() {
	t.Helper()
	holder, err := p.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holder.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock_shared(6216456209771226)`); err != nil {
		_ = holder.Rollback()
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = holder.Rollback()
		}
	}
	t.Cleanup(release)
	return release
}

// compactionLockProvider opens an isolated database with retention on, so
// compaction runs, and maintenance stopped, so the test drives every round.
func compactionLockProvider(t *testing.T) *postgresProvider {
	t.Helper()
	return worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{EvidenceRetention: "168h"})
}

// timedCompaction runs one compaction round, bounded so a round that waits
// for the lock fails the test instead of hanging it.
func timedCompaction(t *testing.T, p *postgresProvider) (bool, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	more, err := p.compactEvidenceBatch(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("compaction with the lock held answered %v after %s, want a skipped round", err, elapsed)
	}
	return more, elapsed
}

func TestPostgresCompactionSkipsWhileSavesHoldTheLock(t *testing.T) {
	p := compactionLockProvider(t)
	holdSharedEvidenceLock(t, p)

	more, elapsed := timedCompaction(t, p)
	if more || elapsed > time.Second {
		t.Fatalf("compaction with a save holding the lock: more=%v after %s, want a skipped round at once", more, elapsed)
	}

	// A save on a third connection, made while compaction keeps trying, is
	// never queued behind an exclusive request.
	stop := make(chan struct{})
	rounds := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				rounds <- nil
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := p.compactEvidenceBatch(ctx)
			cancel()
			if err != nil {
				rounds <- err
				return
			}
		}
	}()
	scope := EvidenceScope{OrgID: "default", ClusterID: "compaction-lock", ClusterUID: "physical"}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := p.CommitEvidence(ctx, scope, evidenceTestBatch("first", "", at, true, true))
	saved := time.Since(start)
	close(stop)
	if roundErr := <-rounds; roundErr != nil {
		t.Fatalf("compaction round failed beside a save: %v", roundErr)
	}
	if err != nil {
		t.Fatalf("save beside compaction failed after %s: %v", saved, err)
	}
	if saved > 3*time.Second {
		t.Fatalf("save beside compaction took %s, want it never to wait for compaction", saved)
	}
}

func TestPostgresCompactionWaitsBrieflyAfterLongSkip(t *testing.T) {
	p := compactionLockProvider(t)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	p.evidenceClock = func() time.Time { return now }
	release := holdSharedEvidenceLock(t, p)

	// The first round starts the interval and only tries the lock.
	if more, elapsed := timedCompaction(t, p); more || elapsed > time.Second {
		t.Fatalf("first round: more=%v after %s, want a skipped try", more, elapsed)
	}
	if !p.compactionWaitedAt.IsZero() {
		t.Fatal("the first round waited for the lock")
	}

	// Thirty minutes without the lock: one attempt that waits, for about
	// 200ms, and then gives up without an error.
	now = now.Add(compactionWaitAfter)
	more, elapsed := timedCompaction(t, p)
	if more || elapsed < 150*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("bounded attempt: more=%v after %s, want a skipped round after about 200ms", more, elapsed)
	}
	if !p.compactionWaitedAt.Equal(now) {
		t.Fatalf("bounded attempt recorded at %s, want %s", p.compactionWaitedAt, now)
	}

	// The next rounds only try again until another interval has passed.
	now = now.Add(time.Minute)
	if more, elapsed := timedCompaction(t, p); more || elapsed > time.Second {
		t.Fatalf("round after the attempt: more=%v after %s, want a skipped try", more, elapsed)
	}
	if !p.compactionWaitedAt.Equal(now.Add(-time.Minute)) {
		t.Fatal("a round inside the interval waited for the lock again")
	}

	// Once the lock is free the waiting attempt takes it and records it.
	release()
	now = now.Add(compactionWaitAfter)
	timedCompaction(t, p)
	if !p.compactionLockedAt.Equal(now) {
		t.Fatalf("compaction last held the lock at %s, want %s", p.compactionLockedAt, now)
	}
}

func TestPostgresSaveQueuedBehindBoundedAttemptWaitsBriefly(t *testing.T) {
	p := compactionLockProvider(t)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	p.evidenceClock = func() time.Time { return now }
	holdSharedEvidenceLock(t, p)
	timedCompaction(t, p)
	now = now.Add(compactionWaitAfter)

	type round struct {
		more bool
		err  error
	}
	compacted := make(chan round, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		more, err := p.compactEvidenceBatch(ctx)
		compacted <- round{more, err}
	}()
	// Start the save only once compaction's exclusive request is queued, so
	// the save's shared request queues behind it.
	queued := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && !queued; time.Sleep(2 * time.Millisecond) {
		if err := p.db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND mode='ExclusiveLock' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`).Scan(&queued); err != nil {
			t.Fatal(err)
		}
	}
	if !queued {
		t.Fatal("compaction's bounded attempt was never seen waiting for the lock")
	}
	// Watch the save's shared lock request in pg_locks: while it waits, the
	// database reports when the wait began, so the longest wait seen is the
	// save's time in the queue, short by at most one poll.
	stop, watched := make(chan struct{}), make(chan time.Duration, 1)
	go func() {
		var longest time.Duration
		for {
			select {
			case <-stop:
				watched <- longest
				return
			default:
			}
			var waited sql.NullFloat64
			if err := p.db.QueryRowContext(context.Background(), `SELECT MAX(EXTRACT(EPOCH FROM clock_timestamp()-waitstart)) FROM pg_locks WHERE locktype='advisory' AND mode='ShareLock' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&waited); err == nil && waited.Valid {
				longest = max(longest, time.Duration(waited.Float64*float64(time.Second)))
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	scope := EvidenceScope{OrgID: "default", ClusterID: "queued-save", ClusterUID: "physical"}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := p.CommitEvidence(ctx, scope, evidenceTestBatch("first", "", at, true, true))
	saved := time.Since(start)
	close(stop)
	waited := <-watched
	r := <-compacted
	if r.err != nil || r.more {
		t.Fatalf("bounded attempt answered more=%v err=%v, want a skipped round", r.more, r.err)
	}
	if err != nil {
		t.Fatalf("save behind the bounded attempt failed after %s: %v", saved, err)
	}
	t.Logf("save behind the bounded attempt waited %s for the partition lock (save took %s)", waited, saved)
	if waited == 0 {
		t.Fatal("the save was never seen waiting behind the bounded attempt")
	}
	if waited > 250*time.Millisecond {
		t.Fatalf("save waited %s for the partition lock, want about 200ms at most", waited)
	}
}

func TestPostgresSkippedCompactionWarnsFromStoredMonths(t *testing.T) {
	p := compactionLockProvider(t)
	old := time.Now().UTC().AddDate(0, -3, 0)
	if _, err := p.db.ExecContext(t.Context(), `SELECT versus_ensure_evidence_partition(to_regclass('kubernetes_evidence_events'),$1)`, old); err != nil {
		t.Fatal(err)
	}
	holdSharedEvidenceLock(t, p)
	buf := captureStorageLog(t, "info")
	// A provider that has just started still warns: the stored months, not
	// its uptime, show retention is behind.
	timedCompaction(t, p)
	lines := logLines(buf, "evidence retention")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN ") || !strings.Contains(lines[0], "behind") {
		t.Fatalf("skipped round with a month long past retention logged %q, want one warning", lines)
	}
	timedCompaction(t, p)
	if lines := logLines(buf, "evidence retention"); len(lines) != 0 {
		t.Fatalf("second skipped round logged %q inside the throttle window", lines)
	}
}

// captureStorageLog sends the standard logger to a buffer at the given level
// for one test. Tests that use it do not run in parallel.
func captureStorageLog(t *testing.T, level string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	writer, flags, prefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	previous := logging.LevelName(logging.Level())
	logging.Setup(level)
	t.Cleanup(func() {
		log.SetOutput(writer)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
		logging.Setup(previous)
	})
	return &buf
}

// logLines returns the captured lines that contain about, and empties the
// buffer.
func logLines(buf *bytes.Buffer, about string) []string {
	lines := []string{}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, about) {
			lines = append(lines, line)
		}
	}
	buf.Reset()
	return lines
}

func TestEvidenceRetentionLagFromOldestMonth(t *testing.T) {
	cutoff := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		oldest string
		want   time.Duration
	}{
		{"", 0},
		{"202611", 0},
		{"202610", 11 * 24 * time.Hour},
		{"202609", 42 * 24 * time.Hour},
		{"bogus", 0},
	} {
		if got := evidenceRetentionLag(test.oldest, cutoff); got != test.want {
			t.Errorf("lag of oldest month %q = %s, want %s", test.oldest, got, test.want)
		}
	}
}

func TestRetentionWarningFollowsStoredMonths(t *testing.T) {
	buf := captureStorageLog(t, "info")
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	p := &postgresProvider{evidenceClock: func() time.Time { return now }}

	// Caught up, or behind by less than a day: nothing by default.
	p.noteRetentionLag(0)
	p.noteRetentionLag(0)
	p.noteRetentionLag(23 * time.Hour)
	if lines := logLines(buf, "evidence retention"); len(lines) != 0 {
		t.Fatalf("retention within a day logged %q", lines)
	}

	// More than a day behind: one warning naming what to check.
	p.noteRetentionLag(25 * time.Hour)
	lines := logLines(buf, "evidence retention")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN ") || !strings.Contains(lines[0], "pg_stat_activity") || !strings.Contains(lines[0], "6216456209771226") {
		t.Fatalf("stalled retention logged %q, want one warning naming the lock and where to look", lines)
	}

	// Further rounds inside the throttle window add nothing; after it, one
	// repeat with a count.
	now = now.Add(time.Minute)
	p.noteRetentionLag(25 * time.Hour)
	if lines := logLines(buf, "evidence retention"); len(lines) != 0 {
		t.Fatalf("repeat inside the throttle window logged %q", lines)
	}
	now = now.Add(logging.DefaultThrottleWindow)
	p.noteRetentionLag(25 * time.Hour)
	if lines := logLines(buf, "evidence retention"); len(lines) != 1 || !strings.Contains(lines[0], "still failing, 3 times") {
		t.Fatalf("repeat after the throttle window logged %q, want one line with a count", lines)
	}

	// Catching up ends the warning with one info line.
	p.noteRetentionLag(0)
	if lines := logLines(buf, "evidence retention"); len(lines) != 1 || strings.HasPrefix(lines[0], "WARN ") || !strings.Contains(lines[0], "caught up") {
		t.Fatalf("catching up logged %q, want one info line", lines)
	}

	// The warning comes from the stored months, not from how long this
	// process has run: a process that has just started warns at once.
	restarted := &postgresProvider{evidenceClock: func() time.Time { return now }}
	restarted.noteRetentionLag(25 * time.Hour)
	if lines := logLines(buf, "evidence retention"); len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN ") {
		t.Fatalf("a restarted process behind on retention logged %q, want one warning", lines)
	}
}

func TestCompactionSkipWritesOneDebugLine(t *testing.T) {
	buf := captureStorageLog(t, "debug")
	p := &postgresProvider{}
	p.noteCompactionSkipped(t.Context())
	if lines := logLines(buf, "evidence compaction"); len(lines) != 1 || !strings.HasPrefix(lines[0], "DEBUG ") {
		t.Fatalf("skipped round logged %q, want one debug line", lines)
	}
}

func TestMaintenanceFailuresAreDebugUntilTheyLast(t *testing.T) {
	buf := captureStorageLog(t, "info")
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	p := &postgresProvider{evidenceClock: func() time.Time { return now }}
	failure := &pgconn.PgError{Code: "57014"}

	// Healthy passes, and failures shorter than the threshold, write nothing
	// by default.
	p.noteMaintenanceResult(nil)
	p.noteMaintenanceResult(nil)
	for range 9 {
		p.noteMaintenanceResult(failure)
		now = now.Add(time.Minute)
	}
	if lines := logLines(buf, "evidence background maintenance"); len(lines) != 0 {
		t.Fatalf("healthy or briefly failing maintenance logged %q", lines)
	}

	// Failing for the threshold: one warning naming the cause and what to do.
	now = now.Add(time.Minute)
	p.noteMaintenanceResult(failure)
	lines := logLines(buf, "evidence background maintenance")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN ") || !strings.Contains(lines[0], "(timed out)") || !strings.Contains(lines[0], "LOG_LEVEL=debug") {
		t.Fatalf("lasting failure logged %q, want one warning", lines)
	}
	now = now.Add(time.Minute)
	p.noteMaintenanceResult(failure)
	if lines := logLines(buf, "evidence background maintenance"); len(lines) != 0 {
		t.Fatalf("repeat inside the throttle window logged %q", lines)
	}

	// Recovery: one info line, then quiet again; a new failure starts its
	// own interval.
	p.noteMaintenanceResult(nil)
	if lines := logLines(buf, "evidence background maintenance"); len(lines) != 1 || !strings.Contains(lines[0], "running again") {
		t.Fatalf("recovery logged %q, want one info line", lines)
	}
	p.noteMaintenanceResult(failure)
	p.noteMaintenanceResult(nil)
	if lines := logLines(buf, "evidence background maintenance"); len(lines) != 0 {
		t.Fatalf("a short failure after recovery logged %q", lines)
	}
}

func TestBackfillSkipsQuietlyWhenCompactionHoldsTheLock(t *testing.T) {
	buf := captureStorageLog(t, "info")
	more, err := evidenceBackfillLockBusy(fmt.Errorf("lock: %w", &pgconn.PgError{Code: "55P03"}))
	if !more || err != nil {
		t.Fatalf("busy lock answered more=%v err=%v, want a quiet retry", more, err)
	}
	if lines := logLines(buf, "backfill"); len(lines) != 0 {
		t.Fatalf("busy lock logged %q by default", lines)
	}
	other := &pgconn.PgError{Code: "57014"}
	if more, err = evidenceBackfillLockBusy(other); more || err != other {
		t.Fatalf("other error answered more=%v err=%v, want it returned", more, err)
	}
}

func TestSaveLockTimeoutCountsAsTimedOut(t *testing.T) {
	if !evidenceSaveTimedOut(fmt.Errorf("save: %w", &pgconn.PgError{Code: "55P03"})) {
		t.Fatal("a save whose lock wait ran out was not treated as timed out")
	}
	if evidenceSaveTimedOut(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("a unique violation was treated as a timeout")
	}
}

func TestCompactionWaitsAtMostOncePerInterval(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	p := &postgresProvider{}
	if p.compactionWaitDue(now) {
		t.Fatal("the first round of a process waited for the lock")
	}
	if p.compactionWaitDue(now.Add(compactionWaitAfter - time.Second)) {
		t.Fatal("a round inside the interval waited for the lock")
	}
	waited := now.Add(compactionWaitAfter)
	if !p.compactionWaitDue(waited) {
		t.Fatal("a round after the interval without the lock did not wait for it")
	}
	if p.compactionWaitDue(waited.Add(time.Minute)) {
		t.Fatal("a failed wait was repeated on the next round")
	}
	if !p.compactionWaitDue(waited.Add(compactionWaitAfter)) {
		t.Fatal("no new attempt an interval after the failed one")
	}
	p.noteCompactionLocked(waited.Add(compactionWaitAfter))
	if p.compactionWaitDue(waited.Add(compactionWaitAfter + time.Minute)) {
		t.Fatal("a round right after compaction held the lock waited for it")
	}
}

// A save builds its journal input from its records alone, before its
// transaction opens: expired records are left out and the retained ones keep
// their order.
func TestEvidenceEventsInputBuiltFromRecordsAlone(t *testing.T) {
	cutoff := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	records := []EvidenceRecord{
		{ID: "old", Type: "change", At: cutoff.Add(-time.Hour), Payload: []byte(`{}`)},
		{ID: "kept", Type: "change", At: cutoff.Add(time.Hour), Payload: []byte(`{"a":1}`)},
		{ID: "also-kept", Type: "crash", At: cutoff, Payload: []byte(`{"b":2}`)},
	}
	input, err := newEvidenceEventsInput(records, 0, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if input.offered != 3 || input.expired != 1 || input.retained != 2 || input.heartbeat != 1 {
		t.Fatalf("input counts offered=%d expired=%d retained=%d heartbeat=%v", input.offered, input.expired, input.retained, input.heartbeat)
	}
	if strings.Join(input.ids, ",") != "kept,also-kept" || bytes.Contains(input.raw, []byte(`"old"`)) {
		t.Fatalf("input ids %v raw %s", input.ids, input.raw)
	}
	empty, err := newEvidenceEventsInput(records[:1], time.Minute, cutoff)
	if err != nil || empty.retained != 0 || empty.raw != nil {
		t.Fatalf("all-expired input = %+v, %v; want nothing to write", empty, err)
	}
}
