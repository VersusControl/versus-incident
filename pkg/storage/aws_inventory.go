package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

var (
	// ErrInvalidAWSInventory reports a unit, row or query the inventory
	// store cannot hold or answer as given.
	ErrInvalidAWSInventory = errors.New("storage: invalid aws inventory data")
	// ErrAWSInventoryFenced reports a guarded write refused because the
	// account was purged after the writer read its epoch. Nothing was
	// written, so a purged account cannot come back from an in-flight sync.
	ErrAWSInventoryFenced = errors.New("storage: aws account purged since the writer started")
)

// AWSInventoryStore keeps AWS collector output in indexed tables. It is
// optional: a provider that does not implement it reports the inventory as
// not ready, and callers find it by type assertion like EvidenceStore.
//
// Every write goes to scope.Write. Every read resolves the ordered
// scope.Read: for each unit (account, region, service) the first
// organization that holds the unit answers for it, and for each change
// window pair (account, region) the first organization holding coverage
// answers for it.
//
// Resources, health and security flags are temporal: a row is current while
// ValidTo is unset, and a read as of t sees rows with
// valid_from <= t < valid_to. A commit swaps a unit atomically; a failed or
// partial sync writes only the unit's sync state.
//
// A guard is the account epoch a writer read when it started. A guarded write
// whose account was purged since then fails with ErrAWSInventoryFenced and
// writes nothing. A nil guard writes unconditionally.
type AWSInventoryStore interface {
	// AWSAccountEpoch returns the account's current purge epoch in the write
	// organization, to be passed as the guard of later writes.
	AWSAccountEpoch(ctx context.Context, scope tenancy.OrgScope, account string) (uint64, error)
	// CommitAWSUnit makes candidate the unit's current snapshot. A candidate
	// with the current generation's digest only refreshes the verification
	// time. Rows that did not change keep their original valid_from.
	CommitAWSUnit(ctx context.Context, scope tenancy.OrgScope, unit AWSUnit, candidate AWSCandidate, guard *uint64) (AWSCommit, error)
	// UpdateAWSSync reads the unit's sync state under the write lock and
	// stores it when update returns true. The generation is owned by commits
	// and is never changed by update.
	UpdateAWSSync(ctx context.Context, scope tenancy.OrgScope, unit AWSUnit, guard *uint64, update func(state *AWSSyncState, found bool) bool) error
	// AppendAWSChanges stores changes for one account and region, replacing
	// any stored change with the same event ID, and merges covered into the
	// pair's coverage in the same transaction.
	AppendAWSChanges(ctx context.Context, scope tenancy.OrgScope, account, region string, changes []AWSChange, covered []AWSInterval, guard *uint64) error
	// PurgeAWSAccount deletes every row the write organization holds for an
	// account, or only one region of it when region is set, and advances the
	// account epoch in the same transaction.
	PurgeAWSAccount(ctx context.Context, scope tenancy.OrgScope, account, region string) error
	// PruneAWSHistory deletes history rows that stopped being current before
	// the cutoff. Current rows are never deleted.
	PruneAWSHistory(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error)
	// PruneAWSUnitHistory keeps the unit's newest keep generations and
	// deletes its older generations and every row that stopped being current
	// before the oldest kept generation began.
	PruneAWSUnitHistory(ctx context.Context, scope tenancy.OrgScope, unit AWSUnit, keep int) (int64, error)
	// PruneAWSChanges deletes changes before the cutoff and trims coverage
	// so it never claims the deleted stretch.
	PruneAWSChanges(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error)

	// RegisterAWSPending records every unit as pending in one transaction, so
	// a reader during a first sync sees the plan's fixed total from the
	// start instead of a total that grows unit by unit. Units that already
	// have a sync state are left alone.
	RegisterAWSPending(ctx context.Context, scope tenancy.OrgScope, units []AWSUnit, guard *uint64) error
	// AWSUnits lists units with their sync state and the generation that was
	// current at query.AsOf (the latest when AsOf is zero).
	AWSUnits(ctx context.Context, scope tenancy.OrgScope, query AWSUnitQuery) ([]AWSUnitStatus, error)
	// AWSAccountView reads one account's units, resources, health and flags
	// from a single snapshot, so a concurrent commit or purge can never
	// pair one generation's status with another generation's rows. An
	// unreadable view is an error, never a mixed answer.
	AWSAccountView(ctx context.Context, scope tenancy.OrgScope, account string, asOf time.Time) (AWSSnapshotView, error)
	// AWSResources lists resources ordered by account, region, service, ARN.
	AWSResources(ctx context.Context, scope tenancy.OrgScope, query AWSResourceQuery) ([]AWSResource, error)
	// AWSHealth lists health rows ordered by account, region, service, ARN,
	// kind, ref.
	AWSHealth(ctx context.Context, scope tenancy.OrgScope, query AWSHealthQuery) ([]AWSHealth, error)
	// AWSSecurityFlags lists flags ordered by account, region, service, ARN,
	// flag.
	AWSSecurityFlags(ctx context.Context, scope tenancy.OrgScope, query AWSFlagQuery) ([]AWSSecurityFlag, error)
	// AWSChanges lists changes in [Since, Until) ordered by time and event ID.
	AWSChanges(ctx context.Context, scope tenancy.OrgScope, query AWSChangeQuery) ([]AWSChange, error)
	// AWSChangeCoverage lists the covered intervals of every matching pair,
	// ordered by account and region, intervals by start.
	AWSChangeCoverage(ctx context.Context, scope tenancy.OrgScope, account, region string) ([]AWSPairCoverage, error)
	// AWSPairs lists the account and region pairs the write organization
	// holds a sync unit or change coverage for.
	AWSPairs(ctx context.Context, scope tenancy.OrgScope) ([]AWSPair, error)
}

