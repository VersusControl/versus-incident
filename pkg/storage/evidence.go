package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrInvalidEvidence = errors.New("storage: invalid evidence data")

// ErrEvidenceCursorExpired reports a well-formed page cursor for the same list
// and filters that can no longer be continued: an earlier release issued it
// before the list changed how it pages, or the stored history changed under it
// since, because retention removed a month or thinned samples or the history
// was deleted and captured again. The reader restarts the list from its first
// page.
var ErrEvidenceCursorExpired = fmt.Errorf("%w: page cursor no longer matches the stored history", ErrInvalidEvidence)

var ErrEvidenceConflict = errors.New("storage: evidence checkpoint conflict")
var ErrEvidenceLimit = errors.New("storage: evidence operation exceeds budget")

// ErrEvidenceUnstorable is a save the store cannot hold as given, such as text
// the database refuses. It is invalid evidence, and a retry of the same batch
// cannot succeed.
var ErrEvidenceUnstorable = fmt.Errorf("%w: the store cannot hold this content", ErrInvalidEvidence)

// ErrEvidenceNotReady reports a scope whose read models are still being built.
// History reads are served only from read models, so a scope that is still
// converting answers this fast, retryable error.
var ErrEvidenceNotReady = errors.New("storage: evidence read models are not ready")

type evidenceReadContextKey struct{}

// EvidenceReadContext routes metadata reads through the isolated history pool.
// Collection checkpoints and writes retain the writer pool.
func EvidenceReadContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, evidenceReadContextKey{}, true)
}

// EvidenceStore is optional. Scope is explicit even when a provider is wrapped.
// Implementations atomically commit observations, lifecycle events and checkpoint.
// Query bounds limit work, never the history retained by a database.
type EvidenceStore interface {
	SetEvidenceAuthority(context.Context, EvidenceScope, string, map[string]EvidenceAuthority) error
	EvidencePolicy() EvidencePolicy
	CommitEvidence(context.Context, EvidenceScope, EvidenceBatch) error
	QueryEvidence(context.Context, EvidenceScope, EvidenceQuery) (EvidencePage, error)
	EvidenceCheckpoint(context.Context, EvidenceScope) (EvidenceCheckpoint, error)
	DeleteEvidence(context.Context, EvidenceScope) error
}

// EvidenceAuthorityStore can persist a positive denial before a collection
// finishes. It may change only the matching principal's authorization proof.
type EvidenceAuthorityStore interface {
	SetEvidenceAuthority(context.Context, EvidenceScope, string, map[string]EvidenceAuthority) error
}

// EvidenceScopeLookup resolves committed physical identity without reading an
// unbounded blob catalog or contacting a cluster.
type EvidenceScopeLookup interface {
	FindEvidenceScope(context.Context, string, string, string) (EvidenceScope, error)
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
	CommitRevision     uint64                       `json:"commit_revision,omitempty"`
	HighSequence       uint64                       `json:"high_sequence,omitempty"`
	Principal          string                       `json:"principal,omitempty"`
	Authority          map[string]EvidenceAuthority `json:"authority,omitempty"`
	OccurrenceRevision uint64                       `json:"occurrence_revision,omitempty"`
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
	// ThinRevision counts removals that took only issue observation samples:
	// retention thinning in the database, and the sample age and size limits
	// of the local stores. Journal rows and occurrences stay, so a list over
	// them keeps its page links; PruneRevision counts every other removal.
	ThinRevision uint64 `json:"thin_revision,omitempty"`
	// ThinnedThrough is the newest bound before which samples were removed.
	ThinnedThrough time.Time `json:"thinned_through,omitempty"`
	// Saving names a capture whose save has written journal parts but not yet
	// its final part. Readers stay pinned to this checkpoint meanwhile, and
	// counters that the parts already moved are not used to answer them.
	Saving string `json:"saving,omitempty"`
	// SavingHigh is the highest sequence those parts wrote, kept across
	// attempts, so the checkpoint that finally lands covers every part.
	SavingHigh uint64 `json:"saving_high,omitempty"`
}

// EvidenceAuthority records a completed collection proof, not a credential claim.
// Empty namespace means an all-namespaces LIST. Unknown is never an allow.
type EvidenceAuthority struct {
	Namespace string    `json:"namespace,omitempty"`
	State     string    `json:"state"`
	ProvedAt  time.Time `json:"proved_at"`
}

