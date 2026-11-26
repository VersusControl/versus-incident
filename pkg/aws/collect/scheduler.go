package collect

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// ledgerFlushTimeout bounds the ledger write a stopping scheduler makes.
const ledgerFlushTimeout = 5 * time.Second

// schedulerTick is how often the scheduler looks for due work.
const schedulerTick = 10 * time.Second

// kindOrder runs health first so a first sync shows what is broken early,
// then changes, then the slower inventory, then the policy reads.
var kindOrder = []Kind{KindHealth, KindChanges, KindInventory, KindPolicy}

// Scheduler runs the collector for every connected account on its interval
// per kind. One goroutine does all the work, account after account, so the
// collectors never compete with each other for an account's budget.
type Scheduler struct {
	collector *Collector
	targets   func(context.Context) []Target
	intervals func() Intervals
	enabled   func() bool
	now       func() time.Time
	tick      time.Duration
	accounts  func(context.Context) ([]string, error)

	// callTarget is the daily call target per account and callTotals
	// reads each account's calls in the last 24 hours from the ledger.
	// Without both nothing is ever stretched.
	callTarget func() int
	callTotals func(ctx context.Context, since time.Time) (map[string]int64, error)
	// changes reads the change feed that drives re-reads; nil reads the
	// collector's store.
	changes func(ctx context.Context, account string, since, until time.Time) ([]model.Change, error)

	mu      sync.Mutex
	policy  map[string]policyState
	pending map[string]bool
	// units is each unit's state from this process's own runs.
	units map[string]*unitState
	// policyChanged holds policy units a change made stale.
	policyChanged map[string]bool
	// seen holds, per account, the change IDs already acted on and when
	// they were first seen; scanned is the last scan of the feed.
	seen    map[string]map[string]time.Time
	scanned map[string]time.Time
	// totals caches the ledger's 24-hour totals per account, and
	// stretched the factor each account runs at: it rises when the
	// account is over its target and falls only below StretchRelease of
	// it.
	totals    map[string]int64
	totalsAt  time.Time
	stretched map[string]int
	// feedsDown holds the account Regions ("account/region") whose change
	// feed cannot drive re-reads.
	feedsDown map[string]bool
	wake      chan struct{}
	stopped   chan struct{}
	once      sync.Once
}

// SchedulerOption adjusts a Scheduler.
type SchedulerOption func(*Scheduler)

// WithSchedulerClock injects the clock that decides what is due.
func WithSchedulerClock(now func() time.Time) SchedulerOption {
	return func(scheduler *Scheduler) {
		if now != nil {
			scheduler.now = now
		}
	}
}

// WithAccounts names every connected account. Stored data for any other
// account (removed from config or deleted elsewhere) is purged on each pass.
// When the list cannot be read nothing is purged.
func WithAccounts(accounts func(context.Context) ([]string, error)) SchedulerOption {
	return func(scheduler *Scheduler) { scheduler.accounts = accounts }
}

// WithTick sets how often the loop looks for due work.
func WithTick(tick time.Duration) SchedulerOption {
	return func(scheduler *Scheduler) {
		if tick > 0 {
			scheduler.tick = tick
		}
	}
}

// WithCallTarget stretches the inventory and policy cadences of an account
// whose calls in the last 24 hours, as totals reads them from the call
// ledger, are over target. Health is never stretched.
func WithCallTarget(target func() int, totals func(ctx context.Context, since time.Time) (map[string]int64, error)) SchedulerOption {
	return func(scheduler *Scheduler) {
		scheduler.callTarget = target
		scheduler.callTotals = totals
	}
}

// WithChangeFeed replaces the change feed the scheduler reads to re-read
// changed units; the default reads the collector's store.
func WithChangeFeed(read func(ctx context.Context, account string, since, until time.Time) ([]model.Change, error)) SchedulerOption {
	return func(scheduler *Scheduler) {
		if read != nil {
			scheduler.changes = read
		}
	}
}

// NewScheduler builds a scheduler. targets lists the accounts to read with
// their credentials; intervals and enabled are read live on every pass.
func NewScheduler(collector *Collector, targets func(context.Context) []Target, intervals func() Intervals, enabled func() bool, options ...SchedulerOption) *Scheduler {
	scheduler := &Scheduler{
		collector:     collector,
		targets:       targets,
		intervals:     intervals,
		enabled:       enabled,
		now:           time.Now,
		tick:          schedulerTick,
		policy:        make(map[string]policyState),
		pending:       make(map[string]bool),
		units:         make(map[string]*unitState),
		policyChanged: make(map[string]bool),
		seen:          make(map[string]map[string]time.Time),
		stretched:     make(map[string]int),
		feedsDown:     make(map[string]bool),
		scanned:       make(map[string]time.Time),
		wake:          make(chan struct{}, 1),
		stopped:       make(chan struct{}),
	}
	for _, option := range options {
		option(scheduler)
	}
	return scheduler
}

