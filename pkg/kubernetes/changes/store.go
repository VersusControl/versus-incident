package changes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

var (
	ErrInvalidQuery = errors.New("kubernetes changes: invalid query")
	ErrStoreLimit   = errors.New("kubernetes changes: stored snapshot exceeds limit")
	ErrLeaseLost    = errors.New("kubernetes changes: capture lease lost")
)

const (
	defaultRetention    = 24 * time.Hour
	maxChanges          = 5000
	maxStoreBytes       = (4 << 20) + breaklogs.MaxBytes
	maxChangeFields     = 16
	maxLifecycleSamples = 256
)

type Gap struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type Query struct {
	Since     time.Time
	Until     time.Time
	Namespace string
	Kind      string
	Name      string
	Type      Type
	Limit     int
	Cursor    string
}

type Page struct {
	Items    []Change       `json:"items"`
	Next     string         `json:"next,omitempty"`
	Gaps     []Gap          `json:"gaps,omitempty"`
	Coverage map[string]Gap `json:"coverage,omitempty"`
}

type snapshot struct {
	BreakLogs      breaklogs.State              `json:"break_logs,omitempty"`
	Changes        []Change                     `json:"changes"`
	Gaps           []Gap                        `json:"gaps,omitempty"`
	Coverage       map[string]Gap               `json:"coverage,omitempty"`
	Lease          *Lease                       `json:"lease,omitempty"`
	WarningEvents  map[string]warningCheckpoint `json:"warning_events,omitempty"`
	WarningWindows map[string]warningCheckpoint `json:"warning_windows,omitempty"`
	Crashes        []CrashEvent                 `json:"crashes,omitempty"`
	CrashSequence  uint64                       `json:"crash_sequence,omitempty"`
	CrashStartedAt time.Time                    `json:"crash_started_at,omitempty"`
	CrashGaps      []Gap                        `json:"crash_gaps,omitempty"`
}

type warningCheckpoint struct {
	Count int64     `json:"count"`
	At    time.Time `json:"at"`
}

type Lease struct {
	Owner   string    `json:"owner"`
	Expires time.Time `json:"expires"`
}

type Store struct {
	mu        sync.Mutex
	provider  storage.Provider
	key       string
	retention time.Duration
	archives  []*Store
}

func NewView(provider storage.Provider, scope index.Scope, archiveOrgs []string) (*Store, error) {
	store := NewStore(provider, scope)
	if store == nil || len(archiveOrgs) > 1 {
		return nil, ErrStoreLimit
	}
	for _, org := range archiveOrgs {
		if org == "" || org == scope.OrgID {
			return nil, ErrInvalidQuery
		}
		archiveScope := scope
		archiveScope.OrgID = org
		store.archives = append(store.archives, NewStore(provider, archiveScope))
	}
	return store, nil
}

// NewStore creates a bounded timeline store isolated by the complete scope.
func NewStore(provider storage.Provider, scope index.Scope) *Store {
	if provider == nil || scope.ClusterID == "" {
		return nil
	}
	digest := sha256.Sum256([]byte(scope.OrgID + "\x00" + scope.ClusterID + "\x00" + scope.CredentialID))
	return &Store{provider: provider, key: "kubernetes/changes/" + hex.EncodeToString(digest[:]) + ".json", retention: defaultRetention}
}

// Append adds projected changes and evicts only the oldest records as required by bounds.
func (store *Store) Append(changes []Change, now time.Time) error {
	return store.Commit(changes, nil, nil, "", now)
}

// MarkGap records a bounded interval for which the timeline cannot guarantee completeness.
func (store *Store) MarkGap(gap Gap) error {
	return store.Commit(nil, []Gap{gap}, nil, "", time.Now().UTC())
}

func (store *Store) Commit(changes []Change, gaps []Gap, coverage map[string]Gap, owner string, now time.Time) error {
	return store.CommitCapture(changes, gaps, coverage, owner, now, nil, nil, false)
}

