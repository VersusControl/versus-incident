package storage

import (
	"database/sql"
	"encoding/json"
	"sort"
	"testing"
	"time"
)

// issueRows returns the table's open issues as the save's read returns them
// in full: the columns, and the payload with the last_seen it was written with.
func (table issueTable) issueRows(t *testing.T) []evidenceIssueRow {
	t.Helper()
	rows := []evidenceIssueRow{}
	for id, stored := range table {
		if stored.issue.ClosedAt != nil || stored.issue.ObservationOnly {
			continue
		}
		payload := stored.issue
		payload.LastSeen = stored.payloadSeen
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		issue := stored.issue
		rows = append(rows, evidenceIssueRow{occurrenceID: id, issueKey: issue.Key, version: stored.version, rootUID: issue.UID, namespace: issue.Namespace, kind: issue.Kind, rule: issue.Rule, authorization: issue.AuthorizationKind, broken: issue.Broken, modelReady: stored.modelReady, firstSeen: issue.FirstSeen, lastSeen: issue.LastSeen, lastObserved: storedObservedAt(stored.lastObserved), payloadSeen: stored.payloadSeen.Format(time.RFC3339Nano), payload: raw})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].occurrenceID < rows[j].occurrenceID })
	return rows
}

func storedObservedAt(at time.Time) sql.NullTime {
	return sql.NullTime{Time: at, Valid: !at.IsZero()}
}

// loadIssueState builds the state a save starts from the way
// readEvidenceIssues does: in full, or with the payload of every issue the
// capture cannot change left out. It returns how many issues were read
// without their payload.
func loadIssueState(t *testing.T, table issueTable, checkpoint EvidenceCheckpoint, batch EvidenceBatch, full bool) (evidenceState, int) {
	t.Helper()
	state := evidenceState{Checkpoint: checkpoint, Active: map[string]EvidenceIssue{}, Occurrences: map[string]EvidenceIssue{}, OccurrenceVersions: map[string]EvidenceIssue{}, OccurrenceHeads: map[string]string{}, storedIssues: map[string]storedIssue{}}
	present := evidenceBatchIssueKeys(batch)
	payloadless := 0
	for _, row := range table.issueRows(t) {
		if !full {
			narrow := row
			narrow.payload = nil
			if !issueNeedsPayload(narrow, batch, present) {
				row = narrow
				payloadless++
			}
		}
		if err := addStoredIssue(&state, row); err != nil {
			t.Fatal(err)
		}
	}
	return state, payloadless
}

// saveOutcome is everything a save derives from its issue state and writes:
// the journal records, the whole issue rows and narrow updates, the
// occurrence versions and observations, and the checkpoint, each encoded in
// a fixed order.
type saveOutcome struct {
	records, rows, seen, versions, observations, checkpoint string
	save                                                    issueSave
}

