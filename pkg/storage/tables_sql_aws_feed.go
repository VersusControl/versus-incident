package storage

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

var _ AWSChangeFeedStore = (*sqlTables)(nil)

// changeIDChunk bounds the IDs one IN list carries.
const changeIDChunk = 500

func (t *sqlTables) RecordAWSChangePoll(ctx context.Context, scope tenancy.OrgScope, poll AWSChangePoll, guard *uint64) (AWSChangePollResult, error) {
	cleaned, dropped, err := cleanChangePoll(poll)
	if err != nil {
		return AWSChangePollResult{}, err
	}
	scope = inventoryScope(scope)
	var result AWSChangePollResult
	err = t.write(ctx, func(tx *sql.Tx) error {
		if err := t.lockAccount(ctx, tx, scope.Write, cleaned.Account, guard, true); err != nil {
			return err
		}
		ops := &sqlChangePoll{t: t, ctx: ctx, tx: tx, org: scope.Write, account: cleaned.Account, region: cleaned.Region}
		var err error
		result, err = recordChangePoll(ops, cleaned, dropped)
		return err
	})
	return result, err
}

type sqlChangePoll struct {
	t                    *sqlTables
	ctx                  context.Context
	tx                   *sql.Tx
	org, account, region string
}

func (op *sqlChangePoll) pair() []any { return []any{op.org, op.account, op.region} }

