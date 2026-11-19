package storage

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VersusControl/versus-incident/pkg/logging"
)

const (
	// compactionWaitAfter is how long compaction only tries the partition
	// lock before it makes one attempt that waits for it. Saves hold the lock
	// shared, and a waiting exclusive request queues every later save behind
	// it, so the wait is bounded by compactionWaitTimeout and made at most
	// once per this interval.
	compactionWaitAfter = 30 * time.Minute
	// compactionWaitTimeout bounds that attempt, and so how long a save
	// queued behind it can wait.
	compactionWaitTimeout = "200ms"
	// compactionStalledAfter is how far the oldest stored month may lag
	// behind the retention cutoff before the operator is warned that
	// retention has stopped.
	compactionStalledAfter = 24 * time.Hour
	// compactionStalledKey names the stalled-retention warning in its
	// throttle.
	compactionStalledKey = "evidence retention"
)

// compactionNow is the clock compaction's lock bookkeeping reads.
func (p *postgresProvider) compactionNow() time.Time {
	if p.evidenceClock != nil {
		return p.evidenceClock()
	}
	return time.Now()
}

// lockEvidenceForCompaction takes the partition lock exclusively for one
// compaction round, without ever leaving an exclusive request queued ahead
// of saves. Normally it only tries the lock and reports false when a save or
// a backfill holds it, so the round is skipped and retried by the next one.
// When compaction has not had the lock for compactionWaitAfter it waits for
// it once, for at most compactionWaitTimeout, and then gives up the same way.
// The transaction's lock_timeout is restored to its usual value after.
func (p *postgresProvider) lockEvidenceForCompaction(ctx context.Context, tx *sql.Tx) (bool, error) {
	now := p.compactionNow()
	if !p.compactionWaitDue(now) {
		var locked bool
		if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(6216456209771226)`).Scan(&locked); err != nil {
			return false, err
		}
		if !locked {
			p.noteCompactionSkipped(ctx)
			return false, nil
		}
		p.noteCompactionLocked(now)
		return true, nil
	}
	_, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout='`+compactionWaitTimeout+`'; SELECT pg_advisory_xact_lock(6216456209771226)`)
	if lockNotAvailable(err) {
		p.noteCompactionSkipped(ctx)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
		return false, err
	}
	p.noteCompactionLocked(now)
	return true, nil
}

// lockNotAvailable reports a lock request that ran past lock_timeout.
func lockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// compactionWaitDue reports whether this round waits for the partition lock,
// and records the attempt. The first round of a process starts the interval.
func (p *postgresProvider) compactionWaitDue(now time.Time) bool {
	p.maintenanceLogMu.Lock()
	defer p.maintenanceLogMu.Unlock()
	if p.compactionLockedAt.IsZero() {
		p.compactionLockedAt = now
	}
	last := p.compactionLockedAt
	if p.compactionWaitedAt.After(last) {
		last = p.compactionWaitedAt
	}
	if now.Sub(last) < compactionWaitAfter {
		return false
	}
	p.compactionWaitedAt = now
	return true
}

// noteCompactionSkipped logs a round skipped because the lock was held: one
// debug line, and a throttled warning when the stored months show retention
// is more than compactionStalledAfter behind. The check reads the database,
// not this process's memory, so it holds across restarts and replicas.
func (p *postgresProvider) noteCompactionSkipped(ctx context.Context) {
	logging.Debugf("storage: evidence compaction skipped a round; evidence saves or backfills hold the partition lock")
	if p.evidenceRetention <= 0 || p.db == nil {
		return
	}
	oldest, err := p.oldestEvidenceMonth(ctx)
	if err != nil {
		logging.Debugf("storage: evidence compaction could not read the oldest stored month: %v", err)
		return
	}
	p.noteRetentionLag(evidenceRetentionLag(oldest, p.compactionNow().UTC().Add(-p.evidenceRetention)))
}

// oldestEvidenceMonth returns the oldest month partition of the journal or the
// observations, as YYYYMM, or "" when there is none. It reads only the
// catalog, so it takes no lock that compaction or a save holds.
func (p *postgresProvider) oldestEvidenceMonth(ctx context.Context) (string, error) {
	var oldest sql.NullString
	err := p.db.QueryRowContext(ctx, `SELECT min(right(c.relname,6)) FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
 WHERE i.inhparent IN(to_regclass('kubernetes_evidence_events'),to_regclass('kubernetes_evidence_occurrence_observations'))
 AND c.relkind='r' AND c.relname ~ '^evidence_(event|observation)_month_[0-9]{6}$'`).Scan(&oldest)
	return oldest.String, err
}

// evidenceRetentionLag is how long ago the oldest stored month passed the
// retention cutoff, the point after which compaction drops it; zero when it
// has not, or when there is no month.
func evidenceRetentionLag(oldest string, cutoff time.Time) time.Duration {
	month, err := time.Parse("200601", oldest)
	if err != nil {
		return 0
	}
	return max(0, cutoff.Sub(month.AddDate(0, 1, 0)))
}

// noteRetentionLag warns, throttled, while retention is more than
// compactionStalledAfter behind, and logs one info line when it catches up
// after a warning.
func (p *postgresProvider) noteRetentionLag(lag time.Duration) {
	p.maintenanceLogMu.Lock()
	throttle := p.compactionThrottleLocked()
	p.maintenanceLogMu.Unlock()
	if lag < compactionStalledAfter {
		if throttle.Clear(compactionStalledKey, "storage: evidence retention caught up") {
			logging.Infof("storage: evidence retention caught up; compaction is removing old evidence again")
		}
		return
	}
	throttle.Fail(slog.LevelWarn, compactionStalledKey, "lock held",
		"storage: evidence retention is %s behind (compaction cannot get the partition lock); old evidence is not removed and the journal keeps growing. Check pg_stat_activity for long or idle-in-transaction sessions holding advisory lock 6216456209771226, and evidence save times",
		lag.Truncate(time.Minute))
}

// noteCompactionLocked records that compaction holds the partition lock.
func (p *postgresProvider) noteCompactionLocked(now time.Time) {
	p.maintenanceLogMu.Lock()
	p.compactionLockedAt = now
	p.maintenanceLogMu.Unlock()
}

// compactionThrottleLocked returns the stalled-retention throttle; the caller
// holds maintenanceLogMu.
func (p *postgresProvider) compactionThrottleLocked() *logging.Throttle {
	if p.compactionStalled == nil {
		p.compactionStalled = logging.NewThrottle(0, p.compactionNow)
	}
	return p.compactionStalled
}
