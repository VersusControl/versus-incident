package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// keptEventIndexes are the definitions every month of the journal carries:
// the column list and predicate of each index, as pg_get_indexdef prints them
// after "USING btree ".
var keptEventIndexes = []string{
	"(scope_key, event_id, event_version, event_at)",
	"(scope_key, event_type, event_at DESC, event_id DESC)",
	"(scope_key, event_type, observed_at DESC, event_id DESC)",
	"(scope_key, gap_to DESC, event_id DESC) WHERE (event_type = 'coverage_gap'::text)",
	"(scope_key, namespace, event_at DESC, event_id DESC)",
	"(scope_key, namespace, kind, resource_name, event_at DESC, event_id DESC)",
	"(scope_key, root_uid, event_at DESC, event_id DESC)",
	"(scope_key, sequence DESC)",
	"(sequence, event_at)",
}

// guideDroppedIndexes are the journal indexes a fresh installation has that
// the migration guide removes. The schema itself drops the unread ones the
// guide also names, so a fresh installation no longer has those.
var guideDroppedIndexes = []string{
	"evidence_partition_current_break",
	"evidence_partition_current_root",
	"evidence_partition_current_type",
	"evidence_partition_event_pod",
	"evidence_partition_event_priority",
	"evidence_partition_event_rule",
	"evidence_partition_event_time",
	"evidence_partition_event_uid",
	"kubernetes_partition_termination_markers",
}

// guideIndexDropQuery is the migration guide's query: it writes one drop
// statement for each unused journal index the installation has.
const guideIndexDropQuery = `SELECT format('DROP INDEX IF EXISTS %I;', c.relname)
FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
WHERE i.indrelid = 'kubernetes_evidence_events'::regclass
  AND c.relname IN ('evidence_partition_event_time','evidence_partition_event_rule','evidence_partition_event_priority','evidence_partition_event_uid','evidence_partition_event_pod','evidence_partition_current_type','evidence_partition_current_namespace','evidence_partition_current_root','evidence_partition_current_break','kubernetes_partition_termination_markers','kubernetes_history_termination_markers','kubernetes_evidence_current_type','kubernetes_evidence_current_namespace','kubernetes_evidence_current_root','kubernetes_evidence_current_break')
ORDER BY 1;`

