package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const occurrenceMetadata = `scope_key,occurrence_id,payload_version,revision,first_seen,closed_at,namespace,kind,root_uid,rule,severity,authorization_kind,broken,observation_only`

func occurrenceCandidates(keys []string, q EvidenceQuery, c evidenceCursor, cap int, next bool, args *historySQLArgs) string {
	branches := []string{}
	for i, key := range keys {
		scope, pin := args.add(key), args.add(c.OccurrenceHighs[i])
		since, until := args.add(q.Since), args.add(q.Until)
		where := "i.scope_key=" + scope + " AND i.first_seen<=" + until + " AND(i.closed_at IS NULL OR i.closed_at>=" + since + ") AND i.revision<=" + pin +
			" AND(i.superseded_revision IS NULL OR i.superseded_revision>" + pin + " OR i.superseded_at>" + until + ") AND i.observed_at<=" + until
		if i > 0 {
			where += " AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_read_versions p WHERE p.scope_key=" + args.add(keys[0]) +
				" AND p.occurrence_id=i.occurrence_id AND p.revision<=" + args.add(c.OccurrenceHighs[0]) + " AND p.observed_at<=" + until + ")"
		}
		for _, f := range []struct{ col, value string }{{"occurrence_id", q.OccurrenceID}, {"root_uid", q.RootUID}, {"root_uid", q.UID}, {"root_uid", q.ResourceUID}, {"namespace", q.Namespace}, {"kind", q.Kind}, {"rule", q.Rule}} {
			if f.value != "" {
				where += " AND i." + f.col + "=" + args.add(f.value)
			}
		}
		if !q.Center.IsZero() {
			where += " AND i.broke_at=" + args.add(q.Center)
		}
		if q.Name != "" || q.PodUID != "" {
			where += " AND FALSE"
		}
		severity, broken, _ := strings.Cut(q.Variant, ":")
		if severity != "" {
			where += " AND i.severity=" + args.add(severity)
		}
		if broken == "broken" {
			where += " AND i.broken"
		}
		if next && !c.At.IsZero() {
			where += " AND(i.first_seen,i.occurrence_id)<(" + args.add(c.At) + "," + args.add(c.ID) + ")"
		}
		branches = append(branches, "(SELECT "+occurrenceMetadata+" FROM kubernetes_evidence_occurrence_read_versions i WHERE "+where+
			" ORDER BY first_seen DESC,occurrence_id DESC LIMIT "+strconv.Itoa(cap)+")")
	}
	return "SELECT * FROM(" + strings.Join(branches, " UNION ALL ") + ") visible ORDER BY first_seen DESC,occurrence_id DESC LIMIT " + strconv.Itoa(cap)
}

func (p *postgresProvider) queryOccurrenceReadPage(ctx context.Context, tx *sql.Tx, keys []string, q EvidenceQuery, c evidenceCursor, page EvidencePage) (EvidencePage, error) {
	for _, high := range c.OccurrenceHighs {
		if high > uint64(1<<63-1) {
			return page, ErrInvalidEvidence
		}
	}
	if q.Cursor == "" && q.OccurrenceID == "" && q.Center.IsZero() {
		args := historySQLArgs{}
		candidates := occurrenceCandidates(keys, q, c, 1001, false, &args)
		values := []any(args)
		allowed, err := evidenceAuthoritySQL(q, &values, "m.authorization_kind", "m.namespace")
		if err != nil {
			return page, err
		}
		rows, err := tx.QueryContext(ctx, `WITH candidates AS MATERIALIZED(`+candidates+`)
 SELECT type,severity,broken,COUNT(*) FROM(SELECT CASE WHEN NOT COALESCE(`+allowed+`,FALSE) THEN 'coverage_gap'
 WHEN m.closed_at IS NOT NULL THEN 'issue_resolved' WHEN m.observation_only THEN 'issue_observed' ELSE 'issue_opened' END AS type,
 CASE WHEN `+allowed+` THEN m.severity ELSE '' END AS severity,CASE WHEN `+allowed+` THEN m.broken ELSE FALSE END AS broken FROM candidates m) bounded GROUP BY type,severity,broken`, values...)
		if err != nil {
			return page, err
		}
		for rows.Next() {
			var kind, severity string
			var broken bool
			var count int
			if err = rows.Scan(&kind, &severity, &broken, &count); err != nil {
				rows.Close()
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
		if err = rows.Err(); err != nil {
			rows.Close()
			return page, err
		}
		rows.Close()
		page.CountCapped = page.Total > 1000
	}
	args := historySQLArgs{}
	candidates := occurrenceCandidates(keys, q, c, q.Limit+1, true, &args)
	pin := "CASE m.scope_key"
	for i, key := range keys {
		pin += " WHEN " + args.add(key) + "::text THEN " + args.add(c.OccurrenceHighs[i]) + "::bigint"
	}
	pin += " END"
	until := args.add(q.Until)
	values := []any(args)
	allowed, err := evidenceAuthoritySQL(q, &values, "m.authorization_kind", "m.namespace")
	if err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `WITH candidates AS MATERIALIZED(`+candidates+`)
 SELECT m.occurrence_id,m.first_seen,COALESCE(`+allowed+`,FALSE),
 CASE WHEN `+allowed+` THEN v.payload ELSE '{}'::jsonb END,o.last_seen,o.observed_at,o.measurements
 FROM candidates m LEFT JOIN LATERAL(SELECT payload FROM kubernetes_evidence_occurrence_versions v
 WHERE v.scope_key=m.scope_key AND v.payload_version=m.payload_version LIMIT 1)v ON `+allowed+`
 LEFT JOIN LATERAL(SELECT last_seen,observed_at,measurements FROM kubernetes_evidence_occurrence_observations o
 WHERE o.scope_key=m.scope_key AND o.occurrence_id=m.occurrence_id AND o.payload_version=m.payload_version AND o.revision<=`+pin+`
 AND o.observed_at<=`+until+` ORDER BY revision DESC LIMIT 1)o ON `+allowed+`
 ORDER BY m.first_seen DESC,m.occurrence_id DESC`, values...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var id string
		var first time.Time
		var authorized bool
		var raw, measurements []byte
		var seen, observed sql.NullTime
		if err = rows.Scan(&id, &first, &authorized, &raw, &seen, &observed, &measurements); err != nil {
			rows.Close()
			return page, err
		}
		record := EvidenceRecord{ID: id, At: first, ObservedAt: first}
		if authorized && seen.Valid && observed.Valid {
			var issue EvidenceIssue
			if err = json.Unmarshal(raw, &issue); err != nil {
				rows.Close()
				return page, err
			}
			record = occurrenceRecord(issue, occurrenceObservation{At: observed.Time, LastSeen: seen.Time, Measurements: measurements})
			record.At = first
		} else {
			record = projectEvidenceAuthority(record, EvidenceQuery{EnforceAuthority: true})
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
	for i, record := range page.Items {
		if q.Projection != nil {
			page.Items[i] = q.Projection(record)
		}
	}
	return finishEvidencePage(page, q, c), nil
}
