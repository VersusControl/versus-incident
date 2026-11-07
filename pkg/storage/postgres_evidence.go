package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func (p *postgresProvider) EvidencePolicy() EvidencePolicy {
	return EvidencePolicy{Mode: "unbounded_database", Retention: p.evidenceRetention}
}

func (p *postgresProvider) CommitEvidence(parent context.Context, scope EvidenceScope, batch EvidenceBatch) error {
	key, err := evidenceScopeKey(scope)
	if err != nil {
		return err
	}
	batch = normalizeEvidenceBatch(batch)
	if err = validEvidenceBatch(batch); err != nil {
		return err
	}
	if batch.ObservationHeartbeat == 0 {
		batch.ObservationHeartbeat = p.observationHeartbeat
	}
	return evidenceSaveError(p.saveEvidence(parent, key, scope, batch))
}

// errEvidenceAlreadySaved ends a save whose capture an earlier attempt saved.
var errEvidenceAlreadySaved = errors.New("storage: capture already saved")

// errEvidenceSaveSplit reports that a capture's journal records, lifecycle
// records included, do not fit one save part.
var errEvidenceSaveSplit = errors.New("storage: capture is saved in parts")

// saveEvidence saves a capture in transactions that each write at most one
// part: the save chunk in records and the part byte bound in payload.
//
// A capture whose journal records fit one part is saved in one transaction.
// A larger one writes its journal records part by part, each in its own
// transaction, and then a final transaction that holds only the read models
// and the checkpoint. Readers pin to the checkpoint's sequence, so they never
// see a partly saved capture. Each record write is idempotent by event and
// content: a failed part is retried alone, a later retry of the capture
// appends nothing twice, and a part that times out is retried at half the
// size, never the whole capture at full size. Every transaction runs inside
// the caller's fence and confirms the lease first, so a caller can serve
// other work between parts.
func (p *postgresProvider) saveEvidence(parent context.Context, key string, scope EvidenceScope, batch EvidenceBatch) error {
	batch.Records = latestEvidenceRecords(batch.Records)
	cutoff := p.evidenceCutoff()
	size := max(1, p.evidenceSaveChunk)
	stats := evidenceWriteStats{Offered: len(batch.Records)}
	part := 0
	written, records, err := p.commitEvidenceFinal(parent, key, scope, batch, nil, 0, size, part, cutoff)
	if errors.Is(err, errEvidenceAlreadySaved) {
		return nil
	}
	if err == nil {
		stats.add(written)
		p.noteEvidenceWrite(stats)
		return nil
	}
	if records == nil || !errors.Is(err, errEvidenceSaveSplit) && !evidenceSaveTimedOut(err) {
		return err
	}
	if evidenceSaveTimedOut(err) {
		if parent.Err() != nil || size <= minEvidenceSaveChunk {
			return err
		}
		size = max(minEvidenceSaveChunk, size/2)
		log.Printf("storage: evidence save timed out in one transaction; saving it in parts of %d records", size)
	}
	saved := map[string]bool{}
	for start := 0; start < len(records); {
		end := evidencePartEnd(records, start, size)
		part++
		written, committed, err := p.commitEvidencePart(parent, key, scope, batch, records[start:end], part, cutoff)
		if evidenceSaveTimedOut(err) {
			// A save whose own deadline has passed stops; otherwise the part
			// is retried alone at half the size.
			if parent.Err() != nil || size <= minEvidenceSaveChunk {
				return err
			}
			size = max(minEvidenceSaveChunk, size/2)
			log.Printf("storage: evidence save part timed out; retrying it alone at %d records", size)
			continue
		}
		if err != nil {
			return err
		}
		if committed {
			// This capture was already saved in full by an earlier attempt.
			return nil
		}
		stats.add(written)
		stats.Parts++
		for _, record := range records[start:end] {
			saved[record.ID+"\x00"+evidenceVersion(record)] = true
		}
		start = end
	}
	written, _, err = p.commitEvidenceFinal(parent, key, scope, batch, saved, stats.High, size, part+1, cutoff)
	if errors.Is(err, errEvidenceAlreadySaved) {
		return nil
	}
	if err != nil {
		return err
	}
	stats.add(written)
	p.noteEvidenceWrite(stats)
	return nil
}

// evidenceFenced runs one save transaction inside the caller's fence.
func evidenceFenced(ctx context.Context, batch EvidenceBatch, transaction func() error) error {
	if batch.Fence == nil {
		return transaction()
	}
	return batch.Fence(ctx, transaction)
}

