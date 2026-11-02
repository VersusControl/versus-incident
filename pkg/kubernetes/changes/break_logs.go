package changes

import (
	"encoding/json"
	"errors"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"time"
)

const breakSettleAdmissionKey = "kubernetes/break-settle-admission.json"

type breakSettleSlot struct {
	Token string    `json:"token"`
	Until time.Time `json:"until"`
}

func (store *Store) commitBreakSettleSlots(update func([]breakSettleSlot) ([]breakSettleSlot, error)) error {
	if store == nil {
		return ErrStoreLimit
	}
	cas, ok := store.provider.(storage.BlobCAS)
	if !ok {
		return ErrStoreLimit
	}
	for attempt := 0; attempt < 16; attempt++ {
		raw, err := store.provider.ReadBlob(breakSettleAdmissionKey)
		if errors.Is(err, storage.ErrNotFound) {
			raw, err = nil, nil
		}
		if err != nil {
			return err
		}
		slots := []breakSettleSlot{}
		if len(raw) > 4096 || len(raw) > 0 && json.Unmarshal(raw, &slots) != nil || len(slots) > 2 {
			return ErrStoreLimit
		}
		next, err := update(slots)
		if err != nil {
			return err
		}
		replacement, err := json.Marshal(next)
		if err != nil {
			return err
		}
		swapped, err := cas.CompareAndSwapBlob(breakSettleAdmissionKey, raw, replacement)
		if err != nil {
			return err
		}
		if swapped {
			return nil
		}
	}
	return ErrStoreLimit
}

// Admission is deployment-wide: all clusters and the upgrade's default/licensed
// scopes share these two CAS slots, not a per-org or per-cluster budget.
func (store *Store) AcquireBreakSettleSlot(token string, now time.Time) error {
	if token == "" || len(token) > 256 {
		return ErrStoreLimit
	}
	return store.commitBreakSettleSlots(func(slots []breakSettleSlot) ([]breakSettleSlot, error) {
		kept := []breakSettleSlot{}
		for _, slot := range slots {
			if slot.Until.After(now) {
				if slot.Token == token {
					return nil, ErrStoreLimit
				}
				kept = append(kept, slot)
			}
		}
		if len(kept) == 2 {
			return nil, ErrStoreLimit
		}
		return append(kept, breakSettleSlot{token, now.Add(30 * time.Second)}), nil
	})
}

func (store *Store) ReleaseBreakSettleSlot(token string) error {
	return store.commitBreakSettleSlots(func(slots []breakSettleSlot) ([]breakSettleSlot, error) {
		kept := []breakSettleSlot{}
		for _, slot := range slots {
			if slot.Token != token {
				kept = append(kept, slot)
			}
		}
		return kept, nil
	})
}

func (store *Store) BreakLogs(now time.Time) (breaklogs.State, error) {
	if store == nil {
		return breaklogs.State{}, ErrStoreLimit
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current, err := store.read()
	if err != nil {
		return breaklogs.State{}, err
	}
	if afterRead := time.Now().UTC(); afterRead.After(now) {
		now = afterRead
	}
	state := current.BreakLogs
	for _, archive := range store.archives {
		old, readErr := archive.BreakLogs(now)
		if readErr != nil {
			return breaklogs.State{}, readErr
		}
		known := map[string]bool{}
		for _, item := range state.Snapshots {
			known[item.ID] = true
		}
		for _, item := range old.Snapshots {
			if !known[item.ID] {
				state.Snapshots = append(state.Snapshots, item)
			}
		}
		if err := breaklogs.MergeManifests(&state, old.Manifests, now); err != nil {
			return breaklogs.State{}, err
		}
		knownPods := map[string]bool{}
		for _, pod := range state.PodHistory {
			knownPods[pod.UID] = true
		}
		for _, pod := range old.PodHistory {
			if !knownPods[pod.UID] {
				state.PodHistory = append(state.PodHistory, pod)
			}
		}
		if old.PodHistoryTruncatedAt.After(state.PodHistoryTruncatedAt) {
			state.PodHistoryTruncatedAt = old.PodHistoryTruncatedAt
		}
		state.PodHistoryGaps = append(state.PodHistoryGaps, old.PodHistoryGaps...)
		if state.Jobs == nil {
			state.Jobs = map[string]time.Time{}
		}
		for id, at := range old.Jobs {
			if _, exists := state.Jobs[id]; !exists {
				state.Jobs[id] = at
			}
		}
		state.Gaps = append(state.Gaps, old.Gaps...)
	}
	if err := breaklogs.Merge(&state, nil, nil, "", now); err != nil {
		return breaklogs.State{}, err
	}
	return state, nil
}

func (store *Store) CommitBreakLogs(items []breaklogs.Snapshot, gaps []breaklogs.Gap, job, owner string, now time.Time, manifests ...breaklogs.Manifest) error {
	if store == nil {
		return ErrStoreLimit
	}
	if _, ok := store.provider.(storage.BlobCAS); !ok {
		return ErrStoreLimit
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
		if err := breaklogs.MergeManifests(&current.BreakLogs, manifests, now); err != nil {
			return err
		}
		return breaklogs.Merge(&current.BreakLogs, items, gaps, job, now)
	})
}

