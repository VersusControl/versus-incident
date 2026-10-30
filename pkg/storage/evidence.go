package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrInvalidEvidence = errors.New("storage: invalid evidence data")

var ErrEvidenceConflict = errors.New("storage: evidence checkpoint conflict")
var ErrEvidenceLimit = errors.New("storage: evidence operation exceeds budget")

// EvidenceStore is optional. Scope is explicit even when a provider is wrapped.
// Implementations atomically commit observations, lifecycle events and checkpoint.
// Query bounds limit work, never the history retained by a database.
type EvidenceStore interface {
	EvidencePolicy() EvidencePolicy
	CommitEvidence(context.Context, EvidenceScope, EvidenceBatch) error
	QueryEvidence(context.Context, EvidenceScope, EvidenceQuery) (EvidencePage, error)
	EvidenceCheckpoint(context.Context, EvidenceScope) (EvidenceCheckpoint, error)
	DeleteEvidence(context.Context, EvidenceScope) error
}

type EvidencePolicy struct {
	Mode       string        `json:"mode"`
	Retention  time.Duration `json:"retention,omitempty"`
	MaxRecords int           `json:"max_records,omitempty"`
	MaxBytes   int           `json:"max_bytes,omitempty"`
}

type EvidenceScope struct {
	ReadOrgs  []string
	OrgID     string
	ClusterID string
	// ClusterUID is independently observed physical cluster identity, not a role,
	// profile, bearer fingerprint, display name or user-supplied namespace.
	ClusterUID string
}

type EvidenceRecord struct {
	AuthorizationKind string `json:"authorization_kind,omitempty"`
	Priority          int    `json:"priority,omitempty"`
	GapFrom, GapTo    time.Time
	Broken            bool            `json:"broken,omitempty"`
	Name              string          `json:"name,omitempty"`
	Sequence          uint64          `json:"sequence,omitempty"`
	ID                string          `json:"id"`
	Type              string          `json:"type"`
	At                time.Time       `json:"at"`
	ObservedAt        time.Time       `json:"observed_at"`
	Namespace         string          `json:"namespace,omitempty"`
	Kind              string          `json:"kind,omitempty"`
	UID               string          `json:"uid,omitempty"`
	RootUID           string          `json:"root_uid,omitempty"`
	PodUID            string          `json:"pod_uid,omitempty"`
	Rule              string          `json:"rule,omitempty"`
	Epoch             string          `json:"epoch"`
	Payload           json.RawMessage `json:"payload"`
}

type EvidenceIssue struct {
	AuthorizationKind string          `json:"authorization_kind,omitempty"`
	ObservationOnly   bool            `json:"observation_only,omitempty"`
	Broken            bool            `json:"broken,omitempty"`
	Key               string          `json:"key"`
	OccurrenceID      string          `json:"occurrence_id"`
	Namespace         string          `json:"namespace,omitempty"`
	Kind              string          `json:"kind"`
	UID               string          `json:"uid"`
	Rule              string          `json:"rule"`
	FirstSeen         time.Time       `json:"first_seen"`
	LastSeen          time.Time       `json:"last_seen"`
	ClosedAt          *time.Time      `json:"closed_at,omitempty"`
	Epoch             string          `json:"epoch"`
	Payload           json.RawMessage `json:"payload"`
}

type EvidenceInterval struct{ From, To time.Time }

type EvidenceCheckpoint struct {
	OccurrenceRevision uint64 `json:"occurrence_revision,omitempty"`
	// Generation survives explicit deletion and fences captures prepared before it.
	Generation     uint64                      `json:"generation,omitempty"`
	PruneRevision  uint64                      `json:"prune_revision,omitempty"`
	Coverage       map[string]EvidenceInterval `json:"coverage,omitempty"`
	StartedAt      time.Time                   `json:"started_at,omitempty"`
	Kinds          []string                    `json:"kinds,omitempty"`
	ID             string                      `json:"id"`
	At             time.Time                   `json:"at"`
	CoveredThrough time.Time                   `json:"covered_through"`
	// PrunedThrough is file-only lost history, not an invented database window.
	PrunedThrough time.Time `json:"pruned_through,omitempty"`
}

type EvidenceBatch struct {
	Generation     uint64
	HistoricalOnly bool
	Coverage       map[string]EvidenceInterval
	GuardKey       string
	GuardExpected  []byte
	ID             string
	Expected       string
	At             time.Time
	Epoch          string
	// Only sufficient successful observations may advance CoveredThrough.
	// A partial batch can add findings but cannot close absent issues.
	CoveredThrough time.Time
	Records        []EvidenceRecord
	Issues         []EvidenceIssue
}

