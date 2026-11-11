package cost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

var (
	// ErrDisabled means cost data is turned off; nothing is fetched.
	ErrDisabled = errors.New("cost: cost data is off")
	// ErrManualUsed means the manual refresh already ran today.
	ErrManualUsed = errors.New("cost: manual refresh already used today")
	// ErrUnknownAccount means a drill-down names an account no payer covers.
	ErrUnknownAccount = errors.New("cost: account not covered by a cost source")
	// ErrInvalidAccount means an account ID is not 12 digits.
	ErrInvalidAccount = errors.New("cost: invalid account")
	// ErrUnknownPair means a drill-down names an account and service the
	// cached cost does not hold, so no paid request is spent on it.
	ErrUnknownPair = errors.New("cost: account and service not in cached cost")
	// ErrReservedForRefresh means a drill-down would leave the scheduled
	// refresh without the requests it needs today.
	ErrReservedForRefresh = errors.New("cost: requests reserved for the scheduled refresh")
	// ErrBusy means another replica sharing the storage is calling the AWS
	// cost APIs for the deployment right now; a retry shortly after runs.
	ErrBusy = errors.New("cost: another replica is refreshing cost data")
	// errGroupFailed stops an optional call whose day group already failed,
	// so no request is spent on it.
	errGroupFailed = errors.New("cost: day group failed")
	// errNotDue means today's scheduled refresh already ran, found once the
	// replica lock is held.
	errNotDue = errors.New("cost: scheduled refresh already ran today")
	// ErrLockLost means the database ended the session that held the
	// replica lock while the work ran (a dropped connection, an idle-session
	// timeout). Another replica may hold it now, so the work stops before
	// its next write.
	ErrLockLost = errors.New("cost: the replica lock was lost")
)

// replicaLockKey is the advisory lock key that keeps the paid cost work of a
// deployment (refreshes, drill-downs and payer removals) on one replica at a
// time when replicas share a Postgres database. The value is ASCII
// "VSCOSTR1". It must stay constant: replicas of different versions running
// side by side during a rollout must lock on the same key.
const replicaLockKey int64 = 0x5653434F53545231

// replicaLockPoll is how often a payer removal waiting to purge tries the
// locks again.
const replicaLockPoll = 100 * time.Millisecond

// statusSwapAttempts bounds the compare-and-swap retries of one status
// write. Each lost attempt means another writer stored the status in
// between.
const statusSwapAttempts = 32

// errStatusContended means the refresh state kept changing under other
// writers and could not be stored.
var errStatusContended = errors.New("cost: refresh state contended")

// Adapter fetches AWS cost data into the cache. Only the scheduled refresh,
// "Refresh now" and an on-click drill-down call AWS; Source never does.
type Adapter struct {
	// opts is replaced by Update when the configuration or the connected
	// accounts change; read it through options().
	opts   Options
	optsMu sync.RWMutex
	// removed names payers marked removed (MarkPayerRemoved), with the account
	// settings revision each was removed at, so options read at or before
	// that revision cannot bring their cost lines back. The status record
	// keeps the same tombstones across restarts. Guarded by optsMu.
	removed map[string]uint64
	cache   cache
	meter   *Meter

	// mu serializes refreshes, drill-downs and payer purges of the
	// deployment.
	mu sync.Mutex
	// statusMu orders this process's writes of the refresh state; across
	// replicas compare-and-swap orders them. It is held only for the write.
	statusMu sync.Mutex

	// rollupsMu serializes the rollup reconcile; rollupsDone is set once it
	// has succeeded, or found the store needs none (reconcileRollups).
	rollupsMu   sync.Mutex
	rollupsDone atomic.Bool

	// lease checks that this process still holds the replica lock; nil
	// while it does not hold it or the backend cannot tell.
	lease   func(ctx context.Context) error
	leaseMu sync.Mutex
}

// New builds the adapter over the storage provider, writing under the
// scope's write organization.
func New(provider storage.Provider, scope tenancy.OrgScope, opts Options) (*Adapter, error) {
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	c := cache{provider: provider, scope: scope.Normalized()}
	a := &Adapter{opts: opts, removed: map[string]uint64{}, cache: c}
	a.meter = newMeter(c, func() int { return a.options().MaxPaidRequests }, func() finops.Date { return a.options().today() }, func() time.Time { return a.options().Now() })
	return a, nil
}

func (a *Adapter) options() Options {
	a.optsMu.RLock()
	defer a.optsMu.RUnlock()
	return a.opts
}

// removedPayers names the payers the live options may not collect: each
// payer removed at a settings revision the options are not newer than. A
// tombstone comes from this process or from the status record, so a
// restart keeps it.
func (a *Adapter) removedPayers(st *statusRecord) map[string]bool {
	a.optsMu.RLock()
	defer a.optsMu.RUnlock()
	var out map[string]bool
	add := func(tombstones map[string]uint64) {
		for payer, revision := range tombstones {
			if a.opts.SettingsRevision <= revision {
				if out == nil {
					out = map[string]bool{}
				}
				out[payer] = true
			}
		}
	}
	add(a.removed)
	if st != nil {
		add(st.Removed)
	}
	return out
}

// forgetPayer drops a payer from the live options and remembers the
// revision it was removed at, so a later planner or commit cannot restore
// its cost data from options read before the removal.
func (a *Adapter) forgetPayer(payer string, revision uint64) {
	a.optsMu.Lock()
	defer a.optsMu.Unlock()
	kept := make([]Payer, 0, len(a.opts.Payers))
	for _, p := range a.opts.Payers {
		if p.AccountID != payer {
			kept = append(kept, p)
		}
	}
	a.opts.Payers = kept
	unreachable := make([]UnreachablePayer, 0, len(a.opts.Unreachable))
	for _, u := range a.opts.Unreachable {
		if u.AccountID != payer {
			unreachable = append(unreachable, u)
		}
	}
	a.opts.Unreachable = unreachable
	if a.removed == nil {
		a.removed = map[string]uint64{}
	}
	a.removed[payer] = max(a.removed[payer], revision)
}

// Update replaces the options: payers, connected accounts, owners and
// limits follow the live configuration. The meter and cache are kept. A
// removed payer the options name again becomes collectable only when the
// options were read at a newer settings revision than its removal; the
// tombstone itself is kept, so an older snapshot arriving later still
// cannot revive it.
func (a *Adapter) Update(opts Options) error {
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return err
	}
	a.optsMu.Lock()
	defer a.optsMu.Unlock()
	a.opts = opts
	return nil
}

// Meter is the deployment's paid-request meter.
func (a *Adapter) Meter() *Meter { return a.meter }

// Source is the cache-only finops.CostSource.
func (a *Adapter) Source() *Source { return &Source{adapter: a, cache: a.cache, meter: a.meter} }

func (a *Adapter) env(p Payer) env {
	return env{credentials: p.Credentials, httpClient: a.options().HTTPClient}
}

func (a *Adapter) payer(id string) (Payer, bool) {
	for _, p := range a.options().Payers {
		if p.AccountID == id {
			return p, true
		}
	}
	return Payer{}, false
}

// Estimate prices the refresh without calling AWS, for "returns the
// estimate first" and the Settings planner line.
func (a *Adapter) Estimate(ctx context.Context, kind Kind) (finops.Plan, error) {
	st, err := a.cache.status()
	if err != nil {
		return finops.Plan{}, err
	}
	remaining := a.meter.Remaining(ctx)
	pl := planCalls(a.options(), st, a.removedPayers(st), kind, a.options().today(), remaining)
	return finops.BuildPlan(pl.calls, remaining), nil
}