func (op *sqlChangePoll) existingIDs(ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for start := 0; start < len(ids); start += changeIDChunk {
		chunk := ids[start:min(start+changeIDChunk, len(ids))]
		rows, err := op.tx.QueryContext(op.ctx, op.t.q(`SELECT event_id FROM aws_changes WHERE org_id = ? AND event_id IN (`+placeholders(len(chunk))+`)`),
			append([]any{op.org}, stringArgs(chunk)...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (op *sqlChangePoll) scanIntervals(statement string, args ...any) ([]AWSInterval, error) {
	rows, err := op.tx.QueryContext(op.ctx, op.t.q(statement), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AWSInterval
	for rows.Next() {
		var from, to tableTimeValue
		if err := rows.Scan(&from, &to); err != nil {
			return nil, err
		}
		out = append(out, AWSInterval{From: from.Time, To: to.Time})
	}
	return out, rows.Err()
}

func (op *sqlChangePoll) coverageTouching(from, to time.Time) ([]AWSInterval, error) {
	return op.scanIntervals(`SELECT covered_from, covered_until FROM aws_change_coverage WHERE org_id = ? AND account = ? AND region = ?
 AND covered_until >= ? AND covered_from <= ? ORDER BY covered_from`, append(op.pair(), op.t.at(from), op.t.at(to))...)
}

func (op *sqlChangePoll) failureRows(row AWSChange, from, to time.Time) ([]changeIDTime, error) {
	rows, err := op.tx.QueryContext(op.ctx, op.t.q(`SELECT event_id, event_time FROM aws_changes WHERE org_id = ? AND account = ? AND region = ?
 AND event_name = ? AND error_code = ? AND actor = ? AND session_class = ? AND event_time >= ? AND event_time < ?`),
		append(op.pair(), row.EventName, row.ErrorCode, row.Actor, row.SessionClass, op.t.at(from), op.t.at(to))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []changeIDTime
	for rows.Next() {
		var item changeIDTime
		var at tableTimeValue
		if err := rows.Scan(&item.id, &at); err != nil {
			return nil, err
		}
		item.at = at.Time
		out = append(out, item)
	}
	return out, rows.Err()
}

func (op *sqlChangePoll) deleteChanges(ids []string) error {
	for start := 0; start < len(ids); start += changeIDChunk {
		chunk := ids[start:min(start+changeIDChunk, len(ids))]
		if _, err := op.tx.ExecContext(op.ctx, op.t.q(`DELETE FROM aws_changes WHERE org_id = ? AND event_id IN (`+placeholders(len(chunk))+`)`),
			append([]any{op.org}, stringArgs(chunk)...)...); err != nil {
			return err
		}
	}
	return nil
}

var awsChangeColumns = []string{"org_id", "event_id", "account", "region", "service", "event_time", "event_name", "actor_class", "actor", "resource_arn", "app", "summary", "fields", "detail",
	"error_code", "template", "session_class", "via", "recorded_at"}

// The arrival time of a row is kept on re-reads: it is when the row was
// first stored, which pins the rows a paged listing sees.
const awsChangeUpsert = ` ON CONFLICT (org_id, event_id) DO UPDATE SET account = excluded.account, region = excluded.region, service = excluded.service,
 event_time = excluded.event_time, event_name = excluded.event_name, actor_class = excluded.actor_class, actor = excluded.actor,
 resource_arn = excluded.resource_arn, app = excluded.app, summary = excluded.summary, fields = excluded.fields, detail = excluded.detail,
 error_code = excluded.error_code, template = excluded.template, session_class = excluded.session_class, via = excluded.via`

func (op *sqlChangePoll) upsertChanges(changes []AWSChange, at time.Time) error {
	rows := make([][]any, 0, len(changes))
	for _, change := range changes {
		rows = append(rows, []any{op.org, change.EventID, change.Account, change.Region, change.Service, op.t.at(change.EventTime), change.EventName, change.ActorClass,
			change.Actor, change.ResourceARN, change.App, change.Summary, encodeTableJSON(change.Fields), jsonParam(change.Detail),
			change.ErrorCode, change.Template, change.SessionClass, change.Via, op.t.at(at)})
	}
	return op.t.insertRows(op.ctx, op.tx, "aws_changes", awsChangeColumns, rows, awsChangeUpsert)
}

var awsChangeCountColumns = []string{"org_id", "account", "region", "kind", "event_name", "actor", "session_class", "error_code", "minute", "service", "event_source", "actor_class", "events"}

func (op *sqlChangePoll) upsertCounts(counts []AWSChangeCount) error {
	rows := make([][]any, 0, len(counts))
	for _, count := range counts {
		rows = append(rows, []any{op.org, op.account, op.region, count.Kind, count.EventName, count.Actor, count.SessionClass, count.ErrorCode, op.t.at(count.Minute),
			count.Service, count.EventSource, count.ActorClass, count.Events})
	}
	larger := "MAX"
	if op.t.postgres {
		larger = "GREATEST"
	}
	suffix := ` ON CONFLICT (org_id, account, region, kind, event_name, actor, session_class, error_code, minute) DO UPDATE SET
 events = ` + larger + `(aws_change_counts.events, excluded.events), service = excluded.service, event_source = excluded.event_source, actor_class = excluded.actor_class`
	return op.t.insertRows(op.ctx, op.tx, "aws_change_counts", awsChangeCountColumns, rows, suffix)
}

func (op *sqlChangePoll) replaceCoverage(remove, add []AWSInterval) error {
	for _, interval := range remove {
		if _, err := op.tx.ExecContext(op.ctx, op.t.q(`DELETE FROM aws_change_coverage WHERE org_id = ? AND account = ? AND region = ? AND covered_from = ?`),
			append(op.pair(), op.t.at(interval.From))...); err != nil {
			return err
		}
	}
	rows := make([][]any, 0, len(add))
	for _, interval := range add {
		rows = append(rows, []any{op.org, op.account, op.region, op.t.at(interval.From), op.t.at(interval.To)})
	}
	return op.t.insertRows(op.ctx, op.tx, "aws_change_coverage", []string{"org_id", "account", "region", "covered_from", "covered_until"}, rows, "")
}

func (op *sqlChangePoll) coverageCount() (int, error) {
	var count int
	err := op.tx.QueryRowContext(op.ctx, op.t.q(`SELECT COUNT(*) FROM aws_change_coverage WHERE org_id = ? AND account = ? AND region = ?`), op.pair()...).Scan(&count)
	return count, err
}

func (op *sqlChangePoll) oldestCoverage(limit int) ([]AWSInterval, error) {
	return op.scanIntervals(`SELECT covered_from, covered_until FROM aws_change_coverage WHERE org_id = ? AND account = ? AND region = ?
 ORDER BY covered_from LIMIT ?`, append(op.pair(), limit)...)
}

func (op *sqlChangePoll) gapReasons() ([]AWSChangeGapReason, error) {
	rows, err := op.tx.QueryContext(op.ctx, op.t.q(`SELECT gap_from, gap_to, reason, detail FROM aws_change_gaps WHERE org_id = ? AND account = ? AND region = ? ORDER BY gap_from`), op.pair()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AWSChangeGapReason
	for rows.Next() {
		gap := AWSChangeGapReason{Account: op.account, Region: op.region}
		var from, to tableTimeValue
		if err := rows.Scan(&from, &to, &gap.Reason, &gap.Detail); err != nil {
			return nil, err
		}
		gap.From, gap.To = from.Time, to.Time
		out = append(out, gap)
	}
	return out, rows.Err()
}

func (op *sqlChangePoll) replaceGapReasons(list []AWSChangeGapReason) error {
	if _, err := op.tx.ExecContext(op.ctx, op.t.q(`DELETE FROM aws_change_gaps WHERE org_id = ? AND account = ? AND region = ?`), op.pair()...); err != nil {
		return err
	}
	rows := make([][]any, 0, len(list))
	for _, gap := range list {
		rows = append(rows, []any{op.org, op.account, op.region, op.t.at(gap.From), op.t.at(gap.To), gap.Reason, gap.Detail})
	}
	return op.t.insertRows(op.ctx, op.tx, "aws_change_gaps", []string{"org_id", "account", "region", "gap_from", "gap_to", "reason", "detail"}, rows, "")
}

func (op *sqlChangePoll) upsertPair(poll AWSChangePoll, late, dropped int64) error {
	history := poll.HistoryFrom
	if history.IsZero() {
		history = poll.At
	}
	_, err := op.tx.ExecContext(op.ctx, op.t.q(`INSERT INTO aws_change_pairs (org_id, account, region, connected_at, history_from, last_poll_at, late_arrivals, dropped, not_changes, clock_skew_ms)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (org_id, account, region) DO UPDATE SET
 history_from = CASE WHEN excluded.history_from < aws_change_pairs.history_from THEN excluded.history_from ELSE aws_change_pairs.history_from END,
 last_poll_at = excluded.last_poll_at, late_arrivals = aws_change_pairs.late_arrivals + excluded.late_arrivals,
 dropped = aws_change_pairs.dropped + excluded.dropped, not_changes = aws_change_pairs.not_changes + excluded.not_changes, clock_skew_ms = excluded.clock_skew_ms`),
		op.org, op.account, op.region, op.t.at(poll.At), op.t.at(history), op.t.at(poll.At), late, dropped, poll.NotChanges, poll.ClockSkew.Milliseconds())
	return err
}

// minuteColumn renders a time column as its UTC minute, 2006-01-02T15:04.
func (t *sqlTables) minuteColumn(column string) string {
	if t.postgres {
		return `to_char(` + column + ` AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI')`
	}
	return `substr(` + column + `, 1, 16)`
}

// detailText reads one string field of a JSON document column.
func (t *sqlTables) detailText(column, key string) string {
	if t.postgres {
		return column + "->>'" + key + "'"
	}
	return "json_extract(" + column + ", '$." + key + "')"
}

// apiNameColumn renders an event as service:Action, the raw API name the
// change list shows (s3:PutBucketTagging), from its event source and name.
func (t *sqlTables) apiNameColumn(source, name string) string {
	return "replace(" + source + ", '.amazonaws.com', '') || ':' || " + name
}

// jsonListHas is a condition true when the string list under key in the
// JSON document column holds the next parameter exactly.
func (t *sqlTables) jsonListHas(column, key string) string {
	if t.postgres {
		return "jsonb_exists(" + column + "->'" + key + "', ?)"
	}
	return "EXISTS (SELECT 1 FROM json_each(" + column + ", '$." + key + "') WHERE value = ?)"
}

func (t *sqlTables) AWSChangePairState(ctx context.Context, scope tenancy.OrgScope, account, region string) (AWSChangePairState, bool, error) {
	db, err := t.tablesDB()
	if err != nil {
		return AWSChangePairState{}, false, err
	}
	scope = inventoryScope(scope)
	pairs, err := t.feedPairs(ctx, db, scope, AWSChangeFeedQuery{Account: account, Region: region})
	if err != nil || len(pairs) == 0 {
		return AWSChangePairState{}, false, err
	}
	return pairs[0], true, nil
}

// firstChangeCopy keeps one copy of each change event across the read
// organizations: the row of the first organization, in read order, that
// stored the event. Coverage plays no part, so a later read of a stretch
// never hides a row a listing already showed. recordedBefore, when set,
// counts only copies stored by then, matching the listing's pin: a copy
// stored after a listing began never replaces one it already paged through.
// The guard probe uses the (org_id, event_id) primary key.
func (t *sqlTables) firstChangeCopy(orgs []string, alias string, recordedBefore time.Time) (string, []any) {
	if len(orgs) == 1 {
		return alias + ".org_id = ?", []any{orgs[0]}
	}
	guardRank, guardArgs := orgRank(orgs, "d.org_id")
	rowRank, rowArgs := orgRank(orgs, alias+".org_id")
	args := append(stringArgs(orgs), stringArgs(orgs)...)
	pin := ""
	if !recordedBefore.IsZero() {
		pin = " AND (d.recorded_at IS NULL OR d.recorded_at <= ?)"
		args = append(args, t.at(recordedBefore))
	}
	args = append(append(args, guardArgs...), rowArgs...)
	return alias + ".org_id IN (" + placeholders(len(orgs)) + ") AND NOT EXISTS (SELECT 1 FROM aws_changes d WHERE d.org_id IN (" + placeholders(len(orgs)) + ")" +
		" AND d.event_id = " + alias + ".event_id" + pin + " AND " + guardRank + " < " + rowRank + ")", args
}

// largestCount keeps, for each minute and count key, the row of the
// organization that counted the most events; a tie goes to the first in
// read order. A minute both organizations read counts once, never twice.
// The guard probe uses the count table's primary key.
//
// A limit on the inherited organizations applies to the guard too, so a
// count the limit hides never hides the write organization's own count.
func largestCount(orgs []string, alias, write string, limit *AWSInheritedLimit) (string, []any) {
	if len(orgs) == 1 {
		return alias + ".org_id = ?", []any{orgs[0]}
	}
	guardRank, guardArgs := orgRank(orgs, "d.org_id")
	rowRank, rowArgs := orgRank(orgs, alias+".org_id")
	args := append(stringArgs(orgs), stringArgs(orgs)...)
	guardLimit := ""
	if condition, limitArgs, ok := limit.inheritedCondition(write, "d", "account"); ok {
		guardLimit = " AND " + condition
		args = append(args, limitArgs...)
	}
	args = append(append(args, guardArgs...), rowArgs...)
	match := make([]string, 0, 8)
	for _, column := range []string{"account", "region", "kind", "event_name", "actor", "session_class", "error_code", "minute"} {
		match = append(match, "d."+column+" = "+alias+"."+column)
	}
	return alias + ".org_id IN (" + placeholders(len(orgs)) + ") AND NOT EXISTS (SELECT 1 FROM aws_change_counts d WHERE d.org_id IN (" + placeholders(len(orgs)) + ")" + guardLimit +
		" AND " + strings.Join(match, " AND ") + " AND (d.events > " + alias + ".events OR d.events = " + alias + ".events AND " + guardRank + " < " + rowRank + "))", args
}

// readOrgs keeps rows of alias from every read organization.
func readOrgs(orgs []string, alias string) (string, []any) {
	if len(orgs) == 1 {
		return alias + ".org_id = ?", []any{orgs[0]}
	}
	return alias + ".org_id IN (" + placeholders(len(orgs)) + ")", stringArgs(orgs)
}

// likePattern escapes a value for LIKE ... ESCAPE '\' and wraps it.
func likePattern(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
	return "%" + value + "%"
}

// feedWhere filters one feed table aliased alias. The text, actor class and
// resource terms apply to the change rows only.
func (t *sqlTables) feedWhere(condition string, args []any, alias, timeColumn string, query AWSChangeFeedQuery) sqlWhere {
	var where sqlWhere
	where.add(condition, args...)
	where.equal(alias+".account", query.Account)
	where.equal(alias+".region", query.Region)
	where.equal(alias+".service", query.Service)
	where.in(alias+".service", query.Services)
	where.equal(alias+".actor_class", query.ActorClass)
	if !query.Since.IsZero() {
		where.add(alias+"."+timeColumn+" >= ?", t.at(query.Since))
	}
	if !query.Until.IsZero() {
		where.add(alias+"."+timeColumn+" < ?", t.at(query.Until))
	}
	return where
}

func (t *sqlTables) rowWhere(scope tenancy.OrgScope, query AWSChangeFeedQuery) sqlWhere {
	condition, args := t.firstChangeCopy(scope.Read, "c", query.RecordedBefore)
	where := t.feedWhere(condition, args, "c", "event_time", query)
	if condition, args, ok := query.Inherited.inheritedCondition(scope.Write, "c", "account"); ok {
		where.add(condition, args...)
	}
	if query.ResourceARN != "" {
		key := query.ResourceKey
		if key == "" {
			key = query.ResourceARN
		}
		where.add("("+t.jsonListHas("c.detail", ChangeDetailResources)+" OR "+t.jsonListHas("c.detail", ChangeDetailResourceKeys)+")", query.ResourceARN, key)
	}
	if query.Text != "" {
		pattern := likePattern(asciiLower(query.Text))
		where.add(`(lower(c.summary) LIKE ? ESCAPE '\' OR lower(c.event_name) LIKE ? ESCAPE '\' OR lower(`+t.apiNameColumn(t.detailText("c.detail", "event_source"), "c.event_name")+`) LIKE ? ESCAPE '\')`, pattern, pattern, pattern)
	}
	if !query.RecordedBefore.IsZero() {
		where.add("(c.recorded_at IS NULL OR c.recorded_at <= ?)", t.at(query.RecordedBefore))
	}
	return where
}

func (t *sqlTables) AWSChangeFeed(ctx context.Context, scope tenancy.OrgScope, query AWSChangeFeedQuery) (AWSChangeFeed, error) {
	db, err := t.tablesDB()
	if err != nil {
		return AWSChangeFeed{}, err
	}
	options := &sql.TxOptions{}
	if t.postgres {
		// One snapshot for every statement, so a sweep or a poll between
		// them can never pair coverage with rows it no longer matches.
		options.Isolation = sql.LevelRepeatableRead
		options.ReadOnly = true
	}
	tx, err := db.BeginTx(ctx, options)
	if err != nil {
		return AWSChangeFeed{}, err
	}
	defer func() { _ = tx.Rollback() }()
	scope = inventoryScope(scope)
	var feed AWSChangeFeed
	if feed.Coverage, err = t.awsChangeCoverage(ctx, tx, scope, query.Account, query.Region, query.Inherited); err != nil {
		return AWSChangeFeed{}, err
	}
	if feed.Gaps, err = t.feedGapReasons(ctx, tx, scope, query); err != nil {
		return AWSChangeFeed{}, err
	}
	if feed.Pairs, err = t.feedPairs(ctx, tx, scope, query); err != nil {
		return AWSChangeFeed{}, err
	}
	if feed.Minutes, err = t.feedMinutes(ctx, tx, scope, query); err != nil {
		return AWSChangeFeed{}, err
	}
	if query.ResourceARN == "" {
		if feed.Counts, err = t.feedCounts(ctx, tx, scope, query); err != nil {
			return AWSChangeFeed{}, err
		}
	}
	where := t.rowWhere(scope, query)
	where.add("c.actor_class = 'person'")
	where.add("c.error_code = ''")
	if err := tx.QueryRowContext(ctx, t.q(`SELECT COUNT(DISTINCT c.actor) FROM aws_changes c WHERE `+where.String()), where.args...).Scan(&feed.People); err != nil {
		return AWSChangeFeed{}, err
	}
	if feed.TopActors, err = t.feedTopActors(ctx, tx, scope, query); err != nil {
		return AWSChangeFeed{}, err
	}
	if query.ServiceCounts {
		if feed.ServiceCounts, err = t.feedServiceCounts(ctx, tx, scope, query); err != nil {
			return AWSChangeFeed{}, err
		}
	}
	if !query.Since.IsZero() {
		before := query
		before.Since, before.Until = time.Time{}, query.Since
		where := t.rowWhere(scope, before)
		where.add("c.error_code = ''")
		var last tableTimeValue
		if err := tx.QueryRowContext(ctx, t.q(`SELECT MAX(c.event_time) FROM aws_changes c WHERE `+where.String()), where.args...).Scan(&last); err != nil {
			return AWSChangeFeed{}, err
		}
		feed.LastBefore = last.Time
	}
	limit := query.RowLimit
	if limit <= 0 {
		limit = DefaultAWSChangeReadCap
	}
	if feed.Rows, err = t.feedRows(ctx, tx, scope, query, limit+1); err != nil {
		return AWSChangeFeed{}, err
	}
	if len(feed.Rows) > limit {
		feed.Rows, feed.Truncated = feed.Rows[:limit], true
	}
	return feed, nil
}

func (t *sqlTables) feedTopActors(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSChangeFeedQuery) ([]AWSChangeActorCount, error) {
	where := t.rowWhere(scope, query)
	where.add("c.error_code = ''")
	rows, err := q.QueryContext(ctx, t.q(`SELECT c.actor, c.actor_class, c.session_class, COUNT(*) FROM aws_changes c WHERE `+where.String()+`
 GROUP BY c.actor, c.actor_class, c.session_class ORDER BY 4 DESC, 1, 2, 3 LIMIT ?`), append(where.args, topActorLimit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSChangeActorCount, 0)
	for rows.Next() {
		var item AWSChangeActorCount
		if err := rows.Scan(&item.Actor, &item.ActorClass, &item.SessionClass, &item.Events); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// feedServiceCounts counts successful changes per service with every
// filter but the service ones.
func (t *sqlTables) feedServiceCounts(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSChangeFeedQuery) ([]AWSChangeServiceCount, error) {
	where := t.rowWhere(scope, serviceCountQuery(query))
	where.add("c.error_code = ''")
	rows, err := q.QueryContext(ctx, t.q(`SELECT c.service, COUNT(*) FROM aws_changes c WHERE `+where.String()+` GROUP BY c.service`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSChangeServiceCount, 0)
	for rows.Next() {
		var item AWSChangeServiceCount
		if err := rows.Scan(&item.Service, &item.Events); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortServiceCounts(out)
	return out, nil
}

func (t *sqlTables) feedGapReasons(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSChangeFeedQuery) ([]AWSChangeGapReason, error) {
	var where sqlWhere
	condition, args := readOrgs(scope.Read, "g2")
	where.add(condition, args...)
	if condition, args, ok := query.Inherited.inheritedCondition(scope.Write, "g2", "account"); ok {
		where.add(condition, args...)
	}
	where.equal("g2.account", query.Account)
	where.equal("g2.region", query.Region)
	if !query.Since.IsZero() {
		where.add("g2.gap_to > ?", t.at(query.Since))
	}
	if !query.Until.IsZero() {
		where.add("g2.gap_from < ?", t.at(query.Until))
	}
	rank, rankArgs := orgRank(scope.Read, "g2.org_id")
	rows, err := q.QueryContext(ctx, t.q(`SELECT g2.account, g2.region, g2.gap_from, g2.gap_to, g2.reason, g2.detail FROM aws_change_gaps g2
 WHERE `+where.String()+` ORDER BY g2.account, g2.region, g2.gap_from, `+rank), append(where.args, rankArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSChangeGapReason, 0)
	for rows.Next() {
		var gap AWSChangeGapReason
		var from, to tableTimeValue
		if err := rows.Scan(&gap.Account, &gap.Region, &from, &to, &gap.Reason, &gap.Detail); err != nil {
			return nil, err
		}
		gap.From, gap.To = from.Time, to.Time
		out = append(out, gap)
	}
	return out, rows.Err()
}

func (t *sqlTables) feedPairs(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSChangeFeedQuery) ([]AWSChangePairState, error) {
	var where sqlWhere
	condition, args := readOrgs(scope.Read, "p")
	where.add(condition, args...)
	if condition, args, ok := query.Inherited.inheritedCondition(scope.Write, "p", "account"); ok {
		where.add(condition, args...)
	}
	where.equal("p.account", query.Account)
	where.equal("p.region", query.Region)
	rank, rankArgs := orgRank(scope.Read, "p.org_id")
	rows, err := q.QueryContext(ctx, t.q(`SELECT p.org_id, p.account, p.region, p.connected_at, p.history_from, p.last_poll_at, p.late_arrivals, p.dropped, p.not_changes, p.clock_skew_ms
 FROM aws_change_pairs p WHERE `+where.String()+` ORDER BY p.account, p.region, `+rank), append(where.args, rankArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSChangePairState, 0)
	for rows.Next() {
		var pair AWSChangePairState
		var connected, history, polled tableTimeValue
		var skew int64
		if err := rows.Scan(&pair.Org, &pair.Account, &pair.Region, &connected, &history, &polled, &pair.LateArrivals, &pair.Dropped, &pair.NotChanges, &skew); err != nil {
			return nil, err
		}
		pair.ConnectedAt, pair.HistoryFrom, pair.LastPollAt, pair.ClockSkew = connected.Time, history.Time, polled.Time, time.Duration(skew)*time.Millisecond
		out = append(out, pair)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mergeChangePairs(scope.Write, out), nil
}

func (t *sqlTables) feedMinutes(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSChangeFeedQuery) ([]AWSChangeMinute, error) {
	where := t.rowWhere(scope, query)
	where.add("c.error_code = ''")
	minute := t.minuteColumn("c.event_time")
	rows, err := q.QueryContext(ctx, t.q(`SELECT `+minute+`, c.actor_class, COUNT(*) FROM aws_changes c WHERE `+where.String()+` GROUP BY `+minute+`, c.actor_class ORDER BY 1, 2`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSChangeMinute, 0)
	for rows.Next() {
		var text string
		var item AWSChangeMinute
		if err := rows.Scan(&text, &item.ActorClass, &item.Events); err != nil {
			return nil, err
		}
		at, ok := minuteOf(text)
		if !ok {
			continue
		}
		item.Minute = at
		out = append(out, item)
	}
	return out, rows.Err()
}

func (t *sqlTables) feedCounts(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSChangeFeedQuery) ([]AWSChangeCount, error) {
	condition, args := largestCount(scope.Read, "n", scope.Write, query.Inherited)
	where := t.feedWhere(condition, args, "n", "minute", query)
	if condition, args, ok := query.Inherited.inheritedCondition(scope.Write, "n", "account"); ok {
		where.add(condition, args...)
	}
	if query.Text != "" {
		pattern := likePattern(asciiLower(query.Text))
		where.add(`(lower(n.event_name) LIKE ? ESCAPE '\' OR lower(`+t.apiNameColumn("n.event_source", "n.event_name")+`) LIKE ? ESCAPE '\')`, pattern, pattern)
	}
	rows, err := q.QueryContext(ctx, t.q(`SELECT n.account, n.region, n.kind, n.event_name, n.actor, n.session_class, n.error_code, n.minute, n.service, n.event_source, n.actor_class, n.events
 FROM aws_change_counts n WHERE `+where.String()+` ORDER BY n.minute, n.account, n.region, n.kind, n.event_name, n.actor, n.session_class, n.error_code`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSChangeCount, 0)
	for rows.Next() {
		var count AWSChangeCount
		var minute tableTimeValue
		if err := rows.Scan(&count.Account, &count.Region, &count.Kind, &count.EventName, &count.Actor, &count.SessionClass, &count.ErrorCode, &minute,
			&count.Service, &count.EventSource, &count.ActorClass, &count.Events); err != nil {
			return nil, err
		}
		count.Minute = minute.Time
		out = append(out, count)
	}
	return out, rows.Err()
}

const feedRowColumns = `c.event_id, c.account, c.region, c.service, c.event_time, c.event_name, c.actor_class, c.actor, c.resource_arn, c.app, c.summary, c.fields,
 c.error_code, c.template, c.session_class, c.via, c.recorded_at`

func (t *sqlTables) scanFeedRows(rows *sql.Rows, detail bool) ([]AWSChange, error) {
	defer rows.Close()
	out := make([]AWSChange, 0)
	for rows.Next() {
		var row AWSChange
		var at, recorded tableTimeValue
		var fields, document []byte
		targets := []any{&row.EventID, &row.Account, &row.Region, &row.Service, &at, &row.EventName, &row.ActorClass, &row.Actor, &row.ResourceARN, &row.App, &row.Summary, &fields,
			&row.ErrorCode, &row.Template, &row.SessionClass, &row.Via, &recorded}
		if detail {
			targets = append(targets, &document)
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row.EventTime, row.RecordedAt = at.Time, recorded.Time
		var err error
		if row.Fields, err = decodeTableMap(fields); err != nil {
			return nil, err
		}
		row.Detail = scannedJSON(document)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (t *sqlTables) feedRows(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSChangeFeedQuery, limit int) ([]AWSChange, error) {
	where := t.rowWhere(scope, query)
	columns := feedRowColumns
	if query.IncludeDetail {
		columns += ", c.detail"
	}
	rows, err := q.QueryContext(ctx, t.q(`SELECT `+columns+` FROM aws_changes c WHERE `+where.String()+` ORDER BY c.event_time DESC, c.event_id DESC LIMIT ?`),
		append(where.args, limit)...)
	if err != nil {
		return nil, err
	}
	return t.scanFeedRows(rows, query.IncludeDetail)
}

func (t *sqlTables) AWSChangesByID(ctx context.Context, scope tenancy.OrgScope, ids []string) ([]AWSChange, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	out := make([]AWSChange, 0, len(ids))
	for start := 0; start < len(ids); start += changeIDChunk {
		chunk := ids[start:min(start+changeIDChunk, len(ids))]
		var where sqlWhere
		condition, args := t.firstChangeCopy(scope.Read, "c", time.Time{})
		where.add(condition, args...)
		where.in("c.event_id", chunk)
		rows, err := db.QueryContext(ctx, t.q(`SELECT `+feedRowColumns+`, c.detail FROM aws_changes c WHERE `+where.String()+` ORDER BY c.event_time DESC, c.event_id DESC`), where.args...)
		if err != nil {
			return nil, err
		}
		found, err := t.scanFeedRows(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// trimChangeTablesTx makes coverage and gap labels stop claiming anything
// before cutoff. It runs before, and in the same transaction as, the first
// row delete of a sweep, so no reader ever sees coverage over rows already
// gone.
func (t *sqlTables) trimChangeTablesTx(ctx context.Context, tx *sql.Tx, org string, cutoff any) error {
	// Take every account's lock first, in account order, as a poll does for
	// its one account: a poll merging coverage and a trim can then never
	// interleave on Postgres. SQLite serializes writers already.
	if t.postgres {
		rows, err := tx.QueryContext(ctx, t.q(`SELECT account FROM aws_account_epochs WHERE org_id = ? ORDER BY account`+t.lockClause(true)), org)
		if err != nil {
			return err
		}
		for rows.Next() {
			var account string
			if err := rows.Scan(&account); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM aws_change_coverage WHERE org_id = ? AND covered_until <= ?`), org, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, t.q(`UPDATE aws_change_coverage SET covered_from = ? WHERE org_id = ? AND covered_from < ?`), cutoff, org, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM aws_change_gaps WHERE org_id = ? AND gap_to <= ?`), org, cutoff); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, t.q(`UPDATE aws_change_gaps SET gap_from = ? WHERE org_id = ? AND gap_from < ?`), cutoff, org, cutoff)
	return err
}
