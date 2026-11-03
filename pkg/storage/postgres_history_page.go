package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

type historySQLArgs []any

func (a *historySQLArgs) add(value any) string {
	*a = append(*a, value)
	return "$" + strconv.Itoa(len(*a))
}

const eventMetadata = `scope_key,sequence,event_id,event_type,event_at,observed_at,namespace,resource_name,kind,resource_uid,root_uid,pod_uid,rule,epoch,is_break,gap_from,gap_to,priority,authorization_kind`

// historyTimelineCrashMarker mirrors the native timeline rule: a crash record is a
// timeline marker when it declares no subtype or declares the terminal marker. A
// declared status observation is not a timeline marker.
const historyTimelineCrashMarker = "(e.payload->>'type' IS NULL OR e.payload->>'type'='' OR e.payload->>'type'='termination')"

func journalCandidates(keys []string, q EvidenceQuery, c evidenceCursor, cap int, next bool, args *historySQLArgs) string {
	column := "event_at"
	if q.ByObservation {
		column = "observed_at"
	}
	branches := []string{}
	classes := []string{""}
	if q.Type == "timeline" {
		classes = []string{"change", "crash", "break"}
	}
	for i, key := range keys {
		for classIndex, class := range classes {
			for _, gap := range []bool{false, true} {
				if gap && (!q.WithGaps || classIndex > 0) {
					continue
				}
				scope, pin := args.add(key), args.add(c.Highs[i])
				where := "e.scope_key=" + scope + " AND e.sequence<=" + pin + " AND(e.superseded_seq IS NULL OR e.superseded_seq>" + pin + ")"
				if i > 0 {
					where += " AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_events p WHERE p.scope_key=" + args.add(keys[0]) + " AND p.event_id=e.event_id AND p.sequence<=" + args.add(c.Highs[0]) + ")"
				}
				from, to := args.add(q.Since), args.add(q.Until)
				if gap {
					where += " AND e.event_type='coverage_gap' AND e.gap_to>=" + from + " AND e.gap_from<=" + to
				} else {
					where += " AND e." + column + ">=" + from + " AND e." + column + "<=" + to
					if q.WithGaps {
						where += " AND e.event_type<>'coverage_gap'"
					}
					switch q.Type {
					case "timeline":
						switch class {
						case "change":
							where += ` AND e.event_type='change'`
						case "crash":
							where += ` AND e.event_type='crash' AND ` + historyTimelineCrashMarker
						case "break":
							where += ` AND e.is_break AND e.event_type IN('issue_opened','issue_resolved')`
						}
					case "break":
						where += ` AND e.is_break AND e.event_type IN('issue_opened','issue_resolved')`
					case "issue":
						where += ` AND e.event_type IN('issue_opened','issue_observed','issue_resolved')`
					case "":
					default:
						where += " AND e.event_type=" + args.add(q.Type)
					}
				}
				for _, f := range []struct{ col, value string }{{"namespace", q.Namespace}, {"resource_name", q.Name}, {"kind", q.Kind}, {"resource_uid", q.UID}, {"root_uid", q.RootUID}, {"rule", q.Rule}} {
					if f.value == "" {
						continue
					}
					if gap {
						if f.col == "namespace" || f.col == "kind" {
							where += " AND(e." + f.col + "=" + args.add(f.value) + " OR e." + f.col + "='')"
						}
					} else {
						where += " AND e." + f.col + "=" + args.add(f.value)
					}
				}
				if !gap && q.ResourceUID != "" {
					value := args.add(q.ResourceUID)
					if q.Type == "timeline" {
						where += " AND(e.resource_uid=" + value + " OR(e.event_type='crash' AND(e.root_uid=" + value + " OR e.pod_uid=" + value + ")))"
					} else {
						where += " AND(e.resource_uid=" + value + " OR e.root_uid=" + value + " OR e.pod_uid=" + value + ")"
					}
				}
				if !gap && q.PodUID != "" {
					value := args.add(q.PodUID)
					if q.Type == "timeline" {
						where += " AND(e.pod_uid=" + value + " OR(e.event_type='change' AND e.resource_uid=" + value + "))"
					} else {
						where += " AND e.pod_uid=" + value
					}
				}
				if next && !c.At.IsZero() {
					where += " AND(e." + column + ",e.event_id)<(" + args.add(c.At) + "," + args.add(c.ID) + ")"
				}
				branches = append(branches, "(SELECT "+eventMetadata+" FROM kubernetes_evidence_events e WHERE "+where+" ORDER BY "+column+" DESC,event_id DESC LIMIT "+strconv.Itoa(cap)+")")
			}
		}
	}
	return "SELECT * FROM(" + strings.Join(branches, " UNION ALL ") + ") visible ORDER BY " + column + " DESC,event_id DESC LIMIT " + strconv.Itoa(cap)
}