// SuggestedCap is what a normal day with full detail costs, priced from the
// pages each call used, for the Settings line "about N a day".
func (a *Adapter) SuggestedCap(context.Context) (int, error) {
	st, err := a.cache.status()
	if err != nil {
		return 0, err
	}
	return suggestedCap(a.options(), st), nil
}

// RefreshResult reports one refresh.
type RefreshResult struct {
	Plan      finops.Plan       `json:"plan"`
	Outcome   finops.RunOutcome `json:"outcome"`
	Committed []string          `json:"committed,omitempty"`
	Skipped   []string          `json:"skipped,omitempty"`
}

// run holds one refresh's fetched results until commit.
type run struct {
	tables map[string]map[string]*grouped // group → call → result
	drills map[string]map[string]*grouped // payer → pair → result
	extras map[string]*payerExtras        // payer → optional results
	pages  map[string]map[string]pageStat // payer → call → pages used
}

type payerExtras struct {
	forecast  *forecastRest
	coverage  *coverage
	resources *grouped
	// resCode is the closed error code of a failed resource-level call.
	resCode string
}

// lockReplicas takes the deployment's replica lock for paid cost work, so
// two replicas never spend the daily request cap twice or write the same
// day at once. It waits up to wait for a holder to finish and answers ErrBusy
// after that; zero tries once. A backend without the lock serves one
// process, where the caller's in-process lock is enough.
//
// Holding the lock, it first reconciles the cost rollups once per process
// (reconcileRollups), so nothing this process commits precedes it. A failed
// reconcile is logged and the work goes on: totals then keep reading the
// lines, and the next holder tries again.
func (a *Adapter) lockReplicas(ctx context.Context, wait time.Duration) (func(), error) {
	unlock, err := a.acquireReplicas(ctx, wait)
	if err != nil {
		return nil, err
	}
	if _, err := a.reconcileRollups(ctx, false); err != nil {
		log.Printf("aws cost: cost rollups not reconciled, totals read the cost lines: %v", err)
	}
	return unlock, nil
}

