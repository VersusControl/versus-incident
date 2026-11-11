package cost

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// schedulerTick is how often the scheduler looks at the clock and the live
// configuration.
const schedulerTick = time.Minute

// reconcileEvery is the refresh interval: the scheduler reconciles the cost
// rollups again at most this often, on a tick that gets the replica lock,
// whether or not the day's refresh succeeded or runs at all.
const reconcileEvery = 24 * time.Hour

// ScheduleFunc returns the live schedule: whether cost data is on and the
// daily refresh time in its location.
type ScheduleFunc func() (enabled bool, hour, minute int, loc *time.Location)

// Scheduler runs the once-a-day paid refresh at the configured time and,
// after it, hands new increases to the alert path. Every tick it first
// brings the adapter's options up to date (accounts and settings change at
// runtime). It is the only background caller of AWS cost APIs.
type Scheduler struct {
	adapter  *Adapter
	schedule ScheduleFunc
	prepare  func(ctx context.Context) error
	after    func(ctx context.Context, res RefreshResult)
	now      func() time.Time
	logf     func(format string, args ...any)
	tick     time.Duration

	// reconciled is when this scheduler last compared the rollups with the
	// cost lines; zero until it has.
	reconciled time.Time
	reconcileM sync.Mutex

	stopped chan struct{}
	once    sync.Once
}

// SchedulerOption adjusts a Scheduler.
type SchedulerOption func(*Scheduler)

