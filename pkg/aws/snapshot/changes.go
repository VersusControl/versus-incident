package snapshot

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// ChangeGap is one stretch of a change window that was not read. Reason says
// why when the last poll failed; otherwise the stretch is simply not checked.
type ChangeGap struct {
	Account string          `json:"account"`
	Region  string          `json:"region"`
	From    time.Time       `json:"from"`
	To      time.Time       `json:"to"`
	Reason  model.GapReason `json:"reason"`
}

// ChangesResult is the Changes answer. Complete is true only when every
// account and region in scope has a checkpoint covering the whole window, so
// "no changes" may be claimed only when Complete is true.
type ChangesResult struct {
	AsOf     time.Time      `json:"as_of,omitzero"`
	Since    time.Time      `json:"since"`
	Until    time.Time      `json:"until"`
	Changes  []model.Change `json:"changes"`
	Gaps     []ChangeGap    `json:"gaps"`
	Complete bool           `json:"complete"`
}

// Checkpoint returns the write organization's change coverage for one
// account and region, with the poller's last attempt, success and error.
func (store *Store) Checkpoint(account, region string) (changes.Checkpoint, error) {
	if !store.ready() {
		return changes.Checkpoint{}, ErrNoStorage
	}
	checkpoint := changes.Checkpoint{Account: account, Region: region}
	ctx := context.Background()
	coverage, err := store.inventory.AWSChangeCoverage(ctx, store.writeScope(), account, region)
	if err != nil {
		return changes.Checkpoint{}, err
	}
	for _, pair := range coverage {
		for _, interval := range pair.Intervals {
			checkpoint.Covered = append(checkpoint.Covered, changes.Interval{From: interval.From, To: interval.To})
		}
	}
	units, err := store.inventory.AWSUnits(ctx, store.writeScope(), storage.AWSUnitQuery{Account: account, Region: region, Service: ChangeService})
	if err != nil {
		return changes.Checkpoint{}, err
	}
	if len(units) == 1 {
		checkpoint.LastAttemptAt, checkpoint.LastSuccessAt, checkpoint.ErrorCode = units[0].Sync.AttemptedAt, units[0].Sync.LastGoodAt, units[0].Sync.ErrorCode
	}
	return checkpoint, nil
}

// AppendChanges records one poll: the changes it found, the intervals it read
// to the end and the error code of a failed or partial poll. The changes and
// the coverage they prove commit in one transaction, so coverage is never
// claimed for changes that were not stored.
func (store *Store) AppendChanges(account, region string, found []model.Change, covered []changes.Interval, errorCode string) error {
	return store.appendChanges(account, region, found, covered, errorCode, nil)
}

func (store *Store) appendChanges(account, region string, found []model.Change, covered []changes.Interval, errorCode string, guard *uint64) error {
	unit := model.Unit{Account: account, Region: region, Service: ChangeService}
	if !ValidUnit(unit) {
		return ErrInvalidUnit
	}
	if !store.ready() {
		return ErrNoStorage
	}
	rows := make([]storage.AWSChange, 0, len(found))
	for _, change := range found {
		if change.ID == "" || change.Account != account || change.Time.IsZero() {
			continue
		}
		rows = append(rows, changeRow(region, change))
	}
	intervals := make([]storage.AWSInterval, 0, len(covered))
	for _, interval := range covered {
		intervals = append(intervals, storage.AWSInterval{From: interval.From, To: interval.To})
	}
	ctx := context.Background()
	if err := store.inventory.AppendAWSChanges(ctx, store.scope, account, region, rows, intervals, guard); err != nil {
		return storeError(err)
	}
	now := store.clock()
	err := store.updateSync(unit, guard, func(state *storage.AWSSyncState, _ bool) bool {
		state.AttemptedAt, state.ErrorCode = now, errorCode
		if errorCode == "" {
			state.LastGoodAt = now
		}
		return true
	})
	if err != nil {
		return err
	}
	if store.bounded {
		store.retain(ctx, unit)
	}
	return nil
}