// acquireReplicas takes the replica lock without the rollup reconcile.
func (a *Adapter) acquireReplicas(ctx context.Context, wait time.Duration) (func(), error) {
	locker, ok := a.cache.provider.(storage.SessionLocker)
	if !ok {
		return func() {}, nil
	}
	deadline := time.Now().Add(wait)
	for {
		unlock, acquired, err := locker.TryLockSession(ctx, replicaLockKey)
		if err != nil {
			return nil, err
		}
		if acquired {
			return a.holdLease(unlock), nil
		}
		if !time.Now().Before(deadline) {
			return nil, ErrBusy
		}
		timer := time.NewTimer(replicaLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// holdLease records how to check the replica lock just taken, until unlock.
// A session lock has no lease of its own: the database can end the holding
// session while the work runs, and another replica can then take the lock.
func (a *Adapter) holdLease(unlock func()) func() {
	verifier, ok := a.cache.provider.(storage.SessionLockVerifier)
	if !ok {
		return unlock
	}
	a.leaseMu.Lock()
	a.lease = func(ctx context.Context) error {
		held, err := verifier.SessionLockHeld(ctx, replicaLockKey)
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The caller's own deadline or cancellation ended the check:
			// that stays a timeout or a cancellation, not a lost lock.
			return ctxErr
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrLockLost, err)
		}
		if !held {
			return ErrLockLost
		}
		return nil
	}
	a.leaseMu.Unlock()
	return func() {
		a.leaseMu.Lock()
		a.lease = nil
		a.leaseMu.Unlock()
		unlock()
	}
}

// checkLease answers ErrLockLost when the replica lock this process took
// is no longer held. Every write of cost data made under the lock checks it
// first, so a holder whose session was lost stops instead of writing beside
// the replica that holds the lock now.
func (a *Adapter) checkLease(ctx context.Context) error {
	a.leaseMu.Lock()
	lease := a.lease
	a.leaseMu.Unlock()
	if lease == nil {
		return nil
	}
	return lease(ctx)
}

// Refresh runs one refresh: build and price the plan, run it under the
// meter, then commit every day group whose required calls all succeeded.
// Groups that failed keep their old data and mark the payer stale. When
// another replica is refreshing it answers ErrBusy and spends nothing.
func (a *Adapter) Refresh(ctx context.Context, kind Kind) (RefreshResult, error) {
	return a.refresh(ctx, kind, false)
}

// refreshDue is the scheduled refresh unless it already ran today. The check
// is made under the replica lock, so a replica that waited for another one's
// run does not run the day again.
func (a *Adapter) refreshDue(ctx context.Context) (RefreshResult, error) {
	return a.refresh(ctx, KindScheduled, true)
}

func (a *Adapter) refresh(ctx context.Context, kind Kind, onlyDue bool) (RefreshResult, error) {
	if !a.options().enabled() {
		return RefreshResult{}, ErrDisabled
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := a.lockReplicas(ctx, 0)
	if err != nil {
		return RefreshResult{}, err
	}
	defer unlock()
	st, err := a.cache.status()
	if err != nil {
		return RefreshResult{}, err
	}
	prior := snapshotPayerState(st)
	today := a.options().today()
	if onlyDue && st.ScheduledOn == today {
		return RefreshResult{}, errNotDue
	}
	if kind == KindManual && st.ManualOn == today {
		return RefreshResult{}, ErrManualUsed
	}
	remaining := a.meter.Remaining(ctx)
	pl := planCalls(a.options(), st, a.removedPayers(st), kind, today, remaining)
	plan := finops.BuildPlan(pl.calls, remaining)
	r := &run{tables: map[string]map[string]*grouped{}, drills: map[string]map[string]*grouped{}, extras: map[string]*payerExtras{}, pages: map[string]map[string]pageStat{}}
	failed := map[string]bool{}
	outcome := finops.Execute(ctx, plan, a.meter, func(ctx context.Context, c finops.Call, m finops.Meter) (string, error) {
		code, err := a.runCall(ctx, st, pl.groups[c.Group], c, m, r, failed, today)
		if err != nil && c.Required {
			failed[c.Group] = true
		}
		return code, err
	})
	res := RefreshResult{Plan: plan, Outcome: outcome, Skipped: finops.SkippedCalls(plan, outcome)}
	res.Committed, err = a.commit(ctx, st, pl, plan, outcome, r, today)
	if err == nil {
		err = a.purgeRemovedPayers(ctx, st)
	}
	if errors.Is(err, ErrLockLost) {
		// Another replica may hold the lock and own the status now.
		return res, err
	}
	if err != nil {
		// Some payer days may already be stored. Keep the progress and the
		// failure, so a later tick resumes with what is missing instead of
		// paying for the completed payers again.
		a.restoreUnwritten(st, pl, res.Committed, prior)
		a.markProgress(st, pl, res.Committed, today, kind)
		if a.allPayersRefreshed(st, today, kind) {
			if kind == KindManual {
				st.ManualOn = today
			} else {
				st.ScheduledOn = today
			}
		}
		st.SkippedCalls = res.Skipped
		st.UpdatedAt = a.options().Now()
		if saveErr := a.saveStatus(st); saveErr != nil {
			log.Printf("aws cost: status not saved after a partial refresh: %v", saveErr)
		}
		return res, err
	}
	a.markProgress(st, pl, res.Committed, today, kind)
	if a.allPayersRefreshed(st, today, kind) {
		if kind == KindManual {
			st.ManualOn = today
		} else {
			st.ScheduledOn = today
		}
	}
	if kind == KindScheduled {
		a.freeCalls(ctx, st)
	}
	st.SkippedCalls = res.Skipped
	st.UpdatedAt = a.options().Now()
	return res, a.saveStatus(st)
}

// payerPrior is the part of a payer's state that only a completed write may
// change: history and closed months must not claim days that are not stored.
type payerPrior struct {
	historyStart, lastComplete finops.Date
	closedThrough              string
	closedMonths               []string
	accounts                   map[string]*accountState
	lastSuccess                time.Time
}

func snapshotPayerState(st *statusRecord) map[string]payerPrior {
	out := make(map[string]payerPrior, len(st.Payers))
	for payer, ps := range st.Payers {
		accounts := make(map[string]*accountState, len(ps.Accounts))
		for id, as := range ps.Accounts {
			copied := *as
			accounts[id] = &copied
		}
		out[payer] = payerPrior{
			historyStart: ps.HistoryStart, lastComplete: ps.LastCompleteDay,
			closedThrough: ps.ClosedThrough, closedMonths: append([]string(nil), ps.ClosedMonths...),
			accounts: accounts, lastSuccess: ps.LastSuccess,
		}
	}
	return out
}

// restoreUnwritten puts back the stored state of every payer whose days did
// not reach storage, so a retry plans the same work instead of believing a
// partial setup or a closed month already happened.
func (a *Adapter) restoreUnwritten(st *statusRecord, pl planned, committed []string, prior map[string]payerPrior) {
	done := make(map[string]bool, len(committed))
	for _, g := range committed {
		done[g] = true
	}
	for g, spec := range pl.groups {
		if done[g] {
			continue
		}
		ps := st.Payers[spec.payer]
		if ps == nil {
			continue
		}
		pr, ok := prior[spec.payer]
		if !ok {
			pr = payerPrior{}
		}
		ps.HistoryStart, ps.LastCompleteDay = pr.historyStart, pr.lastComplete
		ps.ClosedThrough, ps.ClosedMonths = pr.closedThrough, pr.closedMonths
		ps.Accounts, ps.LastSuccess = pr.accounts, pr.lastSuccess
		ps.Stale = true
	}
}

// markProgress records every payer whose whole planned work committed, so a
// retry after a partial write plans only the missing payers.
func (a *Adapter) markProgress(st *statusRecord, pl planned, committed []string, today finops.Date, kind Kind) {
	done := make(map[string]bool, len(committed))
	for _, g := range committed {
		done[g] = true
	}
	pending := map[string]bool{}
	for g, spec := range pl.groups {
		if !done[g] {
			pending[spec.payer] = true
		}
	}
	removed := a.removedPayers(st)
	for _, p := range a.options().Payers {
		if pending[p.AccountID] || removed[p.AccountID] {
			continue
		}
		ps := st.Payers[p.AccountID]
		if ps == nil {
			ps = &payerState{}
			st.Payers[p.AccountID] = ps
		}
		if kind == KindManual {
			ps.ManualRefreshedOn = today
		} else {
			ps.ScheduledRefreshedOn = today
		}
	}
}

// allPayersRefreshed reports whether every configured payer completed the
// planned refresh of this kind. A removed payer the options still name does
// not count.
func (a *Adapter) allPayersRefreshed(st *statusRecord, today finops.Date, kind Kind) bool {
	removed := a.removedPayers(st)
	for _, p := range a.options().Payers {
		if removed[p.AccountID] {
			continue
		}
		ps := st.Payers[p.AccountID]
		if ps == nil {
			return false
		}
		if kind == KindManual {
			if ps.ManualRefreshedOn != today {
				return false
			}
		} else if ps.ScheduledRefreshedOn != today {
			return false
		}
	}
	return true
}

// runCall executes one planned call and keeps its result in the run.
func (a *Adapter) runCall(ctx context.Context, st *statusRecord, g groupSpec, c finops.Call, m finops.Meter, r *run, failed map[string]bool, today finops.Date) (string, error) {
	p, ok := a.payer(g.payer)
	if !ok {
		return CodeUnavailable, ErrUnknownAccount
	}
	if !c.Required && failed[c.Group] {
		return "", errGroupFailed
	}
	pages := 0
	f := fetcher{payer: p.AccountID, env: a.env(p), meter: m, limiter: a.options().Limiter, pages: &pages}
	var res *grouped
	var err error
	switch c.Name {
	case callAccount:
		res, err = f.byAccount(ctx, g.window)
	case callUsage:
		res, err = f.byUsage(ctx, g.window)
	case callTeam:
		res, err = f.byTeam(ctx, g.window, a.options().TeamTagKey, p.Organization)
	case callRecord:
		res, err = f.byRecord(ctx, g.window)
	case callForecast:
		var fc *forecastRest
		if fc, err = f.forecast(ctx, today); err == nil {
			a.extras(r, p.AccountID).forecast = fc
		}
	case callCoverage:
		var cv *coverage
		if cv, err = f.coverage(ctx, window{today.AddDays(-coverageDays), today.AddDays(-1)}); err == nil {
			a.extras(r, p.AccountID).coverage = cv
		}
	case callResources:
		// The list is read again at run time: with no open increase left,
		// nothing is spent.
		var services []string
		if fn := a.options().ResourceServices; fn != nil {
			services = fn()
		}
		if len(services) == 0 {
			return "", nil
		}
		var rs *grouped
		rs, err = f.resources(ctx, window{today.AddDays(-resourceDays), today}, services)
		ex := a.extras(r, p.AccountID)
		ex.resources = rs
		if err != nil {
			ex.resCode = ErrorCode(err)
		}
	case callDrill:
		err = a.drillMovers(ctx, f, st, g, c.Requests, r, today)
	}
	if pages > 0 && isTable(c.Name) {
		if r.pages[p.AccountID] == nil {
			r.pages[p.AccountID] = map[string]pageStat{}
		}
		r.pages[p.AccountID][c.Name] = pageStat{Pages: pages, Days: g.window.days(), Done: err == nil}
	}
	if err != nil {
		return ErrorCode(err), err
	}
	if res != nil {
		if r.tables[c.Group] == nil {
			r.tables[c.Group] = map[string]*grouped{}
		}
		r.tables[c.Group][c.Name] = res
	}
	return "", nil
}

func (a *Adapter) extras(r *run, payer string) *payerExtras {
	if r.extras[payer] == nil {
		r.extras[payer] = &payerExtras{}
	}
	return r.extras[payer]
}

// drillMovers reads usage-type detail for the Organization's top movers:
// the account and service pairs whose newest complete day moved most
// against the same weekday a week earlier. Each drill is one or more paid
// requests through the meter.
func (a *Adapter) drillMovers(ctx context.Context, f fetcher, st *statusRecord, g groupSpec, limit int, r *run, today finops.Date) error {
	acct := r.tables[groupName(g.payer, g.mode)][callAccount]
	if acct == nil {
		return errGroupFailed
	}
	movers, err := a.movers(ctx, st, g.payer, acct, today)
	if err != nil {
		return err
	}
	if len(movers) > limit {
		movers = movers[:limit]
	}
	w := window{prevMonth(today), today}
	before := 0
	if f.pages != nil {
		before = *f.pages
	}
	defer func() {
		if f.pages != nil && len(movers) > 0 {
			if ps := st.Payers[g.payer]; ps != nil {
				recordDrillPages(ps, *f.pages-before, len(movers), w.days())
			}
		}
	}()
	for _, pair := range movers {
		account, service := splitPair(pair)
		res, err := f.drill(ctx, w, account, service, a.options().TeamTagKey)
		if err != nil {
			return err
		}
		if r.drills[g.payer] == nil {
			r.drills[g.payer] = map[string]*grouped{}
		}
		r.drills[g.payer][pair] = res
	}
	return nil
}

func (a *Adapter) movers(ctx context.Context, st *statusRecord, payer string, acct *grouped, today finops.Date) ([]string, error) {
	// Movers compare the newest settled day; days still settling are partial.
	complete := completeThrough(a.options().Now(), DaySettle)
	var newest finops.Date
	for d := range acct.Days {
		if !d.After(complete) && d.After(newest) {
			newest = d
		}
	}
	if newest.IsZero() {
		return nil, nil
	}
	now := map[string]int64{}
	for _, c := range acct.Days[newest].Cells {
		now[pairKey(c.A, c.B)] += c.M
	}
	before := map[string]int64{}
	weekAgo := newest.AddDays(-7)
	rows, err := a.cache.payerRows(ctx, payer, finops.DateRange{Start: weekAgo, End: weekAgo})
	if err != nil {
		return nil, err
	}
	for _, r := range rows[weekAgo] {
		if r.ResourceID == "" && r.RecordType == finops.RecordUsage {
			before[pairKey(r.Account, r.ServiceName)] += r.NetAmortizedMicros
		}
	}
	ps := st.Payers[payer]
	type mv struct {
		pair  string
		delta int64
	}
	var list []mv
	seen := map[string]bool{}
	for _, src := range []map[string]int64{now, before} {
		for k := range src {
			if seen[k] {
				continue
			}
			seen[k] = true
			if ps != nil {
				if last, ok := ps.Drilled[k]; ok && last.AddDays(drillCooldownDays).After(today) {
					continue
				}
			}
			d := now[k] - before[k]
			if d < 0 {
				d = -d
			}
			if d > 0 {
				list = append(list, mv{k, d})
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].delta != list[j].delta {
			return list[i].delta > list[j].delta
		}
		return list[i].pair < list[j].pair
	})
	out := make([]string, len(list))
	for i, m := range list {
		out[i] = m.pair
	}
	return out, nil
}

// commit writes committed day groups and the optional results, and records
// failures. It returns the committed groups. Each payer's days are written
// in one transaction: every committed group's days with their drill and
// resource detail.
func (a *Adapter) commit(ctx context.Context, st *statusRecord, pl planned, plan finops.Plan, out finops.RunOutcome, r *run, today finops.Date) ([]string, error) {
	now := a.options().Now()
	firstError := map[string]string{}
	for _, res := range out.Results {
		code := res.ErrorCode
		if code == "" && res.Skipped != "" && res.Call.Required {
			code = res.Skipped
		}
		if code != "" && res.Call.Required && firstError[res.Call.Group] == "" {
			firstError[res.Call.Group] = code
		}
	}
	for _, s := range plan.Skipped {
		if s.Call.Required && firstError[s.Call.Group] == "" {
			firstError[s.Call.Group] = finops.SkipCapMidRun
		}
	}
	capFailure := map[string]bool{}
	codeFor := map[string]string{}
	for g, code := range firstError {
		payer := pl.groups[g].payer
		if payer == "" {
			continue
		}
		if code == finops.SkipCapMidRun {
			capFailure[payer] = true
		} else if codeFor[payer] == "" {
			codeFor[payer] = code
		}
	}
	// A payer that ran at least one planned call was attempted; a payer
	// whose whole plan was dropped before its turn was not. The two carry
	// different labels: the first hit the cap, the second is deferred.
	attempted, capHit, retried := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, res := range out.Results {
		payer := pl.groups[res.Call.Group].payer
		if payer == "" {
			continue
		}
		if res.Skipped == "" {
			attempted[payer] = true
		}
		if res.ErrorCode == finops.SkipCapMidRun {
			capHit[payer] = true
		}
	}

	works := map[string]*payerWork{}
	work := func(payer string, ps *payerState) *payerWork {
		if w := works[payer]; w != nil {
			return w
		}
		w := a.newWork(payer, ps)
		works[payer] = w
		return w
	}
	var committed []string
	// Groups become committed only once their payer's days are flushed;
	// until then a failed write must not report progress that is not stored.
	pending := map[string][]string{}
	failedPayer := map[string]bool{}
	closed := map[string]bool{}
	groups := make([]string, 0, len(pl.groups))
	for g := range pl.groups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	removed := a.removedPayers(st)
	for _, g := range groups {
		spec := pl.groups[g]
		if removed[spec.payer] {
			continue
		}
		p, _ := a.payer(spec.payer)
		ps := st.Payers[spec.payer]
		if ps == nil {
			ps = &payerState{}
			st.Payers[spec.payer] = ps
		}
		if _, seen := retried[spec.payer]; !seen {
			retried[spec.payer] = !ps.LastAttempt.IsZero() && finops.DateOf(ps.LastAttempt.In(a.options().Location)) == today
		}
		ps.Organization = p.Organization
		ps.LastAttempt = now
		if !out.Commit[g] {
			failedPayer[spec.payer] = true
			continue
		}
		if err := a.writeGroup(ctx, work(spec.payer, ps), ps, spec, r.tables[g], completeThrough(a.options().Now(), DaySettle)); err != nil {
			return committed, err
		}
		pending[spec.payer] = append(pending[spec.payer], g)
		ps.LastSuccess = now
		readPrev := spec.mode == modeSetup && !spec.window.Start.After(prevMonth(today))
		if spec.mode == modeClose || (readPrev && finops.MonthClosed(prevMonth(today), today, billCloseDay)) {
			ps.closeMonth(prevMonth(today).MonthKey())
			ps.ClosedThrough = prevMonth(today).MonthKey()
			closed[spec.payer] = true
		}
	}
	for payer := range pending {
		if failedPayer[payer] {
			continue
		}
		if ps := st.Payers[payer]; ps != nil {
			ps.Stale, ps.ErrorCode = false, ""
		}
	}
	for payer := range failedPayer {
		ps := st.Payers[payer]
		if ps == nil {
			continue
		}
		ps.Stale = true
		switch {
		case capFailure[payer] && (capHit[payer] || (attempted[payer] && !retried[payer])):
			ps.ErrorCode = finops.SkipCapMidRun
		case codeFor[payer] != "":
			ps.ErrorCode = codeFor[payer]
		default:
			ps.ErrorCode = finops.SkipNotAttempted
		}
	}
	for payer, stats := range r.pages {
		if ps := st.Payers[payer]; ps != nil {
			if ps.Pages == nil {
				ps.Pages = map[string]pageStat{}
			}
			for call, stat := range stats {
				ps.Pages[call] = stat
			}
		}
	}
	for _, ps := range st.Payers {
		pruneDrilled(ps, today)
	}
	for _, payer := range sortedKeys(r.extras) {
		ps := st.Payers[payer]
		if ps == nil {
			continue
		}
		if err := a.writeResources(ctx, work(payer, ps), ps, r.extras[payer], today); err != nil {
			return committed, err
		}
	}
	for _, payer := range sortedKeys(r.drills) {
		ps := st.Payers[payer]
		if ps == nil {
			continue
		}
		if err := a.writeDrills(ctx, work(payer, ps), ps, r.drills[payer], today); err != nil {
			return committed, err
		}
	}
	for _, payer := range sortedKeys(works) {
		if _, err := works[payer].flush(ctx, now); err != nil {
			return committed, err
		}
		committed = append(committed, pending[payer]...)
	}
	for _, payer := range sortedKeys(r.extras) {
		if ps := st.Payers[payer]; ps != nil {
			if err := a.writeForecast(ctx, ps, payer, r.extras[payer], today); err != nil {
				return committed, err
			}
		}
	}
	if len(closed) > 0 {
		if err := a.cache.pruneLines(ctx, today); err != nil {
			return committed, err
		}
	}
	if err := a.cache.pruneDecisions(ctx, now); err != nil {
		return committed, err
	}
	return committed, nil
}

// purgeRemovedPayers deletes the cost lines and refresh state of every
// payer no longer configured as a cost source, every payer removed at a
// settings revision the options are not newer than, and every payer whose
// removal left its purge pending. A removal does not wait for a running
// refresh, so the tombstones and pending purges are read again from storage
// here: the refresh holding the replica lock purges a payer removed while it
// ran.
func (a *Adapter) purgeRemovedPayers(ctx context.Context, st *statusRecord) error {
	stored, err := a.cache.status()
	if err != nil {
		return err
	}
	mergeRemoved(st, stored.Removed)
	mergePending(st, stored.PurgePending)
	removed := a.removedPayers(st)
	configured := map[string]bool{}
	for _, p := range a.options().Payers {
		configured[p.AccountID] = true
	}
	// A payer that cannot be read now is still configured: losing access
	// never deletes the cost already stored for it.
	for _, u := range a.options().Unreachable {
		configured[u.AccountID] = true
	}
	// Cost data off means the configuration carries no payers; that is not
	// the same as every payer having been removed.
	if len(configured) == 0 && !a.options().enabled() {
		return nil
	}
	candidates := map[string]bool{}
	for payer := range st.Payers {
		candidates[payer] = true
	}
	for payer := range st.PurgePending {
		candidates[payer] = true
	}
	for _, payer := range sortedKeys(candidates) {
		if configured[payer] && !removed[payer] {
			// Added again at a newer revision: its data is live.
			markPurged(st, payer)
			continue
		}
		if err := a.removePayer(ctx, st, payer); err != nil {
			return err
		}
		delete(st.Payers, payer)
		markPurged(st, payer)
	}
	return nil
}

// payerAccounts lists the accounts the refresh state places under payer,
// the payer included, leaving out every account another payer lists: a
// decision stored without a payer on such an account cannot be told apart,
// and in an organization a removed member account is listed under its live
// payer. A payer with no state of its own names only itself, and only when
// no other payer lists it.
func payerAccounts(st *statusRecord, payer string) []string {
	shared := map[string]bool{}
	for other, ops := range st.Payers {
		if other == payer {
			continue
		}
		shared[other] = true
		if ops == nil {
			continue
		}
		for account := range ops.Accounts {
			shared[account] = true
		}
	}
	var out []string
	if !shared[payer] {
		out = append(out, payer)
	}
	if ps := st.Payers[payer]; ps != nil {
		for _, account := range sortedKeys(ps.Accounts) {
			if account != payer && !shared[account] {
				out = append(out, account)
			}
		}
	}
	return out
}

// lastPayer reports whether removing payer leaves the deployment without a
// payer: none other in the refresh state, none other configured.
func (a *Adapter) lastPayer(st *statusRecord, payer string) bool {
	for other := range st.Payers {
		if other != payer {
			return false
		}
	}
	removed := a.removedPayers(st)
	for _, p := range a.options().Payers {
		if p.AccountID != payer && !removed[p.AccountID] {
			return false
		}
	}
	return true
}

// removePayer deletes one payer's cost lines, increases and saving
// decisions, including decisions stored without a payer on an account st
// places under it.
func (a *Adapter) removePayer(ctx context.Context, st *statusRecord, payer string) error {
	store, err := a.cache.tables()
	if err != nil {
		return err
	}
	if err := a.checkLease(ctx); err != nil {
		return err
	}
	var errs []error
	if err := store.DeleteFinOpsPayer(ctx, a.cache.scope, payer); err != nil {
		errs = append(errs, fmt.Errorf("cost lines: %w", err))
	} else if _, err := store.DeleteFinOpsIncreasesForPayer(ctx, a.cache.scope, payer); err != nil {
		errs = append(errs, fmt.Errorf("increases: %w", err))
	} else if err := a.cache.deletePayerDecisions(ctx, store, payer, payerAccounts(st, payer), a.lastPayer(st, payer)); err != nil {
		errs = append(errs, fmt.Errorf("saving decisions: %w", err))
	}
	if len(errs) == 0 {
		return nil
	}
	joined := errors.Join(errs...)
	log.Printf("aws cost: payer %s only partly removed: %v", payer, joined)
	return joined
}

// purgePending deletes the data of every payer whose removal left its purge
// pending, without a refresh: it makes no AWS call and spends nothing, so it
// also runs when cost data is off or no payer is left. It skips when this
// process or another replica is doing cost work, which purges them itself,
// and reports how many payers it purged.
func (a *Adapter) purgePending(ctx context.Context) (int, error) {
	st, err := a.cache.status()
	if err != nil || len(st.PurgePending) == 0 {
		return 0, err
	}
	if !a.mu.TryLock() {
		return 0, nil
	}
	defer a.mu.Unlock()
	unlock, err := a.lockReplicas(ctx, 0)
	if errors.Is(err, ErrBusy) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer unlock()
	if st, err = a.cache.status(); err != nil {
		return 0, err
	}
	opts := a.options()
	if opts.SettingsAccounts == nil && !opts.enabled() {
		// Cost data is off and the settings' accounts were not read, so a
		// payer added again cannot be told from one still removed.
		return 0, nil
	}
	removed := a.removedPayers(st)
	listed := map[string]bool{}
	for _, p := range opts.Payers {
		listed[p.AccountID] = true
	}
	for _, u := range opts.Unreachable {
		listed[u.AccountID] = true
	}
	for _, account := range opts.SettingsAccounts {
		listed[account] = true
	}
	purged := 0
	for _, payer := range sortedKeys(st.PurgePending) {
		if listed[payer] && !removed[payer] {
			// Added again at a newer revision: its data is live.
			markPurged(st, payer)
			continue
		}
		if err := a.removePayer(ctx, st, payer); err != nil {
			return purged, err
		}
		delete(st.Payers, payer)
		markPurged(st, payer)
		purged++
	}
	return purged, a.saveStatus(st)
}

// MarkPayerRemoved records that payer stopped being a cost source. revision
// is the account settings revision read before the account was removed:
// options read at that revision or earlier may still name the payer, so it
// stays out of collection until options carry a newer one. The tombstone is
// stored in the status record, so it survives a restart and every replica
// sees it. It never waits for a refresh: a status written later by a
// refresh that read it earlier keeps the tombstone (see saveStatus).
func (a *Adapter) MarkPayerRemoved(_ context.Context, payer string, revision uint64) error {
	if !accountPattern.MatchString(payer) {
		return ErrInvalidAccount
	}
	a.forgetPayer(payer, revision)
	return a.updateStatus(func(stored *statusRecord) *statusRecord {
		mergeRemoved(stored, map[string]uint64{payer: revision})
		mergePending(stored, map[string]bool{payer: true})
		return stored
	})
}

// PurgePayer deletes a removed payer's cost lines, increases, saving
// decisions and refresh state. Mark it removed first. It waits at most wait
// for a refresh, drill-down or purge running in this process or on another
// replica, and holds no lock while it waits. purged is false when it did
// not get to run; the payer's tombstone is stored, so the refresh holding
// the lock, or the next one, purges it.
func (a *Adapter) PurgePayer(ctx context.Context, payer string, wait time.Duration) (purged bool, err error) {
	if !accountPattern.MatchString(payer) {
		return false, ErrInvalidAccount
	}
	unlock, err := a.lockForPurge(ctx, wait)
	if err != nil || unlock == nil {
		return false, err
	}
	defer unlock()
	st, err := a.cache.status()
	if err != nil {
		return false, err
	}
	if err := a.removePayer(ctx, st, payer); err != nil {
		return false, err
	}
	delete(st.Payers, payer)
	markPurged(st, payer)
	return true, a.saveStatus(st)
}

// lockForPurge takes the adapter lock and the replica lock without waiting
// on either while holding the other: the adapter lock is only tried, and the
// replica lock is tried once while it is held, as a refresh does. Between
// tries it holds nothing. It returns a nil unlock when wait passed first.
func (a *Adapter) lockForPurge(ctx context.Context, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		if a.mu.TryLock() {
			unlockReplicas, err := a.lockReplicas(ctx, 0)
			switch {
			case err == nil:
				return func() {
					unlockReplicas()
					a.mu.Unlock()
				}, nil
			case !errors.Is(err, ErrBusy):
				a.mu.Unlock()
				return nil, err
			}
			a.mu.Unlock()
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, nil
		}
		timer := time.NewTimer(min(replicaLockPoll, left))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil
		case <-timer.C:
		}
	}
}

