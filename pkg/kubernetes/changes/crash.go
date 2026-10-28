package changes

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

const maxCrashEvents = 5000
const maxCrashBytes = 2 << 20
const maxCrashPageBytes = 64 << 10

type CrashEvent struct {
	ID              string    `json:"id"`
	Sequence        uint64    `json:"sequence"`
	Cluster         string    `json:"cluster"`
	Namespace       string    `json:"namespace"`
	Pod             string    `json:"pod"`
	PodUID          string    `json:"pod_uid"`
	RootUID         string    `json:"root_uid"`
	RootKind        string    `json:"root_kind"`
	RootName        string    `json:"root_name"`
	AncestryPartial bool      `json:"ancestry_partial,omitempty"`
	Container       string    `json:"container"`
	Init            bool      `json:"init,omitempty"`
	Type            string    `json:"type"`
	TerminationSlot string    `json:"termination_slot,omitempty"`
	WaitingReason   string    `json:"waiting_reason,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	ExitCode        int32     `json:"exit_code"`
	Signal          int32     `json:"signal,omitempty"`
	RestartCount    int32     `json:"restart_count"`
	At              time.Time `json:"at"`
	TimeSource      string    `json:"time_source"`
	ObservedAt      time.Time `json:"observed_at"`
}

// CrashQuery bounds observation time; CrashEvent.At remains the actual event time.
type CrashQuery struct {
	PodUID    string
	Since     time.Time
	Until     time.Time
	Namespace string
	RootUID   string
	Limit     int
	Cursor    string
}

type CrashPage struct {
	CrashCount     int            `json:"crash_count"`
	Items          []CrashEvent   `json:"items"`
	Next           string         `json:"next,omitempty"`
	Since          time.Time      `json:"since"`
	Until          time.Time      `json:"until"`
	RetentionFrom  time.Time      `json:"retention_from"`
	RetentionBasis string         `json:"retention_basis"`
	StartedAt      time.Time      `json:"started_at"`
	Gaps           []Gap          `json:"gaps"`
	CaptureGaps    []Gap          `json:"capture_gaps"`
	Coverage       map[string]Gap `json:"coverage"`
	Limitations    []string       `json:"limitations"`
}

type crashCursor struct {
	Binding        string    `json:"binding"`
	Since          time.Time `json:"since"`
	Until          time.Time `json:"until"`
	RequestedUntil time.Time `json:"requested_until,omitempty"`
	High           []uint64  `json:"high"`
	LastAt         time.Time `json:"last_at"`
	LastID         string    `json:"last_id"`
}

func CrashID(event CrashEvent) string {
	identity := []any{event.Cluster, event.PodUID, event.Container, event.Init, event.Type}
	if event.Type == "termination" {
		identity = append(identity, event.Reason, event.ExitCode, event.Signal)
		if event.TimeSource == "finished_at" {
			identity = append(identity, event.At.UTC().Format(time.RFC3339Nano))
		} else {
			identity = append(identity, event.RestartCount, event.TerminationSlot)
		}
	} else {
		identity = append(identity, event.WaitingReason, event.RestartCount)
	}
	encoded, _ := json.Marshal(identity)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (event CrashEvent) Valid() bool {
	return validCrash(event)
}

func validCrash(event CrashEvent) bool {
	for _, value := range []string{event.Cluster, event.Pod, event.PodUID, event.RootUID, event.RootKind, event.RootName, event.Container} {
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n\t ") {
			return false
		}
	}
	if len(event.Namespace) > 253 || !safeFilter(event.Namespace) || event.ObservedAt.IsZero() || event.At.IsZero() || event.At.After(event.ObservedAt) || event.RestartCount < 0 || event.Signal < 0 || event.Signal > 64 {
		return false
	}
	if event.TerminationSlot != "" && event.TerminationSlot != "current" && event.TerminationSlot != "previous" {
		return false
	}
	for _, reason := range []string{event.Reason, event.WaitingReason} {
		if len(reason) > 64 {
			return false
		}
		for _, letter := range reason {
			if letter < 'A' || letter > 'Z' && letter < 'a' || letter > 'z' {
				return false
			}
		}
	}
	return (event.Type == "status" || event.Type == "termination") && (event.TimeSource == "observed_at" || event.TimeSource == "finished_at") && event.ID == CrashID(event)
}

func mergeCrashes(current *snapshot, incoming []CrashEvent, gaps []Gap, podRead bool, now time.Time, retention time.Duration) error {
	if podRead && current.CrashStartedAt.IsZero() {
		current.CrashStartedAt = now
	}
	kept := make([]CrashEvent, 0, len(current.Crashes)+len(incoming))
	seen := map[string]bool{}
	for _, event := range current.Crashes {
		if !event.ObservedAt.Before(now.Add(-retention)) {
			kept = append(kept, event)
			seen[event.ID] = true
		}
	}
	for _, event := range incoming {
		if seen[event.ID] || event.ObservedAt.Before(now.Add(-retention)) {
			continue
		}
		if current.CrashSequence == ^uint64(0) {
			return ErrStoreLimit
		}
		current.CrashSequence++
		event.Sequence = current.CrashSequence
		seen[event.ID] = true
		kept = append(kept, event)
	}
	sort.Slice(kept, func(left, right int) bool { return kept[left].Sequence < kept[right].Sequence })
	current.Crashes = kept
	current.CrashGaps = append(current.CrashGaps, gaps...)
	compactCrashGaps(current)
	encoded, _ := json.Marshal(current.Crashes)
	remaining := len(encoded)
	for len(current.Crashes) > maxCrashEvents || remaining > maxCrashBytes {
		dropped, _ := json.Marshal(current.Crashes[0])
		remaining -= len(dropped) + 1
		dropCrash(current)
	}
	return nil
}

func compactCrashGaps(current *snapshot) {
	temporary := snapshot{Gaps: current.CrashGaps}
	compactGaps(&temporary)
	current.CrashGaps = temporary.Gaps
}

func dropCrash(current *snapshot) {
	event := current.Crashes[0]
	current.CrashGaps = append(current.CrashGaps, Gap{From: event.ObservedAt, To: event.ObservedAt})
	current.Crashes = current.Crashes[1:]
	compactCrashGaps(current)
}

func (store *Store) QueryCrashes(query CrashQuery, now time.Time) (CrashPage, error) {
	if store == nil || store.provider == nil {
		return CrashPage{}, ErrStoreLimit
	}
	if !safeFilter(query.Namespace) || !safeFilter(query.RootUID) || !safeFilter(query.PodUID) || len(query.Cursor) > 2048 || query.Limit < 0 || query.Limit > 100 {
		return CrashPage{}, ErrInvalidQuery
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	bindingBytes, _ := json.Marshal([]string{store.key, query.Namespace, query.RootUID, query.PodUID})
	digest := sha256.Sum256(bindingBytes)
	binding := hex.EncodeToString(digest[:])
	cursor := crashCursor{Binding: binding, Since: query.Since, Until: query.Until}
	if query.Cursor != "" {
		encoded, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil || json.Unmarshal(encoded, &cursor) != nil || cursor.Binding != binding || cursor.LastAt.IsZero() || cursor.LastID == "" || len(cursor.LastID) > 64 || len(cursor.High) != 1+len(store.archives) || !query.Since.IsZero() && !query.Since.Equal(cursor.Since) || !query.Until.IsZero() && !query.Until.Equal(cursor.Until) && !query.Until.Equal(cursor.RequestedUntil) {
			return CrashPage{}, ErrInvalidQuery
		}
	}
	if query.Cursor == "" && cursor.Until.After(now) {
		cursor.RequestedUntil, cursor.Until = cursor.Until, now
	}
	if cursor.Until.IsZero() {
		cursor.Until = now
	}
	if cursor.Since.IsZero() {
		cursor.Since = cursor.Until.Add(-store.retention)
	}
	if cursor.Since.After(cursor.Until) || cursor.Until.Sub(cursor.Since) > store.retention || cursor.Until.After(now) {
		return CrashPage{}, ErrInvalidQuery
	}
	page := CrashPage{Items: []CrashEvent{}, Gaps: []Gap{}, CaptureGaps: []Gap{}, Coverage: map[string]Gap{}, Since: cursor.Since, Until: cursor.Until, RetentionFrom: now.Add(-store.retention), Limitations: []string{"polling_observations_only", "restart_counts_are_lifetime_not_occurrences", "summary_caps_do_not_limit_journal", "ancestry_is_capture_time_uid_scoped", "unknown_finished_at_uses_observation_time", "duplicate_status_states_are_coalesced"}}
	page.RetentionBasis = "observed_at"
	page.Limitations = append(page.Limitations, "unknown_termination_identity_is_ambiguous")
	store.mu.Lock()
	defer store.mu.Unlock()
	seen := map[string]bool{}
	var events []CrashEvent
	for position, source := range append([]*Store{store}, store.archives...) {
		current, err := source.read()
		if err != nil {
			return CrashPage{}, err
		}
		if query.Cursor == "" {
			cursor.High = append(cursor.High, current.CrashSequence)
		}
		if page.StartedAt.IsZero() || !current.CrashStartedAt.IsZero() && current.CrashStartedAt.Before(page.StartedAt) {
			page.StartedAt = current.CrashStartedAt
		}
		page.Gaps = append(page.Gaps, gapsInWindow(current.CrashGaps, cursor.Since, cursor.Until)...)
		page.CaptureGaps = append(page.CaptureGaps, gapsInWindow(current.Gaps, cursor.Since, cursor.Until)...)
		if position == 0 {
			page.Coverage = current.Coverage
		}
		for _, event := range current.Crashes {
			if event.Sequence > cursor.High[position] || seen[event.ID] {
				continue
			}
			seen[event.ID] = true
			if event.ObservedAt.Before(page.RetentionFrom) || event.ObservedAt.Before(cursor.Since) || event.ObservedAt.After(cursor.Until) || query.Namespace != "" && event.Namespace != query.Namespace || query.RootUID != "" && event.RootUID != query.RootUID {
				continue
			}
			if query.PodUID != "" && event.PodUID != query.PodUID {
				continue
			}
			if event.Type == "termination" {
				page.CrashCount++
			}
			if query.Cursor != "" && (event.ObservedAt.After(cursor.LastAt) || event.ObservedAt.Equal(cursor.LastAt) && event.ID >= cursor.LastID) {
				continue
			}
			events = append(events, event)
		}
	}
	if page.Coverage == nil {
		page.Coverage = map[string]Gap{}
	}
	if page.StartedAt.IsZero() || page.StartedAt.After(cursor.Since) {
		page.Limitations = append(page.Limitations, "journal_not_started_for_full_window")
	}
	if cursor.Since.Before(page.RetentionFrom) {
		page.Limitations = append(page.Limitations, "outside_retention")
	}
	sort.Slice(events, func(left, right int) bool {
		if events[left].ObservedAt.Equal(events[right].ObservedAt) {
			return events[left].ID > events[right].ID
		}
		return events[left].ObservedAt.After(events[right].ObservedAt)
	})
	metadata, _ := json.Marshal(page)
	pageBytes := len(metadata) + 2048
	for position, event := range events {
		encoded, _ := json.Marshal(event)
		if len(page.Items) >= query.Limit || pageBytes+len(encoded) > maxCrashPageBytes {
			previous := events[position-1]
			cursor.LastAt, cursor.LastID = previous.ObservedAt, previous.ID
			encoded, _ := json.Marshal(cursor)
			page.Next = base64.RawURLEncoding.EncodeToString(encoded)
			break
		}
		page.Items = append(page.Items, event)
		pageBytes += len(encoded)
	}
	return page, nil
}
