package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"
)

// everyEvidenceStore opens the memory, file and Postgres evidence stores; the
// Postgres one skips without a test database.
func everyEvidenceStore() map[string]func(t *testing.T) EvidenceStore {
	return map[string]func(t *testing.T) EvidenceStore{
		"memory": func(t *testing.T) EvidenceStore { return NewMemory().(EvidenceStore) },
		"file": func(t *testing.T) EvidenceStore {
			provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			return provider.(EvidenceStore)
		},
		"postgres": func(t *testing.T) EvidenceStore {
			return worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
		},
	}
}

// commitTo saves one capture to store, after the scope's current checkpoint.
func commitTo(t *testing.T, store EvidenceStore, scope EvidenceScope, id string, at time.Time, issues map[string]string, edit func(*EvidenceBatch)) {
	t.Helper()
	cp, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	batch := issueCapture(t, id, cp.ID, cp.Generation, at, 1, issues)
	if edit != nil {
		edit(&batch)
	}
	if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
}

// issueRecordCounts counts the journal's issue records of the scope by type.
func issueRecordCounts(t *testing.T, store EvidenceStore, scope EvidenceScope, kind string, since, until time.Time) (map[string]int, map[string]int) {
	t.Helper()
	page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: kind, Since: since, Until: until, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	items := map[string]int{}
	for _, record := range page.Items {
		items[record.Type]++
	}
	return items, page.Counts
}

func TestPartialRootKeepsOnlyItsOwnIssuesOpenInEveryStore(t *testing.T) {
	for name, open := range everyEvidenceStore() {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			scope := EvidenceScope{OrgID: "default", ClusterID: "partial-root", ClusterUID: "physical"}
			start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			commitTo(t, store, scope, "c0", start, steadyIssues, nil)
			// Every issue is gone from the capture, but the api root's
			// evidence was partial: only its issue stays open.
			at := start.Add(time.Minute)
			commitTo(t, store, scope, "c1", at, map[string]string{}, func(batch *EvidenceBatch) {
				batch.CoveredThrough = at
				batch.HoldOpen = []string{"api-uid"}
			})
			items, _ := issueRecordCounts(t, store, scope, "issue", start.Add(-time.Hour), start.Add(time.Hour))
			if items["issue_resolved"] != len(steadyIssues)-1 {
				t.Fatalf("issue records %v after a capture holding the api root open, want the %d other issues resolved", items, len(steadyIssues)-1)
			}
			page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "issue", RootUID: "api-uid", Since: start.Add(-time.Hour), Until: start.Add(time.Hour), Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range page.Items {
				if record.Type != "issue_opened" {
					t.Fatalf("the held root has a %s record, want it still open", record.Type)
				}
			}
			// Once its evidence is complete again, it resolves too.
			at = at.Add(time.Minute)
			commitTo(t, store, scope, "c2", at, map[string]string{}, func(batch *EvidenceBatch) { batch.CoveredThrough = at })
			if items, _ = issueRecordCounts(t, store, scope, "issue", start.Add(-time.Hour), start.Add(time.Hour)); items["issue_resolved"] != len(steadyIssues) {
				t.Fatalf("issue records %v after a complete capture, want every issue resolved", items)
			}
		})
	}
}

// boundedState applies captures one after another to one in-memory state, as
// a store does, and reports what each save changed.
type boundedState struct {
	t     *testing.T
	state evidenceState
}

func newBoundedState(t *testing.T) *boundedState {
	return &boundedState{t: t, state: issueTable{}.load(EvidenceCheckpoint{})}
}

func (bounded *boundedState) apply(id string, at time.Time, issues map[string]string, edit func(*EvidenceBatch)) (records []EvidenceRecord, observations int) {
	bounded.t.Helper()
	batch := issueCapture(bounded.t, id, bounded.state.Checkpoint.ID, 0, at, 1, issues)
	if edit != nil {
		edit(&batch)
	}
	before := len(bounded.state.Observations)
	records, err := applyEvidence(&bounded.state, batch)
	if err != nil {
		bounded.t.Fatal(err)
	}
	return records, len(bounded.state.Observations) - before
}

