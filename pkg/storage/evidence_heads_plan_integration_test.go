package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// headsPartitionedProvider is headsTestProvider on the partitioned journal
// that long-running deployments use: the partition copy and swap are driven
// to completion before the test writes anything.
func headsPartitionedProvider(t *testing.T) (*postgresProvider, EvidenceScope) {
	t.Helper()
	p, scope := headsTestProvider(t)
	headsConvert(t, p)
	return p, scope
}

// headsConvert runs the background passes until the journal has been copied
// into monthly partitions and swapped in.
func headsConvert(t *testing.T, p *postgresProvider) {
	t.Helper()
	for i := 0; i < 4096; i++ {
		more, err := p.backfillEvidenceBatch(t.Context(), evidenceBackgroundBackfill)
		if err != nil {
			t.Fatal(err)
		}
		copyMore, err := p.partitionEvidenceBatch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more && !copyMore {
			break
		}
	}
	var kind string
	if err := p.db.QueryRow(`SELECT relkind::text FROM pg_class WHERE oid='kubernetes_evidence_events'::regclass`).Scan(&kind); err != nil || kind != "p" {
		t.Fatalf("events table not partitioned: %q %v", kind, err)
	}
}

// headsPlanNode is one plan node with the CTE it belongs to.
type headsPlanNode struct {
	CTE, NodeType, Operation, Relation, Index string
	Loops, Rows, Removed                      float64
}

func headsPlanNodes(raw []byte) ([]headsPlanNode, error) {
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded) == 0 {
		return nil, fmt.Errorf("decode plan: %v", err)
	}
	var nodes []headsPlanNode
	var walk func(node map[string]any, cte string)
	walk = func(node map[string]any, cte string) {
		if name, _ := node["Subplan Name"].(string); strings.HasPrefix(name, "CTE ") {
			cte = strings.TrimPrefix(name, "CTE ")
		}
		n := headsPlanNode{CTE: cte}
		n.NodeType, _ = node["Node Type"].(string)
		n.Operation, _ = node["Operation"].(string)
		n.Relation, _ = node["Relation Name"].(string)
		n.Index, _ = node["Index Name"].(string)
		n.Loops, _ = node["Actual Loops"].(float64)
		n.Rows, _ = node["Actual Rows"].(float64)
		removed, _ := node["Rows Removed by Filter"].(float64)
		recheck, _ := node["Rows Removed by Index Recheck"].(float64)
		n.Removed = removed + recheck
		nodes = append(nodes, n)
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if nested, ok := child.(map[string]any); ok {
				walk(nested, cte)
			}
		}
	}
	root, _ := decoded[0]["Plan"].(map[string]any)
	walk(root, "")
	return nodes, nil
}

func headsJournalRelation(name string) bool {
	return strings.HasPrefix(name, "kubernetes_evidence_events") || strings.HasPrefix(name, "evidence_event_month_")
}