type EvidenceQuery struct {
	// Projection transforms unavailable records into explicit gaps before counts
	// and pagination. Identity and ordering fields must remain unchanged.
	Projection                                  func(EvidenceRecord) EvidenceRecord
	ProjectionID                                string
	Variant                                     string
	WithGaps                                    bool
	Name, ResourceUID                           string
	ByObservation                               bool
	Since, Until                                time.Time
	Namespace, Kind, UID, RootUID, PodUID, Rule string
	// Type "occurrence" returns latest occurrence observations at a pinned
	// revision; "issue" retains the immutable lifecycle journal view.
	Type   string
	Limit  int
	Cursor string
}

type EvidencePage struct {
	Since, Until time.Time
	Total        int
	Counts       map[string]int
	Items        []EvidenceRecord   `json:"items"`
	Next         string             `json:"next,omitempty"`
	Checkpoint   EvidenceCheckpoint `json:"checkpoint"`
	Policy       EvidencePolicy     `json:"policy"`
}

type evidenceState struct {
	OccurrenceVersions map[string]EvidenceIssue
	OccurrenceHeads    map[string]string
	Observations       []occurrenceObservation
	Latest             map[string]uint64
	Sequence           uint64
	Captures           map[string]time.Time
	Checkpoint         EvidenceCheckpoint
	Active             map[string]EvidenceIssue
	Records            []EvidenceRecord
}

type evidenceCursor struct {
	CaptureScope    string
	OccurrenceHighs []uint64
	Generations     []uint64
	PruneRevisions  []uint64
	Highs           []uint64
	Pruned          []time.Time
	Since, Until    time.Time
	Binding         string
	PrunedAt        time.Time
	High            uint64
	At              time.Time
	ID              string
}

func evidenceDigest(values ...string) string {
	raw, _ := json.Marshal(values)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func evidenceReadScopes(scope EvidenceScope) ([]EvidenceScope, error) {
	orgs := scope.ReadOrgs
	if len(orgs) == 0 {
		orgs = []string{scope.OrgID}
	}
	if len(orgs) > 2 {
		return nil, ErrInvalidEvidence
	}
	result := []EvidenceScope{}
	seen := map[string]bool{}
	for i, org := range orgs {
		org = NormalizeOrgID(org)
		if seen[org] || i == 0 && org != NormalizeOrgID(scope.OrgID) {
			return nil, ErrInvalidEvidence
		}
		seen[org] = true
		item := scope
		item.OrgID = org
		item.ReadOrgs = nil
		result = append(result, item)
	}
	return result, nil
}

func mergeEvidenceCheckpoint(current, next EvidenceCheckpoint) EvidenceCheckpoint {
	if current.ID == "" {
		return next
	}
	if !next.StartedAt.IsZero() && (current.StartedAt.IsZero() || next.StartedAt.Before(current.StartedAt)) {
		current.StartedAt = next.StartedAt
	}
	if next.PrunedThrough.After(current.PrunedThrough) {
		current.PrunedThrough = next.PrunedThrough
	}
	kinds := map[string]bool{}
	for _, kind := range current.Kinds {
		kinds[kind] = true
	}
	for _, kind := range next.Kinds {
		kinds[kind] = true
	}
	current.Kinds = nil
	for kind := range kinds {
		current.Kinds = append(current.Kinds, kind)
	}
	sort.Strings(current.Kinds)
	return current
}

func evidenceScopeKey(scope EvidenceScope) (string, error) {
	scope.OrgID = NormalizeOrgID(scope.OrgID)
	for _, value := range []string{scope.OrgID, scope.ClusterID, scope.ClusterUID} {
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return "", ErrInvalidEvidence
		}
	}
	return "kubernetes/captured-evidence/" + evidenceDigest(scope.OrgID, scope.ClusterID, scope.ClusterUID), nil
}

// Validate bounds one operation without imposing any retained-history limit.
func (batch EvidenceBatch) Validate() error { return validEvidenceBatch(batch) }

func validEvidenceBatch(batch EvidenceBatch) error {
	if len(batch.GuardKey) > 1024 || len(batch.GuardExpected) > 16<<10 || len(batch.Expected) > 256 {
		return ErrEvidenceLimit
	}
	if batch.HistoricalOnly && (!batch.CoveredThrough.IsZero() || len(batch.Coverage) > 0) {
		return ErrInvalidEvidence
	}
	if batch.ID == "" || len(batch.ID) > 256 || batch.Epoch == "" || len(batch.Epoch) > 256 || batch.At.IsZero() || batch.CoveredThrough.After(batch.At) {
		return ErrInvalidEvidence
	}
	for kind, interval := range batch.Coverage {
		if kind == "" || len(kind) > 256 || interval.From.IsZero() || interval.To.Before(interval.From) || interval.To.After(batch.At) {
			return ErrInvalidEvidence
		}
	}
	if len(batch.Records)+len(batch.Issues) > 10000 {
		return ErrEvidenceLimit
	}
	size := 0
	for _, record := range batch.Records {
		if !validEvidenceRecord(record) || record.ObservedAt.After(batch.At) {
			return ErrInvalidEvidence
		}
		size += len(record.Payload)
	}
	keys := map[string]bool{}
	for _, issue := range batch.Issues {
		if issue.Key == "" || issue.UID == "" || issue.Kind == "" || issue.Rule == "" || len(issue.Key) > 256 || len(issue.Payload) > 64<<10 || !json.Valid(issue.Payload) || keys[issue.Key] {
			return ErrInvalidEvidence
		}
		for _, value := range []string{issue.UID, issue.Kind, issue.Rule, issue.Namespace, issue.AuthorizationKind} {
			if len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
				return ErrInvalidEvidence
			}
		}
		if issue.LastSeen.After(batch.At) || !issue.FirstSeen.IsZero() && issue.FirstSeen.After(issue.LastSeen) {
			return ErrInvalidEvidence
		}
		keys[issue.Key] = true
		size += len(issue.Payload)
	}
	if size > 8<<20 {
		return ErrEvidenceLimit
	}
	return nil
}

