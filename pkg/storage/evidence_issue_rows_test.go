package storage

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

// issueTable stands in for kubernetes_evidence_issues: the rows a save
// writes, read back the way the database returns them (the payload through
// JSON, timestamps at microsecond precision).
type issueTable map[string]storedIssue

// write applies a save's whole rows and its narrow freshness updates.
func (table issueTable) write(t *testing.T, saved issueSave) {
	t.Helper()
	for _, row := range saved.rows {
		raw, err := json.Marshal(row["payload"])
		if err != nil {
			t.Fatal(err)
		}
		var issue EvidenceIssue
		if err = json.Unmarshal(raw, &issue); err != nil {
			t.Fatal(err)
		}
		payloadSeen := issue.LastSeen
		issue.LastSeen = row["last_seen"].(time.Time).Truncate(time.Microsecond)
		var observed time.Time
		if at, ok := row["last_observed_at"].(time.Time); ok {
			observed = at.Truncate(time.Microsecond)
		}
		table[issue.OccurrenceID] = storedIssue{issue: issue, payloadSeen: payloadSeen, version: row["payload_version"].(string), lastObserved: observed, modelReady: true}
	}
	for _, row := range saved.seen {
		stored := table[row["occurrence_id"].(string)]
		stored.issue.LastSeen = row["last_seen"].(time.Time).Truncate(time.Microsecond)
		stored.lastObserved = time.Time{}
		if at, ok := row["last_observed_at"].(time.Time); ok {
			stored.lastObserved = at.Truncate(time.Microsecond)
		}
		table[row["occurrence_id"].(string)] = stored
	}
}

// load builds the state a database save starts from: the open issues of the
// table, as the save's issue query reads them, and the last checkpoint.
func (table issueTable) load(checkpoint EvidenceCheckpoint) evidenceState {
	state := evidenceState{Checkpoint: checkpoint, Active: map[string]EvidenceIssue{}, Occurrences: map[string]EvidenceIssue{}, OccurrenceVersions: map[string]EvidenceIssue{}, OccurrenceHeads: map[string]string{}, LastObservations: map[string]time.Time{}, storedIssues: map[string]storedIssue{}}
	for id, stored := range table {
		if stored.issue.ClosedAt != nil || stored.issue.ObservationOnly {
			continue
		}
		state.Active[stored.issue.Key] = stored.issue
		state.Occurrences[id] = stored.issue
		state.OccurrenceHeads[id] = stored.version
		state.LastObservations[id] = stored.lastObserved
		state.storedIssues[id] = stored
	}
	return state
}