func (bounded *boundedState) open() []string {
	keys := []string{}
	for key := range bounded.state.Active {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func recordTypes(records []EvidenceRecord) map[string]int {
	types := map[string]int{}
	for _, record := range records {
		types[record.Type]++
	}
	return types
}

func TestChangeLimitOpensFirstThenDueObservationsThenResolutions(t *testing.T) {
	bounded := newBoundedState(t)
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	stale := map[string]string{"s1": "low", "s2": "low", "s3": "low", "s4": "low", "s5": "low"}
	bounded.apply("c0", start, stale, nil)
	fresh := map[string]string{"n1": "high", "n2": "high", "n3": "high"}

	// Three openings and five resolvable issues, limit three: the openings
	// all land and every resolution waits; coverage is not recorded.
	records, _ := bounded.apply("c1", start.Add(10*time.Second), fresh, func(batch *EvidenceBatch) {
		batch.CoveredThrough, batch.ChangeLimit = start.Add(5*time.Second), 3
	})
	if types := recordTypes(records); types["issue_opened"] != 3 || types["issue_resolved"] != 0 {
		t.Fatalf("records %v, want the 3 openings before any resolution", types)
	}
	if !bounded.state.Checkpoint.CoveredThrough.IsZero() {
		t.Fatalf("coverage %s recorded while every resolution waits", bounded.state.Checkpoint.CoveredThrough)
	}

	// Nothing to open or change: two resolutions, oldest sighting first.
	records, _ = bounded.apply("c2", start.Add(20*time.Second), fresh, func(batch *EvidenceBatch) {
		batch.CoveredThrough, batch.ChangeLimit = start.Add(15*time.Second), 2
	})
	if types := recordTypes(records); types["issue_resolved"] != 2 || len(records) != 2 {
		t.Fatalf("records %v, want 2 resolutions", types)
	}
	if open := fmt.Sprint(bounded.open()); open != "[n1 n2 n3 s3 s4 s5]" {
		t.Fatalf("open issues %s, want s1 and s2 resolved first", open)
	}

	// The reported issues are due on the heartbeat: their observations come
	// before the remaining resolutions.
	at := start.Add(6 * time.Minute)
	records, observations := bounded.apply("c3", at, fresh, func(batch *EvidenceBatch) {
		batch.CoveredThrough, batch.ChangeLimit = at.Add(-time.Second), 4
	})
	if types := recordTypes(records); types["issue_resolved"] != 1 || observations != 4 {
		t.Fatalf("records %v and %d observations, want 3 due observations and then 1 resolution", types, observations)
	}

	// The default limit takes the rest, and coverage is recorded again.
	at = at.Add(30 * time.Second)
	bounded.apply("c4", at, fresh, func(batch *EvidenceBatch) { batch.CoveredThrough = at.Add(-time.Second) })
	if open := fmt.Sprint(bounded.open()); open != "[n1 n2 n3]" {
		t.Fatalf("open issues %s, want only the reported ones", open)
	}
	if !bounded.state.Checkpoint.CoveredThrough.Equal(at.Add(-time.Second)) {
		t.Fatalf("coverage %s, want the last capture's once nothing waits", bounded.state.Checkpoint.CoveredThrough)
	}
}

func TestChangeOverTheLimitWaitsAndTheIssueStaysSeen(t *testing.T) {
	bounded := newBoundedState(t)
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	bounded.apply("c0", start, steadyIssues, nil)
	at := start.Add(30 * time.Second)
	changed := map[string]string{"api": "critical", "worker": "critical", "cron": "critical"}
	records, _ := bounded.apply("c1", at, changed, func(batch *EvidenceBatch) { batch.ChangeLimit = 1 })
	if len(records) != 1 {
		t.Fatalf("%d records under a limit of 1, want 1", len(records))
	}
	for key, issue := range bounded.state.Active {
		if !issue.LastSeen.Equal(at) {
			t.Fatalf("issue %s last seen %s, want the capture's: a waiting change still counts as a sighting", key, issue.LastSeen)
		}
	}
	records, _ = bounded.apply("c2", at.Add(30*time.Second), changed, nil)
	if len(records) != 2 {
		t.Fatalf("%d records once the limit allows, want the 2 waiting changes", len(records))
	}
}

func TestCatchUpResolutionIsDatedOneIntervalAfterTheLastSighting(t *testing.T) {
	bounded := newBoundedState(t)
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	interval := time.Minute
	bounded.apply("c0", start, map[string]string{"gone": "high", "recent": "high"}, nil)
	// A saved capture ten minutes later misses "gone" but cannot resolve it.
	at := start.Add(10 * time.Minute)
	bounded.apply("c1", at, map[string]string{"recent": "high"}, nil)
	at = at.Add(2 * time.Minute)
	records, _ := bounded.apply("c2", at, map[string]string{}, func(batch *EvidenceBatch) {
		batch.CoveredThrough, batch.CaptureInterval = at, interval
	})
	types := map[string]EvidenceRecord{}
	for _, record := range records {
		types[record.Type] = record
	}
	if caught, ok := types[EvidenceIssueCaughtUp]; len(records) != 2 || !ok || !caught.At.Equal(start.Add(interval)) {
		t.Fatalf("records %v, want a catch-up resolution at %s", recordTypes(records), start.Add(interval))
	}
	// "recent" was seen by the previous saved capture: an ordinary
	// resolution at the coverage.
	if resolved, ok := types["issue_resolved"]; !ok || !resolved.At.Equal(at) {
		t.Fatalf("records %v, want an ordinary resolution at the coverage %s", recordTypes(records), at)
	}
}

// A long gap between saved captures (a save backoff, a restart or a lease
// handover) does not make a real recovery a catch-up: the previous saved
// capture still saw the issue.
func TestRecoveryAfterAGapBetweenSavedCapturesIsAnOrdinaryResolution(t *testing.T) {
	bounded := newBoundedState(t)
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	bounded.apply("c0", start, map[string]string{"api": "high"}, nil)
	at := start.Add(10 * time.Minute)
	records, _ := bounded.apply("c1", at, map[string]string{}, func(batch *EvidenceBatch) {
		batch.CoveredThrough, batch.CaptureInterval = at, time.Minute
	})
	if len(records) != 1 || records[0].Type != "issue_resolved" || !records[0].At.Equal(at) {
		t.Fatalf("records %v, want one ordinary resolution at %s", recordTypes(records), at)
	}
}

func TestCatchUpResolutionsStayOutOfHistoryCountsInEveryStore(t *testing.T) {
	for name, open := range everyEvidenceStore() {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			scope := EvidenceScope{OrgID: "default", ClusterID: "catch-up", ClusterUID: "physical"}
			start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			commitTo(t, store, scope, "c0", start, map[string]string{"gone": "high", "recent": "high"}, nil)
			at := start.Add(10 * time.Minute)
			commitTo(t, store, scope, "c1", at, map[string]string{"recent": "high"}, nil)
			at = at.Add(2 * time.Minute)
			commitTo(t, store, scope, "c2", at, map[string]string{}, func(batch *EvidenceBatch) {
				batch.CoveredThrough, batch.CaptureInterval = at, time.Minute
			})
			for _, kind := range []string{"timeline", "break", "issue"} {
				items, counts := issueRecordCounts(t, store, scope, kind, start.Add(-time.Hour), start.Add(time.Hour))
				if items[EvidenceIssueCaughtUp] != 0 || counts[EvidenceIssueCaughtUp] != 0 {
					t.Fatalf("%s history lists %v and counts %v, want no catch-up resolution", kind, items, counts)
				}
				if items["issue_resolved"] != 1 || items["issue_opened"] != 2 {
					t.Fatalf("%s history lists %v, want 2 openings and the 1 timely resolution", kind, items)
				}
			}
			// The catch-up issue is still resolved.
			items, _ := issueRecordCounts(t, store, scope, "", start.Add(-time.Hour), start.Add(time.Hour))
			if items[EvidenceIssueCaughtUp] != 1 {
				t.Fatalf("journal holds %v, want the catch-up resolution kept", items)
			}
		})
	}
}

