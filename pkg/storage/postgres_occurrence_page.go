package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const occurrenceMetadata = `scope_key,occurrence_id,payload_version,revision,observed_at,first_seen,closed_at,namespace,kind,root_uid,rule,severity,authorization_kind,broken,observation_only`

// occurrenceOpenSpan and occurrenceVisibleSpan are the expressions of the
// range indexes of occurrence versions; a query must spell them exactly so to
// use them. The open span runs from first seen to closing, or without end; the
// visible span of a replaced version runs from its opening observation until
// it was replaced. Bounds are ordered so a clock disagreement between replicas
// never makes an invalid range.
const (
	occurrenceOpenSpan    = "tstzrange(i.first_seen,GREATEST(i.first_seen,COALESCE(i.closed_at,'infinity'::timestamptz)),'[]')"
	occurrenceVisibleSpan = "tstzrange(LEAST(i.observed_at,i.superseded_at),i.superseded_at,'[)')"
)

// occurrenceRecentSamples is how far before a read's end the newest sample of
// a version is looked for first. Versions are sampled every heartbeat while
// they last, so the newest sample is almost always this recent; only a
// version not sampled for that long is looked up from its opening onward.
const occurrenceRecentSamples = 48 * time.Hour

// occurrenceSampleSkewDuration bounds how much earlier than the observation
// that opened a payload version any later sample of it can be stamped, across
// replicas whose clocks disagree. It lets a sample lookup start at the
// opening's month instead of searching every month before the read's end.
const occurrenceSampleSkewDuration = 24 * time.Hour

// occurrenceThinningReaches reports whether thinning the samples observed
// before thinned can change the sample a read up to until shows. Thinning keeps
// the last sample of each hour and a read takes the newest sample, looking
// first occurrenceRecentSamples back from until and otherwise from the
// version's opening, so a read that ends later than that reach always finds a
// kept sample that outranks every thinned one.
func occurrenceThinningReaches(until, thinned time.Time) bool {
	return !thinned.IsZero() && until.Before(thinned.Add(occurrenceRecentSamples+time.Hour))
}

// occurrenceSampleKey names the sample a read needs: the newest, by revision
// up to pin, of one occurrence (and one payload version when version is set)
// observed by until and no earlier than its version opened, less the skew.
type occurrenceSampleKey struct {
	scope, occurrence, version string
	pin                        int64
	opened                     time.Time
}

type occurrenceSample struct {
	found              bool
	revision           int64
	observed, lastSeen time.Time
	version            string
	measurements       []byte
}

