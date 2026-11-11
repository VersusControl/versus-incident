package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"strings"
	"time"
)

var (
	_ FinOpsRollupStore = (*sqlTables)(nil)
	_ FinOpsRollupStore = (*memoryTables)(nil)
)

// rollupsReconciled reports whether totals may read the rollups.
func (t *sqlTables) rollupsReconciled() bool {
	return t.rollupsReady != nil && t.rollupsReady.Load()
}

func (t *sqlTables) FinOpsRollupsReconciled() bool { return t.rollupsReconciled() }

func (t *sqlTables) MarkFinOpsRollupsReconciled() {
	if t.rollupsReady != nil {
		t.rollupsReady.Store(true)
	}
}

// ReconcileFinOpsRollups implements FinOpsRollupStore. It compares whole
// rollup rows, every sum, count and flag by the full rollup key, across
// every organization in the tables: a rewrite that keeps a day's count and
// total (usage moved between teams, an estimate flag flipped) still differs.
func (t *sqlTables) ReconcileFinOpsRollups(ctx context.Context) (FinOpsRollupRepair, error) {
	var repair FinOpsRollupRepair
	db, err := t.tablesDB()
	if err != nil {
		return repair, err
	}
	days, err := t.rollupDiff(ctx, db, t.rollupDayDiffSQL())
	if err != nil {
		return repair, fmt.Errorf("compare daily rollup: %w", err)
	}
	repair.Days = len(days)
	months := map[[3]string]bool{}
	for _, d := range days {
		org, payer, onDay := d[0], d[1], d[2]
		if err := t.write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM finops_cost_daily WHERE org_id = ? AND payer = ? AND day = ?`), org, payer, onDay); err != nil {
				return err
			}
			dayLines, err := t.dayLines(ctx, tx, org, payer, onDay)
			if err != nil {
				return err
			}
			return t.insertDaily(ctx, tx, org, rollUpFinOpsLines(dayLines))
		}); err != nil {
			return repair, fmt.Errorf("rebuild daily rollup %s %s: %w", payer, onDay, err)
		}
		repair.RebuiltDays++
		months[[3]string{org, payer, finOpsMonthOf(onDay)}] = true
	}
	stale, err := t.rollupDiff(ctx, db, t.rollupMonthDiffSQL())
	if err != nil {
		return repair, fmt.Errorf("compare monthly rollup: %w", err)
	}
	for _, m := range stale {
		months[m] = true
	}
	for m := range months {
		if err := t.write(ctx, func(tx *sql.Tx) error { return t.rebuildMonthly(ctx, tx, m[0], m[1], m[2]) }); err != nil {
			return repair, fmt.Errorf("rebuild monthly rollup %s %s: %w", m[1], m[2], err)
		}
		repair.RebuiltMonths++
	}
	if repair.RebuiltDays >= rollupVacuumDays {
		repair.Vacuumed = t.vacuumRollups(ctx, db)
	}
	t.MarkFinOpsRollupsReconciled()
	return repair, nil
}

// rollupVacuumDays is how many rebuilt days make a reconcile a bulk rewrite
// of the rollups: about a month of days, where a reconcile after a scheduled
// refresh rebuilds none.
const rollupVacuumDays = 31

// Bounds on the post-rebuild vacuum. It runs under the replica lock, so it
// must never wait on another lock holder (an anti-wraparound autovacuum, an
// index built concurrently) for long: past these it gives up and leaves the
// tables to autovacuum.
const (
	rollupVacuumLockTimeout      = "5s"
	rollupVacuumStatementTimeout = "2min"
	rollupVacuumDeadline         = 3 * time.Minute
)

// vacuumRollups vacuums and analyzes both rollup tables on Postgres after a
// bulk rebuild, before totals read them. A rebuild deletes and inserts every
// row of each day it rebuilds: until a vacuum runs, the tables hold that many
// dead rows, every index-only read of the daily rollup goes back to the table
// for each row it returns, and the planner's row counts are those an
// autovacuum sampled partway through the rebuild. The autovacuum that would
// fix it runs later, beside the next process's reads. VACUUM cannot run in a
// transaction, so it runs on its own, on a connection of its own with lock
// and statement timeouts; that connection is then discarded so the settings
// never reach the pool. A role that does not own the tables, a timeout or
// any other failure only leaves the tables to autovacuum. SQLite keeps no
// dead rows or visibility map and reads as fast after a rebuild as before,
// so it does nothing there. It reports whether the tables were vacuumed.
func (t *sqlTables) vacuumRollups(ctx context.Context, db *sql.DB) bool {
	if !t.postgres {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, rollupVacuumDeadline)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		log.Printf("storage: rollup vacuum skipped: %v", err)
		return false
	}
	defer func() {
		// Discard the connection: its session settings stay out of the pool.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
	}()
	return vacuumRollupsOn(ctx, conn)
}

// vacuumRollupsOn runs the bounded vacuum on one connection, after checking
// that its role may vacuum both rollup tables: Postgres only warns and skips
// a table it may not vacuum, which would otherwise read as done.
func vacuumRollupsOn(ctx context.Context, conn *sql.Conn) bool {
	var owned int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = current_schema() AND c.relname IN ('finops_cost_daily', 'finops_cost_monthly')
 AND pg_has_role(current_user, c.relowner, 'USAGE')`).Scan(&owned); err != nil {
		log.Printf("storage: rollup vacuum skipped: check table owner: %v", err)
		return false
	}
	if owned != 2 {
		log.Printf("storage: rollup vacuum skipped: role does not own tables")
		return false
	}
	for _, set := range []string{
		`SET lock_timeout = '` + rollupVacuumLockTimeout + `'`,
		`SET statement_timeout = '` + rollupVacuumStatementTimeout + `'`,
	} {
		if _, err := conn.ExecContext(ctx, set); err != nil {
			log.Printf("storage: rollup vacuum skipped: %v", err)
			return false
		}
	}
	began := time.Now()
	if _, err := conn.ExecContext(ctx, `VACUUM (ANALYZE) finops_cost_daily, finops_cost_monthly`); err != nil {
		log.Printf("storage: rollup vacuum after a rebuild gave up after %s: %v", time.Since(began).Round(time.Millisecond), err)
		return false
	}
	log.Printf("storage: cost rollups vacuumed and analyzed after a rebuild in %s", time.Since(began).Round(time.Millisecond))
	return true
}