// headsPlanVerdict lists every way a write plan depends on event history.
// Head lookups must be primary-key probes; a supersede must touch one
// partition row by its full key; nothing may scan an event's history.
func headsPlanVerdict(nodes []headsPlanNode, superseded int) []string {
	var problems []string
	partitionProbes := 0.0
	for _, n := range nodes {
		executed := n.Loops > 0
		if n.Relation == "kubernetes_evidence_event_heads" && strings.Contains(n.NodeType, "Scan") {
			if !strings.HasPrefix(n.NodeType, "Index") || n.Index != "kubernetes_evidence_event_heads_pkey" {
				problems = append(problems, fmt.Sprintf("head lookup in %s is %s on %q", n.CTE, n.NodeType, n.Index))
			}
		}
		if !headsJournalRelation(n.Relation) || !strings.Contains(n.NodeType, "Scan") {
			continue
		}
		if strings.Contains(n.NodeType, "Seq Scan") && executed {
			problems = append(problems, fmt.Sprintf("sequential scan of %s in %s", n.Relation, n.CTE))
		}
		cte := strings.TrimRight(strings.TrimRight(n.CTE, "0123456789"), "_")
		switch cte {
		case "superseded", "refreshed":
			if !executed {
				continue
			}
			// The row is found by its sequence: the partition key, or the
			// scope's sequence index, one probe per row either way.
			if !strings.HasSuffix(n.Index, "_pkey") && !strings.HasSuffix(n.Index, "_scope_key_sequence_idx") {
				problems = append(problems, fmt.Sprintf("%s reads %s through %s, not by the row's sequence", n.CTE, n.Relation, n.Index))
			}
			if n.Rows > n.Loops || n.Removed > 0 {
				problems = append(problems, fmt.Sprintf("%s read %v rows (%v filtered) from %s in %v probes", n.CTE, n.Rows, n.Removed, n.Relation, n.Loops))
			}
			if cte == "superseded" {
				partitionProbes += n.Loops
			}
		default:
			if executed && n.CTE != "inserted" {
				problems = append(problems, fmt.Sprintf("%s scans journal %s via %s", n.CTE, n.Relation, n.Index))
			}
		}
	}
	if partitionProbes > float64(superseded) {
		problems = append(problems, fmt.Sprintf("supersede probed %v partitions for %d rows", partitionProbes, superseded))
	}
	return problems
}

// headsNewEventProblems lists the ways a save of events that have no head yet
// reads history beyond the event's own key. Such an event must be looked up
// through an index that leads with its key: an index of the scope's current
// rows answers the lookup by reading them all, and its event condition is
// applied inside the index, so the rows read are not counted as filtered.
func headsNewEventProblems(nodes []headsPlanNode) []string {
	var problems []string
	for _, n := range nodes {
		if n.Relation == "kubernetes_evidence_event_heads" && strings.Contains(n.NodeType, "Scan") && (!strings.HasPrefix(n.NodeType, "Index") || n.Index != "kubernetes_evidence_event_heads_pkey") {
			problems = append(problems, fmt.Sprintf("head lookup in %s is %s on %q", n.CTE, n.NodeType, n.Index))
		}
		if strings.HasPrefix(n.CTE, "superseded") || strings.HasPrefix(n.CTE, "refreshed") || !headsJournalRelation(n.Relation) || !strings.Contains(n.NodeType, "Scan") || n.Loops == 0 {
			continue
		}
		if strings.Contains(n.NodeType, "Seq Scan") || n.Removed > n.Loops || !strings.Contains(n.Index, "scope_key_event_id") {
			problems = append(problems, fmt.Sprintf("%s on %s via %q: %v lookups not keyed by the event", n.NodeType, n.Relation, n.Index, n.Loops))
		}
	}
	return problems
}

type headsPlanResult struct {
	Stored, HistoryDepth int
	Case, Mode           string
	Inserted, Superseded int
	Problems             []string
	Strict               bool
	Nodes                []headsPlanNode
	Plan                 json.RawMessage
}

type headsTiming struct {
	Stored            int
	ChangedSavesMS    []float64
	UnchangedSavesMS  []float64
	ChangedMedianMS   float64
	UnchangedMedianMS float64
}