// Collector returns the collector the scheduler runs.
func (scheduler *Scheduler) Collector() *Collector { return scheduler.collector }

// Trigger asks for a full sync of one account on the next pass, for example
// right after it is connected so the first sync fills in live.
func (scheduler *Scheduler) Trigger(accountID string) {
	if scheduler == nil {
		return
	}
	scheduler.mu.Lock()
	scheduler.pending[accountID] = true
	scheduler.mu.Unlock()
	select {
	case scheduler.wake <- struct{}{}:
	default:
	}
}

// Start runs the loop until ctx ends. It is inert when collection is off;
// the loop re-reads the switch on every pass, so turning it on at runtime
// takes effect without a restart.
func (scheduler *Scheduler) Start(ctx context.Context) {
	if scheduler == nil || ctx == nil {
		return
	}
	go func() {
		defer scheduler.once.Do(func() { close(scheduler.stopped) })
		// Counts a failed ledger write kept are written once more on the
		// way out, best effort.
		defer func() {
			flush, cancel := context.WithTimeout(context.Background(), ledgerFlushTimeout)
			defer cancel()
			_ = scheduler.collector.FlushLedger(flush)
		}()
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			case <-scheduler.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			scheduler.RunDue(ctx)
			timer.Reset(scheduler.tick)
		}
	}()
}

// Stopped closes once the loop started by Start has exited.
func (scheduler *Scheduler) Stopped() <-chan struct{} { return scheduler.stopped }

// RunDue runs every unit that is due for every target, once. Health runs
// first so a first sync shows what is broken early, then the change poll,
// whose new changes mark the units they make stale, then inventory and the
// policy reads. A trigger runs every unit but the policy reads, which keep
// their own cadence in the store.
func (scheduler *Scheduler) RunDue(ctx context.Context) {
	if scheduler.enabled != nil && !scheduler.enabled() {
		return
	}
	scheduler.purgeRemovedAccounts(ctx)
	intervals := scheduler.intervals().Effective()
	for _, target := range scheduler.targets(ctx) {
		if ctx.Err() != nil {
			return
		}
		scheduler.mu.Lock()
		forced := scheduler.pending[target.AccountID]
		delete(scheduler.pending, target.AccountID)
		scheduler.mu.Unlock()
		factor := scheduler.stretchFactor(ctx, target.AccountID)

		for _, kind := range kindOrder {
			switch kind {
			case KindPolicy:
				if !scheduler.runPolicy(ctx, target, intervals.Policies*time.Duration(factor), forced) {
					return
				}
			case KindChanges:
				ran, ok := scheduler.runUnitsReport(ctx, target, kind, intervals, factor, forced)
				if !ok {
					return
				}
				if ran {
					scheduler.checkFeeds(target)
					scheduler.scanChanges(ctx, target.AccountID, intervals)
				}
			default:
				if !scheduler.runUnits(ctx, target, kind, intervals, factor, forced) {
					return
				}
			}
		}
	}
}

// Cadence returns the interval one unit runs at now, as this process
// schedules it.
func (scheduler *Scheduler) Cadence(unit model.Unit) UnitCadence {
	scheduler.mu.Lock()
	var state unitState
	if known := scheduler.units[unit.Key()]; known != nil {
		state = *known
	}
	scheduler.mu.Unlock()
	return cadenceOf(unit, &state, scheduler.intervals(), scheduler.stretchFactor(context.Background(), unit.Account), scheduler.now(),
		scheduler.feedDown(unit))
}

// feedDown reports whether the change feed of a unit's Region cannot drive
// re-reads; a global unit follows the account's global stream.
func (scheduler *Scheduler) feedDown(unit model.Unit) bool {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return scheduler.feedsDown[unit.Account+"/"+unit.Region]
}

// runUnits runs the units of one kind that are due: never run here, made
// stale by a change, or past their interval. It reports false only when
// ctx ended.
func (scheduler *Scheduler) runUnits(ctx context.Context, target Target, kind Kind, intervals Intervals, factor int, forced bool) bool {
	_, ok := scheduler.runUnitsReport(ctx, target, kind, intervals, factor, forced)
	return ok
}

