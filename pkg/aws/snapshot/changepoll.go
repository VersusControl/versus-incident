package snapshot

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// ChangeGapReason says why a stretch of the change list was not read. The
// list is closed; the page names each one in plain words.
type ChangeGapReason string

const (
	GapCloudTrailDenied    ChangeGapReason = "cloudtrail_denied"
	GapChangesThrottled    ChangeGapReason = "throttled"
	GapTooBusy             ChangeGapReason = storage.AWSChangeGapTooBusy
	GapCollectorDown       ChangeGapReason = "collector_down"
	GapChangesNotChecked   ChangeGapReason = "not_checked"
	GapBeforeConnect       ChangeGapReason = "before_connect"
	GapBeyondRetention     ChangeGapReason = "beyond_retention"
	GapRegionNotEnabled    ChangeGapReason = "region_not_enabled"
	GapCredentialsInvalid  ChangeGapReason = "credentials_invalid"
	GapAccountDisconnected ChangeGapReason = "account_disconnected"
	GapAssumeRoleFailed    ChangeGapReason = "assume_role_failed"
	GapAccountMismatch     ChangeGapReason = "account_mismatch"
	GapArriving            ChangeGapReason = "arriving"
	GapGlobalUnknown       ChangeGapReason = "global_unknown"
)

// ChangeGapReasonForCode maps a poll's closed error code onto the reason
// its unread stretch keeps.
func ChangeGapReasonForCode(code string) ChangeGapReason {
	switch code {
	case "":
		return GapChangesNotChecked
	case "denied":
		return GapCloudTrailDenied
	case "throttled":
		return GapChangesThrottled
	// An opt-in refusal from a poll is a real gap until Settings records
	// the region as not enabled; a host that does not resolve is a network
	// fault, never proof that a region is off.
	case "region_disabled":
		return GapRegionNotEnabled
	case "no_server_identity", "credentials_invalid":
		return GapCredentialsInvalid
	case "account_mismatch":
		return GapAccountMismatch
	case "account_disconnected":
		return GapAccountDisconnected
	case "role_not_found", "trust_denied", "external_id_mismatch", "session_duration_invalid", "assume_role_failed":
		return GapAssumeRoleFailed
	}
	return GapCollectorDown
}

// ChangeGapLabel is the reason one stretch of one poll was not read.
type ChangeGapLabel struct {
	From   time.Time
	To     time.Time
	Reason ChangeGapReason
	// Detail is the missing permission of a denied read, or the event rate
	// of a too_busy one.
	Detail string
}

// ChangePoll is one CloudTrail poll of one account and region.
type ChangePoll struct {
	// Found are the changes to store. Routine events are not among them;
	// they arrive as Counts.
	Found  []model.Change
	Counts []storage.AWSChangeCount
	// Covered are the intervals read to the end and settled.
	Covered []changes.Interval
	// Read are the stretches read without error, including the unsettled
	// tail; they clear the labels of earlier failed reads there.
	Read []changes.Interval
	// Gaps label the stretches this poll tried and could not read.
	Gaps []ChangeGapLabel
	// HistoryFrom is where the poll's oldest window started.
	HistoryFrom time.Time
	ErrorCode   string
	// Dropped counts malformed events the poller skipped.
	Dropped int64
	// NotChanges counts events CloudTrail flagged as writes that change
	// nothing (sign-ins, tokens, reads), in the stretch the poll settled.
	NotChanges  int64
	ClockSkew   time.Duration
	FailureKeep int
}

// RecordChangePoll is Store.RecordChangePoll, refused after a purge.
func (writer *Writer) RecordChangePoll(region string, poll ChangePoll) error {
	if err := writer.check(writer.account); err != nil {
		return err
	}
	return writer.store.recordChangePoll(writer.account, region, poll, &writer.epoch)
}

// RecordChangePoll stores one poll: its changes, counts, coverage, gap
// labels and pair state commit in one transaction, then the unit's sync
// state records the outcome. A malformed change is dropped and counted;
// it never stops the rest or the coverage they prove.
func (store *Store) RecordChangePoll(account, region string, poll ChangePoll) error {
	return store.recordChangePoll(account, region, poll, nil)
}

