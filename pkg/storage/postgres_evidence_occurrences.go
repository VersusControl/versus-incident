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
	versions := []map[string]any{}
	for version, issue := range state.OccurrenceVersions {
		versions = append(versions, map[string]any{"payload_version": version, "occurrence_id": issue.OccurrenceID, "payload": issue})
	}
	if len(versions) > 0 {
		raw, err := json.Marshal(versions)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_occurrence_versions(scope_key,payload_version,occurrence_id,payload)
 SELECT $1,x.* FROM jsonb_to_recordset($2::jsonb) AS x(payload_version text,occurrence_id text,payload jsonb) ON CONFLICT DO NOTHING`, key, raw); err != nil {
			return err
		}
	}
	observations := []map[string]any{}
	for _, observation := range state.Observations {
		observations = append(observations, map[string]any{"occurrence_id": observation.OccurrenceID, "revision": observation.Revision, "observed_at": observation.At, "last_seen": observation.LastSeen, "payload_version": observation.Version, "measurements": observation.Measurements})
	}
	if len(observations) > 0 {
		months := map[string]time.Time{}
		for _, observation := range state.Observations {
			months[observation.At.UTC().Format("2006-01")] = observation.At
		}
		for _, at := range months {
			if _, err := tx.ExecContext(ctx, `SELECT versus_ensure_evidence_partition(to_regclass('kubernetes_evidence_occurrence_observations'),$1)`, at); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(observations)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_occurrence_observations(scope_key,occurrence_id,revision,observed_at,last_seen,payload_version,measurements)
 SELECT $1,x.* FROM jsonb_to_recordset($2::jsonb) AS x(occurrence_id text,revision bigint,observed_at timestamptz,last_seen timestamptz,payload_version text,measurements jsonb) ON CONFLICT DO NOTHING`, key, raw)
		return err
	}
	return nil
}

func (p *postgresProvider) queryOccurrences(parent context.Context, scope EvidenceScope, query EvidenceQuery) (EvidencePage, error) {
	if !query.PointInTime {
		return p.queryOccurrenceModels(parent, scope, query)
	}
	if query.OccurrenceID == "" {
		return EvidencePage{}, ErrInvalidEvidence
	}
	return p.queryOccurrenceSnapshots(parent, scope, query)
}