// changedColumns names the columns of a row that the upsert's guard compares
// and that differ from the stored row, as the database compares them (the
// payload as JSON values, so key order does not matter).
func (table issueTable) changedColumns(t *testing.T, row map[string]any) []string {
	t.Helper()
	stored := table[row["occurrence_id"].(string)]
	changed := []string{}
	if !row["last_seen"].(time.Time).Truncate(time.Microsecond).Equal(stored.issue.LastSeen) {
		changed = append(changed, "last_seen")
	}
	if row["payload_version"].(string) != stored.version {
		changed = append(changed, "payload_version")
	}
	observed, _ := row["last_observed_at"].(time.Time)
	if !observed.Truncate(time.Microsecond).Equal(stored.lastObserved) {
		changed = append(changed, "last_observed_at")
	}
	if (row["closed_at"].(*time.Time) != nil) != (stored.issue.ClosedAt != nil) {
		changed = append(changed, "closed_at")
	}
	asValue := func(value any) any {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err = json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	storedPayload := stored.issue
	storedPayload.LastSeen = stored.payloadSeen
	if !reflect.DeepEqual(asValue(row["payload"]), asValue(storedPayload)) {
		changed = append(changed, "payload")
	}
	return changed
}

// issueCapture is the batch a cluster sends: every current issue, each with
// the capture's time as its last_seen, and the issue's payload with a
// measurement (count) that moves on every capture.
func issueCapture(t *testing.T, id, expected string, generation uint64, at time.Time, count int, severities map[string]string) EvidenceBatch {
	t.Helper()
	batch := EvidenceBatch{ID: id, Expected: expected, Generation: generation, At: at, Epoch: "stable", ObservationHeartbeat: 5 * time.Minute}
	names := make([]string, 0, len(severities))
	for name := range severities {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		payload, err := json.Marshal(map[string]any{"rule": "pod.crashloop", "severity": severities[name], "count": count, "last_seen": at, "evidence": map[string]any{"restarts": count}})
		if err != nil {
			t.Fatal(err)
		}
		batch.Issues = append(batch.Issues, EvidenceIssue{Key: name, Kind: "Deployment", UID: name + "-uid", Rule: "pod.crashloop", Namespace: "shop", AuthorizationKind: "Pod", Broken: true, LastSeen: at, Payload: payload})
	}
	return batch
}

// issueSave is what one save writes to the issues table.
type issueSave struct {
	rows, seen []map[string]any
	checkpoint EvidenceCheckpoint
}

// saveIssues applies a capture to the state loaded from the table, as a
// database save does, and returns what it writes. Unfiltered, the save
// compares nothing with the stored rows, as before the comparison existed.
func saveIssues(t *testing.T, table issueTable, checkpoint EvidenceCheckpoint, batch EvidenceBatch, filtered bool) issueSave {
	t.Helper()
	state := table.load(checkpoint)
	if !filtered {
		state.storedIssues = nil
	}
	if _, err := applyEvidence(&state, batch); err != nil {
		t.Fatal(err)
	}
	rows, seen := occurrenceIssueRows(state, batch.ObservationHeartbeat)
	return issueSave{rows: rows, seen: seen, checkpoint: state.Checkpoint}
}

var steadyIssues = map[string]string{"api": "high", "worker": "medium", "cron": "low"}

func TestUnchangedIssuesRewriteEveryRowWithoutTheStoredComparison(t *testing.T) {
	table := issueTable{}
	start := time.Date(2026, 11, 19, 12, 0, 0, 123456789, time.UTC)
	first := saveIssues(t, table, EvidenceCheckpoint{}, issueCapture(t, "c0", "", 0, start, 1, steadyIssues), true)
	table.write(t, first)

	// The same issues, unchanged, thirty seconds later: the cluster sends each
	// with the new capture time as last_seen and a moved measurement. Sent
	// whole, every row differs from the stored one in last_seen and in its
	// payload, so the upsert's guard rewrites every row on every save.
	saved := saveIssues(t, table, first.checkpoint, issueCapture(t, "c1", "c0", 0, start.Add(30*time.Second), 2, steadyIssues), false)
	if len(saved.rows) != len(steadyIssues) {
		t.Fatalf("unfiltered save sent %d rows, want every open issue (%d)", len(saved.rows), len(steadyIssues))
	}
	for _, row := range saved.rows {
		if changed := table.changedColumns(t, row); fmt.Sprint(changed) != "[last_seen payload]" {
			t.Fatalf("unchanged issue %v differs in %v, want last_seen and payload only", row["occurrence_id"], changed)
		}
	}
}

func TestUnchangedIssuesUpdateOnlyLastSeen(t *testing.T) {
	table := issueTable{}
	start := time.Date(2026, 11, 19, 12, 0, 0, 123456789, time.UTC)
	saved := saveIssues(t, table, EvidenceCheckpoint{}, issueCapture(t, "c0", "", 0, start, 1, steadyIssues), true)
	if len(saved.rows) != len(steadyIssues) || len(saved.seen) != 0 {
		t.Fatalf("first save wrote %d whole rows and %d narrow updates, want %d whole", len(saved.rows), len(saved.seen), len(steadyIssues))
	}
	table.write(t, saved)
	for i, id := range []string{"c1", "c2"} {
		at := start.Add(time.Duration(i+1) * 30 * time.Second)
		saved = saveIssues(t, table, saved.checkpoint, issueCapture(t, id, saved.checkpoint.ID, 0, at, i+2, steadyIssues), true)
		if len(saved.rows) != 0 || len(saved.seen) != len(steadyIssues) {
			t.Fatalf("save %s of unchanged issues wrote %d whole rows and %d narrow updates, want none and %d", id, len(saved.rows), len(saved.seen), len(steadyIssues))
		}
		for _, row := range saved.seen {
			if len(row) != 3 || !row["last_seen"].(time.Time).Equal(at) {
				t.Fatalf("narrow update %v, want only last_seen=%s and last_observed_at", row, at)
			}
		}
		table.write(t, saved)
	}
	// A save with nothing new for an issue (no capture of it, still open)
	// writes nothing for it.
	state := table.load(saved.checkpoint)
	if rows, seen := occurrenceIssueRows(state, 5*time.Minute); len(rows) != 0 || len(seen) != 0 {
		t.Fatalf("stored state written back as is produced %d rows and %d updates, want none", len(rows), len(seen))
	}
}

func TestOneChangedIssueWritesOneRow(t *testing.T) {
	table := issueTable{}
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	saved := saveIssues(t, table, EvidenceCheckpoint{}, issueCapture(t, "c0", "", 0, start, 1, steadyIssues), true)
	table.write(t, saved)
	changed := map[string]string{"api": "critical", "worker": "medium", "cron": "low"}
	saved = saveIssues(t, table, saved.checkpoint, issueCapture(t, "c1", "c0", 0, start.Add(30*time.Second), 2, changed), true)
	if len(saved.rows) != 1 {
		t.Fatalf("save with one changed issue wrote %d whole rows, want 1", len(saved.rows))
	}
	if issue := saved.rows[0]["payload"].(EvidenceIssue); issue.Key != "api" || saved.rows[0]["severity"] != "critical" {
		t.Fatalf("written row is %v severity %v, want api with its new severity", issue.Key, saved.rows[0]["severity"])
	}
}

func TestClosedIssueWritesItsRow(t *testing.T) {
	table := issueTable{}
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	saved := saveIssues(t, table, EvidenceCheckpoint{}, issueCapture(t, "c0", "", 0, start, 1, steadyIssues), true)
	table.write(t, saved)
	at := start.Add(30 * time.Second)
	batch := issueCapture(t, "c1", "c0", 0, at, 2, map[string]string{"api": "high", "worker": "medium"})
	batch.CoveredThrough = at
	saved = saveIssues(t, table, saved.checkpoint, batch, true)
	if len(saved.rows) != 1 || saved.rows[0]["payload"].(EvidenceIssue).Key != "cron" || saved.rows[0]["closed_at"].(*time.Time) == nil {
		t.Fatalf("save that resolves one issue wrote %v, want only the resolved issue whole", saved.rows)
	}
}

func TestPayloadTrailsByLessThanAHeartbeatAndLastSeenIsExact(t *testing.T) {
	table := issueTable{}
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	heartbeat := 5 * time.Minute
	saved := saveIssues(t, table, EvidenceCheckpoint{}, issueCapture(t, "c0", "", 0, start, 1, steadyIssues), true)
	table.write(t, saved)
	whole := 0
	for i := 1; i <= 40; i++ {
		at := start.Add(time.Duration(i) * 30 * time.Second)
		saved = saveIssues(t, table, saved.checkpoint, issueCapture(t, fmt.Sprintf("c%d", i), saved.checkpoint.ID, 0, at, i+1, steadyIssues), true)
		if len(saved.rows) > 0 {
			whole++
		}
		table.write(t, saved)
		for _, stored := range table {
			if !stored.issue.LastSeen.Equal(at) {
				t.Fatalf("after the save at %s issue %s shows last_seen %s, want the capture's", at.Format(time.TimeOnly), stored.issue.Key, stored.issue.LastSeen.Format(time.TimeOnly))
			}
			if lag := at.Sub(stored.payloadSeen); lag >= heartbeat {
				t.Fatalf("after the save at %s issue %s has a payload from %s, %s old; want less than a heartbeat", at.Format(time.TimeOnly), stored.issue.Key, stored.payloadSeen.Format(time.TimeOnly), lag)
			}
		}
	}
	// Twenty minutes of captures every thirty seconds: payloads are rewritten
	// about once per heartbeat, not on each of the forty saves.
	if whole < 3 || whole > 5 {
		t.Fatalf("whole issue rows were written by %d of 40 saves, want about one per heartbeat", whole)
	}
}

// An issue missing from one capture whose coverage starts after the issue's
// previous sighting but before an older, heartbeat-old one stays open: the
// resolution compares coverage with the issue's actual last sighting. Every
// store agrees, so the issue never comes back as a second occurrence.
func TestBrieflyMissingIssueStaysOpenInEveryStore(t *testing.T) {
	stores := map[string]func(t *testing.T) EvidenceStore{
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
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			scope := EvidenceScope{OrgID: "default", ClusterID: "briefly-missing", ClusterUID: "physical"}
			start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			commit := func(id string, at time.Time, issues map[string]string, covered time.Time) {
				t.Helper()
				cp, err := store.EvidenceCheckpoint(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				batch := issueCapture(t, id, cp.ID, cp.Generation, at, 1, issues)
				batch.CoveredThrough = covered
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatal(err)
				}
			}
			commit("c0", start, steadyIssues, time.Time{})
			commit("c1", start.Add(time.Minute), steadyIssues, time.Time{})
			// cron is missing; coverage starts between its first sighting and
			// its latest one.
			commit("c2", start.Add(2*time.Minute), map[string]string{"api": "high", "worker": "medium"}, start.Add(30*time.Second))
			commit("c3", start.Add(3*time.Minute), steadyIssues, time.Time{})
			page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "issue", Since: start.Add(-time.Hour), Until: start.Add(time.Hour), Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, record := range page.Items {
				counts[record.Type]++
			}
			if counts["issue_resolved"] != 0 || counts["issue_opened"] != len(steadyIssues) {
				t.Fatalf("issue records %v, want %d opened and none resolved", counts, len(steadyIssues))
			}
		})
	}
}