func TestPostgresCatchUpInAMonthWithoutAPartitionStillResolves(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "catch-up-month", ClusterUID: "physical"}
	key, _ := evidenceScopeKey(scope)
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.UTC).AddDate(0, -2, 0)
	commitTo(t, p, scope, "c0", old, map[string]string{"gone": "high"}, nil)
	// A later saved capture in the same month already misses the issue.
	commitTo(t, p, scope, "c1", old.Add(10*time.Minute), map[string]string{}, nil)
	// Retention drops the month: its journal and observation partitions.
	partitions := map[string]string{"kubernetes_evidence_events": "evidence_event_month_" + old.Format("200601"), "kubernetes_evidence_occurrence_observations": "evidence_observation_month_" + old.Format("200601")}
	for parent, partition := range partitions {
		var partitioned bool
		if err := p.db.QueryRowContext(t.Context(), `SELECT relkind='p' FROM pg_class WHERE oid=to_regclass($1)`, parent).Scan(&partitioned); err != nil {
			t.Fatal(err)
		}
		if !partitioned {
			t.Fatalf("%s is not partitioned by month", parent)
		}
		if _, err := p.db.ExecContext(t.Context(), `ALTER TABLE `+parent+` DETACH PARTITION `+partition+`; DROP TABLE `+partition); err != nil {
			t.Fatal(err)
		}
	}
	commitTo(t, p, scope, "c2", now, map[string]string{}, func(batch *EvidenceBatch) {
		batch.CoveredThrough, batch.CaptureInterval = now, time.Minute
	})
	var closed time.Time
	if err := p.db.QueryRowContext(t.Context(), `SELECT closed_at FROM kubernetes_evidence_issues WHERE scope_key=$1`, key).Scan(&closed); err != nil {
		t.Fatal(err)
	}
	if !closed.Equal(old.Add(time.Minute)) {
		t.Fatalf("issue closed at %s, want one interval after its last sighting %s", closed, old.Add(time.Minute))
	}
	var records int
	if err := p.db.QueryRowContext(t.Context(), `SELECT count(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_type=$2`, key, EvidenceIssueCaughtUp).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 0 {
		t.Fatalf("%d catch-up records written without their month's partition, want none", records)
	}
	for _, partition := range partitions {
		var exists bool
		if err := p.db.QueryRowContext(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, partition).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("the catch-up resolution recreated the dropped partition %s", partition)
		}
	}
}