// queryOccurrenceSnapshots answers a point-in-time read of one occurrence.
func (p *postgresProvider) queryOccurrenceSnapshots(parent context.Context, scope EvidenceScope, query EvidenceQuery) (EvidencePage, error) {
	if query.OccurrenceID == "" {
		return EvidencePage{}, ErrInvalidEvidence
	}
	query, cursor, err := prepareEvidenceQuery(scope, query)
	if err != nil {
		return EvidencePage{}, err
	}
	if query.Cursor != "" && cursor.Ordering != "last_seen" {
		return EvidencePage{}, ErrEvidenceConflict
	}
	cursor.Ordering = "last_seen"
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
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	tx, err := p.beginEvidenceRead(ctx)
	if err != nil {
		return EvidencePage{}, err
	}
	defer tx.Rollback()
	page := EvidencePage{Since: query.Since, Until: query.Until, Policy: p.EvidencePolicy(), Counts: map[string]int{}}
	generations := make([]uint64, len(keys))
	highs := make([]uint64, len(keys))
	prunes := make([]uint64, len(keys))
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
				prunes[i] = cp.PruneRevision
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
		cursor.PruneRevisions = prunes
	} else {
		if len(cursor.Generations) != len(keys) || len(cursor.OccurrenceHighs) != len(keys) {
			return page, ErrInvalidEvidence
		}
		for i := range keys {
			if cursor.Generations[i] != generations[i] || cursor.OccurrenceHighs[i] > highs[i] || len(cursor.PruneRevisions) != len(keys) || cursor.PruneRevisions[i] != prunes[i] {
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
	args := []any{keys, query.Since, query.Until, values, query.OccurrenceID}
	// The repeatable-read transaction pins all scopes together on page one. Later
	// transactions explicitly constrain every observation to that same prefix.
	// One indexed latest-revision probe per scope for the requested occurrence.
	visible := `SELECT s.scope_key,o.occurrence_id,o.revision,o.observed_at,o.last_seen,o.payload_version,o.measurements FROM unnest($1::text[])s(scope_key)
 JOIN LATERAL(SELECT occurrence_id,revision,observed_at,last_seen,payload_version,measurements FROM kubernetes_evidence_occurrence_observations
 WHERE scope_key=s.scope_key AND occurrence_id=$5 AND revision<=($4::bigint[])[array_position($1::text[],s.scope_key)] AND observed_at<=$3 ORDER BY revision DESC LIMIT 1)o ON TRUE`
	cte := `WITH visible AS (` + visible + `), preferred AS (
 SELECT DISTINCT ON(o.occurrence_id) o.occurrence_id,o.revision,o.observed_at,o.last_seen,v.payload,o.measurements
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
	if !query.Center.IsZero() && query.OccurrenceID == "" {
		args = append(args, query.Center)
		where += fmt.Sprintf(` AND(payload->'payload'->>'broke_at')::timestamptz=$%d`, len(args))
	}
	allowed, err := evidenceAuthoritySQL(query, &args, "COALESCE(payload->>'authorization_kind','')", "COALESCE(payload->>'namespace','')")
	if err != nil {
		return page, err
	}
	if query.Cursor == "" {
		countRows, countErr := tx.QueryContext(ctx, cte+`SELECT type,severity,broken,COUNT(*) FROM(
 SELECT CASE WHEN NOT COALESCE(`+allowed+`,FALSE) THEN 'coverage_gap' WHEN payload->>'closed_at' IS NOT NULL THEN 'issue_resolved' WHEN payload->>'observation_only'='true' THEN 'issue_observed' ELSE 'issue_opened' END AS type,
 CASE WHEN `+allowed+` THEN COALESCE(payload->'payload'->>'severity','') ELSE '' END AS severity,
 CASE WHEN `+allowed+` THEN COALESCE((payload->>'broken')::boolean,FALSE) ELSE FALSE END AS broken
 FROM preferred WHERE `+where+` LIMIT 1001) bounded GROUP BY type,severity,broken`, args...)
		if countErr != nil {
			return page, countErr
		}
		for countRows.Next() {
			var kind, severity string
			var broken bool
			var count int
			if err = countRows.Scan(&kind, &severity, &broken, &count); err != nil {
				countRows.Close()
				return page, err
			}
			page.Total += count
			page.Counts[kind] += count
			if severity != "" {
				page.Counts["severity:"+severity] += count
				if broken {
					page.Counts["broken:"+severity] += count
				}
			}
		}
		if err = countRows.Err(); err != nil {
			countRows.Close()
			return page, err
		}
		countRows.Close()
		page.CountCapped = page.Total > 1000
	}
	if query.Cursor != "" {
		args = append(args, cursor.At, cursor.ID)
		where += fmt.Sprintf(" AND (%s<$%d OR (%s=$%d AND occurrence_id<$%d))", column, len(args)-1, column, len(args)-1, len(args))
	}
	rowLimit := query.Limit + 1
	args = append(args, rowLimit)
	rows, err = tx.QueryContext(ctx, cte+`SELECT occurrence_id,revision,observed_at,last_seen,COALESCE(`+allowed+`,FALSE),CASE WHEN `+allowed+` THEN payload ELSE '{}'::jsonb END,CASE WHEN `+allowed+` THEN measurements END FROM preferred WHERE `+where+` ORDER BY `+column+` DESC,occurrence_id DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var observation occurrenceObservation
		var raw []byte
		var issue EvidenceIssue
		var authorized bool
		if err = rows.Scan(&observation.OccurrenceID, &observation.Revision, &observation.At, &observation.LastSeen, &authorized, &raw, &observation.Measurements); err != nil {
			rows.Close()
			return page, err
		}
		if authorized && json.Unmarshal(raw, &issue) != nil {
			rows.Close()
			return page, ErrInvalidEvidence
		}
		record := occurrenceRecord(issue, observation)
		if !authorized {
			record = projectEvidenceAuthority(EvidenceRecord{ID: observation.OccurrenceID, At: observation.LastSeen, ObservedAt: observation.At}, EvidenceQuery{EnforceAuthority: true})
		}
		record = projectEvidenceAuthority(record, query)
		if query.Projection != nil {
			record = query.Projection(record)
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