func validEvidenceRecord(record EvidenceRecord) bool {
	if record.Priority < 0 || record.Priority > 100 {
		return false
	}
	if record.ID == "" || record.At.IsZero() || record.ObservedAt.IsZero() || record.At.After(record.ObservedAt) || record.Epoch == "" || !json.Valid(record.Payload) || len(record.Payload) > 64<<10 {
		return false
	}
	if record.Type == "coverage_gap" && (record.GapFrom.IsZero() || record.GapTo.Before(record.GapFrom)) {
		return false
	}
	switch record.Type {
	case "change", "crash", "issue_opened", "issue_observed", "issue_resolved", "coverage_gap":
	default:
		return false
	}
	for _, value := range []string{record.ID, record.Namespace, record.Name, record.Kind, record.AuthorizationKind, record.UID, record.RootUID, record.PodUID, record.Rule, record.Epoch} {
		if len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return true
}

// applyEvidence is shared by the transactional and CAS-backed implementations.
// Active state is never subject to file history pruning.
func applyEvidence(state *evidenceState, batch EvidenceBatch) ([]EvidenceRecord, error) {
	if state.Checkpoint.Generation != batch.Generation {
		return nil, ErrEvidenceConflict
	}
	if state.Checkpoint.ID == batch.ID {
		return nil, nil
	}
	if state.Checkpoint.ID != batch.Expected || batch.At.Before(state.Checkpoint.At) {
		return nil, ErrEvidenceConflict
	}
	for _, active := range state.Active {
		if state.OccurrenceHeads[active.OccurrenceID] == "" {
			return nil, ErrInvalidEvidence
		}
	}
	if state.Checkpoint.OccurrenceRevision >= uint64(1<<63-1) {
		return nil, ErrEvidenceLimit
	}
	state.Checkpoint.OccurrenceRevision++
	if state.Active == nil {
		state.Active = map[string]EvidenceIssue{}
	}
	kindSet := map[string]bool{}
	for _, kind := range state.Checkpoint.Kinds {
		kindSet[kind] = true
	}
	for _, record := range batch.Records {
		kind := record.AuthorizationKind
		if kind == "" {
			kind = record.Kind
		}
		if kind != "" {
			kindSet[kind] = true
		}
	}
	for _, issue := range batch.Issues {
		kind := issue.AuthorizationKind
		if kind == "" {
			kind = issue.Kind
		}
		kindSet[kind] = true
	}
	state.Checkpoint.Kinds = nil
	for kind := range kindSet {
		state.Checkpoint.Kinds = append(state.Checkpoint.Kinds, kind)
	}
	sort.Strings(state.Checkpoint.Kinds)
	added := append([]EvidenceRecord(nil), batch.Records...)
	present := map[string]bool{}
	for _, observed := range batch.Issues {
		if batch.HistoricalOnly {
			observed.ObservationOnly = true
			observed.Epoch = batch.Epoch
			if observed.LastSeen.IsZero() {
				observed.LastSeen = batch.At
			}
			if observed.FirstSeen.IsZero() {
				observed.FirstSeen = observed.LastSeen
			}
			observed.OccurrenceID = evidenceDigest(batch.ID, observed.Key, "observation")
			addOccurrenceObservation(state, observed, state.Checkpoint.OccurrenceRevision, true)
			added = append(added, issueEvidence(observed, batch, "issue_observed"))
			continue
		}
		present[observed.Key] = true
		if observed.LastSeen.IsZero() {
			observed.LastSeen = batch.At
		}
		active, exists := state.Active[observed.Key]
		if exists && observed.LastSeen.Before(active.LastSeen) {
			continue
		}
		advanced := !exists || observed.LastSeen.After(active.LastSeen)
		changed := !exists || active.Broken != observed.Broken || active.AuthorizationKind != observed.AuthorizationKind || !sameIssueEvidence(active.Payload, observed.Payload)
		eventType := "issue_observed"
		if !exists {
			active = observed
			active.OccurrenceID = evidenceDigest(batch.ID, observed.Key)
			active.FirstSeen = observed.FirstSeen
			if active.FirstSeen.IsZero() {
				active.FirstSeen = observed.LastSeen
			}
			eventType = "issue_opened"
		}
		active.AuthorizationKind = observed.AuthorizationKind
		active.Broken = observed.Broken
		active.LastSeen, active.Epoch, active.Payload = observed.LastSeen, batch.Epoch, append(json.RawMessage(nil), observed.Payload...)
		state.Active[observed.Key] = active
		if changed || advanced {
			addOccurrenceObservation(state, active, state.Checkpoint.OccurrenceRevision, changed)
		}
		if changed {
			added = append(added, issueEvidence(active, batch, eventType))
		}
	}
	if !batch.CoveredThrough.IsZero() {
		for key, active := range state.Active {
			if present[key] || !active.LastSeen.Before(batch.CoveredThrough) {
				continue
			}
			closed := batch.CoveredThrough
			active.ClosedAt = &closed
			addOccurrenceObservation(state, active, state.Checkpoint.OccurrenceRevision, true)
			added = append(added, issueEvidence(active, batch, "issue_resolved"))
			delete(state.Active, key)
		}
	}
	if state.Checkpoint.Coverage == nil {
		state.Checkpoint.Coverage = map[string]EvidenceInterval{}
	}
	for kind, interval := range batch.Coverage {
		previous, exists := state.Checkpoint.Coverage[kind]
		if !exists || interval.To.After(previous.To) {
			if exists && previous.From.Before(interval.From) {
				interval.From = previous.From
			}
			state.Checkpoint.Coverage[kind] = interval
		}
	}
	if state.Checkpoint.StartedAt.IsZero() {
		state.Checkpoint.StartedAt = batch.At
	}
	state.Checkpoint.ID, state.Checkpoint.At = batch.ID, batch.At
	if batch.CoveredThrough.After(state.Checkpoint.CoveredThrough) {
		state.Checkpoint.CoveredThrough = batch.CoveredThrough
	}
	return added, nil
}

// Poll timestamps describe occurrence freshness, not a new evidence payload.
// Journal rows stay immutable so a pinned page never acquires later poll data.
func sameIssueEvidence(a, b json.RawMessage) bool {
	normalize := func(raw json.RawMessage) []byte {
		var value map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil || value == nil {
			return raw
		}
		delete(value, "first_seen")
		delete(value, "last_seen")
		normalized, _ := json.Marshal(value)
		return normalized
	}
	return bytes.Equal(normalize(a), normalize(b))
}

func issueEvidence(issue EvidenceIssue, batch EvidenceBatch, eventType string) EvidenceRecord {
	raw, _ := json.Marshal(issue)
	at := issue.LastSeen
	id := evidenceDigest(issue.OccurrenceID, batch.ID, eventType)
	if eventType == "issue_observed" {
		id = evidenceDigest(issue.OccurrenceID, eventType, issue.LastSeen.UTC().Format(time.RFC3339Nano))
	}
	if issue.ClosedAt != nil {
		at = *issue.ClosedAt
	}
	return EvidenceRecord{AuthorizationKind: issue.AuthorizationKind, Broken: issue.Broken, ID: id, Type: eventType, At: at, ObservedAt: batch.At, Namespace: issue.Namespace, Kind: issue.Kind, UID: issue.UID, RootUID: issue.UID, Rule: issue.Rule, Epoch: batch.Epoch, Payload: raw}

}

func prepareEvidenceQuery(scope EvidenceScope, query EvidenceQuery) (EvidenceQuery, evidenceCursor, error) {
	if query.Projection != nil && query.ProjectionID == "" || query.Projection == nil && query.ProjectionID != "" || len(query.ProjectionID) > 256 {
		return query, evidenceCursor{}, ErrInvalidEvidence
	}
	captureScope, err := evidenceScopeKey(scope)
	if err != nil {
		return query, evidenceCursor{}, err
	}
	if query.Type == "timeline" || query.Type == "break" {
		query.WithGaps = true
	}
	if len(query.Cursor) > 4096 {
		return query, evidenceCursor{}, ErrInvalidEvidence
	}
	cursor := evidenceCursor{}
	if query.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.CaptureScope != captureScope || cursor.At.IsZero() || cursor.ID == "" || cursor.Since.IsZero() || cursor.Until.IsZero() {
			return query, cursor, ErrInvalidEvidence
		}
		if query.Since.IsZero() {
			query.Since = cursor.Since
		}
		if query.Until.IsZero() {
			query.Until = cursor.Until
		}
	}
	if query.Since.IsZero() || query.Until.IsZero() || query.Since.After(query.Until) {
		return query, cursor, ErrInvalidEvidence
	}
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > 500 {
		query.Limit = 500
	}
	for _, value := range []string{query.Namespace, query.Kind, query.UID, query.RootUID, query.PodUID, query.Rule, query.Type, query.Name, query.ResourceUID, query.Variant} {
		if len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return query, cursor, ErrInvalidEvidence
		}
	}
	readScopes, scopeErr := evidenceReadScopes(scope)
	if scopeErr != nil {
		return query, cursor, scopeErr
	}
	orgBinding := []string{}
	for _, readScope := range readScopes {
		orgBinding = append(orgBinding, readScope.OrgID)
	}
	binding := evidenceDigest(strings.Join(orgBinding, "\x00"), NormalizeOrgID(scope.OrgID), scope.ClusterID, scope.ClusterUID, query.Since.UTC().Format(time.RFC3339Nano), query.Until.UTC().Format(time.RFC3339Nano), query.Namespace, query.Kind, query.UID, query.RootUID, query.PodUID, query.Rule, query.Type, query.Name, query.ResourceUID, strconv.FormatBool(query.ByObservation), strconv.FormatBool(query.WithGaps), query.Variant)
	if query.ProjectionID != "" {
		binding = evidenceDigest(binding, query.ProjectionID)
	}
	if query.Cursor != "" && cursor.Binding != binding {
		return query, cursor, ErrInvalidEvidence
	}
	cursor.CaptureScope = captureScope
	cursor.Binding, cursor.Since, cursor.Until = binding, query.Since, query.Until
	return query, cursor, nil
}