// headsMonthPartitions lists the monthly partitions of the evidence journal
// and observation tables that exist now.
func headsMonthPartitions(t *testing.T, dsn string) map[string]bool {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(t.Context(), `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
 WHERE i.inhparent IN(to_regclass('kubernetes_evidence_events'),to_regclass('kubernetes_evidence_occurrence_observations'))`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, name := range names {
		found[name] = true
	}
	return found
}

// headsDropNewPartitions drops every empty monthly partition created since
// existing was listed, so seeded months long past retention never outlive
// the test. On a shared database another test may have written into a
// partition this test created (the current month); that one is kept.
func headsDropNewPartitions(t *testing.T, dsn string, existing map[string]bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Errorf("drop seeded partitions: %v", err)
		return
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
 WHERE i.inhparent IN(to_regclass('kubernetes_evidence_events'),to_regclass('kubernetes_evidence_occurrence_observations'))`)
	if err != nil {
		t.Errorf("drop seeded partitions: %v", err)
		return
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Errorf("drop seeded partitions: %v", err)
		return
	}
	for _, name := range names {
		if existing[name] {
			continue
		}
		table := pgx.Identifier{name}.Sanitize()
		var occupied bool
		if err = conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+")").Scan(&occupied); err != nil {
			t.Errorf("inspect partition %s: %v", name, err)
			continue
		}
		if occupied {
			t.Logf("partition %s holds other tests' rows; kept", name)
			continue
		}
		if _, err = conn.Exec(ctx, "DROP TABLE "+table); err != nil {
			t.Errorf("drop partition %s: %v", name, err)
		}
	}
}

// headsVersionBytes is the observed database growth per stored version on
// the partitioned journal with all of its indexes (10⁷ took about 46 GB).
const headsVersionBytes = 5000

// headsVolumeGuard stops seeding before the database outgrows its budget.
// SQL cannot see the free space of the server's data volume, and a Docker
// Desktop container reports its sparse VM disk, not the host's real free
// space. So the large case is opt-in and bounded by a database size budget,
// EVIDENCE_SCALE_HEADS_PLAN_MAX_BYTES (default 60 GiB), that the operator
// sizes to the volume.
func headsVolumeGuard(t *testing.T, p *postgresProvider, next int64) {
	t.Helper()
	budget := int64(60 << 30)
	if raw := os.Getenv("EVIDENCE_SCALE_HEADS_PLAN_MAX_BYTES"); raw != "" {
		if _, err := fmt.Sscan(raw, &budget); err != nil || budget <= 0 {
			t.Fatalf("invalid EVIDENCE_SCALE_HEADS_PLAN_MAX_BYTES %q", raw)
		}
	}
	var size int64
	if err := p.db.QueryRowContext(t.Context(), `SELECT pg_database_size(current_database())`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size+next*headsVersionBytes > budget {
		t.Fatalf("database size safety stop: %d bytes used, next chunk needs about %d, budget %d", size, next*headsVersionBytes, budget)
	}
}

// headsSeed stores exactly stored versions in a fresh scope, set-based. A
// fixed set of batch events carries deep history over six monthly
// partitions, later versions later in time; the remainder are other current
// events. Every current version has its head, as after one post-upgrade save.
func headsSeed(t *testing.T, p *postgresProvider, key string, at time.Time, stored, batch int, current []string) int {
	t.Helper()
	headsVolumeGuard(t, p, int64(stored))
	t.Cleanup(func() {
		// The scope owns every seeded row; deleting it cascades to the
		// journal, heads and captures.
		if _, err := p.db.Exec(`DELETE FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key); err != nil {
			t.Errorf("clean up seeded scope: %v", err)
			return
		}
		var left int
		if err := p.db.QueryRow(`SELECT (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1)+(SELECT COUNT(*) FROM kubernetes_evidence_event_heads WHERE scope_key=$1)+(SELECT COUNT(*) FROM kubernetes_evidence_captures WHERE scope_key=$1)`, key).Scan(&left); err != nil || left != 0 {
			t.Errorf("seeded scope left %d rows behind (%v)", left, err)
		}
	})
	depth := max(1, stored/(10*batch))
	origin := at.Add(-180 * 24 * time.Hour)
	for month := origin; !month.After(at.AddDate(0, 1, 0)); month = month.AddDate(0, 1, 0) {
		if _, err := p.db.ExecContext(t.Context(), `SELECT versus_ensure_evidence_partition('kubernetes_evidence_events',$1)`, month); err != nil {
			t.Fatal(err)
		}
	}
	var base int64
	if err := p.db.QueryRowContext(t.Context(), `SELECT nextval('kubernetes_evidence_events_sequence_seq')`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecContext(t.Context(), `SELECT setval('kubernetes_evidence_events_sequence_seq',$1)`, base+int64(stored)); err != nil {
		t.Fatal(err)
	}
	const chunk = 500000
	for start := 0; start < stored; start += chunk {
		headsVolumeGuard(t, p, int64(min(stored, start+chunk)-start))
		end := min(stored, start+chunk) - 1
		if _, err := headsWriterExec(t.Context(), p.db, `INSERT INTO kubernetes_evidence_events
 (sequence,scope_key,event_id,event_version,event_type,event_at,observed_at,namespace,resource_name,kind,authorization_kind,resource_uid,root_uid,epoch,payload,priority,superseded_seq)
 SELECT k.base+n,k.scope,
 CASE WHEN n<k.batch*k.depth THEN 'change:batch-'||(n/k.depth) ELSE 'change:other-'||n END,
 CASE WHEN n<k.batch*k.depth AND n%k.depth=k.depth-1 THEN k.current[1+n/k.depth] WHEN n<k.batch*k.depth THEN 'history-'||(n%k.depth) ELSE 'other' END,
 'change',ts,ts,'shop','seeded','Deployment','Deployment','uid-'||n,'root-'||(n%1000),'stable',
 jsonb_build_object('value',n),0,
 CASE WHEN n<k.batch*k.depth AND n%k.depth<>k.depth-1 THEN k.base+n+1 END
 FROM (SELECT $1::text AS scope,$2::bigint AS base,$3::timestamptz AS origin,$4::bigint AS batch,$5::bigint AS depth,$8::text[] AS current,$9::double precision AS stored)k
 CROSS JOIN generate_series($6::bigint,$7::bigint)n
 CROSS JOIN LATERAL(SELECT k.origin+INTERVAL '180 days'*CASE WHEN n<k.batch*k.depth
  THEN ((n%k.depth)+(n/k.depth)::double precision/k.batch)/k.depth ELSE n/k.stored END AS ts)t`,
			key, base, origin, batch, depth, start, end, current, float64(stored)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := headsWriterExec(t.Context(), p.db, `INSERT INTO kubernetes_evidence_event_heads(scope_key,event_id,sequence,event_at,event_version,priority,observed_at)
 SELECT scope_key,event_id,sequence,event_at,event_version,priority,observed_at FROM kubernetes_evidence_events WHERE scope_key=$1 AND superseded_seq IS NULL`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ExecContext(t.Context(), `UPDATE kubernetes_evidence_scopes SET high_sequence=$2 WHERE scope_key=$1`, key, base+int64(stored)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"kubernetes_evidence_events", "kubernetes_evidence_event_heads"} {
		if _, err := p.db.ExecContext(t.Context(), "ANALYZE "+table); err != nil {
			t.Fatal(err)
		}
	}
	var rows, heads int
	if err := p.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1),(SELECT COUNT(*) FROM kubernetes_evidence_event_heads WHERE scope_key=$1)`, key).Scan(&rows, &heads); err != nil {
		t.Fatal(err)
	}
	if rows != stored || heads != stored-batch*(depth-1) {
		t.Fatalf("seed: rows=%d heads=%d, want %d and %d", rows, heads, stored, stored-batch*(depth-1))
	}
	return depth
}

// headsExplain runs the write statement under EXPLAIN ANALYZE in a
// transaction that is rolled back. Mode custom is the planner's own plan for
// the bound values, generic is the plan a long-lived cached statement may
// switch to, and index forbids sequential scans and hash or merge joins to
// prove the per-row key path exists where the planner prefers to read a
// tiny table whole.
func headsExplain(t *testing.T, dsn string, statement scaleStatement, mode string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	types := "text,jsonb,double precision"
	if len(statement.Args) > 3 {
		// A save of a complete scope bounds its updates by its current rows' times.
		types += ",timestamptz[]"
	}
	if _, err = tx.Exec(ctx, "PREPARE heads_write("+types+") AS "+statement.SQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	settings := map[string][]string{
		"custom":  {"SET LOCAL plan_cache_mode=force_custom_plan"},
		"generic": {"SET LOCAL plan_cache_mode=force_generic_plan"},
		"index":   {"SET LOCAL plan_cache_mode=force_custom_plan", "SET LOCAL enable_seqscan=off", "SET LOCAL enable_hashjoin=off", "SET LOCAL enable_mergejoin=off"},
	}[mode]
	if len(settings) == 0 {
		t.Fatalf("unknown plan mode %q", mode)
	}
	for _, setting := range settings {
		if _, err = tx.Exec(ctx, setting, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatal(err)
		}
	}
	args := []any{pgx.QueryExecModeSimpleProtocol}
	for _, value := range statement.Args {
		if raw, ok := value.([]byte); ok {
			value = string(raw)
		}
		args = append(args, value)
	}
	var plan []byte
	parameters := "$1,$2,$3"
	if len(statement.Args) > 3 {
		parameters += ",$4"
	}
	if err = tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE heads_write("+parameters+")", args...).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func headsMedian(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

// TestPostgresEvidenceSavePlanStaysFlatAsHistoryGrows proves a save compares
// with and supersedes only current versions: at 10³, 10⁵ and 10⁷ stored
// versions the write statement probes heads by primary key, supersedes each
// old row in one partition by its full key and never reads an event's
// history, and a fixed batch saves in under twice the time at 10⁷ as at 10³.
// The 10⁷ case seeds about 46 GB and runs only with
// EVIDENCE_SCALE_HEADS_PLAN=1 outside short mode.
func TestPostgresEvidenceSavePlanStaysFlatAsHistoryGrows(t *testing.T) {
	sizes := []int{1000, 100000, 10000000}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated Postgres required")
	}
	existing := headsMonthPartitions(t, dsn)
	p, _ := headsPartitionedProvider(t)
	// Registered after the provider, so it runs before the provider closes
	// and after every size has deleted its seeded scope.
	t.Cleanup(func() { headsDropNewPartitions(t, dsn, existing) })
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	trace := &scaleQueryTrace{}
	cfg.Tracer = trace
	traced := stdlib.OpenDB(*cfg)
	defer traced.Close()
	const batchSize, reps = 1000, 7
	var plans []headsPlanResult
	var timings []headsTiming
	results := os.Getenv("HARNESS_RESULTS_DIR")
	persist := func() {
		if results != "" {
			scaleWriteJSON(t, filepath.Join(results, "heads-plan-shape.json"), plans)
			scaleWriteJSON(t, filepath.Join(results, "heads-save-timings.json"), timings)
		}
	}
	defer persist()
	for _, stored := range sizes {
		t.Run(fmt.Sprintf("stored=%d", stored), func(t *testing.T) {
			if stored >= 10000000 && (testing.Short() || os.Getenv("EVIDENCE_SCALE_HEADS_PLAN") != "1") {
				t.Skip("10⁷ stored versions seed about 46 GB; set EVIDENCE_SCALE_HEADS_PLAN=1 outside short mode in an isolated harness database")
			}
			at := time.Now().UTC().Truncate(time.Microsecond)
			scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("heads-plan-%d-%d", stored, at.UnixNano()), ClusterUID: "physical"}
			key, _ := evidenceScopeKey(scope)
			headsCommit(t, p, scope, "create", at.Add(-181*24*time.Hour))
			batch := func(value int, when time.Time) []EvidenceRecord {
				records := make([]EvidenceRecord, batchSize)
				for i := range records {
					records[i] = headsChange(fmt.Sprintf("change:batch-%d", i), fmt.Sprint(value), when)
				}
				return records
			}
			current := make([]string, batchSize)
			for i, record := range batch(0, at) {
				current[i] = evidenceVersion(record)
			}
			seedStarted := time.Now()
			depth := headsSeed(t, p, key, at, stored, batchSize, current)
			t.Logf("SEED stored=%d history_depth=%d batch=%d seconds=%.1f", stored, depth, batchSize, time.Since(seedStarted).Seconds())

			capture := func(records []EvidenceRecord) scaleStatement {
				t.Helper()
				trace.mu.Lock()
				trace.queries = nil
				trace.mu.Unlock()
				tx, err := traced.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				trace.enabled.Store(true)
				_, err = writeEvidenceEvents(t.Context(), tx, key, records, 5*time.Minute)
				trace.enabled.Store(false)
				if err != nil {
					t.Fatal(err)
				}
				for _, statement := range trace.queries {
					if strings.Contains(statement.SQL, "kubernetes_evidence_event_heads") && strings.Contains(statement.SQL, "WITH incoming") {
						return statement
					}
				}
				t.Fatal("write statement not captured")
				return scaleStatement{}
			}
			later := at.Add(time.Minute)
			for _, c := range []struct {
				name       string
				records    []EvidenceRecord
				inserted   int
				superseded int
			}{{"unchanged", batch(0, later), 0, 0}, {"changed", batch(1, later), batchSize, batchSize}} {
				statement := capture(c.records)
				for _, mode := range []string{"custom", "generic", "index"} {
					raw := headsExplain(t, dsn, statement, mode)
					nodes, err := headsPlanNodes(raw)
					if err != nil {
						t.Fatal(err)
					}
					result := headsPlanResult{Stored: stored, HistoryDepth: depth, Case: c.name, Mode: mode, Inserted: c.inserted, Superseded: c.superseded, Nodes: nodes, Plan: raw}
					result.Problems = headsPlanVerdict(nodes, c.superseded)
					for _, n := range nodes {
						if n.Relation != "" && n.Loops > 0 {
							t.Logf("PLAN stored=%d case=%s mode=%s cte=%s node=%q relation=%s index=%s loops=%v rows=%v filtered=%v", stored, c.name, mode, n.CTE, n.NodeType, n.Relation, n.Index, n.Loops, n.Rows, n.Removed)
						}
					}
					// At 10³ the whole scope is a few pages, and the planner rightly
					// reads it whole; that is recorded. With sequential scans off
					// the head lookups must still be key probes and nothing may
					// scan a table, but a save names the month partitions it
					// updates, and one month of a tiny scope is a few dozen rows:
					// reading it once instead of probing it per row is the
					// planner's cheaper choice there, and the per-row path is held
					// at 10⁵ and above.
					// Saves plan every statement for its values, so the generic
					// plan is recorded but never used: it is not held to the rules.
					strict := stored >= 100000 && mode != "generic"
					for _, problem := range result.Problems {
						keyed := mode == "index" && (strings.HasPrefix(problem, "head lookup") || strings.HasPrefix(problem, "sequential scan"))
						if strict || keyed {
							t.Errorf("PLAN stored=%d case=%s mode=%s: %s", stored, c.name, mode, problem)
						} else {
							t.Logf("PLAN-SMALL stored=%d case=%s mode=%s (planner choice on a tiny table): %s", stored, c.name, mode, problem)
						}
					}
					result.Strict = strict || mode == "index"
					plans = append(plans, result)
				}
			}
			// Brand-new events have no head, so each is looked up once in the
			// journal. The scope's newest rows sit in a month partition that was
			// just created and never analyzed, as in a new month or a fresh
			// database, where a generic plan once read them all for every event.
			fresh := at.AddDate(0, -3, 0)
			freshBatch := func(prefix string, count int) []EvidenceRecord {
				records := make([]EvidenceRecord, count)
				for i := range records {
					records[i] = headsChange(fmt.Sprintf("change:%s-%d", prefix, i), "new", fresh)
				}
				return records
			}
			headsCommit(t, p, scope, "fresh-month", later, freshBatch("fresh", batchSize)...)
			statement := capture(freshBatch("brand-new", batchSize))
			for _, mode := range []string{"custom", "generic", "index"} {
				raw := headsExplain(t, dsn, statement, mode)
				nodes, err := headsPlanNodes(raw)
				if err != nil {
					t.Fatal(err)
				}
				result := headsPlanResult{Stored: stored, HistoryDepth: depth, Case: "new events, unanalyzed month", Mode: mode, Inserted: batchSize, Nodes: nodes, Plan: raw}
				result.Problems = headsNewEventProblems(nodes)
				result.Strict = (stored >= 100000 && mode != "generic") || mode == "index"
				for _, problem := range result.Problems {
					if result.Strict {
						t.Errorf("PLAN stored=%d case=new mode=%s: %s", stored, mode, problem)
					} else {
						t.Logf("PLAN-SMALL stored=%d case=new mode=%s (planner choice on a tiny table): %s", stored, mode, problem)
					}
				}
				plans = append(plans, result)
			}
			persist()

			timing := headsTiming{Stored: stored}
			rows := func() int {
				var n int
				if err := p.db.QueryRow(`SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1`, key).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			// One unmeasured unchanged save warms the cached statement.
			headsCommit(t, p, scope, "warm", later, batch(0, later)...)
			for rep := 1; rep <= reps; rep++ {
				when := later.Add(time.Duration(rep) * time.Minute)
				before := rows()
				started := time.Now()
				headsCommit(t, p, scope, fmt.Sprintf("changed-%d", rep), when, batch(rep, when)...)
				timing.ChangedSavesMS = append(timing.ChangedSavesMS, float64(time.Since(started).Microseconds())/1000)
				if added := rows() - before; added != batchSize {
					t.Fatalf("changed save appended %d rows, want %d", added, batchSize)
				}
				again := when.Add(30 * time.Second)
				before = rows()
				started = time.Now()
				headsCommit(t, p, scope, fmt.Sprintf("unchanged-%d", rep), again, batch(rep, again)...)
				timing.UnchangedSavesMS = append(timing.UnchangedSavesMS, float64(time.Since(started).Microseconds())/1000)
				if added := rows() - before; added != 0 {
					t.Fatalf("unchanged save appended %d rows", added)
				}
			}
			var stale int
			if err := p.db.QueryRow(`SELECT COUNT(*) FROM (SELECT event_id FROM kubernetes_evidence_events WHERE scope_key=$1 AND superseded_seq IS NULL GROUP BY event_id HAVING COUNT(*)<>1) x`, key).Scan(&stale); err != nil || stale != 0 {
				t.Fatalf("events without exactly one current version: %d %v", stale, err)
			}
			timing.ChangedMedianMS, timing.UnchangedMedianMS = headsMedian(timing.ChangedSavesMS), headsMedian(timing.UnchangedSavesMS)
			t.Logf("SAVE stored=%d batch=%d changed_median_ms=%.1f unchanged_median_ms=%.1f changed=%v unchanged=%v", stored, batchSize, timing.ChangedMedianMS, timing.UnchangedMedianMS, timing.ChangedSavesMS, timing.UnchangedSavesMS)
			timings = append(timings, timing)
			persist()
		})
	}
	if len(timings) < 2 {
		t.Fatalf("only %d sizes measured; no growth comparison", len(timings))
	}
	first, last := timings[0], timings[len(timings)-1]
	for _, ratio := range []struct {
		name     string
		from, to float64
	}{{"changed", first.ChangedMedianMS, last.ChangedMedianMS}, {"unchanged", first.UnchangedMedianMS, last.UnchangedMedianMS}} {
		t.Logf("RATIO %s stored %d→%d: %.2f×", ratio.name, first.Stored, last.Stored, ratio.to/ratio.from)
		if ratio.to >= 2*ratio.from {
			t.Errorf("%s save grew with stored history: %.1fms at %d, %.1fms at %d", ratio.name, ratio.from, first.Stored, ratio.to, last.Stored)
		}
	}
}