// mergeRemoved adds tombstones to the status, keeping the newer revision of
// a payer named twice. Tombstones are never dropped.
func mergeRemoved(st *statusRecord, tombstones map[string]uint64) {
	if len(tombstones) == 0 {
		return
	}
	if st.Removed == nil {
		st.Removed = map[string]uint64{}
	}
	for payer, revision := range tombstones {
		st.Removed[payer] = max(st.Removed[payer], revision)
	}
}

// mergePending adds pending purges to the status.
func mergePending(st *statusRecord, pending map[string]bool) {
	for payer, ok := range pending {
		if !ok {
			continue
		}
		if st.PurgePending == nil {
			st.PurgePending = map[string]bool{}
		}
		st.PurgePending[payer] = true
	}
}

// markPurged records that payer's purge is done, so writing st clears it
// from the stored pending purges.
func markPurged(st *statusRecord, payer string) {
	delete(st.PurgePending, payer)
	if st.purged == nil {
		st.purged = map[string]bool{}
	}
	st.purged[payer] = true
}

// saveStatus stores the refresh state. A payer removal stores its tombstone
// and pending purge without the replica lock, so the stored ones are merged
// in first: a status read before a removal never drops them. Only the
// purges done since st was read are cleared.
func (a *Adapter) saveStatus(st *statusRecord) error {
	return a.updateStatus(func(stored *statusRecord) *statusRecord {
		mergeRemoved(st, stored.Removed)
		mergePending(st, stored.PurgePending)
		for payer := range st.purged {
			delete(st.PurgePending, payer)
		}
		if len(st.PurgePending) == 0 {
			st.PurgePending = nil
		}
		return st
	})
}

