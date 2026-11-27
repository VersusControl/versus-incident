package storage

import (
	"testing"
	"time"
)

// The save's issue read leaves out the payloads of the open issues a capture
// cannot change. Applied to the same capture, the state it reads must derive
// exactly the writes the state read in full derives.
func TestPostgresNarrowIssueReadSavesWhatTheFullReadSaves(t *testing.T) {
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "narrow-issue-read", ClusterUID: "physical"}
	key, err := evidenceScopeKey(scope)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	capture := func(id string, at time.Time, count int, issues map[string]string) EvidenceBatch {
		t.Helper()
		cp, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		return normalizeEvidenceBatch(issueCapture(t, id, cp.ID, cp.Generation, at, count, issues))
	}
	commit := func(batch EvidenceBatch) {
		t.Helper()
		if err := p.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
	}
	compare := func(name string, batch EvidenceBatch, wantPayloadless bool) {
		t.Helper()
		outcome := func(full bool) (saveOutcome, int, bool) {
			t.Helper()
			tx, err := p.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			state, err := loadEvidenceSaveState(t.Context(), tx, key, scope, batch, full)
			if err != nil {
				t.Fatal(err)
			}
			payloadless := 0
			for _, stored := range state.storedIssues {
				if stored.payloadless {
					payloadless++
				}
			}
			records, err := applyEvidence(&state, batch)
			if err != nil {
				t.Fatal(err)
			}
			return outcomeOf(t, state, records, batch.ObservationHeartbeat), payloadless, payloadlessIssuesTouched(state, batch.ObservationHeartbeat)
		}
		full, _, _ := outcome(true)
		narrow, payloadless, touched := outcome(false)
		if touched {
			t.Fatalf("%s: the capture touched an issue read without its payload", name)
		}
		if wantPayloadless && payloadless == 0 {
			t.Fatalf("%s: every open issue was read with its payload, want the ones the capture leaves alone without", name)
		}
		for field, pair := range map[string][2]string{"records": {full.records, narrow.records}, "issue rows": {full.rows, narrow.rows}, "narrow updates": {full.seen, narrow.seen}, "versions": {full.versions, narrow.versions}, "observations": {full.observations, narrow.observations}, "checkpoint": {full.checkpoint, narrow.checkpoint}} {
			if pair[0] != pair[1] {
				t.Fatalf("%s: %s differ\nfull:   %s\nnarrow: %s", name, field, pair[0], pair[1])
			}
		}
		commit(batch)
	}

	everyone := map[string]string{"api": "high", "worker": "medium", "cron": "low", "stale-a": "high", "stale-b": "low"}
	commit(capture("c0", start, 1, everyone))
	at := start.Add(30 * time.Second)
	compare("two issues missing", capture("c1", at, 2, steadyIssues), true)
	at = at.Add(10 * time.Minute)
	compare("one issue changes after a heartbeat", capture("c2", at, 3, map[string]string{"api": "critical", "worker": "medium", "cron": "low"}), true)
	at = at.Add(30 * time.Second)
	batch := capture("c3", at, 4, steadyIssues)
	batch.CoveredThrough = start.Add(10 * time.Second)
	compare("coverage resolves the missing issues", batch, false)

	var open, closed int
	if err = p.db.QueryRowContext(t.Context(), `SELECT count(*) FILTER (WHERE closed_at IS NULL),count(*) FILTER (WHERE closed_at IS NOT NULL) FROM kubernetes_evidence_issues WHERE scope_key=$1`, key).Scan(&open, &closed); err != nil {
		t.Fatal(err)
	}
	if open != len(steadyIssues) || closed != 2 {
		t.Fatalf("after the saves %d issues are open and %d resolved, want %d open and the 2 missing ones resolved", open, closed, len(steadyIssues))
	}
	var indexed bool
	if err = p.db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE tablename='kubernetes_evidence_issues' AND indexname='kubernetes_evidence_open_issue')`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("the open issue index is missing after the migrations ran")
	}
}
