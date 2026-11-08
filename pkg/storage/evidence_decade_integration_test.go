package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// decadeMonths is the number of monthly partitions ten years of history fills.
const decadeMonths = 120

// decadeSequenceSlot is the sequence range reserved for one scope's month, so
// seeded sequences follow time even when older months are seeded later.
const decadeSequenceSlot = 1000000

// decadeVolume is the seeded shape of one deployment month. A busy cluster
// writes far more; the gate keeps every partition the same size so that the
// year one and year ten stores differ only in age, partition count and the
// tables that grow with distinct objects.
type decadeVolume struct {
	MonthRows   int   // journal rows per month in the busiest scope
	Occurrences int   // occurrences opened per month in the busiest scope
	Chronic     int   // long-lived open occurrences added at each stage's start
	MaxBytes    int64 // database size budget
}

func decadeVolumeFromEnv(t *testing.T) decadeVolume {
	t.Helper()
	volume := decadeVolume{MonthRows: 8000, Occurrences: 600, Chronic: 10, MaxBytes: 20 << 30}
	for _, setting := range []struct {
		name  string
		value *int
	}{{"EVIDENCE_DECADE_MONTH_ROWS", &volume.MonthRows}, {"EVIDENCE_DECADE_MONTH_OCCURRENCES", &volume.Occurrences}, {"EVIDENCE_DECADE_CHRONIC", &volume.Chronic}} {
		if raw := os.Getenv(setting.name); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 100 && setting.name != "EVIDENCE_DECADE_CHRONIC" || parsed >= decadeSequenceSlot || parsed < 0 {
				t.Fatalf("invalid %s %q", setting.name, raw)
			}
			*setting.value = parsed
		}
	}
	if raw := os.Getenv("EVIDENCE_DECADE_MAX_BYTES"); raw != "" {
		if _, err := fmt.Sscan(raw, &volume.MaxBytes); err != nil || volume.MaxBytes <= 0 {
			t.Fatalf("invalid EVIDENCE_DECADE_MAX_BYTES %q", raw)
		}
	}
	return volume
}

// decadeScope is one seeded capture scope and the months it holds history for.
type decadeScope struct {
	Name        string
	Scope       EvidenceScope
	Key         string
	Rows        int
	Occurrences int
	From, To    int
	Base        int64
}

type decadeHarness struct {
	t       *testing.T
	p       *postgresProvider
	dsn     string
	now     time.Time
	current time.Time
	volume  decadeVolume
	scopes  []*decadeScope
	trace   *scaleQueryTrace
	seeded  map[string]bool
	results string
}

func (h *decadeHarness) monthStart(m int) time.Time {
	return h.current.AddDate(0, m-(decadeMonths-1), 0)
}

// monthEnd is where seeded rows of month m stop. The current month stops two
// hours before now so every seeded row is in the past.
func (h *decadeHarness) monthEnd(m int) time.Time {
	if m == decadeMonths-1 {
		return h.now.Add(-2 * time.Hour)
	}
	return h.monthStart(m + 1)
}

// revision numbers captures by minute since the first decade month, the
// order a capture's occurrence revision follows.
func (h *decadeHarness) revision(at time.Time) int64 {
	return int64(at.Sub(h.monthStart(0)) / time.Minute)
}

func (h *decadeHarness) scope(name string) *decadeScope {
	for _, s := range h.scopes {
		if s.Name == name {
			return s
		}
	}
	h.t.Fatalf("unknown scope %s", name)
	return nil
}

func (h *decadeHarness) exec(query string, args ...any) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), 30*time.Minute)
	defer cancel()
	// Seeding writes journal rows with their heads, as a heads writer does.
	// VACUUM cannot run inside the transaction that declares it.
	run := headsWriterExec
	if strings.HasPrefix(query, "VACUUM") {
		run = func(ctx context.Context, db *sql.DB, query string, args ...any) (sql.Result, error) {
			return db.ExecContext(ctx, query, args...)
		}
	}
	if _, err := run(ctx, h.p.db, query, args...); err != nil {
		h.t.Fatalf("%v\n%s", err, query)
	}
}

func (h *decadeHarness) budget(next int64) {
	h.t.Helper()
	var size int64
	if err := h.p.db.QueryRowContext(h.t.Context(), `SELECT pg_database_size(current_database())`).Scan(&size); err != nil {
		h.t.Fatal(err)
	}
	if size+next*headsVersionBytes > h.volume.MaxBytes {
		h.t.Fatalf("database size safety stop: %d bytes used, next step needs about %d, budget %d", size, next*headsVersionBytes, h.volume.MaxBytes)
	}
}

// ensureMonth creates the journal and observation partitions of month m, as a
// save in that month would.
func (h *decadeHarness) ensureMonth(m int) {
	at := h.monthStart(m).Add(time.Hour)
	h.exec(`SELECT versus_ensure_evidence_partition('kubernetes_evidence_events',$1)`, at)
	h.exec(`SELECT versus_ensure_evidence_partition('kubernetes_evidence_occurrence_observations',$1)`, at)
}

// decadeJournalSQL seeds one month of one scope, set-based. Per hundred rows:
// 64 changes (one in ten superseded by the next row of the same event), 17
// crash terminations, 8 crash status rows refreshed in place for up to a
// week, 10 issue lifecycle rows and 1 coverage gap. Objects keep their
// identity across months; Pods are new every month, so heads grow with Pod
// churn as they do in a real cluster.
const decadeJournalSQL = `INSERT INTO kubernetes_evidence_events
 (sequence,scope_key,event_id,event_version,priority,event_type,is_break,event_at,observed_at,gap_from,gap_to,namespace,resource_name,kind,authorization_kind,resource_uid,root_uid,pod_uid,rule,epoch,payload,superseded_seq)
SELECT $2::bigint+n,$1::text,x.id,'v'||n,0,c.class,x.is_break,t.at,x.observed,x.gap_from,x.gap_to,x.ns,x.name,x.kind,x.authz,x.uid,x.root,x.pod,x.rule,'stable',x.payload,
 CASE WHEN t.b<60 AND t.b%10=0 THEN $2::bigint+n+1 END
FROM generate_series(0,$5::bigint-1) n
CROSS JOIN LATERAL(SELECT $3::timestamptz+($4::timestamptz-$3::timestamptz)*(n::float8/$5::bigint) AS at,n%100 AS b,$6::int AS m) t
CROSS JOIN LATERAL(SELECT CASE WHEN t.b<60 AND t.b%10=0 THEN n+1 ELSE n END%2000 AS o,n%400 AS q,n%300 AS r) i
CROSS JOIN LATERAL(SELECT
 CASE WHEN t.b<60 OR t.b BETWEEN 95 AND 98 THEN 'change' WHEN t.b<85 THEN 'crash' WHEN t.b<95 THEN (ARRAY['issue_opened','issue_observed','issue_resolved'])[1+t.b%3] ELSE 'coverage_gap' END AS class,
 CASE WHEN t.b BETWEEN 60 AND 76 THEN 'termination' WHEN t.b BETWEEN 77 AND 84 THEN 'status' END AS crash) c
CROSS JOIN LATERAL(SELECT
 CASE c.class WHEN 'change' THEN 'change:'||t.m||':'||(CASE WHEN t.b<60 AND t.b%10=0 THEN n+1 ELSE n END)
  WHEN 'crash' THEN 'crash:'||c.crash||':'||t.m||':'||n WHEN 'coverage_gap' THEN 'gap:'||t.m||':'||n ELSE 'issue:'||t.m||':'||n END AS id,
 c.class LIKE 'issue%' AND t.b%2=0 AND c.class<>'issue_observed' AS is_break,
 CASE WHEN c.crash='termination' THEN t.at+INTERVAL '5 seconds' WHEN c.crash='status' THEN LEAST(t.at+(n%7)*INTERVAL '1 day',$7::timestamptz) ELSE t.at END AS observed,
 CASE WHEN c.class='coverage_gap' THEN t.at-INTERVAL '10 minutes' END AS gap_from,
 CASE WHEN c.class='coverage_gap' THEN t.at END AS gap_to,
 CASE c.class WHEN 'change' THEN 'ns-'||(i.o%40) WHEN 'crash' THEN 'ns-'||((i.q%300)%40) WHEN 'coverage_gap' THEN '' ELSE 'ns-'||(i.r%40) END AS ns,
 CASE c.class WHEN 'change' THEN 'obj-'||i.o WHEN 'crash' THEN 'obj-'||(i.q%300) WHEN 'coverage_gap' THEN '' ELSE 'obj-'||i.r END AS name,
 CASE c.class WHEN 'change' THEN (ARRAY['Deployment','ConfigMap','Service','Secret'])[1+(i.o/40)%4] WHEN 'coverage_gap' THEN CASE WHEN n%2=0 THEN '' ELSE 'Pod' END ELSE 'Deployment' END AS kind,
 CASE c.class WHEN 'change' THEN (ARRAY['Deployment','ConfigMap','Service','Secret'])[1+(i.o/40)%4] WHEN 'crash' THEN 'Pod' WHEN 'coverage_gap' THEN '' ELSE 'Deployment' END AS authz,
 CASE c.class WHEN 'change' THEN 'uid-'||i.o WHEN 'crash' THEN 'pod-'||t.m||'-'||i.q WHEN 'coverage_gap' THEN '' ELSE 'root-'||i.r END AS uid,
 CASE c.class WHEN 'change' THEN 'root-'||i.o WHEN 'crash' THEN 'root-'||(i.q%300) WHEN 'coverage_gap' THEN '' ELSE 'root-'||i.r END AS root,
 CASE WHEN c.class='crash' THEN 'pod-'||t.m||'-'||i.q ELSE '' END AS pod,
 CASE WHEN c.class LIKE 'issue%' THEN 'rule-'||(n%20) ELSE '' END AS rule,
 CASE c.class WHEN 'change' THEN jsonb_build_object('cluster','decade','kind',(ARRAY['Deployment','ConfigMap','Service','Secret'])[1+(i.o/40)%4],'name','obj-'||i.o,'namespace','ns-'||(i.o%40),'type','updated','value',n,'at',t.at)
  WHEN 'crash' THEN jsonb_build_object('type',c.crash,'pod','pod-'||t.m||'-'||i.q,'container','app','reason','OOMKilled','restart_count',n%50,'at',t.at)
  WHEN 'coverage_gap' THEN jsonb_build_object('from',t.at-INTERVAL '10 minutes','to',t.at,'reason','collection_stopped')
  ELSE jsonb_build_object('rule','rule-'||(n%20),'severity','warning','at',t.at) END AS payload) x`

