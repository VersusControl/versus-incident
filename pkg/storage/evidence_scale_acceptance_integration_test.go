package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type scaleStatement struct {
	SQL  string
	Args []any
}

type scaleQueryTrace struct {
	enabled atomic.Bool
	mu      sync.Mutex
	queries []scaleStatement
}

func (s *scaleQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if s.enabled.Load() && (strings.HasPrefix(data.SQL, "SELECT") || strings.HasPrefix(data.SQL, "WITH")) {
		args := data.Args
	options:
		for len(args) > 0 {
			switch args[0].(type) {
			case pgx.QueryExecMode, pgx.QueryResultFormats, pgx.QueryResultFormatsByOID:
				args = args[1:]
			default:
				break options
			}
		}
		s.mu.Lock()
		s.queries = append(s.queries, scaleStatement{data.SQL, append([]any(nil), args...)})
		s.mu.Unlock()
	}
	return ctx
}

func (*scaleQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type scaleMeasurement struct {
	Tier, Shape string
	Samples     int
	P95MS       float64
	BudgetMS    float64
	RatioToS    float64
	Rows, Bytes int
	Error       string `json:",omitempty"`
}

type scalePlan struct {
	Tier, Shape, SQL string
	Args             []any
	PreparedName     string
	GenericPlans     int64
	CustomPlans      int64
	PlanSource       string
	Plan             json.RawMessage
	HotSeqScans      []string
	Indexes          []string
	SharedBlocks     float64
	Error            string `json:",omitempty"`
}

type scaleShape struct {
	name   string
	scope  EvidenceScope
	query  EvidenceQuery
	budget time.Duration
	next   bool
}

// This opt-in test retains every corpus. Timing excludes fixture creation,
// validation, SQL tracing and EXPLAIN; each measured first page recomputes counts.
func TestPostgresEvidenceScaleAcceptance(t *testing.T) {
	if os.Getenv("EVIDENCE_SCALE_ACCEPTANCE") != "1" {
		t.Skip("set EVIDENCE_SCALE_ACCEPTANCE=1 in a retained, isolated harness database")
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	results := os.Getenv("HARNESS_RESULTS_DIR")
	if dsn == "" || results == "" {
		t.Fatal("TEST_POSTGRES_DSN and HARNESS_RESULTS_DIR are required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	p := provider.(*postgresProvider)
	trace := &scaleQueryTrace{}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tracer = trace
	if err = p.historyDB.Close(); err != nil {
		t.Fatal(err)
	}
	p.historyDB = stdlib.OpenDB(*cfg)
	p.historyDB.SetMaxOpenConns(8)
	p.historyDB.SetMaxIdleConns(5)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var complete bool
		if err = p.db.QueryRowContext(t.Context(), `SELECT complete FROM kubernetes_evidence_partition_migration WHERE singleton`).Scan(&complete); err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("empty fixture partition cutover did not settle; no scale acceptance")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var measurements []scaleMeasurement
	var plans []scalePlan
	defer func() {
		scaleWriteJSON(t, filepath.Join(results, "scale-p95.json"), measurements)
		scaleWriteJSON(t, filepath.Join(results, "scale-plans.json"), plans)
	}()
	baseline := map[string]float64{}
	tiers := []struct {
		name                        string
		events, roots, observations int
	}{{"S", 10000, 100, 10000}, {"M", 1000000, 2000, 1000000}, {"L", 20000000, 20000, 10000000}}
	for _, tier := range tiers {
		if selected := os.Getenv("EVIDENCE_SCALE_ACCEPTANCE_TIERS"); selected != "" && !strings.Contains(","+selected+",", ","+tier.name+",") {
			continue
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("acceptance-%s-%d", tier.name, at.UnixNano()), ClusterUID: "synthetic-physical"}
		key, _ := evidenceScopeKey(scope)
		scaleDiskGuard(t, 30<<30)
		// Provision fixture partitions explicitly so the scale gate is independent
		// of the deployment's partition-maintenance scheduler.
		if _, err = p.db.ExecContext(t.Context(), `SELECT versus_ensure_evidence_partition('kubernetes_evidence_events',$1),versus_ensure_evidence_partition('kubernetes_evidence_events',$2)`, at.Add(-24*time.Hour), at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err = p.CommitEvidence(t.Context(), scope, EvidenceBatch{ID: "initial", At: at.Add(-24 * time.Hour), Epoch: "synthetic"}); err != nil {
			t.Fatal(err)
		}
		occurrence := seedScaleOccurrences(t, p, scope, at, tier.name)
		scaleSeedJournal(t, p, key, at, tier.events)
		for _, table := range []string{"kubernetes_evidence_events", "kubernetes_evidence_issues", "kubernetes_evidence_occurrence_read_versions", "kubernetes_evidence_occurrence_observations", "kubernetes_evidence_occurrence_versions"} {
			if _, err = p.db.ExecContext(t.Context(), "ANALYZE "+table); err != nil {
				t.Fatal(err)
			}
		}
		var eventCount, rootCount, observationCount int
		if err = p.db.QueryRowContext(t.Context(), `SELECT
 (SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id LIKE 'retained-%' AND event_version='immutable'),
 (SELECT COUNT(*) FROM kubernetes_evidence_issues WHERE scope_key=$1),
 (SELECT COUNT(*) FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1)`, key).Scan(&eventCount, &rootCount, &observationCount); err != nil {
			t.Fatal(err)
		}
		if eventCount != tier.events || rootCount != tier.roots || observationCount < tier.observations {
			t.Fatalf("invalid corpus %s: %d events, %d roots, %d observations", tier.name, eventCount, rootCount, observationCount)
		}
		t.Logf("CORPUS tier=%s scope=%s events=%d roots=%d observations=%d", tier.name, key, eventCount, rootCount, observationCount)

		licensed := scope
		licensed.OrgID = "licensed"
		if err = p.CommitEvidence(t.Context(), licensed, EvidenceBatch{ID: "licensed", At: at, Epoch: "synthetic"}); err != nil {
			t.Fatal(err)
		}
		licensed.ReadOrgs = []string{"licensed", "default"}
		shapes := scaleShapes(scope, licensed, at, occurrence)
		for _, shape := range shapes {
			measurement, captured := scaleMeasure(t, p, trace, tier.name, shape)
			if tier.name == "S" {
				baseline[shape.name] = measurement.P95MS
				measurement.RatioToS = 1
			} else if small := baseline[shape.name]; small > 0 {
				measurement.RatioToS = measurement.P95MS / small
				if measurement.RatioToS >= 2 {
					t.Errorf("SCALE tier=%s shape=%s ratio=%.3f >= 2", tier.name, shape.name, measurement.RatioToS)
				}
			} else {
				t.Errorf("missing S baseline for %s", shape.name)
			}
			measurements = append(measurements, measurement)
			t.Logf("P95 tier=%s shape=%s n=%d p95_ms=%.3f budget_ms=%.0f ratio_S=%.3f rows=%d bytes=%d error=%s", tier.name, shape.name, measurement.Samples, measurement.P95MS, measurement.BudgetMS, measurement.RatioToS, measurement.Rows, measurement.Bytes, measurement.Error)
			for _, statement := range captured {
				plan := scaleExplain(t, p, tier.name, shape.name, statement)
				plans = append(plans, plan)
				if tier.name == "L" && (plan.Error != "" || len(plan.HotSeqScans) > 0) {
					t.Errorf("PLAN tier=L shape=%s seq_scans=%v error=%s", shape.name, plan.HotSeqScans, plan.Error)
				}
				t.Logf("PLAN tier=%s shape=%s indexes=%v hot_seq_scans=%v shared_blocks=%.0f generic=%d custom=%d source=%s error=%s", tier.name, shape.name, plan.Indexes, plan.HotSeqScans, plan.SharedBlocks, plan.GenericPlans, plan.CustomPlans, plan.PlanSource, plan.Error)
			}
			// Persist partial matrices even if a later tier reaches a safety limit.
			scaleWriteJSON(t, filepath.Join(results, "scale-p95.json"), measurements)
			scaleWriteJSON(t, filepath.Join(results, "scale-plans.json"), plans)
		}
		scalePinnedContinuation(t, p, scope, at)
		scaleLicenseFallback(t, p, scope, licensed, at, occurrence)
	}
	t.Log("UNCOVERED: online legacy migration/partition-copy under concurrent traffic; write budgets; HTTP break-logs adapter; browser/endurance; before-fix baseline. Shared-buffer numerical budget is unspecified by contract; actual blocks are recorded, not waived.")
}

func scaleShapes(scope, licensed EvidenceScope, at time.Time, occurrence string) []scaleShape {
	base := EvidenceQuery{Since: at.Add(-24 * time.Hour), Until: at, Limit: 50, EnforceAuthority: true, AllowedKinds: []string{"Pod", "Deployment"}}
	var result []scaleShape
	for _, kind := range []string{"timeline", "change", "crash", "issue", "break"} {
		for _, filter := range []string{"all", "combined", "absent"} {
			q := base
			q.Type = kind
			if filter != "all" {
				q.Namespace, q.Kind, q.RootUID, q.Rule = "shop", "Pod", "root-0", "changed"
				if filter == "absent" {
					q.Rule = "never-observed"
				}
			}
			name := kind + "/" + filter
			result = append(result, scaleShape{name + "/first", scope, q, 250 * time.Millisecond, false})
			if filter != "absent" {
				result = append(result, scaleShape{name + "/next", scope, q, 100 * time.Millisecond, true})
			}
		}
	}
	q := base
	q.Type = "occurrence"
	result = append(result,
		scaleShape{"occurrence/all/first", scope, q, 100 * time.Millisecond, false},
		scaleShape{"occurrence/all/next", scope, q, 100 * time.Millisecond, true})
	q.Namespace, q.Kind, q.Rule, q.Variant = "shop", "Pod", "pod.crash_loop", "warning:broken"
	result = append(result,
		scaleShape{"occurrence/combined/first", scope, q, 100 * time.Millisecond, false},
		scaleShape{"occurrence/combined/next", scope, q, 100 * time.Millisecond, true})
	q.Rule = "never-observed"
	result = append(result, scaleShape{"occurrence/absent/first", scope, q, 100 * time.Millisecond, false})
	q = base
	q.Type, q.Lookup, q.OccurrenceID, q.Limit = "occurrence", true, occurrence, 1
	result = append(result, scaleShape{"occurrence/point", scope, q, 20 * time.Millisecond, false})
	q.OccurrenceID, q.RootUID, q.Namespace = "", "root-0", "shop"
	result = append(result, scaleShape{"occurrence/root", scope, q, 50 * time.Millisecond, false})
	q.Center = at.Add(-23 * time.Hour)
	result = append(result, scaleShape{"occurrence/root-center", scope, q, 50 * time.Millisecond, false})
	q = base
	q.Type, q.PointInTime, q.OccurrenceID, q.Limit = "occurrence", true, occurrence, 1
	q.Until = at.Add(-20*time.Hour + 37*time.Second)
	result = append(result, scaleShape{"occurrence/pit", scope, q, 20 * time.Millisecond, false})
	q = base
	q.Type = "timeline"
	result = append(result,
		scaleShape{"licensed/timeline/first", licensed, q, 250 * time.Millisecond, false},
		scaleShape{"licensed/timeline/next", licensed, q, 100 * time.Millisecond, true})
	q.Type = "occurrence"
	result = append(result,
		scaleShape{"licensed/occurrence/first", licensed, q, 100 * time.Millisecond, false},
		scaleShape{"licensed/occurrence/next", licensed, q, 100 * time.Millisecond, true})
	q.Lookup, q.OccurrenceID, q.Limit = true, occurrence, 1
	result = append(result, scaleShape{"licensed/occurrence/point", licensed, q, 20 * time.Millisecond, false})
	return result
}

// Reuse a completed retained corpus to measure the service's namespace-proof,
// gap-overlap and crash observation-order SQL without loading history again.
func TestPostgresEvidenceScaleServiceReadAcceptance(t *testing.T) {
	if os.Getenv("EVIDENCE_SCALE_SERVICE_ACCEPTANCE") != "1" {
		t.Skip("set EVIDENCE_SCALE_SERVICE_ACCEPTANCE=1 against a completed retained scale corpus")
	}
	dsn, results := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("HARNESS_RESULTS_DIR")
	if dsn == "" || results == "" {
		t.Fatal("TEST_POSTGRES_DSN and HARNESS_RESULTS_DIR are required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	p := provider.(*postgresProvider)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	trace := &scaleQueryTrace{}
	cfg.Tracer = trace
	if err = p.historyDB.Close(); err != nil {
		t.Fatal(err)
	}
	p.historyDB = stdlib.OpenDB(*cfg)
	p.historyDB.SetMaxOpenConns(8)
	p.historyDB.SetMaxIdleConns(5)
	var measurements []scaleMeasurement
	var plans []scalePlan
	defer func() {
		scaleWriteJSON(t, filepath.Join(results, "service-scale-p95.json"), measurements)
		scaleWriteJSON(t, filepath.Join(results, "service-scale-plans.json"), plans)
	}()
	baseline := map[string]float64{}
	absentDelta := os.Getenv("EVIDENCE_SCALE_ABSENT_DELTA") == "1"
	delta := os.Getenv("EVIDENCE_SCALE_REPAIR_DELTA") == "1" || absentDelta
	for _, tier := range []string{"S", "M", "L"} {
		if delta && tier == "L" {
			t.Log("BLOCKED tier=L: retained journal is incomplete; repair delta does not seed or accept L")
			continue
		}
		scope := EvidenceScope{OrgID: "default"}
		if err = p.db.QueryRowContext(t.Context(), `SELECT cluster_id,cluster_uid FROM kubernetes_evidence_scopes WHERE org_id='default' AND cluster_id LIKE $1 ORDER BY updated_at DESC LIMIT 1`, "acceptance-"+tier+"-%").Scan(&scope.ClusterID, &scope.ClusterUID); err != nil {
			t.Errorf("missing retained tier %s: %v", tier, err)
			continue
		}
		key, _ := evidenceScopeKey(scope)
		var at time.Time
		if err = p.db.QueryRowContext(t.Context(), `SELECT observed_at FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id='retained-1' AND event_version='immutable' LIMIT 1`, key).Scan(&at); err != nil {
			t.Fatal(err)
		}
		var retainedEvents int
		if err = p.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id LIKE 'retained-%' AND event_version='immutable'`, key).Scan(&retainedEvents); err != nil {
			t.Fatal(err)
		}
		wantEvents := map[string]int{"S": 10000, "M": 1000000, "L": 20000000}[tier]
		if retainedEvents < wantEvents && os.Getenv("EVIDENCE_SCALE_RESUME_EVENTS") == "1" {
			scaleSeedJournal(t, p, key, at, wantEvents)
			retainedEvents = wantEvents
			if _, err = p.db.ExecContext(t.Context(), `ANALYZE kubernetes_evidence_events`); err != nil {
				t.Fatal(err)
			}
		}
		if retainedEvents != wantEvents {
			t.Errorf("incomplete retained tier %s: events=%d want=%d; no acceptance measurements", tier, retainedEvents, wantEvents)
			continue
		}
		var occurrence string
		var roots, observations int
		if err = p.db.QueryRowContext(t.Context(), `SELECT
 (SELECT COUNT(*) FROM kubernetes_evidence_issues WHERE scope_key=$1),
 (SELECT COUNT(*) FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1)`, key).Scan(&roots, &observations); err != nil {
			t.Fatal(err)
		}
		if roots != map[string]int{"S": 100, "M": 2000, "L": 20000}[tier] || observations < map[string]int{"S": 10000, "M": 1000000, "L": 10000000}[tier] {
			t.Errorf("incomplete retained tier %s: roots=%d observations=%d", tier, roots, observations)
			continue
		}
		if err = p.db.QueryRowContext(t.Context(), `SELECT occurrence_id FROM kubernetes_evidence_issues WHERE scope_key=$1 AND root_uid='root-0' LIMIT 1`, key).Scan(&occurrence); err != nil {
			t.Fatal(err)
		}
		licensed := scope
		licensed.OrgID, licensed.ReadOrgs = "licensed", []string{"licensed", "default"}
		if !absentDelta {
			scaleLicensePrecedence(t, p, scope, licensed, at, occurrence)
		}
		for _, shape := range scaleShapes(scope, licensed, at, occurrence) {
			if delta && (shape.query.Lookup || shape.query.PointInTime) {
				continue
			}
			prepare := false
			if absentDelta {
				if shape.query.Type != "issue" && shape.query.Type != "occurrence" {
					continue
				}
				prepare = strings.HasPrefix(shape.name, "issue/combined/") || strings.HasPrefix(shape.name, "occurrence/combined/")
				if !prepare && shape.name != "issue/absent/first" && shape.name != "occurrence/absent/first" {
					continue
				}
			}
			shape.name = "service/" + shape.name
			shape.query.Authorities = map[string]EvidenceAuthority{
				"Pod":        {State: "allowed", ProvedAt: at},
				"Deployment": {State: "allowed", ProvedAt: at},
				"Pod@ops":    {Namespace: "ops", State: "denied", ProvedAt: at.Add(time.Second)},
			}
			if shape.query.Type == "change" || shape.query.Type == "crash" {
				shape.query.WithGaps = true
			}
			if shape.query.Type == "crash" {
				shape.query.ByObservation = true
			}
			if prepare {
				warm, captured := scaleMeasure(t, p, trace, tier, shape)
				for _, statement := range captured {
					plan := scaleExplain(t, p, tier, "prepare/"+shape.name, statement)
					plans = append(plans, plan)
					if plan.Error != "" {
						t.Errorf("prepared sequence plan: %s", plan.Error)
					}
				}
				t.Logf("PREPARE tier=%s shape=%s samples=%d error=%s", tier, shape.name, warm.Samples, warm.Error)
				continue
			}
			m, captured := scaleMeasure(t, p, trace, tier, shape)
			if tier == "S" {
				baseline[shape.name], m.RatioToS = m.P95MS, 1
			} else if small := baseline[shape.name]; small > 0 {
				m.RatioToS = m.P95MS / small
				if m.RatioToS >= 2 {
					t.Errorf("SCALE tier=%s shape=%s ratio=%.3f >= 2", tier, shape.name, m.RatioToS)
				}
			} else {
				t.Errorf("missing S baseline for %s", shape.name)
			}
			measurements = append(measurements, m)
			t.Logf("P95 tier=%s shape=%s n=%d p95_ms=%.3f budget_ms=%.0f ratio_S=%.3f rows=%d bytes=%d error=%s", tier, shape.name, m.Samples, m.P95MS, m.BudgetMS, m.RatioToS, m.Rows, m.Bytes, m.Error)
			for _, statement := range captured {
				plan := scaleExplain(t, p, tier, shape.name, statement)
				plans = append(plans, plan)
				if tier == "L" && (plan.Error != "" || len(plan.HotSeqScans) > 0) {
					t.Errorf("PLAN tier=L shape=%s seq_scans=%v error=%s", shape.name, plan.HotSeqScans, plan.Error)
				}
				t.Logf("PLAN tier=%s shape=%s indexes=%v hot_seq_scans=%v shared_blocks=%.0f generic=%d custom=%d source=%s error=%s", tier, shape.name, plan.Indexes, plan.HotSeqScans, plan.SharedBlocks, plan.GenericPlans, plan.CustomPlans, plan.PlanSource, plan.Error)
			}
			scaleWriteJSON(t, filepath.Join(results, "service-scale-p95.json"), measurements)
			scaleWriteJSON(t, filepath.Join(results, "service-scale-plans.json"), plans)
		}
		if !absentDelta {
			scalePinnedContinuation(t, p, scope, at.Add(2*time.Minute))
		}
	}
}

func scaleMeasure(t *testing.T, p *postgresProvider, trace *scaleQueryTrace, tier string, shape scaleShape) (scaleMeasurement, []scaleStatement) {
	t.Helper()
	m := scaleMeasurement{Tier: tier, Shape: shape.name, BudgetMS: float64(shape.budget) / float64(time.Millisecond)}
	q := shape.query
	if shape.next {
		first, err := p.QueryEvidence(t.Context(), shape.scope, q)
		if err != nil || first.Next == "" {
			m.Error = fmt.Sprintf("no genuine continuation: %v", err)
			t.Error(shape.name, m.Error)
			return m, nil
		}
		q.Cursor = first.Next
	}
	var elapsed []time.Duration
	var last EvidencePage
	for i := -3; i < 21; i++ {
		if q.Cursor == "" {
			p.evidenceQueryMu.Lock()
			p.evidenceQueries = nil
			p.evidenceQueryMu.Unlock()
		}
		start := time.Now()
		page, err := p.QueryEvidence(t.Context(), shape.scope, q)
		duration := time.Since(start)
		if err != nil {
			m.Error = err.Error()
			t.Errorf("QUERY tier=%s shape=%s: %v", tier, shape.name, err)
			break
		}
		if i >= 0 {
			elapsed = append(elapsed, duration)
		}
		scaleBoundedPage(t, page, q)
		if !strings.Contains(shape.name, "absent") && len(page.Items) == 0 {
			t.Errorf("empty nonselective result tier=%s shape=%s", tier, shape.name)
		}
		if i >= 0 && !reflect.DeepEqual(last.Items, page.Items) && len(last.Items) != 0 {
			t.Errorf("unstable retained result tier=%s shape=%s", tier, shape.name)
		}
		last = page
	}
	sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
	m.Samples = len(elapsed)
	if m.Samples > 0 {
		m.P95MS = float64(elapsed[int(math.Ceil(float64(len(elapsed))*.95))-1]) / float64(time.Millisecond)
	}
	if m.P95MS >= m.BudgetMS {
		t.Errorf("BUDGET tier=%s shape=%s p95=%.3fms >= %.0fms", tier, shape.name, m.P95MS, m.BudgetMS)
	}
	raw, _ := json.Marshal(last)
	m.Rows, m.Bytes = len(last.Items), len(raw)
	trace.mu.Lock()
	trace.queries = nil
	trace.mu.Unlock()
	p.evidenceQueryMu.Lock()
	p.evidenceQueries = nil
	p.evidenceQueryMu.Unlock()
	trace.enabled.Store(true)
	_, _ = p.QueryEvidence(t.Context(), shape.scope, q)
	trace.enabled.Store(false)
	trace.mu.Lock()
	captured := append([]scaleStatement(nil), trace.queries...)
	trace.mu.Unlock()
	return m, captured
}

func scaleBoundedPage(t *testing.T, page EvidencePage, q EvidenceQuery) {
	t.Helper()
	if len(page.Items) > q.Limit || page.Total > 1001 {
		t.Errorf("unbounded response rows=%d total=%d", len(page.Items), page.Total)
	}
	if page.CountCapped != (page.Total > 1000) {
		t.Errorf("incorrect cap flag total=%d flag=%t", page.Total, page.CountCapped)
	}
	if q.Cursor != "" && (page.Total != 0 || len(page.Counts) != 0) {
		t.Error("continuation unexpectedly recomputed counts")
	}
	raw, _ := json.Marshal(page)
	if len(raw) > 256<<10 {
		t.Errorf("bounded fixture response exceeded 256KiB: %d", len(raw))
	}
	seen := map[string]bool{}
	for _, record := range page.Items {
		if seen[record.ID] {
			t.Errorf("duplicate record %s", record.ID)
		}
		seen[record.ID] = true
		if q.PointInTime {
			var issue EvidenceIssue
			if err := json.Unmarshal(record.Payload, &issue); err != nil || issue.LastSeen.After(q.Until) || issue.LastSeen.IsZero() {
				t.Errorf("invalid retained PIT observation: %s (%v)", record.Payload, err)
			}
		}
	}
}

func scaleExplain(t *testing.T, p *postgresProvider, tier, shape string, statement scaleStatement) scalePlan {
	t.Helper()
	result := scalePlan{Tier: tier, Shape: shape, SQL: statement.SQL, Args: statement.Args}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var raw []byte
	err := scaleExplainPrepared(ctx, p, statement, &result, &raw)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if result.PreparedName == "" {
		result.PlanSource = "SQL text; original prepared plan unavailable"
		if err = p.db.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement.SQL, statement.Args...).Scan(&raw); err != nil {
			result.Error = err.Error()
			return result
		}
	}
	result.Plan = raw
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		result.Error = err.Error()
		return result
	}
	if len(decoded) > 0 {
		root, _ := decoded[0]["Plan"].(map[string]any)
		hit, _ := root["Shared Hit Blocks"].(float64)
		read, _ := root["Shared Read Blocks"].(float64)
		result.SharedBlocks = hit + read
		scalePlanTree(&result, root)
	}
	return result
}

func scaleExplainPrepared(ctx context.Context, p *postgresProvider, statement scaleStatement, result *scalePlan, raw *[]byte) error {
	conn, err := p.historyDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		native := driverConn.(*stdlib.Conn).Conn()
		rows, err := native.Query(ctx, `SELECT name,generic_plans,custom_plans FROM pg_prepared_statements WHERE statement=$1`, statement.SQL)
		if err != nil {
			return err
		}
		if rows.Next() {
			err = rows.Scan(&result.PreparedName, &result.GenericPlans, &result.CustomPlans)
		}
		rows.Close()
		if err != nil {
			return err
		}
		if err = rows.Err(); err != nil || result.PreparedName == "" {
			return err
		}
		parameters := make([]string, len(statement.Args))
		for i := range parameters {
			parameters[i] = fmt.Sprintf("$%d", i+1)
		}
		sql := "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE " + pgx.Identifier{result.PreparedName}.Sanitize()
		if len(parameters) > 0 {
			sql += "(" + strings.Join(parameters, ",") + ")"
		}
		// Simple protocol quotes bound values through pgx. EXECUTE reuses the
		// very same cached custom/generic plan as the measured history pool.
		args := []any{pgx.QueryExecModeSimpleProtocol}
		for _, value := range statement.Args {
			if rawJSON, ok := value.([]byte); ok {
				args = append(args, string(rawJSON))
			} else {
				args = append(args, value)
			}
		}
		planRows, err := native.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer planRows.Close()
		if planRows.Next() {
			err = planRows.Scan(raw)
		}
		if err != nil {
			return err
		}
		result.PlanSource = "history session EXPLAIN EXECUTE"
		return planRows.Err()
	})
}

func scalePlanTree(result *scalePlan, node map[string]any) {
	relation, _ := node["Relation Name"].(string)
	nodeType, _ := node["Node Type"].(string)
	if strings.Contains(nodeType, "Seq Scan") && (strings.HasPrefix(relation, "kubernetes_evidence_events") || strings.HasPrefix(relation, "evidence_event_month_") || strings.HasPrefix(relation, "kubernetes_evidence_issues") || strings.HasPrefix(relation, "kubernetes_evidence_occurrence_observations") || strings.HasPrefix(relation, "evidence_observation_month_") || strings.HasPrefix(relation, "kubernetes_evidence_occurrence_read_versions")) {
		result.HotSeqScans = append(result.HotSeqScans, relation)
	}
	if index, ok := node["Index Name"].(string); ok {
		result.Indexes = append(result.Indexes, index)
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		if nested, ok := child.(map[string]any); ok {
			scalePlanTree(result, nested)
		}
	}
}

func TestEvidenceScalePlanDetection(t *testing.T) {
	trace := &scaleQueryTrace{}
	trace.enabled.Store(true)
	trace.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL:  "SELECT $1::text",
		Args: []any{pgx.QueryResultFormatsByOID{23: 1}, pgx.QueryExecModeCacheStatement, "bound"},
	})
	if len(trace.queries) != 1 || !reflect.DeepEqual(trace.queries[0].Args, []any{"bound"}) {
		t.Fatal("driver control options were mistaken for SQL bind values")
	}
	for _, relation := range []string{"kubernetes_evidence_events", "evidence_event_month_202611", "kubernetes_evidence_issues", "kubernetes_evidence_occurrence_read_versions", "kubernetes_evidence_occurrence_observations", "evidence_observation_month_202611"} {
		var plan scalePlan
		scalePlanTree(&plan, map[string]any{"Node Type": "Gather", "Plans": []any{map[string]any{"Node Type": "Seq Scan", "Relation Name": relation}}})
		if !reflect.DeepEqual(plan.HotSeqScans, []string{relation}) {
			t.Errorf("missed hot relation sequential scan: %s", relation)
		}
	}
	var plan scalePlan
	scalePlanTree(&plan, map[string]any{"Node Type": "Index Scan", "Relation Name": "evidence_event_month_202611", "Index Name": "real_hot_index"})
	if len(plan.HotSeqScans) != 0 || !reflect.DeepEqual(plan.Indexes, []string{"real_hot_index"}) {
		t.Errorf("incorrect index classification: %+v", plan)
	}
}

func TestPostgresEvidenceScalePreparedPlanExplain(t *testing.T) {
	if os.Getenv("EVIDENCE_SCALE_PLAN_PROBE") != "1" {
		t.Skip("set EVIDENCE_SCALE_PLAN_PROBE=1 for the isolated cached-plan probe")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: os.Getenv("TEST_POSTGRES_DSN")})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	p := provider.(*postgresProvider)
	statement := scaleStatement{
		SQL:  `SELECT 1 WHERE $1::timestamptz IS NOT NULL AND cardinality($2::text[])>0 AND cardinality($3::bigint[])>0 AND $4::jsonb IS NOT NULL AND $5::int>0`,
		Args: []any{time.Now().UTC(), []string{"a'b", "second"}, []int64{42}, []byte(`{"state":"allowed"}`), 51},
	}
	for range 8 {
		var value int
		if err = p.historyDB.QueryRowContext(t.Context(), statement.SQL, statement.Args...).Scan(&value); err != nil || value != 1 {
			t.Fatalf("warm cached query: value=%d error=%v", value, err)
		}
	}
	plan := scaleExplain(t, p, "probe", "cached-plan", statement)
	if plan.Error != "" || plan.PlanSource != "history session EXPLAIN EXECUTE" || plan.PreparedName == "" || len(plan.Plan) == 0 || plan.GenericPlans+plan.CustomPlans < 8 {
		t.Fatalf("cached plan was not explained: %+v", plan)
	}
	t.Logf("prepared_source=%s generic=%d custom=%d", plan.PlanSource, plan.GenericPlans, plan.CustomPlans)
}

func scaleSeedJournal(t *testing.T, p *postgresProvider, key string, at time.Time, count int) {
	t.Helper()
	if _, err := p.db.ExecContext(t.Context(), `SELECT versus_ensure_evidence_partition('kubernetes_evidence_events',$1),versus_ensure_evidence_partition('kubernetes_evidence_events',$2)`, at.Add(-24*time.Hour), at); err != nil {
		t.Fatal(err)
	}
	var existing, highest int
	if err := p.db.QueryRowContext(t.Context(), `SELECT COUNT(*),COALESCE(MAX(split_part(event_id,'-',2)::bigint),0) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id LIKE 'retained-%' AND event_version='immutable'`, key).Scan(&existing, &highest); err != nil {
		t.Fatal(err)
	}
	if existing != highest || existing > count {
		t.Fatalf("cannot append to a noncontiguous corpus: count=%d highest=%d target=%d", existing, highest, count)
	}
	for start := existing + 1; start <= count; start += 100000 {
		scaleDiskGuard(t, 30<<30)
		end := min(count, start+99999)
		if _, err := p.db.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_events
 (scope_key,event_id,event_version,event_type,event_at,observed_at,namespace,kind,authorization_kind,resource_uid,root_uid,rule,epoch,payload,is_break,gap_from,gap_to)
 SELECT $1,'retained-'||n,'immutable',(ARRAY['change','crash','issue_opened','issue_observed','issue_resolved','coverage_gap'])[1+((n/20)%6)],
 $2::timestamptz-(n%86400)*INTERVAL '1 second',$2,
 CASE WHEN n%10<5 THEN 'shop' ELSE 'ops' END,
 CASE WHEN (n/10)%2=0 THEN 'Pod' ELSE 'Deployment' END,
 CASE WHEN (n/10)%2=0 THEN 'Pod' ELSE 'Deployment' END,
 'pod-'||(n%100),'root-'||(n%10),'changed','synthetic',
 jsonb_build_object('description',repeat('x',256)),(n/20)%6 IN(2,4),
 $2::timestamptz-INTERVAL '24 hours',$2
 FROM generate_series($3::bigint,$4::bigint)n`, key, at, start, end); err != nil {
			t.Fatal(err)
		}
		if start == 1 || end%1000000 == 0 || end == count {
			var size int64
			if err := p.db.QueryRowContext(t.Context(), `SELECT pg_database_size(current_database())`).Scan(&size); err != nil {
				t.Fatal(err)
			}
			t.Logf("SEED journal=%d/%d database_bytes=%d", end, count, size)
		}
	}
	if _, err := p.db.ExecContext(t.Context(), `UPDATE kubernetes_evidence_scopes SET high_sequence=(SELECT MAX(sequence) FROM kubernetes_evidence_events WHERE scope_key=$1),checkpoint=jsonb_set(checkpoint,'{at}',to_jsonb($2::timestamptz)) WHERE scope_key=$1`, key, at); err != nil {
		t.Fatal(err)
	}
}

func scaleDiskGuard(t *testing.T, reserve uint64) {
	t.Helper()
	var disk syscall.Statfs_t
	if err := syscall.Statfs(".", &disk); err != nil {
		t.Fatal(err)
	}
	free := uint64(disk.Bavail) * uint64(disk.Bsize)
	if free < reserve {
		t.Fatalf("disk safety stop: %d bytes free, require %d; retained corpus is NOT acceptance", free, reserve)
	}
}

func scaleWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err == nil {
		err = os.WriteFile(path, raw, 0600)
	}
	if err != nil {
		t.Errorf("persist scale evidence: %v", err)
	}
}

func scalePinnedContinuation(t *testing.T, p *postgresProvider, scope EvidenceScope, at time.Time) {
	t.Helper()
	for _, kind := range []string{"timeline", "occurrence"} {
		q := EvidenceQuery{Type: kind, Since: at.Add(-24 * time.Hour), Until: at.Add(time.Hour), Limit: 50}
		first, err := p.QueryEvidence(t.Context(), scope, q)
		if err != nil || first.Next == "" {
			t.Errorf("pin %s first: %v", kind, err)
			continue
		}
		q.Cursor = first.Next
		before, err := p.QueryEvidence(t.Context(), scope, q)
		if err != nil || len(before.Items) == 0 {
			t.Errorf("pin %s before: %v", kind, err)
			continue
		}
		cp, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		batchAt := cp.At.Add(time.Minute)
		if batchAt.Before(at.Add(time.Minute)) {
			batchAt = at.Add(time.Minute)
		}
		batch := EvidenceBatch{ID: fmt.Sprintf("concurrent-%s-%d", kind, time.Now().UnixNano()), Expected: cp.ID, Generation: cp.Generation, At: batchAt, Epoch: "synthetic"}
		if kind == "timeline" {
			record := before.Items[0]
			record.Payload = json.RawMessage(`{"new_revision":true}`)
			record.ObservedAt = batch.At
			batch.Records = []EvidenceRecord{record, {ID: "new-after-pin", Type: "change", At: at.Add(time.Second), ObservedAt: batch.At, Epoch: "synthetic", AuthorizationKind: "Pod", Payload: json.RawMessage(`{}`)}}
		} else {
			var issue EvidenceIssue
			if err = json.Unmarshal(before.Items[0].Payload, &issue); err != nil {
				t.Fatal(err)
			}
			issue.LastSeen = batch.At
			issue.ClosedAt, issue.Broken = &batch.At, false
			batch.Issues = []EvidenceIssue{issue}
		}
		if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Errorf("pin %s concurrent commit: %v", kind, err)
			continue
		}
		after, err := p.QueryEvidence(t.Context(), scope, q)
		if err != nil || !reflect.DeepEqual(before.Items, after.Items) {
			t.Errorf("pinned %s continuation changed after genuine commit: %v", kind, err)
			for i := 0; i < min(len(before.Items), len(after.Items)); i++ {
				if !reflect.DeepEqual(before.Items[i], after.Items[i]) {
					t.Logf("PIN_DIFF type=%s index=%d before=%s after=%s", kind, i, scaleJSON(before.Items[i]), scaleJSON(after.Items[i]))
					break
				}
			}
		}
		t.Logf("PINNED type=%s stable=%t concurrent_commit=true", kind, err == nil && reflect.DeepEqual(before.Items, after.Items))
	}
}

func scaleJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func TestPostgresEvidenceScalePinnedContinuation(t *testing.T) {
	if os.Getenv("EVIDENCE_SCALE_PINNED_PROBE") != "1" {
		t.Skip("set EVIDENCE_SCALE_PINNED_PROBE=1 for the retained-corpus pin probe")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: os.Getenv("TEST_POSTGRES_DSN")})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	p := provider.(*postgresProvider)
	scope := EvidenceScope{OrgID: "default"}
	if err = p.db.QueryRowContext(t.Context(), `SELECT cluster_id,cluster_uid FROM kubernetes_evidence_scopes WHERE org_id='default' AND cluster_id LIKE 'acceptance-S-%' ORDER BY updated_at DESC LIMIT 1`).Scan(&scope.ClusterID, &scope.ClusterUID); err != nil {
		t.Fatal(err)
	}
	key, _ := evidenceScopeKey(scope)
	var at time.Time
	if err = p.db.QueryRowContext(t.Context(), `SELECT observed_at FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_id='retained-1' AND event_version='immutable' LIMIT 1`, key).Scan(&at); err != nil {
		t.Fatal(err)
	}
	scalePinnedContinuation(t, p, scope, at.Add(2*time.Minute))
}

func scaleLicenseFallback(t *testing.T, p *postgresProvider, source, licensed EvidenceScope, at time.Time, occurrence string) {
	t.Helper()
	q := EvidenceQuery{Type: "occurrence", Lookup: true, OccurrenceID: occurrence, Since: at.Add(-24 * time.Hour), Until: at, Limit: 1}
	oss, err := p.QueryEvidence(t.Context(), source, q)
	if err != nil || len(oss.Items) != 1 {
		t.Errorf("license OFF fallback source: %v", err)
		return
	}
	on, err := p.QueryEvidence(t.Context(), licensed, q)
	if err != nil || !reflect.DeepEqual(on.Items, oss.Items) {
		t.Errorf("license ON did not inherit default evidence: %v", err)
	}
	pit := q
	pit.Lookup, pit.PointInTime = false, true
	pit.Until = at.Add(-20*time.Hour + 37*time.Second)
	page, err := p.QueryEvidence(t.Context(), source, pit)
	if err != nil || len(page.Items) != 1 {
		t.Errorf("genuine PIT: %v", err)
		return
	}
	var issue EvidenceIssue
	if err = json.Unmarshal(page.Items[0].Payload, &issue); err != nil {
		t.Fatal(err)
	}
	key, _ := evidenceScopeKey(source)
	var exists bool
	if err = p.db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1 AND occurrence_id=$2 AND last_seen=$3 AND observed_at<=$4)`, key, occurrence, issue.LastSeen, pit.Until).Scan(&exists); err != nil || !exists {
		t.Errorf("PIT invented a timestamp instead of a retained observation: %v", err)
	}
	if issue.LastSeen.Equal(pit.Until) {
		t.Error("PIT fixture unexpectedly equals non-observation boundary")
	}
	t.Logf("FALLBACK off_default=true on_inherits=%t PIT_retained=%t", reflect.DeepEqual(on.Items, oss.Items), exists)
}

func scaleLicensePrecedence(t *testing.T, p *postgresProvider, source, licensed EvidenceScope, at time.Time, occurrence string) {
	t.Helper()
	q := EvidenceQuery{Type: "occurrence", Lookup: true, OccurrenceID: occurrence, Since: at.Add(-24 * time.Hour), Until: at, Limit: 1}
	before, err := p.QueryEvidence(t.Context(), source, q)
	if err != nil || len(before.Items) != 1 {
		t.Errorf("default source for licensed override: %v", err)
		return
	}
	var issue EvidenceIssue
	if err = json.Unmarshal(before.Items[0].Payload, &issue); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(issue.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["source"] = "licensed"
	issue.Payload, _ = json.Marshal(payload)
	issue.LastSeen, issue.ClosedAt, issue.Broken = at, nil, true
	write := licensed
	write.ReadOrgs = nil
	cp, err := p.EvidenceCheckpoint(t.Context(), write)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.CommitEvidence(t.Context(), write, EvidenceBatch{ID: "roots-0", Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: "synthetic", Issues: []EvidenceIssue{issue}}); err != nil {
		t.Errorf("licensed override fixture: %v", err)
		return
	}
	on, err := p.QueryEvidence(t.Context(), licensed, q)
	if err != nil || len(on.Items) != 1 || reflect.DeepEqual(on.Items, before.Items) {
		t.Errorf("licensed occurrence did not override matching default identity: %v", err)
	}
	after, err := p.QueryEvidence(t.Context(), source, q)
	if err != nil || !reflect.DeepEqual(before.Items, after.Items) {
		t.Errorf("licensed override mutated license-OFF/default evidence: %v", err)
	}
	key, _ := evidenceScopeKey(source)
	if err = p.db.QueryRowContext(t.Context(), `SELECT occurrence_id FROM kubernetes_evidence_issues WHERE scope_key=$1 AND root_uid='root-1' LIMIT 1`, key).Scan(&q.OccurrenceID); err != nil {
		t.Fatal(err)
	}
	offFallback, err := p.QueryEvidence(t.Context(), source, q)
	if err != nil {
		t.Fatal(err)
	}
	onFallback, err := p.QueryEvidence(t.Context(), licensed, q)
	if err != nil || !reflect.DeepEqual(offFallback.Items, onFallback.Items) || len(onFallback.Items) != 1 {
		t.Errorf("unshadowed default occurrence did not survive licensed precedence: %v", err)
	}
	t.Logf("PRECEDENCE licensed_override=%t default_unchanged=%t unshadowed_fallback=%t", !reflect.DeepEqual(on.Items, before.Items), reflect.DeepEqual(before.Items, after.Items), reflect.DeepEqual(offFallback.Items, onFallback.Items))
}