func evidenceTypeMatches(recordType, filter string, broken bool) bool {
	switch filter {
	case "":
		return true
	case "timeline":
		return recordType == "change" || recordType == "crash" || recordType == "coverage_gap" || broken && (recordType == "issue_opened" || recordType == "issue_resolved")
	case "break":
		return broken && (recordType == "issue_opened" || recordType == "issue_resolved")
	case "issue", "occurrence":
		return recordType == "issue_opened" || recordType == "issue_observed" || recordType == "issue_resolved"
	default:
		return recordType == filter
	}
}

func evidenceRetentionTime(record EvidenceRecord) time.Time {
	if record.Type == "crash" {
		return record.ObservedAt
	}
	return record.At
}

func evidenceTime(record EvidenceRecord, query EvidenceQuery) time.Time {
	if query.ByObservation {
		return record.ObservedAt
	}
	return record.At
}

func evidenceMatches(record EvidenceRecord, query EvidenceQuery, cursor evidenceCursor) bool {
	if record.Type == "coverage_gap" && query.WithGaps {
		if record.GapTo.Before(query.Since) || record.GapFrom.After(query.Until) {
			return false
		}
		if query.Namespace != "" && record.Namespace != "" && record.Namespace != query.Namespace || query.Kind != "" && record.Kind != "" && record.Kind != query.Kind {
			return false
		}
	} else {
		at := evidenceTime(record, query)
		if at.Before(query.Since) || at.After(query.Until) {
			return false
		}
		if query.Namespace != "" && record.Namespace != query.Namespace || query.Kind != "" && record.Kind != query.Kind || query.UID != "" && record.UID != query.UID || query.RootUID != "" && record.RootUID != query.RootUID || query.PodUID != "" && record.PodUID != query.PodUID || query.Rule != "" && record.Rule != query.Rule || !evidenceTypeMatches(record.Type, query.Type, record.Broken) || query.Name != "" && record.Name != query.Name || query.ResourceUID != "" && record.UID != query.ResourceUID && record.RootUID != query.ResourceUID && record.PodUID != query.ResourceUID {
			return false
		}
	}
	at := evidenceTime(record, query)
	return cursor.At.IsZero() || at.Before(cursor.At) || at.Equal(cursor.At) && record.ID < cursor.ID
}

