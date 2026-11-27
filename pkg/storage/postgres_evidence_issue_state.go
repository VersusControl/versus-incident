package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/VersusControl/versus-incident/pkg/logging"
)

// evidenceIssueRow is one issue row as a save reads it. A row read without
// its payload carries the payload's last_seen alone, in payloadSeen.
type evidenceIssueRow struct {
	occurrenceID, issueKey, version               string
	rootUID, namespace, kind, rule, authorization string
	broken, modelReady                            bool
	firstSeen, lastSeen                           time.Time
	lastObserved                                  sql.NullTime
	payloadSeen                                   string
	payload                                       []byte
}

// evidenceIssueColumns are the columns of a row read in full, in the order
// scanEvidenceIssueRow expects.
const evidenceIssueColumns = `occurrence_id,issue_key,payload_version,root_uid,namespace,kind,rule,authorization_kind,broken,model_ready,first_seen,last_seen,last_observed_at,COALESCE(payload->>'last_seen',''),payload`

func scanEvidenceIssueRow(rows *sql.Rows, withPayload bool) (evidenceIssueRow, error) {
	var row evidenceIssueRow
	targets := []any{&row.occurrenceID, &row.issueKey, &row.version, &row.rootUID, &row.namespace, &row.kind, &row.rule, &row.authorization, &row.broken, &row.modelReady, &row.firstSeen, &row.lastSeen, &row.lastObserved, &row.payloadSeen}
	if withPayload {
		targets = append(targets, &row.payload)
	}
	return row, rows.Scan(targets...)
}

