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

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

var (
	ErrInvalidQuery = errors.New("kubernetes changes: invalid query")
	ErrStoreLimit   = errors.New("kubernetes changes: stored snapshot exceeds limit")
)

const (
	defaultRetention = 24 * time.Hour
	maxChanges       = 5000
	maxStoreBytes    = 4 << 20
	maxChangeFields  = 16
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
	Items []Change `json:"items"`
	Next  string   `json:"next,omitempty"`
	Gaps  []Gap    `json:"gaps,omitempty"`
}

type snapshot struct {
	Changes []Change `json:"changes"`
	Gaps    []Gap    `json:"gaps,omitempty"`
}

type Store struct {
	mu        sync.Mutex
	provider  storage.Provider
	key       string
	retention time.Duration
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
	if store == nil || store.provider == nil {
		return ErrStoreLimit
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for _, change := range changes {
		if !validChange(change) {
			return ErrInvalidQuery
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current, err := store.read()
	if err != nil {
		return err
	}
	before, err := json.Marshal(current)
	if err != nil {
		return ErrStoreLimit
	}
	cutoff := now.Add(-store.retention)
	kept := current.Changes[:0]
	for _, existing := range current.Changes {
		if !existing.At.Before(cutoff) {
			kept = append(kept, existing)
		}
	}
	current.Changes = kept
	seen := make(map[string]bool, len(current.Changes)+len(changes))
	for _, existing := range current.Changes {
		seen[existing.ID] = true
	}
	for _, change := range changes {
		if !seen[change.ID] {
			current.Changes = append(current.Changes, cloneChange(change))
			seen[change.ID] = true
		}
	}
	sort.Slice(current.Changes, func(left, right int) bool {
		if current.Changes[left].At.Equal(current.Changes[right].At) {
			return current.Changes[left].ID < current.Changes[right].ID
		}
		return current.Changes[left].At.Before(current.Changes[right].At)
	})
	for len(current.Changes) > maxChanges || encodedSize(current) > maxStoreBytes {
		if len(current.Changes) == 0 {
			return ErrStoreLimit
		}
		dropped := current.Changes[0]
		current.Changes = current.Changes[1:]
		appendGap(&current.Gaps, Gap{From: dropped.At, To: dropped.At})
	}
	if len(current.Gaps) > 16 {
		current.Gaps = append([]Gap(nil), current.Gaps[len(current.Gaps)-16:]...)
	}
	encoded, err := json.Marshal(current)
	if err != nil || len(encoded) > maxStoreBytes {
		return ErrStoreLimit
	}
	if bytes.Equal(before, encoded) {
		return nil
	}
	return store.provider.WriteBlob(store.key, encoded)
}

// MarkGap records a bounded interval for which the timeline cannot guarantee completeness.
func (store *Store) MarkGap(gap Gap) error {
	if store == nil || store.provider == nil || gap.From.IsZero() || gap.To.Before(gap.From) {
		return ErrInvalidQuery
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current, err := store.read()
	if err != nil {
		return err
	}
	before, err := json.Marshal(current)
	if err != nil {
		return ErrStoreLimit
	}
	appendGap(&current.Gaps, gap)
	if len(current.Gaps) > 16 {
		current.Gaps = append([]Gap(nil), current.Gaps[len(current.Gaps)-16:]...)
	}
	for encodedSize(current) > maxStoreBytes && len(current.Changes) > 0 {
		dropped := current.Changes[0]
		current.Changes = current.Changes[1:]
		appendGap(&current.Gaps, Gap{From: dropped.At, To: dropped.At})
	}
	encoded, err := json.Marshal(current)
	if err != nil || len(encoded) > maxStoreBytes {
		return ErrStoreLimit
	}
	if bytes.Equal(before, encoded) {
		return nil
	}
	return store.provider.WriteBlob(store.key, encoded)
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
		if err != nil || parsed < 0 || parsed > maxChanges {
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
	page := Page{Items: append([]Change(nil), filtered[offset:end]...), Gaps: gapsInWindow(current.Gaps, query.Since, query.Until)}
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
	if len(encoded) == 0 {
		return snapshot{Changes: []Change{}, Gaps: []Gap{}}, nil
	}
	if len(encoded) > maxStoreBytes {
		return snapshot{}, ErrStoreLimit
	}
	var current snapshot
	if err := json.Unmarshal(encoded, &current); err != nil || len(current.Changes) > maxChanges || len(current.Gaps) > 16 {
		return snapshot{}, ErrStoreLimit
	}
	return current, nil
}

func validChange(change Change) bool {
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
	case Created, Deleted, ImageChanged, ReplicasChanged, SpecChanged:
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
