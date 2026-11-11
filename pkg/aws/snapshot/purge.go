package snapshot

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Writer writes one account's sync. It is bound to the purge epoch the
// account had in storage when it was opened: once the account or one of its
// regions is purged, every later write returns ErrPurged, so an in-flight
// sync cannot bring purged data back. The epoch lives in the database, so
// the fence holds across stores and processes.
type Writer struct {
	store   *Store
	account string
	epoch   uint64
	err     error
}

// Writer opens a guarded writer for account.
func (store *Store) Writer(account string) *Writer {
	writer := &Writer{store: store, account: account}
	if !store.ready() {
		writer.err = ErrNoStorage
		return writer
	}
	writer.epoch, writer.err = store.inventory.AWSAccountEpoch(context.Background(), store.scope, account)
	return writer
}

// Stale reports whether the account was purged after the writer was opened.
func (writer *Writer) Stale() bool {
	if writer.err != nil {
		return false
	}
	epoch, err := writer.store.inventory.AWSAccountEpoch(context.Background(), writer.store.scope, writer.account)
	return err == nil && epoch != writer.epoch
}

func (writer *Writer) check(account string) error {
	if account != writer.account {
		return ErrInvalidUnit
	}
	return writer.err
}

// Commit is Store.Commit, refused after a purge.
func (writer *Writer) Commit(unit model.Unit, candidate Candidate) (Generation, error) {
	if err := writer.check(unit.Account); err != nil {
		return Generation{}, err
	}
	return writer.store.commit(unit, candidate, &writer.epoch)
}

// RecordFailure is Store.RecordFailure, refused after a purge.
func (writer *Writer) RecordFailure(unit model.Unit, code string, gaps ...model.Gap) error {
	if err := writer.check(unit.Account); err != nil {
		return err
	}
	return writer.store.recordFailure(unit, code, gaps, &writer.epoch)
}

// RecordSuccess is Store.RecordSuccess, refused after a purge.
func (writer *Writer) RecordSuccess(unit model.Unit, detailGaps []model.Gap) error {
	if err := writer.check(unit.Account); err != nil {
		return err
	}
	return writer.store.recordSuccess(unit, detailGaps, &writer.epoch)
}

// RecordPending is Store.RecordPending, refused after a purge.
func (writer *Writer) RecordPending(units []model.Unit) error {
	for _, unit := range units {
		if err := writer.check(unit.Account); err != nil {
			return err
		}
	}
	return writer.store.recordPending(units, &writer.epoch)
}

// AppendChanges is Store.AppendChanges, refused after a purge.
func (writer *Writer) AppendChanges(region string, found []model.Change, covered []changes.Interval, errorCode string) error {
	if err := writer.check(writer.account); err != nil {
		return err
	}
	return writer.store.appendChanges(writer.account, region, found, covered, errorCode, &writer.epoch)
}

// pairs lists the account and region pairs the write organization holds.
func (store *Store) pairs() ([]storage.AWSPair, error) {
	if !store.ready() {
		return nil, ErrNoStorage
	}
	return store.inventory.AWSPairs(context.Background(), store.scope)
}

// Accounts lists the accounts the write organization holds data for.
func (store *Store) Accounts() ([]string, error) {
	pairs, err := store.pairs()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, pair := range pairs {
		seen[pair.Account] = true
	}
	return sortedKeys(seen), nil
}

// Regions lists the regions (not the global region) the write organization
// holds data for in one account.
func (store *Store) Regions(account string) ([]string, error) {
	pairs, err := store.pairs()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, pair := range pairs {
		if pair.Account == account && pair.Region != model.GlobalRegion {
			seen[pair.Region] = true
		}
	}
	return sortedKeys(seen), nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// ChangeRetention is how long changes are kept, or zero when retention is
// turned off.
func (store *Store) ChangeRetention() time.Duration {
	if store == nil || !store.bounded {
		return 0
	}
	return store.effectiveOptions().ChangeRetention
}

// PurgeAccount deletes every row the write organization holds for an
// account and advances its epoch in the same transaction. Writers opened
// before the purge can no longer write.
func (store *Store) PurgeAccount(account string) error {
	return store.purge(account, "")
}

// PurgeRegion deletes one region of an account the same way. The global
// region cannot be purged on its own; it goes with the account.
func (store *Store) PurgeRegion(account, region string) error {
	if region == "" || region == model.GlobalRegion {
		return ErrInvalidUnit
	}
	return store.purge(account, region)
}

func (store *Store) purge(account, region string) error {
	if !accountPattern.MatchString(account) || (region != "" && !regionPattern.MatchString(region)) {
		return ErrInvalidUnit
	}
	if !store.ready() {
		return ErrNoStorage
	}
	err := store.inventory.PurgeAWSAccount(context.Background(), store.scope, account, region)
	store.mu.Lock()
	store.views = make(map[string]cachedView)
	store.viewOrder = nil
	store.mu.Unlock()
	if errors.Is(err, storage.ErrInvalidAWSInventory) {
		return ErrInvalidUnit
	}
	return err
}