// updateStatus stores change applied to the stored status. On a backend
// with compare-and-swap the write only lands over the status change saw,
// and a lost race applies change again to the newer one, so writers on
// different replicas never lose each other's tombstones. A backend without
// it serves one process, where statusMu orders the writes.
func (a *Adapter) updateStatus(change func(stored *statusRecord) *statusRecord) error {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	key := statusKey(a.cache.scope.Write)
	cas, swaps := a.cache.provider.(storage.BlobCAS)
	for range statusSwapAttempts {
		raw, err := a.cache.provider.ReadBlob(key)
		if err != nil {
			return err
		}
		stored := &statusRecord{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, stored); err != nil {
				return err
			}
		} else {
			raw = nil
			if stored, err = a.cache.status(); err != nil {
				return err
			}
		}
		if stored.Payers == nil {
			stored.Payers = map[string]*payerState{}
		}
		data, err := json.Marshal(change(stored))
		if err != nil {
			return err
		}
		if !swaps {
			return a.cache.provider.WriteBlob(key, data)
		}
		swapped, err := cas.CompareAndSwapBlob(key, raw, data)
		if err != nil {
			return err
		}
		if swapped {
			return nil
		}
	}
	return errStatusContended
}

// payerWork collects one payer's day tables for one write: days read from
// the stored lines on first use, changed in place and stored together.
type payerWork struct {
	cache cache
	payer string
	opts  synthOptions
	// allowTeams keeps team tag values in the stored rows.
	allowTeams bool
	loaded     map[finops.Date]bool
	stored     map[finops.Date][]storage.FinOpsCostLine
	touched    map[finops.Date]*dayTables
	// lease checks the replica lock before the write.
	lease func(ctx context.Context) error
}

