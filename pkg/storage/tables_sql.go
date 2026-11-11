package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sqlTables implements AWSInventoryStore and FinOpsStore over SQL. Postgres
// and the local SQLite index share every statement; the dialect only
// renders placeholders, times, the month expression and row locks.
type sqlTables struct {
	postgres bool
	path     string

	openMu sync.Mutex
	db     *sql.DB
	// writeMu serializes SQLite write transactions in this process, which
	// keeps a deferred transaction from failing to upgrade its lock. Postgres
	// relies on row locks instead.
	writeMu sync.Mutex

	// rollupsReady is set once this process has found the cost rollups to
	// agree with the cost lines (ReconcileFinOpsRollups or
	// MarkFinOpsRollupsReconciled). Until then every total reads the lines.
	// It is shared by the statement sets bound to one provider's pools.
	rollupsReady *atomic.Bool
}

var errLocalTablesDriverMissing = errors.New("storage: local AWS and FinOps tables require host registration: import _ \"github.com/VersusControl/versus-incident/pkg/storage/sqlitedriver\"")

// localTablesFile is the file-mode SQLite database for the AWS and FinOps
// tables.
const localTablesFile = "aws-finops.sqlite"

func newPostgresTables(db *sql.DB) *sqlTables {
	return &sqlTables{postgres: true, db: db, rollupsReady: new(atomic.Bool)}
}

// newSQLiteTables opens the database file at path on first use, so a
// deployment that never collects AWS or FinOps data never creates it.
func newSQLiteTables(path string) *sqlTables {
	return &sqlTables{path: path, rollupsReady: new(atomic.Bool)}
}

func (t *sqlTables) tablesDB() (*sql.DB, error) {
	if t == nil {
		return nil, ErrUnsupported
	}
	t.openMu.Lock()
	defer t.openMu.Unlock()
	if t.db != nil {
		return t.db, nil
	}
	if t.postgres {
		return nil, ErrUnsupported
	}
	db, err := openLocalTables(t.path)
	if err != nil {
		return nil, err
	}
	t.db = db
	return db, nil
}

func (t *sqlTables) closeTables() error {
	if t == nil || t.postgres {
		return nil
	}
	t.openMu.Lock()
	defer t.openMu.Unlock()
	if t.db == nil {
		return nil
	}
	err := t.db.Close()
	t.db = nil
	return err
}

var sqliteDateType = regexp.MustCompile(`\bDATE\b`)

// sqliteIndexInclude moves the columns of a Postgres index INCLUDE clause
// into the key list. SQLite has no INCLUDE, but a column at the end of the
// key is still read from the index entry, so the index stays covering for a
// statement that only reads those columns.
var sqliteIndexInclude = regexp.MustCompile(`\)\s+INCLUDE\s*\(([^()]*)\)`)

// localTableMigrations are the migrations the local SQLite tables are built
// from, in order.
var localTableMigrations = []string{
	"migrations/031_aws_inventory_tables.sql",
	"migrations/032_finops_tables.sql",
	"migrations/033_finops_cost_lines_region_index.sql",
	"migrations/034_finops_cost_lines_totals_index.sql",
	"migrations/035_finops_saving_decision_subject.sql",
	"migrations/036_finops_cost_daily.sql",
}

// sqliteAddColumn matches one rendered ALTER TABLE … ADD COLUMN IF NOT
// EXISTS statement. SQLite has no IF NOT EXISTS for a column and the local
// schema is applied on every open, so each one becomes a column added only
// when the table lacks it.
var sqliteAddColumn = regexp.MustCompile(`(?m)^ALTER TABLE (\w+) ADD COLUMN IF NOT EXISTS (\w+) ([^;]+);[ \t]*$`)

// localColumn is one column a migration adds to an existing table.
type localColumn struct{ table, column, definition string }

// localSchemaStep is one migration rendered for SQLite: the columns it adds
// to tables an earlier migration created, then its other statements.
type localSchemaStep struct {
	columns    []localColumn
	statements string
}

// renderSQLite rewrites Postgres-only types and clauses for SQLite.
func renderSQLite(text string) string {
	text = strings.ReplaceAll(text, ` COLLATE "C"`, "")
	text = sqliteIndexInclude.ReplaceAllString(text, ", $1)")
	text = strings.NewReplacer("TIMESTAMPTZ", "TEXT", "JSONB", "TEXT", "BOOLEAN", "INTEGER", "FALSE", "0").Replace(text)
	return sqliteDateType.ReplaceAllString(text, "TEXT")
}