func (p *postgresProvider) queryJournalPage(ctx context.Context, tx *sql.Tx, keys []string, q EvidenceQuery, c evidenceCursor, page EvidencePage, countGeneration uint64, summary map[string]int) (EvidencePage, error) {
	for _, high := range c.Highs {
		if high > uint64(1<<63-1) {
			return page, ErrInvalidEvidence
		}
	}
	cacheKey := evidenceDigest(c.Binding, fmt.Sprint(c.Generations), fmt.Sprint(c.PruneRevisions), fmt.Sprint(q.Authorities), fmt.Sprint(q.AllowedKinds), fmt.Sprint(page.Checkpoint.CommitRevision))
	unfiltered := p.evidenceRetention <= 0 && len(keys) == 1 && !q.EnforceAuthority && q.Type != "timeline" && q.Type != "break" && q.Namespace == "" && q.Kind == "" && q.RootUID == "" && q.UID == "" && q.ResourceUID == "" && q.PodUID == "" && q.Name == "" && q.Rule == "" && !q.Since.After(page.Checkpoint.StartedAt) && !q.Until.Before(page.Checkpoint.At)
	if q.Cursor == "" && unfiltered {
		for kind, count := range summary {
			if evidenceTypeMatches(kind, q.Type, true) {
				page.Total += count
				page.Counts[kind] = count
			}
		}
	} else if q.Cursor == "" {
		if total, counts, ok := p.evidenceCounts(cacheKey, countGeneration); ok {
			page.Total, page.Counts = total, counts
		} else {
			args := historySQLArgs{}
			candidates := journalCandidates(keys, q, c, 1001, false, &args)
			values := []any(args)
			authority, err := evidenceAuthoritySQL(q, &values, "m.authorization_kind", "m.namespace")
			if err != nil {
				return page, err
			}
			rows, err := tx.QueryContext(ctx, `WITH candidates AS MATERIALIZED(`+candidates+`) SELECT type,COUNT(*) FROM(
 SELECT CASE WHEN m.event_type='coverage_gap' OR (`+authority+`) THEN m.event_type ELSE 'coverage_gap' END AS type FROM candidates m) bounded GROUP BY type`, values...)
			if err != nil {
				return page, err
			}
			for rows.Next() {
				var kind string
				var count int
				if err = rows.Scan(&kind, &count); err != nil {
					rows.Close()
					return page, err
				}
				page.Total += count
				page.Counts[kind] += count
			}
			if err = rows.Err(); err != nil {
				rows.Close()
				return page, err
			}
			rows.Close()
			p.rememberEvidenceCounts(cacheKey, page.Total, page.Counts, countGeneration)
		}
		page.CountCapped = page.Total > 1000
	}
	args := historySQLArgs{}
	candidates := journalCandidates(keys, q, c, q.Limit+1, true, &args)
	values := []any(args)
	authority, err := evidenceAuthoritySQL(q, &values, "m.authorization_kind", "m.namespace")
	if err != nil {
		return page, err
	}
	permission := "(m.event_type='coverage_gap' OR (" + authority + "))"
	column := "event_at"
	if q.ByObservation {
		column = "observed_at"
	}
	rows, err := tx.QueryContext(ctx, `WITH candidates AS MATERIALIZED(`+candidates+`)
 SELECT m.event_id,m.event_type,m.event_at,m.observed_at,m.namespace,m.resource_name,m.kind,m.resource_uid,m.root_uid,m.pod_uid,m.rule,m.epoch,
 CASE WHEN `+permission+` THEN body.payload ELSE '{}'::jsonb END,m.is_break,m.gap_from,m.gap_to,m.priority,m.authorization_kind
 FROM candidates m LEFT JOIN LATERAL(SELECT payload FROM kubernetes_evidence_events b
 WHERE b.scope_key=m.scope_key AND b.sequence=m.sequence AND b.event_at=m.event_at LIMIT 1)body ON `+permission+`
 ORDER BY m.`+column+` DESC,m.event_id DESC`, values...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var record EvidenceRecord
		var from, to sql.NullTime
		if err = rows.Scan(&record.ID, &record.Type, &record.At, &record.ObservedAt, &record.Namespace, &record.Name, &record.Kind, &record.UID, &record.RootUID, &record.PodUID, &record.Rule, &record.Epoch, &record.Payload, &record.Broken, &from, &to, &record.Priority, &record.AuthorizationKind); err != nil {
			rows.Close()
			return page, err
		}
		record.GapFrom, record.GapTo = from.Time, to.Time
		page.Items = append(page.Items, projectEvidenceAuthority(record, q))
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