// AWSSyncOK is the sync state a commit gives its unit.
const AWSSyncOK = "ok"

// AWSSyncPending is the state RegisterAWSPending gives a unit the first time
// a sync names it.
const AWSSyncPending = "pending"

// AWSInventory returns the provider's inventory store, if it has one.
func AWSInventory(provider Provider) (AWSInventoryStore, bool) {
	store, ok := provider.(AWSInventoryStore)
	return store, ok
}

// AWSRetentionSweeper deletes expired inventory rows in bounded, resumable
// batches. It is an optional capability beside AWSInventoryStore: a caller
// repeats one pass with the returned cursor until More is false, so each
// statement holds its locks only briefly and no sweep blocks a reader or a
// writer for the whole backlog. Current rows (valid_to IS NULL) are never
// deleted.
type AWSRetentionSweeper interface {
	SweepAWSRetention(ctx context.Context, sweep AWSRetentionSweep) (AWSRetentionResult, error)
}

// AWSRetentionSweep is one bounded retention pass. A pass walks units,
// expired history and expired changes in that order and stops at Deadline or
// after Batch rows; Cursor resumes it next time.
type AWSRetentionSweep struct {
	// Scope names the organizations; only the write organization is swept.
	Scope tenancy.OrgScope
	// HistoryBefore deletes history rows that stopped being current before
	// it. Zero skips the history stage.
	HistoryBefore time.Time
	// ChangeBefore deletes changes recorded before it. Zero skips the change
	// stage.
	ChangeBefore time.Time
	// KeepGenerations is how many newest generations each unit keeps; it
	// must be at least one.
	KeepGenerations int
	// Batch is the most rows one statement deletes. Zero uses the default.
	Batch int
	// Deadline stops the pass early; zero means no time budget.
	Deadline time.Time
	// Cursor resumes an earlier pass. The zero value starts from the top.
	Cursor AWSRetentionCursor
}

// DefaultAWSRetentionBatch is the row count one retention statement deletes.
const DefaultAWSRetentionBatch = 2000

