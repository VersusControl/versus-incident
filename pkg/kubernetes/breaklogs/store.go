package breaklogs

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const MaxSnapshots = 2000
const MaxBytes = 8 << 20
const Retention = 24 * time.Hour

var ErrInvalid = errors.New("invalid break logs request")

type Line struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

type Snapshot struct {
	BreakAt       time.Time `json:"break_at,omitempty"`
	ID            string    `json:"id"`
	Cluster       string    `json:"cluster"`
	Namespace     string    `json:"namespace"`
	RootUID       string    `json:"root_uid"`
	Pod           string    `json:"pod"`
	PodUID        string    `json:"pod_uid"`
	PodCreatedAt  time.Time `json:"pod_created_at,omitempty"`
	Container     string    `json:"container"`
	Init          bool      `json:"init"`
	Slot          string    `json:"slot"`
	TerminationAt time.Time `json:"termination_at,omitempty"`
	Restarts      int32     `json:"restarts"`
	CapturedAt    time.Time `json:"captured_at"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	Lines         []Line    `json:"lines"`
	Truncated     bool      `json:"truncated"`
}

type Gap struct {
	SnapshotID string    `json:"snapshot_id,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Namespace  string    `json:"namespace,omitempty"`
	RootUID    string    `json:"root_uid,omitempty"`
	PodUID     string    `json:"pod_uid,omitempty"`
	Container  string    `json:"container,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

type State struct {
	Snapshots []Snapshot           `json:"snapshots"`
	Gaps      []Gap                `json:"gaps"`
	Jobs      map[string]time.Time `json:"jobs,omitempty"`
}

func Identity(snapshot Snapshot) string {
	encoded, _ := json.Marshal([]any{snapshot.Cluster, snapshot.PodUID, snapshot.Container, snapshot.Init, snapshot.Slot, snapshot.TerminationAt})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func Merge(current *State, incoming []Snapshot, gaps []Gap, job string, now time.Time) error {
	current.Snapshots = append([]Snapshot(nil), current.Snapshots...)
	positions := map[string]int{}
	for position, item := range current.Snapshots {
		positions[item.ID] = position
	}
	for _, item := range incoming {
		position, exists := positions[item.ID]
		if !exists || item.Slot != "current" || item.ID != Identity(item) || item.CapturedAt.After(now) || !item.CapturedAt.After(current.Snapshots[position].CapturedAt) {
			continue
		}
		old := current.Snapshots[position]
		if old.CapturedAt.Before(now.Add(-Retention)) {
			continue
		}
		merged, gap, err := mergeCurrent(old, item)
		if err != nil {
			return err
		}
		current.Snapshots[position] = merged
		if gap != nil {
			gaps = append(gaps, *gap)
		}
	}
	kept := make([]Snapshot, 0, len(current.Snapshots)+len(incoming))
	seen := map[string]bool{}
	for _, item := range append(append([]Snapshot(nil), current.Snapshots...), incoming...) {
		if item.ID != Identity(item) || item.CapturedAt.IsZero() || item.CapturedAt.After(now) || item.From.IsZero() || item.To.Before(item.From) || len(item.Lines) > 500 || item.Slot != "previous" && item.Slot != "current" || item.RootUID == "" || item.PodUID == "" || item.Container == "" {
			return ErrInvalid
		}
		textBytes := 0
		for _, line := range item.Lines {
			textBytes += len(line.Text)
			if line.At.IsZero() || line.At.Before(item.From) || line.At.After(item.To) || len(line.Text) > 4096 || textBytes > 64<<10 {
				return ErrInvalid
			}
		}
		if seen[item.ID] || item.CapturedAt.Before(now.Add(-Retention)) {
			continue
		}
		seen[item.ID] = true
		kept = append(kept, item)
	}
	sort.Slice(kept, func(left, right int) bool { return kept[left].CapturedAt.Before(kept[right].CapturedAt) })
	current.Snapshots = kept
	current.Gaps = ResolveBudgetGaps(current.Gaps, incoming)
	current.Gaps = append(current.Gaps, gaps...)
	if current.Jobs == nil {
		current.Jobs = map[string]time.Time{}
	}
	if job != "" {
		current.Jobs[job] = now
	}
	for key, at := range current.Jobs {
		if at.Before(now.Add(-Retention)) {
			delete(current.Jobs, key)
		}
	}
	for len(current.Jobs) > MaxSnapshots {
		oldest := ""
		for key, at := range current.Jobs {
			if oldest == "" || at.Before(current.Jobs[oldest]) {
				oldest = key
			}
		}
		delete(current.Jobs, oldest)
	}
	compactGaps(current, now)
	encoded, _ := json.Marshal(current)
	for len(current.Snapshots) > MaxSnapshots || len(encoded) > MaxBytes {
		if len(current.Snapshots) == 0 {
			return ErrInvalid
		}
		item := current.Snapshots[0]
		current.Snapshots = current.Snapshots[1:]
		current.Gaps = append(current.Gaps, Gap{From: item.From, To: item.To, Namespace: item.Namespace, RootUID: item.RootUID, PodUID: item.PodUID, Container: item.Container, Reason: "rotated"})
		compactGaps(current, now)
		encoded, _ = json.Marshal(current)
	}
	return nil
}

func ResolveBudgetGaps(gaps []Gap, snapshots []Snapshot) []Gap {
	for _, item := range snapshots {
		if item.Truncated {
			continue
		}
		remaining := []Gap{}
		for _, gap := range gaps {
			if gap.Reason != "budget" || gap.SnapshotID == "" || gap.SnapshotID != item.ID || gap.Namespace != item.Namespace || gap.RootUID != item.RootUID || gap.PodUID != item.PodUID || gap.Container != item.Container || item.CapturedAt.Before(gap.ObservedAt) || !item.To.After(gap.From) || !gap.To.After(item.From) {
				remaining = append(remaining, gap)
				continue
			}
			if gap.From.Before(item.From) {
				before := gap
				before.To = item.From
				remaining = append(remaining, before)
			}
			if gap.To.After(item.To) {
				gap.From = item.To
				remaining = append(remaining, gap)
			}
		}
		gaps = remaining
	}
	return gaps
}

func mergeCurrent(old, next Snapshot) (Snapshot, *Gap, error) {
	if next.From.IsZero() || next.To.Before(next.From) || len(next.Lines) > 500 {
		return Snapshot{}, nil, ErrInvalid
	}
	merged := old
	merged.Lines = append([]Line(nil), old.Lines...)
	seen := map[Line]bool{}
	used := 0
	for _, line := range old.Lines {
		seen[line] = true
		used += len(line.Text)
	}
	var gap *Gap
	if next.From.After(old.To) {
		gap = &Gap{From: old.To, To: next.From, Namespace: old.Namespace, RootUID: old.RootUID, PodUID: old.PodUID, Container: old.Container, Reason: "capture_late"}
	}
	for _, line := range next.Lines {
		if line.At.IsZero() || line.At.Before(next.From) || line.At.After(next.To) || len(line.Text) > 4096 {
			return Snapshot{}, nil, ErrInvalid
		}
		if seen[line] {
			continue
		}
		if len(merged.Lines) == 500 || used+len(line.Text) > 64<<10 {
			merged.Truncated = true
			gap = &Gap{From: next.From, To: next.To, Namespace: old.Namespace, RootUID: old.RootUID, PodUID: old.PodUID, Container: old.Container, Reason: "rotated"}
			break
		}
		seen[line] = true
		used += len(line.Text)
		merged.Lines = append(merged.Lines, line)
	}
	sort.SliceStable(merged.Lines, func(left, right int) bool { return merged.Lines[left].At.Before(merged.Lines[right].At) })
	if next.From.Before(merged.From) {
		merged.From = next.From
	}
	if next.To.After(merged.To) {
		merged.To = next.To
	}
	merged.Truncated = merged.Truncated || next.Truncated
	return merged, gap, nil
}

func compactGaps(state *State, now time.Time) {
	kept := []Gap{}
	for _, gap := range state.Gaps {
		if gap.From.IsZero() || gap.To.Before(gap.From) || gap.To.Before(now.Add(-Retention)) {
			continue
		}
		if len(kept) > 0 && kept[len(kept)-1] == gap {
			continue
		}
		kept = append(kept, gap)
	}
	if len(kept) > 2000 {
		kept = kept[len(kept)-2000:]
	}
	state.Gaps = kept
}

type Query struct {
	Namespace, RootUID, Mode, Level, Search, Cursor string
	// Container selects one container by name. Empty asks for the break's main
	// container and AllContainers merges every container.
	Container string
	Pods      []string
	From, To  time.Time
	Limit     int
}

// AllContainers is the Container value that merges every container's lines.
const AllContainers = "*"

func (query Query) Validate() error {
	if query.Namespace == "" || query.RootUID == "" || len(query.Namespace) > 253 || len(query.RootUID) > 256 || strings.ContainsAny(query.Namespace+query.RootUID, "\x00\r\n\t /\\") || query.From.IsZero() || !query.To.After(query.From) || query.To.Sub(query.From) > 2*time.Hour || query.Limit < 0 || query.Limit > 500 || len(query.Search) > 256 || len(query.Cursor) > 2048 || len(query.Pods) > 100 || len(query.Container) > 63 || strings.ContainsAny(query.Container, "\x00\r\n\t /\\") {
		return ErrInvalid
	}
	if query.Mode != "" && query.Mode != "merged" && query.Mode != "previous" && query.Mode != "current" {
		return ErrInvalid
	}
	if query.Level != "" && query.Level != "error" && query.Level != "warning" && query.Level != "info" && query.Level != "debug" {
		return ErrInvalid
	}
	return nil
}

type cursor struct {
	Binding  string    `json:"binding"`
	Offset   int       `json:"offset"`
	Observed time.Time `json:"observed"`
	Digest   string    `json:"digest"`
}

func CursorOffset(scope string, query Query, digest string, now time.Time) (int, time.Time, error) {
	if query.Cursor == "" {
		return 0, now, nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(query.Cursor)
	var value cursor
	if err != nil || json.Unmarshal(encoded, &value) != nil || value.Binding != binding(scope, query) || value.Offset < 0 || value.Offset > 1000000 || value.Observed.After(now) || value.Observed.Before(now.Add(-Retention)) || value.Digest != digest {
		return 0, time.Time{}, ErrInvalid
	}
	return value.Offset, value.Observed, nil
}

func NextCursor(scope string, query Query, digest string, offset int, observed time.Time) string {
	encoded, _ := json.Marshal(cursor{binding(scope, query), offset, observed, digest})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func binding(scope string, query Query) string {
	query.Cursor = ""
	encoded, _ := json.Marshal([]any{scope, query})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
