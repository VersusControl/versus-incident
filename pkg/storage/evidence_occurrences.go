package storage

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// An observation contains freshness only. Full payloads are versioned only when
// evidence or lifecycle changes; a query pins the revision before pagination.
type occurrenceObservation struct {
	Revision              uint64
	At, LastSeen          time.Time
	OccurrenceID, Version string
}

func addOccurrenceObservation(state *evidenceState, issue EvidenceIssue, revision uint64, meaningful bool) {
	if state.OccurrenceVersions == nil {
		state.OccurrenceVersions = map[string]EvidenceIssue{}
	}
	if state.OccurrenceHeads == nil {
		state.OccurrenceHeads = map[string]string{}
	}
	version := state.OccurrenceHeads[issue.OccurrenceID]
	if meaningful {
		raw, _ := json.Marshal(issue)
		version = evidenceDigest(issue.OccurrenceID, string(raw))
		state.OccurrenceVersions[version] = issue
		state.OccurrenceHeads[issue.OccurrenceID] = version
	}
	at := issue.LastSeen
	if issue.ClosedAt != nil && issue.ClosedAt.After(at) {
		at = *issue.ClosedAt
	}
	state.Observations = append(state.Observations, occurrenceObservation{Revision: revision, At: at, LastSeen: issue.LastSeen, OccurrenceID: issue.OccurrenceID, Version: version})
}

func occurrenceRecord(issue EvidenceIssue, observation occurrenceObservation) EvidenceRecord {
	issue.LastSeen = observation.LastSeen
	raw, _ := json.Marshal(issue)
	kind := "issue_opened"
	if issue.ObservationOnly {
		kind = "issue_observed"
	}
	if issue.ClosedAt != nil {
		kind = "issue_resolved"
	}
	return EvidenceRecord{ID: issue.OccurrenceID, Type: kind, At: issue.LastSeen, ObservedAt: observation.At, Namespace: issue.Namespace, Kind: issue.Kind, AuthorizationKind: issue.AuthorizationKind, UID: issue.UID, RootUID: issue.UID, Rule: issue.Rule, Epoch: issue.Epoch, Broken: issue.Broken, Payload: raw}
}

func occurrenceVariantMatches(issue EvidenceIssue, variant string) bool {
	severity, broken, _ := strings.Cut(variant, ":")
	if broken == "broken" && !issue.Broken {
		return false
	}
	if severity != "" {
		var payload struct {
			Severity string `json:"severity"`
		}
		if json.Unmarshal(issue.Payload, &payload) != nil || payload.Severity != severity {
			return false
		}
	}
	return true
}

func occurrenceRecords(state evidenceState, high uint64, query EvidenceQuery) ([]EvidenceRecord, error) {
	latest := map[string]occurrenceObservation{}
	for _, observation := range state.Observations {
		if observation.Revision > high || observation.At.After(query.Until) {
			continue
		}
		before, exists := latest[observation.OccurrenceID]
		if !exists || observation.Revision > before.Revision || observation.Revision == before.Revision && observation.At.After(before.At) {
			latest[observation.OccurrenceID] = observation
		}
	}
	records := make([]EvidenceRecord, 0, len(latest))
	for _, observation := range latest {
		issue, ok := state.OccurrenceVersions[observation.Version]
		if !ok {
			return nil, ErrInvalidEvidence
		}
		records = append(records, occurrenceRecord(issue, observation))
	}
	return records, nil
}

func pruneOccurrenceVersions(state *evidenceState) {
	referenced := map[string]bool{}
	for _, observation := range state.Observations {
		referenced[observation.Version] = true
	}
	for _, issue := range state.Active {
		referenced[state.OccurrenceHeads[issue.OccurrenceID]] = true
	}
	for version := range state.OccurrenceVersions {
		if !referenced[version] {
			delete(state.OccurrenceVersions, version)
		}
	}
	for occurrence, version := range state.OccurrenceHeads {
		if !referenced[version] {
			delete(state.OccurrenceHeads, occurrence)
		}
	}
}

func sortOccurrenceHistory(state *evidenceState) {
	sort.SliceStable(state.Observations, func(i, j int) bool { return state.Observations[i].At.Before(state.Observations[j].At) })
}

// The existing explicit file budget covers compact observations as well as
// journal rows. Removing either invalidates pinned pages, never silently rebases.
func dropOldestEvidence(state *evidenceState) int {
	var at time.Time
	var raw []byte
	if len(state.Observations) > 0 && (len(state.Records) == 0 || state.Observations[0].At.Before(evidenceRetentionTime(state.Records[0]))) {
		at = state.Observations[0].At
		raw, _ = json.Marshal(state.Observations[0])
		state.Observations = state.Observations[1:]
	} else if len(state.Records) > 0 {
		at = state.Records[0].At
		raw, _ = json.Marshal(state.Records[0])
		state.Records = state.Records[1:]
	} else {
		return 0
	}
	if at.After(state.Checkpoint.PrunedThrough) {
		state.Checkpoint.PrunedThrough = at
	}
	state.Checkpoint.PruneRevision++
	return len(raw) + 1
}

func pruneOccurrenceHistory(state *evidenceState, policy EvidencePolicy, at time.Time) {
	sortOccurrenceHistory(state)
	if policy.Retention > 0 {
		cutoff := at.Add(-policy.Retention)
		for len(state.Observations) > 0 && state.Observations[0].At.Before(cutoff) {
			dropOldestEvidence(state)
		}
	}
	for policy.MaxRecords > 0 && len(state.Records)+len(state.Observations) > policy.MaxRecords {
		dropOldestEvidence(state)
	}
	pruneOccurrenceVersions(state)
}