// AWSRetentionCursor is where a pass stopped. Stage is one of "units",
// "history" or "changes"; empty means the pass finished.
type AWSRetentionCursor struct {
	Stage string
	// After is the last unit fully swept in the units stage.
	After AWSUnit
}

// AWSRetentionResult is what one pass removed.
type AWSRetentionResult struct {
	Units       int
	Generations int64
	History     int64
	Changes     int64
	// More is true when the pass stopped before the backlog was empty.
	More   bool
	Cursor AWSRetentionCursor
}

// AWSUnit is one collected slice: an account, a region and a service.
type AWSUnit struct {
	Account string `json:"account"`
	Region  string `json:"region"`
	Service string `json:"service"`
}

// AWSPair is an account and region.
type AWSPair struct {
	Account string `json:"account"`
	Region  string `json:"region"`
}

// AWSSyncState is a unit's latest attempt. Gaps is the caller's document.
type AWSSyncState struct {
	State       string          `json:"state"`
	Generation  uint64          `json:"generation"`
	LastGoodAt  time.Time       `json:"last_good_at,omitzero"`
	AttemptedAt time.Time       `json:"attempted_at,omitzero"`
	VerifiedAt  time.Time       `json:"verified_at,omitzero"`
	ErrorCode   string          `json:"error_code,omitempty"`
	Gaps        json.RawMessage `json:"gaps,omitempty"`
}

// AWSGeneration is one committed snapshot of a unit.
type AWSGeneration struct {
	Seq         uint64          `json:"seq"`
	Digest      string          `json:"digest"`
	CommittedAt time.Time       `json:"committed_at"`
	VerifiedAt  time.Time       `json:"verified_at"`
	Count       int             `json:"count"`
	Gaps        json.RawMessage `json:"gaps,omitempty"`
}

// AWSUnitStatus is a unit as a read sees it.
type AWSUnitStatus struct {
	Org        string         `json:"org"`
	Unit       AWSUnit        `json:"unit"`
	Sync       AWSSyncState   `json:"sync"`
	Generation *AWSGeneration `json:"generation,omitempty"`
}

// AWSSnapshotView is one account's inventory read from one consistent
// snapshot: the unit statuses and every row the units held at the same
// instant.
type AWSSnapshotView struct {
	Units     []AWSUnitStatus
	Resources []AWSResource
	Health    []AWSHealth
	Flags     []AWSSecurityFlag
}

// AWSRelation is one edge from a resource.
type AWSRelation struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

// AWSResource is one resource row. Account, Region and Service name the unit
// that holds it. Tags must already be redacted. Detail holds the caller's
// fields the store does not query.
type AWSResource struct {
	Account    string            `json:"account"`
	Region     string            `json:"region"`
	Service    string            `json:"service"`
	ARN        string            `json:"arn"`
	Type       string            `json:"type"`
	App        string            `json:"app,omitempty"`
	Name       string            `json:"name,omitempty"`
	State      string            `json:"state,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Relations  []AWSRelation     `json:"relations,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Detail     json.RawMessage   `json:"detail,omitempty"`
}

// AWSHealth is one health signal of a unit. ARN is the affected resource
// (empty for an account-level signal), Kind its source and Ref the alarm or
// event that raised it.
type AWSHealth struct {
	Account  string          `json:"account"`
	Region   string          `json:"region"`
	Service  string          `json:"service"`
	ARN      string          `json:"arn,omitempty"`
	Kind     string          `json:"kind"`
	Ref      string          `json:"ref,omitempty"`
	State    string          `json:"state"`
	Summary  string          `json:"summary,omitempty"`
	Since    time.Time       `json:"since,omitzero"`
	Children []string        `json:"children,omitempty"`
	Detail   json.RawMessage `json:"detail,omitempty"`
}