func finishEvidencePage(page EvidencePage, query EvidenceQuery, cursor evidenceCursor) EvidencePage {
	end, bytes := 0, 0
	for end < len(page.Items) && end < query.Limit {
		raw, _ := json.Marshal(page.Items[end])
		if bytes+len(raw) > 1<<20 {
			break
		}
		bytes += len(raw)
		end++
	}
	if end < len(page.Items) && end > 0 {
		page.Items = page.Items[:end]
		last := page.Items[len(page.Items)-1]
		cursor.At, cursor.ID = evidenceTime(last, query), last.ID
		raw, _ := json.Marshal(cursor)
		page.Next = base64.RawURLEncoding.EncodeToString(raw)
	}

	if page.Items == nil {
		page.Items = []EvidenceRecord{}
	}
	return page
}

func blobEvidenceCommit(ctx context.Context, p Provider, policy EvidencePolicy, scope EvidenceScope, batch EvidenceBatch) error {
	key, err := evidenceScopeKey(scope)
	if err != nil {
		return err
	}
	if err = validEvidenceBatch(batch); err != nil {
		return err
	}
	cas, ok := p.(BlobCAS)
	if !ok {
		return ErrUnsupported
	}
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := p.ReadBlob(key)
		if err != nil {
			return err
		}
		state := evidenceState{}
		if len(raw) > 0 && json.Unmarshal(raw, &state) != nil {
			return ErrInvalidEvidence
		}
		if state.Checkpoint.Generation != batch.Generation {
			return ErrEvidenceConflict
		}
		if state.Checkpoint.ID == batch.ID {
			return nil
		}
		if _, seen := state.Captures[batch.ID]; seen {
			return nil
		}
		added, err := applyEvidence(&state, batch)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		priority := map[string]int{}
		latestObservation := map[string]time.Time{}
		if state.Latest == nil {
			state.Latest = map[string]uint64{}
		}
		for _, record := range state.Records {
			seen[record.ID+"\x00"+evidenceVersion(record)] = true
			if record.Priority > priority[record.ID] || record.Priority == priority[record.ID] && record.ObservedAt.After(latestObservation[record.ID]) {
				priority[record.ID] = record.Priority
				latestObservation[record.ID] = record.ObservedAt
			}
		}
		for _, record := range added {
			version := record.ID + "\x00" + evidenceVersion(record)
			if seen[version] || record.Priority < priority[record.ID] || record.Priority == priority[record.ID] && record.ObservedAt.Before(latestObservation[record.ID]) {
				continue
			}
			state.Sequence++
			record.Sequence = state.Sequence
			state.Records = append(state.Records, record)
			seen[version] = true
			priority[record.ID] = record.Priority
			latestObservation[record.ID] = record.ObservedAt
			if record.Type == "change" {
				state.Latest[record.ID] = record.Sequence
			}
		}

		sort.Slice(state.Records, func(i, j int) bool {
			a, b := state.Records[i], state.Records[j]
			return evidenceRetentionTime(a).Before(evidenceRetentionTime(b)) || evidenceRetentionTime(a).Equal(evidenceRetentionTime(b)) && a.ID < b.ID
		})
		if policy.Retention > 0 {
			cutoff := batch.At.Add(-policy.Retention)
			for len(state.Records) > 0 && (evidenceRetentionTime(state.Records[0]).Before(cutoff) || len(state.Records) > policy.MaxRecords) {
				if state.Records[0].At.After(state.Checkpoint.PrunedThrough) {
					state.Checkpoint.PrunedThrough = state.Records[0].At
				}
				state.Checkpoint.PruneRevision++
				state.Records = state.Records[1:]
			}
		}
		pruneOccurrenceHistory(&state, policy, batch.At)
		pruneEvidenceLatest(&state)
		if state.Captures == nil {
			state.Captures = map[string]time.Time{}
		}
		state.Captures[batch.ID] = batch.At
		for id, at := range state.Captures {
			if policy.Retention > 0 && at.Before(batch.At.Add(-policy.Retention)) {
				delete(state.Captures, id)
			}
		}
		replacement, err := json.Marshal(state)
		if err != nil {
			return err
		}
		for policy.MaxBytes > 0 && len(replacement) > policy.MaxBytes && len(state.Records)+len(state.Observations) > 0 {
			needed, removed := len(replacement)-policy.MaxBytes, 0
			for removed < needed && len(state.Records)+len(state.Observations) > 0 {
				removed += dropOldestEvidence(&state)
			}
			pruneOccurrenceVersions(&state)
			pruneEvidenceLatest(&state)
			replacement, err = json.Marshal(state)
			if err != nil {
				return err
			}
		}

		if policy.MaxBytes > 0 && len(replacement) > policy.MaxBytes {
			return ErrEvidenceLimit
		}
		var won bool
		if batch.GuardKey != "" {
			guarded, ok := p.(interface {
				compareEvidenceBlob(string, []byte, []byte, string, []byte) (bool, error)
			})
			if !ok {
				return ErrUnsupported
			}
			won, err = guarded.compareEvidenceBlob(key, raw, replacement, batch.GuardKey, batch.GuardExpected)
		} else {
			won, err = cas.CompareAndSwapBlob(key, raw, replacement)
		}
		if err != nil {
			return err
		}
		if won {
			return nil
		}
	}
	return ErrEvidenceConflict
}

