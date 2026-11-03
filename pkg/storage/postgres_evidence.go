package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func (p *postgresProvider) EvidencePolicy() EvidencePolicy {
	return EvidencePolicy{Mode: "unbounded_database", Retention: p.evidenceRetention}
}

func (p *postgresProvider) CommitEvidence(parent context.Context, scope EvidenceScope, batch EvidenceBatch) error {
	key, err := evidenceScopeKey(scope)
	if err != nil {
		return err
	}
	if err = validEvidenceBatch(batch); err != nil {
		return err
	}
	if batch.ObservationHeartbeat == 0 {
		batch.ObservationHeartbeat = p.observationHeartbeat
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = prepareEvidencePartitions(ctx, tx, batch); err != nil {
		return err
	}
	if batch.GuardKey != "" {
		var actual []byte
		err = tx.QueryRowContext(ctx, fmt.Sprintf("SELECT data FROM %s WHERE name=$1 FOR UPDATE", blobTable(batch.GuardKey)), batch.GuardKey).Scan(&actual)
		if err == sql.ErrNoRows {
			return ErrEvidenceConflict
		}
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, batch.GuardExpected) {
			return ErrEvidenceConflict
		}
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid,read_model_ready) VALUES($1,$2,$3,$4,TRUE) ON CONFLICT DO NOTHING`, key, NormalizeOrgID(scope.OrgID), scope.ClusterID, scope.ClusterUID)
	if err != nil {
		return err
	}
	var raw []byte
	var storedHigh uint64
	err = tx.QueryRowContext(ctx, `SELECT checkpoint,high_sequence FROM kubernetes_evidence_scopes WHERE scope_key=$1 FOR UPDATE`, key).Scan(&raw, &storedHigh)
	if err != nil {
		return err
	}
	state := evidenceState{Active: map[string]EvidenceIssue{}, Occurrences: map[string]EvidenceIssue{}, OccurrenceVersions: map[string]EvidenceIssue{}, OccurrenceHeads: map[string]string{}}
	if err = json.Unmarshal(raw, &state.Checkpoint); err != nil {
		return err
	}
	state.Checkpoint.HighSequence = max(state.Checkpoint.HighSequence, storedHigh)
	if state.Checkpoint.Generation != batch.Generation {
		return ErrEvidenceConflict
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kubernetes_evidence_captures WHERE scope_key=$1 AND capture_id=$2)`, key, batch.ID).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return tx.Commit()
	}
	if state.Checkpoint.ID != batch.Expected || batch.At.Before(state.Checkpoint.At) {
		return ErrEvidenceConflict
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
		return err
	}
	for rows.Next() {
		var data []byte
		var lastSeen time.Time
		var version string
		var lastObserved sql.NullTime
		if err = rows.Scan(&data, &lastSeen, &version, &lastObserved); err != nil {
			rows.Close()
			return err
		}
		var issue EvidenceIssue
		if err = json.Unmarshal(data, &issue); err != nil {
			rows.Close()
			return err
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
		return err
	}
	rows.Close()
	records, err := applyEvidence(&state, batch)
	if err != nil {
		return err
	}
	high, err := writeEvidenceEvents(ctx, tx, key, records)
	if err != nil {
		return err
	}
	state.Checkpoint.HighSequence = max(state.Checkpoint.HighSequence, high)
	if err = writeOccurrenceVersions(ctx, tx, key, state); err != nil {
		return err
	}
	if err = writeOccurrenceReadModels(ctx, tx, key, state); err != nil {
		return err
	}
	raw, err = json.Marshal(state.Checkpoint)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=$2,high_sequence=GREATEST(high_sequence,$3),scope_binding=COALESCE(NULLIF($4,''),scope_binding),updated_at=$5 WHERE scope_key=$1`, key, raw, int64(state.Checkpoint.HighSequence), batch.ScopeBinding, batch.At)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_captures(scope_key,capture_id,observed_at) VALUES($1,$2,$3)`, key, batch.ID, batch.At)
	if err != nil {
		return err
	}
	return tx.Commit()
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
	for i, generation := range generations {
		if cursor.Generations[i] != generation {
			return page, ErrEvidenceConflict
		}
		if len(cursor.PruneRevisions) != len(keys) || cursor.PruneRevisions[i] != pruneRevisions[i] {
			return page, ErrEvidenceConflict
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
	for _, table := range []string{"kubernetes_evidence_occurrence_observations", "kubernetes_evidence_occurrence_read_versions", "kubernetes_evidence_occurrence_versions", "kubernetes_evidence_events", "kubernetes_evidence_issues", "kubernetes_evidence_captures"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE scope_key=$1", key); err != nil {
			return err
		}
	}
	raw, err = json.Marshal(EvidenceCheckpoint{Generation: checkpoint.Generation + 1, CommitRevision: max(checkpoint.CommitRevision, checkpoint.OccurrenceRevision) + 1})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=$2,high_sequence=0,event_counts='{}',read_model_ready=TRUE,backfill_sequence=0,backfill_occurrence='' WHERE scope_key=$1`, key, raw); err != nil {
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