// AWSSecurityFlag is one security flag of a unit.
type AWSSecurityFlag struct {
	Account string          `json:"account"`
	Region  string          `json:"region"`
	Service string          `json:"service"`
	ARN     string          `json:"arn"`
	Flag    string          `json:"flag"`
	Summary string          `json:"summary,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// AWSChange is one recorded change event. Fields must hold only allowlisted
// values; Detail holds the caller's fields the store does not query.
type AWSChange struct {
	EventID     string            `json:"event_id"`
	Account     string            `json:"account"`
	Region      string            `json:"region"`
	Service     string            `json:"service,omitempty"`
	EventTime   time.Time         `json:"event_time"`
	EventName   string            `json:"event_name,omitempty"`
	ActorClass  string            `json:"actor_class,omitempty"`
	Actor       string            `json:"actor,omitempty"`
	ResourceARN string            `json:"resource_arn,omitempty"`
	App         string            `json:"app,omitempty"`
	Summary     string            `json:"summary,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"`
	Detail      json.RawMessage   `json:"detail,omitempty"`
}

// AWSInterval is a half-open time interval [From, To).
type AWSInterval struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// AWSPairCoverage is the change coverage of one account and region.
type AWSPairCoverage struct {
	Org       string        `json:"org"`
	Account   string        `json:"account"`
	Region    string        `json:"region"`
	Intervals []AWSInterval `json:"intervals"`
}

// AWSCandidate is one sync's validated output for a unit. At is the commit
// time; Digest identifies the content so an identical sync only refreshes
// the verification time.
type AWSCandidate struct {
	At        time.Time
	Digest    string
	Gaps      json.RawMessage
	Resources []AWSResource
	Health    []AWSHealth
	Flags     []AWSSecurityFlag
}

// AWSCommit reports a commit. Unchanged is true when the candidate matched
// the current generation and only its verification time moved.
type AWSCommit struct {
	Generation AWSGeneration
	Unchanged  bool
}

// AWSUnitQuery selects units. Empty fields match everything.
type AWSUnitQuery struct {
	Account string
	Region  string
	Service string
	AsOf    time.Time
}

// AWSResourceQuery selects resources. NamePrefix matches the start of the
// name without ASCII case.
type AWSResourceQuery struct {
	Account    string
	Region     string
	Service    string
	Type       string
	App        string
	ARN        string
	NamePrefix string
	AsOf       time.Time
	Limit      int
}

// AWSHealthQuery selects health rows. ARN matches the affected resource or
// the ref.
type AWSHealthQuery struct {
	Account string
	Region  string
	Service string
	State   string
	ARN     string
	AsOf    time.Time
}

// AWSFlagQuery selects security flags.
type AWSFlagQuery struct {
	Account string
	Region  string
	Service string
	Flag    string
	ARN     string
	AsOf    time.Time
}

// AWSChangeQuery selects changes in [Since, Until). A zero bound is open.
type AWSChangeQuery struct {
	Account     string
	Region      string
	Service     string
	ResourceARN string
	App         string
	Since       time.Time
	Until       time.Time
	Limit       int
}

// inventoryScope normalizes a scope; reads always have at least the write
// organization.
func inventoryScope(scope tenancy.OrgScope) tenancy.OrgScope {
	return scope.Normalized()
}

// tableTime is the stored precision of every table time: UTC microseconds,
// which every backend can hold.
func tableTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Time{}
	}
	return at.UTC().Truncate(time.Microsecond)
}

func validAWSUnit(unit AWSUnit) bool {
	return validTableKey(unit.Account) && validTableKey(unit.Region) && validTableKey(unit.Service)
}

// validTableKey accepts a non-empty identifier without control characters.
func validTableKey(value string) bool {
	if value == "" || len(value) > 2048 {
		return false
	}
	for _, ch := range value {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

// validTableText accepts any text the databases can hold: valid UTF-8 without
// NUL.
func validTableText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validTableJSON(document json.RawMessage) bool {
	return len(document) == 0 || json.Valid(document) && validTableText(string(document))
}

func validTableStrings(values ...string) bool {
	for _, value := range values {
		if !validTableText(value) {
			return false
		}
	}
	return true
}

func validTableMap(values map[string]string) bool {
	for key, value := range values {
		if !validTableText(key) || !validTableText(value) {
			return false
		}
	}
	return true
}

// emptyJSON reports whether a document holds nothing worth storing.
func emptyJSON(document json.RawMessage) bool {
	return len(document) == 0 || string(document) == "null"
}

// rowDigest identifies a row's full content. Rows with the same digest are
// the same row, so a commit keeps them instead of closing and reinserting.
func rowDigest(row any) string {
	data, err := json.Marshal(row)
	if err != nil {
		panic(fmt.Sprintf("storage: row digest: %v", err))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cleanAWSResource(unit AWSUnit, row AWSResource) (AWSResource, error) {
	row.Account, row.Region, row.Service = unit.Account, unit.Region, unit.Service
	if !validTableKey(row.ARN) || !validTableStrings(row.Type, row.App, row.Name, row.State) || !validTableMap(row.Tags) || !validTableMap(row.Attributes) || !validTableJSON(row.Detail) {
		return AWSResource{}, ErrInvalidAWSInventory
	}
	for _, relation := range row.Relations {
		if !validTableStrings(relation.Type, relation.Target) {
			return AWSResource{}, ErrInvalidAWSInventory
		}
	}
	row.Tags = cloneTableMap(row.Tags)
	row.Attributes = cloneTableMap(row.Attributes)
	if len(row.Relations) == 0 {
		row.Relations = nil
	} else {
		row.Relations = append([]AWSRelation(nil), row.Relations...)
	}
	row.Detail = cloneTableJSON(row.Detail)
	return row, nil
}

func cleanAWSHealth(unit AWSUnit, row AWSHealth) (AWSHealth, error) {
	row.Account, row.Region, row.Service = unit.Account, unit.Region, unit.Service
	if row.Kind == "" || !validTableStrings(row.ARN, row.Kind, row.Ref, row.State, row.Summary) || !validTableStrings(row.Children...) || !validTableJSON(row.Detail) {
		return AWSHealth{}, ErrInvalidAWSInventory
	}
	row.Since = tableTime(row.Since)
	if len(row.Children) == 0 {
		row.Children = nil
	} else {
		row.Children = append([]string(nil), row.Children...)
	}
	row.Detail = cloneTableJSON(row.Detail)
	return row, nil
}

func cleanAWSFlag(unit AWSUnit, row AWSSecurityFlag) (AWSSecurityFlag, error) {
	row.Account, row.Region, row.Service = unit.Account, unit.Region, unit.Service
	if !validTableKey(row.ARN) || !validTableKey(row.Flag) || !validTableText(row.Summary) || !validTableJSON(row.Detail) {
		return AWSSecurityFlag{}, ErrInvalidAWSInventory
	}
	row.Detail = cloneTableJSON(row.Detail)
	return row, nil
}

func cleanAWSChange(account, region string, row AWSChange) (AWSChange, error) {
	if !validTableKey(row.EventID) || row.Account != account || row.Region != region || row.EventTime.IsZero() ||
		!validTableStrings(row.Service, row.EventName, row.ActorClass, row.Actor, row.ResourceARN, row.App, row.Summary) ||
		!validTableMap(row.Fields) || !validTableJSON(row.Detail) {
		return AWSChange{}, ErrInvalidAWSInventory
	}
	row.EventTime = tableTime(row.EventTime)
	row.Fields = cloneTableMap(row.Fields)
	row.Detail = cloneTableJSON(row.Detail)
	return row, nil
}

func cloneTableMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneTableJSON(document json.RawMessage) json.RawMessage {
	if emptyJSON(document) {
		return nil
	}
	return append(json.RawMessage(nil), document...)
}

// awsCommitRows is a candidate cleaned for a unit: rows deduplicated by key
// (first wins) and each row's digest.
type awsCommitRows struct {
	at        time.Time
	gaps      json.RawMessage
	resources []AWSResource
	health    []AWSHealth
	flags     []AWSSecurityFlag
	digests   [3][]string
}

func prepareAWSCommit(unit AWSUnit, candidate AWSCandidate) (awsCommitRows, error) {
	if !validAWSUnit(unit) || candidate.At.IsZero() || candidate.Digest == "" || !validTableKey(candidate.Digest) || !validTableJSON(candidate.Gaps) {
		return awsCommitRows{}, ErrInvalidAWSInventory
	}
	out := awsCommitRows{at: tableTime(candidate.At), gaps: cloneTableJSON(candidate.Gaps)}
	seen := make(map[string]bool, len(candidate.Resources))
	for _, row := range candidate.Resources {
		cleaned, err := cleanAWSResource(unit, row)
		if err != nil {
			return awsCommitRows{}, err
		}
		if seen[cleaned.ARN] {
			continue
		}
		seen[cleaned.ARN] = true
		out.resources = append(out.resources, cleaned)
		out.digests[0] = append(out.digests[0], rowDigest(cleaned))
	}
	seen = make(map[string]bool, len(candidate.Health))
	for _, row := range candidate.Health {
		cleaned, err := cleanAWSHealth(unit, row)
		if err != nil {
			return awsCommitRows{}, err
		}
		key := cleaned.ARN + "\x00" + cleaned.Kind + "\x00" + cleaned.Ref
		if seen[key] {
			continue
		}
		seen[key] = true
		out.health = append(out.health, cleaned)
		out.digests[1] = append(out.digests[1], rowDigest(cleaned))
	}
	seen = make(map[string]bool, len(candidate.Flags))
	for _, row := range candidate.Flags {
		cleaned, err := cleanAWSFlag(unit, row)
		if err != nil {
			return awsCommitRows{}, err
		}
		key := cleaned.ARN + "\x00" + cleaned.Flag
		if seen[key] {
			continue
		}
		seen[key] = true
		out.flags = append(out.flags, cleaned)
		out.digests[2] = append(out.digests[2], rowDigest(cleaned))
	}
	return out, nil
}

// mergeAWSIntervals returns the union of intervals, sorted and with
// touching intervals joined. Empty intervals are dropped.
func mergeAWSIntervals(intervals []AWSInterval) []AWSInterval {
	cleaned := make([]AWSInterval, 0, len(intervals))
	for _, interval := range intervals {
		interval.From, interval.To = tableTime(interval.From), tableTime(interval.To)
		if interval.From.IsZero() || !interval.To.After(interval.From) {
			continue
		}
		cleaned = append(cleaned, interval)
	}
	sort.Slice(cleaned, func(left, right int) bool { return cleaned[left].From.Before(cleaned[right].From) })
	merged := make([]AWSInterval, 0, len(cleaned))
	for _, interval := range cleaned {
		if last := len(merged) - 1; last >= 0 && !interval.From.After(merged[last].To) {
			if interval.To.After(merged[last].To) {
				merged[last].To = interval.To
			}
			continue
		}
		merged = append(merged, interval)
	}
	return merged
}

// trimAWSIntervals drops the part of each interval before cutoff.
func trimAWSIntervals(intervals []AWSInterval, cutoff time.Time) []AWSInterval {
	out := make([]AWSInterval, 0, len(intervals))
	for _, interval := range intervals {
		if !interval.To.After(cutoff) {
			continue
		}
		if interval.From.Before(cutoff) {
			interval.From = cutoff
		}
		out = append(out, interval)
	}
	return out
}

// asciiLower lowers ASCII letters only, as every backend's lower() does.
func asciiLower(value string) string {
	out := []byte(value)
	for index, ch := range out {
		if ch >= 'A' && ch <= 'Z' {
			out[index] = ch + ('a' - 'A')
		}
	}
	return string(out)
}

// prefixUpperBound is the exclusive upper bound of every string that starts
// with prefix, for an index range scan in byte order.
func prefixUpperBound(prefix string) string { return prefix + "\U0010FFFF" }