func encodeSorted(t *testing.T, values []string) string {
	t.Helper()
	sort.Strings(values)
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func encodeValue(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func outcomeOf(t *testing.T, state evidenceState, records []EvidenceRecord, heartbeat time.Duration) saveOutcome {
	t.Helper()
	rows, seen := occurrenceIssueRows(state, heartbeat)
	encodeAll := func(items []map[string]any) string {
		values := []string{}
		for _, item := range items {
			values = append(values, encodeValue(t, item))
		}
		return encodeSorted(t, values)
	}
	recordValues := []string{}
	for _, record := range records {
		recordValues = append(recordValues, encodeValue(t, record))
	}
	observationValues := []string{}
	for _, observation := range state.Observations {
		observationValues = append(observationValues, encodeValue(t, observation))
	}
	return saveOutcome{
		records: encodeSorted(t, recordValues), rows: encodeAll(rows), seen: encodeAll(seen),
		versions: encodeValue(t, state.OccurrenceVersions), observations: encodeSorted(t, observationValues),
		checkpoint: encodeValue(t, state.Checkpoint),
		save:       issueSave{rows: rows, seen: seen, checkpoint: state.Checkpoint},
	}
}

// saveBothWays applies one capture to the state read in full and to the state
// read narrowly, falling back to the full read when the save finds it touched
// a payloadless issue, as commitEvidenceFinal does. It returns both outcomes,
// whether the narrow save fell back and how many issues it read without
// their payload.
func saveBothWays(t *testing.T, table issueTable, checkpoint EvidenceCheckpoint, batch EvidenceBatch) (full, narrow saveOutcome, fellBack bool, payloadless int) {
	t.Helper()
	state, _ := loadIssueState(t, table, checkpoint, batch, true)
	records, err := applyEvidence(&state, batch)
	if err != nil {
		t.Fatal(err)
	}
	full = outcomeOf(t, state, records, batch.ObservationHeartbeat)
	state, payloadless = loadIssueState(t, table, checkpoint, batch, false)
	records, err = applyEvidence(&state, batch)
	if err != nil {
		t.Fatal(err)
	}
	if payloadlessIssuesTouched(state, batch.ObservationHeartbeat) {
		fellBack = true
		state, _ = loadIssueState(t, table, checkpoint, batch, true)
		if records, err = applyEvidence(&state, batch); err != nil {
			t.Fatal(err)
		}
	}
	narrow = outcomeOf(t, state, records, batch.ObservationHeartbeat)
	return full, narrow, fellBack, payloadless
}

func TestNarrowIssueReadSavesWhatTheFullReadSaves(t *testing.T) {
	table := issueTable{}
	start := time.Date(2026, 11, 19, 12, 0, 0, 123456789, time.UTC)
	heartbeat := 5 * time.Minute
	everyone := map[string]string{"api": "high", "worker": "medium", "cron": "low", "stale-a": "high", "stale-b": "low"}
	checkpoint := EvidenceCheckpoint{}
	step := 0
	save := func(name string, batch EvidenceBatch, wantPayloadless bool) {
		t.Helper()
		step++
		full, narrow, fellBack, payloadless := saveBothWays(t, table, checkpoint, batch)
		if fellBack {
			t.Fatalf("%s: the save touched an issue read without its payload and had to read every issue again", name)
		}
		if wantPayloadless && payloadless == 0 {
			t.Fatalf("%s: every issue was read with its payload, want the ones the capture leaves alone without", name)
		}
		for field, pair := range map[string][2]string{"records": {full.records, narrow.records}, "issue rows": {full.rows, narrow.rows}, "narrow updates": {full.seen, narrow.seen}, "versions": {full.versions, narrow.versions}, "observations": {full.observations, narrow.observations}, "checkpoint": {full.checkpoint, narrow.checkpoint}} {
			if pair[0] != pair[1] {
				t.Fatalf("%s: %s differ\nfull:   %s\nnarrow: %s", name, field, pair[0], pair[1])
			}
		}
		table.write(t, full.save)
		checkpoint = full.save.checkpoint
	}
	capture := func(id string, at time.Time, count int, issues map[string]string) EvidenceBatch {
		batch := issueCapture(t, id, checkpoint.ID, 0, at, count, issues)
		batch.ObservationHeartbeat = heartbeat
		return batch
	}

	save("first capture", capture("c0", start, 1, everyone), false)
	// Two issues stop being reported, and coverage does not resolve them:
	// they stay open, untouched by every later capture.
	at := start.Add(30 * time.Second)
	save("two issues missing", capture("c1", at, 2, steadyIssues), true)
	at = at.Add(30 * time.Second)
	batch := capture("c2", at, 3, map[string]string{"api": "critical", "worker": "medium", "cron": "low"})
	batch.CoveredThrough = start.Add(-time.Minute)
	save("one issue changes, coverage before the missing issues", batch, true)

	// An open issue whose payload trails its last_seen by a heartbeat, as an
	// older build left it, and one whose row is not a complete read model,
	// are rewritten whole even though no capture reports them.
	for id, stored := range table {
		switch stored.issue.Key {
		case "stale-a":
			stored.payloadSeen = stored.issue.LastSeen.Add(-heartbeat)
		case "stale-b":
			stored.modelReady = false
		}
		table[id] = stored
	}
	at = at.Add(30 * time.Second)
	save("stored rows that need a whole rewrite", capture("c3", at, 4, steadyIssues), false)

	// A new issue opens and one comes back while the others stay missing.
	at = at.Add(10 * time.Minute)
	save("an issue opens", capture("c4", at, 5, map[string]string{"api": "high", "worker": "medium", "cron": "low", "db": "high"}), true)
	at = at.Add(30 * time.Second)
	save("a missing issue returns", capture("c5", at, 6, map[string]string{"api": "high", "worker": "medium", "cron": "low", "db": "high", "stale-a": "high"}), true)

	// Coverage passes the last sightings of both missing issues: they resolve.
	at = at.Add(30 * time.Second)
	batch = capture("c6", at, 7, map[string]string{"api": "high", "worker": "medium", "cron": "low", "db": "high"})
	batch.CoveredThrough = at.Add(-time.Second)
	save("coverage resolves the missing issues", batch, false)
	resolved := 0
	for _, stored := range table {
		if stored.issue.ClosedAt != nil {
			resolved++
		}
	}
	if resolved != 2 {
		t.Fatalf("%d issues resolved after %d saves, want the two missing issues", resolved, step)
	}
}

func TestSaveNoticesACaptureThatChangesAPayloadlessIssue(t *testing.T) {
	table := issueTable{}
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	first := saveIssues(t, table, EvidenceCheckpoint{}, issueCapture(t, "c0", "", 0, start, 1, steadyIssues), true)
	table.write(t, first)
	at := start.Add(30 * time.Second)
	batch := issueCapture(t, "c1", "c0", 0, at, 2, map[string]string{"api": "high"})
	batch.CoveredThrough = at

	// Every issue read without its payload, as if the read had misjudged
	// which ones the capture changes: the capture resolves two of them and
	// updates the third, so the save must notice.
	state := evidenceState{Checkpoint: first.checkpoint, Active: map[string]EvidenceIssue{}, Occurrences: map[string]EvidenceIssue{}, OccurrenceVersions: map[string]EvidenceIssue{}, OccurrenceHeads: map[string]string{}, storedIssues: map[string]storedIssue{}}
	for _, row := range table.issueRows(t) {
		row.payload = nil
		if err := addStoredIssue(&state, row); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := applyEvidence(&state, batch); err != nil {
		t.Fatal(err)
	}
	if !payloadlessIssuesTouched(state, batch.ObservationHeartbeat) {
		t.Fatal("a capture that resolved and updated issues read without their payload went unnoticed")
	}

	// The same issues left alone by a capture of nothing new are not touched.
	state, payloadless := loadIssueState(t, table, first.checkpoint, issueCapture(t, "c2", "c0", 0, at, 2, map[string]string{}), false)
	if payloadless != len(steadyIssues) {
		t.Fatalf("%d of %d issues read without their payload for a capture that reports none, want all", payloadless, len(steadyIssues))
	}
	empty := issueCapture(t, "c2", "c0", 0, at, 2, map[string]string{})
	if _, err := applyEvidence(&state, empty); err != nil {
		t.Fatal(err)
	}
	if payloadlessIssuesTouched(state, empty.ObservationHeartbeat) {
		t.Fatal("a capture that left every issue alone was reported as touching one")
	}
	if rows, seen := occurrenceIssueRows(state, empty.ObservationHeartbeat); len(rows) != 0 || len(seen) != 0 {
		t.Fatalf("issues read without their payload produced %d rows and %d updates, want none", len(rows), len(seen))
	}
}