func blobEvidenceRead(ctx context.Context, p Provider, scope EvidenceScope) (evidenceState, error) {
	if err := ctx.Err(); err != nil {
		return evidenceState{}, err
	}
	key, err := evidenceScopeKey(scope)
	if err != nil {
		return evidenceState{}, err
	}
	raw, err := p.ReadBlob(key)
	if err != nil {
		return evidenceState{}, err
	}
	state := evidenceState{}
	if len(raw) > 0 {
		err = json.Unmarshal(raw, &state)
	}
	return state, err
}
func blobEvidenceQuery(ctx context.Context, p Provider, policy EvidencePolicy, scope EvidenceScope, query EvidenceQuery) (EvidencePage, error) {
	query, cursor, err := prepareEvidenceQuery(scope, query)
	if err != nil {
		return EvidencePage{}, err
	}
	scopes, err := evidenceReadScopes(scope)
	if err != nil {
		return EvidencePage{}, err
	}
	page := EvidencePage{Policy: policy, Since: query.Since, Until: query.Until, Counts: map[string]int{}}
	if query.Cursor == "" {
		cursor.OccurrenceHighs = make([]uint64, len(scopes))
		cursor.Generations = make([]uint64, len(scopes))
		cursor.Highs = make([]uint64, len(scopes))
		cursor.Pruned = make([]time.Time, len(scopes))
		cursor.PruneRevisions = make([]uint64, len(scopes))
	} else if len(cursor.OccurrenceHighs) != len(scopes) || len(cursor.Generations) != len(scopes) || len(cursor.Highs) != len(scopes) || len(cursor.Pruned) != len(scopes) || len(cursor.PruneRevisions) != len(scopes) {
		return page, ErrInvalidEvidence
	}
	seen := map[string]bool{}
	for i, readScope := range scopes {
		state, err := blobEvidenceRead(ctx, p, readScope)
		if err != nil {
			return page, err
		}
		if query.Cursor == "" {
			cursor.OccurrenceHighs[i] = state.Checkpoint.OccurrenceRevision
			cursor.Generations[i] = state.Checkpoint.Generation
			cursor.Highs[i], cursor.Pruned[i], cursor.PruneRevisions[i] = state.Sequence, state.Checkpoint.PrunedThrough, state.Checkpoint.PruneRevision
		} else if cursor.OccurrenceHighs[i] > state.Checkpoint.OccurrenceRevision && query.Type == "occurrence" || cursor.Generations[i] != state.Checkpoint.Generation || cursor.Highs[i] > state.Sequence || !cursor.Pruned[i].Equal(state.Checkpoint.PrunedThrough) || cursor.PruneRevisions[i] != state.Checkpoint.PruneRevision {
			return page, ErrEvidenceConflict
		}
		page.Checkpoint = mergeEvidenceCheckpoint(page.Checkpoint, state.Checkpoint)
		latest := map[string]EvidenceRecord{}
		if query.Type == "occurrence" {
			records, occurrenceErr := occurrenceRecords(state, cursor.OccurrenceHighs[i], query)
			if occurrenceErr != nil {
				return page, occurrenceErr
			}
			for _, record := range records {
				latest[record.ID] = record
			}
		}
		for _, record := range state.Records {
			if query.Type == "occurrence" {
				break
			}
			if record.Sequence <= cursor.Highs[i] && record.Sequence > latest[record.ID].Sequence {
				latest[record.ID] = record
			}
		}
		for _, record := range latest {
			if seen[record.ID] {
				continue
			}
			seen[record.ID] = true
			if query.Type == "occurrence" {
				var issue EvidenceIssue
				if err := json.Unmarshal(record.Payload, &issue); err != nil {
					return page, err
				}
				if !occurrenceVariantMatches(issue, query.Variant) {
					continue
				}
			}
			if expected := state.Latest[record.ID]; expected > 0 && expected <= cursor.Highs[i] && record.Sequence != expected {
				continue
			}
			matches := evidenceMatches(record, query, evidenceCursor{})
			inPage := evidenceMatches(record, query, cursor)
			if matches && query.Projection != nil {
				record = query.Projection(record)
			}
			if matches {
				page.Total++
				page.Counts[record.Type]++
			}
			if inPage {
				page.Items = append(page.Items, record)
			}
		}

	}
	sort.Slice(page.Items, func(i, j int) bool {
		a, b := page.Items[i], page.Items[j]
		return evidenceTime(a, query).After(evidenceTime(b, query)) || evidenceTime(a, query).Equal(evidenceTime(b, query)) && a.ID > b.ID
	})
	return finishEvidencePage(page, query, cursor), nil
}