// issueStats returns the issues table's update counters once they have
// counted at least wantUpdates updates (statistics reach the view shortly
// after the transactions that made them).
func issueStats(t *testing.T, p *postgresProvider, wantUpdates int64) (updates, hot int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := p.db.ExecContext(t.Context(), `SELECT pg_stat_clear_snapshot()`); err != nil {
			t.Fatal(err)
		}
		if err := p.db.QueryRowContext(t.Context(), `SELECT n_tup_upd,n_tup_hot_upd FROM pg_stat_user_tables WHERE relname='kubernetes_evidence_issues'`).Scan(&updates, &hot); err != nil {
			t.Fatal(err)
		}
		if updates >= wantUpdates || time.Now().After(deadline) {
			return updates, hot
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestPostgresUnchangedIssuesKeepTheirPayloadAndUpdateInPlace(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "issue-rows", ClusterUID: "physical"}
	key, _ := evidenceScopeKey(scope)
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	commit := func(id string, at time.Time, count int, severities map[string]string) {
		t.Helper()
		cp, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.CommitEvidence(t.Context(), scope, issueCapture(t, id, cp.ID, cp.Generation, at, count, severities)); err != nil {
			t.Fatal(err)
		}
	}
	payloads := func() map[string]string {
		t.Helper()
		rows, err := p.db.QueryContext(t.Context(), `SELECT issue_key,payload::text FROM kubernetes_evidence_issues WHERE scope_key=$1`, key)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var issue, payload string
			if err = rows.Scan(&issue, &payload); err != nil {
				t.Fatal(err)
			}
			out[issue] = payload
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	lastSeen := func(want time.Time) {
		t.Helper()
		rows, err := p.db.QueryContext(t.Context(), `SELECT issue_key,last_seen FROM kubernetes_evidence_issues WHERE scope_key=$1`, key)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var issue string
			var seen time.Time
			if err = rows.Scan(&issue, &seen); err != nil {
				t.Fatal(err)
			}
			if !seen.Equal(want) {
				t.Errorf("issue %s last_seen %s, want the capture's %s", issue, seen, want)
			}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
	}

	commit("c0", start, 1, steadyIssues)
	first := payloads()
	if len(first) != len(steadyIssues) {
		t.Fatalf("first save stored %d issues, want %d", len(first), len(steadyIssues))
	}
	baseUpdates, baseHot := issueStats(t, p, 0)

	// Unchanged issues: last_seen follows each capture exactly, the payload
	// stays, and every update is made in place.
	for i, id := range []string{"c1", "c2"} {
		at := start.Add(time.Duration(i+1) * 30 * time.Second)
		commit(id, at, i+2, steadyIssues)
		lastSeen(at)
		if after := payloads(); !reflect.DeepEqual(after, first) {
			t.Fatalf("a save of unchanged issues rewrote payloads: before %v after %v", first, after)
		}
	}
	updates, hot := issueStats(t, p, baseUpdates+2*int64(len(steadyIssues)))
	t.Logf("issue updates by two saves of unchanged issues: %d, in place: %d", updates-baseUpdates, hot-baseHot)
	if updates-baseUpdates < 2*int64(len(steadyIssues)) || hot-baseHot != updates-baseUpdates {
		t.Fatalf("issue updates %d, in place %d; want every last_seen update in place", updates-baseUpdates, hot-baseHot)
	}

	// One changed issue: only its payload is rewritten.
	commit("c3", start.Add(90*time.Second), 4, map[string]string{"api": "critical", "worker": "medium", "cron": "low"})
	rewritten := []string{}
	for issue, payload := range payloads() {
		if first[issue] != payload {
			rewritten = append(rewritten, issue)
		}
	}
	if fmt.Sprint(rewritten) != "[api]" {
		t.Fatalf("a save with one changed issue rewrote payloads of %v, want [api]", rewritten)
	}
}