// finalCall is one call of the save fault hook: a part transaction or a final.
type finalCall struct{ part, size int }

func TestPostgresTimedOutFinalRunsOncePerCaptureAndLaterSavesMatchAnUninterruptedRun(t *testing.T) {
	dsn := worstCaseDatabase(t)
	faulty := worstCaseProvider(t, dsn, PostgresOptions{})
	calls := []finalCall{}
	failing := true
	faulty.evidenceChunkFault = func(_ context.Context, _ *sql.Tx, part, size int) error {
		calls = append(calls, finalCall{part, size})
		if failing && part == 0 {
			return context.DeadlineExceeded
		}
		return nil
	}
	faultyScope := EvidenceScope{OrgID: "default", ClusterID: "stalled", ClusterUID: "physical"}
	cleanScope := EvidenceScope{OrgID: "default", ClusterID: "uninterrupted", ClusterUID: "physical"}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	issues := func(severity string, count int) map[string]string {
		out := map[string]string{}
		for i := range count {
			out[fmt.Sprintf("issue-%02d", i)] = severity
		}
		return out
	}
	finals := func() int {
		count := 0
		for _, call := range calls {
			// A final reads the issue state first; a part never has nothing
			// to write.
			if call.part == 0 || call.size == 0 {
				count++
			}
		}
		return count
	}
	failing = false
	commitTo(t, faulty, faultyScope, "c0", start, issues("low", 30), nil)
	commitTo(t, faulty, cleanScope, "c0", start, issues("low", 30), nil)
	failing = true
	// Every capture after c0 reports the stretch since c0's coverage as a
	// gap, named after the capture, as the cluster producer does; each
	// failed attempt reports it again over a longer stretch.
	withGap := func(batch *EvidenceBatch) {
		raw, _ := json.Marshal(map[string]any{"from": start, "to": batch.At})
		batch.Records = append(batch.Records, EvidenceRecord{ID: "gap:" + batch.ID + ":Pod", Type: "coverage_gap", GapFrom: start, GapTo: batch.At, Kind: "Pod", At: batch.At, ObservedAt: batch.At, Epoch: "stable", Payload: raw})
	}
	for i, limit := range []int{40, 20, 10, 10, 10} {
		calls = calls[:0]
		cp, err := faulty.EvidenceCheckpoint(t.Context(), faultyScope)
		if err != nil {
			t.Fatal(err)
		}
		batch := issueCapture(t, fmt.Sprintf("stalled-%d", i), cp.ID, cp.Generation, start.Add(time.Duration(i+1)*20*time.Second), 1, issues("high", 40))
		batch.ChangeLimit = limit
		withGap(&batch)
		err = faulty.CommitEvidence(t.Context(), faultyScope, batch)
		if !evidenceSaveTimedOut(err) {
			t.Fatalf("save %d answered %v, want the final's timeout", i, err)
		}
		if finals() != 1 {
			t.Fatalf("save %d ran the final %d times (calls %v), want once per capture", i, finals(), calls)
		}
		if calls[0].size > limit+len(batch.Records) {
			t.Fatalf("save %d applied %d changes beside its %d records, want at most its limit %d", i, calls[0].size-len(batch.Records), len(batch.Records), limit)
		}
	}
	failing = false
	for i, at := range []time.Duration{3 * time.Minute, 4 * time.Minute, 10 * time.Minute} {
		id := fmt.Sprintf("c%d", i+1)
		edit := func(batch *EvidenceBatch) {
			batch.ChangeLimit = 40
			if i == 0 {
				withGap(batch)
			}
		}
		commitTo(t, faulty, faultyScope, id, start.Add(at), issues("high", 40), edit)
		commitTo(t, faulty, cleanScope, id, start.Add(at), issues("high", 40), edit)
	}
	stored := func(scope EvidenceScope) []string {
		t.Helper()
		key, _ := evidenceScopeKey(scope)
		rows := []string{}
		for _, query := range []string{
			`SELECT occurrence_id||issue_key||first_seen::text||last_seen::text||COALESCE(closed_at::text,'')||payload::text||payload_version||COALESCE(last_observed_at::text,'') FROM kubernetes_evidence_issues WHERE scope_key=$1`,
			`SELECT payload_version||occurrence_id||payload::text FROM kubernetes_evidence_occurrence_versions WHERE scope_key=$1`,
			`SELECT occurrence_id||revision::text||observed_at::text||payload_version FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1`,
			// The journal: every record once, under the name the committed
			// save gave it, and none from the captures whose final failed.
			`SELECT 'journal '||event_type||' '||event_id FROM kubernetes_evidence_events WHERE scope_key=$1`,
		} {
			result, err := faulty.db.QueryContext(t.Context(), query, key)
			if err != nil {
				t.Fatal(err)
			}
			for result.Next() {
				var row string
				if err = result.Scan(&row); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, row)
			}
			if err = result.Err(); err != nil {
				t.Fatal(err)
			}
			result.Close()
		}
		sort.Strings(rows)
		return rows
	}
	faultyKey, _ := evidenceScopeKey(faultyScope)
	var gaps int
	if err := faulty.db.QueryRowContext(t.Context(), `SELECT count(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_type='coverage_gap'`, faultyKey).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if gaps != 1 {
		t.Fatalf("%d coverage gaps after one stall, want one", gaps)
	}
	page, err := faulty.QueryEvidence(t.Context(), faultyScope, EvidenceQuery{Type: "timeline", Since: start.Add(-time.Hour), Until: start.Add(time.Hour), Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if timelineGaps := page.Counts["coverage_gap"]; timelineGaps != 1 {
		t.Fatalf("the timeline counts %d coverage gaps (%v), want one", timelineGaps, page.Counts)
	}
	if faultyRows, cleanRows := stored(faultyScope), stored(cleanScope); fmt.Sprint(faultyRows) != fmt.Sprint(cleanRows) {
		t.Fatalf("after the stall the stored issues, versions, observations and journal differ from an uninterrupted run:\nstalled:       %d rows %v\nuninterrupted: %d rows %v", len(faultyRows), journalRows(faultyRows), len(cleanRows), journalRows(cleanRows))
	}
}

func TestPostgresFullIssueReadAfterTheNarrowReadIsCounted(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "full-read", ClusterUID: "physical"}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	commitTo(t, p, scope, "c0", start, steadyIssues, nil)
	at := start.Add(time.Minute)
	commitTo(t, p, scope, "c1", at, steadyIssues, nil)
	if count := p.fullIssueReadCount(); count != 0 {
		t.Fatalf("%d full re-reads for captures the narrow read predicts, want none", count)
	}
	// A rule that loads no payload makes the resolution below touch issues
	// read without theirs.
	issuePayloadRule = func(evidenceIssueRow, EvidenceBatch, map[string]bool) bool { return false }
	t.Cleanup(func() { issuePayloadRule = issueNeedsPayload })
	at = at.Add(time.Minute)
	commitTo(t, p, scope, "c2", at, map[string]string{}, func(batch *EvidenceBatch) { batch.CoveredThrough = at })
	if count := p.fullIssueReadCount(); count != 1 {
		t.Fatalf("%d full re-reads, want 1", count)
	}
	items, _ := issueRecordCounts(t, p, scope, "issue", start.Add(-time.Hour), start.Add(time.Hour))
	if items["issue_resolved"] != len(steadyIssues) {
		t.Fatalf("issue records %v after the full re-read, want every issue resolved", items)
	}
}