func (p *fileProvider) EvidencePolicy() EvidencePolicy {
	return EvidencePolicy{Mode: "bounded_file", Retention: 24 * time.Hour, MaxRecords: 5000, MaxBytes: 4 << 20}
}
func (p *memoryProvider) EvidencePolicy() EvidencePolicy {
	return EvidencePolicy{Mode: "ephemeral"}
}
func (p *fileProvider) CommitEvidence(ctx context.Context, s EvidenceScope, b EvidenceBatch) error {
	return blobEvidenceCommit(ctx, p, p.EvidencePolicy(), s, b)
}
func (p *memoryProvider) CommitEvidence(ctx context.Context, s EvidenceScope, b EvidenceBatch) error {
	return blobEvidenceCommit(ctx, p, p.EvidencePolicy(), s, b)
}
func (p *fileProvider) QueryEvidence(ctx context.Context, s EvidenceScope, q EvidenceQuery) (EvidencePage, error) {
	return blobEvidenceQuery(ctx, p, p.EvidencePolicy(), s, q)
}
func (p *memoryProvider) QueryEvidence(ctx context.Context, s EvidenceScope, q EvidenceQuery) (EvidencePage, error) {
	return blobEvidenceQuery(ctx, p, p.EvidencePolicy(), s, q)
}
func (p *fileProvider) EvidenceCheckpoint(ctx context.Context, s EvidenceScope) (EvidenceCheckpoint, error) {
	state, err := blobEvidenceRead(ctx, p, s)
	return state.Checkpoint, err
}
func (p *memoryProvider) EvidenceCheckpoint(ctx context.Context, s EvidenceScope) (EvidenceCheckpoint, error) {
	state, err := blobEvidenceRead(ctx, p, s)
	return state.Checkpoint, err
}
func (p *fileProvider) DeleteEvidence(ctx context.Context, s EvidenceScope) error {
	return blobEvidenceDelete(ctx, p, s)
}
func (p *memoryProvider) DeleteEvidence(ctx context.Context, s EvidenceScope) error {
	return blobEvidenceDelete(ctx, p, s)
}

