package collect

import (
	"context"
	"sync"
	"time"
)

// Intervals is how often each kind of unit is re-read.
type Intervals struct {
	Inventory time.Duration
	Health    time.Duration
	Changes   time.Duration
}

func (intervals Intervals) of(kind Kind) time.Duration {
	switch kind {
	case KindHealth:
		return intervals.Health
	case KindChanges:
		return intervals.Changes
	default:
		return intervals.Inventory
	}
}

// schedulerTick is how often the scheduler looks for due work.
const schedulerTick = 10 * time.Second

// kindOrder runs health first so a first sync shows what is broken early,
// then changes, then the slower inventory.
var kindOrder = []Kind{KindHealth, KindChanges, KindInventory}

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

	mu      sync.Mutex
	last    map[string]time.Time
	pending map[string]bool
	wake    chan struct{}
	stopped chan struct{}
	once    sync.Once
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

// NewScheduler builds a scheduler. targets lists the accounts to read with
// their credentials; intervals and enabled are read live on every pass.
func NewScheduler(collector *Collector, targets func(context.Context) []Target, intervals func() Intervals, enabled func() bool, options ...SchedulerOption) *Scheduler {
	scheduler := &Scheduler{
		collector: collector,
		targets:   targets,
		intervals: intervals,
		enabled:   enabled,
		now:       time.Now,
		tick:      schedulerTick,
		last:      make(map[string]time.Time),
		pending:   make(map[string]bool),
		wake:      make(chan struct{}, 1),
		stopped:   make(chan struct{}),
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

func lastKey(accountID string, kind Kind) string { return accountID + "|" + string(kind) }

// RunDue runs every kind that is due for every target, once.
func (scheduler *Scheduler) RunDue(ctx context.Context) {
	if scheduler.enabled != nil && !scheduler.enabled() {
		return
	}
	scheduler.purgeRemovedAccounts(ctx)
	intervals := scheduler.intervals()
	for _, target := range scheduler.targets(ctx) {
		if ctx.Err() != nil {
			return
		}
		scheduler.mu.Lock()
		forced := scheduler.pending[target.AccountID]
		delete(scheduler.pending, target.AccountID)
		scheduler.mu.Unlock()

		for _, kind := range kindOrder {
			now := scheduler.now()
			key := lastKey(target.AccountID, kind)
			scheduler.mu.Lock()
			last, ran := scheduler.last[key]
			scheduler.mu.Unlock()
			if !forced && ran && now.Sub(last) < intervals.of(kind) {
				continue
			}
			if _, err := scheduler.collector.Sync(ctx, target, kind); err != nil && ctx.Err() != nil {
				return
			}
			scheduler.mu.Lock()
			scheduler.last[key] = now
			scheduler.mu.Unlock()
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
			if err := store.PurgeAccount(account); err == nil {
				scheduler.collector.logf("aws collect: account=%s purged after it was removed", account)
			}
		}
	}
}