func (store *Store) CommitCapture(changes []Change, gaps []Gap, coverage map[string]Gap, owner string, now time.Time, crashes []CrashEvent, crashGaps []Gap, podRead bool) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for _, change := range changes {
		if !validChange(change) {
			return ErrInvalidQuery
		}
	}
	for _, gap := range gaps {
		if gap.From.IsZero() || gap.To.Before(gap.From) {
			return ErrInvalidQuery
		}
	}
	for kind, interval := range coverage {
		if !safeFilter(kind) || interval.From.IsZero() || interval.To.Before(interval.From) {
			return ErrInvalidQuery
		}
	}
	for _, event := range crashes {
		if !validCrash(event) || event.ObservedAt.After(now) {
			return ErrInvalidQuery
		}
	}
	for _, gap := range crashGaps {
		if gap.From.IsZero() || gap.To.Before(gap.From) {
			return ErrInvalidQuery
		}
	}
	return store.commitSnapshot(func(current *snapshot) error {
		if owner == "" && current.Lease != nil {
			if current.Lease.Expires.After(time.Now().UTC()) {
				return ErrLeaseLost
			}
			current.Lease = nil
		}
		if owner != "" && (current.Lease == nil || current.Lease.Owner != owner || !current.Lease.Expires.After(time.Now().UTC())) {
			return ErrLeaseLost
		}
		if err := mergeCrashes(current, crashes, crashGaps, podRead, now, store.retention); err != nil {
			return err
		}
		if err := breaklogs.Merge(&current.BreakLogs, nil, nil, "", now); err != nil {
			return err
		}
		cutoff := now.Add(-store.retention)
		pruneWarningCheckpoints(current, now)
		kept := []Change{}
		seen := map[string]int{}
		for _, change := range current.Changes {
			if !change.At.Before(cutoff) {
				seen[change.ID] = len(kept)
				kept = append(kept, change)
			}
		}
		for _, change := range changes {
			if change.At.Before(cutoff) {
				continue
			}
			if change.warning != nil {
				var emit bool
				change, emit = accumulateWarning(current, change, now)
				if !emit {
					continue
				}
			}
			if position, exists := seen[change.ID]; exists {
				if change.Type == WarningBurst {
					previous := kept[position]
					updated := cloneChange(change)
					if previous.At.After(updated.At) {
						updated.At = previous.At
					}
					if previous.Source == "observed" {
						updated.Source = "observed"
					}
					if warningCount(previous) > warningCount(updated) {
						updated.Fields = append([]FieldChange(nil), previous.Fields...)
					}
					kept[position] = updated
				} else if change.Source == "observed" && kept[position].Source == "reconstructed" {
					kept[position] = cloneChange(change)
				}
				continue
			}
			seen[change.ID] = len(kept)
			kept = append(kept, cloneChange(change))
		}
		sort.Slice(kept, func(left, right int) bool {
			if kept[left].At.Equal(kept[right].At) {
				return kept[left].ID < kept[right].ID
			}
			return kept[left].At.Before(kept[right].At)
		})
		current.Changes = kept
		samples := 0
		for _, change := range kept {
			if change.Coalesced {
				samples++
			}
		}
		if samples > maxLifecycleSamples {
			bounded := make([]Change, 0, len(kept))
			for _, change := range kept {
				if change.Coalesced && samples > maxLifecycleSamples {
					samples--
					continue
				}
				bounded = append(bounded, change)
			}
			current.Changes = bounded
		}
		for _, gap := range gaps {
			current.Gaps = append(current.Gaps, gap)
		}
		if current.Coverage == nil && len(coverage) > 0 {
			current.Coverage = map[string]Gap{}
		}
		for kind, interval := range coverage {
			old, exists := current.Coverage[kind]
			if exists && !interval.From.After(old.To) {
				if interval.From.Before(old.From) {
					old.From = interval.From
				}
				if interval.To.After(old.To) {
					old.To = interval.To
				}
				interval = old
			}
			current.Coverage[kind] = interval
		}
		for kind, interval := range current.Coverage {
			if interval.To.Before(cutoff) {
				delete(current.Coverage, kind)
			} else if interval.From.Before(cutoff) {
				interval.From = cutoff
				current.Coverage[kind] = interval
			}
		}
		compactGaps(current)
		for size := historySize(*current); size > 4<<20 && len(current.Crashes) > 0; size = historySize(*current) {
			removed := 0
			for removed < size-(4<<20) && len(current.Crashes) > 0 {
				encoded, _ := json.Marshal(current.Crashes[0])
				removed += len(encoded) + 1
				dropCrash(current)
			}
		}
		for len(current.Changes) > maxChanges || historySize(*current) > 4<<20 {
			if len(current.Changes) == 0 {
				return ErrStoreLimit
			}
			position := 0
			for candidate, change := range current.Changes {
				if change.Coalesced {
					position = candidate
					break
				}
			}
			dropped := current.Changes[position]
			current.Changes = append(current.Changes[:position], current.Changes[position+1:]...)
			if !dropped.Coalesced {
				current.Gaps = append(current.Gaps, Gap{From: dropped.At, To: dropped.At})
			}
			compactGaps(current)
		}
		return nil
	})
}

