package storage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

var finOpsLineColumns = []string{"org_id", "payer", "day", "account", "region", "service", "usage_type", "record_type", "team", "resource_id",
	"service_name", "usage_group", "app", "env", "currency", "net_amortized_micros", "quantity_micros", "unit", "estimated_team", "estimated_region", "refreshed_at"}

var finOpsIncreaseColumns = []string{"org_id", "id", "payer", "account", "service", "usage_type", "record_type", "resource_id", "started_on", "resolved_on", "status",
	"owner_team", "owner_rule", "owner_estimated", "monthly_impact_micros", "expected_reason", "expected_until", "alerted_at", "suspects", "detail"}

func (t *sqlTables) ReplaceFinOpsCostDays(ctx context.Context, scope tenancy.OrgScope, payer string, days []string, lines []FinOpsCostLine) error {
	days, lines, err := prepareFinOpsDays(payer, days, lines)
	if err != nil {
		return err
	}
	scope = inventoryScope(scope)
	org := scope.Write
	return t.write(ctx, func(tx *sql.Tx) error {
		err := inChunks(days, func(chunk []string) error {
			args := append([]any{org, payer}, stringArgs(chunk)...)
			for _, table := range []string{"finops_cost_lines", "finops_cost_daily"} {
				if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM `+table+` WHERE org_id = ? AND payer = ? AND day IN (`+placeholders(len(chunk))+`)`), args...); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		rows := make([][]any, len(lines))
		for index, line := range lines {
			rows[index] = []any{org, payer, line.Day, line.Account, line.Region, line.Service, line.UsageType, line.RecordType, line.Team, line.ResourceID,
				line.ServiceName, line.UsageGroup, line.App, line.Env, line.Currency, line.NetAmortizedMicros, line.QuantityMicros, line.Unit,
				line.EstimatedTeam, line.EstimatedRegion, t.at(line.RefreshedAt)}
		}
		if err := t.insertRows(ctx, tx, "finops_cost_lines", finOpsLineColumns, rows, ""); err != nil {
			return err
		}
		if err := t.insertDaily(ctx, tx, org, rollUpFinOpsLines(lines)); err != nil {
			return err
		}
		months := map[string]bool{}
		for _, day := range days {
			months[finOpsMonthOf(day)] = true
		}
		for _, month := range sortedSet(months) {
			if err := t.rebuildMonthly(ctx, tx, org, payer, month); err != nil {
				return err
			}
		}
		return nil
	})
}

// rebuildMonthly replaces one month's monthly rollup rows, of one payer or of
// every payer when payer is empty, with the sums of the daily rollup rows
// that month holds now.
func (t *sqlTables) rebuildMonthly(ctx context.Context, tx *sql.Tx, org, payer, month string) error {
	deleteMonth := `DELETE FROM finops_cost_monthly WHERE org_id = ? AND month = ?`
	args := []any{org, month}
	if payer != "" {
		deleteMonth += ` AND payer = ?`
		args = append(args, payer)
	}
	if _, err := tx.ExecContext(ctx, t.q(deleteMonth), args...); err != nil {
		return err
	}
	daily, err := t.dailyRows(ctx, tx, org, payer, month, finOpsMonthEnd(month))
	if err != nil {
		return err
	}
	months := rollUpFinOpsMonths(daily)
	rows := make([][]any, len(months))
	for index, row := range months {
		l := row.line
		rows[index] = []any{org, l.Payer, l.Day, l.Account, l.Region, l.Service, l.ServiceName, l.UsageGroup, l.RecordType, l.Team, row.minDay,
			l.NetAmortizedMicros, l.QuantityMicros, row.lines, l.EstimatedTeam, l.EstimatedRegion, row.overflowed}
	}
	return t.insertRows(ctx, tx, "finops_cost_monthly", finOpsMonthlyColumns, rows, "")
}

// dailyRows reads the daily rollup rows of one organization between two
// days, of one payer or of every payer when payer is empty.
func (t *sqlTables) dailyRows(ctx context.Context, tx *sql.Tx, org, payer, from, through string) ([]finOpsDailyRow, error) {
	statement := `SELECT payer, ` + t.dayColumn("day") + `, account, region, service, service_name, usage_group, record_type, team,
 net_amortized_micros, quantity_micros, lines, estimated_team, estimated_region, overflowed FROM finops_cost_daily WHERE org_id = ? AND day >= ? AND day <= ?`
	args := []any{org, from, through}
	if payer != "" {
		statement += ` AND payer = ?`
		args = append(args, payer)
	}
	rows, err := tx.QueryContext(ctx, t.q(statement), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []finOpsDailyRow
	for rows.Next() {
		var row finOpsDailyRow
		var day tableDayValue
		l := &row.line
		if err := rows.Scan(&l.Payer, &day, &l.Account, &l.Region, &l.Service, &l.ServiceName, &l.UsageGroup, &l.RecordType, &l.Team,
			&l.NetAmortizedMicros, &l.QuantityMicros, &row.lines, &l.EstimatedTeam, &l.EstimatedRegion, &row.overflowed); err != nil {
			return nil, err
		}
		l.Day = day.Day
		out = append(out, row)
	}
	return out, rows.Err()
}

// insertDaily inserts rollup rows of one organization.
func (t *sqlTables) insertDaily(ctx context.Context, tx *sql.Tx, org string, daily []finOpsDailyRow) error {
	rows := make([][]any, len(daily))
	for index, row := range daily {
		l := row.line
		rows[index] = []any{org, l.Payer, l.Day, l.Account, l.Region, l.Service, l.ServiceName, l.UsageGroup, l.RecordType, l.Team,
			l.NetAmortizedMicros, l.QuantityMicros, row.lines, l.EstimatedTeam, l.EstimatedRegion, row.overflowed}
	}
	return t.insertRows(ctx, tx, "finops_cost_daily", finOpsDailyColumns, rows, "")
}

// dayLines reads one payer-day's lines without a resource ID, with the
// columns the rollup sums.
func (t *sqlTables) dayLines(ctx context.Context, tx *sql.Tx, org, payer, day string) ([]FinOpsCostLine, error) {
	rows, err := tx.QueryContext(ctx, t.q(`SELECT account, region, service, service_name, usage_group, record_type, team,
 net_amortized_micros, quantity_micros, estimated_team, estimated_region FROM finops_cost_lines WHERE org_id = ? AND payer = ? AND day = ? AND resource_id = ''`), org, payer, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FinOpsCostLine
	for rows.Next() {
		line := FinOpsCostLine{Payer: payer, Day: day}
		if err := rows.Scan(&line.Account, &line.Region, &line.Service, &line.ServiceName, &line.UsageGroup, &line.RecordType, &line.Team,
			&line.NetAmortizedMicros, &line.QuantityMicros, &line.EstimatedTeam, &line.EstimatedRegion); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

func (t *sqlTables) DeleteFinOpsPayer(ctx context.Context, scope tenancy.OrgScope, payer string) error {
	if !validTableKey(payer) {
		return ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	return t.write(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"finops_cost_lines", "finops_cost_daily", "finops_cost_monthly"} {
			if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM `+table+` WHERE org_id = ? AND payer = ?`), scope.Write, payer); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t *sqlTables) DeleteFinOpsIncreasesForPayer(ctx context.Context, scope tenancy.OrgScope, payer string) (int64, error) {
	if !validTableKey(payer) {
		return 0, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	ownIDs := `SELECT id FROM finops_increases WHERE org_id = ? AND payer = ?`
	var count int64
	err := t.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM finops_increase_events WHERE org_id = ? AND increase_id IN (`+ownIDs+`)`), scope.Write, scope.Write, payer); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, t.q(`DELETE FROM finops_increases WHERE org_id = ? AND payer = ?`), scope.Write, payer)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	})
	return count, err
}

func (t *sqlTables) PruneFinOpsCostLines(ctx context.Context, scope tenancy.OrgScope, before string) (int64, error) {
	if !validFinOpsDay(before) {
		return 0, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	var count int64
	err := t.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, t.q(`DELETE FROM finops_cost_lines WHERE org_id = ? AND day < ?`), scope.Write, before)
		if err != nil {
			return err
		}
		if count, err = result.RowsAffected(); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, t.q(`DELETE FROM finops_cost_daily WHERE org_id = ? AND day < ?`), scope.Write, before); err != nil {
			return err
		}
		// Months wholly before the day go; the month it starts inside keeps
		// only its remaining days.
		month := finOpsMonthOf(before)
		if _, err = tx.ExecContext(ctx, t.q(`DELETE FROM finops_cost_monthly WHERE org_id = ? AND month < ?`), scope.Write, month); err != nil {
			return err
		}
		if month == before {
			return nil
		}
		return t.rebuildMonthly(ctx, tx, scope.Write, "", month)
	})
	return count, err
}

func (t *sqlTables) finOpsLineWhere(scope tenancy.OrgScope, query FinOpsCostQuery) sqlWhere {
	var where sqlWhere
	condition, args := visible(scope.Read, "l", "finops_cost_lines", "payer", "day")
	where.add(condition, args...)
	if query.From != "" {
		where.add("l.day >= ?", query.From)
	}
	if query.Through != "" {
		where.add("l.day <= ?", query.Through)
	}
	where.equal("l.payer", query.Payer)
	where.equal("l.account", query.Account)
	where.equal("l.region", query.Region)
	where.equal("l.service", query.Service)
	where.equal("l.service_name", query.ServiceName)
	where.equal("l.usage_type", query.UsageType)
	where.equal("l.record_type", query.RecordType)
	if query.Team != nil {
		where.add("l.team = ?", *query.Team)
	}
	if query.ResourceID != nil {
		where.add("l.resource_id = ?", *query.ResourceID)
	}
	where.in("l.account", query.Accounts)
	where.in("l.region", query.Regions)
	where.in("l.team", query.Teams)
	return where
}

func (t *sqlTables) FinOpsCostLines(ctx context.Context, scope tenancy.OrgScope, query FinOpsCostQuery) ([]FinOpsCostLine, error) {
	if !validFinOpsQuery(query) {
		return nil, ErrInvalidFinOps
	}
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	where := t.finOpsLineWhere(scope, query)
	statement := `SELECT l.payer, ` + t.dayColumn("l.day") + `, l.account, l.region, l.service, l.usage_type, l.record_type, l.team, l.resource_id,
 l.service_name, l.usage_group, l.app, l.env, l.currency, l.net_amortized_micros, l.quantity_micros, l.unit, l.estimated_team, l.estimated_region, l.refreshed_at
 FROM finops_cost_lines l WHERE ` + where.String() + `
 ORDER BY l.day, l.payer, l.account, l.region, l.service, l.service_name, l.usage_type, l.record_type, l.team, l.resource_id`
	args := where.args
	if query.Limit > 0 {
		statement += ` LIMIT ?`
		args = append(args, query.Limit)
	}
	rows, err := db.QueryContext(ctx, t.q(statement), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]FinOpsCostLine, 0)
	for rows.Next() {
		var line FinOpsCostLine
		var day tableDayValue
		var refreshed tableTimeValue
		if err := rows.Scan(&line.Payer, &day, &line.Account, &line.Region, &line.Service, &line.UsageType, &line.RecordType, &line.Team, &line.ResourceID,
			&line.ServiceName, &line.UsageGroup, &line.App, &line.Env, &line.Currency, &line.NetAmortizedMicros, &line.QuantityMicros, &line.Unit,
			&line.EstimatedTeam, &line.EstimatedRegion, &refreshed); err != nil {
			return nil, err
		}
		line.Day, line.RefreshedAt = day.Day, refreshed.Time
		out = append(out, line)
	}
	return out, rows.Err()
}

// amountSumColumns renders the columns FinOpsCostTotals scans for one amount
// column. Postgres sums a BIGINT into an arbitrary-precision NUMERIC, so the
// exact sum is cast to text and read back. SQLite sums integers and raises on
// overflow, so the sum is split into high and low 32-bit halves that stay in
// range, then rebuilt. A plain BIGINT cast would turn two extreme rows into a
// dialect error instead of the closed overflow error.
func (t *sqlTables) amountSumColumns(column string) []string {
	return t.amountColumns(column, true)
}

// amountColumns renders one amount column the way amountSumColumns does,
// summed or, for a read where each row is its own group, as stored.
func (t *sqlTables) amountColumns(column string, sum bool) []string {
	wrap := func(expression string) string {
		if sum {
			return "SUM(" + expression + ")"
		}
		return expression
	}
	if t.postgres {
		return []string{"CAST(" + wrap(column) + " AS TEXT)"}
	}
	high := column + " / 4294967296"
	return []string{wrap(high), wrap(column + " - (" + high + ") * 4294967296")}
}

// amountSumUnit is the radix SQLite sums are split by.
const amountSumUnit = int64(1) << 32

// parseAmountSum reads an exact numeric sum rendered as text. A sum outside
// the signed 64-bit range is the closed overflow error, never a cut number.
func parseAmountSum(text string) (int64, error) {
	if text == "" {
		return 0, nil
	}
	sum, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, ErrAmountOverflow
		}
		return 0, err
	}
	return sum, nil
}

// combineAmountSum rebuilds a sum from its high and low halves, reporting the
// closed overflow error when the total does not fit.
func combineAmountSum(high, low int64) (int64, error) {
	if high > math.MaxInt64/amountSumUnit || high < math.MinInt64/amountSumUnit {
		return 0, ErrAmountOverflow
	}
	base := high * amountSumUnit
	if low > 0 && base > math.MaxInt64-low || low < 0 && base < math.MinInt64-low {
		return 0, ErrAmountOverflow
	}
	return base + low, nil
}

// costTotalsSQL is the statement FinOpsCostTotals runs over the cost lines.
func (t *sqlTables) costTotalsSQL(scope tenancy.OrgScope, query FinOpsCostQuery, groupBy []FinOpsDimension) (string, []any) {
	return t.totalsSQL("finops_cost_lines", "COUNT(*)", "", t.finOpsLineWhere(scope, query), groupBy)
}

// dailyTotalsSQL is the statement FinOpsCostTotals runs over the daily
// rollup. Its last column reports whether a group touched an overflowed row.
// A grouping by the whole rollup key reads each row as its group.
func (t *sqlTables) dailyTotalsSQL(scope tenancy.OrgScope, query FinOpsCostQuery, groupBy []FinOpsDimension) (string, []any) {
	where := t.finOpsLineWhere(scope, finOpsDailyQuery(query))
	if !finOpsDailyWholeKey(groupBy) {
		lines := "SUM(l.lines)"
		if t.postgres {
			lines = "CAST(SUM(l.lines) AS BIGINT)"
		}
		return t.totalsSQL("finops_cost_daily", lines, "MAX(CASE WHEN l.overflowed THEN 1 ELSE 0 END)", where, groupBy)
	}
	selects := make([]string, 0, len(groupBy)+6)
	order := make([]string, 0, len(groupBy))
	for _, dimension := range groupBy {
		column := "l." + finOpsDimensionColumns[dimension]
		order = append(order, column)
		if dimension == FinOpsByDay {
			column = t.dayColumn(column)
		}
		selects = append(selects, column)
	}
	selects = append(selects, "l.net_amortized_micros", "l.quantity_micros", "l.lines",
		"CASE WHEN l.estimated_team THEN 1 ELSE 0 END", "CASE WHEN l.estimated_region THEN 1 ELSE 0 END", "CASE WHEN l.overflowed THEN 1 ELSE 0 END")
	statement := `SELECT ` + strings.Join(selects, ", ") + ` FROM finops_cost_daily l WHERE ` + where.String() + ` ORDER BY ` + strings.Join(order, ", ")
	return t.q(statement), where.args
}

// monthlyTotalsSQL is dailyTotalsSQL for a grouping without the day over a
// range holding whole months: those months are read from the monthly rollup,
// each row standing on its first billing day, and the days around them from
// the daily rollup. It only serves a single-organization read: which
// organization answers is decided per payer-day, below a month row.
func (t *sqlTables) monthlyTotalsSQL(scope tenancy.OrgScope, query FinOpsCostQuery, groupBy []FinOpsDimension, months []string, head, tail [2]string) (string, []any) {
	const columns = `l.payer, l.account, l.region, l.service, l.service_name, l.usage_group, l.record_type, l.team,
 l.net_amortized_micros, l.quantity_micros, l.lines, l.estimated_team, l.estimated_region, l.overflowed`
	inMonths := query
	inMonths.From, inMonths.Through = "", ""
	where := t.finOpsLineWhere(scope, finOpsDailyQuery(inMonths))
	where.add("l.month >= ? AND l.month <= ?", months[0], months[len(months)-1])
	branches := []string{`SELECT l.min_day AS day, ` + columns + ` FROM finops_cost_monthly l WHERE ` + where.String()}
	args := append([]any{}, where.args...)
	for _, days := range [][2]string{head, tail} {
		if days[0] == "" {
			continue
		}
		edge := query
		edge.From, edge.Through = days[0], days[1]
		where := t.finOpsLineWhere(scope, finOpsDailyQuery(edge))
		branches = append(branches, `SELECT l.day, `+columns+` FROM finops_cost_daily l WHERE `+where.String())
		args = append(args, where.args...)
	}
	lines := "SUM(l.lines)"
	if t.postgres {
		lines = "CAST(SUM(l.lines) AS BIGINT)"
	}
	var all sqlWhere
	all.add("1 = 1")
	statement, _ := t.totalsSQL("("+strings.Join(branches, " UNION ALL ")+")", lines, "MAX(CASE WHEN l.overflowed THEN 1 ELSE 0 END)", all, groupBy)
	return statement, args
}

// totalsSQL groups table l by the dimensions. lines is the expression a
// group's line count is read from; overflow, when set, is one more column.
func (t *sqlTables) totalsSQL(table, lines, overflow string, where sqlWhere, groupBy []FinOpsDimension) (string, []any) {
	selects := make([]string, 0, len(groupBy)+8)
	groups := make([]string, 0, len(groupBy))
	for _, dimension := range groupBy {
		switch dimension {
		case FinOpsByDay:
			selects = append(selects, t.dayColumn("l.day"))
			groups = append(groups, "l.day")
		case FinOpsByMonth:
			month := t.monthExpression("l.day")
			if t.postgres {
				month += ` COLLATE "C"`
			}
			selects = append(selects, month)
			groups = append(groups, month)
		case FinOpsByUsageGroup, FinOpsByUnit:
			// These cost-line columns keep the database's default collation,
			// while every other dimension is declared byte-ordered. Ordering
			// them as byte strings agrees with the in-process backend. The
			// rollup declares its usage group byte-ordered already.
			column := "l." + finOpsDimensionColumns[dimension]
			if t.postgres && table == "finops_cost_lines" {
				column += ` COLLATE "C"`
			}
			selects = append(selects, column)
			groups = append(groups, column)
		default:
			column := "l." + finOpsDimensionColumns[dimension]
			selects = append(selects, column)
			groups = append(groups, column)
		}
	}
	selects = append(selects, t.amountSumColumns("l.net_amortized_micros")...)
	selects = append(selects, t.amountSumColumns("l.quantity_micros")...)
	selects = append(selects, lines, t.dayColumn("MIN(l.day)"),
		"MAX(CASE WHEN l.estimated_team THEN 1 ELSE 0 END)", "MAX(CASE WHEN l.estimated_region THEN 1 ELSE 0 END)")
	if overflow != "" {
		selects = append(selects, overflow)
	}
	statement := `SELECT ` + strings.Join(selects, ", ") + ` FROM ` + table + ` l WHERE ` + where.String()
	if len(groups) > 0 {
		statement += ` GROUP BY ` + strings.Join(groups, ", ") + ` ORDER BY ` + strings.Join(groups, ", ")
	}
	return t.q(statement), where.args
}

// FinOpsCostTotals reads a total the daily rollup can answer from the
// rollup, and anything else, or any total that touches an overflowed rollup
// row, from the cost lines.
func (t *sqlTables) FinOpsCostTotals(ctx context.Context, scope tenancy.OrgScope, query FinOpsCostQuery, groupBy ...FinOpsDimension) ([]FinOpsCostTotal, error) {
	if !validFinOpsQuery(query) || !validFinOpsDimensions(groupBy) {
		return nil, ErrInvalidFinOps
	}
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	if t.rollupsReconciled() && finOpsDailyAnswers(query, groupBy) {
		statement, args := t.dailyTotalsSQL(scope, query, groupBy)
		read := t.scanCostTotals
		if finOpsDailyWholeKey(groupBy) {
			read = t.scanDailyRows
		} else if months, head, tail := finOpsWholeMonths(query.From, query.Through); len(months) > 0 && len(scope.Read) == 1 && !slices.Contains(groupBy, FinOpsByDay) {
			statement, args = t.monthlyTotalsSQL(scope, query, groupBy, months, head, tail)
		}
		out, overflowed, err := read(ctx, db, statement, args, groupBy, true)
		if err != nil || !overflowed {
			return out, err
		}
	}
	statement, args := t.costTotalsSQL(scope, query, groupBy)
	out, _, err := t.scanCostTotals(ctx, db, statement, args, groupBy, false)
	return out, err
}

// scanCostTotals runs a totals statement and reads its groups. With
// overflowColumn it reports whether any group touched an overflowed rollup
// row; the groups are then incomplete and the caller reads the lines.
func (t *sqlTables) scanCostTotals(ctx context.Context, db *sql.DB, statement string, args []any, groupBy []FinOpsDimension, overflowColumn bool) ([]FinOpsCostTotal, bool, error) {
	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]FinOpsCostTotal, 0)
	overflowed := false
	for rows.Next() {
		values := make([]sql.NullString, len(groupBy))
		var net, quantity int64
		var count, team, region, overflow sql.NullInt64
		var minDay sql.NullString
		targets := make([]any, 0, len(groupBy)+9)
		for index := range values {
			if groupBy[index] == FinOpsByDay {
				targets = append(targets, &dayTarget{target: &values[index]})
				continue
			}
			targets = append(targets, &values[index])
		}
		var netSum, quantitySum sql.NullString
		var netHigh, netLow, quantityHigh, quantityLow sql.NullInt64
		if t.postgres {
			targets = append(targets, &netSum, &quantitySum)
		} else {
			targets = append(targets, &netHigh, &netLow, &quantityHigh, &quantityLow)
		}
		targets = append(targets, &count, &dayTarget{target: &minDay}, &team, &region)
		if overflowColumn {
			targets = append(targets, &overflow)
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, false, err
		}
		if overflow.Int64 > 0 {
			overflowed = true
			break
		}
		if t.postgres {
			if net, err = parseAmountSum(netSum.String); err != nil {
				return nil, false, err
			}
			if quantity, err = parseAmountSum(quantitySum.String); err != nil {
				return nil, false, err
			}
		} else {
			if net, err = combineAmountSum(netHigh.Int64, netLow.Int64); err != nil {
				return nil, false, err
			}
			if quantity, err = combineAmountSum(quantityHigh.Int64, quantityLow.Int64); err != nil {
				return nil, false, err
			}
		}
		if count.Int64 == 0 {
			continue
		}
		total := FinOpsCostTotal{MinDay: minDay.String, NetAmortizedMicros: net, QuantityMicros: quantity, Lines: count.Int64, EstimatedTeam: team.Int64 > 0, EstimatedRegion: region.Int64 > 0}
		for index, dimension := range groupBy {
			setFinOpsDimension(&total, dimension, values[index].String)
		}
		out = append(out, total)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if overflowed {
		return nil, true, nil
	}
	return out, false, nil
}

// scanDailyRows reads rollup rows as their own groups: the stored sums, the
// day as the group's first day, and whether the row overflowed. The targets
// are reused across rows, since a page reads tens of thousands of them.
func (t *sqlTables) scanDailyRows(ctx context.Context, db *sql.DB, statement string, args []any, groupBy []FinOpsDimension, _ bool) ([]FinOpsCostTotal, bool, error) {
	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	values := make([]string, len(groupBy))
	var day sql.NullString
	var net, quantity, lines, team, region, overflow int64
	targets := make([]any, 0, len(groupBy)+6)
	dayIndex := -1
	for index, dimension := range groupBy {
		if dimension == FinOpsByDay {
			dayIndex = index
			targets = append(targets, &dayTarget{target: &day})
			continue
		}
		targets = append(targets, &values[index])
	}
	targets = append(targets, &net, &quantity, &lines, &team, &region, &overflow)
	out := make([]FinOpsCostTotal, 0)
	overflowed := false
	for rows.Next() {
		if err := rows.Scan(targets...); err != nil {
			return nil, false, err
		}
		if overflow > 0 {
			overflowed = true
			break
		}
		if dayIndex >= 0 {
			values[dayIndex] = day.String
		}
		total := FinOpsCostTotal{MinDay: day.String, NetAmortizedMicros: net, QuantityMicros: quantity, Lines: lines, EstimatedTeam: team > 0, EstimatedRegion: region > 0}
		for index, dimension := range groupBy {
			setFinOpsDimension(&total, dimension, values[index])
		}
		out = append(out, total)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if overflowed {
		return nil, true, nil
	}
	return out, false, nil
}

// dayTarget scans a day of either backend into a nullable string.
type dayTarget struct{ target *sql.NullString }

func (d *dayTarget) Scan(src any) error {
	var day tableDayValue
	if err := day.Scan(src); err != nil {
		return err
	}
	*d.target = sql.NullString{String: day.Day, Valid: src != nil}
	return nil
}

func (t *sqlTables) PutFinOpsIncrease(ctx context.Context, scope tenancy.OrgScope, increase FinOpsIncrease) error {
	increase, err := cleanFinOpsIncrease(increase)
	if err != nil {
		return err
	}
	scope = inventoryScope(scope)
	org := scope.Write
	return t.write(ctx, func(tx *sql.Tx) error {
		row := []any{org, increase.ID, increase.Payer, increase.Account, increase.Service, increase.UsageType, increase.RecordType, increase.ResourceID,
			increase.StartedOn, optionalDay(increase.ResolvedOn), increase.Status, increase.OwnerTeam, increase.OwnerRule, increase.OwnerEstimated,
			increase.MonthlyImpactMicros, increase.ExpectedReason, optionalDay(increase.ExpectedUntil), t.at(increase.AlertedAt), jsonParam(increase.Suspects), jsonParam(increase.Detail)}
		updates := make([]string, 0, len(finOpsIncreaseColumns)-2)
		for _, column := range finOpsIncreaseColumns[2:] {
			updates = append(updates, column+" = excluded."+column)
		}
		if err := t.insertRows(ctx, tx, "finops_increases", finOpsIncreaseColumns, [][]any{row}, ` ON CONFLICT (org_id, id) DO UPDATE SET `+strings.Join(updates, ", ")); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM finops_increase_events WHERE org_id = ? AND increase_id = ?`), org, increase.ID); err != nil {
			return err
		}
		events := make([][]any, len(increase.Events))
		for index, event := range increase.Events {
			events[index] = []any{org, increase.ID, index, t.at(event.At), event.Actor, event.FromStatus, event.ToStatus, event.Reason, optionalDay(event.Until)}
		}
		return t.insertRows(ctx, tx, "finops_increase_events", []string{"org_id", "increase_id", "seq", "at", "actor", "from_status", "to_status", "reason", "until_day"}, events, "")
	})
}

