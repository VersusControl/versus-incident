package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func writeOccurrenceVersions(ctx context.Context, tx *sql.Tx, key string, state evidenceState) error {
	for version, issue := range state.OccurrenceVersions {
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_occurrence_versions(scope_key,payload_version,occurrence_id,payload) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, key, version, issue.OccurrenceID, raw); err != nil {
			return err
		}
	}
	for _, observation := range state.Observations {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_occurrence_observations(scope_key,occurrence_id,revision,observed_at,last_seen,payload_version) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, key, observation.OccurrenceID, int64(observation.Revision), observation.At, observation.LastSeen, observation.Version); err != nil {
			return err
		}
	}
	return nil
}

func (p *postgresProvider) queryOccurrences(parent context.Context, scope EvidenceScope, query EvidenceQuery) (EvidencePage, error) {
	query, cursor, err := prepareEvidenceQuery(scope, query)
	if err != nil {
		return EvidencePage{}, err
	}
	scopes, err := evidenceReadScopes(scope)
	if err != nil {
		return EvidencePage{}, err
	}
	keys := make([]string, len(scopes))
	for i, s := range scopes {
		keys[i], err = evidenceScopeKey(s)
		if err != nil {
			return EvidencePage{}, err
		}
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return EvidencePage{}, err
	}
	defer tx.Rollback()
	page := EvidencePage{Since: query.Since, Until: query.Until, Policy: p.EvidencePolicy(), Counts: map[string]int{}}
	generations := make([]uint64, len(keys))
	highs := make([]uint64, len(keys))
	rows, err := tx.QueryContext(ctx, `SELECT scope_key,checkpoint FROM kubernetes_evidence_scopes WHERE scope_key=ANY($1::text[])`, keys)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var key string
		var raw []byte
		var cp EvidenceCheckpoint
		if err = rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return page, err
		}
		if err = json.Unmarshal(raw, &cp); err != nil {
			rows.Close()
			return page, err
		}
		for i, k := range keys {
			if k == key {
				generations[i], highs[i] = cp.Generation, cp.OccurrenceRevision
			}
		}
		page.Checkpoint = mergeEvidenceCheckpoint(page.Checkpoint, cp)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return page, err
	}
	rows.Close()
	if query.Cursor == "" {
		cursor.Generations = generations
		cursor.OccurrenceHighs = highs
	} else {
		if len(cursor.Generations) != len(keys) || len(cursor.OccurrenceHighs) != len(keys) {
			return page, ErrInvalidEvidence
		}
		for i := range keys {
			if cursor.Generations[i] != generations[i] || cursor.OccurrenceHighs[i] > highs[i] {
				return page, ErrEvidenceConflict
			}
		}
	}
	values := make([]int64, len(keys))
	for i, high := range cursor.OccurrenceHighs {
		if high > uint64(1<<63-1) {
			return page, ErrInvalidEvidence
		}
		values[i] = int64(high)
	}
	args := []any{keys, query.Since, query.Until, values}
	// The repeatable-read transaction pins all scopes together on page one. Later
	// transactions explicitly constrain every observation to that same prefix.
	cte := `WITH visible AS (
 SELECT DISTINCT ON(scope_key,occurrence_id) scope_key,occurrence_id,revision,observed_at,last_seen,payload_version
 FROM kubernetes_evidence_occurrence_observations
 WHERE scope_key=ANY($1::text[]) AND revision<=($4::bigint[])[array_position($1::text[],scope_key)] AND observed_at<=$3
 ORDER BY scope_key,occurrence_id,revision DESC
 ), preferred AS (
 SELECT DISTINCT ON(o.occurrence_id) o.occurrence_id,o.revision,o.observed_at,o.last_seen,v.payload
 FROM visible o JOIN kubernetes_evidence_occurrence_versions v ON v.scope_key=o.scope_key AND v.payload_version=o.payload_version
 ORDER BY o.occurrence_id,array_position($1::text[],o.scope_key)
 ) `
	column := "last_seen"
	if query.ByObservation {
		column = "observed_at"
	}
	where := column + ` >= $2 AND ` + column + ` <= $3`
	for _, filter := range []struct{ column, value string }{{"namespace", query.Namespace}, {"kind", query.Kind}, {"uid", query.UID}, {"uid", query.RootUID}, {"rule", query.Rule}, {"uid", query.ResourceUID}} {
		if filter.value != "" {
			args = append(args, filter.value)
			where += fmt.Sprintf(" AND payload->>'%s'=$%d", filter.column, len(args))
		}
	}
	if query.PodUID != "" || query.Name != "" {
		where += " AND FALSE"
	}
	severity, broken, _ := strings.Cut(query.Variant, ":")
	if severity != "" {
		args = append(args, severity)
		where += fmt.Sprintf(" AND payload->'payload'->>'severity'=$%d", len(args))
	}
	if broken == "broken" {
		where += ` AND payload->>'broken'='true'`
	}
	var opened, observed, resolved int
	if query.Projection == nil {
		err = tx.QueryRowContext(ctx, cte+`SELECT COUNT(*),COUNT(*) FILTER(WHERE payload->>'closed_at' IS NULL AND COALESCE(payload->>'observation_only','false')<>'true'),COUNT(*) FILTER(WHERE payload->>'closed_at' IS NULL AND payload->>'observation_only'='true'),COUNT(*) FILTER(WHERE payload->>'closed_at' IS NOT NULL) FROM preferred WHERE `+where, args...).Scan(&page.Total, &opened, &observed, &resolved)
		if err != nil {
			return page, err
		}
		if opened > 0 {
			page.Counts["issue_opened"] = opened
		}
		if observed > 0 {
			page.Counts["issue_observed"] = observed
		}
		if resolved > 0 {
			page.Counts["issue_resolved"] = resolved
		}
	}
	if query.Cursor != "" && query.Projection == nil {
		args = append(args, cursor.At, cursor.ID)
		where += fmt.Sprintf(" AND (%s<$%d OR (%s=$%d AND occurrence_id<$%d))", column, len(args)-1, column, len(args)-1, len(args))
	}
	rowLimit := query.Limit + 1
	if query.Projection != nil {
		rowLimit = 100001
	}
	args = append(args, rowLimit)
	rows, err = tx.QueryContext(ctx, cte+`SELECT occurrence_id,revision,observed_at,last_seen,payload FROM preferred WHERE `+where+` ORDER BY `+column+` DESC,occurrence_id DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return page, err
	}
	scanned := 0
	for rows.Next() {
		var observation occurrenceObservation
		var raw []byte
		var issue EvidenceIssue
		if err = rows.Scan(&observation.OccurrenceID, &observation.Revision, &observation.At, &observation.LastSeen, &raw); err != nil {
			rows.Close()
			return page, err
		}
		if err = json.Unmarshal(raw, &issue); err != nil {
			rows.Close()
			return page, err
		}
		record := occurrenceRecord(issue, observation)
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