// WithClock injects the clock (tests).
func WithClock(now func() time.Time) SchedulerOption {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// WithLogger sets the log function.
func WithLogger(logf func(string, ...any)) SchedulerOption {
	return func(s *Scheduler) {
		if logf != nil {
			s.logf = logf
		}
	}
}

// WithAfterRefresh runs after every scheduled refresh (the alert pump).
func WithAfterRefresh(after func(ctx context.Context, res RefreshResult)) SchedulerOption {
	return func(s *Scheduler) { s.after = after }
}

// NewScheduler builds the scheduler. prepare updates the adapter's options
// from the live configuration; it may be nil.
func NewScheduler(adapter *Adapter, schedule ScheduleFunc, prepare func(ctx context.Context) error, options ...SchedulerOption) *Scheduler {
	s := &Scheduler{adapter: adapter, schedule: schedule, prepare: prepare, now: time.Now,
		logf: func(string, ...any) {}, tick: schedulerTick, stopped: make(chan struct{})}
	for _, o := range options {
		o(s)
	}
	return s
}

// Start runs the loop until ctx ends. Stopped closes when it has returned.
func (s *Scheduler) Start(ctx context.Context) {
	s.once.Do(func() {
		go func() {
			defer close(s.stopped)
			t := time.NewTicker(s.tick)
			defer t.Stop()
			for {
				s.RunDue(ctx)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	})
}

// Stopped closes when the loop has returned.
func (s *Scheduler) Stopped() <-chan struct{} { return s.stopped }

// RunDue refreshes when today's refresh time has passed and today's
// scheduled refresh has not run. It reports whether a refresh ran. Then,
// at most once per refresh interval, it reconciles the cost rollups again,
// also when the refresh failed, was not due or cost data is off: any writer
// may have changed cost lines around the rollups since the last reconcile
// (an older replica during a rollout, a downgrade, a manual edit), and such
// drift must not wait for a successful refresh. It makes no AWS call.
func (s *Scheduler) RunDue(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	ran := s.runDue(ctx)
	if ctx.Err() == nil && s.reconcileDue() {
		s.reconcileAgain(ctx, "")
	}
	return ran
}

// reconcileDue reports whether the last reconcile is a refresh interval
// old, or none ran yet.
func (s *Scheduler) reconcileDue() bool {
	s.reconcileM.Lock()
	defer s.reconcileM.Unlock()
	return s.reconciled.IsZero() || s.now().Sub(s.reconciled) >= reconcileEvery
}

// noteReconciled records a reconcile that compared the rollups; one that
// could not take the replica lock is retried on the next tick.
func (s *Scheduler) noteReconciled(r RollupReconcile) {
	if !r.Compared {
		return
	}
	s.reconcileM.Lock()
	s.reconciled = s.now()
	s.reconcileM.Unlock()
}

// reconcileAgain reconciles the rollups even when this process has before,
// under the replica lock, and logs any drift it repaired.
func (s *Scheduler) reconcileAgain(ctx context.Context, when string) {
	again, err := s.adapter.reconcileAfterRefresh(ctx)
	s.noteReconciled(again)
	if err != nil {
		s.logf("aws cost: cost rollups not reconciled%s: %v", when, err)
	} else if again.Repair.RebuiltDays > 0 || again.Repair.RebuiltMonths > 0 {
		s.logf("aws cost: cost rollups drifted: %d days and %d months rebuilt", again.Repair.RebuiltDays, again.Repair.RebuiltMonths)
	}
}

func (s *Scheduler) runDue(ctx context.Context) bool {
	if s.prepare != nil {
		if err := s.prepare(ctx); err != nil {
			s.logf("aws cost: options not updated: %v", ErrorCode(err))
		}
	}
	// Until this process has reconciled the cost rollups, totals read the
	// cost lines; reconcile here, in the background loop, so serving never
	// waits for it. A busy replica lock means another replica works; the
	// next tick tries again.
	first, err := s.adapter.ReconcileRollups(ctx)
	s.noteReconciled(first)
	if err != nil {
		s.logf("aws cost: cost rollups not reconciled: %v", err)
	}
	// A payer removal that met a running refresh left its purge pending.
	// Purge it on the tick, also when cost data is off or no payer is left,
	// so it never waits for a refresh that will not run.
	if n, err := s.adapter.purgePending(ctx); err != nil {
		s.logf("aws cost: pending purge failed: %s", refreshErrorText(err))
	} else if n > 0 {
		s.logf("aws cost: purged %d removed payers", n)
	}
	enabled, hour, minute, loc := s.schedule()
	if !enabled {
		return false
	}
	if loc == nil {
		loc = time.UTC
	}
	now := s.now().In(loc)
	if now.Hour()*60+now.Minute() < hour*60+minute {
		return false
	}
	st, err := s.adapter.cache.status()
	if err != nil || st.ScheduledOn == finops.DateOf(now) {
		return false
	}
	res, err := s.adapter.refreshDue(ctx)
	if errors.Is(err, ErrBusy) || errors.Is(err, errNotDue) {
		// Another replica is running or already ran today's refresh.
		return false
	}
	if errors.Is(err, ErrDisabled) {
		s.logf("aws cost: scheduled refresh skipped: %s", disabledReasonText(s.adapter.options().DisabledReason))
		return false
	}
	if err != nil {
		s.logf("aws cost: scheduled refresh failed: %s", refreshErrorText(err))
		return false
	}
	// Reconcile again right after a scheduled refresh as well, so drift
	// written before it lasts at most one refresh cycle; this counts as the
	// interval's reconcile.
	s.reconcileAgain(ctx, " after the refresh")
	if s.after != nil {
		s.after(ctx, res)
	}
	return true
}

// disabledReasonText names why cost is not read, so the log never says
// "cost data is off" while cost data is on but no account is usable yet.
func disabledReasonText(reason string) string {
	switch reason {
	case ReasonNoConnectedAccounts:
		return "no account is ready to read cost through yet"
	case ReasonCostSourceNotLinked:
		return "the configured cost source account is not connected"
	case "", ReasonCostDataOff:
		return "cost data is off"
	}
	return "cost data is unavailable"
}

func refreshErrorText(err error) string {
	switch {
	case errors.Is(err, ErrDisabled), errors.Is(err, ErrManualUsed):
		return err.Error()
	case errors.Is(err, ErrLockLost):
		return ErrLockLost.Error()
	}
	return "storage error"
}

// ServiceFunc builds the FinOps service from the live configuration; it
// lets the signal source follow configuration changes.
type ServiceFunc func() *finops.Service

// SyncIncreases implements IncreaseSyncer.
func (f ServiceFunc) SyncIncreases(ctx context.Context) ([]finops.IncreaseRecord, error) {
	return f().SyncIncreases(ctx)
}

// MarkIncreaseAlerted implements IncreaseSyncer.
func (f ServiceFunc) MarkIncreaseAlerted(ctx context.Context, id string) error {
	return f().MarkIncreaseAlerted(ctx, id)
}

// SyncBudgetAlerts implements BudgetSyncer.
func (f ServiceFunc) SyncBudgetAlerts(ctx context.Context) ([]finops.BudgetAlert, error) {
	return f().SyncBudgetAlerts(ctx)
}

// MarkBudgetAlerted implements BudgetSyncer.
func (f ServiceFunc) MarkBudgetAlerted(ctx context.Context, a finops.BudgetAlert) error {
	return f().MarkBudgetAlerted(ctx, a)
}

// AlertSink sends one increase signal through the alert path.
type AlertSink func(ctx context.Context, sig core.Signal) error

// PumpAlerts pulls the increases due for an alert from the signal source,
// sends each through sink, and records the alerts only when every send
// succeeded, so an increase is alerted once and a failed send is retried by
// the next pump (the alert path deduplicates a repeat).
func PumpAlerts(ctx context.Context, src *SignalSource, sink AlertSink) (int, error) {
	sigs, _, err := src.Pull(ctx, time.Time{})
	if err != nil {
		return 0, err
	}
	for _, sig := range sigs {
		if err := sink(ctx, sig); err != nil {
			src.mu.Lock()
			src.staged, src.budgets = nil, nil
			src.mu.Unlock()
			return 0, err
		}
	}
	return len(sigs), src.Commit(ctx)
}