func (a *Adapter) newWork(payer string, ps *payerState) *payerWork {
	p, _ := a.payer(payer)
	return &payerWork{
		cache:      a.cache,
		payer:      payer,
		opts:       synthOptions{payer: payer, organization: ps.Organization || p.Organization, currency: ps.Currency},
		allowTeams: a.options().TagAllowlisted,
		loaded:     map[finops.Date]bool{},
		stored:     map[finops.Date][]storage.FinOpsCostLine{},
		touched:    map[finops.Date]*dayTables{},
		lease:      a.checkLease,
	}
}

// load reads the stored lines of the range's days not read yet.
func (w *payerWork) load(ctx context.Context, r finops.DateRange) error {
	missing := false
	for d := r.Start; !d.After(r.End); d = d.AddDays(1) {
		if !w.loaded[d] {
			missing = true
			break
		}
	}
	if !missing {
		return nil
	}
	rows, err := w.cache.payerRows(ctx, w.payer, r)
	if err != nil {
		return err
	}
	for d := r.Start; !d.After(r.End); d = d.AddDays(1) {
		if !w.loaded[d] {
			w.loaded[d] = true
			w.stored[d] = rows[d]
		}
	}
	return nil
}

// day returns the day's tables: changed ones, else rebuilt from the stored
// lines; nil when the day has none.
func (w *payerWork) day(ctx context.Context, d finops.Date) (*dayTables, error) {
	if t, ok := w.touched[d]; ok {
		return t, nil
	}
	if err := w.load(ctx, finops.DateRange{Start: d, End: d}); err != nil {
		return nil, err
	}
	rows := w.stored[d]
	if len(rows) == 0 {
		return nil, nil
	}
	return tablesOf(rows, w.opts.organization), nil
}

func (w *payerWork) put(d finops.Date, t *dayTables) { w.touched[d] = t }

