package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
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

// journalShape says which slice of the journal a candidate read covers and
// how it recognizes an event the first read scope also holds.
type journalShape struct {
	// windowFrom and windowTo, when set, bound every branch to rows stored in
	// [windowFrom, windowTo): whole months, so a plan made for the bound
	// values covers only those months' partitions.
	windowFrom, windowTo time.Time
	// leaves, when set, are the monthly partitions of the window, read by
	// name instead of through the partitioned journal: the statement then
	// names only those months, so the plan a connection keeps for it is
	// built from them alone and only a statistics refresh of one of them
	// makes it plan again.
	leaves []string
	// aboveAt and aboveID, when set, keep only rows that rank above this
	// position: an observation-ordered walk with a full page needs nothing
	// that would not displace one of its rows.
	aboveAt time.Time
	aboveID string
	// headsComplete is true when every current row of the first scope has a
	// head, so an event of a later scope is matched against the first scope's
	// heads instead of every monthly partition of its journal.
	headsComplete bool
	// gapReach is the earliest time any gap of the read scopes is stored
	// before its end (zero when none is): a gap that overlaps a range is
	// stored at or after the earlier of this and the range's start.
	gapReach time.Time
}

// journalCandidates selects the bounded, ordered metadata of the journal rows
// a page can draw from. visible, when set, is an extra condition every
// non-gap row must meet inside its branch, before the branch's LIMIT, so a
// filtered keyset is never cut short by rows the condition removes.
func journalCandidates(keys []string, q EvidenceQuery, c evidenceCursor, cap int, next bool, visible string, shape journalShape, args *historySQLArgs) string {
	// The limit is bound, so one kept plan serves every page size.
	limit := args.add(int64(cap)) + "::bigint"
	column := "event_at"
	if q.ByObservation {
		column = "observed_at"
	}
	branches := []string{}
	// Each branch reads one event type, so its rows come in index order and
	// its LIMIT stops the read; a branch over a list of types would read and
	// sort every row of those types in the range.
	classes := []string{""}
	switch q.Type {
	case "timeline":
		classes = []string{"change", "crash", "break:issue_opened", "break:issue_resolved"}
	case "break":
		classes = []string{"break:issue_opened", "break:issue_resolved"}
	case "issue":
		classes = []string{"issue_opened", "issue_observed", "issue_resolved"}
	}
	for i, key := range keys {
		for classIndex, class := range classes {
			for _, gap := range []bool{false, true} {
				if gap && (!q.WithGaps || q.GapsApart || classIndex > 0) {
					continue
				}
				scope, pin := args.add(key), args.add(c.Highs[i])
				recheck := "FALSE"
				where := "e.scope_key=" + scope + " AND e.sequence<=" + pin + " AND(e.superseded_seq IS NULL OR e.superseded_seq>" + pin + ")"
				if i > 0 {
					first, high := args.add(keys[0]), args.add(c.Highs[0])
					journal := "SELECT 1 FROM kubernetes_evidence_events p WHERE p.scope_key=" + first + " AND p.event_id=e.event_id AND p.sequence<=" + high
					if shape.headsComplete {
						// An event's head is its newest version. An event whose
						// head is newer than this read's pin may still have had
						// a version at the pin: such a row is marked for a
						// journal check after the read, so no statement here
						// searches the first scope's journal in every month.
						// OFFSET 0 keeps each a key lookup per row.
						where += " AND NOT EXISTS(SELECT 1 FROM kubernetes_evidence_event_heads h WHERE h.scope_key=" + first + " AND h.event_id=e.event_id AND h.sequence<=" + high + " OFFSET 0)"
						recheck = "EXISTS(SELECT 1 FROM kubernetes_evidence_event_heads h WHERE h.scope_key=" + first + " AND h.event_id=e.event_id AND h.sequence>" + high + " OFFSET 0)"
					} else {
						where += " AND NOT EXISTS(" + journal + ")"
					}
				}
				if !shape.windowFrom.IsZero() {
					where += " AND e.event_at>=" + args.add(shape.windowFrom) + " AND e.event_at<" + args.add(shape.windowTo)
				}
				from, to := args.add(q.Since), args.add(q.Until)
				if gap {
					// A gap that overlaps the range ends at or after its start;
					// it is stored at its end, or, for the few stored earlier,
					// no earlier than the scope's gap reach. Either way it is in
					// a month at or after the earlier of the two.
					floor := q.Since
					if !shape.gapReach.IsZero() && shape.gapReach.Before(floor) {
						floor = shape.gapReach
					}
					where += " AND e.event_type='coverage_gap' AND e.gap_to>=" + from + " AND e.gap_from<=" + to + " AND e.event_at>=" + args.add(floor)
				} else {
					where += " AND e." + column + ">=" + from + " AND e." + column + "<=" + to
					if q.WithGaps {
						where += " AND e.event_type<>'coverage_gap'"
					}
					switch {
					case class == "crash":
						where += ` AND e.event_type='crash' AND ` + historyTimelineCrashMarker
					case strings.HasPrefix(class, "break:"):
						where += ` AND e.is_break AND e.event_type='` + strings.TrimPrefix(class, "break:") + `'`
					case class != "":
						where += ` AND e.event_type='` + class + `'`
					case q.Type != "":
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
				if !gap && visible != "" {
					where += " AND(" + visible + ")"
				}
				if next && !c.At.IsZero() {
					where += " AND(e." + column + ",e.event_id)<(" + args.add(c.At) + "," + args.add(c.ID) + ")"
				}
				if !shape.aboveAt.IsZero() {
					where += " AND(e." + column + ",e.event_id)>(" + args.add(shape.aboveAt) + "," + args.add(shape.aboveID) + ")"
				}
				columns := eventMetadata + "," + recheck + " AS recheck"
				source := journalSource(shape) + " e WHERE " + where
				if gap {
					// The few gaps that overlap the range are found through the
					// gap index first (OFFSET 0 keeps that apart) and then put in
					// order, rather than walking every gap in time order.
					source = "(SELECT * FROM " + journalSource(shape) + " e WHERE " + where + " OFFSET 0) e"
				}
				branches = append(branches, "(SELECT "+columns+" FROM "+source+" ORDER BY "+column+" DESC,event_id DESC LIMIT "+limit+")")
			}
		}
	}
	return "SELECT * FROM(" + strings.Join(branches, " UNION ALL ") + ") visible ORDER BY " + column + " DESC,event_id DESC LIMIT " + limit
}

// journalSource is the relation a candidate branch reads: the named monthly
// partitions of a walk step, or the journal.
func journalSource(shape journalShape) string {
	if len(shape.leaves) == 0 {
		return "kubernetes_evidence_events"
	}
	parts := make([]string, len(shape.leaves))
	for i, leaf := range shape.leaves {
		parts[i] = "SELECT * FROM " + leaf
	}
	return "(" + strings.Join(parts, " UNION ALL ") + ")"
}

// journalRow is one journal candidate: its metadata, whether the reader may
// see it, and whether it needs the first-scope journal check.
type journalRow struct {
	scope     string
	sequence  int64
	record    EvidenceRecord
	recheck   bool
	permitted bool
}

// journalWalk reads journal candidates in order, newest first, a few months
// at a time. Each step is one statement that names the monthly partitions it
// reads, so its plan covers those months only: a pooled connection keeps it,
// and a statistics refresh of a month (autovacuum's, say) re-plans only the
// steps that name that month. Rows ordered by event time arrive month by
// month and the walk stops when it has enough. Crashes ordered by observation
// time can sit in any earlier month, because a record is observed at or after
// its event and a crash status is refreshed in place; each step keeps the
// best rows so far, and the walk stops once the scope's longest observation
// delay rules out every older month.
type journalWalk struct {
	keys      []string
	q         EvidenceQuery
	c         evidenceCursor
	next      bool
	condition func(*historySQLArgs) (string, error)
	shape     journalShape
	months    []time.Time
	lag       time.Duration
	lagKnown  bool
}

// journalWalkSettings are the plan settings of journal reads. A walk step
// names its monthly partitions, so the plan a pooled connection keeps for it
// covers those months only, and a statistics refresh of the current month
// re-plans only the steps that name it. A kept plan does not know how much of
// a month a range covers, so sorting is discouraged: rows come in index order
// and a step stops at its limit instead of reading a month to sort it. A read
// that has no index order, such as the few gaps of a range, still sorts.
const journalWalkSettings = `SET LOCAL plan_cache_mode=force_generic_plan; SET LOCAL enable_sort=off`

// journalWindowMonths is the most months one step of a walk covers. Steps
// start at one month and double, so a page that the newest month fills reads
// one month, and a sparse read over many years takes few steps.
const journalWindowMonths = 12

// journalLeaves names the monthly partitions that exist in [from, to).
func journalLeaves(months []time.Time, from, to time.Time) []string {
	leaves := []string{}
	for _, month := range months {
		if !month.Before(from) && month.Before(to) {
			leaves = append(leaves, "evidence_event_month_"+month.Format("200601"))
		}
	}
	return leaves
}

func monthStart(at time.Time) time.Time {
	at = at.UTC()
	return time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (w journalWalk) column() string {
	if w.q.ByObservation {
		return "observed_at"
	}
	return "event_at"
}

// run returns up to cap candidates in the read's order.
func (w journalWalk) run(ctx context.Context, tx *sql.Tx, cap int) ([]journalRow, error) {
	if len(w.months) == 0 {
		// A journal not yet partitioned is read in one step.
		return w.window(ctx, tx, w.shape, cap)
	}
	gapBranch := w.q.WithGaps && !w.q.GapsApart
	low, high := w.q.Since, w.q.Until
	if w.q.ByObservation {
		// A record is never observed before it happened.
		low = time.Time{}
		if w.lagKnown {
			low = w.q.Since.Add(-w.lag)
		}
	}
	if gapBranch {
		floor := w.q.Since
		if !w.shape.gapReach.IsZero() && w.shape.gapReach.Before(floor) {
			floor = w.shape.gapReach
		}
		if floor.Before(low) {
			low = floor
		}
		// A gap that overlaps the range may be stored after its end.
		high = time.Time{}
	}
	oldest, newestEnd := w.months[0], w.months[len(w.months)-1].AddDate(0, 1, 0)
	if low.Before(oldest) {
		low = oldest
	}
	top := newestEnd
	if !high.IsZero() && high.Before(newestEnd) {
		top = monthStart(high).AddDate(0, 1, 0)
	}
	// A later page reads only rows before its cursor in the read's order, and
	// a record is never observed before it happened, so none is stored after
	// the cursor's month.
	if w.next && !w.c.At.IsZero() {
		if after := monthStart(w.c.At).AddDate(0, 1, 0); after.Before(top) {
			top = after
		}
	}
	if !top.After(low) {
		return nil, nil
	}
	collected := []journalRow{}
	span := 1
	for top.After(low) {
		from := top.AddDate(0, -span, 0)
		if from.Before(monthStart(low)) {
			from = monthStart(low)
		}
		shape := w.shape
		shape.windowFrom, shape.windowTo = from, top
		shape.leaves = journalLeaves(w.months, from, top)
		if w.q.ByObservation && len(collected) == cap {
			shape.aboveAt, shape.aboveID = collected[cap-1].record.ObservedAt, collected[cap-1].record.ID
		}
		need := cap
		if !w.q.ByObservation {
			need = cap - len(collected)
		}
		rows := []journalRow{}
		if len(shape.leaves) > 0 {
			var err error
			if rows, err = w.window(ctx, tx, shape, need); err != nil {
				return nil, err
			}
		}
		if w.q.ByObservation {
			collected = append(collected, rows...)
			sort.SliceStable(collected, func(i, j int) bool { return journalRowAfter(collected[i], collected[j], "observed_at") })
			if len(collected) > cap {
				collected = collected[:cap]
			}
			// Older months hold only rows stored before from, observed
			// before from plus the lag: none can rank above a full page that
			// already ends at or after that time.
			if w.lagKnown && len(collected) == cap && !collected[cap-1].record.ObservedAt.Before(from.Add(w.lag)) {
				break
			}
		} else {
			collected = append(collected, rows...)
			if len(collected) >= cap {
				break
			}
		}
		top = from
		span = min(span*2, journalWindowMonths)
	}
	return collected, nil
}

// window reads up to need candidates of one step, continuing inside the
// step when rows the first-scope check removes leave it short.
func (w journalWalk) window(ctx context.Context, tx *sql.Tx, shape journalShape, need int) ([]journalRow, error) {
	cursor, next := w.c, w.next
	kept := []journalRow{}
	for len(kept) < need {
		rows, last, err := w.step(ctx, tx, shape, cursor, next, need-len(kept))
		if err != nil {
			return nil, err
		}
		kept = append(kept, rows...)
		if last == nil {
			break
		}
		cursor.At, cursor.ID, next = last.record.At, last.record.ID, true
		if w.q.ByObservation {
			cursor.At = last.record.ObservedAt
		}
	}
	return kept, nil
}

// step runs one bounded candidate statement and applies the first-scope
// journal check to the rows that need it. last is the final row the
// statement returned when it returned as many as asked, so more may follow.
func (w journalWalk) step(ctx context.Context, tx *sql.Tx, shape journalShape, cursor evidenceCursor, next bool, limit int) ([]journalRow, *journalRow, error) {
	args := historySQLArgs{}
	condition := ""
	if w.condition != nil {
		var err error
		if condition, err = w.condition(&args); err != nil {
			return nil, nil, err
		}
	}
	candidates := journalCandidates(w.keys, w.q, cursor, limit, next, condition, shape, &args)
	values := []any(args)
	authority, err := evidenceAuthoritySQL(w.q, &values, "m.authorization_kind", "m.namespace")
	if err != nil {
		return nil, nil, err
	}
	column := w.column()
	rows, err := tx.QueryContext(ctx, `WITH candidates AS MATERIALIZED(`+candidates+`)
 SELECT m.scope_key,m.sequence,m.event_id,m.event_type,m.event_at,m.observed_at,m.namespace,m.resource_name,m.kind,m.resource_uid,m.root_uid,m.pod_uid,m.rule,m.epoch,
 m.is_break,m.gap_from,m.gap_to,m.priority,m.authorization_kind,m.recheck,(m.event_type='coverage_gap' OR COALESCE(`+authority+`,FALSE))
 FROM candidates m ORDER BY m.`+column+` DESC,m.event_id DESC`, values...)
	if err != nil {
		return nil, nil, err
	}
	found := []journalRow{}
	for rows.Next() {
		var row journalRow
		var from, to sql.NullTime
		record := &row.record
		if err = rows.Scan(&row.scope, &row.sequence, &record.ID, &record.Type, &record.At, &record.ObservedAt, &record.Namespace, &record.Name, &record.Kind, &record.UID, &record.RootUID, &record.PodUID, &record.Rule, &record.Epoch,
			&record.Broken, &from, &to, &record.Priority, &record.AuthorizationKind, &row.recheck, &row.permitted); err != nil {
			rows.Close()
			return nil, nil, err
		}
		record.GapFrom, record.GapTo = from.Time, to.Time
		found = append(found, row)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	var last *journalRow
	if len(found) == limit && limit > 0 {
		final := found[len(found)-1]
		last = &final
	}
	if err = w.dropShadowed(ctx, tx, &found); err != nil {
		return nil, nil, err
	}
	return found, last, nil
}

// dropShadowed removes rows of a later scope whose event the first scope
// held at this read's pin, among the rows whose first-scope head is newer
// than the pin. This rare check is the only one that reads the first scope's
// journal across months.
func (w journalWalk) dropShadowed(ctx context.Context, tx *sql.Tx, rows *[]journalRow) error {
	ids := []string{}
	for _, row := range *rows {
		if row.recheck {
			ids = append(ids, row.record.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// Planned for these values alone: a kept plan for this rare check would
	// cover every month and be rebuilt after any month's statistics change.
	if _, err := tx.ExecContext(ctx, `SET LOCAL plan_cache_mode=force_custom_plan`); err != nil {
		return err
	}
	result, err := tx.QueryContext(ctx, `SELECT DISTINCT event_id FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id=ANY($2::text[]) AND sequence<=$3`, w.keys[0], ids, int64(w.c.Highs[0]))
	if err != nil {
		return err
	}
	shadowed := map[string]bool{}
	for result.Next() {
		var id string
		if err = result.Scan(&id); err != nil {
			result.Close()
			return err
		}
		shadowed[id] = true
	}
	if err = result.Err(); err != nil {
		result.Close()
		return err
	}
	result.Close()
	if _, err = tx.ExecContext(ctx, journalWalkSettings); err != nil {
		return err
	}
	kept := (*rows)[:0]
	for _, row := range *rows {
		if !(row.recheck && shadowed[row.record.ID]) {
			kept = append(kept, row)
		}
	}
	*rows = kept
	return nil
}

// journalRowAfter orders candidates as the read does: newest first, then by
// event identifier, descending.
func journalRowAfter(a, b journalRow, column string) bool {
	at, bt := a.record.At, b.record.At
	if column == "observed_at" {
		at, bt = a.record.ObservedAt, b.record.ObservedAt
	}
	if !at.Equal(bt) {
		return at.After(bt)
	}
	return a.record.ID > b.record.ID
}

// journalBodies reads the payloads of the given rows by their full key, so
// the statement is planned for the months those rows are in.
func journalBodies(ctx context.Context, tx *sql.Tx, rows []journalRow, months []time.Time, wanted func(journalRow) bool) (map[int64][]byte, error) {
	scopes, sequences, ats := []string{}, []int64{}, []time.Time{}
	shape, seen := journalShape{}, map[time.Time]bool{}
	for _, row := range rows {
		if wanted(row) {
			scopes, sequences, ats = append(scopes, row.scope), append(sequences, row.sequence), append(ats, row.record.At)
			seen[monthStart(row.record.At)] = true
		}
	}
	bodies := map[int64][]byte{}
	if len(sequences) == 0 {
		return bodies, nil
	}
	for _, month := range months {
		if seen[month] {
			shape.leaves = append(shape.leaves, "evidence_event_month_"+month.Format("200601"))
		}
	}
	result, err := tx.QueryContext(ctx, `SELECT sequence,payload FROM `+journalSource(shape)+` e WHERE scope_key=ANY($1::text[]) AND sequence=ANY($2::bigint[]) AND event_at=ANY($3::timestamptz[])`, scopes, sequences, ats)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	for result.Next() {
		var sequence int64
		var body []byte
		if err = result.Scan(&sequence, &body); err != nil {
			return nil, err
		}
		bodies[sequence] = body
	}
	return bodies, result.Err()
}

// journalReadSetup reads what every journal read of these scopes needs: how
// far back gaps may be stored, whether the first scope's heads are complete,
// the journal's months, and the scopes' observation delay.
func journalReadSetup(ctx context.Context, tx *sql.Tx, keys []string, q EvidenceQuery) (journalShape, []time.Time, time.Duration, bool, error) {
	shape := journalShape{}
	if q.WithGaps {
		var reach sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT MIN(gap_reach) FROM kubernetes_evidence_scopes WHERE scope_key=ANY($1::text[])`, keys).Scan(&reach); err != nil {
			return shape, nil, 0, false, err
		}
		shape.gapReach = reach.Time
	}
	if len(keys) > 1 {
		complete, err := evidenceHeadsComplete(ctx, tx, keys[0])
		if err != nil {
			return shape, nil, 0, false, err
		}
		shape.headsComplete = complete
	}
	months, err := evidenceJournalMonths(ctx, tx)
	if err != nil {
		return shape, nil, 0, false, err
	}
	var lag time.Duration
	known := false
	// The observation delay bounds only crash reads, the records it is kept
	// for; any other read ordered by observation walks every month.
	if q.ByObservation && q.Type == "crash" {
		if lag, known, err = evidenceObservedLag(ctx, tx, keys); err != nil {
			return shape, nil, 0, false, err
		}
	}
	return shape, months, lag, known, nil
}

func (p *postgresProvider) queryJournalPage(ctx context.Context, tx *sql.Tx, keys []string, q EvidenceQuery, c evidenceCursor, page EvidencePage, countGeneration uint64, summary map[string]int) (EvidencePage, error) {
	for _, high := range c.Highs {
		if high > uint64(1<<63-1) {
			return page, ErrInvalidEvidence
		}
	}
	shape, months, lag, lagKnown, err := journalReadSetup(ctx, tx, keys, q)
	if err != nil {
		return page, err
	}
	if p.evidenceMonthsListed != nil {
		p.evidenceMonthsListed()
	}
	if _, err = tx.ExecContext(ctx, journalWalkSettings); err != nil {
		return page, err
	}
	if p.evidenceSingleStep {
		months = nil
	}
	walk := journalWalk{keys: keys, q: q, c: c, next: true, shape: shape, months: months, lag: lag, lagKnown: lagKnown,
		condition: func(args *historySQLArgs) (string, error) { return historyListableSQL(q, args) }}
	cacheKey := evidenceDigest(c.Binding, fmt.Sprint(c.Generations), fmt.Sprint(c.PruneRevisions), fmt.Sprint(q.Authorities), fmt.Sprint(q.AllowedKinds), fmt.Sprint(page.Checkpoint.CommitRevision))
	unfiltered := p.evidenceRetention <= 0 && len(keys) == 1 && page.Checkpoint.Saving == "" && !q.EnforceAuthority && q.Type != "timeline" && q.Type != "break" && q.Namespace == "" && q.Kind == "" && q.RootUID == "" && q.UID == "" && q.ResourceUID == "" && q.PodUID == "" && q.Name == "" && q.Rule == "" && !q.Since.After(page.Checkpoint.StartedAt) && !q.Until.Before(page.Checkpoint.At)
	counting := false
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
			counting = true
		}
		page.CountCapped = page.Total > 1000
	}
	// The first page's count and its rows come from one walk: without a
	// cursor both read the same candidates in the same order.
	need := q.Limit + 1
	if counting {
		need = max(need, 1001)
	}
	rows, err := walk.run(ctx, tx, need)
	if err != nil {
		return page, err
	}
	if counting {
		for _, row := range rows[:min(len(rows), 1001)] {
			kind := "coverage_gap"
			if row.permitted {
				kind = row.record.Type
			}
			page.Total++
			page.Counts[kind]++
		}
		p.rememberEvidenceCounts(cacheKey, page.Total, page.Counts, countGeneration)
		page.CountCapped = page.Total > 1000
	}
	rows = rows[:min(len(rows), q.Limit+1)]
	bodies, err := journalBodies(ctx, tx, rows, months, func(row journalRow) bool { return row.permitted })
	if err != nil {
		return page, err
	}
	for _, row := range rows {
		record := row.record
		record.Payload = json.RawMessage(`{}`)
		if body, ok := bodies[row.sequence]; ok && row.permitted {
			record.Payload = body
		}
		page.Items = append(page.Items, projectEvidenceAuthority(record, q))
	}
	for i, record := range page.Items {
		if q.Projection != nil {
			page.Items[i] = q.Projection(record)
		}
	}
	page = finishEvidencePage(page, q, c)
	if q.GapsApart {
		lower, upper := evidencePageRange(page, q, c)
		if page.Gaps, err = p.queryPageGaps(ctx, tx, keys, q, c, lower, upper, shape, months, lag, lagKnown); err != nil {
			return page, err
		}
	}
	if err = tx.Commit(); err != nil {
		return page, err
	}
	return page, nil
}

// historyListableSQL is the in-branch condition that keeps a gaps-apart keyset
// to rows this reader may list. Other queries keep every row in the keyset.
func historyListableSQL(q EvidenceQuery, args *historySQLArgs) (string, error) {
	if !q.GapsApart || !q.EnforceAuthority {
		return "", nil
	}
	values := []any(*args)
	expression, err := evidenceAuthoritySQL(q, &values, "e.authorization_kind", "e.namespace")
	*args = historySQLArgs(values)
	return "COALESCE(" + expression + ",FALSE)", err
}

// queryPageGaps is the bounded gap read of one page kept apart from its items:
// coverage gaps that overlap [lower, upper], and rows in that range the reader
// may not see, which are returned as point gaps without their payload.
func (p *postgresProvider) queryPageGaps(ctx context.Context, tx *sql.Tx, keys []string, q EvidenceQuery, c evidenceCursor, lower, upper time.Time, shape journalShape, months []time.Time, lag time.Duration, lagKnown bool) ([]EvidenceRecord, error) {
	ranged := q
	ranged.Since, ranged.Until, ranged.GapsApart, ranged.WithGaps = lower, upper, false, true
	walk := journalWalk{keys: keys, q: ranged, c: c, shape: shape, months: months, lag: lag, lagKnown: lagKnown,
		condition: func(args *historySQLArgs) (string, error) {
			if !q.EnforceAuthority {
				return "FALSE", nil
			}
			values := []any(*args)
			expression, err := evidenceAuthoritySQL(q, &values, "e.authorization_kind", "e.namespace")
			*args = historySQLArgs(values)
			return "NOT COALESCE(" + expression + ",FALSE)", err
		}}
	rows, err := walk.run(ctx, tx, evidencePageGapLimit+1)
	if err != nil {
		return nil, err
	}
	bodies, err := journalBodies(ctx, tx, rows, months, func(row journalRow) bool { return row.record.Type == "coverage_gap" })
	if err != nil {
		return nil, err
	}
	gaps := []EvidenceRecord{}
	for _, row := range rows {
		record := row.record
		record.Payload = json.RawMessage(`{}`)
		if body, ok := bodies[row.sequence]; ok && record.Type == "coverage_gap" {
			record.Payload = body
		}
		if record.Type != "coverage_gap" {
			// A row the reader may not see keeps only its time.
			record = projectEvidenceAuthority(EvidenceRecord{ID: record.ID, Type: record.Type, At: record.At, ObservedAt: record.ObservedAt}, EvidenceQuery{EnforceAuthority: true})
		}
		if q.Projection != nil {
			record = q.Projection(record)
		}
		gaps = append(gaps, record)
	}
	return boundEvidenceGaps(gaps, q, lower, upper), nil
}
