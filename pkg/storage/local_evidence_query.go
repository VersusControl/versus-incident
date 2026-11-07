package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type localEvidenceArgs []any

func (args *localEvidenceArgs) add(value any) string {
	*args = append(*args, value)
	return "?" + strconv.Itoa(len(*args))
}

// localTimelineCrashMarker keeps a crash record on the timeline when it declares no
// subtype, which is how crash records written before subtypes existed are stored, or
// when it declares the terminal marker. A declared status observation is not a
// timeline marker, and no subtype is inferred for a record that declares none.
const localTimelineCrashMarker = "(json_extract(e.raw,'$.payload.type') IS NULL OR json_extract(e.raw,'$.payload.type')='' OR json_extract(e.raw,'$.payload.type')='termination')"

// localTimelineClasses are the mutually exclusive evidence classes a timeline shows.
// Each class bounds its own window, because a merged coverage-gap/non-gap window
// cannot serve an ordered index scan and would make every class sort its history.
var localTimelineClasses = []struct {
	filter string
	gap    bool
}{
	{`e.type='change'`, false},
	{`e.type='crash' AND ` + localTimelineCrashMarker, false},
	{`e.broken=1 AND e.type IN('issue_opened','issue_resolved')`, false},
	{`e.type='coverage_gap'`, true},
}

func localAuthority(query EvidenceQuery, args *localEvidenceArgs) (string, string, error) {
	if !query.EnforceAuthority {
		return "", "1", nil
	}
	proofs := query.Authorities
	if len(proofs) == 0 {
		proofs = map[string]EvidenceAuthority{}
		for _, kind := range query.AllowedKinds {
			if kind != "" {
				proofs[kind] = EvidenceAuthority{State: "allowed", ProvedAt: time.Unix(1, 0).UTC()}
			}
		}
	}
	type grant struct {
		Key   string `json:"key"`
		State string `json:"state"`
		At    string `json:"at"`
	}
	grants := make([]grant, 0, len(proofs))
	for key, proof := range proofs {
		grants = append(grants, grant{key, proof.State, localEvidenceTime(proof.ProvedAt)})
	}
	raw, err := json.Marshal(grants)
	if err != nil {
		return "", "", err
	}
	parameter := args.add(string(raw))
	cte := `WITH authority AS MATERIALIZED(SELECT json_extract(value,'$.key') AS key,json_extract(value,'$.state') AS state,json_extract(value,'$.at') AS at FROM json_each(` + parameter + `)) `
	allowed := `(e.authority_kind<>'' AND CASE WHEN a.key IS NOT NULL AND(g.key IS NULL OR a.at>g.at OR(a.at=g.at AND a.state<>'allowed')) THEN a.state='allowed' ELSE COALESCE(g.state='allowed',0) END)`
	return cte, allowed, nil
}

func localAuthorityJoins(query EvidenceQuery) string {
	if !query.EnforceAuthority {
		return ""
	}
	return ` LEFT JOIN authority g ON g.key=e.authority_kind LEFT JOIN authority a ON a.key=e.authority_kind||'@'||e.namespace AND e.namespace<>'' `
}

func localPins(keys []string, highs []uint64, args *localEvidenceArgs) string {
	expression := "CASE e.key"
	for i, key := range keys {
		expression += " WHEN " + args.add(key) + " THEN " + args.add(highs[i])
	}
	return expression + " END"
}
func localKeys(keys []string, args *localEvidenceArgs) string {
	parameters := []string{}
	for _, key := range keys {
		parameters = append(parameters, args.add(key))
	}
	return "e.key IN(" + strings.Join(parameters, ",") + ")"
}