// commitEvidencePart writes one part of a capture's journal records in its
// own transaction. It reports committed when the capture is already saved.
func (p *postgresProvider) commitEvidencePart(parent context.Context, key string, scope EvidenceScope, batch EvidenceBatch, records []EvidenceRecord, part int, cutoff time.Time) (evidenceWriteStats, bool, error) {
	var written evidenceWriteStats
	committed := false
	err := evidenceFenced(parent, batch, func() error {
		ctx, cancel, tx, err := p.beginEvidenceSave(parent, &batch)
		if err != nil {
			return err
		}
		defer cancel()
		defer tx.Rollback()
		if p.evidenceChunkFault != nil {
			if err = p.evidenceChunkFault(ctx, tx, part, len(records)); err != nil {
				return err
			}
		}
		if err = prepareEvidencePartitions(ctx, tx, EvidenceBatch{At: batch.At, Records: records}, cutoff); err != nil {
			return err
		}
		if err = evidenceSaveGuard(ctx, tx, batch); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid,read_model_ready,heads_backfill,observed_lag_scan) VALUES($1,$2,$3,$4,TRUE,NULL,NULL) ON CONFLICT DO NOTHING`, key, NormalizeOrgID(scope.OrgID), scope.ClusterID, scope.ClusterUID); err != nil {
			return err
		}
		var raw []byte
		if err = tx.QueryRowContext(ctx, `SELECT checkpoint FROM kubernetes_evidence_scopes WHERE scope_key=$1 FOR UPDATE`, key).Scan(&raw); err != nil {
			return err
		}
		var checkpoint EvidenceCheckpoint
		if err = json.Unmarshal(raw, &checkpoint); err != nil {
			return err
		}
		if checkpoint.Generation != batch.Generation {
			return ErrEvidenceConflict
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kubernetes_evidence_captures WHERE scope_key=$1 AND capture_id=$2)`, key, batch.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			committed = true
			return tx.Commit()
		}
		if checkpoint.ID != batch.Expected || batch.At.Before(checkpoint.At) {
			return ErrEvidenceConflict
		}
		if written, err = writeRetainedEvidenceEvents(ctx, tx, key, records, batch.ObservationHeartbeat, cutoff); err != nil {
			return err
		}
		// Mark the save in progress and remember the highest sequence it
		// wrote, which a later attempt that re-offers this part cannot see.
		if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=jsonb_set(jsonb_set(checkpoint,'{saving}',to_jsonb($2::text)),'{saving_high}',to_jsonb(GREATEST(COALESCE((checkpoint->>'saving_high')::bigint,0),$3::bigint))) WHERE scope_key=$1`, key, batch.ID, int64(written.High)); err != nil {
			return err
		}
		return tx.Commit()
	})
	return written, committed, err
}

// beginEvidenceSave opens one save transaction, bounded by the save timeout.
// The lease is confirmed, and renewed when needed, before every transaction,
// so a long capture never outlives it. Every save statement is planned for its
// actual values: a generic plan can look new events up through an index that
// does not lead with the event's key and read the scope's whole history.
func (p *postgresProvider) beginEvidenceSave(parent context.Context, batch *EvidenceBatch) (context.Context, context.CancelFunc, *sql.Tx, error) {
	if batch.KeepLease != nil {
		guard, err := batch.KeepLease(parent)
		if err != nil {
			return nil, nil, nil, err
		}
		batch.GuardExpected = guard
	}
	timeout := p.evidenceSaveTimeout
	if timeout <= 0 {
		timeout = defaultEvidenceSaveTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL plan_cache_mode = force_custom_plan; `+evidenceHeadsWriter); err != nil {
		_ = tx.Rollback()
		cancel()
		return nil, nil, nil, err
	}
	return ctx, cancel, tx, nil
}

// evidenceSaveGuard fences a save to the lease holder named by the batch.
func evidenceSaveGuard(ctx context.Context, tx *sql.Tx, batch EvidenceBatch) error {
	if batch.GuardKey == "" {
		return nil
	}
	var actual []byte
	err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT data FROM %s WHERE name=$1 FOR UPDATE", blobTable(batch.GuardKey)), batch.GuardKey).Scan(&actual)
	if err == sql.ErrNoRows {
		return ErrEvidenceConflict
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, batch.GuardExpected) {
		return ErrEvidenceConflict
	}
	return nil
}