// rollupKeyColumns are the rollup key columns after organization, payer and
// day, in the order both sides of a comparison select them.
var rollupKeyColumns = []string{"account", "region", "service", "service_name", "usage_group", "record_type", "team"}

// rollupValueColumns are the daily rollup's value columns a comparison
// checks.
var rollupValueColumns = []string{"net_amortized_micros", "quantity_micros", "lines", "estimated_team", "estimated_region"}

// exactSum renders the exact sum of an integer column. Postgres sums into
// NUMERIC. SQLite raises on an integer overflow, so it sums 32-bit halves,
// which never overflow, and rebuilds the total; a total outside the 64-bit
// range becomes a real number there, never matches a stored row, and its day
// is rebuilt, which stores it overflowed again.
func (t *sqlTables) exactSum(column string) string {
	if t.postgres {
		return "SUM(" + column + ")"
	}
	high := column + " / 4294967296"
	return "(SUM(" + high + ") * 4294967296 + SUM(" + column + " - (" + high + ") * 4294967296))"
}

// fits renders whether an exactSum result is a signed 64-bit integer.
func (t *sqlTables) fits(sum string) string {
	if t.postgres {
		return sum + " BETWEEN -9223372036854775808 AND 9223372036854775807"
	}
	return "typeof(" + sum + ") = 'integer'"
}

// amountsOrNull renders the net and quantity sums of a group, both NULL when
// either does not fit (or overflowed is true), as an overflowed rollup row
// stores no sums: such a group then compares equal to its overflowed row.
func (t *sqlTables) amountsOrNull(net, quantity, overflowed string) string {
	ok := "(" + t.fits(net) + ") AND (" + t.fits(quantity) + ")"
	if overflowed != "" {
		ok = "NOT (" + overflowed + ") AND " + ok
	}
	return "CASE WHEN " + ok + " THEN " + net + " END, CASE WHEN " + ok + " THEN " + quantity + " END"
}

// storedOrNull renders a rollup row's stored net and quantity, NULL when the
// row is overflowed.
func (t *sqlTables) storedOrNull() string {
	return "CASE WHEN overflowed THEN NULL ELSE " + t.stored("net_amortized_micros") + " END, CASE WHEN overflowed THEN NULL ELSE " +
		t.stored("quantity_micros") + " END"
}

// stored renders a stored integer so it compares with exactSum.
func (t *sqlTables) stored(column string) string {
	if t.postgres {
		return "CAST(" + column + " AS NUMERIC)"
	}
	return column
}

// anyTrue renders whether any row of a group has a flag set.
func (t *sqlTables) anyTrue(column string) string {
	if t.postgres {
		return "bool_or(" + column + ")"
	}
	return "MAX(" + column + ")"
}