// localTablesSchema renders the Postgres migrations for SQLite, so both
// backends always have the same tables, columns, keys and indexes.
func localTablesSchema() ([]localSchemaStep, error) {
	steps := make([]localSchemaStep, 0, len(localTableMigrations))
	for _, name := range localTableMigrations {
		data, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			return nil, err
		}
		text := renderSQLite(string(data))
		var step localSchemaStep
		for _, match := range sqliteAddColumn.FindAllStringSubmatch(text, -1) {
			step.columns = append(step.columns, localColumn{table: match[1], column: match[2], definition: strings.TrimSpace(match[3])})
		}
		step.statements = sqliteAddColumn.ReplaceAllString(text, "")
		steps = append(steps, step)
	}
	return steps, nil
}

// applyLocalSchema builds or upgrades the local tables in migration order.
// A column a migration adds is added in place when the table lacks it, so a
// database an earlier version created gains it instead of failing.
func applyLocalSchema(ctx context.Context, db *sql.DB, steps []localSchemaStep) error {
	for _, step := range steps {
		for _, column := range step.columns {
			if err := addLocalColumn(ctx, db, column); err != nil {
				return err
			}
		}
		if _, err := db.ExecContext(ctx, step.statements); err != nil {
			return err
		}
	}
	return nil
}

func addLocalColumn(ctx context.Context, db *sql.DB, column localColumn) error {
	have, err := tableColumns(ctx, db, column.table, false)
	if err != nil || have[column.column] {
		return err
	}
	// The table and column names come from the embedded migrations, never
	// from input.
	_, err = db.ExecContext(ctx, `ALTER TABLE `+column.table+` ADD COLUMN `+column.column+` `+column.definition)
	if err != nil {
		// Another opener may have added it first.
		if have, checkErr := tableColumns(ctx, db, column.table, false); checkErr == nil && have[column.column] {
			return nil
		}
	}
	return err
}

func openLocalTables(path string) (*sql.DB, error) {
	if !localEvidenceDriverRegistered() {
		return nil, errLocalTablesDriverMissing
	}
	// A relative path in a file: URI parses as an authority, so the URI
	// always carries an absolute path.
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := validateLocalEvidenceArtifact(absolute + suffix); err != nil {
			return nil, err
		}
	}
	if err := prepareLocalEvidenceArtifact(absolute); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: absolute}).String() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	steps, err := localTablesSchema()
	if err == nil {
		err = applyLocalSchema(context.Background(), db, steps)
	}
	if err != nil {
		// A statement that names a column a newer migration added fails on a
		// database built by an older one. Report that as the classed drift
		// error the startup check expects, not as an unclassified SQL error.
		if driftErr := assertTableSchema(context.Background(), db, false); errors.Is(driftErr, ErrSchemaOutOfDate) {
			_ = db.Close()
			return nil, driftErr
		}
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// q renders ? placeholders for the dialect.
func (t *sqlTables) q(query string) string {
	if !t.postgres {
		return query
	}
	var out strings.Builder
	out.Grow(len(query) + 16)
	position := 0
	for index := 0; index < len(query); index++ {
		if query[index] == '?' {
			position++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(position))
			continue
		}
		out.WriteByte(query[index])
	}
	return out.String()
}

// sqliteTimeLayout is fixed-width so stored times sort as text.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000Z"

// at renders a time parameter; the zero time is NULL.
func (t *sqlTables) at(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	value = tableTime(value)
	if t.postgres {
		return value
	}
	return value.Format(sqliteTimeLayout)
}

// day renders an optional day parameter; "" is NULL.
func optionalDay(day string) any {
	if day == "" {
		return nil
	}
	return day
}

func jsonParam(document []byte) any {
	if emptyJSON(document) {
		return nil
	}
	return string(document)
}

func (t *sqlTables) lockClause(exclusive bool) string {
	if !t.postgres {
		return ""
	}
	if exclusive {
		return " FOR UPDATE"
	}
	return " FOR SHARE"
}

func (t *sqlTables) monthExpression(column string) string {
	if t.postgres {
		return "to_char(" + column + ", 'YYYY-MM')"
	}
	return "substr(" + column + ", 1, 7)"
}

// dayColumn selects a day as text on every backend.
func (t *sqlTables) dayColumn(column string) string {
	if t.postgres {
		return "to_char(" + column + ", 'YYYY-MM-DD')"
	}
	return column
}