// increasesSQL is the statement FinOpsIncreases runs.
func (t *sqlTables) increasesSQL(scope tenancy.OrgScope, query FinOpsIncreaseQuery) (string, []any) {
	var where sqlWhere
	condition, args := visible(scope.Read, "i", "finops_increases", "id")
	where.add(condition, args...)
	if len(query.IDs) > 0 {
		where.add("i.id IN ("+placeholders(len(query.IDs))+")", stringArgs(query.IDs)...)
	}
	if len(query.Statuses) > 0 {
		where.add("i.status IN ("+placeholders(len(query.Statuses))+")", stringArgs(query.Statuses)...)
	}
	where.equal("i.owner_team", query.OwnerTeam)
	where.equal("i.service", query.Service)
	return t.q(`SELECT i.org_id, i.id, i.payer, i.account, i.service, i.usage_type, i.record_type, i.resource_id, ` + t.dayColumn("i.started_on") + `, ` + t.dayColumn("i.resolved_on") + `,
 i.status, i.owner_team, i.owner_rule, i.owner_estimated, i.monthly_impact_micros, i.expected_reason, ` + t.dayColumn("i.expected_until") + `, i.alerted_at, i.suspects, i.detail
 FROM finops_increases i WHERE ` + where.String() + ` ORDER BY i.started_on, i.id`), where.args
}

