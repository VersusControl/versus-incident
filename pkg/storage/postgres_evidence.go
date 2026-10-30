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
	return EvidencePolicy{Mode: "unbounded_database"}
}

func (p *postgresProvider) CommitEvidence(parent context.Context, scope EvidenceScope, batch EvidenceBatch) error {
	key, err := evidenceScopeKey(scope)
	if err != nil {
		return err
	}
	if err = validEvidenceBatch(batch); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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

	_, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, key, NormalizeOrgID(scope.OrgID), scope.ClusterID, scope.ClusterUID)
	if err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT checkpoint FROM kubernetes_evidence_scopes WHERE scope_key=$1 FOR UPDATE`, key).Scan(&raw)
	if err != nil {
		return err
	}
	state := evidenceState{Active: map[string]EvidenceIssue{}, OccurrenceVersions: map[string]EvidenceIssue{}, OccurrenceHeads: map[string]string{}}
	if err = json.Unmarshal(raw, &state.Checkpoint); err != nil {
		return err
	}
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
	rows, err := tx.QueryContext(ctx, `SELECT payload,last_seen,payload_version FROM kubernetes_evidence_issues WHERE scope_key=$1 AND closed_at IS NULL`, key)
	if err != nil {
		return err
	}
	for rows.Next() {
		var data []byte
		var lastSeen time.Time
		var version string
		if err = rows.Scan(&data, &lastSeen, &version); err != nil {
			rows.Close()
			return err
		}
		var issue EvidenceIssue
		if err = json.Unmarshal(data, &issue); err != nil {
			rows.Close()
			return err
		}
		issue.LastSeen = lastSeen
		state.Active[issue.Key] = issue
		state.OccurrenceHeads[issue.OccurrenceID] = version
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
	for _, record := range records {
		_, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_events(scope_key,event_id,event_type,event_at,observed_at,namespace,resource_name,kind,resource_uid,root_uid,pod_uid,rule,epoch,payload,is_break,gap_from,gap_to,priority,event_version,authorization_kind) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20 WHERE NOT EXISTS(SELECT 1 FROM kubernetes_evidence_events preferred WHERE preferred.scope_key=$1 AND preferred.event_id=$2 AND (preferred.priority>$18 OR (preferred.priority=$18 AND preferred.observed_at>$5))) ON CONFLICT(scope_key,event_id,event_version) DO NOTHING`, key, record.ID, record.Type, record.At, record.ObservedAt, record.Namespace, record.Name, record.Kind, record.UID, record.RootUID, record.PodUID, record.Rule, record.Epoch, []byte(record.Payload), record.Broken, nullEvidenceTime(record.GapFrom), nullEvidenceTime(record.GapTo), record.Priority, evidenceVersion(record), record.AuthorizationKind)
		if err != nil {
			return err
		}
		if record.Type == "issue_opened" || record.Type == "issue_observed" || record.Type == "issue_resolved" {
			var issue EvidenceIssue
			if err = json.Unmarshal(record.Payload, &issue); err != nil {
				return err
			}
			if issue.ObservationOnly {
				continue
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_issues(scope_key,occurrence_id,issue_key,first_seen,last_seen,closed_at,payload,payload_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(scope_key,occurrence_id) DO UPDATE SET last_seen=EXCLUDED.last_seen,closed_at=EXCLUDED.closed_at,payload=EXCLUDED.payload,payload_version=EXCLUDED.payload_version`, key, issue.OccurrenceID, issue.Key, issue.FirstSeen, issue.LastSeen, issue.ClosedAt, []byte(record.Payload), state.OccurrenceHeads[issue.OccurrenceID])
			if err != nil {
				return err
			}
		}
	}
	if err = writeOccurrenceVersions(ctx, tx, key, state); err != nil {
		return err
	}
	// Freshness is mutable occurrence state, not an immutable journal event.
	for _, issue := range state.Active {
		_, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_issues SET last_seen=$3 WHERE scope_key=$1 AND occurrence_id=$2 AND last_seen<$3`, key, issue.OccurrenceID, issue.LastSeen)
		if err != nil {
			return err
		}
	}
	raw, err = json.Marshal(state.Checkpoint)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=$2 WHERE scope_key=$1`, key, raw)
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
	err = p.db.QueryRowContext(ctx, `SELECT checkpoint FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return EvidenceCheckpoint{}, nil
	}
	if err != nil {
		return EvidenceCheckpoint{}, err
	}
	var checkpoint EvidenceCheckpoint
	err = json.Unmarshal(raw, &checkpoint)
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
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return EvidencePage{}, err
	}
	defer tx.Rollback()
	countGeneration := p.evidenceCountGeneration()
	page := EvidencePage{Policy: p.EvidencePolicy(), Since: query.Since, Until: query.Until, Counts: map[string]int{}}
	generations := make([]uint64, len(keys))
	checkpointRows, err := tx.QueryContext(ctx, `SELECT scope_key,checkpoint FROM kubernetes_evidence_scopes WHERE scope_key=ANY($1::text[]) ORDER BY array_position($1::text[],scope_key)`, keys)
	if err != nil {
		return page, err
	}
	for checkpointRows.Next() {
		var raw []byte
		var checkpoint EvidenceCheckpoint
		var key string
		if err = checkpointRows.Scan(&key, &raw); err != nil {
			checkpointRows.Close()
			return page, err
		}
		if err = json.Unmarshal(raw, &checkpoint); err != nil {
			checkpointRows.Close()
			return page, err
		}
		for i, candidate := range keys {
			if candidate == key {
				generations[i] = checkpoint.Generation
			}
		}
		page.Checkpoint = mergeEvidenceCheckpoint(page.Checkpoint, checkpoint)
	}
	if err = checkpointRows.Err(); err != nil {
		checkpointRows.Close()
		return page, err
	}
	checkpointRows.Close()

	if query.Cursor == "" {
		cursor.Generations = generations
		cursor.Highs = make([]uint64, len(keys))
		highs, err := tx.QueryContext(ctx, `SELECT scope_key,MAX(sequence) FROM kubernetes_evidence_events WHERE scope_key=ANY($1::text[]) GROUP BY scope_key`, keys)
		if err != nil {
			return page, err
		}
		for highs.Next() {
			var key string
			var high uint64
			if err = highs.Scan(&key, &high); err != nil {
				highs.Close()
				return page, err
			}
			for i, candidate := range keys {
				if candidate == key {
					cursor.Highs[i] = high
				}
			}
		}
		if err = highs.Err(); err != nil {
			highs.Close()
			return page, err
		}
		highs.Close()
	} else if len(cursor.Generations) != len(keys) || len(cursor.Highs) != len(keys) {
		return page, ErrInvalidEvidence
	}
	for i, generation := range generations {
		if cursor.Generations[i] != generation {
			return page, ErrEvidenceConflict
		}
	}
	highValues := make([]int64, len(keys))
	for i, high := range cursor.Highs {
		if high > uint64(1<<63-1) {
			return page, ErrInvalidEvidence
		}
		highValues[i] = int64(high)
	}
	args := []any{keys, query.Since, query.Until, highValues}

	timeColumn := "event_at"
	if query.ByObservation {
		timeColumn = "observed_at"
	}
	where := `scope_key=ANY($1::text[]) AND ` + timeColumn + ` >= $2 AND ` + timeColumn + ` <= $3 AND sequence <= ($4::bigint[])[array_position($1::text[],scope_key)]`
	if query.WithGaps {
		where = `scope_key=ANY($1::text[]) AND sequence<=($4::bigint[])[array_position($1::text[],scope_key)] AND ((event_type<>'coverage_gap' AND ` + timeColumn + ` >= $2 AND ` + timeColumn + ` <= $3) OR (event_type='coverage_gap' AND gap_to >= $2 AND gap_from <= $3))`
	}

	where += ` AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_events newer WHERE newer.scope_key=kubernetes_evidence_events.scope_key AND newer.event_id=kubernetes_evidence_events.event_id AND newer.sequence>kubernetes_evidence_events.sequence AND newer.sequence<=($4::bigint[])[array_position($1::text[],kubernetes_evidence_events.scope_key)])`
	if len(keys) > 1 {
		args = append(args, keys[0])
		where += ` AND (scope_key=$5 OR NOT EXISTS(SELECT 1 FROM kubernetes_evidence_events higher WHERE higher.scope_key=$5 AND higher.event_id=kubernetes_evidence_events.event_id AND higher.sequence<=($4::bigint[])[1]))`
	}
	for _, filter := range []struct{ column, value string }{{"namespace", query.Namespace}, {"resource_name", query.Name}, {"kind", query.Kind}, {"resource_uid", query.UID}, {"root_uid", query.RootUID}, {"pod_uid", query.PodUID}, {"rule", query.Rule}, {"event_type", ""}} {
		if filter.value != "" {
			args = append(args, filter.value)
			if query.WithGaps {
				if filter.column == "namespace" || filter.column == "kind" {
					where += fmt.Sprintf(" AND (%s=$%d OR(event_type='coverage_gap' AND %s=''))", filter.column, len(args), filter.column)
				} else {
					where += fmt.Sprintf(" AND (event_type='coverage_gap' OR %s=$%d)", filter.column, len(args))
				}
			} else {
				where += fmt.Sprintf(" AND %s=$%d", filter.column, len(args))
			}
		}
	}
	if query.ResourceUID != "" {
		args = append(args, query.ResourceUID)
		if query.WithGaps {
			where += " AND (event_type='coverage_gap' OR "
		} else {
			where += " AND ("
		}
		where += fmt.Sprintf("resource_uid=$%d OR root_uid=$%d OR pod_uid=$%d)", len(args), len(args), len(args))
	}
	switch query.Type {
	case "timeline":
		where += " AND (event_type IN ('change','crash','coverage_gap') OR (is_break AND event_type IN ('issue_opened','issue_resolved')))"
	case "break":
		where += " AND ((is_break AND event_type IN ('issue_opened','issue_resolved')) OR event_type='coverage_gap')"
	case "issue":
		where += " AND event_type IN ('issue_opened','issue_observed','issue_resolved')"
	case "":
	default:
		args = append(args, query.Type)
		if query.WithGaps {
			where += fmt.Sprintf(" AND (event_type=$%d OR event_type='coverage_gap')", len(args))
		} else {
			where += fmt.Sprintf(" AND event_type=$%d", len(args))
		}
	}
	cacheKey := evidenceDigest(cursor.Binding, fmt.Sprint(cursor.Highs), fmt.Sprint(cursor.Generations))
	if total, counts, found := p.evidenceCounts(cacheKey, countGeneration); found && query.Projection == nil {
		page.Total, page.Counts = total, counts
	} else if query.Projection == nil {
		countRows, countErr := tx.QueryContext(ctx, `SELECT event_type,COUNT(*) FROM kubernetes_evidence_events WHERE `+where+` GROUP BY event_type`, args...)
		if countErr != nil {
			return page, countErr
		}
		for countRows.Next() {
			var eventType string
			var count int
			if err = countRows.Scan(&eventType, &count); err != nil {
				countRows.Close()
				return page, err
			}
			page.Counts[eventType] = count
			page.Total += count
		}
		if err = countRows.Err(); err != nil {
			countRows.Close()
			return page, err
		}
		countRows.Close()
		p.rememberEvidenceCounts(cacheKey, page.Total, page.Counts, countGeneration)
	}

	if !cursor.At.IsZero() && query.Projection == nil {
		args = append(args, cursor.At, cursor.ID)
		where += fmt.Sprintf(" AND (%s,event_id)<($%d,$%d)", timeColumn, len(args)-1, len(args))
	}
	rowLimit := query.Limit + 1
	if query.Projection != nil {
		// Read a bounded pinned snapshot to project accurate counts. Keep only
		// one output page in memory; never silently truncate a larger snapshot.
		rowLimit = 100001
	}
	args = append(args, rowLimit)
	rows, err := tx.QueryContext(ctx, `SELECT event_id,event_type,event_at,observed_at,namespace,resource_name,kind,resource_uid,root_uid,pod_uid,rule,epoch,payload,is_break,gap_from,gap_to,priority,authorization_kind FROM kubernetes_evidence_events WHERE `+where+fmt.Sprintf(` ORDER BY %s DESC,event_id DESC LIMIT $%d`, timeColumn, len(args)), args...)
	if err != nil {
		return page, err
	}
	scanned := 0
	for rows.Next() {
		var record EvidenceRecord
		var gapFrom, gapTo sql.NullTime
		if err = rows.Scan(&record.ID, &record.Type, &record.At, &record.ObservedAt, &record.Namespace, &record.Name, &record.Kind, &record.UID, &record.RootUID, &record.PodUID, &record.Rule, &record.Epoch, &record.Payload, &record.Broken, &gapFrom, &gapTo, &record.Priority, &record.AuthorizationKind); err != nil {
			rows.Close()
			return page, err
		}
		record.GapFrom, record.GapTo = gapFrom.Time, gapTo.Time
		if query.Projection != nil {
			scanned++
			if scanned > 100000 {
				rows.Close()
				return page, ErrEvidenceLimit
			}
			record = query.Projection(record)
			page.Total++
			page.Counts[record.Type]++
			at := evidenceTime(record, query)
			if !cursor.At.IsZero() && (at.After(cursor.At) || at.Equal(cursor.At) && record.ID >= cursor.ID) || len(page.Items) >= query.Limit+1 {
				continue
			}
		}
		page.Items = append(page.Items, record)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return page, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return page, err
	}
	return finishEvidencePage(page, query, cursor), nil
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
	for _, table := range []string{"kubernetes_evidence_occurrence_observations", "kubernetes_evidence_occurrence_versions", "kubernetes_evidence_events", "kubernetes_evidence_issues", "kubernetes_evidence_captures"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE scope_key=$1", key); err != nil {
			return err
		}
	}
	raw, err = json.Marshal(EvidenceCheckpoint{Generation: checkpoint.Generation + 1})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=$2 WHERE scope_key=$1`, key, raw); err != nil {
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