// readEvidenceIssues loads the issues a save applies a capture to: every
// open issue of the scope, and the observation-only occurrences a historical
// capture may extend.
//
// Unless full is set, the open issues are read without their payloads, which
// are most of each row, and a payload is read only for an issue the capture
// can change (see issueNeedsPayload). The others enter the state as
// payloadless issues built from their columns. A save never writes such an
// issue: payloadlessIssuesTouched tells the caller when the capture changed
// one anyway, and the caller then reads every open issue in full.
//
// The open issues and the occurrences named by id are separate statements,
// so each is served by its own index: the open set by its partial index, and
// the named occurrences by the key.
func readEvidenceIssues(ctx context.Context, tx *sql.Tx, key string, batch EvidenceBatch, state *evidenceState, full bool) error {
	wanted := []string{}
	if batch.HistoricalOnly {
		for _, issue := range batch.Issues {
			at := issue.FirstSeen
			if at.IsZero() {
				at = issue.LastSeen
			}
			if at.IsZero() {
				at = batch.At
			}
			wanted = append(wanted, evidenceDigest(issue.Key, "observation", at.UTC().Format("2006-01-02")))
		}
	}
	columns := evidenceIssueColumns
	if !full {
		columns = columns[:len(columns)-len(",payload")]
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM kubernetes_evidence_issues WHERE scope_key=$1 AND closed_at IS NULL AND NOT observation_only`, key)
	if err != nil {
		return err
	}
	present := evidenceBatchIssueKeys(batch)
	open := []evidenceIssueRow{}
	for rows.Next() {
		row, err := scanEvidenceIssueRow(rows, full)
		if err != nil {
			rows.Close()
			return err
		}
		if !full && issuePayloadRule(row, batch, present) {
			wanted = append(wanted, row.occurrenceID)
		}
		open = append(open, row)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	loaded := map[string]bool{}
	if len(wanted) > 0 {
		rows, err = tx.QueryContext(ctx, `SELECT `+evidenceIssueColumns+` FROM kubernetes_evidence_issues WHERE scope_key=$1 AND occurrence_id=ANY($2::text[])`, key, wanted)
		if err != nil {
			return err
		}
		for rows.Next() {
			row, err := scanEvidenceIssueRow(rows, true)
			if err != nil {
				rows.Close()
				return err
			}
			if err = addStoredIssue(state, row); err != nil {
				rows.Close()
				return err
			}
			loaded[row.occurrenceID] = true
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	for _, row := range open {
		if loaded[row.occurrenceID] {
			continue
		}
		if err = addStoredIssue(state, row); err != nil {
			return err
		}
	}
	return nil
}

// issuePayloadRule decides which open issues the narrow read loads with their
// payload; tests replace it to exercise the full re-read.
var issuePayloadRule = issueNeedsPayload

// evidenceBatchIssueKeys are the keys of the open issues a live capture
// reports; a historical capture reports none.
func evidenceBatchIssueKeys(batch EvidenceBatch) map[string]bool {
	keys := map[string]bool{}
	if batch.HistoricalOnly {
		return keys
	}
	for _, issue := range batch.Issues {
		keys[issue.Key] = true
	}
	return keys
}

// issueNeedsPayload reports whether applying the capture can change an open
// issue, or the save can rewrite its row whole, so its payload must be read:
// the capture reports it, the capture's coverage can resolve it, its row is
// not a complete read model or has no version, or its payload trails its
// last_seen by a heartbeat or more. Any other open issue is left as stored.
func issueNeedsPayload(row evidenceIssueRow, batch EvidenceBatch, present map[string]bool) bool {
	if row.payload != nil || !row.modelReady || row.version == "" || present[row.issueKey] {
		return true
	}
	if !batch.CoveredThrough.IsZero() && row.lastSeen.Before(batch.CoveredThrough) {
		return true
	}
	payloadSeen, ok := parsePayloadSeen(row.payloadSeen)
	return !ok || row.lastSeen.Sub(payloadSeen) >= evidenceHeartbeat(batch.ObservationHeartbeat)
}

func parsePayloadSeen(value string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, value)
	return at, err == nil
}

func evidenceHeartbeat(heartbeat time.Duration) time.Duration {
	if heartbeat <= 0 {
		return 5 * time.Minute
	}
	return heartbeat
}

// addStoredIssue adds one issue row to the state a save applies a capture to.
// A row without a payload enters as a payloadless issue built from its
// columns, which only stands in for the stored row and is never written.
func addStoredIssue(state *evidenceState, row evidenceIssueRow) error {
	var issue EvidenceIssue
	var payloadSeen time.Time
	payloadless := row.payload == nil
	if payloadless {
		payloadSeen, _ = parsePayloadSeen(row.payloadSeen)
		issue = EvidenceIssue{OccurrenceID: row.occurrenceID, Key: row.issueKey, UID: row.rootUID, Namespace: row.namespace, Kind: row.kind, Rule: row.rule, AuthorizationKind: row.authorization, Broken: row.broken, FirstSeen: row.firstSeen}
	} else {
		if err := json.Unmarshal(row.payload, &issue); err != nil {
			return err
		}
		payloadSeen = issue.LastSeen
	}
	issue.LastSeen = row.lastSeen
	state.storedIssues[issue.OccurrenceID] = storedIssue{issue: issue, payloadSeen: payloadSeen, version: row.version, lastObserved: row.lastObserved.Time, modelReady: row.modelReady, payloadless: payloadless}
	if !issue.ObservationOnly {
		state.Active[issue.Key] = issue
	}
	state.Occurrences[issue.OccurrenceID] = issue
	state.OccurrenceHeads[issue.OccurrenceID] = row.version
	if state.LastObservations == nil {
		state.LastObservations = map[string]time.Time{}
	}
	state.LastObservations[issue.OccurrenceID] = row.lastObserved.Time
	if row.version == "" {
		addOccurrenceObservation(state, issue, state.Checkpoint.OccurrenceRevision, true)
	}
	return nil
}

// payloadlessIssuesTouched reports whether applying a capture changed an
// issue that was read without its payload, or left it in a state the save
// would rewrite whole. Either means the payload was needed after all.
func payloadlessIssuesTouched(state evidenceState, heartbeat time.Duration) bool {
	heartbeat = evidenceHeartbeat(heartbeat)
	payloadless := map[string]bool{}
	for id, stored := range state.storedIssues {
		if !stored.payloadless {
			continue
		}
		payloadless[id] = true
		if _, updated := state.updatedOccurrences[id]; updated {
			return true
		}
		issue := state.Occurrences[id]
		if issue.ClosedAt != nil || !issue.LastSeen.Equal(stored.issue.LastSeen) || !storedPayloadFresh(stored, issue, state.OccurrenceHeads[id], heartbeat) {
			return true
		}
	}
	if len(payloadless) == 0 {
		return false
	}
	for _, observation := range state.Observations {
		if payloadless[observation.OccurrenceID] {
			return true
		}
	}
	for _, issue := range state.OccurrenceVersions {
		if payloadless[issue.OccurrenceID] {
			return true
		}
	}
	return false
}

// noteFullIssueRead counts a save that read every open issue in full after the
// narrow read: a capture changed an issue the narrow read left without its
// payload. It should be rare; the count shows how rare.
func (p *postgresProvider) noteFullIssueRead() {
	p.writeMu.Lock()
	p.fullIssueReads++
	count := p.fullIssueReads
	p.writeMu.Unlock()
	logging.Debugf("storage: evidence save read every open issue in full after the narrow read (%d times)", count)
}

// fullIssueReadCount is how many saves fell back to the full issue read.
func (p *postgresProvider) fullIssueReadCount() int {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.fullIssueReads
}
