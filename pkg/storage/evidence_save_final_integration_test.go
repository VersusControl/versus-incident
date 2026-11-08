package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// finalSaveTransaction is one timed save transaction of a capture.
type finalSaveTransaction struct {
	part     int
	size     int
	labelled bool
	took     time.Duration
	err      error
}

// finalSaveIssue is one open issue of a busy cluster, with a realistic payload
// of about 1 KiB. restarts changes the issue's evidence when it moves.
func finalSaveIssue(index, restarts int, seen time.Time) EvidenceIssue {
	uid := fmt.Sprintf("pod-%06d", index)
	payload, _ := json.Marshal(map[string]any{
		"rule":     "pod.crash_loop",
		"severity": "warning",
		"reason":   "CrashLoopBackOff",
		"restarts": restarts,
		"message":  "back-off restarting failed container app in pod " + uid + ": " + strings.Repeat("m", 760),
		"owner":    map[string]string{"kind": "Deployment", "name": fmt.Sprintf("web-%04d", index%2000)},
	})
	return EvidenceIssue{Key: "issue:" + uid, Namespace: "shop", Kind: "Pod", UID: uid, Rule: "pod.crash_loop", AuthorizationKind: "Pod", LastSeen: seen, Payload: payload}
}

// finalSaveDuration is the duration at quantile q of sorted durations.
func finalSaveDuration(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(q*float64(len(sorted)-1)+0.5)]
}

// The final transaction of a capture saved in parts holds the read models and
// the checkpoint, so it cannot be split further. This saves three captures in
// a row, each exactly at the record limit, onto a cluster that already has
// 5,000 open issues: every capture carries 20,000 changes (half new versions
// of existing events, half new events) and 5,000 issues, of which 1,000 are
// new, 2,000 changed and 2,000 seen again, while 1,000 others resolve. It
// times every transaction of each save and checks that the final one, like
// every part, finishes well inside the 20 s lease, that each whole save
// completes, and that the issues and journal it leaves are right. It runs once
// with planner statistics gathered after the first capture and once before
// the database has gathered any, as in the first minute after a busy cluster
// is connected, until autovacuum's first pass.
func TestPostgresFinalSaveTransactionAtTheCaptureLimitFitsTheLease(t *testing.T) {
	if testing.Short() {
		t.Skip("saves captures at the record limit")
	}
	t.Run("after the statistics are gathered", func(t *testing.T) {
		finalSaveAtTheLimit(t, 5000, 1000, true)
	})
	t.Run("before the first statistics are gathered", func(t *testing.T) {
		finalSaveAtTheLimit(t, 5000, 1000, false)
	})
}