// Changes lists changes in [since, until) for the query's account, region,
// service and ARN. AsOf, when set, caps until. Every account and region the
// store expects coverage for reports its uncovered stretches as gaps.
func (store *Store) Changes(ctx context.Context, query Query, since, until time.Time) (ChangesResult, error) {
	if !store.ready() {
		return ChangesResult{}, ErrNoStorage
	}
	if !query.AsOf.IsZero() && query.AsOf.Before(until) {
		until = query.AsOf
	}
	result := ChangesResult{AsOf: query.AsOf, Since: since, Until: until, Changes: []model.Change{}, Gaps: []ChangeGap{}}
	if !until.After(since) {
		result.Complete = true
		return result, nil
	}

	type pair struct {
		account  string
		region   string
		covered  []changes.Interval
		sync     storage.AWSSyncState
		coverage bool
	}
	pairs := make(map[string]*pair)
	coverage, err := store.inventory.AWSChangeCoverage(ctx, store.scope, query.Account, query.Region)
	if err != nil {
		return ChangesResult{}, err
	}
	for _, entry := range coverage {
		item := &pair{account: entry.Account, region: entry.Region, coverage: true}
		for _, interval := range entry.Intervals {
			item.covered = append(item.covered, changes.Interval{From: interval.From, To: interval.To})
		}
		pairs[pairKey(entry.Account, entry.Region)] = item
	}
	units, err := store.units(query.Account, query.Region, ChangeService, time.Time{})
	if err != nil {
		return ChangesResult{}, err
	}
	for _, unit := range units {
		key := pairKey(unit.Unit.Account, unit.Unit.Region)
		if pairs[key] == nil {
			pairs[key] = &pair{account: unit.Unit.Account, region: unit.Unit.Region}
		}
		pairs[key].sync = unit.Sync
	}

	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := pairs[key]
		reason := model.GapNotChecked
		if entry.sync.ErrorCode != "" {
			reason = model.GapReasonForCode(entry.sync.ErrorCode)
		}
		if !entry.coverage {
			result.Gaps = append(result.Gaps, ChangeGap{Account: entry.account, Region: entry.region, From: since, To: until, Reason: reason})
			continue
		}
		for _, gap := range (changes.Checkpoint{Covered: entry.covered}).Gaps(since, until) {
			result.Gaps = append(result.Gaps, ChangeGap{Account: entry.account, Region: entry.region, From: gap.From, To: gap.To, Reason: reason})
		}
	}
	rows, err := store.inventory.AWSChanges(ctx, store.scope, storage.AWSChangeQuery{Account: query.Account, Region: query.Region, Service: query.Service, Since: since, Until: until})
	if err != nil {
		return ChangesResult{}, err
	}
	for _, row := range rows {
		change, ok := changeOf(row)
		if !ok || change.Time.Before(since) || !change.Time.Before(until) {
			continue
		}
		if query.ARN != "" && !changeTouches(change, query.ARN) {
			continue
		}
		result.Changes = append(result.Changes, change)
	}
	sortChanges(result.Changes)
	result.Complete = len(result.Gaps) == 0 && len(pairs) > 0
	return result, nil
}

func sortChanges(list []model.Change) {
	sort.Slice(list, func(left, right int) bool {
		if !list[left].Time.Equal(list[right].Time) {
			return list[left].Time.Before(list[right].Time)
		}
		return list[left].ID < list[right].ID
	})
}

// changeTouches reports whether a change names the ARN or the short
// identifier at the end of it.
func changeTouches(change model.Change, arn string) bool {
	short := arn
	if index := strings.LastIndexAny(arn, "/:"); index >= 0 {
		short = arn[index+1:]
	}
	for _, resource := range change.Resources {
		if resource == arn || resource == short || strings.HasSuffix(resource, "/"+short) {
			return true
		}
	}
	return false
}