type EvidenceBatch struct {
	// KeepLease, when set, is called before each save transaction. It confirms
	// the caller still holds its lease, renewing it when needed, and returns
	// the current guard value for GuardKey. A capture saved in several
	// transactions therefore never outlives the lease that fences it.
	KeepLease func(context.Context) ([]byte, error) `json:"-"`
	// Fence, when set, runs each save transaction. A caller that must hold a
	// lock while it writes holds it for one transaction at a time, so work it
	// serves elsewhere can run between the transactions of a large save.
	Fence                func(context.Context, func() error) error `json:"-"`
	ScopeBinding         string
	Principal            string
	Authority            map[string]EvidenceAuthority
	ObservationHeartbeat time.Duration
	Generation           uint64
	HistoricalOnly       bool
	Coverage             map[string]EvidenceInterval
	GuardKey             string
	GuardExpected        []byte
	ID                   string
	Expected             string
	At                   time.Time
	Epoch                string
	// Only sufficient successful observations may advance CoveredThrough.
	// A partial batch can add findings but cannot close absent issues.
	CoveredThrough time.Time
	Records        []EvidenceRecord
	Issues         []EvidenceIssue
}

type EvidenceQuery struct {
	Authorities map[string]EvidenceAuthority
	Lookup      bool
	// Authority is supplied by the trusted service from its persisted checkpoint.
	// Projection remains for compatibility, but is applied only to the bounded page.
	EnforceAuthority   bool
	AllowedKinds       []string
	AuthorityNamespace string
	OccurrenceID       string
	Center             time.Time
	PointInTime        bool
	// Projection transforms unavailable records into explicit gaps before counts
	// and pagination. Identity and ordering fields must remain unchanged.
	Projection   func(EvidenceRecord) EvidenceRecord
	ProjectionID string
	Variant      string
	WithGaps     bool
	// GapsApart keeps coverage gaps, and rows this reader may not see, out of
	// the item keyset: Items, Total and Counts describe only rows the reader
	// can list, so every page but the last holds Limit items. The gaps that
	// overlap the page's time range, including denied rows as point gaps,
	// arrive in EvidencePage.Gaps from a separate bounded read. It implies
	// WithGaps for the gap filters.
	GapsApart                                   bool
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
	CountCapped  bool   `json:"count_capped,omitempty"`
	Authority    string `json:"authority,omitempty"`
	Since, Until time.Time
	Total        int
	Counts       map[string]int
	Items        []EvidenceRecord `json:"items"`
	// Gaps holds the page's coverage gaps when the query keeps them apart.
	Gaps       []EvidenceRecord   `json:"gaps,omitempty"`
	Next       string             `json:"next,omitempty"`
	Checkpoint EvidenceCheckpoint `json:"checkpoint"`
	// resume is this page's cursor state, from which ResumeAfter positions a
	// continuation after any item on the page.
	resume      evidenceCursor
	resumeQuery EvidenceQuery
	Policy      EvidencePolicy `json:"policy"`
}

