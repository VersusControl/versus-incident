package storage

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

func occurrenceCenterKey(issue EvidenceIssue) string {
	var facts struct {
		BrokeAt *time.Time `json:"broke_at"`
	}
	if json.Unmarshal(issue.Payload, &facts) != nil || facts.BrokeAt == nil {
		return ""
	}
	return issue.UID + "\x00" + facts.BrokeAt.UTC().Format(time.RFC3339Nano)
}

func updateOccurrenceLookups(state *evidenceState) {
	ids := state.updatedOccurrences
	if state.RootLatest == nil {
		state.RootLatest = map[string]string{}
		ids = map[string]struct{}{}
		for id := range state.Occurrences {
			ids[id] = struct{}{}
		}
	}
	if state.RootCenters == nil {
		state.RootCenters = map[string][]string{}
	}
	for id := range ids {
		issue := state.Occurrences[id]
		prior := state.Occurrences[state.RootLatest[issue.UID]]
		if prior.OccurrenceID == "" || issue.LastSeen.After(prior.LastSeen) || issue.LastSeen.Equal(prior.LastSeen) && id > prior.OccurrenceID {
			state.RootLatest[issue.UID] = id
		}
		key := occurrenceCenterKey(issue)
		if key == "" {
			continue
		}
		found := false
		for _, existing := range state.RootCenters[key] {
			found = found || existing == id
		}
		if !found && len(state.RootCenters[key]) < 2 {
			state.RootCenters[key] = append(state.RootCenters[key], id)
		}
	}
}

func lookupOccurrenceRecords(state evidenceState, query EvidenceQuery) []EvidenceRecord {
	ids := []string{}
	if query.OccurrenceID != "" {
		ids = append(ids, query.OccurrenceID)
	} else if !query.Center.IsZero() {
		ids = append(ids, state.RootCenters[query.RootUID+"\x00"+query.Center.UTC().Format(time.RFC3339Nano)]...)
	} else if id := state.RootLatest[query.RootUID]; id != "" {
		ids = append(ids, id)
	}
	records := []EvidenceRecord{}
	for _, id := range ids {
		if issue, ok := state.Occurrences[id]; ok {
			record := occurrenceRecord(issue, occurrenceObservation{At: issue.LastSeen, LastSeen: issue.LastSeen})
			record.At = issue.FirstSeen
			records = append(records, record)
		}
	}
	return records
}

// An observation contains freshness only. Full payloads are versioned only when
// evidence or lifecycle changes; a query pins the revision before pagination.
type occurrenceObservation struct {
	Measurements          json.RawMessage `json:",omitempty"`
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
	state.Observations = append(state.Observations, occurrenceObservation{Revision: revision, At: at, LastSeen: issue.LastSeen, OccurrenceID: issue.OccurrenceID, Version: version, Measurements: issueMeasurements(issue.Payload)})
	if state.LastObservations == nil {
		state.LastObservations = map[string]time.Time{}
	}
	state.LastObservations[issue.OccurrenceID] = at
}

func occurrenceRecord(issue EvidenceIssue, observation occurrenceObservation) EvidenceRecord {
	issue.LastSeen = observation.LastSeen
	issue.Payload = overlayIssueMeasurements(issue.Payload, observation.Measurements)
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
		if query.OccurrenceID != "" && observation.OccurrenceID != query.OccurrenceID {
			continue
		}
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
		if !query.Center.IsZero() {
			var payload struct {
				BrokeAt *time.Time `json:"broke_at"`
			}
			if json.Unmarshal(issue.Payload, &payload) != nil || payload.BrokeAt == nil || !payload.BrokeAt.Equal(query.Center) {
				continue
			}
		}
		record := occurrenceRecord(issue, observation)
		record.At = issue.FirstSeen
		records = append(records, record)
	}
	return records, nil
}

func pruneOccurrenceVersions(state *evidenceState) {
	observed := map[string]bool{}
	active := map[string]bool{}
	for _, observation := range state.Observations {
		observed[observation.OccurrenceID] = true
	}
	for _, issue := range state.Active {
		active[issue.OccurrenceID] = true
	}
	removed := false
	for id, issue := range state.Occurrences {
		if active[id] || observed[id] {
			continue
		}
		delete(state.Occurrences, id)
		at := issue.LastSeen
		if issue.ClosedAt != nil && issue.ClosedAt.After(at) {
			at = *issue.ClosedAt
		}
		if at.After(state.Checkpoint.PrunedThrough) {
			state.Checkpoint.PrunedThrough = at
		}
		state.Checkpoint.PruneRevision++
		removed = true
	}
	if removed {
		state.RootLatest = nil
		state.RootCenters = nil
		updateOccurrenceLookups(state)
	}
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