// runUnitsReport is runUnits that also reports whether any unit ran. A
// unit whose last run did not commit is due at its retry time.
func (scheduler *Scheduler) runUnitsReport(ctx context.Context, target Target, kind Kind, intervals Intervals, factor int, forced bool) (bool, bool) {
	now := scheduler.now()
	due := map[string]bool{}
	scheduler.mu.Lock()
	for _, unit := range Plan(target.AccountID, target.Regions, kind) {
		state := scheduler.units[unit.Key()]
		switch {
		case forced || state == nil:
			due[unit.Key()] = true
		case state.failures > 0 && !state.off:
			// A unit that is failing waits for its retry, even with a
			// change to read: the change stays marked until a run
			// commits.
			due[unit.Key()] = !now.Before(state.retryAt)
		case state.changed:
			due[unit.Key()] = true
		case now.Sub(state.last) >= cadenceOf(unit, state, intervals, factor, now, scheduler.feedsDown[unit.Account+"/"+unit.Region]).Interval:
			due[unit.Key()] = true
		}
	}
	scheduler.mu.Unlock()
	for key, run := range due {
		if !run {
			delete(due, key)
		}
	}
	if len(due) == 0 {
		return false, true
	}
	report, err := scheduler.collector.sync(ctx, target, []Kind{kind}, due)
	if err != nil && ctx.Err() != nil {
		return true, false
	}
	scheduler.note(report, intervals, now)
	return true, true
}

// note records what each unit run said: when it ran, whether its Region or
// plan is off, and whether a health unit is unhealthy, which keeps it on
// the fast cadence for an hour after it is clear.
func (scheduler *Scheduler) note(report Report, intervals Intervals, now time.Time) {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	for _, unit := range report.Units {
		state := scheduler.units[unit.Unit.Key()]
		if state == nil {
			state = &unitState{}
			scheduler.units[unit.Unit.Key()] = state
		}
		state.last = now
		state.off = unit.ErrorCode == awsauth.ErrRegionDisabled.Error() || unit.ErrorCode == CodeNotOnPlan
		switch {
		case unit.Outcome == OutcomeCommitted || unit.Outcome == OutcomeSkipped:
			state.changed = false
			state.failures, state.retryAt = 0, time.Time{}
		case !state.off:
			// The run did not commit: a change it was to read stays
			// unread, and the unit retries soon rather than a full
			// interval later, so a permission granted now shows within
			// the backoff.
			state.failures++
			interval := cadenceOf(unit.Unit, state, intervals, 1, now).Interval
			state.retryAt = now.Add(retryBackoff(unit.Unit, state.failures, intervals, interval))
		}
		if unit.Outcome == OutcomeCommitted && unit.Unhealthy {
			state.heat(now, now.Add(intervals.HotFor))
		}
	}
}

// changeScanOverlap re-reads the newest stretch of the feed on each scan,
// because CloudTrail delivers events late. IDs already acted on are not
// acted on again.
const changeScanOverlap = 2 * time.Hour