type evidenceState struct {
	updatedOccurrences map[string]struct{}
	RootLatest         map[string]string
	RootCenters        map[string][]string
	Occurrences        map[string]EvidenceIssue
	LastObservations   map[string]time.Time
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
	Ordering        string
	CaptureScope    string
	OccurrenceHighs []uint64
	Generations     []uint64
	PruneRevisions  []uint64
	ThinRevisions   []uint64
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
	current.PruneRevision = max(current.PruneRevision, next.PruneRevision)
	if next.ThinnedThrough.After(current.ThinnedThrough) {
		current.ThinnedThrough = next.ThinnedThrough
	}
	current.ThinRevision = max(current.ThinRevision, next.ThinRevision)
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
// Validate checks the batch as a store will receive it, after normalization.
func (batch EvidenceBatch) Validate() error { return validEvidenceBatch(normalizeEvidenceBatch(batch)) }

func validEvidenceBatch(batch EvidenceBatch) error {
	if batch.Principal == "" && len(batch.Authority) > 0 {
		return ErrInvalidEvidence
	}
	if len(batch.ScopeBinding) > 256 || strings.ContainsAny(batch.ScopeBinding, "\x00\r\n") {
		return ErrInvalidEvidence
	}
	if len(batch.Principal) > 256 || len(batch.Authority) > 4096 {
		return ErrEvidenceLimit
	}
	for kind, proof := range batch.Authority {
		if kind == "" || len(kind) > 512 || len(proof.Namespace) > 256 || proof.ProvedAt.IsZero() ||
			(proof.State != "allowed" && proof.State != "denied" && proof.State != "unknown") {
			return ErrInvalidEvidence
		}
		_, namespace, _ := strings.Cut(kind, "@")
		if namespace != proof.Namespace || strings.ContainsAny(kind, "\x00\r\n") {
			return ErrInvalidEvidence
		}
	}
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
	if len(batch.Records)+len(batch.Issues) > MaxEvidenceBatchRecords {
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
	if size > MaxEvidenceBatchBytes {
		return ErrEvidenceLimit
	}
	return nil
}

// A capture is held whole in memory, published with the inventory and saved in
// bounded parts, so these limits bound memory, not the length of one save
// transaction. They keep one capture's history within 16 MiB of the published
// inventory blob and its validation, publication and database save within
// 128 MiB of transient heap (measured by TestCaptureMemoryAtTheHistorySaveLimits).
// A store refuses a batch over either limit; a producer fits its capture to
// them before publishing it.
const (
	MaxEvidenceBatchRecords = 25000
	MaxEvidenceBatchBytes   = 15 << 20
)

// EvidencePayloadSize is how much of MaxEvidenceBatchBytes one record or issue
// payload uses once a store has made its text storable.
func EvidencePayloadSize(payload json.RawMessage) int {
	return len(normalizeEvidencePayload(payload))
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
	if state.Checkpoint.Principal != "" && batch.Principal == "" {
		return nil, ErrInvalidEvidence
	}
	state.updatedOccurrences = map[string]struct{}{}
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
	state.Checkpoint.CommitRevision = max(state.Checkpoint.CommitRevision, state.Checkpoint.OccurrenceRevision) + 1
	state.Checkpoint.OccurrenceRevision++
	if state.Occurrences == nil {
		state.Occurrences = map[string]EvidenceIssue{}
		for _, observation := range state.Observations {
			issue, ok := state.OccurrenceVersions[observation.Version]
			if ok {
				issue.LastSeen = observation.LastSeen
				state.Occurrences[issue.OccurrenceID] = issue
			}
		}
	}
	if state.LastObservations == nil {
		state.LastObservations = map[string]time.Time{}
		for _, observation := range state.Observations {
			if observation.At.After(state.LastObservations[observation.OccurrenceID]) {
				state.LastObservations[observation.OccurrenceID] = observation.At
			}
		}
	}
	heartbeat := batch.ObservationHeartbeat
	if heartbeat <= 0 {
		heartbeat = 5 * time.Minute
	}
	if batch.Principal != "" {
		if state.Checkpoint.Principal != batch.Principal {
			state.Checkpoint.Authority = nil
		}
		state.Checkpoint.Principal = batch.Principal
	}
	if state.Checkpoint.Authority == nil {
		state.Checkpoint.Authority = map[string]EvidenceAuthority{}
	}
	mergeEvidenceAuthority(&state.Checkpoint, batch.Authority)
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
			observed.OccurrenceID = evidenceDigest(observed.Key, "observation", observed.FirstSeen.UTC().Format("2006-01-02"))
			previous, exists := state.Occurrences[observed.OccurrenceID]
			changed := !exists || !sameIssueEvidence(previous.Payload, observed.Payload) || previous.Broken != observed.Broken || previous.AuthorizationKind != observed.AuthorizationKind
			if exists {
				observed.FirstSeen = previous.FirstSeen
			}
			state.Occurrences[observed.OccurrenceID] = observed
			state.updatedOccurrences[observed.OccurrenceID] = struct{}{}
			if changed || observed.LastSeen.Sub(state.LastObservations[observed.OccurrenceID]) >= heartbeat {
				addOccurrenceObservation(state, observed, state.Checkpoint.OccurrenceRevision, changed)
			}
			if changed {
				added = append(added, issueEvidence(observed, batch, "issue_observed"))
			}
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
		if exists {
			observed.Payload = pinIssueBreakTime(active.Payload, observed.Payload)
		}
		advanced := !exists || observed.LastSeen.Sub(state.LastObservations[active.OccurrenceID]) >= heartbeat
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
		state.Occurrences[active.OccurrenceID] = active
		state.updatedOccurrences[active.OccurrenceID] = struct{}{}
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
			state.Occurrences[active.OccurrenceID] = active
			state.updatedOccurrences[active.OccurrenceID] = struct{}{}
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
	updateOccurrenceLookups(state)
	return added, nil
}

// Poll timestamps describe occurrence freshness, not a new evidence payload.
// Journal rows stay immutable so a pinned page never acquires later poll data.
// Preserve the opening condition time while an occurrence remains open; later
// condition transitions refresh evidence, not the occurrence identity.
func pinIssueBreakTime(previous, current json.RawMessage) json.RawMessage {
	var old, next map[string]json.RawMessage
	if json.Unmarshal(previous, &old) != nil || json.Unmarshal(current, &next) != nil || next == nil {
		return current
	}
	var at time.Time
	if json.Unmarshal(old["broke_at"], &at) != nil || at.IsZero() {
		return current
	}
	next["broke_at"] = old["broke_at"]
	if source, ok := old["broke_at_source"]; ok {
		next["broke_at_source"] = source
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return current
	}
	return raw
}

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
		delete(value, "count")
		// These are mutable measurements, not occurrence identity or lifecycle.
		delete(value, "evidence")
		for _, field := range []string{"examples", "of", "changes", "more_changes", "history", "gaps", "rebuilt", "untracked", "partial", "pending"} {
			delete(value, field)
		}
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
	if query.Type == "timeline" || query.Type == "break" || query.GapsApart {
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
	orgs := strings.Join(orgBinding, "\x00")
	binding := evidenceCursorBinding(orgs, scope, query, query.WithGaps, query.GapsApart)
	if query.Cursor != "" && cursor.Binding != binding {
		// A list that keeps gaps apart walks a different keyset than the one
		// an earlier release bound its page links to. Such a link names this
		// same list, so the reader is sent back to its first page.
		if query.GapsApart && (cursor.Binding == evidenceCursorBinding(orgs, scope, query, true, false) || cursor.Binding == evidenceCursorBinding(orgs, scope, query, false, false)) {
			return query, cursor, ErrEvidenceCursorExpired
		}
		return query, cursor, ErrInvalidEvidence
	}
	cursor.CaptureScope = captureScope
	cursor.Binding, cursor.Since, cursor.Until = binding, query.Since, query.Until
	return query, cursor, nil
}

// cursorThinRevisionsFit accepts a cursor with one thin revision per scope,
// or none from a release that counted sample removals as pruning.
func cursorThinRevisionsFit(cursor evidenceCursor, scopes int) bool {
	return len(cursor.ThinRevisions) == 0 || len(cursor.ThinRevisions) == scopes
}

// cursorSamplesThinned reports whether issue samples of scope i were removed
// since the cursor was issued. A cursor without thin revisions cannot tell, so
// any recorded removal counts.
func cursorSamplesThinned(cursor evidenceCursor, i int, revision uint64) bool {
	if len(cursor.ThinRevisions) == 0 {
		return revision != 0
	}
	return cursor.ThinRevisions[i] != revision
}

// evidenceCursorBinding is the digest a page cursor carries for one query,
// so a cursor is honoured only by the list and filters that issued it.
func evidenceCursorBinding(orgs string, scope EvidenceScope, query EvidenceQuery, withGaps, gapsApart bool) string {
	binding := evidenceDigest(orgs, NormalizeOrgID(scope.OrgID), scope.ClusterID, scope.ClusterUID, query.Since.UTC().Format(time.RFC3339Nano), query.Until.UTC().Format(time.RFC3339Nano), query.Namespace, query.Kind, query.UID, query.RootUID, query.PodUID, query.Rule, query.Type, query.Name, query.ResourceUID, strconv.FormatBool(query.ByObservation), strconv.FormatBool(withGaps), query.Variant)
	if query.ProjectionID != "" {
		binding = evidenceDigest(binding, query.ProjectionID)
	}
	if gapsApart {
		// A cursor walks a different keyset when gaps are kept apart.
		binding = evidenceDigest(binding, "gaps_apart")
	}
	return evidenceDigest(binding, query.OccurrenceID, query.Center.UTC().Format(time.RFC3339Nano), strconv.FormatBool(query.PointInTime))
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

// evidenceTimelineCrashMarker applies the timeline marker rule to a retained crash
// record, and matches the native and Postgres rule exactly: a record that declares
// no subtype keeps its legacy marker, a declared terminal marker keeps it, and a
// declared status observation is not a marker. Only a declared string subtype
// participates, so a declared non-string subtype is not a marker anywhere and a
// payload that cannot be decoded is never silently promoted to a marker.
func evidenceTimelineCrashMarker(record EvidenceRecord) bool {
	if len(record.Payload) == 0 {
		return false
	}
	var payload any
	if json.Unmarshal(record.Payload, &payload) != nil {
		return false
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return true
	}
	value, declared := object["type"]
	if !declared || value == nil {
		return true
	}
	subtype, ok := value.(string)
	if !ok {
		return false
	}
	return subtype == "" || subtype == "termination"
}

func evidenceRecordTypeMatches(record EvidenceRecord, filter string) bool {
	if filter == "timeline" && record.Type == "crash" {
		return evidenceTimelineCrashMarker(record)
	}
	return evidenceTypeMatches(record.Type, filter, record.Broken)
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
		if query.Type == "occurrence" {
			var issue EvidenceIssue
			if json.Unmarshal(record.Payload, &issue) != nil || issue.FirstSeen.After(query.Until) || issue.ClosedAt != nil && issue.ClosedAt.Before(query.Since) {
				return false
			}
		} else {
			if at.Before(query.Since) || at.After(query.Until) {
				return false
			}
		}
		if query.Namespace != "" && record.Namespace != query.Namespace || query.Kind != "" && record.Kind != query.Kind || query.UID != "" && record.UID != query.UID || query.RootUID != "" && record.RootUID != query.RootUID || query.PodUID != "" && record.PodUID != query.PodUID || query.Rule != "" && record.Rule != query.Rule || !evidenceRecordTypeMatches(record, query.Type) || query.Name != "" && record.Name != query.Name || query.ResourceUID != "" && record.UID != query.ResourceUID && record.RootUID != query.ResourceUID && record.PodUID != query.ResourceUID {
			return false
		}
	}
	at := evidenceTime(record, query)
	return cursor.At.IsZero() || at.Before(cursor.At) || at.Equal(cursor.At) && record.ID < cursor.ID
}

// ResumeAfter returns the cursor that continues this page's list right after
// record, one of the page's items. A caller that shows only part of a page
// resumes there instead of skipping the rows it did not show. It is empty for
// a page that was not read through a cursor-paged query.
func (page EvidencePage) ResumeAfter(record EvidenceRecord) string {
	if page.resume.Binding == "" || record.ID == "" {
		return ""
	}
	cursor := page.resume
	cursor.At, cursor.ID = evidenceTime(record, page.resumeQuery), record.ID
	raw, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func finishEvidencePage(page EvidencePage, query EvidenceQuery, cursor evidenceCursor) EvidencePage {
	page.resume, page.resumeQuery = cursor, EvidenceQuery{ByObservation: query.ByObservation}
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

// evidencePageGapLimit bounds the separate gap read of one page.
const evidencePageGapLimit = 200

// evidencePageRange is the time range a finished page covers in a newest-first
// walk: from the previous page's last row, or the query's end on the first
// page, down to this page's last row, or the query's start on the last page.
// Consecutive pages share only their boundary instant, so every gap in the
// window is shown on the page, or pages, whose range it overlaps.
func evidencePageRange(page EvidencePage, query EvidenceQuery, cursor evidenceCursor) (time.Time, time.Time) {
	upper := query.Until
	if !cursor.At.IsZero() && cursor.At.Before(upper) {
		upper = cursor.At
	}
	lower := query.Since
	if page.Next != "" && len(page.Items) > 0 {
		lower = evidenceTime(page.Items[len(page.Items)-1], query)
	}
	if lower.After(upper) {
		lower = upper
	}
	return lower, upper
}

// boundEvidenceGaps keeps at most evidencePageGapLimit gaps, newest first. A
// read that found more cannot name every gap, so the whole page range is
// reported as one gap instead: coverage is never claimed where it was not read.
func boundEvidenceGaps(gaps []EvidenceRecord, query EvidenceQuery, lower, upper time.Time) []EvidenceRecord {
	sort.SliceStable(gaps, func(i, j int) bool {
		a, b := gaps[i], gaps[j]
		return evidenceTime(a, query).After(evidenceTime(b, query)) || evidenceTime(a, query).Equal(evidenceTime(b, query)) && a.ID > b.ID
	})
	if len(gaps) <= evidencePageGapLimit {
		if gaps == nil {
			return []EvidenceRecord{}
		}
		return gaps
	}
	raw, _ := json.Marshal(struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	}{lower, upper})
	return []EvidenceRecord{{ID: "coverage_gap:page:" + lower.UTC().Format(time.RFC3339Nano), Type: "coverage_gap", At: upper, ObservedAt: upper, GapFrom: lower, GapTo: upper, Payload: raw}}
}

// evidenceGapInRange reports whether a gap read candidate belongs to the page
// range: a coverage gap overlaps it, a denied row falls inside it.
func evidenceGapInRange(record EvidenceRecord, query EvidenceQuery, lower, upper time.Time, denied bool) bool {
	if denied {
		at := evidenceTime(record, query)
		return !at.Before(lower) && !at.After(upper)
	}
	return !record.GapTo.Before(lower) && !record.GapFrom.After(upper)
}

// blobEvidenceCommit saves a whole capture in one compare-and-swap, which is
// one transaction for the caller's fence.
func blobEvidenceCommit(ctx context.Context, p Provider, policy EvidencePolicy, scope EvidenceScope, batch EvidenceBatch) error {
	return evidenceFenced(ctx, batch, func() error { return blobEvidenceCommitUnfenced(ctx, p, policy, scope, batch) })
}

func blobEvidenceCommitUnfenced(ctx context.Context, p Provider, policy EvidencePolicy, scope EvidenceScope, batch EvidenceBatch) error {
	batch = normalizeEvidenceBatch(batch)
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
			state.Checkpoint.HighSequence = state.Sequence
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
		cursor.ThinRevisions = make([]uint64, len(scopes))
	} else if len(cursor.OccurrenceHighs) != len(scopes) || len(cursor.Generations) != len(scopes) || len(cursor.Highs) != len(scopes) || len(cursor.Pruned) != len(scopes) || len(cursor.PruneRevisions) != len(scopes) || !cursorThinRevisionsFit(cursor, len(scopes)) {
		return page, ErrInvalidEvidence
	}
	type gapCandidate struct {
		record EvidenceRecord
		denied bool
	}
	gaps := []gapCandidate{}
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
			cursor.ThinRevisions[i] = state.Checkpoint.ThinRevision
		} else if cursor.Generations[i] != state.Checkpoint.Generation || !cursor.Pruned[i].Equal(state.Checkpoint.PrunedThrough) || cursor.PruneRevisions[i] != state.Checkpoint.PruneRevision ||
			query.Type == "occurrence" && cursorSamplesThinned(cursor, i, state.Checkpoint.ThinRevision) {
			// An occurrence list reads the samples, so any removed sample can
			// change what it shows.
			return page, ErrEvidenceCursorExpired
		} else if cursor.OccurrenceHighs[i] > state.Checkpoint.OccurrenceRevision && query.Type == "occurrence" || cursor.Highs[i] > state.Sequence {
			return page, ErrEvidenceConflict
		}
		page.Checkpoint = mergeEvidenceCheckpoint(page.Checkpoint, state.Checkpoint)
		latest := map[string]EvidenceRecord{}
		if query.Type == "occurrence" {
			var records []EvidenceRecord
			var occurrenceErr error
			if query.Lookup && state.Occurrences != nil {
				records = lookupOccurrenceRecords(state, query)
			} else {
				records, occurrenceErr = occurrenceRecords(state, cursor.OccurrenceHighs[i], query)
			}
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
			if matches && query.GapsApart && (record.Type == "coverage_gap" || projectEvidenceAuthority(record, query).Type == "coverage_gap") {
				gap := projectEvidenceAuthority(record, query)
				if query.Projection != nil {
					gap = query.Projection(gap)
				}
				gaps = append(gaps, gapCandidate{gap, record.Type != "coverage_gap"})
				continue
			}
			if matches {
				record = projectEvidenceAuthority(record, query)
			}
			if matches && query.Projection != nil {
				record = query.Projection(record)
			}
			if matches && query.Cursor == "" && page.Total < 1001 {
				page.Total++
				page.Counts[record.Type]++
				if query.Type == "occurrence" && record.Type != "coverage_gap" {
					var wrapper EvidenceIssue
					var facts struct {
						Severity string `json:"severity"`
					}
					if json.Unmarshal(record.Payload, &wrapper) == nil && json.Unmarshal(wrapper.Payload, &facts) == nil && facts.Severity != "" {
						page.Counts["severity:"+facts.Severity]++
						if wrapper.Broken {
							page.Counts["broken:"+facts.Severity]++
						}
					}
				}
			}
			if inPage {
				page.Items = append(page.Items, record)
				sort.Slice(page.Items, func(i, j int) bool {
					a, b := page.Items[i], page.Items[j]
					return evidenceTime(a, query).After(evidenceTime(b, query)) || evidenceTime(a, query).Equal(evidenceTime(b, query)) && a.ID > b.ID
				})
				if len(page.Items) > query.Limit+1 {
					page.Items = page.Items[:query.Limit+1]
				}
			}
		}

	}
	sort.Slice(page.Items, func(i, j int) bool {
		a, b := page.Items[i], page.Items[j]
		return evidenceTime(a, query).After(evidenceTime(b, query)) || evidenceTime(a, query).Equal(evidenceTime(b, query)) && a.ID > b.ID
	})
	page.CountCapped = page.Total > 1000
	page = finishEvidencePage(page, query, cursor)
	if query.GapsApart {
		lower, upper := evidencePageRange(page, query, cursor)
		page.Gaps = []EvidenceRecord{}
		for _, candidate := range gaps {
			if evidenceGapInRange(candidate.record, query, lower, upper, candidate.denied) {
				page.Gaps = append(page.Gaps, candidate.record)
			}
		}
		page.Gaps = boundEvidenceGaps(page.Gaps, query, lower, upper)
	}
	return page, nil
}