func (index *localEvidence) query(parent context.Context, policy EvidencePolicy, scope EvidenceScope, query EvidenceQuery) (EvidencePage, error) {
	query, cursor, err := prepareEvidenceQuery(scope, query)
	if err != nil {
		return EvidencePage{}, err
	}
	if query.PointInTime && (query.Type != "occurrence" || query.OccurrenceID == "") {
		return EvidencePage{}, ErrInvalidEvidence
	}
	if query.Type == "occurrence" {
		// Occurrence lists page by first_seen. A link of this list that pages
		// in another order, or predates links naming their order, restarts the
		// list from its first page.
		if query.Cursor != "" && cursor.Ordering != "first_seen" {
			return EvidencePage{}, ErrEvidenceCursorExpired
		}
		cursor.Ordering = "first_seen"
	}
	scopes, err := evidenceReadScopes(scope)
	if err != nil {
		return EvidencePage{}, err
	}
	keys := make([]string, len(scopes))
	for i, scope := range scopes {
		keys[i], err = evidenceScopeKey(scope)
		if err != nil {
			return EvidencePage{}, err
		}
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	tx, err := index.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EvidencePage{}, err
	}
	defer tx.Rollback()
	page := EvidencePage{Policy: policy, Since: query.Since, Until: query.Until, Counts: map[string]int{}}
	if query.Cursor == "" {
		cursor.OccurrenceHighs = make([]uint64, len(keys))
		cursor.Generations = make([]uint64, len(keys))
		cursor.Highs = make([]uint64, len(keys))
		cursor.Pruned = make([]time.Time, len(keys))
		cursor.PruneRevisions = make([]uint64, len(keys))
		cursor.ThinRevisions = make([]uint64, len(keys))
	} else if len(cursor.OccurrenceHighs) != len(keys) || len(cursor.Generations) != len(keys) || len(cursor.Highs) != len(keys) || len(cursor.Pruned) != len(keys) || len(cursor.PruneRevisions) != len(keys) || !cursorThinRevisionsFit(cursor, len(keys)) {
		return page, ErrInvalidEvidence
	}
	for i, key := range keys {
		var raw []byte
		var cp EvidenceCheckpoint
		var high uint64
		var valid bool
		err = tx.QueryRowContext(ctx, `SELECT checkpoint,high,valid FROM evidence_scopes WHERE key=? AND present=1`, key).Scan(&raw, &high, &valid)
		if err != nil && err != sql.ErrNoRows {
			return page, err
		}
		if err == nil {
			if !valid || json.Unmarshal(raw, &cp) != nil {
				return page, errInvalidLocalEvidence
			}
		}
		if query.Cursor == "" {
			cursor.OccurrenceHighs[i], cursor.Generations[i], cursor.Highs[i] = cp.OccurrenceRevision, cp.Generation, high
			cursor.Pruned[i], cursor.PruneRevisions[i], cursor.ThinRevisions[i] = cp.PrunedThrough, cp.PruneRevision, cp.ThinRevision
		} else if cursor.Generations[i] != cp.Generation || cursor.PruneRevisions[i] != cp.PruneRevision || !cursor.Pruned[i].Equal(cp.PrunedThrough) {
			// History was pruned or replaced since the cursor was issued.
			return page, ErrEvidenceCursorExpired
		} else if query.Type == "occurrence" && cursorSamplesThinned(cursor, i, cp.ThinRevision) {
			// An occurrence is listed only while it keeps a sample in the
			// window, so a removed sample can change the rows. Other lists do
			// not read samples and keep their page links.
			return page, ErrEvidenceCursorExpired
		} else if cursor.Highs[i] > high || cursor.OccurrenceHighs[i] > cp.OccurrenceRevision {
			// A cursor ahead of the stored history was never issued for it.
			return page, ErrEvidenceConflict
		}
		page.Checkpoint = mergeEvidenceCheckpoint(page.Checkpoint, cp)
	}
	if query.Type == "occurrence" {
		return index.queryOccurrences(ctx, tx, keys, query, cursor, page)
	}
	args := localEvidenceArgs{}
	cte, allowed, err := localAuthority(query, &args)
	if err != nil {
		return page, err
	}
	since, until := args.add(localEvidenceTime(query.Since)), args.add(localEvidenceTime(query.Until))
	column := "e.at"
	if query.ByObservation {
		column = "e.observed"
	}
	itemGaps := query.WithGaps && !query.GapsApart
	where := localJournalWhere(query, keys, cursor.Highs, column, since, until, itemGaps, &args)
	branchJoins := ""
	if query.GapsApart {
		// Only rows this reader may list enter the keyset and the count.
		where += " AND COALESCE(" + allowed + ",0)"
		branchJoins = localAuthorityJoins(query)
	}
	from := " FROM evidence_events e" + localAuthorityJoins(query)
	countWhere := where
	if query.Type == "timeline" {
		from = " FROM(" + localTimelineCandidates(where, branchJoins, column, since, until, 1001, itemGaps) + ") e" + localAuthorityJoins(query)
		countWhere = "1"
	}
	permission := "(e.type='coverage_gap' OR " + allowed + ")"
	if query.Cursor == "" {
		countSQL := cte + `SELECT type,COUNT(*) FROM(SELECT CASE WHEN ` + permission + ` THEN e.type ELSE 'coverage_gap' END AS type` + from + " WHERE " + countWhere + ` LIMIT 1001) bounded GROUP BY type`
		rows, err := tx.QueryContext(ctx, countSQL, []any(args)...)
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
			page.Counts[kind] += count
			page.Total += count
			index.countReads.Add(uint64(count))
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return page, err
		}
		rows.Close()
		page.CountCapped = page.Total > 1000
	}
	if !cursor.At.IsZero() {
		where += " AND(" + column + ",e.id)<(" + args.add(localEvidenceTime(cursor.At)) + "," + args.add(cursor.ID) + ")"
	}
	limit := args.add(query.Limit + 1)
	rawColumn := "e.raw"
	if query.Type == "timeline" {
		from = " FROM(" + localTimelineCandidates(where, branchJoins, column, since, until, query.Limit+1, itemGaps) + ") e" + localAuthorityJoins(query) + " JOIN evidence_events body ON body.key=e.key AND body.sequence=e.sequence"
		where = "1"
		rawColumn = "body.raw"
	}
	rows, err := tx.QueryContext(ctx, cte+"SELECT e.id,e.sequence,e.at,e.observed,COALESCE("+permission+",0),CASE WHEN "+permission+" THEN "+rawColumn+" ELSE NULL END"+from+" WHERE "+where+" ORDER BY "+column+" DESC,e.id DESC LIMIT "+limit, []any(args)...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var id, at, observed string
		var sequence uint64
		var authorized bool
		var raw []byte
		if err = rows.Scan(&id, &sequence, &at, &observed, &authorized, &raw); err != nil {
			rows.Close()
			return page, err
		}
		record := EvidenceRecord{ID: id, Sequence: sequence}
		record.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			rows.Close()
			return page, err
		}
		record.ObservedAt, err = time.Parse(time.RFC3339Nano, observed)
		if err != nil {
			rows.Close()
			return page, err
		}
		if authorized {
			index.payloadReads.Add(1)
			if err = json.Unmarshal(raw, &record); err != nil {
				rows.Close()
				return page, err
			}
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
	for i, record := range page.Items {
		if query.Projection != nil {
			page.Items[i] = query.Projection(record)
		}
	}
	page = finishEvidencePage(page, query, cursor)
	if query.GapsApart {
		lower, upper := evidencePageRange(page, query, cursor)
		if page.Gaps, err = index.queryPageGaps(ctx, tx, keys, query, cursor, column, lower, upper); err != nil {
			return page, err
		}
	}
	if err = tx.Commit(); err != nil {
		return page, err
	}
	return page, nil
}