// decadeOccurrenceSQL seeds the occurrence read models of one month. Each
// occurrence opens with one payload version and, once closed six hours later,
// gains a closing version; it is sampled hourly while open. Mode 0 closes every
// occurrence, mode 1 (the current month) leaves one in ten open, and mode 2
// leaves all open from the given start: chronic issues that stay open for
// years and keep being sampled.
const decadeOccurrenceSQL = `WITH o AS MATERIALIZED(
 SELECT j,$2::int AS month,$9::text||j AS id,x.f AS first_seen,
 CASE WHEN $6::int=2 OR $6::int=1 AND j%10=0 THEN NULL ELSE x.f+INTERVAL '6 hours' END AS closed,
 'ns-'||((j%300)%40) AS ns,'root-'||(j%300) AS root,'rule-'||(j%20) AS rule,
 (ARRAY['critical','warning','info'])[1+j%3] AS severity,j%2=0 AS broken
 FROM generate_series(0,$5::int-1) j
 CROSS JOIN LATERAL(SELECT CASE WHEN $6::int=2 THEN $3::timestamptz+j*INTERVAL '1 hour' ELSE $3::timestamptz+($4::timestamptz-$3::timestamptz)*(j::float8/$5::int) END AS f) x
), v AS MATERIALIZED(
 SELECT o.*,1 AS version,o.first_seen AS opened_at,NULL::timestamptz AS version_closed FROM o
 UNION ALL SELECT o.*,2,o.closed,o.closed FROM o WHERE o.closed IS NOT NULL
), p AS MATERIALIZED(
 SELECT v.*,'pv:'||v.id||':'||v.version AS pv,(extract(epoch FROM v.opened_at-$7::timestamptz)/60)::bigint AS revision,
 jsonb_build_object('key',v.id,'occurrence_id',v.id,'namespace',v.ns,'kind','Deployment','uid',v.root,'rule',v.rule,
 'first_seen',v.first_seen,'last_seen',COALESCE(v.version_closed,v.first_seen),'epoch','stable','broken',v.broken,'authorization_kind','Deployment',
 'payload',jsonb_build_object('severity',v.severity,'broke_at',v.first_seen))
 ||CASE WHEN v.version_closed IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('closed_at',v.version_closed) END AS payload
 FROM v
), versions AS(
 INSERT INTO kubernetes_evidence_occurrence_versions(scope_key,payload_version,occurrence_id,payload) SELECT $1,pv,id,payload FROM p RETURNING 1
), reads AS(
 INSERT INTO kubernetes_evidence_occurrence_read_versions(scope_key,occurrence_id,payload_version,revision,superseded_revision,observed_at,superseded_at,first_seen,closed_at,root_uid,namespace,kind,rule,severity,authorization_kind,broken,observation_only,broke_at,payload)
 SELECT $1,p.id,p.pv,p.revision,n.revision,p.opened_at,n.opened_at,p.first_seen,p.version_closed,p.root,p.ns,'Deployment',p.rule,p.severity,'Deployment',p.broken,FALSE,p.first_seen,p.payload
 FROM p LEFT JOIN p n ON n.id=p.id AND n.version=p.version+1 RETURNING 1
), samples AS(
 INSERT INTO kubernetes_evidence_occurrence_observations(scope_key,occurrence_id,revision,observed_at,last_seen,payload_version)
 SELECT $1,o.id,(extract(epoch FROM s.at-$7::timestamptz)/60)::bigint,s.at,s.at,s.pv
 FROM o CROSS JOIN LATERAL(
 SELECT g AS at,'pv:'||o.id||':1' AS pv FROM generate_series(o.first_seen,COALESCE(o.closed-INTERVAL '1 minute',$8::timestamptz),INTERVAL '1 hour') g
 UNION ALL SELECT o.closed,'pv:'||o.id||':2' WHERE o.closed IS NOT NULL) s
 RETURNING 1
), current AS(
 INSERT INTO kubernetes_evidence_issues(scope_key,occurrence_id,issue_key,first_seen,last_seen,closed_at,payload,payload_version,root_uid,namespace,kind,rule,severity,authorization_kind,broken,broke_at,last_observed_at,model_ready,observation_only)
 SELECT $1,p.id,p.id,p.first_seen,COALESCE(p.version_closed,$8::timestamptz),p.version_closed,p.payload,p.pv,p.root,p.ns,'Deployment',p.rule,p.severity,'Deployment',p.broken,p.first_seen,COALESCE(p.version_closed,$8::timestamptz),TRUE,FALSE
 FROM p WHERE p.version=CASE WHEN p.closed IS NULL THEN 1 ELSE 2 END RETURNING 1
) SELECT (SELECT COUNT(*) FROM versions),(SELECT COUNT(*) FROM reads),(SELECT COUNT(*) FROM samples),(SELECT COUNT(*) FROM current)`

// seedMonth writes one scope's journal, heads and occurrences for month m.
func (h *decadeHarness) seedMonth(s *decadeScope, m int) {
	h.t.Helper()
	if h.seeded[s.Name+"/"+strconv.Itoa(m)] || m < s.From || m > s.To {
		return
	}
	h.seeded[s.Name+"/"+strconv.Itoa(m)] = true
	h.budget(int64(s.Rows + s.Occurrences*4))
	start, end := h.monthStart(m), h.monthEnd(m)
	h.exec(decadeJournalSQL, s.Key, s.Base+int64(m)*decadeSequenceSlot, start, end, s.Rows, m, h.now)
	h.exec(`INSERT INTO kubernetes_evidence_event_heads(scope_key,event_id,sequence,event_at,event_version,priority,observed_at)
 SELECT scope_key,event_id,sequence,event_at,event_version,priority,observed_at FROM kubernetes_evidence_events
 WHERE scope_key=$1 AND event_at>=$2 AND event_at<$3 AND superseded_seq IS NULL`, s.Key, start, h.monthStart(m+1))
	if s.Occurrences > 0 {
		mode := 0
		if m == decadeMonths-1 {
			mode = 1
			end = h.now.Add(-8 * time.Hour)
		}
		h.exec(decadeOccurrenceSQL, s.Key, m, start, end, s.Occurrences, mode, h.monthStart(0), h.now.Add(-time.Hour), "occ:"+strconv.Itoa(m)+":")
	}
}

// seedChronic opens occurrences at month from that stay open until now.
func (h *decadeHarness) seedChronic(s *decadeScope, from int, label string) {
	h.t.Helper()
	if h.volume.Chronic == 0 {
		return
	}
	hours := int64(h.now.Sub(h.monthStart(from)) / time.Hour)
	h.budget(hours * int64(h.volume.Chronic) / 10)
	h.exec(decadeOccurrenceSQL, s.Key, from, h.monthStart(from), h.monthStart(from), h.volume.Chronic, 2, h.monthStart(0), h.now.Add(-time.Hour), "occ:chronic:"+label+":")
}