// Replacing the whole scoped state under CAS clears history atomically while
// retaining a durable fence. Generic model-state deletion remains restricted.
func blobEvidenceDelete(ctx context.Context, p Provider, scope EvidenceScope) error {
	key, err := evidenceScopeKey(scope)
	if err != nil {
		return err
	}
	cas, ok := p.(BlobCAS)
	if !ok {
		return ErrUnsupported
	}
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := p.ReadBlob(key)
		if err != nil {
			return err
		}
		var state evidenceState
		if len(raw) > 0 && json.Unmarshal(raw, &state) != nil {
			return ErrInvalidEvidence
		}
		if state.Checkpoint.Generation == ^uint64(0) {
			return ErrEvidenceLimit
		}
		replacement, err := json.Marshal(evidenceState{Checkpoint: EvidenceCheckpoint{Generation: state.Checkpoint.Generation + 1}})
		if err != nil {
			return err
		}
		won, err := cas.CompareAndSwapBlob(key, raw, replacement)
		if err != nil {
			return err
		}
		if won {
			return nil
		}
	}
	return ErrEvidenceConflict
}

func (p *memoryProvider) compareEvidenceBlob(key string, expected, replacement []byte, guard string, guardExpected []byte) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !bytes.Equal(p.blobs[guard], guardExpected) {
		return false, ErrEvidenceConflict
	}
	current, exists := p.blobs[key]
	if (expected == nil && exists) || (expected != nil && !exists) || !bytes.Equal(current, expected) {
		return false, nil
	}
	if !exists {
		p.blobNames = append(p.blobNames, key)
		sort.Strings(p.blobNames)
	}
	p.blobs[key] = append([]byte(nil), replacement...)
	p.blobAt[key] = time.Now().UTC()
	return true, nil
}

func (p *fileProvider) compareEvidenceBlob(key string, expected, replacement []byte, guard string, guardExpected []byte) (bool, error) {
	target, guardPath := p.blobPath(key), p.blobPath(guard)
	shard := func(path string) uint32 {
		hash := fnv.New32a()
		_, _ = hash.Write([]byte(path))
		return hash.Sum32() % fileBlobLockShardCount
	}
	a, b := shard(target), shard(guardPath)
	if a > b {
		a, b = b, a
	}
	fileBlobLocks[a].Lock()
	defer fileBlobLocks[a].Unlock()
	if a != b {
		fileBlobLocks[b].Lock()
		defer fileBlobLocks[b].Unlock()
	}
	guardData, err := os.ReadFile(guardPath)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(guardData, guardExpected) {
		return false, ErrEvidenceConflict
	}
	current, err := os.ReadFile(target)
	if errors.Is(err, os.ErrNotExist) {
		if expected != nil {
			return false, nil
		}
		current = nil
	} else if err != nil {
		return false, err
	} else if expected == nil {
		return false, nil
	}
	if !bytes.Equal(current, expected) {
		return false, nil
	}
	if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return false, err
	}
	if err = writeFileAtomicSync(target, replacement, 0644); err != nil {
		return false, err
	}
	return true, nil
}

// Changes may gain stronger observed facts after reconstruction. Immutable
// versions preserve pinned query order while new queries see the refinement.
func evidenceVersion(record EvidenceRecord) string {
	if record.Type != "change" {
		return "immutable"
	}
	raw, _ := json.Marshal([]any{record.Type, record.At, record.Namespace, record.Name, record.Kind, record.UID, record.RootUID, record.Rule, record.Priority, record.Payload})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func pruneEvidenceLatest(state *evidenceState) {
	retained := map[string]bool{}
	for _, record := range state.Records {
		retained[record.ID] = true
	}
	for id := range state.Latest {
		if !retained[id] {
			delete(state.Latest, id)
		}
	}
}