// rollupKeys renders the key columns. On Postgres the cost lines' usage
// group is compared byte-ordered, as the rollup declares it.
func (t *sqlTables) rollupKeys(lines bool) string {
	keys := make([]string, len(rollupKeyColumns))
	for index, column := range rollupKeyColumns {
		keys[index] = column
		if lines && t.postgres && column == "usage_group" {
			keys[index] = `usage_group COLLATE "C"`
		}
	}
	return strings.Join(keys, ", ")
}

// rollupValues renders the value columns of one side of the day comparison:
// summed from the lines, or as the daily rollup stores them.
func (t *sqlTables) rollupValues(fromLines bool) string {
	values := make([]string, 0, len(rollupValueColumns))
	for _, column := range rollupValueColumns {
		flag := strings.HasPrefix(column, "estimated_")
		switch {
		case column == "net_amortized_micros" && fromLines:
			values = append(values, t.amountsOrNull(t.exactSum("net_amortized_micros"), t.exactSum("quantity_micros"), ""))
		case column == "net_amortized_micros":
			values = append(values, t.storedOrNull())
		case column == "quantity_micros":
			// Rendered with the net amount.
		case !fromLines && flag:
			values = append(values, column)
		case !fromLines:
			values = append(values, t.stored(column))
		case flag:
			values = append(values, t.anyTrue(column))
		default:
			values = append(values, t.stored("COUNT(*)"))
		}
	}
	return strings.Join(values, ", ")
}

// rollupDayDiffSQL lists the organization, payer and day of every daily
// rollup row that differs from the lines summed by the rollup key, in either
// direction: a row missing, extra, or with any sum, count or flag different.
func (t *sqlTables) rollupDayDiffSQL() string {
	lines := `SELECT org_id, payer, day, ` + t.rollupKeys(true) + `, ` + t.rollupValues(true) +
		` FROM finops_cost_lines WHERE resource_id = '' GROUP BY org_id, payer, day, ` + t.rollupKeys(true)
	daily := `SELECT org_id, payer, day, ` + t.rollupKeys(false) + `, ` + t.rollupValues(false) + ` FROM finops_cost_daily`
	day := t.dayColumn("day")
	return `SELECT org_id, payer, ` + day + ` FROM (` + lines + ` EXCEPT ` + daily + `) missing
 UNION SELECT org_id, payer, ` + day + ` FROM (` + daily + ` EXCEPT ` + lines + `) extra`
}

// rollupMonthDiffSQL lists the organization, payer and month (its first
// day) of every monthly rollup row that differs from its daily rows summed.
func (t *sqlTables) rollupMonthDiffSQL() string {
	month := "substr(day, 1, 7) || '-01'"
	if t.postgres {
		month = "CAST(date_trunc('month', day) AS DATE)"
	}
	fromDays := "MIN(day), " + t.amountsOrNull(t.exactSum("net_amortized_micros"), t.exactSum("quantity_micros"), t.anyTrue("overflowed")) + ", " +
		t.exactSum("lines") + ", " + t.anyTrue("estimated_team") + ", " + t.anyTrue("estimated_region")
	asStored := "min_day, " + t.storedOrNull() + ", " + t.stored("lines") + ", estimated_team, estimated_region"
	days := `SELECT org_id, payer, ` + month + ` AS month, ` + t.rollupKeys(false) + `, ` + fromDays +
		` FROM finops_cost_daily GROUP BY org_id, payer, ` + month + `, ` + t.rollupKeys(false)
	monthly := `SELECT org_id, payer, month, ` + t.rollupKeys(false) + `, ` + asStored + ` FROM finops_cost_monthly`
	first := t.dayColumn("month")
	return `SELECT org_id, payer, ` + first + ` FROM (` + days + ` EXCEPT ` + monthly + `) missing
 UNION SELECT org_id, payer, ` + first + ` FROM (` + monthly + ` EXCEPT ` + days + `) extra`
}

// rollupDiff runs a diff statement and returns its organization, payer and
// day (or month) triples.
func (t *sqlTables) rollupDiff(ctx context.Context, db *sql.DB, statement string) ([][3]string, error) {
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var org, payer string
		var day tableDayValue
		if err := rows.Scan(&org, &payer, &day); err != nil {
			return nil, err
		}
		out = append(out, [3]string{org, payer, day.Day})
	}
	return out, rows.Err()
}

// The in-process tables keep lines and rollup under one lock, in one
// process: nothing else writes them, so they cannot disagree and totals
// always read the rollup.

func (m *memoryTables) ReconcileFinOpsRollups(context.Context) (FinOpsRollupRepair, error) {
	return FinOpsRollupRepair{}, nil
}

func (m *memoryTables) MarkFinOpsRollupsReconciled() {}

func (m *memoryTables) FinOpsRollupsReconciled() bool { return true }