func compactGaps(current *snapshot) {
	sort.Slice(current.Gaps, func(left, right int) bool { return current.Gaps[left].From.Before(current.Gaps[right].From) })
	combined := []Gap{}
	for _, gap := range current.Gaps {
		appendGap(&combined, gap)
	}
	for len(combined) > 16 {
		combined[1].From = combined[0].From
		combined = combined[1:]
	}
	current.Gaps = combined
}

func warningCount(change Change) int {
	for _, field := range change.Fields {
		if field.Path == "events.warning_count" {
			count, _ := strconv.Atoi(field.To)
			return count
		}
	}
	return 0
}

func pruneWarningCheckpoints(current *snapshot, now time.Time) {
	for _, checkpoints := range []map[string]warningCheckpoint{current.WarningEvents, current.WarningWindows} {
		for key, checkpoint := range checkpoints {
			if checkpoint.At.Before(now.Add(-10 * time.Minute)) {
				delete(checkpoints, key)
			}
		}
	}
}

func accumulateWarning(current *snapshot, change Change, now time.Time) (Change, bool) {
	observation := change.warning
	if change.At.After(now) || change.At.Before(now.Add(-5*time.Minute)) {
		return change, false
	}
	if current.WarningEvents == nil {
		current.WarningEvents = map[string]warningCheckpoint{}
	}
	if current.WarningWindows == nil {
		current.WarningWindows = map[string]warningCheckpoint{}
	}
	digest := sha256.Sum256([]byte(observation.UID))
	eventKey := hex.EncodeToString(digest[:])
	previous, exists := current.WarningEvents[eventKey]
	if exists && (observation.Count <= previous.Count || change.At.Before(previous.At)) {
		return change, false
	}
	baseline := observation.Count
	baselineAt := change.At
	if exists {
		baseline, baselineAt = previous.Count, previous.At
	} else if observation.HasPrevious {
		baseline, baselineAt = observation.Previous, observation.PreviousAt
	} else if observation.First.UTC().Truncate(5 * time.Minute).Equal(change.At.UTC().Truncate(5 * time.Minute)) {
		baseline, baselineAt = 0, observation.First
	}
	if !exists && len(current.WarningEvents) >= maxChanges {
		return change, false
	}
	current.WarningEvents[eventKey] = warningCheckpoint{Count: observation.Count, At: change.At}
	delta := observation.Count - baseline
	if delta <= 0 || change.At.Sub(baselineAt) > 5*time.Minute || baselineAt.After(change.At) {
		return change, false
	}
	window, exists := current.WarningWindows[change.ID]
	if !exists && len(current.WarningWindows) >= maxChanges {
		return change, false
	}
	window.Count += delta
	if change.At.After(window.At) {
		window.At = change.At
	}
	current.WarningWindows[change.ID] = window
	change.warning = nil
	change.Fields = []FieldChange{{Path: "events.warning_count", To: strconv.FormatInt(window.Count, 10)}}
	return change, window.Count >= 5
}

func historySize(current snapshot) int {
	current.BreakLogs = breaklogs.State{}
	return encodedSize(current)
}