// write runs fn in one transaction.
func (t *sqlTables) write(ctx context.Context, fn func(*sql.Tx) error) error {
	db, err := t.tablesDB()
	if err != nil {
		return err
	}
	if !t.postgres {
		t.writeMu.Lock()
		defer t.writeMu.Unlock()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// placeholders renders count comma-separated placeholders.
func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func stringArgs(values []string) []any {
	out := make([]any, len(values))
	for index, value := range values {
		out[index] = value
	}
	return out
}

// visible keeps a row of alias only when its organization is the first, in
// read order, to hold the row's guard key in the guard table: no guard row
// with the same key belongs to an organization ranked before it. The outer
// condition stays a plain org_id list, so every index on (org_id, ...)
// serves the scan; one organization is a plain equality.
func visible(orgs []string, alias, guard string, keys ...string) (string, []any) {
	if len(orgs) == 1 {
		return alias + ".org_id = ?", []any{orgs[0]}
	}
	rank := func(column string) (string, []any) {
		var expression strings.Builder
		args := make([]any, 0, len(orgs))
		expression.WriteString("CASE ")
		expression.WriteString(column)
		for index, org := range orgs {
			expression.WriteString(" WHEN ? THEN ")
			expression.WriteString(strconv.Itoa(index))
			args = append(args, org)
		}
		expression.WriteString(" END")
		return expression.String(), args
	}
	match := make([]string, len(keys))
	for position, key := range keys {
		match[position] = "g." + key + " = " + alias + "." + key
	}
	guardRank, guardArgs := rank("g.org_id")
	rowRank, rowArgs := rank(alias + ".org_id")
	args := append(stringArgs(orgs), stringArgs(orgs)...)
	args = append(append(args, guardArgs...), rowArgs...)
	return alias + ".org_id IN (" + placeholders(len(orgs)) + ") AND NOT EXISTS (SELECT 1 FROM " + guard + " g WHERE g.org_id IN (" + placeholders(len(orgs)) + ") AND " +
		strings.Join(match, " AND ") + " AND " + guardRank + " < " + rowRank + ")", args
}

// temporal keeps rows current at asOf, or current now when asOf is zero.
func (t *sqlTables) temporal(alias string, asOf time.Time) (string, []any) {
	if asOf.IsZero() {
		return alias + ".valid_to IS NULL", nil
	}
	at := t.at(asOf)
	return "(" + alias + ".valid_from <= ? AND (" + alias + ".valid_to IS NULL OR " + alias + ".valid_to > ?))", []any{at, at}
}

// sqlWhere collects conditions and their arguments in order.
type sqlWhere struct {
	terms []string
	args  []any
}

func (w *sqlWhere) add(term string, args ...any) {
	w.terms = append(w.terms, term)
	w.args = append(w.args, args...)
}

func (w *sqlWhere) equal(column, value string) {
	if value != "" {
		w.add(column+" = ?", value)
	}
}

// in adds column IN (...) for a non-empty list; an empty list filters
// nothing.
func (w *sqlWhere) in(column string, values []string) {
	if len(values) == 0 {
		return
	}
	w.add(column+" IN ("+placeholders(len(values))+")", stringArgs(values)...)
}

func (w *sqlWhere) String() string { return strings.Join(w.terms, " AND ") }

// tableTimeValue scans a time stored by either backend.
type tableTimeValue struct{ Time time.Time }

func (v *tableTimeValue) Scan(src any) error {
	switch value := src.(type) {
	case nil:
		v.Time = time.Time{}
	case time.Time:
		v.Time = tableTime(value)
	case string:
		return v.parse(value)
	case []byte:
		return v.parse(string(value))
	default:
		return fmt.Errorf("storage: cannot scan %T as time", src)
	}
	return nil
}

func (v *tableTimeValue) parse(text string) error {
	if text == "" {
		v.Time = time.Time{}
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return err
	}
	v.Time = tableTime(parsed)
	return nil
}

// tableDayValue scans a day stored by either backend as "YYYY-MM-DD".
type tableDayValue struct{ Day string }

func (v *tableDayValue) Scan(src any) error {
	switch value := src.(type) {
	case nil:
		v.Day = ""
	case time.Time:
		v.Day = value.Format(time.DateOnly)
	case string:
		v.Day = value
	case []byte:
		v.Day = string(value)
	default:
		return fmt.Errorf("storage: cannot scan %T as day", src)
	}
	if len(v.Day) > 10 {
		v.Day = v.Day[:10]
	}
	return nil
}

// insertRows writes rows with multi-row inserts, each under the parameter
// budget of both databases.
func (t *sqlTables) insertRows(ctx context.Context, tx *sql.Tx, table string, columns []string, rows [][]any, suffix string) error {
	if len(rows) == 0 {
		return nil
	}
	per := 8000 / len(columns)
	if per < 1 {
		per = 1
	}
	row := "(" + placeholders(len(columns)) + ")"
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*len(columns))
		for _, item := range rows[start:end] {
			values = append(values, row)
			args = append(args, item...)
		}
		statement := "INSERT INTO " + table + " (" + strings.Join(columns, ", ") + ") VALUES " + strings.Join(values, ", ") + suffix
		if _, err := tx.ExecContext(ctx, t.q(statement), args...); err != nil {
			return err
		}
	}
	return nil
}

// inChunks runs fn over values in chunks small enough for one statement.
func inChunks(values []string, fn func([]string) error) error {
	const size = 4000
	for start := 0; start < len(values); start += size {
		if err := fn(values[start:min(start+size, len(values))]); err != nil {
			return err
		}
	}
	return nil
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