// queryPageGaps is the bounded gap read of one page kept apart from its items:
// coverage gaps that overlap [lower, upper], and rows in that range the reader
// may not see, which are returned as point gaps without their payload.
func (index *localEvidence) queryPageGaps(ctx context.Context, tx *sql.Tx, keys []string, query EvidenceQuery, cursor evidenceCursor, column string, lower, upper time.Time) ([]EvidenceRecord, error) {
	args := localEvidenceArgs{}
	cte, allowed, err := localAuthority(query, &args)
	if err != nil {
		return nil, err
	}
	since, until := args.add(localEvidenceTime(lower)), args.add(localEvidenceTime(upper))
	where := localJournalWhere(query, keys, cursor.Highs, column, since, until, true, &args)
	if query.Type == "timeline" {
		listable := []string{}
		for _, class := range localTimelineClasses {
			if !class.gap {
				listable = append(listable, "("+class.filter+")")
			}
		}
		where += " AND((e.type='coverage_gap' AND e.gap_to>=" + since + " AND e.gap_from<=" + until + ") OR((" + strings.Join(listable, " OR ") + ") AND " + column + ">=" + since + " AND " + column + "<=" + until + "))"
	}
	where += " AND(e.type='coverage_gap' OR NOT COALESCE(" + allowed + ",0))"
	rows, err := tx.QueryContext(ctx, cte+"SELECT e.id,e.sequence,e.at,e.observed,e.type='coverage_gap',CASE WHEN e.type='coverage_gap' THEN e.raw END FROM evidence_events e"+localAuthorityJoins(query)+" WHERE "+where+" ORDER BY "+column+" DESC,e.id DESC LIMIT "+args.add(evidencePageGapLimit+1), []any(args)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	gaps := []EvidenceRecord{}
	for rows.Next() {
		var id, at, observed string
		var sequence uint64
		var gap bool
		var raw []byte
		if err = rows.Scan(&id, &sequence, &at, &observed, &gap, &raw); err != nil {
			return nil, err
		}
		record := EvidenceRecord{ID: id, Sequence: sequence}
		if record.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		if record.ObservedAt, err = time.Parse(time.RFC3339Nano, observed); err != nil {
			return nil, err
		}
		if gap {
			if err = json.Unmarshal(raw, &record); err != nil {
				return nil, err
			}
		} else {
			record = projectEvidenceAuthority(record, EvidenceQuery{EnforceAuthority: true})
		}
		if query.Projection != nil {
			record = query.Projection(record)
		}
		gaps = append(gaps, record)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return boundEvidenceGaps(gaps, query, lower, upper), nil
}

// Each class bounds its own window and row count before the merge, so a page never
// hydrates history it discards and every branch keeps an ordered index scan. joins
// supplies the authority joins a branch filter refers to; withGaps adds the
// coverage-gap class.
func localTimelineCandidates(where, joins, column, since, until string, limit int, withGaps bool) string {
	metadata := "e.key,e.sequence,e.id,e.type,e.at,e.observed,e.namespace,e.name,e.kind,e.authority_kind,e.uid,e.root,e.pod,e.rule,e.broken,e.gap_from,e.gap_to,e.superseded"
	branches := []string{}
	for _, class := range localTimelineClasses {
		window := column + ">=" + since + " AND " + column + "<=" + until
		if class.gap {
			if !withGaps {
				continue
			}
			window = "e.gap_to>=" + since + " AND e.gap_from<=" + until
		}
		branches = append(branches, "SELECT * FROM(SELECT "+metadata+" FROM evidence_events e"+joins+" WHERE "+where+" AND("+class.filter+") AND "+window+" ORDER BY "+column+" DESC,e.id DESC LIMIT "+strconv.Itoa(limit)+")")
	}
	return "SELECT * FROM(" + strings.Join(branches, " UNION ALL ") + ") ORDER BY " + strings.TrimPrefix(column, "e.") + " DESC,id DESC LIMIT " + strconv.Itoa(limit)
}

// localJournalWhere filters the journal to the query over [since, until] at
// the pinned sequences. withGaps also admits coverage gaps that overlap the
// range; a timeline applies its classes and windows in localTimelineCandidates.
func localJournalWhere(query EvidenceQuery, keys []string, highs []uint64, column, since, until string, withGaps bool, args *localEvidenceArgs) string {
	where := localKeys(keys, args)
	pin := localPins(keys, highs, args)
	where += " AND e.sequence<=" + pin + " AND(e.superseded IS NULL OR e.superseded>" + pin + ")"
	window := column + ">=" + since + " AND " + column + "<=" + until
	if query.Type == "timeline" {
		// A timeline reads one class per branch, and each class carries only the
		// window that applies to it (see localTimelineCandidates).
	} else if withGaps {
		where += " AND((e.type<>'coverage_gap' AND " + window + ") OR(e.type='coverage_gap' AND e.gap_to>=" + since + " AND e.gap_from<=" + until + "))"
	} else {
		where += " AND " + window
	}
	if len(keys) > 1 {
		where += " AND(e.key=" + args.add(keys[0]) + " OR NOT EXISTS(SELECT 1 FROM evidence_events p WHERE p.key=" + args.add(keys[0]) + " AND p.id=e.id AND p.sequence<=" + args.add(highs[0]) + "))"
	}
	for _, filter := range []struct{ column, value string }{{"namespace", query.Namespace}, {"name", query.Name}, {"kind", query.Kind}, {"uid", query.UID}, {"root", query.RootUID}, {"rule", query.Rule}} {
		if filter.value == "" {
			continue
		}
		expression := "e." + filter.column + "=" + args.add(filter.value)
		if withGaps {
			if filter.column == "namespace" || filter.column == "kind" {
				expression = "(" + expression + " OR(e.type='coverage_gap' AND e." + filter.column + "=''))"
			} else {
				expression = "(e.type='coverage_gap' OR " + expression + ")"
			}
		}
		where += " AND " + expression
	}
	if query.ResourceUID != "" {
		value := args.add(query.ResourceUID)
		expression := "(e.uid=" + value + " OR e.root=" + value + " OR e.pod=" + value + ")"
		if query.Type == "timeline" {
			expression = "(e.uid=" + value + " OR(e.type='crash' AND(e.root=" + value + " OR e.pod=" + value + ")))"
		}
		if withGaps {
			expression = "(e.type='coverage_gap' OR " + expression + ")"
		}
		where += " AND " + expression
	}
	if query.PodUID != "" {
		value := args.add(query.PodUID)
		expression := "e.pod=" + value
		if query.Type == "timeline" {
			expression = "(" + expression + " OR(e.type='change' AND e.uid=" + value + "))"
		}
		if withGaps {
			expression = "(e.type='coverage_gap' OR " + expression + ")"
		}
		where += " AND " + expression
	}
	switch query.Type {
	case "timeline":
		// localTimelineCandidates applies the classes and their windows.
	case "break":
		if withGaps {
			where += ` AND(e.type='coverage_gap' OR(e.broken=1 AND e.type IN('issue_opened','issue_resolved')))`
		} else {
			where += ` AND e.broken=1 AND e.type IN('issue_opened','issue_resolved')`
		}
	case "issue":
		where += ` AND e.type IN('issue_opened','issue_observed','issue_resolved')`
	case "":
	default:
		value := args.add(query.Type)
		if withGaps {
			where += " AND(e.type=" + value + " OR e.type='coverage_gap')"
		} else {
			where += " AND e.type=" + value
		}
	}
	return where
}

func (index *localEvidence) queryOccurrences(ctx context.Context, tx *sql.Tx, keys []string, query EvidenceQuery, cursor evidenceCursor, page EvidencePage) (EvidencePage, error) {
	args := localEvidenceArgs{}
	cte, allowed, err := localAuthority(query, &args)
	if err != nil {
		return page, err
	}
	table := "evidence_versions"
	if query.Lookup {
		table = "evidence_current"
	}
	where := localKeys(keys, &args)
	until := args.add(localEvidenceTime(query.Until))
	since := args.add(localEvidenceTime(query.Since))
	pin := localPins(keys, cursor.OccurrenceHighs, &args)
	if !query.Lookup {
		where += " AND e.first<=" + until + " AND(e.closed IS NULL OR e.closed>=" + since + ") AND e.revision<=" + pin +
			" AND(e.superseded IS NULL OR e.superseded>" + pin + " OR e.superseded_at>" + until + ") AND e.observed<=" + until
		where += " AND EXISTS(SELECT 1 FROM evidence_observations proof WHERE proof.key=e.key AND proof.id=e.id AND proof.version=e.version AND proof.revision<=" + pin + " AND proof.observed<=" + until + ")"
	}
	if len(keys) > 1 {
		higher := `SELECT 1 FROM evidence_versions p WHERE p.key=` + args.add(keys[0]) + ` AND p.id=e.id AND p.revision<=` + args.add(cursor.OccurrenceHighs[0]) + ` AND p.observed<=` + until
		if query.Lookup {
			higher = `SELECT 1 FROM evidence_current p WHERE p.key=` + args.add(keys[0]) + ` AND p.id=e.id`
		}
		where += " AND(e.key=" + args.add(keys[0]) + " OR NOT EXISTS(" + higher + "))"
	}
	for _, filter := range []struct{ column, value string }{{"id", query.OccurrenceID}, {"root", query.RootUID}, {"root", query.UID}, {"root", query.ResourceUID}, {"namespace", query.Namespace}, {"kind", query.Kind}, {"rule", query.Rule}} {
		if filter.value != "" {
			where += " AND e." + filter.column + "=" + args.add(filter.value)
		}
	}
	if query.Name != "" || query.PodUID != "" {
		where += " AND 0"
	}
	if !query.Center.IsZero() && query.OccurrenceID == "" {
		where += " AND e.center=" + args.add(localEvidenceTime(query.Center))
	}
	severity, broken, _ := strings.Cut(query.Variant, ":")
	if severity != "" {
		where += " AND e.severity=" + args.add(severity)
	}
	if broken == "broken" {
		where += " AND e.broken=1"
	}
	from := " FROM " + table + " e" + localAuthorityJoins(query)
	countAllowed := allowed
	if query.EnforceAuthority {
		countAllowed = "(" + allowed + " AND e.severity<>'')"
	}
	typeSQL := `CASE WHEN NOT COALESCE(` + countAllowed + `,0) THEN 'coverage_gap' WHEN e.closed IS NOT NULL THEN 'issue_resolved' WHEN e.observation_only=1 THEN 'issue_observed' ELSE 'issue_opened' END`
	if query.Cursor == "" && !query.Lookup {
		rows, err := tx.QueryContext(ctx, cte+`SELECT type,severity,broken,COUNT(*) FROM(SELECT `+typeSQL+` AS type,
 CASE WHEN `+allowed+` THEN e.severity ELSE '' END AS severity,CASE WHEN `+allowed+` THEN e.broken ELSE 0 END AS broken`+from+" WHERE "+where+` LIMIT 1001) bounded GROUP BY type,severity,broken`, []any(args)...)
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
			page.Counts[kind] += count
			page.Total += count
			index.countReads.Add(uint64(count))
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
	order := "e.first DESC,e.id DESC"
	if !cursor.At.IsZero() {
		where += " AND(e.first,e.id)<(" + args.add(localEvidenceTime(cursor.At)) + "," + args.add(cursor.ID) + ")"
	}
	raw, seen, observed, measurements := "e.raw", "e.seen", "e.seen", "NULL"
	if !query.Lookup {
		sample := `(SELECT o.seen FROM evidence_observations o WHERE o.key=e.key AND o.id=e.id AND o.version=e.version AND o.revision<=` + pin + ` AND o.observed<=` + until + ` ORDER BY o.revision DESC LIMIT 1)`
		raw = "e.raw"
		seen = sample
		observed = `(SELECT o.observed FROM evidence_observations o WHERE o.key=e.key AND o.id=e.id AND o.version=e.version AND o.revision<=` + pin + ` AND o.observed<=` + until + ` ORDER BY o.revision DESC LIMIT 1)`
		measurements = `(SELECT o.measurements FROM evidence_observations o WHERE o.key=e.key AND o.id=e.id AND o.version=e.version AND o.revision<=` + pin + ` AND o.observed<=` + until + ` ORDER BY o.revision DESC LIMIT 1)`
		if query.OccurrenceID != "" {
			order = "e.revision DESC"
		}
	}
	limit := query.Limit + 1
	if query.Lookup {
		order = "e.seen DESC,e.id DESC"
		limit = 1
		if query.OccurrenceID == "" && !query.Center.IsZero() {
			limit = 2
		}
	}
	rows, err := tx.QueryContext(ctx, cte+"SELECT e.id,e.first,"+seen+","+observed+",COALESCE("+allowed+",0),CASE WHEN "+allowed+" THEN "+raw+" ELSE NULL END,CASE WHEN "+allowed+" THEN "+measurements+" END"+from+" WHERE "+where+" ORDER BY "+order+" LIMIT "+args.add(limit), []any(args)...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var id, first, seen, observed string
		var authorized bool
		var raw []byte
		var measured []byte
		if err = rows.Scan(&id, &first, &seen, &observed, &authorized, &raw, &measured); err != nil {
			rows.Close()
			return page, err
		}
		at, err := time.Parse(time.RFC3339Nano, first)
		if err != nil {
			rows.Close()
			return page, err
		}
		last, err := time.Parse(time.RFC3339Nano, seen)
		if err != nil {
			rows.Close()
			return page, err
		}
		sampled, err := time.Parse(time.RFC3339Nano, observed)
		if err != nil {
			rows.Close()
			return page, err
		}
		record := EvidenceRecord{ID: id, At: at, ObservedAt: sampled}
		if authorized {
			index.payloadReads.Add(1)
			var issue EvidenceIssue
			if err = json.Unmarshal(raw, &issue); err != nil {
				rows.Close()
				return page, err
			}
			record = occurrenceRecord(issue, occurrenceObservation{At: sampled, LastSeen: last, Measurements: measured})
			record.At = at
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
		if query.Projection != nil {
			page.Items[i] = query.Projection(record)
		}
	}
	if query.Lookup {
		return page, nil
	}
	return finishEvidencePage(page, query, cursor), nil
}