func projectEvidenceAuthority(record EvidenceRecord, query EvidenceQuery) EvidenceRecord {
	if !query.EnforceAuthority || record.Type == "coverage_gap" {
		return record
	}
	if len(query.Authorities) > 0 {
		if EvidenceAuthorityAllows(query.Authorities, record.AuthorizationKind, record.Namespace) {
			return record
		}
	} else {
		for _, kind := range query.AllowedKinds {
			if kind != "" && kind == record.AuthorizationKind {
				return record
			}
		}
	}
	raw, _ := json.Marshal(struct{ From, To time.Time }{record.At, record.At})
	return EvidenceRecord{ID: record.ID, Type: "coverage_gap", At: record.At, ObservedAt: record.ObservedAt, Sequence: record.Sequence, GapFrom: record.At, GapTo: record.At, Payload: raw}
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
	index, err := p.evidenceIndex()
	if err != nil {
		return EvidencePage{}, err
	}
	return index.query(ctx, p.EvidencePolicy(), s, q)
}
func (p *memoryProvider) QueryEvidence(ctx context.Context, s EvidenceScope, q EvidenceQuery) (EvidencePage, error) {
	index, err := p.evidenceIndex()
	if err != nil {
		return EvidencePage{}, err
	}
	return index.query(ctx, p.EvidencePolicy(), s, q)
}
func (p *fileProvider) EvidenceCheckpoint(ctx context.Context, s EvidenceScope) (EvidenceCheckpoint, error) {
	key, err := evidenceScopeKey(s)
	if err != nil {
		return EvidenceCheckpoint{}, err
	}
	index, err := p.evidenceIndex()
	if err != nil {
		return EvidenceCheckpoint{}, err
	}
	return index.checkpoint(ctx, key)
}
func (p *memoryProvider) EvidenceCheckpoint(ctx context.Context, s EvidenceScope) (EvidenceCheckpoint, error) {
	key, err := evidenceScopeKey(s)
	if err != nil {
		return EvidenceCheckpoint{}, err
	}
	index, err := p.evidenceIndex()
	if err != nil {
		return EvidenceCheckpoint{}, err
	}
	return index.checkpoint(ctx, key)
}
func (p *fileProvider) DeleteEvidence(ctx context.Context, s EvidenceScope) error {
	return blobEvidenceDelete(ctx, p, s)
}
func (p *memoryProvider) DeleteEvidence(ctx context.Context, s EvidenceScope) error {
	return blobEvidenceDelete(ctx, p, s)
}