func TestOpeningsOverTheLimitWaitForALaterCapture(t *testing.T) {
	bounded := newBoundedState(t)
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	records, _ := bounded.apply("c0", start, steadyIssues, func(batch *EvidenceBatch) { batch.ChangeLimit = 1 })
	if len(records) != 1 || len(bounded.open()) != 1 {
		t.Fatalf("%d records and open issues %v under a limit of 1, want one opening", len(records), bounded.open())
	}
	records, _ = bounded.apply("c1", start.Add(30*time.Second), steadyIssues, nil)
	if types := recordTypes(records); types["issue_opened"] != len(steadyIssues)-1 || len(bounded.open()) != len(steadyIssues) {
		t.Fatalf("records %v and open issues %v, want the waiting openings made", types, bounded.open())
	}
}

// catchUpBacklog is a scope with many issues gone for an hour and a few gone
// for two minutes, the shape of a cluster whose resolutions were withheld.
func catchUpBacklog() (stale, recent map[string]string) {
	stale, recent = map[string]string{}, map[string]string{}
	for i := range 5000 {
		stale[fmt.Sprintf("stale-%04d", i)] = "low"
	}
	for i := range 10 {
		recent[fmt.Sprintf("recent-%02d", i)] = "high"
	}
	return stale, recent
}

