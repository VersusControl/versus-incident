package changes

import (
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"time"
)

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
	if err := breaklogs.Merge(&state, nil, nil, "", now); err != nil {
		return breaklogs.State{}, err
	}
	return state, nil
}

func (store *Store) CommitBreakLogs(items []breaklogs.Snapshot, gaps []breaklogs.Gap, job, owner string, now time.Time) error {
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
		return breaklogs.Merge(&current.BreakLogs, items, gaps, job, now)
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