// EvidenceAuthorityAllows evaluates a namespace proof only if it is at least as
// recent as the all-namespaces proof. Missing proof is never an allow.
func EvidenceAuthorityAllows(proofs map[string]EvidenceAuthority, kind, namespace string) bool {
	if kind == "" {
		return false
	}
	proof := proofs[kind]
	if namespace != "" {
		if scoped, ok := proofs[kind+"@"+namespace]; ok && (scoped.ProvedAt.After(proof.ProvedAt) || scoped.ProvedAt.Equal(proof.ProvedAt) && scoped.State != "allowed") {
			proof = scoped
		}
	}
	return proof.State == "allowed"
}

func blobEvidenceAuthority(ctx context.Context, p Provider, scope EvidenceScope, principal string, proofs map[string]EvidenceAuthority) error {
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
		if len(raw) == 0 {
			return nil
		}
		var state evidenceState
		if err = json.Unmarshal(raw, &state); err != nil {
			return err
		}
		if state.Checkpoint.Principal != principal {
			return ErrEvidenceConflict
		}
		mergeEvidenceAuthority(&state.Checkpoint, proofs)
		state.Checkpoint.CommitRevision = max(state.Checkpoint.CommitRevision, state.Checkpoint.OccurrenceRevision) + 1
		next, err := json.Marshal(state)
		if err != nil {
			return err
		}
		won, err := cas.CompareAndSwapBlob(key, raw, next)
		if err != nil || won {
			return err
		}
	}
	return ErrEvidenceConflict
}