func (store *Store) recordChangePoll(account, region string, poll ChangePoll, guard *uint64) error {
	unit := model.Unit{Account: account, Region: region, Service: ChangeService}
	if !ValidUnit(unit) {
		return ErrInvalidUnit
	}
	if !store.ready() {
		return ErrNoStorage
	}
	feeds, ok := store.inventory.(storage.AWSChangeFeedStore)
	if !ok {
		return ErrNoStorage
	}
	now := store.clock()
	record := storage.AWSChangePoll{Account: account, Region: region, At: now, HistoryFrom: poll.HistoryFrom, Counts: poll.Counts,
		Dropped: poll.Dropped, NotChanges: poll.NotChanges, ClockSkew: poll.ClockSkew, FailureKeep: poll.FailureKeep}
	for _, change := range poll.Found {
		if change.ID == "" || change.Account != account || change.Time.IsZero() {
			record.Dropped++
			continue
		}
		record.Changes = append(record.Changes, changeRow(region, change))
	}
	for _, interval := range poll.Covered {
		record.Covered = append(record.Covered, storage.AWSInterval{From: interval.From, To: interval.To})
	}
	for _, interval := range poll.Read {
		record.Read = append(record.Read, storage.AWSInterval{From: interval.From, To: interval.To})
	}
	for _, gap := range poll.Gaps {
		record.Gaps = append(record.Gaps, storage.AWSChangeGapReason{From: gap.From, To: gap.To, Reason: string(gap.Reason), Detail: gap.Detail})
	}
	ctx := context.Background()
	if _, err := feeds.RecordAWSChangePoll(ctx, store.scope, record, guard); err != nil {
		return storeError(err)
	}
	err := store.updateSync(unit, guard, func(state *storage.AWSSyncState, _ bool) bool {
		state.AttemptedAt, state.ErrorCode = now, poll.ErrorCode
		if poll.ErrorCode == "" {
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

// changeLockKey is the session lock key of one pair's poller. The global
// stream reads CloudTrail in us-east-1, so it shares the us-east-1 pair's
// key: two replicas never read one account's us-east-1 CloudTrail at once.
func (store *Store) changeLockKey(account, region string) int64 {
	if region == model.GlobalRegion {
		region = globalStreamHome
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("aws-changes\x00" + store.scope.Write + "\x00" + account + "\x00" + region))
	return int64(hash.Sum64() & 0x7fffffffffffffff)
}

// LockChangePair takes the pair's poller lock across replicas. A backend
// that serves one process has no such lock and always grants it. When
// acquired is false another replica is polling the pair: the caller reads
// nothing and writes nothing, so two replicas never double the CloudTrail
// rate or overwrite each other's sync state.
func (store *Store) LockChangePair(ctx context.Context, account, region string) (unlock func(), acquired bool, err error) {
	locker, ok := store.provider.(storage.SessionLocker)
	if !ok {
		return func() {}, true, nil
	}
	unlock, acquired, err = locker.TryLockSession(ctx, store.changeLockKey(account, region))
	if err != nil || !acquired {
		return nil, false, err
	}
	return unlock, true, nil
}

// globalStreamHome is the region whose CloudTrail holds the global stream.
const globalStreamHome = "us-east-1"

// ChangePairPolledAt is when any replica last recorded a poll of the pair,
// read from the shared store; zero when it was never polled.
func (store *Store) ChangePairPolledAt(ctx context.Context, account, region string) (time.Time, error) {
	if !store.ready() {
		return time.Time{}, ErrNoStorage
	}
	feeds, ok := store.inventory.(storage.AWSChangeFeedStore)
	if !ok {
		return time.Time{}, ErrNoStorage
	}
	state, found, err := feeds.AWSChangePairState(ctx, store.scope, account, region)
	if err != nil || !found {
		return time.Time{}, err
	}
	return state.LastPollAt, nil
}

// ChangePairLockHeld reports whether this process still holds the pair's
// poller lock. A session lock has no lease: the database may end the
// session, so a poller asks before it writes and drops its poll when the
// lock is gone.
func (store *Store) ChangePairLockHeld(ctx context.Context, account, region string) bool {
	verifier, ok := store.provider.(storage.SessionLockVerifier)
	if !ok {
		return true
	}
	held, err := verifier.SessionLockHeld(ctx, store.changeLockKey(account, region))
	return err == nil && held
}