// scanChanges reads the account's new changes from the feed and marks the
// units they make stale: an inventory or identity unit is read again on
// this pass, a policy unit only for an event that can change a policy,
// and a health unit goes on the fast cadence for an hour from the change.
// The first scan of a process only learns what is there: every unit runs
// once at start anyway.
func (scheduler *Scheduler) scanChanges(ctx context.Context, account string, intervals Intervals) {
	now := scheduler.now()
	scheduler.mu.Lock()
	_, before := scheduler.scanned[account]
	scheduler.mu.Unlock()
	since := now.Add(-changeScanOverlap)
	changes, err := scheduler.readChanges(ctx, account, since, now.Add(time.Minute))
	if err != nil {
		return
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	scheduler.scanned[account] = now
	seen := scheduler.seen[account]
	if seen == nil {
		seen = map[string]time.Time{}
		scheduler.seen[account] = seen
	}
	for id, at := range seen {
		if now.Sub(at) > 2*changeScanOverlap {
			delete(seen, id)
		}
	}
	for _, change := range changes {
		if _, known := seen[change.ID]; known {
			continue
		}
		seen[change.ID] = now
		for _, unit := range unitsForChange(change) {
			switch KindOf(unit.Service) {
			case KindHealth:
				if until := change.Time.Add(intervals.HotFor); now.Before(until) {
					state := scheduler.units[unit.Key()]
					if state == nil {
						state = &unitState{}
						scheduler.units[unit.Key()] = state
					}
					state.heat(now, until)
				}
			case KindPolicy:
				if before {
					scheduler.policyChanged[unit.Key()] = true
				}
			default:
				if state := scheduler.units[unit.Key()]; before && state != nil {
					state.changed = true
				}
			}
		}
	}
}

func (scheduler *Scheduler) readChanges(ctx context.Context, account string, since, until time.Time) ([]model.Change, error) {
	if scheduler.changes != nil {
		return scheduler.changes(ctx, account, since, until)
	}
	result, err := scheduler.collector.Store().Changes(ctx, snapshot.Query{Account: account}, since, until)
	if err != nil {
		return nil, err
	}
	return result.Changes, nil
}

// totalsRefresh is how often the ledger's 24-hour totals are read again.
const totalsRefresh = 5 * time.Minute

// stretchFactor is how much the account's inventory and policy cadences
// are slowed by the daily call target.
func (scheduler *Scheduler) stretchFactor(ctx context.Context, account string) int {
	if scheduler.callTarget == nil || scheduler.callTotals == nil {
		return 1
	}
	target := scheduler.callTarget()
	if target <= 0 {
		return 1
	}
	now := scheduler.now()
	scheduler.mu.Lock()
	stale := scheduler.totals == nil || now.Sub(scheduler.totalsAt) >= totalsRefresh || now.Before(scheduler.totalsAt)
	scheduler.mu.Unlock()
	if stale {
		totals, err := scheduler.callTotals(ctx, now.Add(-24*time.Hour))
		scheduler.mu.Lock()
		if err == nil {
			scheduler.totals = totals
		}
		scheduler.totalsAt = now
		scheduler.mu.Unlock()
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	factor := StretchFactor(scheduler.totals[account], target)
	if previous := scheduler.stretched[account]; factor < previous && float64(scheduler.totals[account]) >= StretchRelease*float64(target) {
		factor = previous
	}
	if factor > 1 {
		scheduler.stretched[account] = factor
	} else {
		delete(scheduler.stretched, account)
	}
	return factor
}

// StretchRelease is the share of the daily target an account must fall
// below before a stretch is released, so the cadence does not flap at the
// target.
const StretchRelease = 0.8

// Stretched returns the stretch each account runs at now; an account not
// slowed down is absent.
func (scheduler *Scheduler) Stretched() map[string]int {
	if scheduler == nil {
		return nil
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	out := make(map[string]int, len(scheduler.stretched))
	for account, factor := range scheduler.stretched {
		out[account] = factor
	}
	return out
}

// checkFeeds marks each Region of the account whose change feed cannot
// drive re-reads: the last poll failed or was cut short, or the stretch
// read to the end is more than FeedLagLimit old. Inventory there falls
// back to FeedFallbackInterval until the feed recovers. A Region that is
// not turned on has no feed to miss.
func (scheduler *Scheduler) checkFeeds(target Target) {
	now := scheduler.now()
	store := scheduler.collector.Store()
	regions := append(cleanRegions(target.Regions), model.GlobalRegion)
	down := make(map[string]bool, len(regions))
	for _, region := range regions {
		unit := model.Unit{Account: target.AccountID, Region: region, Service: snapshot.ChangeService}
		scheduler.mu.Lock()
		state := scheduler.units[unit.Key()]
		off := state != nil && state.off
		scheduler.mu.Unlock()
		if off {
			continue
		}
		checkpoint, err := store.Checkpoint(target.AccountID, region)
		if err != nil {
			down[region] = true
			continue
		}
		var end time.Time
		for _, interval := range checkpoint.Covered {
			if interval.To.After(end) {
				end = interval.To
			}
		}
		down[region] = checkpoint.ErrorCode != "" || end.IsZero() || now.Sub(end) > FeedLagLimit
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	for region, bad := range down {
		key := target.AccountID + "/" + region
		if bad {
			scheduler.feedsDown[key] = true
		} else {
			delete(scheduler.feedsDown, key)
		}
	}
}

// policyRetry is how soon a policy read that failed is tried again.
const policyRetry = time.Hour

// policyState is what this process knows of one account's policy reads:
// the units it planned and when the next of them is due.
type policyState struct {
	units string
	next  time.Time
}

// runPolicy reads the account's policy units that are due. The cadence is
// per unit and kept in the store, not in this process: a unit is due when
// its own stored attempt, as any replica recorded it, is older than the
// interval, or it was never attempted. A restart or a second replica never
// reads a unit again inside the interval, and a new Region or a pass cut
// short reads only the units that need it. A trigger does not force a unit
// read inside its interval, but it always looks at the store again, so a
// unit never attempted (a first sync, an account connected again after a
// purge) runs at once. The replicas share one lock per account, and a
// replica that does not get it leaves the read to the one that did. It
// reports false only when ctx ended.
func (scheduler *Scheduler) runPolicy(ctx context.Context, target Target, interval time.Duration, forced bool) bool {
	now := scheduler.now()
	units := Plan(target.AccountID, target.Regions, KindPolicy)
	keys := make([]string, 0, len(units))
	for _, unit := range units {
		keys = append(keys, unit.Key())
	}
	fingerprint := strings.Join(keys, "\x00")
	scheduler.mu.Lock()
	state, known := scheduler.policy[target.AccountID]
	changed := map[string]bool{}
	for _, key := range keys {
		if scheduler.policyChanged[key] {
			changed[key] = true
		}
	}
	scheduler.mu.Unlock()
	if !forced && len(changed) == 0 && known && state.units == fingerprint && now.Before(state.next) {
		return true
	}
	store := scheduler.collector.Store()
	unlock, acquired, err := store.LockPolicyReads(ctx, target.AccountID)
	if err != nil || !acquired {
		return ctx.Err() == nil
	}
	defer unlock()
	statuses, err := store.Sync(ctx, snapshot.Query{Account: target.AccountID})
	if err != nil {
		return ctx.Err() == nil
	}
	attempted := map[string]time.Time{}
	failed := map[string]bool{}
	off := map[string]bool{}
	for _, status := range statuses {
		attempted[status.Unit.Key()] = status.Sync.AttemptedAt
		code := status.Sync.ErrorCode
		off[status.Unit.Key()] = code == awsauth.ErrRegionDisabled.Error() || code == CodeNotOnPlan
		failed[status.Unit.Key()] = status.Sync.State == model.SyncFailed && !off[status.Unit.Key()]
	}
	due := map[string]bool{}
	next := now.Add(interval)
	for _, key := range keys {
		at := attempted[key]
		// A change the scheduler saw after another replica's read is
		// read again; one older than the stored attempt is already in it.
		wait := interval
		if off[key] {
			// A Region that is not turned on is tried again once a day.
			wait = max(interval, scheduler.intervals().Effective().RegionsOff)
		}
		if failed[key] {
			// A failed read retries within the hour, not a full interval
			// later.
			wait = min(interval, policyRetry)
		}
		if at.IsZero() || now.Sub(at) >= wait || changed[key] {
			due[key] = true
			continue
		}
		if at.Add(wait).Before(next) {
			next = at.Add(wait)
		}
	}
	if len(due) > 0 {
		if _, err := scheduler.collector.sync(ctx, target, []Kind{KindPolicy}, due); err != nil {
			return ctx.Err() == nil
		}
	}
	scheduler.mu.Lock()
	scheduler.policy[target.AccountID] = policyState{units: fingerprint, next: next}
	for key := range changed {
		delete(scheduler.policyChanged, key)
	}
	scheduler.mu.Unlock()
	return true
}

// forget drops what this process knows of an account's units.
func (scheduler *Scheduler) forget(account string) {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	delete(scheduler.policy, account)
	delete(scheduler.seen, account)
	delete(scheduler.scanned, account)
	delete(scheduler.stretched, account)
	for key := range scheduler.feedsDown {
		if strings.HasPrefix(key, account+"/") {
			delete(scheduler.feedsDown, key)
		}
	}
	prefix := account + "/"
	for key := range scheduler.units {
		if strings.HasPrefix(key, prefix) {
			delete(scheduler.units, key)
		}
	}
	for key := range scheduler.policyChanged {
		if strings.HasPrefix(key, prefix) {
			delete(scheduler.policyChanged, key)
		}
	}
}

func (scheduler *Scheduler) purgeRemovedAccounts(ctx context.Context) {
	if scheduler.accounts == nil {
		return
	}
	known, err := scheduler.accounts(ctx)
	if err != nil {
		// An incomplete account list must never read as "removed": skip the
		// whole purge pass and say so with a closed code.
		scheduler.collector.logf("aws collect: purge skipped code=account_list_unavailable")
		return
	}
	connected := make(map[string]bool, len(known))
	for _, account := range known {
		connected[account] = true
	}
	store := scheduler.collector.Store()
	stored, err := store.Accounts()
	if err != nil {
		return
	}
	for _, account := range stored {
		if !connected[account] {
			scheduler.forget(account)
			if err := store.PurgeAccount(account); err == nil {
				scheduler.collector.logf("aws collect: account=%s purged after it was removed", account)
			}
		}
	}
}
