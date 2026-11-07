package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

// evidenceBackfillLimits bounds one read-model conversion transaction.
type evidenceBackfillLimits struct {
	events           int
	occurrences      int
	statementTimeout time.Duration
	batchTimeout     time.Duration
}

var (
	// Startup runs before capture writes or history reads begin, so it uses
	// large batches and a statement budget sized for them.
	evidenceStartupBackfill = evidenceBackfillLimits{events: 4096, occurrences: 1024, statementTimeout: 30 * time.Second, batchTimeout: time.Minute}
	// Background batches share the database with live capture and reads.
	evidenceBackgroundBackfill = evidenceBackfillLimits{events: 512, occurrences: 128, statementTimeout: 2 * time.Second, batchTimeout: 3 * time.Second}
	evidenceStartupBudget      = 10 * time.Minute
)

// completeEvidenceBackfill converts every scope to read models before the
// provider serves history. When the budget runs out the remaining scopes keep
// converting in the background and answer ErrEvidenceNotReady until ready.
func (p *postgresProvider) completeEvidenceBackfill(parent context.Context, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	pending, err := p.pendingEvidenceScopes(ctx)
	if err != nil || pending == 0 {
		return err
	}
	log.Printf("storage: evidence read-model conversion started pending_scopes=%d", pending)
	start, reported, batches := time.Now(), time.Now(), 0
	for {
		batchCtx, stop := context.WithTimeout(ctx, evidenceStartupBackfill.batchTimeout)
		more, err := p.backfillEvidenceBatch(batchCtx, evidenceStartupBackfill)
		stop()
		if err != nil {
			log.Printf("storage: evidence read-model conversion deferred to background batches=%d duration_s=%.0f", batches, time.Since(start).Seconds())
			return err
		}
		batches++
		if !more {
			if pending, err = p.pendingEvidenceScopes(ctx); err != nil {
				return err
			}
			if pending == 0 {
				log.Printf("storage: evidence read-model conversion complete batches=%d duration_s=%.0f", batches, time.Since(start).Seconds())
				return nil
			}
			// Another replica holds the remaining scope; wait for it.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		if time.Since(reported) >= 15*time.Second {
			reported = time.Now()
			log.Printf("storage: evidence read-model conversion progress batches=%d duration_s=%.0f", batches, time.Since(start).Seconds())
		}
	}
}

// nextEvidenceBackfillLimits halves a background conversion batch after a
// failed one and grows it back after each success. A batch's per-event lookup
// searches every monthly partition, so a journal with years of months may
// need smaller batches to finish inside the statement timeout.
func nextEvidenceBackfillLimits(limits evidenceBackfillLimits, err error) evidenceBackfillLimits {
	if err != nil {
		limits.events, limits.occurrences = max(16, limits.events/2), max(8, limits.occurrences/2)
		return limits
	}
	limits.events = min(evidenceBackgroundBackfill.events, limits.events*2)
	limits.occurrences = min(evidenceBackgroundBackfill.occurrences, limits.occurrences*2)
	return limits
}