func validBreakLease(current *snapshot, owner string) bool {
	if owner == "" {
		return current.Lease == nil || !current.Lease.Expires.After(time.Now().UTC())
	}
	return current.Lease != nil && current.Lease.Owner == owner && current.Lease.Expires.After(time.Now().UTC())
}

func (store *Store) ObserveBreakPods(pods []breaklogs.ManifestPod, owner string, now time.Time) error {
	if store == nil {
		return ErrStoreLimit
	}
	view, err := store.BreakLogs(time.Now().UTC())
	if err != nil {
		return err
	}
	return store.commitSnapshot(func(current *snapshot) error {
		if !validBreakLease(current, owner) {
			return ErrLeaseLost
		}
		known := map[string]bool{}
		for _, pod := range current.BreakLogs.PodHistory {
			known[pod.UID] = true
		}
		for _, pod := range view.PodHistory {
			if !known[pod.UID] {
				current.BreakLogs.PodHistory = append(current.BreakLogs.PodHistory, pod)
			}
		}
		breaklogs.ObservePods(&current.BreakLogs, pods, now)
		return breaklogs.Merge(&current.BreakLogs, nil, nil, "", time.Now().UTC())
	})
}

// ClaimBreakSettle persists an attempt before any network request. Lost queries
// consume that attempt; failover cannot issue an unbounded replay.
func (store *Store) ClaimBreakSettle(id, owner string, now time.Time) (breaklogs.Manifest, error) {
	if store == nil || owner == "" {
		return breaklogs.Manifest{}, ErrLeaseLost
	}
	view, err := store.BreakLogs(now)
	if err != nil {
		return breaklogs.Manifest{}, err
	}
	var claimed breaklogs.Manifest
	err = store.commitSnapshot(func(current *snapshot) error {
		if !validBreakLease(current, owner) {
			return ErrLeaseLost
		}
		if err := breaklogs.MergeManifests(&current.BreakLogs, view.Manifests, now); err != nil {
			return err
		}
		running := 0
		for _, item := range current.BreakLogs.Manifests {
			if item.SettlingUntil.After(now) {
				running++
			}
		}
		if running >= 2 {
			return ErrStoreLimit
		}
		for i := range current.BreakLogs.Manifests {
			item := &current.BreakLogs.Manifests[i]
			if item.ID != id {
				continue
			}
			if item.Done || item.Attempts >= 2 || item.SettleAt.IsZero() || item.SettleAt.After(now) || item.SettlingUntil.After(now) {
				return ErrStoreLimit
			}
			item.Attempts++
			item.SettleEpoch = current.Lease.Epoch
			item.SettlingUntil = now.Add(30 * time.Second)
			item.SettleAt = item.BreakAt.Add(10 * time.Minute)
			if item.SettleAt.Before(item.SettlingUntil) {
				item.SettleAt = item.SettlingUntil
			}
			claimed = *item
			return nil
		}
		return ErrStoreLimit
	})
	return claimed, err
}

func (store *Store) CommitBreakSettle(claim breaklogs.Manifest, items []breaklogs.Snapshot, retry bool, owner string, now time.Time) error {
	if store == nil || owner == "" {
		return ErrLeaseLost
	}
	return store.commitSnapshot(func(current *snapshot) error {
		if !validBreakLease(current, owner) {
			return ErrLeaseLost
		}
		for i := range current.BreakLogs.Manifests {
			item := &current.BreakLogs.Manifests[i]
			if item.ID != claim.ID {
				continue
			}
			if item.Attempts != claim.Attempts || !item.SettlingUntil.Equal(claim.SettlingUntil) || !item.SettlingUntil.After(now) || !item.SettlingUntil.After(time.Now().UTC()) || !item.SettleEpoch.Equal(claim.SettleEpoch) || !claim.SettleEpoch.Equal(current.Lease.Epoch) {
				return ErrLeaseLost
			}
			item.SettlingUntil = time.Time{}
			item.Done = !retry || item.Attempts == 2
			return breaklogs.Merge(&current.BreakLogs, items, nil, "", now)
		}
		return ErrLeaseLost
	})
}

func (store *Store) CheckCaptureLease(owner string) error {
	if store == nil {
		return ErrStoreLimit
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current, err := store.read()
	if err != nil {
		return err
	}
	if owner == "" {
		if current.Lease != nil && current.Lease.Expires.After(time.Now().UTC()) {
			return ErrLeaseLost
		}
		return nil
	}
	if current.Lease == nil || current.Lease.Owner != owner || !current.Lease.Expires.After(time.Now().UTC()) {
		return ErrLeaseLost
	}
	return nil
}