// finishSeed points every scope's checkpoint past the seeded history and
// analyzes each table and each month partition, as autovacuum would. The
// partitioned parents are not analyzed: autovacuum never analyzes them.
func (h *decadeHarness) finishSeed() {
	h.t.Helper()
	for _, s := range h.scopes {
		h.exec(`UPDATE kubernetes_evidence_scopes SET high_sequence=GREATEST(high_sequence,$2),
 checkpoint=jsonb_set(jsonb_set(jsonb_set(checkpoint,'{occurrence_revision}',to_jsonb(GREATEST(COALESCE((checkpoint->>'occurrence_revision')::bigint,0),$3::bigint))),
 '{high_sequence}',to_jsonb(GREATEST(COALESCE((checkpoint->>'high_sequence')::bigint,0),$2::bigint))),
 '{commit_revision}',to_jsonb(GREATEST(COALESCE((checkpoint->>'commit_revision')::bigint,0),$3::bigint)))
 WHERE scope_key=$1`, s.Key, s.Base+int64(decadeMonths)*decadeSequenceSlot, h.revision(h.now)+1)
	}

	rows, err := h.p.db.QueryContext(h.t.Context(), `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent IN (
			'kubernetes_evidence_events'::regclass,
			'kubernetes_evidence_occurrence_observations'::regclass
		)
		ORDER BY 1`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			h.t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}

	for _, name := range append(names,
		"kubernetes_evidence_event_heads",
		"kubernetes_evidence_issues",
		"kubernetes_evidence_occurrence_versions",
		"kubernetes_evidence_occurrence_read_versions",
		"kubernetes_evidence_scopes",
	) {
		h.exec("VACUUM (ANALYZE) " + pgx.Identifier{name}.Sanitize())
	}
	// Flush the seeded months before timing, so measurements do not share the
	// disk with writing them out. A role that may not checkpoint skips this.
	if _, err := h.p.db.ExecContext(h.t.Context(), `CHECKPOINT`); err != nil {
		h.t.Logf("checkpoint before measuring skipped: %v", err)
	}
}

// decadeAuthorities allows every seeded kind but Secret, so every read also
// projects denied rows the way the service does.
func (h *decadeHarness) authorities(q EvidenceQuery) EvidenceQuery {
	q.EnforceAuthority = true
	q.Authorities = map[string]EvidenceAuthority{}
	for _, kind := range []string{"ConfigMap", "Deployment", "Pod", "Service"} {
		q.Authorities[kind] = EvidenceAuthority{State: "allowed", ProvedAt: h.now}
		q.AllowedKinds = append(q.AllowedKinds, kind)
	}
	q.Authorities["Secret"] = EvidenceAuthority{State: "denied", ProvedAt: h.now}
	return q
}

// decadeRead is one read path of the history API.
type decadeRead struct {
	Name  string
	Scope EvidenceScope
	Query EvidenceQuery
	// Depth is the page measured: 1 is the first page with its capped count,
	// deeper pages are reached through the real keyset cursor.
	Depth int
	// Checkpoint reads the commit-stream checkpoint instead of a page.
	Checkpoint bool
	// WholeHistory marks a journal read over a range older than all history:
	// its rows may sit in any month, so it starts one index descent in every
	// month and is held to a per-month bound instead.
	WholeHistory bool
	// Observation marks crashes read in the order they were observed, which
	// the journal's months (by event time) do not follow: every read starts
	// one index descent per month, held to its own per-month bound.
	Observation bool
}

func (h *decadeHarness) reads() []decadeRead {
	main, enterprise := h.scope("main").Scope, h.scope("enterprise").Scope
	enterprise.ReadOrgs = []string{enterprise.OrgID, "default"}
	day, week, month := h.now.Add(-24*time.Hour), h.now.Add(-7*24*time.Hour), h.now.Add(-30*24*time.Hour)
	all := h.current.AddDate(-11, 0, 0)
	until := h.now
	// A closed occurrence half a year back exists at year one and year ten.
	past := h.monthStart(decadeMonths - 7)
	pastIndex := h.volume.Occurrences / 5
	pastID := "occ:" + strconv.Itoa(decadeMonths-7) + ":" + strconv.Itoa(pastIndex)
	pastAt := past.Add(time.Duration(float64(h.monthEnd(decadeMonths-7).Sub(past)) * float64(pastIndex) / float64(h.volume.Occurrences)))
	pastRoot := "root-" + strconv.Itoa(pastIndex%300)
	read := func(name string, scope EvidenceScope, depth int, q EvidenceQuery) decadeRead {
		if q.Until.IsZero() {
			q.Until = until
		}
		return decadeRead{Name: name, Scope: scope, Depth: depth, Query: h.authorities(q), Observation: q.ByObservation, WholeHistory: !q.ByObservation && q.Type != "occurrence" && !q.Since.After(all)}
	}
	change := func(since time.Time) EvidenceQuery {
		return EvidenceQuery{Type: "change", GapsApart: true, Since: since, Limit: 100}
	}
	timeline := func(since time.Time) EvidenceQuery {
		return EvidenceQuery{Type: "timeline", GapsApart: true, Since: since, Limit: 100}
	}
	crash := func(since time.Time) EvidenceQuery {
		return EvidenceQuery{Type: "crash", ByObservation: true, GapsApart: true, Since: since, Limit: 100}
	}
	occurrence := func(since time.Time) EvidenceQuery {
		return EvidenceQuery{Type: "occurrence", Since: since, Limit: 100}
	}
	with := func(q EvidenceQuery, edit func(*EvidenceQuery)) EvidenceQuery {
		edit(&q)
		return q
	}
	return []decadeRead{
		read("changes 24h", main, 1, change(day)),
		read("changes 7d namespace", main, 1, with(change(week), func(q *EvidenceQuery) { q.Namespace = "ns-7" })),
		read("changes 30d namespace+kind+name", main, 1, with(change(month), func(q *EvidenceQuery) { q.Namespace, q.Kind, q.Name = "ns-7", "Deployment", "obj-7" })),
		read("changes all-time", main, 1, change(all)),
		read("changes all-time namespace+kind+name", main, 1, with(change(all), func(q *EvidenceQuery) { q.Namespace, q.Kind, q.Name = "ns-7", "Deployment", "obj-7" })),
		read("changes all-time page 10", main, 10, change(all)),
		read("changes all-time namespace page 10", main, 10, with(change(all), func(q *EvidenceQuery) { q.Namespace = "ns-7" })),
		read("issue change window (root, 2h)", main, 1, with(change(pastAt.Add(-time.Hour)), func(q *EvidenceQuery) { q.Until, q.RootUID = pastAt.Add(time.Hour), pastRoot })),
		read("timeline 24h", main, 1, timeline(day)),
		read("timeline 7d root", main, 1, with(timeline(week), func(q *EvidenceQuery) { q.ResourceUID = "root-7" })),
		read("timeline all-time", main, 1, timeline(all)),
		read("timeline all-time page 10", main, 10, timeline(all)),
		read("crashes 24h", main, 1, crash(day)),
		read("crashes 7d namespace", main, 1, with(crash(week), func(q *EvidenceQuery) { q.Namespace = "ns-7" })),
		read("crashes all-time", main, 1, crash(all)),
		read("crashes all-time page 10", main, 10, crash(all)),
		read("issues 24h", main, 1, occurrence(day)),
		read("issues 7d namespace", main, 1, with(occurrence(week), func(q *EvidenceQuery) { q.Namespace = "ns-7" })),
		read("issues 30d severity", main, 1, with(occurrence(month), func(q *EvidenceQuery) { q.Variant = "critical:broken" })),
		read("issues all-time", main, 1, occurrence(all)),
		read("issues all-time page 10", main, 10, occurrence(all)),
		read("issues historical window (6 months ago, 1d)", main, 1, with(occurrence(past.Add(10*24*time.Hour)), func(q *EvidenceQuery) { q.Until = past.Add(11 * 24 * time.Hour) })),
		read("issue point-in-time (closed, 6 months ago)", main, 1, with(occurrence(all), func(q *EvidenceQuery) { q.PointInTime, q.OccurrenceID, q.Until = true, pastID, pastAt.Add(2*time.Hour) })),
		read("issue point-in-time (chronic, now)", main, 1, with(occurrence(all), func(q *EvidenceQuery) { q.PointInTime, q.OccurrenceID = true, "occ:chronic:year1:0" })),
		read("issue lookup (root)", main, 1, with(occurrence(all), func(q *EvidenceQuery) { q.Lookup, q.RootUID = true, "root-7" })),
		read("enterprise two-scope changes 7d", enterprise, 1, change(week)),
		read("enterprise two-scope timeline all-time", enterprise, 1, timeline(all)),
		read("enterprise two-scope issues 7d", enterprise, 1, occurrence(week)),
		{Name: "commit stream checkpoint", Scope: main, Checkpoint: true},
	}
}

// decadePlan is the EXPLAIN summary of one statement of a read path.
type decadePlan struct {
	SQL                string
	PlanningMS         float64
	ExecutionMS        float64
	PartitionsPlanned  int
	PartitionsExecuted int
	SubplansRemoved    float64
	HotSeqScans        []string `json:",omitempty"`
	TopNode            string
	SharedBlocks       float64
	CachedPlan         string `json:",omitempty"`
	// BuildPlanningMS is the planning of the first execution on a connection,
	// which builds the plan the connection then reuses.
	BuildPlanningMS float64
	Error           string          `json:",omitempty"`
	Plan            json.RawMessage `json:",omitempty"`
}

type decadeReadResult struct {
	Stage, Path     string
	WholeHistory    bool
	Observation     bool
	Scopes          int
	Partitions      int
	Samples         []float64
	MedianMS, MaxMS float64
	PlanningMS      float64
	BuildPlanningMS float64
	ExecutionMS     float64
	Rows            int
	Total           int
	Statements      []decadePlan
	Error           string `json:",omitempty"`
}

func decadePercentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[max(0, int(math.Ceil(float64(len(sorted))*p))-1)]
}