func (t *sqlTables) FinOpsIncreases(ctx context.Context, scope tenancy.OrgScope, query FinOpsIncreaseQuery) ([]FinOpsIncrease, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	statement, args := t.increasesSQL(inventoryScope(scope), query)
	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	out := make([]FinOpsIncrease, 0)
	orgs := make([]string, 0)
	for rows.Next() {
		var increase FinOpsIncrease
		var org string
		var started, resolved, until tableDayValue
		var alerted tableTimeValue
		var suspects, detail []byte
		if err := rows.Scan(&org, &increase.ID, &increase.Payer, &increase.Account, &increase.Service, &increase.UsageType, &increase.RecordType, &increase.ResourceID,
			&started, &resolved, &increase.Status, &increase.OwnerTeam, &increase.OwnerRule, &increase.OwnerEstimated, &increase.MonthlyImpactMicros,
			&increase.ExpectedReason, &until, &alerted, &suspects, &detail); err != nil {
			rows.Close()
			return nil, err
		}
		increase.StartedOn, increase.ResolvedOn, increase.ExpectedUntil, increase.AlertedAt = started.Day, resolved.Day, until.Day, alerted.Time
		increase.Suspects, increase.Detail = scannedJSON(suspects), scannedJSON(detail)
		out = append(out, increase)
		orgs = append(orgs, org)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byOrg := map[string][]string{}
	position := map[string]int{}
	for index, increase := range out {
		byOrg[orgs[index]] = append(byOrg[orgs[index]], increase.ID)
		position[orgs[index]+"\x00"+increase.ID] = index
	}
	for _, org := range sortedKeysOf(byOrg) {
		err := inChunks(byOrg[org], func(chunk []string) error {
			args := append([]any{org}, stringArgs(chunk)...)
			events, err := db.QueryContext(ctx, t.q(`SELECT increase_id, at, actor, from_status, to_status, reason, `+t.dayColumn("until_day")+`
 FROM finops_increase_events WHERE org_id = ? AND increase_id IN (`+placeholders(len(chunk))+`) ORDER BY increase_id, seq`), args...)
			if err != nil {
				return err
			}
			defer events.Close()
			for events.Next() {
				var id string
				var event FinOpsIncreaseEvent
				var at tableTimeValue
				var until tableDayValue
				if err := events.Scan(&id, &at, &event.Actor, &event.FromStatus, &event.ToStatus, &event.Reason, &until); err != nil {
					return err
				}
				event.At, event.Until = at.Time, until.Day
				index := position[org+"\x00"+id]
				out[index].Events = append(out[index].Events, event)
			}
			return events.Err()
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (t *sqlTables) PutFinOpsReport(ctx context.Context, scope tenancy.OrgScope, report FinOpsReport) error {
	report, err := cleanFinOpsReport(report)
	if err != nil {
		return err
	}
	scope = inventoryScope(scope)
	return t.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, t.q(`INSERT INTO finops_reports (org_id, month, frozen_at, frozen_by, total_micros, model) VALUES (?, ?, ?, ?, ?, ?)
 ON CONFLICT (org_id, month) DO UPDATE SET frozen_at = excluded.frozen_at, frozen_by = excluded.frozen_by, total_micros = excluded.total_micros, model = excluded.model`),
			scope.Write, report.Month, t.at(report.FrozenAt), report.FrozenBy, report.TotalMicros, string(report.Model))
		return err
	})
}

func (t *sqlTables) FinOpsReport(ctx context.Context, scope tenancy.OrgScope, month string) (FinOpsReport, bool, error) {
	if !validFinOpsMonth(month) {
		return FinOpsReport{}, false, ErrInvalidFinOps
	}
	reports, err := t.finOpsReports(ctx, scope, month, true)
	if err != nil || len(reports) == 0 {
		return FinOpsReport{}, false, err
	}
	return reports[0], true, nil
}

func (t *sqlTables) FinOpsReports(ctx context.Context, scope tenancy.OrgScope) ([]FinOpsReport, error) {
	return t.finOpsReports(ctx, scope, "", false)
}

func (t *sqlTables) finOpsReports(ctx context.Context, scope tenancy.OrgScope, month string, model bool) ([]FinOpsReport, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	var where sqlWhere
	condition, args := visible(scope.Read, "r", "finops_reports", "month")
	where.add(condition, args...)
	where.equal("r.month", month)
	modelColumn := "NULL"
	if model {
		modelColumn = "r.model"
	}
	rows, err := db.QueryContext(ctx, t.q(`SELECT r.month, r.frozen_at, r.frozen_by, r.total_micros, `+modelColumn+` FROM finops_reports r WHERE `+where.String()+` ORDER BY r.month`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]FinOpsReport, 0)
	for rows.Next() {
		var report FinOpsReport
		var frozen tableTimeValue
		var document []byte
		if err := rows.Scan(&report.Month, &frozen, &report.FrozenBy, &report.TotalMicros, &document); err != nil {
			return nil, err
		}
		report.FrozenAt, report.Model = frozen.Time, scannedJSON(document)
		out = append(out, report)
	}
	return out, rows.Err()
}

func (t *sqlTables) PutFinOpsSavingDecision(ctx context.Context, scope tenancy.OrgScope, decision FinOpsSavingDecision) error {
	decision, err := cleanFinOpsDecision(decision)
	if err != nil {
		return err
	}
	scope = inventoryScope(scope)
	return t.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, t.q(`INSERT INTO finops_saving_decisions (org_id, saving_id, state, reason, actor, at, payer, account, kind, title, orphaned_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (org_id, saving_id) DO UPDATE SET state = excluded.state, reason = excluded.reason, actor = excluded.actor, at = excluded.at,
 payer = excluded.payer, account = excluded.account, kind = excluded.kind, title = excluded.title, orphaned_at = excluded.orphaned_at`),
			scope.Write, decision.SavingID, decision.State, decision.Reason, decision.Actor, t.at(decision.At),
			decision.Payer, decision.Account, decision.Kind, decision.Title, t.at(decision.OrphanedAt))
		return err
	})
}

