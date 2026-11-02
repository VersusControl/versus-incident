package breaklogs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

const MaxManifestPods = 20
const MaxHistoryPodsPerRoot = 2 * MaxManifestPods
const MaxHistoryBytesPerRoot = 64 << 10
const MaxMetadataBytes = 2 << 20

// Manifest contains identity evidence, never labels, environment or log text.
type Manifest struct {
	ID            string        `json:"id"`
	OccurrenceID  string        `json:"occurrence_id,omitempty"`
	Namespace     string        `json:"namespace"`
	RootUID       string        `json:"root_uid"`
	BreakAt       time.Time     `json:"break_at"`
	From          time.Time     `json:"from"`
	To            time.Time     `json:"to"`
	CapturedAt    time.Time     `json:"captured_at"`
	Pods          []ManifestPod `json:"pods"`
	Truncated     bool          `json:"truncated,omitempty"`
	SettleAt      time.Time     `json:"settle_at,omitempty,omitzero"`
	Attempts      int           `json:"attempts"`
	Done          bool          `json:"done,omitempty"`
	SettlingUntil time.Time     `json:"settling_until,omitempty,omitzero"`
	SettleEpoch   time.Time     `json:"settle_epoch,omitempty,omitzero"`
}

type ManifestPod struct {
	Namespace string    `json:"namespace"`
	RootUID   string    `json:"root_uid"`
	Name      string    `json:"name"`
	UID       string    `json:"uid"`
	Node      string    `json:"node,omitempty"`
	Broken    bool      `json:"broken,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// LastSeen bounds a vanished pod when its exact deletion time is unknown.
	// A later complete inventory proves absence, not survival until that LIST.
	LastSeen   time.Time           `json:"last_seen"`
	DeletedAt  time.Time           `json:"deleted_at,omitempty,omitzero"`
	Containers []ManifestContainer `json:"containers"`
}

type ManifestContainer struct {
	Name                string    `json:"name"`
	Init                bool      `json:"init,omitempty"`
	Restarts            int32     `json:"restarts"`
	StartedAt           time.Time `json:"started_at,omitempty,omitzero"`
	TerminationAt       time.Time `json:"termination_at,omitempty,omitzero"`
	TerminationReason   string    `json:"termination_reason,omitempty"`
	TerminationExitCode int32     `json:"termination_exit_code,omitempty"`
	PreviousStartedAt   time.Time `json:"previous_started_at,omitempty,omitzero"`
	CurrentTermination  bool      `json:"current_termination,omitempty"`
	CurrentEndedAt      time.Time `json:"current_ended_at,omitempty,omitzero"`
}

func ManifestIdentity(root string, at time.Time) string {
	raw, _ := json.Marshal([]any{root, at})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func MergeManifests(state *State, incoming []Manifest, now time.Time) error {
	byID := map[string]Manifest{}
	for _, item := range append(append([]Manifest(nil), state.Manifests...), incoming...) {
		if item.ID == "" || item.RootUID == "" || item.Namespace == "" || item.BreakAt.IsZero() || item.From.IsZero() || !item.To.After(item.From) || item.CapturedAt.IsZero() || item.CapturedAt.After(now.Add(ClockTolerance)) || item.Attempts < 0 || item.Attempts > 2 || len(item.Pods) > MaxManifestPods {
			return ErrInvalid
		}
		if item.CapturedAt.Before(now.Add(-Retention)) {
			continue
		}
		for _, pod := range item.Pods {
			if pod.UID == "" || pod.Name == "" || pod.CreatedAt.IsZero() || pod.LastSeen.IsZero() || pod.Namespace != item.Namespace || pod.RootUID != item.RootUID || len(pod.Containers) > 20 {
				return ErrInvalid
			}
		}
		// Capture retries cannot rewind pinned windows, progress or identity.

		if prior, exists := byID[item.ID]; !exists {
			byID[item.ID] = item
		} else if len(prior.Pods) == 0 && len(item.Pods) > 0 && prior.Namespace == item.Namespace && prior.RootUID == item.RootUID {
			// Late identity proof may fill an empty manifest, never replace pinned
			// membership or reset the window, retention age or attempt budget.
			for _, pod := range item.Pods {
				if pod.CreatedAt.After(prior.To) || !pod.DeletedAt.IsZero() && pod.DeletedAt.Before(prior.From) {
					continue
				}
				pod.Containers = append([]ManifestContainer(nil), pod.Containers...)
				prior.Pods = append(prior.Pods, pod)
			}
			if len(prior.Pods) > 0 && prior.Attempts < 2 {
				prior.Done = false
			}
			prior.Truncated = prior.Truncated || item.Truncated
			byID[item.ID] = prior
		}

	}
	state.Manifests = nil
	for _, item := range byID {
		state.Manifests = append(state.Manifests, item)
	}
	sort.Slice(state.Manifests, func(i, j int) bool {
		if state.Manifests[i].CapturedAt.Equal(state.Manifests[j].CapturedAt) {
			return state.Manifests[i].ID < state.Manifests[j].ID
		}
		return state.Manifests[i].CapturedAt.Before(state.Manifests[j].CapturedAt)
	})
	kept := []ManifestPod{}
	for _, pod := range state.PodHistory {
		if !pod.LastSeen.Before(now.Add(-Retention)) {
			kept = append(kept, pod)
		}
	}
	if state.PodHistoryTruncatedAt.Before(now.Add(-Retention)) {
		state.PodHistoryTruncatedAt = time.Time{}
	}
	state.PodHistory = kept
	return boundMetadata(state, now)
}

// Identity metadata has its own budget, reserved independently of log evidence.
// History reserves at most half, leaving room for pending manifests and scoped
// loss markers. Healthy sampling loses coverage too: its window ends at the
// next complete inventory, which bounds an unseen disappearance without
// asserting a Pod's lifetime. Retired losses use only recorded lifetime.
func boundMetadata(state *State, now time.Time) error {
	compactGaps(state, now)
	if len(state.Gaps) > MaxSnapshots {
		return ErrInvalid
	}
	history := append([]ManifestPod(nil), state.PodHistory...)
	sort.Slice(history, func(i, j int) bool {
		left, right := history[i], history[j]
		if PodPriority(left) != PodPriority(right) {
			return PodPriority(left) < PodPriority(right)
		}
		if !left.LastSeen.Equal(right.LastSeen) {
			return left.LastSeen.After(right.LastSeen)
		}
		return left.UID < right.UID
	})
	kept := []ManifestPod{}
	counts := map[string]int{}
	rootBytes := map[string]int{}
	bytes := 2
	losses := map[string]Gap{}
	addLoss := func(gap Gap) {
		key := gap.Namespace + "\x00" + gap.RootUID + "\x00" + gap.Reason
		if old, exists := losses[key]; exists {
			gap.From = minTimestamp(gap.From, old.From)
			gap.To = maxTimestamp(gap.To, old.To)
		}
		losses[key] = gap
	}
	for _, gap := range state.PodHistoryGaps {
		if !gap.To.Before(now.Add(-Retention)) {
			gap.From = maxTimestamp(gap.From, now.Add(-Retention))
			addLoss(gap)
		}
	}
	for _, pod := range history {
		key := pod.Namespace + "\x00" + pod.RootUID
		raw, _ := json.Marshal(pod)
		if counts[key] < MaxHistoryPodsPerRoot && rootBytes[key]+len(raw)+1 <= MaxHistoryBytesPerRoot && bytes+len(raw)+1 <= MaxMetadataBytes/2 {
			counts[key]++
			rootBytes[key] += len(raw) + 1
			bytes += len(raw) + 1
			kept = append(kept, pod)
			continue
		}
		gap := Gap{Namespace: pod.Namespace, RootUID: pod.RootUID, From: maxTimestamp(pod.CreatedAt, now.Add(-Retention)), To: pod.LastSeen, Reason: "sampled"}
		if !pod.DeletedAt.IsZero() {
			gap.From, gap.To, gap.Reason = maxTimestamp(pod.CreatedAt, now.Add(-Retention)), pod.DeletedAt, "budget"
		}
		addLoss(gap)
	}
	state.PodHistory = kept
	state.PodHistoryGaps = nil
	for _, gap := range losses {
		state.PodHistoryGaps = append(state.PodHistoryGaps, gap)
	}
	sort.Slice(state.PodHistoryGaps, func(i, j int) bool {
		left, right := state.PodHistoryGaps[i], state.PodHistoryGaps[j]
		if !left.To.Equal(right.To) {
			return left.To.Before(right.To)
		}
		return left.Namespace+"\x00"+left.RootUID+"\x00"+left.Reason < right.Namespace+"\x00"+right.RootUID+"\x00"+right.Reason
	})
	// If exact root markers alone exhaust their reservation, coarsen only the
	// affected namespaces. Never silently discard a loss or poison other namespaces.
	raw, _ := json.Marshal(state.PodHistoryGaps)
	if len(raw) > MaxMetadataBytes/4 {
		scoped := map[string]Gap{}
		for _, gap := range state.PodHistoryGaps {
			key := gap.Namespace + "\x00" + gap.Reason
			gap.RootUID, gap.PodUID = "", ""
			if prior, ok := scoped[key]; ok {
				gap.From = minTimestamp(gap.From, prior.From)
				gap.To = maxTimestamp(gap.To, prior.To)
			}
			scoped[key] = gap
		}
		state.PodHistoryGaps = nil
		for _, gap := range scoped {
			state.PodHistoryGaps = append(state.PodHistoryGaps, gap)
		}
		sort.Slice(state.PodHistoryGaps, func(i, j int) bool {
			a, b := state.PodHistoryGaps[i], state.PodHistoryGaps[j]
			return a.Namespace+"\x00"+a.Reason < b.Namespace+"\x00"+b.Reason
		})
	}
	// Drop completed oldest manifests before pending ones. A lost pending
	// occurrence receives an exact scoped marker in ordinary coverage.
	sort.SliceStable(state.Manifests, func(i, j int) bool {
		a, b := state.Manifests[i], state.Manifests[j]
		if a.Done != b.Done {
			return a.Done
		}
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.Before(b.CapturedAt)
		}
		return a.ID < b.ID
	})
	metadataBytes := func() int {
		copy := *state
		copy.Snapshots = nil
		raw, _ := json.Marshal(copy)
		return len(raw)
	}
	// Serialize the envelope once, then account for removed manifest bytes and
	// appended marker bytes. Re-encoding all remaining Pods for every eviction
	// is quadratic under a manifest flood. Comma/key estimates are conservative;
	// the final exact check still enforces the durable cap.
	total := metadataBytes()
	for len(state.Manifests) > MaxSnapshots || total > MaxMetadataBytes {
		if len(state.Manifests) == 0 {
			// An unrepresentable set of scopes is rejected, not accepted with
			// mysterious losses or an over-budget durable payload.
			return ErrInvalid
		}
		item := state.Manifests[0]
		state.Manifests = state.Manifests[1:]
		raw, _ := json.Marshal(item)
		total -= len(raw) + 1
		gap := Gap{SnapshotID: item.ID, Namespace: item.Namespace, RootUID: item.RootUID, From: item.From, To: item.To, ObservedAt: now, Reason: "budget"}
		state.Gaps = append(state.Gaps, gap)
		raw, _ = json.Marshal(gap)
		total += len(raw) + 1
		if len(state.Gaps) > MaxSnapshots {
			compactGaps(state, now)
			total = metadataBytes()
		}
		if len(state.Gaps) > MaxSnapshots {
			return ErrInvalid
		}
	}
	if metadataBytes() > MaxMetadataBytes {
		return ErrInvalid
	}
	return nil
}

// PodPriority orders irreplaceable retired identity, broken live identity, then
// resampleable healthy live identity, both globally and within each workload.
func PodPriority(pod ManifestPod) int {
	if !pod.DeletedAt.IsZero() {
		return 0
	}
	if pod.Broken {
		return 1
	}
	for _, container := range pod.Containers {
		if container.Restarts > 0 || !container.TerminationAt.IsZero() || container.CurrentTermination {
			return 1
		}
	}
	return 2
}

func minTimestamp(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTimestamp(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

// ObservePods retains only verified owner-chain metadata from complete inventory.
// Unknown lifetimes remain unknown; a name or workload prefix is never proof.
func ObservePods(state *State, pods []ManifestPod, now time.Time) {
	if !state.PodHistoryAt.IsZero() && !now.After(state.PodHistoryAt) {
		return
	}
	// An evicted healthy identity cannot prove absence on this LIST. Preserve
	// coverage through the interval between committed observations, then close
	// it. This changes no UID's LastSeen or DeletedAt.
	for i := range state.PodHistoryGaps {
		gap := &state.PodHistoryGaps[i]
		if gap.Reason == "sampled" && !gap.To.Before(now.Add(-Retention)) {
			gap.To = maxTimestamp(gap.To, now)
			gap.Reason = "budget"
		}
	}
	state.PodHistoryAt = now
	byUID := map[string]ManifestPod{}
	for _, pod := range state.PodHistory {
		byUID[pod.UID] = pod
	}
	for _, pod := range pods {
		if old, exists := byUID[pod.UID]; exists {
			for i := range pod.Containers {
				next := &pod.Containers[i]
				for _, prior := range old.Containers {
					if prior.Name != next.Name || prior.Init != next.Init {
						continue
					}
					if !next.PreviousStartedAt.IsZero() {
						continue
					}
					if prior.Restarts == next.Restarts {
						next.PreviousStartedAt = prior.PreviousStartedAt
					} else if prior.Restarts+1 == next.Restarts && !prior.StartedAt.IsZero() && !next.TerminationAt.Before(prior.StartedAt) {
						next.PreviousStartedAt = prior.StartedAt
					}
				}
			}
		}
		pod.LastSeen = now
		pod.DeletedAt = time.Time{}
		byUID[pod.UID] = pod
	}
	state.PodHistory = nil
	for _, pod := range byUID {
		// An absent pod may only be linked up to the last verified sighting.
		if pod.LastSeen.Before(now) && pod.DeletedAt.IsZero() {
			pod.DeletedAt = pod.LastSeen
		}
		state.PodHistory = append(state.PodHistory, pod)
	}
	sort.Slice(state.PodHistory, func(i, j int) bool {
		if state.PodHistory[i].LastSeen.Equal(state.PodHistory[j].LastSeen) {
			return state.PodHistory[i].UID < state.PodHistory[j].UID
		}
		return state.PodHistory[i].LastSeen.Before(state.PodHistory[j].LastSeen)
	})
}

// TerminationEnd includes the complete second reported by Kubernetes status,
// whose whole-second timestamps bound nanosecond kubelet log timestamps.
func TerminationEnd(at time.Time) time.Time {
	if !at.IsZero() && at.Nanosecond() == 0 {
		return at.Add(time.Second - time.Nanosecond)
	}
	return at
}

// ResolveLinkedLine refuses overlapping same-name UIDs and unobserved restarts.
func ResolveLinkedLine(pods []ManifestPod, line LinkedLine) (ManifestPod, ManifestContainer, bool) {
	var selected *ManifestPod
	for i := range pods {
		pod := &pods[i]
		if pod.Name != line.Pod || pod.CreatedAt.IsZero() || line.At.Before(pod.CreatedAt) || !pod.DeletedAt.IsZero() && line.At.After(pod.DeletedAt) {
			continue
		}
		if selected != nil && selected.UID != pod.UID {
			return ManifestPod{}, ManifestContainer{}, false
		}
		selected = pod
	}
	if selected == nil {
		return ManifestPod{}, ManifestContainer{}, false
	}
	for _, container := range selected.Containers {
		if container.Name != line.Container {
			continue
		}
		if !container.TerminationAt.IsZero() && !line.At.After(TerminationEnd(container.TerminationAt)) {
			start := container.PreviousStartedAt
			if container.CurrentTermination {
				start = container.StartedAt
			}
			if start.IsZero() && container.Restarts <= 1 {
				start = selected.CreatedAt
			}
			if start.IsZero() || line.At.Before(start) {
				return ManifestPod{}, ManifestContainer{}, false
			}
		} else {
			start := container.StartedAt
			if start.IsZero() && container.Restarts == 0 {
				start = selected.CreatedAt
			}
			if container.CurrentTermination || start.IsZero() || line.At.Before(start) || !container.CurrentEndedAt.IsZero() && line.At.After(container.CurrentEndedAt) {
				return ManifestPod{}, ManifestContainer{}, false
			}
		}
		return *selected, container, true
	}
	return ManifestPod{}, ManifestContainer{}, false
}