func decadeMonthRelation(name string) bool {
	return strings.HasPrefix(name, "evidence_event_month_") || strings.HasPrefix(name, "evidence_observation_month_")
}

func decadePlanWalk(plan *decadePlan, node map[string]any, planned, executed map[string]bool) {
	relation, _ := node["Relation Name"].(string)
	nodeType, _ := node["Node Type"].(string)
	if decadeMonthRelation(relation) {
		planned[relation] = true
		if loops, _ := node["Actual Loops"].(float64); loops > 0 {
			executed[relation] = true
		}
	}
	if removed, ok := node["Subplans Removed"].(float64); ok {
		plan.SubplansRemoved += removed
	}
	if strings.Contains(nodeType, "Seq Scan") {
		if loops, _ := node["Actual Loops"].(float64); loops > 0 && (decadeMonthRelation(relation) || strings.HasPrefix(relation, "kubernetes_evidence_occurrence_read_versions") || strings.HasPrefix(relation, "kubernetes_evidence_issues") || strings.HasPrefix(relation, "kubernetes_evidence_event_heads")) {
			plan.HotSeqScans = append(plan.HotSeqScans, relation)
		}
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		if nested, ok := child.(map[string]any); ok {
			decadePlanWalk(plan, nested, planned, executed)
		}
	}
}

// explain prepares one traced statement on a connection of its own, with the
// plan cache mode history reads use, and runs it twice under EXPLAIN ANALYZE:
// the first execution builds the plan a pooled connection caches, the second
// is the steady state every later read sees.
func (h *decadeHarness) explain(statement scaleStatement) decadePlan {
	plan := decadePlan{SQL: statement.SQL, CachedPlan: evidenceReadPlanCacheMode}
	ctx, cancel := context.WithTimeout(h.t.Context(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, h.dsn)
	if err != nil {
		plan.Error = err.Error()
		return plan
	}
	defer conn.Close(context.Background())
	settings := "SET jit=off; SET plan_cache_mode=" + evidenceReadPlanCacheMode
	if strings.Contains(statement.SQL, "evidence_event_month_") || strings.Contains(statement.SQL, "WITH candidates") && strings.Contains(statement.SQL, "kubernetes_evidence_events") {
		// Journal walk steps keep their plans, with sorting discouraged.
		settings = "SET jit=off; " + strings.ReplaceAll(journalWalkSettings, "SET LOCAL", "SET")
	}
	if _, err = conn.Exec(ctx, settings); err != nil {
		plan.Error = err.Error()
		return plan
	}
	if _, err = conn.Prepare(ctx, "decade_read", statement.SQL); err != nil {
		plan.Error = err.Error()
		return plan
	}
	parameters := make([]string, len(statement.Args))
	args := []any{pgx.QueryExecModeSimpleProtocol}
	for i, value := range statement.Args {
		parameters[i] = fmt.Sprintf("$%d", i+1)
		if rawJSON, ok := value.([]byte); ok {
			value = string(rawJSON)
		}
		args = append(args, value)
	}
	query := "EXPLAIN (ANALYZE, BUFFERS, SUMMARY, FORMAT JSON) EXECUTE decade_read"
	if len(parameters) > 0 {
		query += "(" + strings.Join(parameters, ",") + ")"
	}
	var raw []byte
	plan.PlanningMS = math.Inf(1)
	for run := 0; run < 3; run++ {
		if err = conn.QueryRow(ctx, query, args...).Scan(&raw); err != nil {
			plan.Error = err.Error()
			return plan
		}
		var decoded []map[string]any
		if err = json.Unmarshal(raw, &decoded); err != nil || len(decoded) == 0 {
			plan.Error = fmt.Sprint("decode plan: ", err)
			return plan
		}
		planning, _ := decoded[0]["Planning Time"].(float64)
		if run == 0 {
			plan.BuildPlanningMS = planning
			continue
		}
		// A statistics refresh of any month invalidates the reused plan; the
		// steady state is the cheaper of two reuses.
		if planning > plan.PlanningMS {
			continue
		}
		plan.PlanningMS = planning
		plan.ExecutionMS, _ = decoded[0]["Execution Time"].(float64)
		root, _ := decoded[0]["Plan"].(map[string]any)
		plan.TopNode, _ = root["Node Type"].(string)
		hit, _ := root["Shared Hit Blocks"].(float64)
		read, _ := root["Shared Read Blocks"].(float64)
		plan.SharedBlocks = hit + read
		planned, executed := map[string]bool{}, map[string]bool{}
		decadePlanWalk(&plan, root, planned, executed)
		plan.PartitionsPlanned, plan.PartitionsExecuted = len(planned), len(executed)
	}
	plan.Plan = raw
	return plan
}

func (h *decadeHarness) partitions() int {
	var count int
	if err := h.p.db.QueryRowContext(h.t.Context(), `SELECT COUNT(*) FROM pg_inherits WHERE inhparent='kubernetes_evidence_events'::regclass`).Scan(&count); err != nil {
		h.t.Fatal(err)
	}
	return count
}

func (h *decadeHarness) clearCounts() {
	h.p.evidenceQueryMu.Lock()
	h.p.evidenceQueries = nil
	h.p.evidenceQueryMu.Unlock()
}

// measureRead times one read path through the provider, recomputing counts
// for every first page, then explains each statement it ran.
func (h *decadeHarness) measureRead(stage string, read decadeRead, reps int) decadeReadResult {
	h.t.Helper()
	result := decadeReadResult{Stage: stage, Path: read.Name, WholeHistory: read.WholeHistory, Observation: read.Observation, Scopes: max(1, len(read.Scope.ReadOrgs)), Partitions: h.partitions()}
	run := func(q EvidenceQuery) (EvidencePage, error) {
		if read.Checkpoint {
			_, err := h.p.EvidenceCheckpoint(EvidenceReadContext(h.t.Context()), read.Scope)
			return EvidencePage{}, err
		}
		if q.Cursor == "" {
			h.clearCounts()
		}
		return h.p.QueryEvidence(h.t.Context(), read.Scope, q)
	}
	q := read.Query
	for depth := 1; depth < read.Depth; depth++ {
		page, err := run(q)
		if err != nil || page.Next == "" {
			result.Error = fmt.Sprintf("page %d has no continuation: %v", depth, err)
			h.t.Errorf("READ stage=%s path=%q: %s", stage, read.Name, result.Error)
			return result
		}
		q.Cursor = page.Next
	}
	for i := -2; i < reps; i++ {
		started := time.Now()
		page, err := run(q)
		elapsed := float64(time.Since(started).Microseconds()) / 1000
		if err != nil {
			result.Error = err.Error()
			h.t.Errorf("READ stage=%s path=%q: %v", stage, read.Name, err)
			return result
		}
		if i >= 0 {
			result.Samples = append(result.Samples, elapsed)
		}
		result.Rows, result.Total = len(page.Items), page.Total
	}
	result.MedianMS, result.MaxMS = decadePercentile(result.Samples, .5), decadePercentile(result.Samples, 1)
	h.trace.mu.Lock()
	h.trace.queries = nil
	h.trace.mu.Unlock()
	h.trace.enabled.Store(true)
	_, _ = run(q)
	h.trace.enabled.Store(false)
	h.trace.mu.Lock()
	statements := append([]scaleStatement(nil), h.trace.queries...)
	h.trace.mu.Unlock()
	for _, statement := range statements {
		if !strings.Contains(statement.SQL, "kubernetes_evidence") && !strings.Contains(statement.SQL, "evidence_event_month_") {
			continue
		}
		plan := h.explain(statement)
		result.PlanningMS += plan.PlanningMS
		result.BuildPlanningMS += plan.BuildPlanningMS
		result.ExecutionMS += plan.ExecutionMS
		result.Statements = append(result.Statements, plan)
	}
	return result
}

// decadeConcurrency is the result of many readers at once while captures are
// saved and the newest months' statistics are refreshed every few seconds,
// as autovacuum does on a busy cluster.
type decadeConcurrency struct {
	Stage                 string
	Readers, Connections  int
	Seconds               float64
	Analyzes              int
	Reads, Errors         int
	P50MS, P99MS, MaxMS   float64
	AfterAnalyzeReads     int
	AfterAnalyzeP99MS     float64
	AfterAnalyzeMaxMS     float64
	Saves, SaveErrors     int
	SaveP50MS, SaveP99MS  float64
	SaveMaxMS             float64
	SaveAfterAnalyzeMaxMS float64
	// SlowReads and SlowAfterAnalyze name the reads over 250 ms.
	SlowReads        []string `json:",omitempty"`
	SlowAfterAnalyze []string `json:",omitempty"`
	ErrorSamples     []string `json:",omitempty"`
}

// measureConcurrency runs 16 readers over the first pages of every ranged
// and all-time read for 40 seconds through the store's default history
// pool, while one writer saves captures to another scope and the newest
// journal and observation months are analyzed every four seconds (autovacuum
// stays on). Every read that starts after an ANALYZE, before the reader's next
// one, is the first after it and is reported apart.
func (h *decadeHarness) measureConcurrency(stage string) decadeConcurrency {
	h.t.Helper()
	const readers, duration, every = 16, 40 * time.Second, 4 * time.Second
	result := decadeConcurrency{Stage: stage, Readers: readers, Connections: 8, Seconds: duration.Seconds()}
	reads := []decadeRead{}
	for _, read := range h.reads() {
		if read.Depth == 1 && !read.Checkpoint {
			reads = append(reads, read)
		}
	}
	newest := h.monthStart(decadeMonths - 1).Format("200601")
	var mu sync.Mutex
	analyzed := []time.Time{}
	type sample struct {
		started time.Time
		ms      float64
		err     error
		path    string
	}
	readSamples, saveSamples := []sample{}, []sample{}
	deadline := time.Now().Add(duration)
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for time.Now().Before(deadline) {
			time.Sleep(every)
			for _, name := range []string{"evidence_event_month_" + newest, "evidence_observation_month_" + newest} {
				if _, err := h.p.db.ExecContext(context.Background(), "ANALYZE "+pgx.Identifier{name}.Sanitize()); err != nil {
					h.t.Errorf("ANALYZE %s: %v", name, err)
				}
			}
			mu.Lock()
			analyzed = append(analyzed, time.Now())
			mu.Unlock()
		}
	}()
	wait.Add(1)
	go func() {
		defer wait.Done()
		s := h.scope("staging")
		at := h.now.Add(-time.Minute).Truncate(time.Microsecond)
		for round := 0; time.Now().Before(deadline); round++ {
			records := make([]EvidenceRecord, 0, 400)
			for i := 0; i < 200; i++ {
				records = append(records, h.decadeChange(fmt.Sprintf("change:concurrent:%s:%d", stage, i), strconv.Itoa(round), at))
				records = append(records, h.decadeChange(fmt.Sprintf("change:concurrent-new:%s:%d:%d", stage, round, i), "new", at))
			}
			started := time.Now()
			cp, err := h.p.EvidenceCheckpoint(context.Background(), s.Scope)
			if err == nil {
				when := cp.At.Add(time.Second)
				if when.Before(h.now) {
					when = h.now
				}
				err = h.p.CommitEvidence(context.Background(), s.Scope, EvidenceBatch{ID: fmt.Sprintf("concurrent-%s-%d", stage, round), Expected: cp.ID, Generation: cp.Generation, At: when, Epoch: "stable", Records: records})
			}
			mu.Lock()
			saveSamples = append(saveSamples, sample{started, float64(time.Since(started).Microseconds()) / 1000, err, "save"})
			mu.Unlock()
		}
	}()
	for reader := 0; reader < readers; reader++ {
		wait.Add(1)
		go func(reader int) {
			defer wait.Done()
			for i := reader; time.Now().Before(deadline); i++ {
				read := reads[i%len(reads)]
				started := time.Now()
				_, err := h.p.QueryEvidence(context.Background(), read.Scope, read.Query)
				mu.Lock()
				readSamples = append(readSamples, sample{started, float64(time.Since(started).Microseconds()) / 1000, err, read.Name})
				mu.Unlock()
			}
		}(reader)
	}
	wait.Wait()
	result.Analyzes = len(analyzed)
	// A read is the first after an ANALYZE when it is the earliest read to
	// start after it; with 16 readers, the first 16 reads after each.
	firstAfter := map[int]bool{}
	sort.Slice(readSamples, func(i, j int) bool { return readSamples[i].started.Before(readSamples[j].started) })
	for _, when := range analyzed {
		taken := 0
		for i, s := range readSamples {
			if s.started.After(when) && taken < readers {
				firstAfter[i] = true
				taken++
			}
		}
	}
	all, after := []float64{}, []float64{}
	for i, s := range readSamples {
		result.Reads++
		if s.err != nil {
			result.Errors++
			if len(result.ErrorSamples) < 5 {
				result.ErrorSamples = append(result.ErrorSamples, s.err.Error())
			}
		}
		all = append(all, s.ms)
		if firstAfter[i] {
			after = append(after, s.ms)
			if s.ms > 250 {
				result.SlowAfterAnalyze = append(result.SlowAfterAnalyze, fmt.Sprintf("%s %.0fms", s.path, s.ms))
			}
		}
		if s.ms > 250 && len(result.SlowReads) < 20 {
			result.SlowReads = append(result.SlowReads, fmt.Sprintf("%s %.0fms", s.path, s.ms))
		}
	}
	result.P50MS, result.P99MS, result.MaxMS = decadePercentile(all, .5), decadePercentile(all, .99), decadePercentile(all, 1)
	result.AfterAnalyzeReads, result.AfterAnalyzeP99MS, result.AfterAnalyzeMaxMS = len(after), decadePercentile(after, .99), decadePercentile(after, 1)
	saves := []float64{}
	for _, s := range saveSamples {
		result.Saves++
		if s.err != nil {
			result.SaveErrors++
			if len(result.ErrorSamples) < 8 {
				result.ErrorSamples = append(result.ErrorSamples, "save: "+s.err.Error())
			}
		}
		saves = append(saves, s.ms)
		for _, when := range analyzed {
			if s.started.After(when) && s.started.Sub(when) < every && s.ms > result.SaveAfterAnalyzeMaxMS {
				result.SaveAfterAnalyzeMaxMS = s.ms
			}
		}
	}
	result.SaveP50MS, result.SaveP99MS, result.SaveMaxMS = decadePercentile(saves, .5), decadePercentile(saves, .99), decadePercentile(saves, 1)
	return result
}