// flush replaces every changed day in one transaction. wrote reports that
// the tables may have changed (also on an error).
func (w *payerWork) flush(ctx context.Context, refreshed time.Time) (wrote bool, err error) {
	if len(w.touched) == 0 {
		return false, nil
	}
	days := make([]finops.Date, 0, len(w.touched))
	for d := range w.touched {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	var rows []storage.FinOpsCostLine
	for _, d := range days {
		rows = append(rows, storedRows(d, w.touched[d], w.opts, refreshed, w.allowTeams)...)
	}
	if err := w.lease(ctx); err != nil {
		return false, err
	}
	if err := w.cache.replaceDays(ctx, w.payer, days, rows); err != nil {
		return true, err
	}
	w.touched = map[finops.Date]*dayTables{}
	for _, d := range days {
		delete(w.loaded, d)
	}
	return true, nil
}

// closed reports whether AWS closed the month's bill.
func (p *payerState) closed(month string) bool { return slices.Contains(p.ClosedMonths, month) }

// closeMonth freezes a month's days once AWS has closed its bill.
func (p *payerState) closeMonth(month string) {
	if !p.closed(month) {
		p.ClosedMonths = append(p.ClosedMonths, month)
		sort.Strings(p.ClosedMonths)
	}
	// Months past the line retention are gone; so is their mark.
	if len(p.ClosedMonths) > linesRetentionMonths+1 {
		p.ClosedMonths = p.ClosedMonths[len(p.ClosedMonths)-linesRetentionMonths-1:]
	}
}

// writeGroup replaces the daily tables of every day the group fetched. A
// manual refresh brings the account and usage tables only, so a day is
// written only when it already has the team and record tables. Closed
// months are never rewritten except by the month close itself.
// complete is the last day settled at commit time; Cost Explorer's
// Estimated flag (set on every day of the open month) never decides it.
func (a *Adapter) writeGroup(ctx context.Context, w *payerWork, ps *payerState, spec groupSpec, res map[string]*grouped, complete finops.Date) error {
	acct, usage := res[callAccount], res[callUsage]
	team, record := res[callTeam], res[callRecord]
	if acct == nil || usage == nil {
		return nil
	}
	if err := w.load(ctx, finops.DateRange{Start: spec.window.Start, End: spec.window.End}); err != nil {
		return err
	}
	currency := firstNonEmpty(acct.Currency, usage.Currency, ps.Currency)
	w.opts.currency = currency
	if ps.Accounts == nil {
		ps.Accounts = map[string]*accountState{}
	}
	for d := spec.window.Start; !d.After(spec.window.End); d = d.AddDays(1) {
		ad, ok := acct.Days[d]
		if !ok {
			continue
		}
		if ps.closed(d.MonthKey()) && spec.mode != modeClose {
			continue
		}
		old, err := w.day(ctx, d)
		if err != nil {
			return err
		}
		t := &dayTables{Account: ad.Cells, Usage: cellsOf(usage, d)}
		if team != nil && record != nil {
			t.Team, t.Record = cellsOf(team, d), nonUsage(cellsOf(record, d))
			if ps.Organization {
				t.TeamBy = teamByAccount
			}
		} else {
			if old == nil {
				continue
			}
			t.Team, t.Record, t.TeamBy = old.Team, old.Record, old.TeamBy
		}
		if old != nil {
			t.Drill, t.Resource = old.Drill, old.Resource
		}
		ps.applyPendingDrill(d, t)
		w.put(d, t)
		if !d.After(complete) && d.After(ps.LastCompleteDay) {
			ps.LastCompleteDay = d
		}
		if ps.HistoryStart.IsZero() || d.Before(ps.HistoryStart) {
			ps.HistoryStart = d
		}
		seen := func(account string, amount int64) {
			as := ps.Accounts[account]
			if as == nil {
				as = &accountState{}
				ps.Accounts[account] = as
			}
			if amount != 0 && (as.FirstCost.IsZero() || d.Before(as.FirstCost)) {
				as.FirstCost = d
			}
		}
		for _, c := range t.Account {
			seen(c.A, c.M)
		}
		for _, c := range cellsOf(record, d) {
			seen(c.B, c.M)
		}
	}
	for _, g := range []*grouped{acct, record} {
		if g == nil {
			continue
		}
		for id, n := range g.Names {
			if as := ps.Accounts[id]; as != nil {
				as.Name = n
			}
		}
	}
	ps.Currency = firstNonEmpty(currency, ps.Currency)
	return nil
}

// nonUsage keeps the record table's non-usage records: usage records are
// already in the account table and never become a line of their own.
func nonUsage(cells []cell) []cell {
	var out []cell
	for _, c := range cells {
		if !isUsageRecord(c.A) {
			out = append(out, c)
		}
	}
	return out
}

// writeForecast stores the forecast and coverage. The forecast covers today
// to month end; the days before today come from the cost tables.
func (a *Adapter) writeForecast(ctx context.Context, ps *payerState, payer string, ex *payerExtras, today finops.Date) error {
	if ex.forecast != nil {
		var mtd int64
		if today.After(today.MonthStart()) {
			store, err := a.cache.tables()
			if err != nil {
				return err
			}
			totals, err := store.FinOpsCostTotals(ctx, a.cache.scope, storage.FinOpsCostQuery{
				From: today.MonthStart().String(), Through: today.AddDays(-1).String(), Payer: payer, ResourceID: noResource})
			if err != nil {
				return err
			}
			for _, t := range totals {
				mtd += t.NetAmortizedMicros
			}
		}
		ps.Forecast = &finops.ProviderForecast{Month: ex.forecast.Month, Mean: mtd + ex.forecast.Mean, Low: mtd + ex.forecast.Low, High: mtd + ex.forecast.High}
		ps.ForecastOn = today
	}
	if ex.coverage != nil {
		ps.Coverage = ex.coverage
	}
	return nil
}

// writeResources records the resource-level outcome and puts resource
// detail on the days that have tables.
func (a *Adapter) writeResources(ctx context.Context, w *payerWork, ps *payerState, ex *payerExtras, today finops.Date) error {
	// Only an answer that resource-level data is not available turns it
	// off for a week; throttling, timeouts or the cap are a skip for today.
	if ex.resCode == CodeDenied || ex.resCode == CodeUnavailable {
		ps.ResourceData, ps.ResourceFailedOn = false, today
	}
	if ex.resources == nil {
		return nil
	}
	ps.ResourceData = true
	if w.opts.organization {
		return nil
	}
	days := make([]finops.Date, 0, len(ex.resources.Days))
	for d := range ex.resources.Days {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	if len(days) > 0 {
		if err := w.load(ctx, finops.DateRange{Start: days[0], End: days[len(days)-1]}); err != nil {
			return err
		}
	}
	for _, d := range days {
		t, err := w.day(ctx, d)
		if err != nil {
			return err
		}
		if t != nil {
			t.Resource = ex.resources.Days[d].Cells
			w.put(d, t)
		}
	}
	return nil
}

// writeDrills puts drill-down detail on the days whose tables carry the
// pair; detail for a day without them would add to no total and is not
// kept. Only an Organization's lines take drill detail: a single account's
// usage types are exact already.
func (a *Adapter) writeDrills(ctx context.Context, w *payerWork, ps *payerState, drills map[string]*grouped, today finops.Date) error {
	if ps.Drilled == nil {
		ps.Drilled = map[string]finops.Date{}
	}
	for _, pair := range sortedKeys(drills) {
		ps.Drilled[pair] = today
		if !w.opts.organization {
			continue
		}
		account, svc := splitPair(pair)
		days := make([]finops.Date, 0, len(drills[pair].Days))
		for d := range drills[pair].Days {
			days = append(days, d)
		}
		sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
		if len(days) > 0 {
			if err := w.load(ctx, finops.DateRange{Start: days[0], End: days[len(days)-1]}); err != nil {
				return err
			}
		}
		for _, d := range days {
			t, err := w.day(ctx, d)
			if err != nil {
				return err
			}
			if t == nil || !hasPair(t.Account, account, svc) {
				ps.pendDrill(d, pair, drills[pair].Days[d].Cells)
				continue
			}
			if t.Drill == nil {
				t.Drill = map[string][]cell{}
			}
			t.Drill[pair] = drills[pair].Days[d].Cells
			w.put(d, t)
		}
	}
	ps.prunePendingDrill(today)
	return nil
}

// pendDrill keeps drill detail for a day whose tables do not carry the
// pair yet.
func (p *payerState) pendDrill(d finops.Date, pair string, cells []cell) {
	if p.PendingDrill == nil {
		p.PendingDrill = map[string]map[string][]cell{}
	}
	if p.PendingDrill[d.String()] == nil {
		p.PendingDrill[d.String()] = map[string][]cell{}
	}
	p.PendingDrill[d.String()][pair] = cells
}

// applyPendingDrill puts the day's pending detail on its tables for every
// pair they carry.
func (p *payerState) applyPendingDrill(d finops.Date, t *dayTables) {
	pending := p.PendingDrill[d.String()]
	for pair, cells := range pending {
		account, svc := splitPair(pair)
		if !hasPair(t.Account, account, svc) {
			continue
		}
		if t.Drill == nil {
			t.Drill = map[string][]cell{}
		}
		t.Drill[pair] = cells
		delete(pending, pair)
	}
	if len(pending) == 0 {
		delete(p.PendingDrill, d.String())
	}
}

// prunePendingDrill drops pending detail older than the drill window.
func (p *payerState) prunePendingDrill(today finops.Date) {
	oldest := prevMonth(today).String()
	for day := range p.PendingDrill {
		if day < oldest {
			delete(p.PendingDrill, day)
		}
	}
}

func hasPair(cells []cell, account, svc string) bool {
	for _, c := range cells {
		if c.A == account && c.B == svc {
			return true
		}
	}
	return false
}

// freeCalls refreshes the tag status, AWS Budgets and Cost Optimization Hub
// recommendations. They cost nothing; a failure is recorded and the
// previous value kept.
func (a *Adapter) freeCalls(ctx context.Context, st *statusRecord) {
	for _, p := range a.options().Payers {
		ps := st.Payers[p.AccountID]
		if ps == nil {
			continue
		}
		ps.FreeErrors = nil
		note := func(name string, err error) {
			if ps.FreeErrors == nil {
				ps.FreeErrors = map[string]string{}
			}
			ps.FreeErrors[name] = ErrorCode(err)
		}
		f := fetcher{payer: p.AccountID, env: a.env(p), meter: a.meter, limiter: a.options().Limiter}
		if active, err := f.tagActive(ctx, a.options().TeamTagKey); err != nil {
			note("cost_allocation_tags", err)
		} else {
			ps.TagActive = &active
		}
		if bs, skipped, err := f.budgets(ctx, a.options().TeamTagKey); err != nil {
			note("budgets", err)
		} else {
			ps.Budgets, ps.SkippedBudgets = bs, skipped
		}
		if hub, err := f.recommendations(ctx, a.options().TeamTagKey, a.options().TagAllowlisted); err == nil {
			ps.Recommendations, ps.IdleFound = hub.Recommendations, hub.Idle
			ps.CommitmentMonthly, ps.CommitmentSuggestion = hub.CommitmentMonthly, hub.CommitmentSuggestion
			ps.RecommendationSource = "cost_optimization_hub"
		} else {
			note("cost_optimization_hub", err)
			// Not enrolled in the hub: Compute Optimizer for machines.
			lines, _ := a.Source().Lines(ctx, finops.DateRange{Start: a.options().today().AddDays(-7), End: a.options().today()})
			if recs, err := f.computeOptimizerRecommendations(ctx, computeRegions(lines), a.options().TeamTagKey, a.options().TagAllowlisted, a.options().NameAllowlisted); err != nil {
				note("compute_optimizer", err)
				ps.RecommendationSource = ""
			} else {
				ps.Recommendations, ps.IdleFound, ps.CommitmentMonthly, ps.CommitmentSuggestion = recs, nil, 0, ""
				ps.RecommendationSource = "compute_optimizer"
			}
		}
	}
}

// DrillDown reads usage-type detail for one account and service on click.
// It costs one request or more (pages) through the meter and is cached for
// the day: a second click the same day spends nothing.
func (a *Adapter) DrillDown(ctx context.Context, account, awsService string) error {
	if !a.options().enabled() {
		return ErrDisabled
	}
	if !accountPattern.MatchString(account) {
		return ErrInvalidAccount
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := a.lockReplicas(ctx, 0)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := a.cache.status()
	if err != nil {
		return err
	}
	today := a.options().today()
	// A payer removed while this drill-down waited, possibly on another
	// replica, is out: its tombstone is read from storage with the status.
	removed := a.removedPayers(st)
	var payer Payer
	found := false
	for _, p := range a.options().Payers {
		if removed[p.AccountID] {
			continue
		}
		if ps := st.Payers[p.AccountID]; ps != nil && ps.Accounts[account] != nil {
			payer, found = p, true
			break
		}
	}
	if !found {
		return ErrUnknownAccount
	}
	ps := st.Payers[payer.AccountID]
	pair := pairKey(account, awsService)
	w := window{prevMonth(today), today}
	cached, err := a.pairCached(ctx, payer.AccountID, account, awsService, w)
	if err != nil {
		return err
	}
	if !cached {
		return ErrUnknownPair
	}
	if ps.Drilled[pair] == today {
		return nil
	}
	if st.ScheduledOn != today {
		// Keep today's scheduled refresh whole: a click may only spend what
		// is left after it. A backfill only uses what is left, so it is
		// not reserved.
		remaining := a.meter.Remaining(ctx)
		if remaining == 0 {
			return finops.ErrCapReached
		}
		pl := planCalls(a.options(), st, a.removedPayers(st), KindScheduled, today, remaining)
		var calls []finops.Call
		for _, c := range pl.calls {
			if pl.groups[c.Group].mode != modeBackfill {
				calls = append(calls, c)
			}
		}
		plan := finops.BuildPlan(calls, remaining)
		if remaining-plan.Estimate < pageRate(ps, payer.Organization, callDrill, w.days()) {
			return ErrReservedForRefresh
		}
	}
	pages := 0
	f := fetcher{payer: payer.AccountID, env: a.env(payer), meter: a.meter, limiter: a.options().Limiter, pages: &pages}
	res, err := f.drill(ctx, w, account, awsService, a.options().TeamTagKey)
	if err != nil {
		if errors.Is(err, finops.ErrCapReached) || errors.Is(err, ErrBusy) {
			return err
		}
		return &Error{Code: ErrorCode(err)}
	}
	pruneDrilled(ps, today)
	recordDrillPages(ps, pages, 1, w.days())
	work := a.newWork(payer.AccountID, ps)
	if err := a.writeDrills(ctx, work, ps, map[string]*grouped{pair: res}, today); err != nil {
		return err
	}
	if _, err := work.flush(ctx, a.options().Now()); err != nil {
		return err
	}
	return a.saveStatus(st)
}

// pairCached reports whether the stored lines of the window hold the
// account and service.
func (a *Adapter) pairCached(ctx context.Context, payer, account, awsService string, w window) (bool, error) {
	store, err := a.cache.tables()
	if err != nil {
		return false, err
	}
	rows, err := store.FinOpsCostLines(ctx, a.cache.scope, storage.FinOpsCostQuery{
		From: w.Start.String(), Through: w.End.String(), Payer: payer, Account: account, RecordType: finops.RecordUsage, ResourceID: noResource})
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.ServiceName == awsService {
			return true, nil
		}
	}
	return false, nil
}

// pruneDrilled forgets drill-down days older than the cooldown; the detail
// itself stays on the cached days.
func pruneDrilled(ps *payerState, today finops.Date) {
	for k, d := range ps.Drilled {
		if d.Before(today.AddDays(-drillCooldownDays)) {
			delete(ps.Drilled, k)
		}
	}
}

// recordDrillPages learns the pages one drill-down takes for its window, so
// the drill reservation and the plan price drills from what AWS returned.
func recordDrillPages(ps *payerState, pages, drills, days int) {
	if pages <= 0 || drills <= 0 {
		return
	}
	if ps.Pages == nil {
		ps.Pages = map[string]pageStat{}
	}
	ps.Pages[callDrill] = pageStat{Pages: (pages + drills - 1) / drills, Days: days, Done: true}
}

func isTable(call string) bool {
	for _, c := range requiredCalls {
		if c == call {
			return true
		}
	}
	return false
}

func cellsOf(g *grouped, d finops.Date) []cell {
	if g == nil || g.Days[d] == nil {
		return nil
	}
	return g.Days[d].Cells
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