func (store *Store) commitSnapshot(update func(*snapshot) error) error {
	if store == nil || store.provider == nil {
		return ErrStoreLimit
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for attempt := 0; attempt < 64; attempt++ {
		expected, err := store.provider.ReadBlob(store.key)
		if err != nil {
			return err
		}
		current, err := decodeSnapshot(expected)
		if err != nil {
			return err
		}
		before, err := json.Marshal(current)
		if err != nil {
			return ErrStoreLimit
		}
		if err := update(&current); err != nil {
			return err
		}
		replacement, err := json.Marshal(current)
		if err != nil || len(replacement) > maxStoreBytes {
			return ErrStoreLimit
		}
		if bytes.Equal(before, replacement) || bytes.Equal(expected, replacement) {
			return nil
		}
		if cas, ok := store.provider.(storage.BlobCAS); ok {
			swapped, err := cas.CompareAndSwapBlob(store.key, expected, replacement)
			if err != nil {
				return err
			}
			if swapped {
				return nil
			}
			continue
		}
		return store.provider.WriteBlob(store.key, replacement)
	}
	return ErrStoreLimit
}

func (store *Store) ClaimLease(owner string, now time.Time, ttl time.Duration) error {
	if owner == "" || len(owner) > 128 || ttl <= 0 || ttl > 5*time.Minute {
		return ErrInvalidQuery
	}
	if store == nil {
		return ErrStoreLimit
	}
	if _, ok := store.provider.(storage.BlobCAS); !ok {
		return ErrLeaseLost
	}
	return store.commitSnapshot(func(current *snapshot) error {
		if current.Lease != nil && current.Lease.Owner != owner && current.Lease.Expires.After(now) {
			return ErrLeaseLost
		}
		current.Lease = &Lease{Owner: owner, Expires: now.Add(ttl)}
		return nil
	})
}

func (store *Store) RenewLease(owner string, now time.Time, ttl time.Duration) error {
	return store.commitSnapshot(func(current *snapshot) error {
		if current.Lease == nil || current.Lease.Owner != owner || !current.Lease.Expires.After(now) {
			return ErrLeaseLost
		}
		current.Lease.Expires = now.Add(ttl)
		return nil
	})
}

func (store *Store) ReleaseLease(owner string) error {
	return store.commitSnapshot(func(current *snapshot) error {
		if current.Lease != nil && current.Lease.Owner == owner {
			current.Lease = nil
		}
		return nil
	})
}

// Query returns at most 500 newest matching changes and a numeric offset cursor.
func (store *Store) Query(query Query, now time.Time) (Page, error) {
	if store == nil || store.provider == nil {
		return Page{}, ErrStoreLimit
	}
	if query.Namespace != "" && !safeFilter(query.Namespace) || query.Kind != "" && !safeFilter(query.Kind) || query.Name != "" && !safeFilter(query.Name) || query.Type != "" && !validType(query.Type) {
		return Page{}, ErrInvalidQuery
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if query.Until.IsZero() {
		query.Until = now
	}
	if query.Since.IsZero() || query.Until.Sub(query.Since) > store.retention || query.Since.After(query.Until) {
		query.Since = query.Until.Add(-store.retention)
	}
	if query.Limit <= 0 || query.Limit > 500 {
		query.Limit = 100
	}
	offset := 0
	if query.Cursor != "" {
		parsed, err := strconv.Atoi(query.Cursor)
		if err != nil || parsed < 0 || parsed > maxChanges*(1+len(store.archives)) {
			return Page{}, ErrInvalidQuery
		}
		offset = parsed
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current, err := store.read()
	if err != nil {
		return Page{}, err
	}
	seen := make(map[string]bool, len(current.Changes))
	merged := make([]Change, 0, len(current.Changes))
	for _, source := range append([]*Store{store}, store.archives...) {
		part := current
		if source != store {
			part, err = source.read()
			if err != nil {
				return Page{}, err
			}
			current.Gaps = append(current.Gaps, part.Gaps...)
		}
		for _, change := range part.Changes {
			if !seen[change.ID] {
				seen[change.ID] = true
				merged = append(merged, cloneChange(change))
			}
		}
	}
	current.Changes = merged
	sort.Slice(current.Gaps, func(left, right int) bool { return current.Gaps[left].From.Before(current.Gaps[right].From) })
	combinedGaps := []Gap{}
	for _, gap := range current.Gaps {
		appendGap(&combinedGaps, gap)
	}
	current.Gaps = combinedGaps
	filtered := make([]Change, 0, len(current.Changes))
	for _, change := range current.Changes {
		if change.At.Before(query.Since) || change.At.After(query.Until) || query.Namespace != "" && change.Namespace != query.Namespace || query.Kind != "" && change.Kind != query.Kind || query.Name != "" && change.Name != query.Name || query.Type != "" && change.Type != query.Type {
			continue
		}
		filtered = append(filtered, change)
	}
	sort.Slice(filtered, func(left, right int) bool {
		if filtered[left].At.Equal(filtered[right].At) {
			return filtered[left].ID > filtered[right].ID
		}
		return filtered[left].At.After(filtered[right].At)
	})
	if offset > len(filtered) {
		return Page{}, ErrInvalidQuery
	}
	end := min(offset+query.Limit, len(filtered))
	page := Page{Items: append([]Change(nil), filtered[offset:end]...), Gaps: gapsInWindow(current.Gaps, query.Since, query.Until), Coverage: current.Coverage}
	if end < len(filtered) {
		page.Next = strconv.Itoa(end)
	}
	return page, nil
}

func (store *Store) read() (snapshot, error) {
	encoded, err := store.provider.ReadBlob(store.key)
	if err != nil {
		return snapshot{}, err
	}
	return decodeSnapshot(encoded)
}

func (store *Store) ReadWindow(from, to, now time.Time) (Page, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	current, err := store.read()
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: []Change{}, Coverage: current.Coverage, Gaps: gapsInWindow(current.Gaps, from, to)}
	for _, change := range current.Changes {
		if !change.At.Before(from) && !change.At.After(to) && !change.At.Before(now.Add(-store.retention)) {
			page.Items = append(page.Items, cloneChange(change))
		}
	}
	return page, nil
}

func decodeSnapshot(encoded []byte) (snapshot, error) {
	if len(encoded) == 0 {
		return snapshot{Changes: []Change{}, Gaps: []Gap{}}, nil
	}
	if len(encoded) > maxStoreBytes {
		return snapshot{}, ErrStoreLimit
	}
	var current snapshot
	if err := json.Unmarshal(encoded, &current); err != nil || len(current.Changes) > maxChanges || len(current.Gaps) > 16 || len(current.WarningEvents) > maxChanges || len(current.WarningWindows) > maxChanges {
		return snapshot{}, ErrStoreLimit
	}
	if len(current.Crashes) > maxCrashEvents || len(current.CrashGaps) > 16 {
		return snapshot{}, ErrStoreLimit
	}
	for _, event := range current.Crashes {
		if !validCrash(event) || event.Sequence == 0 || event.Sequence > current.CrashSequence {
			return snapshot{}, ErrStoreLimit
		}
	}
	for _, gap := range current.CrashGaps {
		if gap.From.IsZero() || gap.To.Before(gap.From) {
			return snapshot{}, ErrStoreLimit
		}
	}
	for _, checkpoints := range []map[string]warningCheckpoint{current.WarningEvents, current.WarningWindows} {
		for key, checkpoint := range checkpoints {
			if len(key) > 128 || key == "" || checkpoint.Count < 0 || checkpoint.At.IsZero() {
				return snapshot{}, ErrStoreLimit
			}
		}
	}
	for _, change := range current.Changes {
		if !validChange(change) {
			return snapshot{}, ErrStoreLimit
		}
	}
	for _, gap := range current.Gaps {
		if gap.From.IsZero() || gap.To.Before(gap.From) {
			return snapshot{}, ErrStoreLimit
		}
	}
	if len(current.Coverage) > 32 {
		return snapshot{}, ErrStoreLimit
	}
	for kind, interval := range current.Coverage {
		if !safeFilter(kind) || interval.From.IsZero() || interval.To.Before(interval.From) {
			return snapshot{}, ErrStoreLimit
		}
	}
	if current.Lease != nil && (current.Lease.Owner == "" || len(current.Lease.Owner) > 128 || current.Lease.Expires.IsZero()) {
		return snapshot{}, ErrStoreLimit
	}
	return current, nil
}

func validChange(change Change) bool {
	if change.Coalesced && (change.Kind != "Pod" || change.Type != Created && change.Type != Deleted) {
		return false
	}
	if change.Source != "" && change.Source != "observed" && change.Source != "reconstructed" || len(change.Revision) > 256 {
		return false
	}
	if change.ID == "" || len(change.ID) > 128 || change.Cluster == "" || len(change.Cluster) > 256 || change.Kind == "" || len(change.Kind) > 128 || change.Name == "" || len(change.Name) > 253 || len(change.Namespace) > 253 || len(change.UID) > 256 || change.At.IsZero() || !validType(change.Type) || len(change.Fields) > maxChangeFields || len(change.Service) > 253 {
		return false
	}
	for _, field := range change.Fields {
		if field.Path == "" || len(field.Path) > 256 || len(field.From) > 1024 || len(field.To) > 1024 || strings.ContainsAny(field.Path+field.From+field.To, "\x00\r\n") {
			return false
		}
	}
	return true
}

func validType(value Type) bool {
	switch value {
	case Created, Deleted, ImageChanged, ReplicasChanged, SpecChanged, RolloutStarted, RolloutCompleted, RolloutStalled, ConfigRefChanged, ConfigChanged, WarningBurst:
		return true
	default:
		return false
	}
}

func safeFilter(value string) bool {
	return len(value) <= 253 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func encodedSize(value snapshot) int {
	encoded, _ := json.Marshal(value)
	return len(encoded)
}

func cloneChange(value Change) Change {
	value.Fields = append([]FieldChange(nil), value.Fields...)
	return value
}

func appendGap(gaps *[]Gap, next Gap) {
	if count := len(*gaps); count > 0 && !next.From.After((*gaps)[count-1].To.Add(time.Second)) {
		if next.To.After((*gaps)[count-1].To) {
			(*gaps)[count-1].To = next.To
		}
		return
	}
	*gaps = append(*gaps, next)
}

func gapsInWindow(gaps []Gap, since, until time.Time) []Gap {
	result := make([]Gap, 0, len(gaps))
	for _, gap := range gaps {
		if !gap.To.Before(since) && !gap.From.After(until) {
			result = append(result, gap)
		}
	}
	return result
}