func (p *postgresProvider) pendingEvidenceScopes(ctx context.Context) (int, error) {
	var pending int
	err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kubernetes_evidence_scopes WHERE NOT read_model_ready`).Scan(&pending)
	return pending, err
}

func (p *postgresProvider) startEvidenceMaintenance() {
	ctx, cancel := context.WithCancel(context.Background())
	p.maintenanceCancel, p.maintenanceDone = cancel, make(chan struct{})
	go func() {
		defer close(p.maintenanceDone)
		// A conversion or heads batch that times out is retried at half its
		// size, so a journal spread over many monthly partitions still
		// converts. Each step has its own deadline and runs whatever the
		// others did, so a busy partition swap never holds the heads pass back.
		conversion, heads := evidenceBackgroundBackfill, evidenceBackgroundBackfill
		var deferredLogged time.Time
		step := func(run func(context.Context) (bool, error)) (bool, error) {
			stepCtx, stop := context.WithTimeout(ctx, evidenceBackgroundBackfill.batchTimeout)
			defer stop()
			return run(stepCtx)
		}
		for {
			more, err := step(func(c context.Context) (bool, error) { return p.backfillEvidenceBatch(c, conversion) })
			conversion = nextEvidenceBackfillLimits(conversion, err)
			partitionMore, partitionErr := step(p.partitionEvidenceBatch)
			headsMore, headsErr := step(func(c context.Context) (bool, error) { return p.backfillEvidenceHeads(c, heads.events) })
			heads = nextEvidenceBackfillLimits(heads, headsErr)
			lagMore, lagErr := step(p.scanObservedLag)
			more = more || partitionMore || headsMore || lagMore
			err = errors.Join(err, partitionErr, headsErr, lagErr)
			if err == nil && !more {
				more, err = step(p.compactEvidenceBatch)
			}
			delay := time.Minute
			switch {
			case more && err == nil:
				delay = 50 * time.Millisecond
			case more:
				delay = time.Second
			}
			if err != nil && ctx.Err() == nil && time.Since(deferredLogged) >= time.Minute {
				deferredLogged = time.Now()
				log.Printf("storage: evidence read-model backfill deferred")
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// Each batch locks one scope briefly. All markers, copied payloads and
// supersession links commit together; a restart repeats an unfinished batch.
// No history is deleted or expired by this upgrade.
func (p *postgresProvider) backfillEvidenceBatch(ctx context.Context, limits evidenceBackfillLimits) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(6216456209771226)`); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL statement_timeout='%dms'; SET LOCAL lock_timeout='100ms'; %s`, limits.statementTimeout.Milliseconds(), evidenceHeadsWriter)); err != nil {
		return false, err
	}
	var key, occurrenceCursor, versionCursor string
	var sequence int64
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT scope_key,backfill_sequence,backfill_occurrence,backfill_version,checkpoint FROM kubernetes_evidence_scopes WHERE NOT read_model_ready ORDER BY scope_key LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&key, &sequence, &occurrenceCursor, &versionCursor, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		// A scope locked by a concurrent capture commit is still pending; report
		// more work so the caller retries promptly instead of idling.
		var locked bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kubernetes_evidence_scopes WHERE NOT read_model_ready)`).Scan(&locked); err != nil {
			return false, err
		}
		return locked, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	var cp EvidenceCheckpoint
	if err = json.Unmarshal(raw, &cp); err != nil {
		return false, err
	}
	var next int64
	err = tx.QueryRowContext(ctx, `WITH batch AS (
 SELECT sequence,event_id FROM kubernetes_evidence_events WHERE scope_key=$1 AND sequence>$2 ORDER BY sequence LIMIT $3
 ), updated AS (
 UPDATE kubernetes_evidence_events e SET superseded_seq=(
 SELECT n.sequence FROM kubernetes_evidence_events n WHERE n.scope_key=$1 AND n.event_id=e.event_id AND n.sequence>e.sequence ORDER BY n.sequence LIMIT 1)
 FROM batch b WHERE e.sequence=b.sequence RETURNING e.sequence)
 SELECT COALESCE(MAX(sequence),$2) FROM updated`, key, sequence, limits.events).Scan(&next)
	if err != nil {
		return false, err
	}
	state := evidenceState{OccurrenceHeads: map[string]string{}, Occurrences: map[string]EvidenceIssue{}, LastObservations: map[string]time.Time{}}
	rows, err := tx.QueryContext(ctx, `SELECT payload,last_seen,payload_version FROM kubernetes_evidence_issues WHERE scope_key=$1 AND NOT model_ready ORDER BY occurrence_id LIMIT $2`, key, limits.occurrences)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var data []byte
		var at time.Time
		var version string
		if err = rows.Scan(&data, &at, &version); err != nil {
			rows.Close()
			return false, err
		}
		var issue EvidenceIssue
		if err = json.Unmarshal(data, &issue); err != nil {
			rows.Close()
			return false, err
		}
		issue.LastSeen = at
		state.Occurrences[issue.OccurrenceID], state.OccurrenceHeads[issue.OccurrenceID] = issue, version
		if version == "" {
			addOccurrenceObservation(&state, issue, cp.OccurrenceRevision, true)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	// Historical-only occurrences were not present in the old issues table.
	// One indexed latest-observation lookup per bounded occurrence identity.
	rows, err = tx.QueryContext(ctx, `SELECT ids.occurrence_id,o.last_seen,o.observed_at,o.payload_version,v.payload
 FROM (SELECT DISTINCT occurrence_id FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1 AND occurrence_id>$2 ORDER BY occurrence_id LIMIT $3) ids
 JOIN LATERAL(SELECT last_seen,observed_at,payload_version FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1 AND occurrence_id=ids.occurrence_id ORDER BY revision DESC LIMIT 1)o ON TRUE
 JOIN kubernetes_evidence_occurrence_versions v ON v.scope_key=$1 AND v.payload_version=o.payload_version
 ORDER BY ids.occurrence_id`, key, occurrenceCursor, limits.occurrences)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id, version string
		var seen, observed time.Time
		var data []byte
		if err = rows.Scan(&id, &seen, &observed, &version, &data); err != nil {
			rows.Close()
			return false, err
		}
		occurrenceCursor = id
		if _, exists := state.Occurrences[id]; exists {
			continue
		}
		var issue EvidenceIssue
		if err = json.Unmarshal(data, &issue); err != nil {
			rows.Close()
			return false, err
		}
		// Existing current rows carry newer truthful freshness than a heartbeat.
		// The guarded upsert below cannot replace them with an older observation.
		issue.LastSeen = seen
		state.Occurrences[id], state.OccurrenceHeads[id], state.LastObservations[id] = issue, version, observed
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if state.OccurrenceVersions == nil {
		state.OccurrenceVersions = map[string]EvidenceIssue{}
	}
	rows, err = tx.QueryContext(ctx, `SELECT v.payload_version,v.payload,o.revision,o.observed_at,o.last_seen FROM kubernetes_evidence_occurrence_versions v
 JOIN LATERAL(SELECT revision,observed_at,last_seen FROM kubernetes_evidence_occurrence_observations WHERE scope_key=v.scope_key AND occurrence_id=v.occurrence_id AND payload_version=v.payload_version ORDER BY revision LIMIT 1)o ON TRUE
 WHERE v.scope_key=$1 AND v.payload_version>$2 ORDER BY v.payload_version LIMIT $3`, key, versionCursor, limits.occurrences)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var version string
		var payload []byte
		var observation occurrenceObservation
		if err = rows.Scan(&version, &payload, &observation.Revision, &observation.At, &observation.LastSeen); err != nil {
			rows.Close()
			return false, err
		}
		var issue EvidenceIssue
		if err = json.Unmarshal(payload, &issue); err != nil {
			rows.Close()
			return false, err
		}
		observation.OccurrenceID, observation.Version = issue.OccurrenceID, version
		state.OccurrenceVersions[version] = issue
		state.Observations = append(state.Observations, observation)
		versionCursor = version
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if err = writeOccurrenceVersions(ctx, tx, key, state); err != nil {
		return false, err
	}
	if err = writeOccurrenceReadModels(ctx, tx, key, state); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes s SET
 event_counts=event_counts||COALESCE((SELECT jsonb_object_agg(c.event_type,COALESCE((s.event_counts->>c.event_type)::bigint,0)+c.total) FROM(
 SELECT event_type,COUNT(*) AS total FROM kubernetes_evidence_events WHERE scope_key=$1 AND sequence>s.backfill_sequence AND sequence<=$2 AND superseded_seq IS NULL GROUP BY event_type)c),'{}'::jsonb),
 backfill_sequence=$2,backfill_occurrence=$3,backfill_version=$4,
 high_sequence=GREATEST(high_sequence,COALESCE((SELECT sequence FROM kubernetes_evidence_events WHERE scope_key=$1 ORDER BY sequence DESC LIMIT 1),0)),
 read_model_ready=NOT EXISTS(SELECT 1 FROM kubernetes_evidence_events WHERE scope_key=$1 AND sequence>$2)
 AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_issues WHERE scope_key=$1 AND NOT model_ready)
 AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1 AND occurrence_id>$3)
 AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_versions v WHERE v.scope_key=$1 AND v.payload_version>$4 AND EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_observations o WHERE o.scope_key=v.scope_key AND o.occurrence_id=v.occurrence_id AND o.payload_version=v.payload_version))
 WHERE s.scope_key=$1`, key, next, occurrenceCursor, versionCursor)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// backfillEvidenceHeads gives a head to every current journal row of a scope
// that holds history from before heads existed, one bounded slice of the
// journal at a time in sequence order. When it reaches the end of the
// journal, every later row was written with its head, so the scope is marked
// complete and saves and reads stop searching its journal for an event's
// current version. A head a save wrote meanwhile is newer and is kept; a head
// whose row is no longer a current journal row (after a restore, say) is
// replaced by the current row the pass finds.
func (p *postgresProvider) backfillEvidenceHeads(ctx context.Context, limit int) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(6216456209771226)`); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='100ms'; `+evidenceHeadsWriter); err != nil {
		return false, err
	}
	var key string
	var cursor int64
	err = tx.QueryRowContext(ctx, `SELECT scope_key,heads_backfill FROM kubernetes_evidence_scopes WHERE heads_backfill IS NOT NULL ORDER BY scope_key LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&key, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		// Every pending scope is held by a save; try again shortly rather
		// than at the next idle pass.
		var locked bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kubernetes_evidence_scopes WHERE heads_backfill IS NOT NULL)`).Scan(&locked); err != nil {
			return false, err
		}
		return locked, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	var next int64
	var read int
	if err = tx.QueryRowContext(ctx, `WITH slice AS MATERIALIZED(
 SELECT scope_key,event_id,sequence,event_at,event_version,priority,observed_at,superseded_seq FROM kubernetes_evidence_events
 WHERE scope_key=$1 AND sequence>$2 ORDER BY sequence LIMIT $3
 ), heads AS(
 INSERT INTO kubernetes_evidence_event_heads AS head(scope_key,event_id,sequence,event_at,event_version,priority,observed_at)
 SELECT DISTINCT ON(event_id) scope_key,event_id,sequence,event_at,event_version,priority,observed_at FROM slice
 WHERE superseded_seq IS NULL ORDER BY event_id,sequence DESC
 ON CONFLICT(scope_key,event_id) DO UPDATE SET sequence=EXCLUDED.sequence,event_at=EXCLUDED.event_at,
 event_version=EXCLUDED.event_version,priority=EXCLUDED.priority,observed_at=EXCLUDED.observed_at
 WHERE head.sequence<EXCLUDED.sequence OR NOT EXISTS(SELECT 1 FROM kubernetes_evidence_events e
 WHERE e.scope_key=head.scope_key AND e.sequence=head.sequence AND e.event_at=head.event_at AND e.superseded_seq IS NULL)
 RETURNING 1)
 SELECT COALESCE(MAX(sequence),$2),COUNT(*) FROM slice`, key, cursor, limit).Scan(&next, &read); err != nil {
		return false, err
	}
	var done any
	if read < limit {
		done = nil
	} else {
		done = next
	}
	if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET heads_backfill=$2 WHERE scope_key=$1`, key, done); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	p.noteHeadsBackfill(ctx, done == nil, next)
	return true, nil
}

// noteHeadsBackfill logs the heads pass: each scope it completes, and its
// progress at most every 30 seconds while scopes are pending.
func (p *postgresProvider) noteHeadsBackfill(ctx context.Context, completed bool, sequence int64) {
	p.maintenanceLogMu.Lock()
	due := completed || time.Since(p.headsLoggedAt) >= 30*time.Second
	if due {
		p.headsLoggedAt = time.Now()
	}
	p.maintenanceLogMu.Unlock()
	if !due {
		return
	}
	var pending int
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kubernetes_evidence_scopes WHERE heads_backfill IS NOT NULL`).Scan(&pending); err != nil {
		return
	}
	if completed {
		log.Printf("storage: evidence heads complete for a scope; its saves no longer search the journal pending_scopes=%d", pending)
		return
	}
	log.Printf("storage: evidence heads backfill progress sequence=%d pending_scopes=%d", sequence, pending)
}

// evidenceHeadsComplete reports whether every current journal row of the scope
// has a head. A scope without a row has no history and is complete.
func evidenceHeadsComplete(ctx context.Context, tx *sql.Tx, key string) (bool, error) {
	var complete bool
	err := tx.QueryRowContext(ctx, `SELECT heads_backfill IS NULL FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return complete, err
}