type decadeSaveResult struct {
	Stage, Case        string
	Partitions         int
	Heads              int64
	Samples            []float64
	P50MS, P99MS       float64
	FinalSamples       []float64
	FinalP50MS         float64
	FinalP99MS         float64
	Inserted           int
	WritePlanProblems  []string `json:",omitempty"`
	WritePlanJournal   int      `json:",omitempty"`
	WritePlanPartition []string `json:",omitempty"`
}

// decadeChange is a change record for an event of the current month.
func (h *decadeHarness) decadeChange(id, value string, at time.Time) EvidenceRecord {
	payload, _ := json.Marshal(map[string]any{"cluster": "decade", "kind": "Deployment", "name": id, "namespace": "ns-1", "type": "updated", "value": value, "at": at})
	return EvidenceRecord{ID: id, Type: "change", At: at, ObservedAt: at, Namespace: "ns-1", Name: id, Kind: "Deployment", AuthorizationKind: "Deployment", UID: id, RootUID: id, Epoch: "stable", Payload: payload}
}

// measureSaves times real saves of a 500-record capture through the full
// provider path: parts, the final transaction with its issue read models,
// partition preparation and the checkpoint. The saves go to the staging
// scope, so the main scope the reads measure stays as seeded. Changed
// captures supersede events seeded this month, which have heads; new captures
// carry events never seen before, which have none; unchanged captures resend
// the last changed capture. Every capture also carries the scope's 500 open
// issues: a changed capture resolves 100, opens 100 and changes half of the
// rest, which writes new occurrence versions in the final transaction.
func (h *decadeHarness) measureSaves(stage string, reps int) []decadeSaveResult {
	h.t.Helper()
	s := h.scope("staging")
	const batch, openIssues, churn = 500, 500, 100
	partitions := h.partitions()
	var heads int64
	if err := h.p.db.QueryRowContext(h.t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_event_heads`).Scan(&heads); err != nil {
		h.t.Fatal(err)
	}
	results := map[string]*decadeSaveResult{}
	for _, name := range []string{"changed", "new", "unchanged"} {
		results[name] = &decadeSaveResult{Stage: stage, Case: name, Partitions: partitions, Heads: heads}
	}
	rows := func() int {
		var n int
		if err := h.p.db.QueryRow(`SELECT COUNT(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_at>=$2`, s.Key, h.current).Scan(&n); err != nil {
			h.t.Fatal(err)
		}
		return n
	}
	// Seeded change events of the current month that are current versions.
	existing := make([]string, 0, batch)
	for n := 1; len(existing) < batch && n < s.Rows; n++ {
		if b := n % 100; b >= 1 && b < 60 && b%10 != 0 {
			existing = append(existing, fmt.Sprintf("change:%d:%d", decadeMonths-1, n))
		}
	}
	for i := 0; len(existing) < batch; i++ {
		existing = append(existing, fmt.Sprintf("change:steady:%d", i))
	}
	at := h.now.Add(-time.Minute).Truncate(time.Microsecond)
	firstIssue, round := 0, 0
	issues := func(when time.Time) []EvidenceIssue {
		window := make([]EvidenceIssue, 0, openIssues)
		for index := firstIssue; index < firstIssue+openIssues; index++ {
			restarts := 1
			if (index-firstIssue)%2 == 0 {
				restarts = round + 1
			}
			issue := finalSaveIssue(index, restarts, when)
			issue.Key = "issue:" + stage + ":" + issue.UID
			window = append(window, issue)
		}
		return window
	}
	commit := func(name string, records []EvidenceRecord) {
		h.t.Helper()
		before := rows()
		cp, err := h.p.EvidenceCheckpoint(h.t.Context(), s.Scope)
		if err != nil {
			h.t.Fatal(err)
		}
		when := cp.At.Add(time.Second)
		if when.Before(h.now) {
			when = h.now
		}
		var final time.Duration
		b := EvidenceBatch{ID: fmt.Sprintf("%s-%s-%d", stage, name, time.Now().UnixNano()), Expected: cp.ID, Generation: cp.Generation, At: when, CoveredThrough: when, Epoch: "stable", Records: records, Issues: issues(when)}
		b.Fence = func(ctx context.Context, transaction func() error) error {
			started := time.Now()
			err := transaction()
			final = time.Since(started)
			return err
		}
		started := time.Now()
		if err = h.p.CommitEvidence(h.t.Context(), s.Scope, b); err != nil {
			h.t.Fatalf("SAVE stage=%s case=%s: %v", stage, name, err)
		}
		if name != "" {
			results[name].Samples = append(results[name].Samples, float64(time.Since(started).Microseconds())/1000)
			results[name].FinalSamples = append(results[name].FinalSamples, float64(final.Microseconds())/1000)
			results[name].Inserted += rows() - before
		}
	}
	// The open issues exist before the first measured save.
	commit("", nil)
	var last []EvidenceRecord
	for rep := 0; rep < reps; rep++ {
		round++
		firstIssue += churn
		changed := make([]EvidenceRecord, batch)
		for i, id := range existing {
			changed[i] = h.decadeChange(id, fmt.Sprintf("%s-%d", stage, rep), at)
		}
		commit("changed", changed)
		last = changed
		fresh := make([]EvidenceRecord, batch)
		for i := range fresh {
			fresh[i] = h.decadeChange(fmt.Sprintf("change:new:%s:%d:%d", stage, rep, i), "new", at)
		}
		commit("new", fresh)
		commit("unchanged", last)
	}
	// The write statements of a new-event and a changed-event capture,
	// explained in rolled-back transactions with the plan production uses for
	// a scope whose heads are complete: planned for its values, its updates
	// bounded to the months of the events' current versions. A new event
	// must read no journal partition; a change must supersede each old row by
	// its full key in one partition.
	explainWrite := func(name string, records []EvidenceRecord, superseded int) {
		statement := h.captureWrite(s.Key, records)
		if strings.Contains(statement.SQL, "ORDER BY p.sequence DESC") {
			h.t.Errorf("SAVE stage=%s case=%s: the scope's heads are not complete", stage, name)
		}
		raw := headsExplain(h.t, h.dsn, statement, "custom")
		nodes, err := headsPlanNodes(raw)
		if err != nil {
			h.t.Fatal(err)
		}
		read := map[string]bool{}
		for _, n := range nodes {
			if n.Loops > 0 && strings.Contains(n.NodeType, "Scan") && strings.HasPrefix(n.Relation, "evidence_event_month_") && !strings.HasPrefix(n.CTE, "superseded") && !strings.HasPrefix(n.CTE, "refreshed") {
				read[n.Relation] = true
			}
		}
		result := results[name]
		if name == "new" {
			result.WritePlanProblems = headsNewEventProblems(nodes)
		} else {
			result.WritePlanProblems = headsPlanVerdict(nodes, superseded)
		}
		result.WritePlanJournal = len(read)
		for relation := range read {
			result.WritePlanPartition = append(result.WritePlanPartition, relation)
		}
		sort.Strings(result.WritePlanPartition)
		if h.results != "" {
			_ = os.WriteFile(filepath.Join(h.results, "decade-write-plan-"+name+"-"+stage+".json"), raw, 0600)
		}
	}
	fresh := make([]EvidenceRecord, batch)
	for i := range fresh {
		fresh[i] = h.decadeChange(fmt.Sprintf("change:probe:%s:%d", stage, i), "new", at)
	}
	explainWrite("new", fresh, 0)
	changed := make([]EvidenceRecord, batch)
	for i, id := range existing {
		changed[i] = h.decadeChange(id, stage+"-explained", at)
	}
	explainWrite("changed", changed, batch)
	out := []decadeSaveResult{}
	for _, name := range []string{"changed", "new", "unchanged"} {
		r := results[name]
		r.P50MS, r.P99MS = decadePercentile(r.Samples, .5), decadePercentile(r.Samples, .99)
		r.FinalP50MS, r.FinalP99MS = decadePercentile(r.FinalSamples, .5), decadePercentile(r.FinalSamples, .99)
		out = append(out, *r)
	}
	return out
}

func (h *decadeHarness) captureWrite(key string, records []EvidenceRecord) scaleStatement {
	h.t.Helper()
	cfg, err := pgx.ParseConfig(h.dsn)
	if err != nil {
		h.t.Fatal(err)
	}
	trace := &scaleQueryTrace{}
	cfg.Tracer = trace
	traced := stdlib.OpenDB(*cfg)
	defer traced.Close()
	tx, err := traced.BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer tx.Rollback()
	trace.enabled.Store(true)
	if _, err = writeEvidenceEvents(h.t.Context(), tx, key, records, 5*time.Minute); err != nil {
		h.t.Fatal(err)
	}
	trace.enabled.Store(false)
	for _, statement := range trace.queries {
		if strings.Contains(statement.SQL, "kubernetes_evidence_event_heads") && strings.Contains(statement.SQL, "WITH incoming") {
			return statement
		}
	}
	h.t.Fatal("write statement not captured")
	return scaleStatement{}
}

type decadeMaintenanceResult struct {
	Stage, Step string
	Partitions  int
	MS          float64
	Detail      string `json:",omitempty"`
}

// measureMaintenance times the scope probe and a retention drop of one
// sacrificial month seeded just before the stage's oldest month, then a
// thinning pass, and one heads backfill batch on a scope marked as holding
// rows from before heads since its oldest month.
func (h *decadeHarness) measureMaintenance(stage string, oldest int) []decadeMaintenanceResult {
	h.t.Helper()
	results := []decadeMaintenanceResult{}
	timed := func(step string, run func() string) {
		h.t.Helper()
		started := time.Now()
		detail := run()
		results = append(results, decadeMaintenanceResult{Stage: stage, Step: step, Partitions: h.partitions(), MS: float64(time.Since(started).Microseconds()) / 1000, Detail: detail})
	}
	partition := "evidence_event_month_" + h.monthStart(oldest).Format("200601")
	timed("scope probe of the oldest month", func() string {
		tx, err := h.p.db.BeginTx(h.t.Context(), nil)
		if err != nil {
			h.t.Fatal(err)
		}
		defer tx.Rollback()
		scopes, err := evidenceScopesIn(h.t.Context(), tx, partition)
		if err != nil {
			h.t.Fatal(err)
		}
		return fmt.Sprintf("%d scopes in %s", len(scopes), partition)
	})
	sacrificial := oldest - 1
	main := h.scope("main")
	h.ensureMonth(sacrificial)
	h.exec(decadeJournalSQL, main.Key, main.Base+int64(sacrificial)*decadeSequenceSlot, h.monthStart(sacrificial), h.monthEnd(sacrificial), main.Rows, sacrificial, h.now)
	h.exec(`INSERT INTO kubernetes_evidence_event_heads(scope_key,event_id,sequence,event_at,event_version,priority,observed_at)
 SELECT scope_key,event_id,sequence,event_at,event_version,priority,observed_at FROM kubernetes_evidence_events
 WHERE scope_key=$1 AND event_at>=$2 AND event_at<$3 AND superseded_seq IS NULL`, main.Key, h.monthStart(sacrificial), h.monthStart(oldest))
	h.exec(decadeOccurrenceSQL, main.Key, sacrificial, h.monthStart(sacrificial), h.monthEnd(sacrificial).Add(-8*time.Hour), main.Occurrences, 0, h.monthStart(0), h.now.Add(-time.Hour), "occ:sacrificial:"+stage+":")
	for _, name := range []string{partition[:len(partition)-6] + h.monthStart(sacrificial).Format("200601"), "evidence_observation_month_" + h.monthStart(sacrificial).Format("200601")} {
		h.exec("VACUUM (ANALYZE) " + pgx.Identifier{name}.Sanitize())
	}
	// Retention reaches into the sacrificial month only.
	h.p.evidenceRetention = h.now.Sub(h.monthStart(oldest)) - 24*time.Hour
	defer func() { h.p.evidenceRetention = 0 }()
	for _, step := range []string{"retention drop of the expired journal month", "retention drop of the expired observation month"} {
		timed(step, func() string {
			more, err := h.p.compactEvidenceBatch(h.t.Context())
			if err != nil {
				h.t.Errorf("MAINTENANCE stage=%s step=%q: %v", stage, step, err)
				return "error: " + err.Error()
			}
			return fmt.Sprintf("more=%v", more)
		})
	}
	timed("hourly thinning pass with nothing left to thin", func() string {
		more, err := h.p.compactEvidenceBatch(h.t.Context())
		if err != nil {
			h.t.Errorf("MAINTENANCE stage=%s thinning: %v", stage, err)
			return "error: " + err.Error()
		}
		return fmt.Sprintf("more=%v", more)
	})
	h.p.evidenceRetention = 0
	staging := h.scope("staging")
	timed("heads backfill batch of a scope from before heads", func() string {
		h.exec(`UPDATE kubernetes_evidence_scopes SET heads_backfill=$2 WHERE scope_key=$1`, staging.Key, staging.Base+int64(oldest)*decadeSequenceSlot)
		defer h.exec(`UPDATE kubernetes_evidence_scopes SET heads_backfill=NULL WHERE scope_key=$1`, staging.Key)
		limits, attempts := evidenceBackgroundBackfill, 0
		for {
			attempts++
			ctx, cancel := context.WithTimeout(h.t.Context(), evidenceBackgroundBackfill.batchTimeout)
			more, err := h.p.backfillEvidenceHeads(ctx, limits.events)
			cancel()
			if err == nil {
				return fmt.Sprintf("more=%v events=%d attempts=%d", more, limits.events, attempts)
			}
			if attempts >= 8 {
				h.t.Errorf("MAINTENANCE stage=%s heads backfill: %v", stage, err)
				return "error: " + err.Error()
			}
			limits = nextEvidenceBackfillLimits(limits, err)
		}
	})
	return results
}

type decadeStorage struct {
	Stage      string
	Partitions int
	Tables     map[string]int64
	Rows       map[string]int64
	Database   int64
	HeapAlloc  uint64
}

func (h *decadeHarness) storage(stage string) decadeStorage {
	h.t.Helper()
	result := decadeStorage{Stage: stage, Partitions: h.partitions(), Tables: map[string]int64{}, Rows: map[string]int64{}}
	for _, table := range []string{"kubernetes_evidence_events", "kubernetes_evidence_occurrence_observations", "kubernetes_evidence_event_heads", "kubernetes_evidence_issues", "kubernetes_evidence_occurrence_versions", "kubernetes_evidence_occurrence_read_versions"} {
		var size, rows int64
		if err := h.p.db.QueryRow(`SELECT COALESCE(SUM(pg_total_relation_size(c.oid)),0),COALESCE(SUM(c.reltuples::bigint) FILTER (WHERE c.relkind='r'),0)
 FROM pg_class c WHERE c.oid=$1::regclass OR c.oid IN(SELECT inhrelid FROM pg_inherits WHERE inhparent=$1::regclass)`, table).Scan(&size, &rows); err != nil {
			h.t.Fatal(err)
		}
		result.Tables[table], result.Rows[table] = size, rows
	}
	if err := h.p.db.QueryRow(`SELECT pg_database_size(current_database())`).Scan(&result.Database); err != nil {
		h.t.Fatal(err)
	}
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	result.HeapAlloc = stats.HeapAlloc
	return result
}

type decadeReport struct {
	Volume      decadeVolume
	Reads       []decadeReadResult
	Saves       []decadeSaveResult
	Maintenance []decadeMaintenanceResult
	Concurrency []decadeConcurrency
	Storage     []decadeStorage
	Verdicts    []string
}

// TestPostgresEvidenceDecadeGate proves history saves and reads keep their
// cost as a deployment ages from one year to ten. It seeds twelve monthly
// partitions of steady history in several scopes, measures every save, read
// and maintenance path, prepends the other 108 months and measures again.
// Pass: a read bounded to a time range at year ten within twice its year-one
// time, or within 2 ms plus 0.025 ms per added month (a reused plan starts one
// index scan per month for per-row month lookups and the gap read); an
// all-time read adding at most 0.25 ms per added month and read scope; a
// crash read ordered by observation time adding at most 0.06 ms per added
// month; every page read under 250 ms; planning of a ranged read within twice its
// year-one value (or within 2 ms) and first planning on a new connection
// under 1.5 s; no sequential scan of a journal, observation, heads or
// read-model table; saves and their final transaction within twice their
// year-one time (or within 5 ms), p99 under 5 s; the reused write plan of a
// complete scope reading no journal partition for new events and superseding
// a change by its full key; recurring maintenance within twice its year-one time (or
// within 20 ms), and the one-time heads backfill committing in batches that
// fit the statement timeout; 16 readers at once over the store's default
// history pool for 40 s, with captures saved and the newest months analyzed
// every 4 s, with p99 at most 250 ms over all reads and over the first reads
// after each ANALYZE, and no failed read or save. The default volume
// seeds about 10 GB in a qa_ database of its own and runs only with
// EVIDENCE_DECADE_GATE=1 outside short mode.
func TestPostgresEvidenceDecadeGate(t *testing.T) {
	if testing.Short() || os.Getenv("EVIDENCE_DECADE_GATE") != "1" {
		t.Skip("set EVIDENCE_DECADE_GATE=1 outside short mode; the gate seeds a decade of history in its own qa_ database")
	}
	if os.Getenv("TEST_POSTGRES_DSN") == "" {
		t.Skip("isolated Postgres required")
	}
	dsn := snapshotOwnDatabase(t)
	p, _ := headsPartitionedProvider(t)
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
	// The history pool as the store opens it by default.
	p.historyDB.SetMaxOpenConns(8)
	p.historyDB.SetMaxIdleConns(8)
	now := time.Now().UTC().Truncate(time.Microsecond)
	h := &decadeHarness{t: t, p: p, dsn: dsn, now: now, current: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC),
		volume: decadeVolumeFromEnv(t), trace: trace, seeded: map[string]bool{}, results: os.Getenv("HARNESS_RESULTS_DIR")}
	v := h.volume
	h.scopes = []*decadeScope{
		{Name: "main", Scope: EvidenceScope{OrgID: "default", ClusterID: "decade-prod", ClusterUID: "physical-prod"}, Rows: v.MonthRows, Occurrences: v.Occurrences, From: 0, To: decadeMonths - 1},
		{Name: "staging", Scope: EvidenceScope{OrgID: "default", ClusterID: "decade-staging", ClusterUID: "physical-staging"}, Rows: v.MonthRows / 4, From: 0, To: decadeMonths - 1},
		{Name: "enterprise", Scope: EvidenceScope{OrgID: "acme", ClusterID: "decade-prod", ClusterUID: "physical-prod"}, Rows: v.MonthRows / 10, Occurrences: v.Occurrences / 10, From: 0, To: decadeMonths - 1},
		{Name: "retired", Scope: EvidenceScope{OrgID: "default", ClusterID: "decade-retired", ClusterUID: "physical-retired"}, Rows: v.MonthRows / 4, From: 0, To: 35},
	}
	var base int64
	if err = p.db.QueryRow(`SELECT nextval('kubernetes_evidence_events_sequence_seq')`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	for i, s := range h.scopes {
		s.Key, _ = evidenceScopeKey(s.Scope)
		s.Base = base + 1 + decadeSequenceSlot + int64(i)*int64(decadeMonths+2)*decadeSequenceSlot
		headsCommit(t, p, s.Scope, "create", now.Add(-time.Hour))
	}
	h.exec(`SELECT setval('kubernetes_evidence_events_sequence_seq',$1)`, base+int64(len(h.scopes)+1)*int64(decadeMonths+2)*decadeSequenceSlot)
	report := decadeReport{Volume: v}
	persist := func() {
		if h.results != "" {
			scaleWriteJSON(t, filepath.Join(h.results, "decade-gate.json"), report)
		}
	}
	defer persist()
	stage := func(name string, from int) {
		started := time.Now()
		for m := decadeMonths - 1; m >= from; m-- {
			h.ensureMonth(m)
			for _, s := range h.scopes {
				h.seedMonth(s, m)
			}
		}
		h.seedChronic(h.scope("main"), from, name)
		h.finishSeed()
		t.Logf("SEED stage=%s months=%d..%d partitions=%d seconds=%.0f", name, from, decadeMonths-1, h.partitions(), time.Since(started).Seconds())
		for _, read := range h.reads() {
			result := h.measureRead(name, read, 7)
			t.Logf("READ stage=%s path=%q partitions=%d median_ms=%.1f max_ms=%.1f planning_ms=%.1f first_planning_ms=%.1f execution_ms=%.1f rows=%d total=%d", name, read.Name, result.Partitions, result.MedianMS, result.MaxMS, result.PlanningMS, result.BuildPlanningMS, result.ExecutionMS, result.Rows, result.Total)
			report.Reads = append(report.Reads, result)
		}
		persist()
		for _, save := range h.measureSaves(name, 15) {
			t.Logf("SAVE stage=%s case=%s partitions=%d heads=%d p50_ms=%.1f p99_ms=%.1f final_p50_ms=%.1f final_p99_ms=%.1f inserted=%d new_event_journal_partitions_read=%d", name, save.Case, save.Partitions, save.Heads, save.P50MS, save.P99MS, save.FinalP50MS, save.FinalP99MS, save.Inserted, save.WritePlanJournal)
			report.Saves = append(report.Saves, save)
		}
		persist()
		for _, step := range h.measureMaintenance(name, from) {
			t.Logf("MAINTENANCE stage=%s step=%q partitions=%d ms=%.1f %s", name, step.Step, step.Partitions, step.MS, step.Detail)
			report.Maintenance = append(report.Maintenance, step)
		}
		concurrency := h.measureConcurrency(name)
		t.Logf("CONCURRENCY stage=%s readers=%d connections=%d seconds=%.0f analyzes=%d reads=%d errors=%d p50_ms=%.1f p99_ms=%.1f max_ms=%.1f after_analyze_reads=%d after_analyze_p99_ms=%.1f after_analyze_max_ms=%.1f saves=%d save_errors=%d save_p50_ms=%.1f save_p99_ms=%.1f save_max_ms=%.1f save_after_analyze_max_ms=%.1f errors=%v slow_after_analyze=%v slow=%v",
			name, concurrency.Readers, concurrency.Connections, concurrency.Seconds, concurrency.Analyzes, concurrency.Reads, concurrency.Errors, concurrency.P50MS, concurrency.P99MS, concurrency.MaxMS,
			concurrency.AfterAnalyzeReads, concurrency.AfterAnalyzeP99MS, concurrency.AfterAnalyzeMaxMS, concurrency.Saves, concurrency.SaveErrors, concurrency.SaveP50MS, concurrency.SaveP99MS, concurrency.SaveMaxMS, concurrency.SaveAfterAnalyzeMaxMS, concurrency.ErrorSamples, concurrency.SlowAfterAnalyze, concurrency.SlowReads)
		report.Concurrency = append(report.Concurrency, concurrency)
		report.Storage = append(report.Storage, h.storage(name))
		persist()
	}
	stage("year1", decadeMonths-12)
	stage("year10", 0)
	report.Verdicts = decadeVerdicts(report)
	for _, verdict := range report.Verdicts {
		t.Error(verdict)
	}
}

// decadeStartupPerMonthMS is what one more month may add to a read bounded to
// a time range: a reused plan starts one index scan per month for a lookup
// whose month is known only from the row it is made for, and finds the row in
// one of them.
const decadeStartupPerMonthMS = 0.025

// decadeObservationPerMonthMS bounds what one more month may add to a crash
// read ordered by observation time. It is its own class: the journal's months
// follow event time, and a crash status is refreshed in place in the month it
// began, so such a read reaches back by the scope's longest observation delay
// and, for an all-time range, into every month. Its page, its count and its
// separate gap read each start an index scan in those months; 0.06 ms per
// month is that cost with headroom for the gap read, which also visits the
// months a gap stored before its end may sit in.
const decadeObservationPerMonthMS = 0.06

// decadePerMonthMS bounds what one more month may add to a whole-history read
// of one scope: a bounded merge over one index descent per month and branch,
// never a scan of a month.
const decadePerMonthMS = 0.25

func decadeOneTimeStep(name string) bool {
	return strings.HasPrefix(name, "heads backfill")
}

// decadeWithin is the growth rule: year ten at most twice year one, or
// within an absolute allowance for paths too fast to time reliably.
func decadeWithin(year1, year10, allowance float64) bool {
	return year10 <= 2*year1 || year10-year1 <= allowance
}

func decadeVerdicts(report decadeReport) []string {
	var problems []string
	reads := map[string]map[string]decadeReadResult{}
	for _, r := range report.Reads {
		if reads[r.Path] == nil {
			reads[r.Path] = map[string]decadeReadResult{}
		}
		reads[r.Path][r.Stage] = r
		if r.Error != "" {
			problems = append(problems, fmt.Sprintf("read %q at %s failed: %s", r.Path, r.Stage, r.Error))
		}
		if r.MaxMS >= 250 {
			problems = append(problems, fmt.Sprintf("read %q at %s took %.1f ms (limit 250)", r.Path, r.Stage, r.MaxMS))
		}
		for _, s := range r.Statements {
			if len(s.HotSeqScans) > 0 {
				problems = append(problems, fmt.Sprintf("read %q at %s scans %v sequentially", r.Path, r.Stage, s.HotSeqScans))
			}
			if s.Error != "" {
				problems = append(problems, fmt.Sprintf("read %q at %s could not be explained: %s", r.Path, r.Stage, s.Error))
			}
		}
	}
	paths := make([]string, 0, len(reads))
	for path := range reads {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		y1, y10 := reads[path]["year1"], reads[path]["year10"]
		added := float64(y10.Partitions - y1.Partitions)
		perMonth := (y10.MedianMS - y1.MedianMS) / max(1, added)
		switch {
		case y10.Observation:
			if bound := decadeObservationPerMonthMS * float64(y10.Scopes); perMonth > bound {
				problems = append(problems, fmt.Sprintf("crash read by observation %q grew by %.3f ms per added month (bound %.2f): %.1f ms at year one, %.1f ms at year ten", path, perMonth, bound, y1.MedianMS, y10.MedianMS))
			}
		case y10.WholeHistory:
			if bound := decadePerMonthMS * float64(y10.Scopes); perMonth > bound {
				problems = append(problems, fmt.Sprintf("all-time read %q grew by %.3f ms per added month (bound %.2f): %.1f ms at year one, %.1f ms at year ten", path, perMonth, bound, y1.MedianMS, y10.MedianMS))
			}
		default:
			if !decadeWithin(y1.MedianMS, y10.MedianMS, 2+decadeStartupPerMonthMS*added) {
				problems = append(problems, fmt.Sprintf("ranged read %q grew from %.1f ms at year one to %.1f ms at year ten (bound twice, or 2 ms + %.3f ms per added month)", path, y1.MedianMS, y10.MedianMS, decadeStartupPerMonthMS))
			}
		}
		if y10.BuildPlanningMS > 1500 {
			problems = append(problems, fmt.Sprintf("read %q takes %.0f ms to plan on a new connection at year ten, too close to the 2 s statement timeout", path, y10.BuildPlanningMS))
		}
		// Each statement is planned for the months it reads, so a ranged read
		// plans the same months at year ten as at year one; an all-time or
		// observation-ordered read plans each month it walks, which the
		// per-month bounds above already include.
		if !y10.WholeHistory && !y10.Observation && !decadeWithin(y1.PlanningMS, y10.PlanningMS, 2) {
			problems = append(problems, fmt.Sprintf("ranged read %q planning grew from %.1f ms at year one to %.1f ms at year ten", path, y1.PlanningMS, y10.PlanningMS))
		}
	}
	saves := map[string]map[string]decadeSaveResult{}
	for _, s := range report.Saves {
		if saves[s.Case] == nil {
			saves[s.Case] = map[string]decadeSaveResult{}
		}
		saves[s.Case][s.Stage] = s
		if s.P99MS > 5000 {
			problems = append(problems, fmt.Sprintf("save %s at %s p99 %.0f ms is not far inside the 20 s lease", s.Case, s.Stage, s.P99MS))
		}
	}
	for name, stages := range saves {
		y1, y10 := stages["year1"], stages["year10"]
		if !decadeWithin(y1.P50MS, y10.P50MS, 5) {
			problems = append(problems, fmt.Sprintf("save %s grew from %.1f ms at year one to %.1f ms at year ten", name, y1.P50MS, y10.P50MS))
		}
		if !decadeWithin(y1.FinalP50MS, y10.FinalP50MS, 5) {
			problems = append(problems, fmt.Sprintf("final save transaction of %s grew from %.1f ms at year one to %.1f ms at year ten", name, y1.FinalP50MS, y10.FinalP50MS))
		}
		for _, stage := range []decadeSaveResult{y1, y10} {
			if len(stage.WritePlanProblems) > 0 || stage.WritePlanJournal > 0 {
				problems = append(problems, fmt.Sprintf("save %s at %s: its reused write plan reads %d journal partitions outside the supersede: %v", name, stage.Stage, stage.WritePlanJournal, stage.WritePlanProblems))
			}
		}
	}
	for _, c := range report.Concurrency {
		if c.Errors > 0 || c.SaveErrors > 0 || c.P99MS > 250 || c.AfterAnalyzeP99MS > 250 || c.Analyzes == 0 {
			problems = append(problems, fmt.Sprintf("%d concurrent readers with saves and an ANALYZE every few seconds at %s: p99 %.1f ms, first after an ANALYZE p99 %.1f ms (limit 250), %d of %d reads and %d of %d saves failed, %d analyzes %v",
				c.Readers, c.Stage, c.P99MS, c.AfterAnalyzeP99MS, c.Errors, c.Reads, c.SaveErrors, c.Saves, c.Analyzes, c.ErrorSamples))
		}
	}
	steps := map[string]map[string]decadeMaintenanceResult{}
	for _, m := range report.Maintenance {
		if steps[m.Step] == nil {
			steps[m.Step] = map[string]decadeMaintenanceResult{}
		}
		steps[m.Step][m.Stage] = m
	}
	for name, stages := range steps {
		y1, y10 := stages["year1"], stages["year10"]
		if strings.HasPrefix(y10.Detail, "error") {
			problems = append(problems, fmt.Sprintf("maintenance %q failed at year ten: %s", name, y10.Detail))
			continue
		}
		// Giving heads to a scope from before heads runs once per upgrade and
		// reads a slice of the journal in sequence order from every month; it
		// is held to committing in batches that fit the statement timeout.
		if decadeOneTimeStep(name) {
			continue
		}
		if !decadeWithin(y1.MS, y10.MS, 20) {
			problems = append(problems, fmt.Sprintf("maintenance %q grew from %.1f ms at year one to %.1f ms at year ten", name, y1.MS, y10.MS))
		}
	}
	sort.Strings(problems)
	return problems
}