// runGuideIndexDrop runs the migration guide's step as an operator does: the
// query writes the statements, and each runs after the guide's lock timeout.
// It returns the statements it ran.
func runGuideIndexDrop(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), guideIndexDropQuery)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{}
	for rows.Next() {
		var statement string
		if err = rows.Scan(&statement); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		statements = append(statements, statement)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(t.Context(), `SET lock_timeout = '2s'`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err = conn.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err = conn.ExecContext(t.Context(), `RESET lock_timeout`); err != nil {
		t.Fatal(err)
	}
	return statements
}

// indexDefinitions lists the definitions of a relation's indexes, sorted.
func indexDefinitions(t *testing.T, db *sql.DB, relation string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT split_part(pg_get_indexdef(indexrelid),'USING btree ',2) FROM pg_index WHERE indrelid=to_regclass($1)`, relation)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	definitions := []string{}
	for rows.Next() {
		var definition string
		if err = rows.Scan(&definition); err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, definition)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(definitions)
	return definitions
}

// The migration guide's step removes the unused indexes from the journal and
// every existing month, a month created afterwards gets only the kept ones,
// and running the step again finds nothing to do.
func TestPostgresGuideIndexDropLeavesTheKeptIndexes(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	if _, err := p.db.ExecContext(t.Context(), `SELECT versus_ensure_evidence_partition(to_regclass('kubernetes_evidence_events'),now())`); err != nil {
		t.Fatal(err)
	}
	current := "evidence_event_month_" + time.Now().UTC().Format("200601")
	want := append([]string(nil), keptEventIndexes...)
	sort.Strings(want)
	if got := len(indexDefinitions(t, p.db, current)); got != len(want)+len(guideDroppedIndexes) {
		t.Fatalf("month %s has %d indexes before the guide's step, want %d", current, got, len(want)+len(guideDroppedIndexes))
	}
	ran := runGuideIndexDrop(t, p.db)
	wantRan := make([]string, len(guideDroppedIndexes))
	for i, name := range guideDroppedIndexes {
		wantRan[i] = "DROP INDEX IF EXISTS " + name + ";"
	}
	if strings.Join(ran, "\n") != strings.Join(wantRan, "\n") {
		t.Fatalf("the guide's query wrote:\n%s\nwant:\n%s", strings.Join(ran, "\n"), strings.Join(wantRan, "\n"))
	}
	for _, relation := range []string{"kubernetes_evidence_events", current} {
		if got := indexDefinitions(t, p.db, relation); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s indexes:\n%s\nwant:\n%s", relation, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	next := time.Now().UTC().AddDate(0, 2, 0)
	if _, err := p.db.ExecContext(t.Context(), `SELECT versus_ensure_evidence_partition(to_regclass('kubernetes_evidence_events'),$1)`, next); err != nil {
		t.Fatal(err)
	}
	month := "evidence_event_month_" + next.Format("200601")
	if got := indexDefinitions(t, p.db, month); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("new month %s indexes:\n%s\nwant:\n%s", month, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if again := runGuideIndexDrop(t, p.db); len(again) != 0 {
		t.Fatalf("running the guide's step again wrote %v, want nothing", again)
	}
}

// seedEventMonth fills the current month of the journal with a cluster's
// mix of records across four scopes: mostly crash statuses, then changes,
// issue lifecycle records and a few coverage gaps, over many namespaces,
// roots and pods.
func seedEventMonth(t *testing.T, p *postgresProvider, rows int) (string, time.Time) {
	t.Helper()
	start := time.Now().UTC()
	month := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	keys := []string{}
	for i := range 4 {
		scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("index-audit-%d", i), ClusterUID: "physical"}
		key, err := evidenceScopeKey(scope)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
		if _, err = p.db.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid,read_model_ready,heads_backfill,observed_lag_scan) VALUES($1,$2,$3,$4,TRUE,NULL,NULL) ON CONFLICT DO NOTHING`, key, "default", scope.ClusterID, scope.ClusterUID); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := p.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), evidenceHeadsWriter); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), `SELECT versus_ensure_evidence_partition(to_regclass('kubernetes_evidence_events'),$1)`, month); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_events(scope_key,event_id,event_version,priority,event_type,is_break,event_at,observed_at,gap_from,gap_to,namespace,resource_name,kind,authorization_kind,resource_uid,root_uid,pod_uid,rule,epoch,payload,superseded_seq)
 SELECT ($1::text[])[1+i%4], 'event-'||(i%40000), 'v'||i, 0, t.type,
 t.type LIKE 'issue_%' AND i%2=0,
 $2::timestamptz+make_interval(secs=>(i%2400000)), $2::timestamptz+make_interval(secs=>(i%2400000)+(i%600)),
 CASE WHEN t.type='coverage_gap' THEN $2::timestamptz+make_interval(secs=>(i%2400000)-600) END,
 CASE WHEN t.type='coverage_gap' THEN $2::timestamptz+make_interval(secs=>(i%2400000)) END,
 'ns-'||(i%20), 'object-'||(i%3000), (ARRAY['Deployment','StatefulSet','DaemonSet','Job','Pod'])[1+i%5], 'Pod',
 'resource-'||(i%5000), 'root-'||(i%2000), 'pod-'||(i%6000),
 CASE WHEN t.type='change' THEN (ARRAY['created','updated','deleted','image'])[1+i%4] WHEN t.type LIKE 'issue_%' THEN (ARRAY['pod.crashloop','pod.oom','node.pressure'])[1+i%3] ELSE '' END,
 'stable',
 CASE WHEN t.type='crash' THEN jsonb_build_object('type',CASE WHEN i%10=0 THEN 'termination' ELSE 'status' END,'observed_at',now()) ELSE jsonb_build_object('type','x') END,
 CASE WHEN i%5<2 THEN i+1 END
 FROM generate_series(1,$3::int) i CROSS JOIN LATERAL (SELECT CASE WHEN i%1000=0 THEN 'coverage_gap' WHEN i%100<70 THEN 'crash' WHEN i%100<95 THEN 'change' WHEN i%100<97 THEN 'issue_opened' WHEN i%100<99 THEN 'issue_observed' ELSE 'issue_resolved' END AS type) t`, keys, month, rows); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = p.db.ExecContext(t.Context(), `ANALYZE kubernetes_evidence_events`); err != nil {
		t.Fatal(err)
	}
	return keys[0], month
}

// sqlLiteral writes a statement argument as a SQL literal, for EXECUTE.
func sqlLiteral(t *testing.T, value any) string {
	t.Helper()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	switch v := value.(type) {
	case string:
		return quote(v)
	case time.Time:
		return quote(v.UTC().Format(time.RFC3339Nano)) + "::timestamptz"
	case int64:
		return fmt.Sprint(v)
	case uint64:
		return fmt.Sprint(v)
	case int:
		return fmt.Sprint(v)
	case bool:
		return fmt.Sprint(v)
	case []string:
		quoted := make([]string, len(v))
		for i, item := range v {
			quoted[i] = quote(item)
		}
		return "ARRAY[" + strings.Join(quoted, ",") + "]::text[]"
	case []byte:
		return quote(string(v)) + "::jsonb"
	}
	t.Fatalf("no literal for %T", value)
	return ""
}

// journalPlan is how the database reads the month for one statement.
type journalPlan struct {
	indexes  []string
	seqScans int
	cost     float64
}

// explainGeneric plans a statement as a pooled connection keeps it: a generic
// plan, without sorts where an index order exists.
func explainGeneric(t *testing.T, db *sql.DB, settings, statement string, args []any) journalPlan {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	// A prepared statement belongs to the connection, not the transaction.
	defer func() { _, _ = tx.ExecContext(context.Background(), "DEALLOCATE audit") }()
	if _, err = tx.ExecContext(t.Context(), "PREPARE audit AS "+statement); err != nil {
		t.Fatalf("prepare: %v\n%s", err, statement)
	}
	literals := make([]string, len(args))
	for i, arg := range args {
		literals[i] = sqlLiteral(t, arg)
	}
	execute := "EXECUTE audit"
	if len(literals) > 0 {
		execute += "(" + strings.Join(literals, ",") + ")"
	}
	var raw []byte
	if err = tx.QueryRowContext(t.Context(), "EXPLAIN (FORMAT JSON) "+execute).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err = json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	plan := journalPlan{cost: plans[0].Plan["Total Cost"].(float64)}
	seen := map[string]bool{}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if name, ok := node["Index Name"].(string); ok && !seen[name] {
			seen[name] = true
			plan.indexes = append(plan.indexes, name)
		}
		if node["Node Type"] == "Seq Scan" && strings.HasPrefix(fmt.Sprint(node["Relation Name"]), "evidence_event_month_") {
			plan.seqScans++
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			walk(child.(map[string]any))
		}
	}
	walk(plans[0].Plan)
	sort.Strings(plan.indexes)
	return plan
}

// journalReadShape is one way the history pages read the journal.
type journalReadShape struct {
	name    string
	query   EvidenceQuery
	visible string
}

func journalReadShapes(since, until time.Time) []journalReadShape {
	q := func(change func(*EvidenceQuery)) EvidenceQuery {
		query := EvidenceQuery{Since: since, Until: until, GapsApart: true}
		change(&query)
		return query
	}
	return []journalReadShape{
		{name: "timeline", query: q(func(q *EvidenceQuery) { q.Type = "timeline" })},
		{name: "timeline namespace", query: q(func(q *EvidenceQuery) { q.Type, q.Namespace = "timeline", "ns-3" })},
		{name: "timeline namespace kind root", query: q(func(q *EvidenceQuery) {
			q.Type, q.Namespace, q.Kind, q.RootUID = "timeline", "ns-3", "Deployment", "root-23"
		})},
		{name: "timeline resource", query: q(func(q *EvidenceQuery) { q.Type, q.ResourceUID = "timeline", "resource-23" })},
		{name: "timeline pod", query: q(func(q *EvidenceQuery) { q.Type, q.PodUID = "timeline", "pod-23" })},
		{name: "breaks", query: q(func(q *EvidenceQuery) { q.Type = "break" })},
		{name: "breaks namespace", query: q(func(q *EvidenceQuery) { q.Type, q.Namespace = "break", "ns-3" })},
		{name: "changes", query: q(func(q *EvidenceQuery) { q.Type = "change" })},
		{name: "changes namespace kind", query: q(func(q *EvidenceQuery) { q.Type, q.Namespace, q.Kind = "change", "ns-3", "Deployment" })},
		{name: "changes object", query: q(func(q *EvidenceQuery) {
			q.Type, q.Namespace, q.Kind, q.Name = "change", "ns-3", "Deployment", "object-23"
		})},
		{name: "changes of a type", query: q(func(q *EvidenceQuery) { q.Type, q.Rule = "change", "deleted" })},
		{name: "changes root", query: q(func(q *EvidenceQuery) { q.Type, q.RootUID = "change", "root-23" })},
		{name: "crashes", query: q(func(q *EvidenceQuery) { q.Type, q.ByObservation = "crash", true })},
		{name: "crashes namespace", query: q(func(q *EvidenceQuery) { q.Type, q.ByObservation, q.Namespace = "crash", true, "ns-3" })},
		{name: "crashes root", query: q(func(q *EvidenceQuery) { q.Type, q.ByObservation, q.RootUID = "crash", true, "root-23" })},
		{name: "crashes pod", query: q(func(q *EvidenceQuery) { q.Type, q.ByObservation, q.PodUID = "crash", true, "pod-23" })},
		{name: "issues filtered", query: q(func(q *EvidenceQuery) {
			q.Type, q.Namespace, q.Kind, q.RootUID, q.Rule = "issue", "ns-3", "Deployment", "root-23", "pod.crashloop"
		})},
		{name: "gaps of a range", query: q(func(q *EvidenceQuery) { q.WithGaps, q.GapsApart = true, false }), visible: "FALSE"},
	}
}

// auditJournalReads plans every history read shape over the month, and the
// point reads saves and walks make.
func auditJournalReads(t *testing.T, p *postgresProvider, key string, month time.Time) map[string]journalPlan {
	t.Helper()
	plans := map[string]journalPlan{}
	leaf := "evidence_event_month_" + month.Format("200601")
	until := month.AddDate(0, 0, 25)
	for _, shape := range journalReadShapes(until.Add(-7*24*time.Hour), until) {
		args := historySQLArgs{}
		statement := journalCandidates([]string{key}, shape.query, evidenceCursor{Highs: []uint64{1 << 40}}, 51, false, shape.visible, journalShape{windowFrom: month, windowTo: month.AddDate(0, 1, 0), leaves: []string{leaf}}, &args)
		plans[shape.name] = explainGeneric(t, p.db, journalWalkSettings, statement, []any(args))
	}
	point := []struct {
		name, statement string
		args            []any
	}{
		{"shadowed versions", `SELECT DISTINCT event_id FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id=ANY($2::text[]) AND sequence<=$3`, []any{key, []string{"event-1", "event-2"}, int64(1 << 40)}},
		{"bodies", `SELECT sequence,payload FROM ` + leaf + ` e WHERE scope_key=ANY($1::text[]) AND sequence=ANY($2::bigint[]) AND event_at=ANY($3::timestamptz[])`, []any{[]string{key}, "{1,2}", "{" + month.Format(time.RFC3339) + "}"}},
		{"newest version before heads", `SELECT p.sequence FROM kubernetes_evidence_events p WHERE p.scope_key=$1 AND p.event_id=$2 ORDER BY p.sequence DESC LIMIT 1`, []any{key, "event-1"}},
		{"journal by sequence", `SELECT sequence,event_id FROM kubernetes_evidence_events WHERE scope_key=$1 AND sequence>$2 ORDER BY sequence LIMIT $3`, []any{key, int64(0), int64(512)}},
		{"observation delay of a month", `SELECT MAX(observed_at-event_at)::text FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_type IN('crash','coverage_gap') AND event_at>=$2 AND event_at<$3`, []any{key, month, month.AddDate(0, 1, 0)}},
	}
	for _, read := range point {
		plans[read.name] = explainGeneric(t, p.db, `SET LOCAL plan_cache_mode=force_generic_plan`, read.statement, read.args)
	}
	return plans
}

// movesOffRetiredIndex are the reads that used an index the guide removes: changes of
// one type read the type index instead of the rule index, at a slightly higher
// estimate, ordered the same way; and the reads of one event's versions move
// from the priority index to the event's unique key, which leads with the
// same columns.
var movesOffRetiredIndex = map[string]bool{"changes of a type": true, "newest version before heads": true, "shadowed versions": true}

// Every way the history pages and saves read the journal is planned over a
// seeded month before and after the migration guide's index step. After it
// every read still reads the month through an index, and through the same
// one, except the reads that used a removed index.
func TestPostgresJournalReadsUseKeptIndexes(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	key, month := seedEventMonth(t, p, 120000)
	before := auditJournalReads(t, p, key, month)
	if ran := runGuideIndexDrop(t, p.db); len(ran) != len(guideDroppedIndexes) {
		t.Fatalf("the guide's step dropped %v, want the %d unused indexes", ran, len(guideDroppedIndexes))
	}
	after := auditJournalReads(t, p, key, month)
	reads := make([]string, 0, len(after))
	for read := range after {
		reads = append(reads, read)
	}
	sort.Strings(reads)
	for _, read := range reads {
		b, a := before[read], after[read]
		t.Logf("%-30s before cost %10.1f %v | after cost %10.1f %v", read, b.cost, b.indexes, a.cost, a.indexes)
		if a.seqScans > 0 || len(a.indexes) == 0 {
			t.Errorf("%s reads the month without an index: %+v", read, a)
		}
		if strings.Join(a.indexes, ",") != strings.Join(b.indexes, ",") && !movesOffRetiredIndex[read] {
			t.Errorf("%s reads through %v after the drop, %v before; only a read listed as moving off a retired index may change", read, a.indexes, b.indexes)
		}
	}
}