// commitEvidenceFinal is the transaction that advances the checkpoint. With
// saved nil it also writes the capture's journal records, but only when they
// fit one part: otherwise it writes nothing and answers errEvidenceSaveSplit
// with the records to save in parts. With saved set, the records were written
// by earlier parts, and only a record that is not among them (the saved state
// cannot change while the checkpoint is held, so normally none) is written
// here beside the read models and the checkpoint, which then covers savedHigh,
// the highest sequence the parts wrote.
func (p *postgresProvider) commitEvidenceFinal(parent context.Context, key string, scope EvidenceScope, batch EvidenceBatch, saved map[string]bool, savedHigh uint64, size, part int, cutoff time.Time) (evidenceWriteStats, []EvidenceRecord, error) {
	var written evidenceWriteStats
	var derived []EvidenceRecord
	err := evidenceFenced(parent, batch, func() error {
		ctx, cancel, tx, err := p.beginEvidenceSave(parent, &batch)
		if err != nil {
			return err
		}
		defer cancel()
		defer tx.Rollback()
		// Partitions first: their shared lock is always taken before the
		// guard and checkpoint rows, as retention takes its own.
		if err = prepareEvidencePartitions(ctx, tx, batch, cutoff); err != nil {
			return err
		}
		state, err := loadEvidenceSaveState(ctx, tx, key, scope, batch)
		if err != nil {
			return err
		}
		records, err := applyEvidence(&state, batch)
		if err != nil {
			return err
		}
		derived = records
		pending := records
		if saved == nil {
			if len(records) > 0 && evidencePartEnd(records, 0, size) < len(records) {
				return errEvidenceSaveSplit
			}
		} else {
			pending = make([]EvidenceRecord, 0)
			for _, record := range records {
				if !saved[record.ID+"\x00"+evidenceVersion(record)] {
					pending = append(pending, record)
				}
			}
		}
		if p.evidenceChunkFault != nil {
			if err = p.evidenceChunkFault(ctx, tx, part, len(pending)); err != nil {
				return err
			}
		}
		if written, err = writeRetainedEvidenceEvents(ctx, tx, key, pending, batch.ObservationHeartbeat, cutoff); err != nil {
			return err
		}
		// Readers pin to this sequence: it covers every part of the capture.
		state.Checkpoint.HighSequence = max(state.Checkpoint.HighSequence, state.Checkpoint.SavingHigh, savedHigh, written.High)
		state.Checkpoint.Saving, state.Checkpoint.SavingHigh = "", 0
		if err = writeOccurrenceVersions(ctx, tx, key, state); err != nil {
			return err
		}
		if err = writeOccurrenceReadModels(ctx, tx, key, state); err != nil {
			return err
		}
		raw, err := json.Marshal(state.Checkpoint)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=$2,high_sequence=GREATEST(high_sequence,$3),scope_binding=COALESCE(NULLIF($4,''),scope_binding),updated_at=$5 WHERE scope_key=$1`, key, raw, int64(state.Checkpoint.HighSequence), batch.ScopeBinding, batch.At); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_captures(scope_key,capture_id,observed_at) VALUES($1,$2,$3)`, key, batch.ID, batch.At); err != nil {
			return err
		}
		return tx.Commit()
	})
	return written, derived, err
}