// occurrenceSamples finds the newest samples in two statements, each bounded
// to whole time values so it is planned for the months it reads: first the
// last two days before until, then, for the keys that found nothing there,
// from the earliest opening among them. A version open for years is not
// looked up in every month since it opened unless it went unsampled.
func occurrenceSamples(ctx context.Context, tx *sql.Tx, keys []occurrenceSampleKey, until time.Time) ([]occurrenceSample, error) {
	samples := make([]occurrenceSample, len(keys))
	pending := make([]int, len(keys))
	for i := range keys {
		pending[i] = i
	}
	for stage := 0; stage < 2 && len(pending) > 0; stage++ {
		floor := until.Add(-occurrenceRecentSamples)
		scopes, occurrences, versions, pins, floors, positions := []string{}, []string{}, []string{}, []int64{}, []time.Time{}, []int64{}
		for _, i := range pending {
			key := keys[i]
			opened := key.opened.Add(-occurrenceSampleSkewDuration)
			if stage == 1 && opened.Before(floor) {
				floor = opened
			}
			scopes, occurrences, versions = append(scopes, key.scope), append(occurrences, key.occurrence), append(versions, key.version)
			pins, floors, positions = append(pins, key.pin), append(floors, opened), append(positions, int64(i))
		}
		rows, err := tx.QueryContext(ctx, `SELECT x.position,o.revision,o.observed_at,o.last_seen,o.payload_version,o.measurements
 FROM unnest($1::text[],$2::text[],$3::text[],$4::bigint[],$5::timestamptz[],$6::bigint[]) AS x(scope_key,occurrence_id,payload_version,pin,opened,position)
 CROSS JOIN LATERAL(SELECT revision,observed_at,last_seen,payload_version,measurements FROM kubernetes_evidence_occurrence_observations o
 WHERE o.scope_key=x.scope_key AND o.occurrence_id=x.occurrence_id AND(x.payload_version='' OR o.payload_version=x.payload_version) AND o.revision<=x.pin
 AND o.observed_at<=$7 AND o.observed_at>=$8 AND o.observed_at>=x.opened ORDER BY o.revision DESC LIMIT 1)o`, scopes, occurrences, versions, pins, floors, positions, until, floor)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var position int64
			var sample occurrenceSample
			if err = rows.Scan(&position, &sample.revision, &sample.observed, &sample.lastSeen, &sample.version, &sample.measurements); err != nil {
				rows.Close()
				return nil, err
			}
			sample.found = true
			samples[position] = sample
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		next := pending[:0]
		for _, i := range pending {
			if !samples[i].found {
				next = append(next, i)
			}
		}
		pending = next
	}
	return samples, nil
}

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
		// Occurrences that opened inside the range are read in first-seen
		// order. One that opened earlier and was still open at the range's
		// start is found by what was true at that instant instead of by
		// walking every older occurrence: its current version by the span it
		// was open, a version replaced after the range by the span it was the
		// visible one, and a version replaced since this read was pinned by
		// the revision that replaced it.
		// The three earlier sets are each read through their own index alone
		// (OFFSET 0 keeps the filters out of the index choice) and filtered
		// afterwards: they are small, while an index on a filter, such as
		// severity or broken, holds every version ever recorded.
		branches = append(branches, "(SELECT "+occurrenceMetadata+" FROM kubernetes_evidence_occurrence_read_versions i WHERE "+where+" AND i.first_seen>="+since+
			" ORDER BY first_seen DESC,occurrence_id DESC LIMIT "+strconv.Itoa(cap)+")")
		for _, earlier := range []string{
			"i.superseded_revision IS NULL AND " + occurrenceOpenSpan + "@>" + since + "::timestamptz",
			"i.superseded_revision IS NOT NULL AND " + occurrenceVisibleSpan + "@>" + until + "::timestamptz",
			"i.scope_key=" + scope + " AND i.superseded_revision BETWEEN " + pin + "::bigint+1 AND 9223372036854775807 AND i.superseded_at<=" + until,
		} {
			branches = append(branches, "(SELECT "+occurrenceMetadata+" FROM(SELECT * FROM kubernetes_evidence_occurrence_read_versions i WHERE "+earlier+" OFFSET 0) i WHERE "+where+
				" AND i.first_seen<"+since+" ORDER BY first_seen DESC,occurrence_id DESC LIMIT "+strconv.Itoa(cap)+")")
		}
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
		// Under enforced authority an occurrence without an indexed severity is
		// one whose payload did not decode; the service shows it as unavailable,
		// so the counts classify it the same way, as the local index does.
		countAllowed := allowed
		if q.EnforceAuthority {
			countAllowed = "(" + allowed + " AND COALESCE(m.severity,'')<>'')"
		}
		rows, err := tx.QueryContext(ctx, `WITH candidates AS MATERIALIZED(`+candidates+`)
 SELECT type,severity,broken,COUNT(*) FROM(SELECT CASE WHEN NOT COALESCE(`+countAllowed+`,FALSE) THEN 'coverage_gap'
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
	values := []any(args)
	allowed, err := evidenceAuthoritySQL(q, &values, "m.authorization_kind", "m.namespace")
	if err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `WITH candidates AS MATERIALIZED(`+candidates+`)
 SELECT m.occurrence_id,m.first_seen,COALESCE(`+allowed+`,FALSE),
 CASE WHEN `+allowed+` THEN v.payload ELSE '{}'::jsonb END,m.scope_key,m.payload_version,m.observed_at,`+pin+`
 FROM candidates m LEFT JOIN LATERAL(SELECT payload FROM kubernetes_evidence_occurrence_versions v
 WHERE v.scope_key=m.scope_key AND v.payload_version=m.payload_version LIMIT 1)v ON `+allowed+`
 ORDER BY m.first_seen DESC,m.occurrence_id DESC`, values...)
	if err != nil {
		return page, err
	}
	type listed struct {
		id         string
		first      time.Time
		authorized bool
		raw        []byte
		sample     int
	}
	items, wanted := []listed{}, []occurrenceSampleKey{}
	for rows.Next() {
		var item listed
		var key occurrenceSampleKey
		if err = rows.Scan(&item.id, &item.first, &item.authorized, &item.raw, &key.scope, &key.version, &key.opened, &key.pin); err != nil {
			rows.Close()
			return page, err
		}
		item.sample = -1
		if item.authorized {
			key.occurrence = item.id
			item.sample = len(wanted)
			wanted = append(wanted, key)
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return page, err
	}
	rows.Close()
	samples, err := occurrenceSamples(ctx, tx, wanted, q.Until)
	if err != nil {
		return page, err
	}
	for _, item := range items {
		record := EvidenceRecord{ID: item.id, At: item.first, ObservedAt: item.first}
		if item.sample >= 0 && samples[item.sample].found {
			sample := samples[item.sample]
			var issue EvidenceIssue
			if err = json.Unmarshal(item.raw, &issue); err != nil {
				return page, err
			}
			record = occurrenceRecord(issue, occurrenceObservation{At: sample.observed, LastSeen: sample.lastSeen, Measurements: sample.measurements})
			record.At = item.first
		} else {
			record = projectEvidenceAuthority(record, EvidenceQuery{EnforceAuthority: true})
		}
		page.Items = append(page.Items, record)
	}
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