func mergeEvidenceAuthority(cp *EvidenceCheckpoint, proofs map[string]EvidenceAuthority) {
	if cp.Authority == nil {
		cp.Authority = map[string]EvidenceAuthority{}
	}
	for kind, proof := range proofs {
		if proof.ProvedAt.Before(cp.Authority[kind].ProvedAt) {
			continue
		}
		if proof.ProvedAt.Equal(cp.Authority[kind].ProvedAt) && cp.Authority[kind].State == "denied" && proof.State == "allowed" {
			continue
		}
		cp.Authority[kind] = proof
	}
}

func (p *memoryProvider) SetEvidenceAuthority(ctx context.Context, scope EvidenceScope, principal string, proofs map[string]EvidenceAuthority) error {
	return blobEvidenceAuthority(ctx, p, scope, principal, proofs)
}
func (p *fileProvider) SetEvidenceAuthority(ctx context.Context, scope EvidenceScope, principal string, proofs map[string]EvidenceAuthority) error {
	return blobEvidenceAuthority(ctx, p, scope, principal, proofs)
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
		replacement, err := json.Marshal(evidenceState{Checkpoint: EvidenceCheckpoint{Generation: state.Checkpoint.Generation + 1, CommitRevision: max(state.Checkpoint.CommitRevision, state.Checkpoint.OccurrenceRevision) + 1}})
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
	index, err := p.evidenceIndex()
	if err != nil {
		return false, err
	}
	won, err := index.swap(context.Background(), key, expected, replacement, false)
	if err != nil || !won {
		return won, localEvidenceError(err)
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
	index, err := p.evidenceIndex()
	if err != nil {
		return false, err
	}
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
	guardData, err := p.ReadBlob(guard)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(guardData, guardExpected) {
		return false, ErrEvidenceConflict
	}
	return index.swap(context.Background(), key, expected, replacement, false)
}

// Changes may gain stronger observed facts after reconstruction. Immutable
// versions preserve pinned query order while new queries see the refinement.
// evidenceVersion identifies a record's content. Observation and detection
// times are excluded: seeing the same change or crash again is not a new
// version, so repeated syncs and credential renewals append no rows.
func evidenceVersion(record EvidenceRecord) string {
	if record.Type != "change" {
		return "immutable"
	}
	raw, _ := json.Marshal([]any{record.Type, record.Namespace, record.Name, record.Kind, record.UID, record.RootUID, record.Rule, record.Priority, evidenceContentWithoutTimes(record.Payload)})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// evidenceContentWithoutTimes drops the top-level detection and observation
// times from a payload. Anything that is not a JSON object is kept as is.
func evidenceContentWithoutTimes(payload json.RawMessage) any {
	var content map[string]json.RawMessage
	if json.Unmarshal(payload, &content) != nil || content == nil {
		return payload
	}
	delete(content, "at")
	delete(content, "observed_at")
	return content
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