// loadEvidenceSaveState locks the scope's checkpoint and loads the issue state
// a save applies the capture to. It answers errEvidenceAlreadySaved when the
// capture was saved by an earlier attempt.
func loadEvidenceSaveState(ctx context.Context, tx *sql.Tx, key string, scope EvidenceScope, batch EvidenceBatch) (evidenceState, error) {
	if err := evidenceSaveGuard(ctx, tx, batch); err != nil {
		return evidenceState{}, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid,read_model_ready,heads_backfill,observed_lag_scan) VALUES($1,$2,$3,$4,TRUE,NULL,NULL) ON CONFLICT DO NOTHING`, key, NormalizeOrgID(scope.OrgID), scope.ClusterID, scope.ClusterUID)
	if err != nil {
		return evidenceState{}, err
	}
	var raw []byte
	var storedHigh uint64
	err = tx.QueryRowContext(ctx, `SELECT checkpoint,high_sequence FROM kubernetes_evidence_scopes WHERE scope_key=$1 FOR UPDATE`, key).Scan(&raw, &storedHigh)
	if err != nil {
		return evidenceState{}, err
	}
	state := evidenceState{Active: map[string]EvidenceIssue{}, Occurrences: map[string]EvidenceIssue{}, OccurrenceVersions: map[string]EvidenceIssue{}, OccurrenceHeads: map[string]string{}}
	if err = json.Unmarshal(raw, &state.Checkpoint); err != nil {
		return evidenceState{}, err
	}
	state.Checkpoint.HighSequence = max(state.Checkpoint.HighSequence, storedHigh)
	if state.Checkpoint.Generation != batch.Generation {
		return evidenceState{}, ErrEvidenceConflict
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kubernetes_evidence_captures WHERE scope_key=$1 AND capture_id=$2)`, key, batch.ID).Scan(&exists)
	if err != nil {
		return evidenceState{}, err
	}
	if exists {
		return evidenceState{}, errEvidenceAlreadySaved
	}
	if state.Checkpoint.ID != batch.Expected || batch.At.Before(state.Checkpoint.At) {
		return evidenceState{}, ErrEvidenceConflict
	}
	historicalIDs := []string{}
	if batch.HistoricalOnly {
		for _, issue := range batch.Issues {
			at := issue.FirstSeen
			if at.IsZero() {
				at = issue.LastSeen
			}
			if at.IsZero() {
				at = batch.At
			}
			historicalIDs = append(historicalIDs, evidenceDigest(issue.Key, "observation", at.UTC().Format("2006-01-02")))
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload,last_seen,payload_version,last_observed_at FROM kubernetes_evidence_issues WHERE scope_key=$1 AND ((closed_at IS NULL AND NOT observation_only) OR occurrence_id=ANY($2::text[]))`, key, historicalIDs)
	if err != nil {
		return evidenceState{}, err
	}
	for rows.Next() {
		var data []byte
		var lastSeen time.Time
		var version string
		var lastObserved sql.NullTime
		if err = rows.Scan(&data, &lastSeen, &version, &lastObserved); err != nil {
			rows.Close()
			return evidenceState{}, err
		}
		var issue EvidenceIssue
		if err = json.Unmarshal(data, &issue); err != nil {
			rows.Close()
			return evidenceState{}, err
		}
		issue.LastSeen = lastSeen
		if !issue.ObservationOnly {
			state.Active[issue.Key] = issue
		}
		state.Occurrences[issue.OccurrenceID] = issue
		state.OccurrenceHeads[issue.OccurrenceID] = version
		if state.LastObservations == nil {
			state.LastObservations = map[string]time.Time{}
		}
		state.LastObservations[issue.OccurrenceID] = lastObserved.Time
		if version == "" {
			addOccurrenceObservation(&state, issue, state.Checkpoint.OccurrenceRevision, true)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return evidenceState{}, err
	}
	rows.Close()
	return state, nil
}

func (p *postgresProvider) EvidenceCheckpoint(parent context.Context, scope EvidenceScope) (EvidenceCheckpoint, error) {
	key, err := evidenceScopeKey(scope)
	if err != nil {
		return EvidenceCheckpoint{}, err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	var raw []byte
	var high uint64
	if history, _ := ctx.Value(evidenceReadContextKey{}).(bool); history {
		tx, readErr := p.beginEvidenceRead(ctx)
		if readErr != nil {
			return EvidenceCheckpoint{}, readErr
		}
		defer tx.Rollback()
		err = tx.QueryRowContext(ctx, `SELECT checkpoint,high_sequence FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key).Scan(&raw, &high)
		if err == nil {
			err = tx.Commit()
		}
	} else {
		err = p.db.QueryRowContext(ctx, `SELECT checkpoint,high_sequence FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key).Scan(&raw, &high)
	}
	if err == sql.ErrNoRows {
		return EvidenceCheckpoint{}, nil
	}
	if err != nil {
		return EvidenceCheckpoint{}, err
	}
	var checkpoint EvidenceCheckpoint
	err = json.Unmarshal(raw, &checkpoint)
	checkpoint.HighSequence = max(checkpoint.HighSequence, high)
	return checkpoint, err
}

func (p *postgresProvider) QueryEvidence(parent context.Context, scope EvidenceScope, query EvidenceQuery) (EvidencePage, error) {
	if query.Type == "occurrence" {
		return p.queryOccurrences(parent, scope, query)
	}
	query, cursor, err := prepareEvidenceQuery(scope, query)
	if err != nil {
		return EvidencePage{}, err
	}
	readScopes, err := evidenceReadScopes(scope)
	if err != nil {
		return EvidencePage{}, err
	}
	keys := []string{}
	for _, readScope := range readScopes {
		key, keyErr := evidenceScopeKey(readScope)
		if keyErr != nil {
			return EvidencePage{}, keyErr
		}
		keys = append(keys, key)
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	page, err := p.queryEvidencePage(ctx, keys, query, cursor)
	if evidenceMonthDropped(err) {
		// Retention dropped a month the read had listed but not yet read:
		// read again with the months that remain.
		page, err = p.queryEvidencePage(ctx, keys, query, cursor)
	}
	return page, err
}

// evidenceMonthDropped reports a read that named a monthly partition removed
// after the read listed the months.
func evidenceMonthDropped(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// queryEvidencePage reads one page of the scopes' journal in one transaction.
func (p *postgresProvider) queryEvidencePage(ctx context.Context, keys []string, query EvidenceQuery, cursor evidenceCursor) (EvidencePage, error) {
	tx, err := p.beginEvidenceRead(ctx)
	if err != nil {
		return EvidencePage{}, err
	}
	defer tx.Rollback()
	countGeneration := p.evidenceCountGeneration()
	page := EvidencePage{Policy: p.EvidencePolicy(), Since: query.Since, Until: query.Until, Counts: map[string]int{}}
	generations := make([]uint64, len(keys))
	pruneRevisions := make([]uint64, len(keys))
	ready := true
	highSequences := make([]uint64, len(keys))
	summaryCounts := map[string]int{}
	checkpointRows, err := tx.QueryContext(ctx, `SELECT scope_key,checkpoint,high_sequence,read_model_ready,event_counts FROM kubernetes_evidence_scopes WHERE scope_key=ANY($1::text[]) ORDER BY array_position($1::text[],scope_key)`, keys)
	if err != nil {
		return page, err
	}
	for checkpointRows.Next() {
		var raw []byte
		var checkpoint EvidenceCheckpoint
		var key string
		var high uint64
		var modelReady bool
		var countsRaw []byte
		if err = checkpointRows.Scan(&key, &raw, &high, &modelReady, &countsRaw); err != nil {
			checkpointRows.Close()
			return page, err
		}
		ready = ready && modelReady
		if len(keys) == 1 {
			if err = json.Unmarshal(countsRaw, &summaryCounts); err != nil {
				checkpointRows.Close()
				return page, err
			}
		}
		if err = json.Unmarshal(raw, &checkpoint); err != nil {
			checkpointRows.Close()
			return page, err
		}
		for i, candidate := range keys {
			if candidate == key {
				generations[i] = checkpoint.Generation
				highSequences[i] = high
				pruneRevisions[i] = checkpoint.PruneRevision
			}
		}
		page.Checkpoint = mergeEvidenceCheckpoint(page.Checkpoint, checkpoint)
	}
	if err = checkpointRows.Err(); err != nil {
		checkpointRows.Close()
		return page, err
	}
	checkpointRows.Close()
	if !ready {
		return page, ErrEvidenceNotReady
	}

	if query.Cursor == "" {
		cursor.Generations = generations
		cursor.PruneRevisions = pruneRevisions
		cursor.Highs = highSequences
	} else if len(cursor.Generations) != len(keys) || len(cursor.Highs) != len(keys) {
		return page, ErrInvalidEvidence
	}
	if len(cursor.PruneRevisions) != len(keys) {
		return page, ErrEvidenceConflict
	}
	for i, generation := range generations {
		if cursor.Generations[i] != generation || cursor.PruneRevisions[i] != pruneRevisions[i] {
			// History was pruned or replaced since the cursor was issued.
			return page, ErrEvidenceCursorExpired
		}
	}
	return p.queryJournalPage(ctx, tx, keys, query, cursor, page, countGeneration, summaryCounts)
}

// DeleteEvidence is explicit operator lifecycle, never part of normal capture.
func (p *postgresProvider) DeleteEvidence(parent context.Context, scope EvidenceScope) error {
	key, err := evidenceScopeKey(scope)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	finish := p.beginEvidenceDeletion()
	defer finish()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, evidenceHeadsWriter); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, key, NormalizeOrgID(scope.OrgID), scope.ClusterID, scope.ClusterUID)
	if err != nil {
		return err
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT checkpoint FROM kubernetes_evidence_scopes WHERE scope_key=$1 FOR UPDATE`, key).Scan(&raw); err != nil {
		return err
	}
	var checkpoint EvidenceCheckpoint
	if err = json.Unmarshal(raw, &checkpoint); err != nil {
		return err
	}
	if checkpoint.Generation == ^uint64(0) {
		return ErrEvidenceLimit
	}
	for _, table := range []string{"kubernetes_evidence_observations_retired", "kubernetes_evidence_events_retired"} {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			return err
		}
		if exists {
			if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE scope_key=$1", key); err != nil {
				return err
			}
		}
	}
	for _, table := range []string{"kubernetes_evidence_occurrence_observations", "kubernetes_evidence_occurrence_read_versions", "kubernetes_evidence_occurrence_versions", "kubernetes_evidence_event_heads", "kubernetes_evidence_events", "kubernetes_evidence_issues", "kubernetes_evidence_captures"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE scope_key=$1", key); err != nil {
			return err
		}
	}
	raw, err = json.Marshal(EvidenceCheckpoint{Generation: checkpoint.Generation + 1, CommitRevision: max(checkpoint.CommitRevision, checkpoint.OccurrenceRevision) + 1})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=$2,high_sequence=0,event_counts='{}',read_model_ready=TRUE,backfill_sequence=0,backfill_occurrence='',heads_backfill=NULL,gap_reach=NULL,observed_lag='0',observed_lag_scan=NULL WHERE scope_key=$1`, key, raw); err != nil {
		return err
	}
	return tx.Commit()
}

func nullEvidenceTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

type evidenceCountSnapshot struct {
	Total   int
	Counts  map[string]int
	Expires time.Time
}

func cloneEvidenceCounts(counts map[string]int) map[string]int {
	copy := make(map[string]int, len(counts))
	for kind, count := range counts {
		copy[kind] = count
	}
	return copy
}

func (p *postgresProvider) evidenceCounts(key string, generations ...uint64) (int, map[string]int, bool) {
	p.evidenceQueryMu.Lock()
	defer p.evidenceQueryMu.Unlock()
	if p.evidenceDeleting > 0 || len(generations) > 0 && generations[0] != p.evidenceQueryGeneration {
		return 0, nil, false
	}
	snapshot, found := p.evidenceQueries[key]
	if !found || !snapshot.Expires.After(time.Now()) {
		if found {
			delete(p.evidenceQueries, key)
		}
		return 0, nil, false
	}
	return snapshot.Total, cloneEvidenceCounts(snapshot.Counts), true
}

// This bounded cache avoids recounting an unchanged indexed prefix on every
// page. Its lifetime limits query work only; it never expires retained rows.
func (p *postgresProvider) rememberEvidenceCounts(key string, total int, counts map[string]int, generations ...uint64) {
	p.evidenceQueryMu.Lock()
	defer p.evidenceQueryMu.Unlock()
	if p.evidenceDeleting > 0 || len(generations) > 0 && generations[0] != p.evidenceQueryGeneration {
		return
	}
	now := time.Now()
	if p.evidenceQueries == nil {
		p.evidenceQueries = map[string]evidenceCountSnapshot{}
	}
	oldestKey := ""
	oldest := time.Time{}
	for candidate, snapshot := range p.evidenceQueries {
		if !snapshot.Expires.After(now) {
			delete(p.evidenceQueries, candidate)
			continue
		}
		if oldest.IsZero() || snapshot.Expires.Before(oldest) {
			oldestKey, oldest = candidate, snapshot.Expires
		}
	}
	if len(p.evidenceQueries) >= 128 {
		delete(p.evidenceQueries, oldestKey)
	}
	p.evidenceQueries[key] = evidenceCountSnapshot{Total: total, Counts: cloneEvidenceCounts(counts), Expires: now.Add(10 * time.Minute)}
}

func (p *postgresProvider) evidenceCountGeneration() uint64 {
	p.evidenceQueryMu.Lock()
	defer p.evidenceQueryMu.Unlock()
	return p.evidenceQueryGeneration
}
func (p *postgresProvider) beginEvidenceDeletion() func() {
	p.evidenceQueryMu.Lock()
	p.evidenceQueryGeneration++
	p.evidenceDeleting++
	p.evidenceQueries = nil
	p.evidenceQueryMu.Unlock()
	return func() {
		p.evidenceQueryMu.Lock()
		p.evidenceQueryGeneration++
		p.evidenceDeleting--
		p.evidenceQueries = nil
		p.evidenceQueryMu.Unlock()
	}
}
