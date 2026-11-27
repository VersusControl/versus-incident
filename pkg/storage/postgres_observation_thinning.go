package storage

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/jackc/pgx/v5/pgconn"
)

// observationThinningWalk is how many old samples of one scope a thinning
// pass examines. A pass reads them in observation order through the scope's
// observation-time index and resumes after the last one the next time, so a
// pass costs the same whether it finds samples to remove or none.
const observationThinningWalk = 2000

// observationThinningStatement examines the next $8 samples of scope $3
// observed before $1, from $4 on and after the position ($5, $6, $7), and
// removes those thinning may remove: not their version's opening observation
// and not the last sample of their hour. Both checks are looked up once per
// sample (OFFSET 0 keeps each from becoming a join): the later sample of the
// same hour only in that hour's month, and the version's opening through the
// read model's key. It answers how many samples it examined and removed and
// the position of the last one examined; no row when none was left.
const observationThinningStatement = `WITH walk AS (
 SELECT o.tableoid AS relation,o.ctid AS tuple,o.observed_at,o.revision,o.occurrence_id FROM kubernetes_evidence_occurrence_observations o
 WHERE o.scope_key=$3 AND o.observed_at < $1 AND o.observed_at >= $4 AND o.observed_at >= $5 AND (o.observed_at,o.revision,o.occurrence_id) > ($5::timestamptz,$6::bigint,$7::text)
 ORDER BY o.observed_at,o.revision,o.occurrence_id LIMIT $8),
gone AS (
 DELETE FROM kubernetes_evidence_occurrence_observations o USING walk w WHERE o.tableoid=w.relation AND o.ctid=w.tuple AND o.observed_at < $1
 AND EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_observations n WHERE n.scope_key=o.scope_key AND n.occurrence_id=o.occurrence_id AND n.payload_version=o.payload_version AND n.observed_at>=date_trunc('hour',o.observed_at) AND n.observed_at<date_trunc('hour',o.observed_at)+INTERVAL '1 hour' AND n.observed_at<$2 AND n.revision>o.revision OFFSET 0)
 AND EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_read_versions opening WHERE opening.scope_key=o.scope_key AND opening.payload_version=o.payload_version AND opening.occurrence_id=o.occurrence_id AND opening.revision<o.revision OFFSET 0)
 RETURNING 1)
SELECT (SELECT COUNT(*) FROM walk),(SELECT COUNT(*) FROM gone),w.observed_at,w.revision,w.occurrence_id FROM walk w ORDER BY w.observed_at DESC,w.revision DESC,w.occurrence_id DESC LIMIT 1`

// observationThinningTimeouts is how many passes in a row may run out of
// their statement time before thinning stops trying for
// observationThinningPause. Old samples are kept meanwhile; nothing is lost.
const (
	observationThinningTimeouts = 3
	observationThinningPause    = time.Hour
	observationThinningKey      = "evidence observation thinning"
)

// observationThinning remembers where thinning's walk is and the passes that
// ran out of time. The walk goes scope by scope, oldest sample first. A walk
// through every scope ends a cycle; samples older than the expiry a finished
// cycle started with were all examined and kept, so the next cycle starts an
// hour before it (thinning keeps one sample an hour). A restart walks once
// from the beginning.
type observationThinning struct {
	mu          sync.Mutex
	timeouts    int
	pausedUntil time.Time
	warning     *logging.Throttle
	// The walk: the scope it is in, the position after which it resumes, the
	// expiry its cycle started with, and the floor below which every sample
	// was examined by an earlier cycle.
	scope       string
	at          time.Time
	revision    int64
	occurrence  string
	cycleExpiry time.Time
	floor       time.Time
	// walk, when set, replaces observationThinningWalk; tests use it.
	walk int
	// before, when set, runs inside each pass before its statement and may
	// answer the pass in its place; tests use it.
	before func(context.Context, *sql.Tx) error
}

// statementTimedOut reports a statement cancelled by its statement_timeout.
func statementTimedOut(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}