// finalSaveAtTheLimit seeds openIssues open issues, then saves three captures
// at the record limit that each resolve and open churn issues, and times every
// save transaction.
func finalSaveAtTheLimit(t *testing.T, openIssues, churn int, analyze bool) {
	const (
		rounds = 3
		bound  = 5 * time.Second
	)
	changes := MaxEvidenceBatchRecords - openIssues
	p := worstCaseProvider(t, worstCaseDatabase(t), PostgresOptions{})
	scope := EvidenceScope{OrgID: "default", ClusterID: "final-save", ClusterUID: "physical"}
	key, err := evidenceScopeKey(scope)
	if err != nil {
		t.Fatal(err)
	}
	origin := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)

	// The cluster already has openIssues open issues and a first set of events.
	seed := make([]EvidenceRecord, changes/2)
	for i := range seed {
		seed[i] = worstChange(fmt.Sprintf("change:steady-%05d", i), "0", origin, origin, 300)
	}
	seedIssues := make([]EvidenceIssue, openIssues)
	for i := range seedIssues {
		seedIssues[i] = finalSaveIssue(i, 1, origin)
	}
	if err = worstCommit(t.Context(), p, scope, "seed", origin, seed, seedIssues...); err != nil {
		t.Fatalf("seeding the open issues: %v", err)
	}
	if analyze {
		if _, err = p.db.Exec(`ANALYZE`); err != nil {
			t.Fatal(err)
		}
	} else {
		// Hold the database in the state before autovacuum's first pass, which
		// otherwise may or may not land within the test. Only this test's own
		// throwaway database is changed.
		if _, err = p.db.Exec(`DO $$ DECLARE r regclass; BEGIN
 FOR r IN SELECT c.oid::regclass FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind='r' LOOP
 EXECUTE format('ALTER TABLE %s SET (autovacuum_enabled=false)', r); END LOOP; END $$`); err != nil {
			t.Fatal(err)
		}
		var analyzed int
		if err = p.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_user_tables WHERE relname IN ('kubernetes_evidence_issues','kubernetes_evidence_occurrence_read_versions') AND COALESCE(last_analyze,last_autoanalyze) IS NOT NULL`).Scan(&analyzed); err != nil {
			t.Fatal(err)
		}
		if analyzed != 0 {
			t.Fatalf("precondition: %d read-model tables were analyzed before the first capture", analyzed)
		}
	}

	var current struct {
		part, size int
		labelled   bool
	}
	p.evidenceChunkFault = func(ctx context.Context, tx *sql.Tx, part, size int) error {
		current.part, current.size, current.labelled = part, size, true
		return nil
	}
	defer func() { p.evidenceChunkFault = nil }()

	first := 0
	var allParts []time.Duration
	var finals []time.Duration
	for round := 1; round <= rounds; round++ {
		at := origin.Add(time.Duration(round) * 10 * time.Minute)
		records := make([]EvidenceRecord, 0, changes)
		for i := 0; i < changes/2; i++ {
			records = append(records, worstChange(fmt.Sprintf("change:steady-%05d", i), fmt.Sprint(round), at, at, 300))
			records = append(records, worstChange(fmt.Sprintf("change:round%d-%05d", round, i), "created", at, at, 300))
		}
		// The oldest churn issues resolve; the next churn indexes open.
		issues := make([]EvidenceIssue, 0, openIssues)
		for index := first + churn; index < first+churn+openIssues; index++ {
			restarts := 1
			if (index-first)%2 == 0 {
				restarts = round + 1
			}
			issues = append(issues, finalSaveIssue(index, restarts, at))
		}
		cp, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		batch := EvidenceBatch{ID: fmt.Sprintf("limit-%d", round), Expected: cp.ID, Generation: cp.Generation, At: at, CoveredThrough: at, Epoch: "stable", Records: records, Issues: issues}
		if total := len(batch.Records) + len(batch.Issues); total != MaxEvidenceBatchRecords {
			t.Fatalf("capture holds %d records and issues, want exactly the limit %d", total, MaxEvidenceBatchRecords)
		}
		bytes := 0
		for _, record := range batch.Records {
			bytes += len(record.Payload)
		}
		for _, issue := range batch.Issues {
			bytes += len(issue.Payload)
		}
		if bytes > MaxEvidenceBatchBytes {
			t.Fatalf("capture is %d payload bytes, over the byte limit", bytes)
		}
		var transactions []finalSaveTransaction
		batch.Fence = func(ctx context.Context, transaction func() error) error {
			current.part, current.size, current.labelled = 0, 0, false
			started := time.Now()
			err := transaction()
			transactions = append(transactions, finalSaveTransaction{part: current.part, size: current.size, labelled: current.labelled, took: time.Since(started), err: err})
			return err
		}
		before := worstCounts(t, p, scope)
		started := time.Now()
		if err = p.CommitEvidence(t.Context(), scope, batch); err != nil {
			for i, transaction := range transactions {
				t.Logf("capture %d transaction %d: part %d, %d journal records, labelled %t, %s, %v", round, i, transaction.part, transaction.size, transaction.labelled, transaction.took, transaction.err)
			}
			t.Fatalf("capture %d at the limit failed after %s in %d transactions: %v", round, time.Since(started), len(transactions), err)
		}
		whole := time.Since(started)
		after := worstCounts(t, p, scope)

		// The first attempt finds the capture too large for one transaction,
		// then each part is written, then the final transaction.
		if len(transactions) < 3 {
			t.Fatalf("capture %d: %d transactions, want a split, parts and a final transaction", round, len(transactions))
		}
		if !errors.Is(transactions[0].err, errEvidenceSaveSplit) {
			t.Fatalf("capture %d: the first attempt answered %v, want a split into parts", round, transactions[0].err)
		}
		final := transactions[len(transactions)-1]
		if !final.labelled || final.size != 0 || final.err != nil {
			t.Fatalf("capture %d: final transaction wrote %d journal records (%v)", round, final.size, final.err)
		}
		var parts []time.Duration
		written := 0
		for _, part := range transactions[1 : len(transactions)-1] {
			if part.err != nil || !part.labelled {
				t.Fatalf("capture %d: part %d failed or was retried: %v", round, part.part, part.err)
			}
			if part.size > defaultEvidenceSaveChunk {
				t.Fatalf("capture %d: part %d wrote %d records, more than the chunk", round, part.part, part.size)
			}
			parts = append(parts, part.took)
			written += part.size
		}
		allParts = append(allParts, parts...)
		finals = append(finals, final.took)
		sort.Slice(parts, func(i, j int) bool { return parts[i] < parts[j] })
		t.Logf("capture %d at the limit (%d changes + %d issues, %d payload bytes) onto %d journal rows: %d journal records in %d parts, part p50 %s max %s; split check %s; final transaction %s; whole save %s",
			round, len(batch.Records), len(batch.Issues), bytes, before.rows, written, len(parts),
			finalSaveDuration(parts, 0.5), parts[len(parts)-1], transactions[0].took, final.took, whole)

		if transactions[0].took > bound {
			t.Errorf("capture %d: the first attempt took %s, not well inside the 20 s lease", round, transactions[0].took)
		}
		if parts[len(parts)-1] > bound {
			t.Errorf("capture %d: the longest part took %s, not well inside the 20 s lease", round, parts[len(parts)-1])
		}
		if final.took > bound {
			t.Errorf("capture %d: the final transaction took %s, not well inside the 20 s lease", round, final.took)
		}
		if after.rows-before.rows != written {
			t.Fatalf("capture %d: the journal grew by %d rows, the parts wrote %d", round, after.rows-before.rows, written)
		}
		if latest, _ := p.EvidenceCheckpoint(t.Context(), scope); latest.ID != batch.ID {
			t.Fatalf("capture %d: checkpoint %q after the save", round, latest.ID)
		}
		var open, closed int
		if err = p.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE closed_at IS NULL AND NOT observation_only), COUNT(*) FILTER (WHERE closed_at IS NOT NULL) FROM kubernetes_evidence_issues WHERE scope_key=$1`, key).Scan(&open, &closed); err != nil {
			t.Fatal(err)
		}
		if open != openIssues || closed != churn*round {
			t.Fatalf("capture %d: %d open and %d resolved issues, want %d and %d", round, open, closed, openIssues, churn*round)
		}
		first += churn
	}
	sort.Slice(allParts, func(i, j int) bool { return allParts[i] < allParts[j] })
	sort.Slice(finals, func(i, j int) bool { return finals[i] < finals[j] })
	t.Logf("over %d captures at the limit: %d parts, part p50 %s max %s; final transaction p50 %s max %s",
		rounds, len(allParts), finalSaveDuration(allParts, 0.5), allParts[len(allParts)-1], finalSaveDuration(finals, 0.5), finals[len(finals)-1])
	worstJournalConsistent(t, p, scope)
}