func TestCatchUpResolutionsAreCappedPerSaveWhileRecentOnesApply(t *testing.T) {
	bounded := newBoundedState(t)
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	stale, recent := catchUpBacklog()
	bounded.apply("c0", start, stale, nil)
	at := start.Add(time.Hour)
	bounded.apply("c1", at, recent, nil)
	for i, want := range []struct{ caughtUp, resolved int }{{2000, 10}, {2000, 0}, {1000, 0}} {
		at = at.Add(2 * time.Minute)
		covered := at
		if i == 0 {
			// The recent issues were last seen two minutes before this
			// coverage: theirs are ordinary resolutions.
			covered = start.Add(time.Hour + 2*time.Minute)
		}
		records, _ := bounded.apply(fmt.Sprintf("c%d", i+2), at, map[string]string{}, func(batch *EvidenceBatch) {
			batch.CoveredThrough, batch.CaptureInterval = covered, time.Minute
		})
		types := recordTypes(records)
		if types[EvidenceIssueCaughtUp] != want.caughtUp || types["issue_resolved"] != want.resolved {
			t.Fatalf("save %d resolved %v, want %d catch-ups and %d recent", i+1, types, want.caughtUp, want.resolved)
		}
		if recorded := !bounded.state.Checkpoint.CoveredThrough.IsZero(); recorded != (i == 2) {
			t.Fatalf("save %d: coverage recorded %t, want it only once nothing waits", i+1, recorded)
		}
	}
	if open := bounded.open(); len(open) != 0 {
		t.Fatalf("%d issues still open after the backlog drained", len(open))
	}
}