// thinObservations runs one thinning pass inside the compaction transaction.
// It returns the scopes it removed samples from, and whether the walk has
// more to examine before its cycle ends. A pass that runs out of statement
// time is undone alone, so the transaction stays usable; after
// observationThinningTimeouts such passes in a row thinning pauses for
// observationThinningPause, with one warning.
func (p *postgresProvider) thinObservations(ctx context.Context, tx *sql.Tx, expired time.Time) (map[string]bool, bool, error) {
	state := &p.thinning
	now := p.compactionNow()
	state.mu.Lock()
	if now.Before(state.pausedUntil) {
		state.mu.Unlock()
		return nil, false, nil
	}
	scope, at, revision, occurrence, floor, cycleExpiry := state.scope, state.at, state.revision, state.occurrence, state.floor, state.cycleExpiry
	before := state.before
	walk := state.walk
	state.mu.Unlock()
	if walk <= 0 {
		walk = observationThinningWalk
	}
	if scope == "" {
		// A new cycle starts at the first scope.
		err := tx.QueryRowContext(ctx, `SELECT scope_key FROM kubernetes_evidence_scopes ORDER BY scope_key LIMIT 1`).Scan(&scope)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		at, revision, occurrence, cycleExpiry = floor, -1, "", expired
	}
	if at.IsZero() {
		at = time.Unix(0, 0).UTC()
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT observation_thinning`); err != nil {
		return nil, false, err
	}
	var walked, removed int
	var lastAt time.Time
	var lastRevision int64
	var lastOccurrence string
	err := func() error {
		if before != nil {
			if err := before(ctx, tx); err != nil {
				return err
			}
		}
		return tx.QueryRowContext(ctx, observationThinningStatement, expired, expired.Add(time.Hour), scope, floor.UTC(), at.UTC(), revision, occurrence, walk).
			Scan(&walked, &removed, &lastAt, &lastRevision, &lastOccurrence)
	}()
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		if !statementTimedOut(err) || ctx.Err() != nil {
			return nil, false, err
		}
		if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT observation_thinning`); rollbackErr != nil {
			return nil, false, rollbackErr
		}
		p.noteThinningTimeout(now)
		return nil, false, nil
	}
	if _, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT observation_thinning`); err != nil {
		return nil, false, err
	}
	more := true
	var compacted map[string]bool
	if removed > 0 {
		compacted = map[string]bool{scope: true}
	}
	if walked == 0 {
		// This scope has nothing left to examine: the walk moves to the next
		// scope, or ends its cycle after the last one.
		next := ""
		err = tx.QueryRowContext(ctx, `SELECT scope_key FROM kubernetes_evidence_scopes WHERE scope_key>$1 ORDER BY scope_key LIMIT 1`, scope).Scan(&next)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
		if next == "" {
			floor, more = cycleExpiry.Add(-time.Hour), false
		}
		scope, lastAt, lastRevision, lastOccurrence = next, floor, -1, ""
	}
	state.mu.Lock()
	state.scope, state.at, state.revision, state.occurrence = scope, lastAt, lastRevision, lastOccurrence
	state.floor, state.cycleExpiry = floor, cycleExpiry
	state.timeouts = 0
	warning := state.warning
	state.mu.Unlock()
	if warning != nil {
		warning.Clear(observationThinningKey, "storage: evidence observation thinning finishes again")
	}
	return compacted, more, nil
}

// noteThinningTimeout counts a pass that ran out of time and pauses thinning
// once too many did in a row.
func (p *postgresProvider) noteThinningTimeout(now time.Time) {
	state := &p.thinning
	state.mu.Lock()
	state.timeouts++
	if timeouts := state.timeouts; timeouts < observationThinningTimeouts {
		state.mu.Unlock()
		logging.Debugf("storage: evidence observation thinning ran out of statement time (%d in a row)", timeouts)
		return
	}
	state.timeouts = 0
	state.pausedUntil = now.Add(observationThinningPause)
	if state.warning == nil {
		state.warning = logging.NewThrottle(0, p.compactionNow)
	}
	warning := state.warning
	state.mu.Unlock()
	warning.Fail(slog.LevelWarn, observationThinningKey, "statement timeout",
		"storage: evidence observation thinning stopped for %s: %d passes in a row ran past the 2s statement limit (statement timeout); old samples are kept, not lost. Check the size and autovacuum of the evidence_observation_month_* partitions and of kubernetes_evidence_occurrence_read_versions",
		observationThinningPause, observationThinningTimeouts)
}