func (t *sqlTables) FinOpsSavingDecisions(ctx context.Context, scope tenancy.OrgScope) ([]FinOpsSavingDecision, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	condition, args := visible(scope.Read, "d", "finops_saving_decisions", "saving_id")
	rows, err := db.QueryContext(ctx, t.q(`SELECT d.saving_id, d.state, d.reason, d.actor, d.at, d.payer, d.account, d.kind, d.title, d.orphaned_at
 FROM finops_saving_decisions d WHERE `+condition+` ORDER BY d.saving_id`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]FinOpsSavingDecision, 0)
	for rows.Next() {
		var decision FinOpsSavingDecision
		var at, orphaned tableTimeValue
		if err := rows.Scan(&decision.SavingID, &decision.State, &decision.Reason, &decision.Actor, &at,
			&decision.Payer, &decision.Account, &decision.Kind, &decision.Title, &orphaned); err != nil {
			return nil, err
		}
		decision.At, decision.OrphanedAt = at.Time, orphaned.Time
		out = append(out, decision)
	}
	return out, rows.Err()
}

// savingDecisionBatch bounds the saving IDs one statement names.
const savingDecisionBatch = 500

func (t *sqlTables) SyncFinOpsSavingDecisions(ctx context.Context, scope tenancy.OrgScope, present []string, at time.Time) (int64, int64, error) {
	keep, err := presentSavings(present, at)
	if err != nil {
		return 0, 0, err
	}
	scope = inventoryScope(scope)
	var orphaned, restored int64
	err = t.write(ctx, func(tx *sql.Tx) error {
		orphan, restore, err := t.savingDecisionChanges(ctx, tx, scope.Write, keep)
		if err != nil {
			return err
		}
		if orphaned, err = t.setOrphanedAt(ctx, tx, scope.Write, orphan, t.at(at), `orphaned_at IS NULL`); err != nil {
			return err
		}
		restored, err = t.setOrphanedAt(ctx, tx, scope.Write, restore, nil, `orphaned_at IS NOT NULL`)
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return orphaned, restored, nil
}

// savingDecisionChanges reads the organization's decisions and splits the
// ones whose orphan mark must change: absent and unmarked, present and marked.
func (t *sqlTables) savingDecisionChanges(ctx context.Context, tx *sql.Tx, org string, keep map[string]bool) (orphan, restore []string, err error) {
	rows, err := tx.QueryContext(ctx, t.q(`SELECT saving_id, orphaned_at FROM finops_saving_decisions WHERE org_id = ? ORDER BY saving_id`+t.lockClause(true)), org)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var mark tableTimeValue
		if err := rows.Scan(&id, &mark); err != nil {
			return nil, nil, err
		}
		switch {
		case !keep[id] && mark.Time.IsZero():
			orphan = append(orphan, id)
		case keep[id] && !mark.Time.IsZero():
			restore = append(restore, id)
		}
	}
	return orphan, restore, rows.Err()
}

// setOrphanedAt sets orphaned_at on the listed decisions that match the
// guard, in batches, and returns how many changed.
func (t *sqlTables) setOrphanedAt(ctx context.Context, tx *sql.Tx, org string, ids []string, value any, guard string) (int64, error) {
	var count int64
	for start := 0; start < len(ids); start += savingDecisionBatch {
		batch := ids[start:min(start+savingDecisionBatch, len(ids))]
		args := append([]any{value, org}, stringArgs(batch)...)
		result, err := tx.ExecContext(ctx, t.q(`UPDATE finops_saving_decisions SET orphaned_at = ? WHERE org_id = ? AND `+guard+` AND saving_id IN (`+placeholders(len(batch))+`)`), args...)
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		count += changed
	}
	return count, nil
}

func (t *sqlTables) FillFinOpsSavingDecisionSubjects(ctx context.Context, scope tenancy.OrgScope, subjects []FinOpsSavingDecision) (int64, error) {
	fills, err := decisionSubjects(subjects)
	if err != nil {
		return 0, err
	}
	scope = inventoryScope(scope)
	var count int64
	if len(fills) == 0 {
		return 0, nil
	}
	err = t.write(ctx, func(tx *sql.Tx) error {
		count = 0
		unfilled, err := t.decisionsWithoutPayer(ctx, tx, scope.Write)
		if err != nil {
			return err
		}
		for _, decision := range unfilled {
			subject, ok := fills[decision.SavingID]
			if !ok || !fillDecisionSubject(&decision, subject) {
				continue
			}
			result, err := tx.ExecContext(ctx, t.q(`UPDATE finops_saving_decisions SET payer = ?, account = ?, kind = ?, title = ?
 WHERE org_id = ? AND saving_id = ? AND payer = ''`),
				decision.Payer, decision.Account, decision.Kind, decision.Title, scope.Write, decision.SavingID)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			count += changed
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// decisionsWithoutPayer reads, locked for the update, the organization's
// decisions whose payer is empty with the subject they hold.
func (t *sqlTables) decisionsWithoutPayer(ctx context.Context, tx *sql.Tx, org string) ([]FinOpsSavingDecision, error) {
	rows, err := tx.QueryContext(ctx, t.q(`SELECT saving_id, account, kind, title FROM finops_saving_decisions WHERE org_id = ? AND payer = '' ORDER BY saving_id`+t.lockClause(true)), org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FinOpsSavingDecision
	for rows.Next() {
		var decision FinOpsSavingDecision
		if err := rows.Scan(&decision.SavingID, &decision.Account, &decision.Kind, &decision.Title); err != nil {
			return nil, err
		}
		out = append(out, decision)
	}
	return out, rows.Err()
}

func (t *sqlTables) DeleteFinOpsSavingDecisionsForPayer(ctx context.Context, scope tenancy.OrgScope, payer string) (int64, error) {
	if !validTableKey(payer) {
		return 0, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	var count int64
	err := t.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, t.q(`DELETE FROM finops_saving_decisions WHERE org_id = ? AND payer = ?`), scope.Write, payer)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	})
	return count, err
}

func (t *sqlTables) DeleteFinOpsSavingDecisionsWithoutPayer(ctx context.Context, scope tenancy.OrgScope, accounts []string) (int64, error) {
	named, err := decisionAccounts(accounts)
	if err != nil {
		return 0, err
	}
	list := make([]string, 0, len(named))
	for account := range named {
		list = append(list, account)
	}
	sort.Strings(list)
	scope = inventoryScope(scope)
	var count int64
	if len(list) == 0 {
		return 0, nil
	}
	err = t.write(ctx, func(tx *sql.Tx) error {
		count = 0
		for start := 0; start < len(list); start += savingDecisionBatch {
			batch := list[start:min(start+savingDecisionBatch, len(list))]
			args := append([]any{scope.Write}, stringArgs(batch)...)
			result, err := tx.ExecContext(ctx, t.q(`DELETE FROM finops_saving_decisions WHERE org_id = ? AND payer = '' AND account IN (`+placeholders(len(batch))+`)`), args...)
			if err != nil {
				return err
			}
			deleted, err := result.RowsAffected()
			if err != nil {
				return err
			}
			count += deleted
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (t *sqlTables) PruneFinOpsSavingDecisions(ctx context.Context, scope tenancy.OrgScope, orphanedBefore time.Time) (int64, error) {
	if orphanedBefore.IsZero() {
		return 0, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	var count int64
	err := t.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, t.q(`DELETE FROM finops_saving_decisions WHERE org_id = ? AND orphaned_at IS NOT NULL AND orphaned_at < ?`), scope.Write, t.at(orphanedBefore))
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	})
	return count, err
}