func TestPostgresCatchUpResolutionsAreCappedPerSaveWhileRecentOnesApply(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "catch-up-cap", ClusterUID: "physical"}
	key, _ := evidenceScopeKey(scope)
	start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	stale, recent := catchUpBacklog()
	commitTo(t, p, scope, "c0", start, stale, nil)
	at := start.Add(time.Hour)
	commitTo(t, p, scope, "c1", at, recent, nil)
	rows := func() (caughtUp, resolved, open int) {
		t.Helper()
		if err := p.db.QueryRowContext(t.Context(), `SELECT
 (SELECT count(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_type=$2),
 (SELECT count(*) FROM kubernetes_evidence_events WHERE scope_key=$1 AND event_type='issue_resolved'),
 (SELECT count(*) FROM kubernetes_evidence_issues WHERE scope_key=$1 AND closed_at IS NULL)`, key, EvidenceIssueCaughtUp).Scan(&caughtUp, &resolved, &open); err != nil {
			t.Fatal(err)
		}
		return caughtUp, resolved, open
	}
	for i, want := range []struct{ caughtUp, resolved, open int }{{2000, 10, 3000}, {4000, 10, 1000}, {5000, 10, 0}} {
		at = at.Add(2 * time.Minute)
		covered := at
		if i == 0 {
			covered = start.Add(time.Hour + 2*time.Minute)
		}
		commitTo(t, p, scope, fmt.Sprintf("c%d", i+2), at, map[string]string{}, func(batch *EvidenceBatch) {
			batch.CoveredThrough, batch.CaptureInterval = covered, time.Minute
		})
		if caughtUp, resolved, open := rows(); caughtUp != want.caughtUp || resolved != want.resolved || open != want.open {
			t.Fatalf("after save %d: %d catch-ups, %d recent resolutions, %d open; want %d, %d, %d", i+1, caughtUp, resolved, open, want.caughtUp, want.resolved, want.open)
		}
		cp, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if recorded := !cp.CoveredThrough.IsZero(); recorded != (i == 2) {
			t.Fatalf("save %d: coverage recorded %t, want it only once nothing waits", i+1, recorded)
		}
	}
}

// journalRows counts the journal rows among stored rows by type.
func journalRows(rows []string) map[string]int {
	counts := map[string]int{}
	for _, row := range rows {
		var kind, id string
		if n, _ := fmt.Sscanf(row, "journal %s %s", &kind, &id); n == 2 {
			counts[kind]++
		}
	}
	return counts
}
