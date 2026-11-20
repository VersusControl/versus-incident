package cost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fixtures/storagefix"
)

func rollupTestAdapter(t *testing.T, provider storage.Provider) *Adapter {
	t.Helper()
	a, err := New(provider, tenancy.DefaultOrgScope(), Options{
		Payers: []Payer{{AccountID: "111111111111", Credentials: testCreds}}, TagAllowlisted: true,
		Now: func() time.Time { return at("2026-11-09", 10) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func rollupsReconciled(t *testing.T, provider storage.Provider) bool {
	t.Helper()
	store, _ := storage.FinOps(provider)
	return store.(storage.FinOpsRollupStore).FinOpsRollupsReconciled()
}

// TestEachReplicaReconcilesItselfUnderTheLock: two server processes on one
// Postgres database. While the replica lock is held elsewhere a replica does
// not reconcile and keeps reading totals from the lines; once the lock is
// free each replica reconciles for itself, one after the other, trusting no
// other process's result.
func TestEachReplicaReconcilesItselfUnderTheLock(t *testing.T) {
	providers := storagefix.PostgresReplicas(t, 2)
	tables, _ := storage.FinOps(providers[0])
	days := []string{"2026-10-01", "2026-10-02"}
	var rows []storage.FinOpsCostLine
	for _, day := range days {
		rows = append(rows, storage.FinOpsCostLine{Payer: "111111111111", Day: day, Account: "111111111111", Service: "compute",
			RecordType: finops.RecordUsage, Currency: "USD", NetAmortizedMicros: 5_000_000})
	}
	if err := tables.ReplaceFinOpsCostDays(ctx, tenancy.DefaultOrgScope(), "111111111111", days, rows); err != nil {
		t.Fatal(err)
	}
	first, second := rollupTestAdapter(t, providers[0]), rollupTestAdapter(t, providers[1])

	unlock, held, err := providers[0].(storage.SessionLocker).TryLockSession(ctx, replicaLockKey)
	if err != nil || !held {
		t.Fatalf("hold the replica lock: %v %v", held, err)
	}
	busy, err := second.ReconcileRollups(ctx)
	unlock()
	if err != nil || busy.Done || busy.Compared || rollupsReconciled(t, providers[1]) {
		t.Fatalf("a replica reconciled beside the lock holder: %+v %v", busy, err)
	}
	for name, a := range map[string]*Adapter{"first": first, "second": second} {
		got, err := a.ReconcileRollups(ctx)
		if err != nil || !got.Done || !got.Compared {
			t.Fatalf("the %s replica did not reconcile for itself: %+v %v", name, got, err)
		}
	}
	if !rollupsReconciled(t, providers[0]) || !rollupsReconciled(t, providers[1]) {
		t.Fatal("a reconciled replica still reads totals from the lines")
	}
}

// TestScheduledRefreshReconcilesAgain: after this process reconciled, a
// writer that knows nothing of the rollups (an older replica during a
// rollout) moves cost between teams on a day the refresh does not rewrite.
// The next scheduled refresh reconciles again, so totals by team are the
// lines' own within one refresh cycle.
func TestScheduledRefreshReconcilesAgain(t *testing.T) {
	if storagefix.Backend() != "postgres" {
		t.Skip("a write around the rollups needs the database; " + storagefix.EnvBackend + "=postgres not set")
	}
	h := newHarness(t, loadScenario(t, "single-account"), nil)
	tables, _ := storage.FinOps(h.provider)
	scope := h.adapter.cache.scope
	const payer, day = "111111111111", "2026-01-15"
	if err := tables.ReplaceFinOpsCostDays(ctx, scope, payer, []string{day}, []storage.FinOpsCostLine{
		{Payer: payer, Day: day, Account: payer, Service: "compute", UsageType: "a", RecordType: finops.RecordUsage, Team: "alpha", Currency: "USD", NetAmortizedMicros: 5_000_000},
		{Payer: payer, Day: day, Account: payer, Service: "compute", UsageType: "b", RecordType: finops.RecordUsage, Team: "beta", Currency: "USD", NetAmortizedMicros: 3_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := h.adapter.ReconcileRollups(ctx); err != nil || !got.Done {
		t.Fatalf("first reconcile: %+v %v", got, err)
	}
	db := h.provider.(storage.SQLAccessor).DB()
	if _, err := db.Exec(`UPDATE finops_cost_lines SET team = 'alpha' WHERE payer = $1 AND day = $2 AND usage_type = 'b'`, payer, day); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(h.adapter, func() (bool, int, int, *time.Location) { return true, 0, 0, time.UTC }, nil, WithClock(h.clock.Now))
	if !s.RunDue(ctx) {
		t.Fatal("the scheduled refresh did not run")
	}
	noResource := ""
	got, err := tables.FinOpsCostTotals(ctx, scope, storage.FinOpsCostQuery{From: day, Through: day, ResourceID: &noResource}, storage.FinOpsByTeam)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Team != "alpha" || got[0].NetAmortizedMicros != 8_000_000 {
		t.Fatalf("totals by team after the refresh %+v, want all 8,000,000 on alpha as the lines say", got)
	}
}

// TestSchedulerTickReconcilesTheRollups: the background tick reconciles the
// rollups, so totals move from the lines to the rollups without a request
// waiting for it, also with cost data off.
func TestSchedulerTickReconcilesTheRollups(t *testing.T) {
	provider := storagefix.Provider(t)
	a := rollupTestAdapter(t, provider)
	s := NewScheduler(a, func() (bool, int, int, *time.Location) { return false, 0, 0, time.UTC }, nil)
	s.RunDue(ctx)
	if !rollupsReconciled(t, provider) {
		t.Fatal("the scheduler tick left totals reading the lines")
	}
	if again, err := a.ReconcileRollups(ctx); err != nil || !again.Done || again.Compared {
		t.Fatalf("a reconciled process reconciled again: %+v %v", again, err)
	}
}

// TestSchedulerReconcilesOncePerIntervalWithoutARefresh: with cost data off
// no refresh ever runs, yet the tick reconciles the rollups again once a
// refresh interval has passed, and not more often.
func TestSchedulerReconcilesOncePerIntervalWithoutARefresh(t *testing.T) {
	provider := storagefix.Provider(t)
	a := rollupTestAdapter(t, provider)
	now := at("2026-11-09", 10)
	s := NewScheduler(a, func() (bool, int, int, *time.Location) { return false, 0, 0, time.UTC }, nil,
		WithClock(func() time.Time { return now }))
	s.RunDue(ctx)
	first := s.reconciled
	if first.IsZero() {
		t.Fatal("the first tick did not reconcile")
	}
	now = now.Add(time.Hour)
	s.RunDue(ctx)
	if !s.reconciled.Equal(first) {
		t.Fatal("a tick within the refresh interval reconciled again")
	}
	now = first.Add(reconcileEvery)
	s.RunDue(ctx)
	if !s.reconciled.Equal(now) {
		t.Fatalf("a tick a refresh interval later did not reconcile: last %s", s.reconciled)
	}
}

// onFirstRequest runs hook before the first AWS request it carries.
type onFirstRequest struct {
	next aws.HTTPClient
	once sync.Once
	hook func()
}

func (c *onFirstRequest) Do(req *http.Request) (*http.Response, error) {
	c.once.Do(c.hook)
	return c.next.Do(req)
}

// endReplicaLockSession ends the database session that holds the replica
// lock, as an idle-session timeout or a dropped connection would.
func endReplicaLockSession(t *testing.T, provider storage.Provider) {
	t.Helper()
	key := uint64(replicaLockKey)
	var ended int
	err := provider.(storage.SQLAccessor).DB().QueryRow(`SELECT count(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND classid::bigint = $1 AND objid::bigint = $2 AND objsubid = 1`,
		int64(uint32(key>>32)), int64(uint32(key))).Scan(&ended)
	if err != nil || ended != 1 {
		t.Errorf("end the replica lock session: %d ended, err %v", ended, err)
	}
}

// TestFailedRefreshStillReconcilesAndALostLockWritesNothing: an older
// writer moves cost between teams after this process reconciled. The
// scheduled refresh then fails: the database ends the session holding the
// replica lock while the refresh calls AWS. The refresh writes no cost line
// and does not mark the day done, since another replica may hold the lock
// now; the same tick still reconciles the rollups, so totals by team are
// the lines' own without waiting for a successful refresh.
func TestFailedRefreshStillReconcilesAndALostLockWritesNothing(t *testing.T) {
	if storagefix.Backend() != "postgres" {
		t.Skip("ending the lock session needs the database; " + storagefix.EnvBackend + "=postgres not set")
	}
	var provider storage.Provider
	h := newHarness(t, loadScenario(t, "single-account"), func(o *Options) {
		o.HTTPClient = &onFirstRequest{next: o.HTTPClient, hook: func() { endReplicaLockSession(t, provider) }}
	})
	provider = h.provider
	tables, _ := storage.FinOps(h.provider)
	scope := h.adapter.cache.scope
	const payer, day = "111111111111", "2026-01-15"
	if err := tables.ReplaceFinOpsCostDays(ctx, scope, payer, []string{day}, []storage.FinOpsCostLine{
		{Payer: payer, Day: day, Account: payer, Service: "compute", UsageType: "a", RecordType: finops.RecordUsage, Team: "alpha", Currency: "USD", NetAmortizedMicros: 5_000_000},
		{Payer: payer, Day: day, Account: payer, Service: "compute", UsageType: "b", RecordType: finops.RecordUsage, Team: "beta", Currency: "USD", NetAmortizedMicros: 3_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := h.adapter.ReconcileRollups(ctx); err != nil || !got.Done {
		t.Fatalf("first reconcile: %+v %v", got, err)
	}
	db := h.provider.(storage.SQLAccessor).DB()
	if _, err := db.Exec(`UPDATE finops_cost_lines SET team = 'alpha' WHERE payer = $1 AND day = $2 AND usage_type = 'b'`, payer, day); err != nil {
		t.Fatal(err)
	}
	var logged []string
	s := NewScheduler(h.adapter, func() (bool, int, int, *time.Location) { return true, 0, 0, time.UTC }, nil,
		WithClock(h.clock.Now), WithLogger(func(format string, args ...any) { logged = append(logged, format) }))
	if s.RunDue(ctx) {
		t.Fatalf("the refresh succeeded after its lock session ended; log %q", logged)
	}
	if h.fake.requests() == 0 {
		t.Fatal("the refresh never reached AWS, so the lock was never lost mid-run")
	}
	var written int
	if err := db.QueryRow(`SELECT count(*) FROM finops_cost_lines WHERE payer = $1 AND day <> $2`, payer, day).Scan(&written); err != nil {
		t.Fatal(err)
	}
	if written != 0 {
		t.Fatalf("a refresh that lost the replica lock wrote %d cost lines", written)
	}
	if st := mustStatus(t, h); st.ScheduledOn == h.adapter.options().today() {
		t.Fatal("a refresh that lost the replica lock marked the day done")
	}
	noResource := ""
	got, err := tables.FinOpsCostTotals(ctx, scope, storage.FinOpsCostQuery{From: day, Through: day, ResourceID: &noResource}, storage.FinOpsByTeam)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Team != "alpha" || got[0].NetAmortizedMicros != 8_000_000 {
		t.Fatalf("totals by team after the failed refresh %+v, want all 8,000,000 on alpha as the lines say", got)
	}
}

// leaseProvider answers the replica lock check with a fixed result.
type leaseProvider struct {
	storage.Provider
	held bool
	err  error
}

func (p leaseProvider) SessionLockHeld(ctx context.Context, _ int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return p.held, p.err
}

// A lock check ended by the caller's own deadline or cancellation stays a
// timeout or cancellation, never a lost lock (which the API answers as a
// busy refresh); a check that cannot reach the session, or finds the lock
// gone, is a lost lock.
func TestLeaseCheckKeepsTheCallersCancellation(t *testing.T) {
	a := rollupTestAdapter(t, storagefix.Provider(t))
	base := a.cache.provider
	for _, c := range []struct {
		name     string
		provider leaseProvider
		ctx      func() context.Context
		want     error
		lost     bool
	}{
		{"held", leaseProvider{Provider: base, held: true}, context.Background, nil, false},
		{"gone", leaseProvider{Provider: base}, context.Background, ErrLockLost, true},
		{"session unreachable", leaseProvider{Provider: base, err: errors.New("connection reset")}, context.Background, ErrLockLost, true},
		{"caller cancelled", leaseProvider{Provider: base, held: true}, func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, context.Canceled, false},
		{"caller deadline", leaseProvider{Provider: base, held: true}, func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		}, context.DeadlineExceeded, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a.cache.provider = c.provider
			release := a.holdLease(func() {})
			defer release()
			err := a.checkLease(c.ctx())
			if !errors.Is(err, c.want) || (c.want == nil && err != nil) || errors.Is(err, ErrLockLost) != c.lost {
				t.Fatalf("lease check = %v, want %v (lost %v)", err, c.want, c.lost)
			}
		})
	}
}

// retentionAdapter is an adapter on 2026-11-09 with one payer and the given
// storage retention and read horizon (nil is the open-source 13).
func retentionAdapter(t *testing.T, provider storage.Provider, stored, visible func() int) *Adapter {
	t.Helper()
	a, err := New(provider, tenancy.DefaultOrgScope(), Options{
		Payers: []Payer{{AccountID: "111111111111", Credentials: testCreds}}, TagAllowlisted: true,
		Now:             func() time.Time { return at("2026-11-09", 10) },
		RetentionMonths: stored, VisibleMonths: visible,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// seedMonthlyLines stores one 5.00 line on the 15th of every month from
// 2024-09 to 2026-10, and the payer state that read them.
func seedMonthlyLines(t *testing.T, a *Adapter) storage.FinOpsStore {
	t.Helper()
	tables, ok := storage.FinOps(a.cache.provider)
	if !ok {
		t.Fatal("the provider has no cost tables")
	}
	const payer = "111111111111"
	var days []string
	var rows []storage.FinOpsCostLine
	for d := finops.MustDate("2024-09-15"); d.Before(finops.MustDate("2026-11-01")); d = d.MonthEnd().AddDays(15) {
		days = append(days, d.String())
		rows = append(rows, storage.FinOpsCostLine{Payer: payer, Day: d.String(), Account: payer, Service: "compute",
			RecordType: finops.RecordUsage, Currency: "USD", NetAmortizedMicros: 5_000_000})
	}
	if err := tables.ReplaceFinOpsCostDays(ctx, a.cache.scope, payer, days, rows); err != nil {
		t.Fatal(err)
	}
	st, err := a.cache.status()
	if err != nil {
		t.Fatal(err)
	}
	st.Payers[payer] = &payerState{HistoryStart: finops.MustDate("2024-09-15"), LastCompleteDay: finops.MustDate("2026-11-08"), Currency: "USD"}
	if err := a.cache.putStatus(st); err != nil {
		t.Fatal(err)
	}
	return tables
}

// storedMonths lists the months that still hold cost lines.
func storedMonths(t *testing.T, a *Adapter, tables storage.FinOpsStore) []string {
	t.Helper()
	lines, err := tables.FinOpsCostLines(ctx, a.cache.scope, storage.FinOpsCostQuery{From: "2000-01-01", Through: "2099-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range lines {
		out = append(out, l.Day[:7])
	}
	return out
}

func prune(t *testing.T, a *Adapter) {
	t.Helper()
	if err := a.cache.pruneLines(ctx, a.options().today(), a.options().retentionMonths()); err != nil {
		t.Fatal(err)
	}
}

// TestPruneFollowsTheStorageRetention: with 24 months of storage the prune
// keeps the months 14 to 24 back that the open-source 13 deletes.
func TestPruneFollowsTheStorageRetention(t *testing.T) {
	long := retentionAdapter(t, storagefix.Provider(t), func() int { return 24 }, nil)
	tables := seedMonthlyLines(t, long)
	prune(t, long)
	if got := storedMonths(t, long, tables); len(got) != 24 || got[0] != "2024-11" || got[23] != "2026-10" {
		t.Fatalf("24 months stored kept %v", got)
	}

	oss := retentionAdapter(t, storagefix.Provider(t), nil, nil)
	tables = seedMonthlyLines(t, oss)
	prune(t, oss)
	if got := storedMonths(t, oss, tables); len(got) != 13 || got[0] != "2025-10" {
		t.Fatalf("the open-source retention kept %v", got)
	}
}

// TestLicenseLapseDeletesNothing: storage stays at 24 months while the read
// horizon falls back to 13. The prune deletes nothing more, reads stop at 13
// months, and the older months show again when the horizon returns.
func TestLicenseLapseDeletesNothing(t *testing.T) {
	visible := 24
	a := retentionAdapter(t, storagefix.Provider(t), func() int { return 24 }, func() int { return visible })
	tables := seedMonthlyLines(t, a)
	prune(t, a)
	before := storedMonths(t, a, tables)
	status, err := a.Source().Status(ctx)
	if err != nil || status.HistoryStart != finops.MustDate("2024-09-15") {
		t.Fatalf("licensed history start %v %v", status.HistoryStart, err)
	}

	visible = 13
	prune(t, a)
	if after := storedMonths(t, a, tables); len(after) != len(before) {
		t.Fatalf("a lapse deleted months: %v, was %v", after, before)
	}
	st, _ := a.cache.status()
	ps := st.Payers["111111111111"]
	for _, m := range []string{"2024-11", "2024-12", "2025-01", "2025-02", "2025-03", "2025-04", "2025-05", "2025-06", "2025-07", "2025-08", "2025-09", "2025-10", "2025-11", "2025-12", "2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06", "2026-07", "2026-08", "2026-09", "2026-10"} {
		ps.closeMonth(m, a.options().retentionMonths())
	}
	if len(ps.ClosedMonths) != 24 || ps.ClosedMonths[0] != "2024-11" {
		t.Fatalf("closed-month marks under a lapse %v", ps.ClosedMonths)
	}
	status, err = a.Source().Status(ctx)
	if err != nil || status.HistoryStart != finops.MustDate("2025-10-01") {
		t.Fatalf("lapsed history start %v %v, want 2025-10-01", status.HistoryStart, err)
	}

	visible = 24
	if status, _ = a.Source().Status(ctx); status.HistoryStart != finops.MustDate("2024-09-15") {
		t.Fatalf("renewed history start %v", status.HistoryStart)
	}
}

// TestReconcileAfterTheRetentionPrune: the month totals the rollups answer
// after a prune and a reconcile are the kept lines' own.
func TestReconcileAfterTheRetentionPrune(t *testing.T) {
	for name, stored := range map[string]func() int{"open-source": nil, "24 months": func() int { return 24 }} {
		t.Run(name, func(t *testing.T) {
			a := retentionAdapter(t, storagefix.Provider(t), stored, nil)
			tables := seedMonthlyLines(t, a)
			if got, err := a.ReconcileRollups(ctx); err != nil || !got.Done {
				t.Fatalf("first reconcile %+v %v", got, err)
			}
			prune(t, a)
			if got, err := a.reconcileAfterRefresh(ctx); err != nil || !got.Done {
				t.Fatalf("reconcile after the prune %+v %v", got, err)
			}
			kept := storedMonths(t, a, tables)
			noResource := ""
			totals, err := tables.FinOpsCostTotals(ctx, a.cache.scope, storage.FinOpsCostQuery{From: "2000-01-01", Through: "2099-12-31", ResourceID: &noResource}, storage.FinOpsByMonth)
			if err != nil {
				t.Fatal(err)
			}
			if len(totals) != len(kept) || len(kept) != a.options().retentionMonths() {
				t.Fatalf("month totals %d, kept months %d, retention %d", len(totals), len(kept), a.options().retentionMonths())
			}
			for _, total := range totals {
				if total.Month < kept[0] || total.NetAmortizedMicros != 5_000_000 {
					t.Fatalf("month total %+v after keeping from %s", total, kept[0])
				}
			}
		})
	}
}

// TestRetentionDefaultsStayOpenSource: unset, short or reversed seams keep
// the open-source 13 months, and the read horizon never passes storage.
func TestRetentionDefaultsStayOpenSource(t *testing.T) {
	for _, tc := range []struct {
		stored, visible func() int
		wantStored      int
		wantVisible     int
	}{
		{nil, nil, 13, 13},
		{func() int { return 0 }, func() int { return -1 }, 13, 13},
		{func() int { return 6 }, func() int { return 12 }, 13, 13},
		{func() int { return 24 }, nil, 24, 13},
		{func() int { return 24 }, func() int { return 36 }, 24, 24},
		{nil, func() int { return 36 }, 13, 13},
	} {
		o := Options{RetentionMonths: tc.stored, VisibleMonths: tc.visible}
		if o.retentionMonths() != tc.wantStored || o.visibleMonths() != tc.wantVisible {
			t.Fatalf("retention %d visible %d, want %d %d", o.retentionMonths(), o.visibleMonths(), tc.wantStored, tc.wantVisible)
		}
	}
	p := &payerState{}
	for m := 1; m <= 12; m++ {
		p.closeMonth(fmt.Sprintf("2025-%02d", m), Options{}.retentionMonths())
	}
	for m := 1; m <= 10; m++ {
		p.closeMonth(fmt.Sprintf("2026-%02d", m), Options{}.retentionMonths())
	}
	if len(p.ClosedMonths) != 14 || p.ClosedMonths[0] != "2025-09" {
		t.Fatalf("open-source closed-month marks %v", p.ClosedMonths)
	}
}

// TestMonthCloseRefreshKeepsStoredMonthsUnderALapse: the prune a month-close
// refresh runs follows the storage retention, not the shown horizon. With 24
// months stored and 13 shown (a lapsed license), the scheduled refresh that
// closes October keeps the lines 14 to 24 months back and their closed-month
// marks.
func TestMonthCloseRefreshKeepsStoredMonthsUnderALapse(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.RetentionMonths = func() int { return 24 }
		o.VisibleMonths = func() int { return 13 }
	})
	h.clock.set(at("2026-11-04", 10))
	sc.PartialFrom = finops.MustDate("2026-11-01")
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("the provider has no cost tables")
	}
	const payer = "111111111111"
	var days []string
	var rows []storage.FinOpsCostLine
	for d := finops.MustDate("2024-11-15"); d.Before(finops.MustDate("2025-10-01")); d = d.MonthEnd().AddDays(15) {
		days = append(days, d.String())
		rows = append(rows, storage.FinOpsCostLine{Payer: payer, Day: d.String(), Account: payer, Service: "compute",
			RecordType: finops.RecordUsage, Currency: "USD", NetAmortizedMicros: 5_000_000})
	}
	if err := tables.ReplaceFinOpsCostDays(ctx, h.adapter.cache.scope, payer, days, rows); err != nil {
		t.Fatal(err)
	}
	// Every month before October was closed when it was read; the marks keep
	// closed months from being rewritten.
	st := mustStatus(t, h)
	for m := finops.MustDate("2024-11-01"); m.Before(finops.MustDate("2026-10-01")); m = m.MonthEnd().AddDays(1) {
		st.Payers[payer].closeMonth(m.MonthKey(), h.adapter.options().retentionMonths())
	}
	if err := h.adapter.cache.putStatus(st); err != nil {
		t.Fatal(err)
	}

	h.clock.set(at("2026-11-05", 10))
	sc.PartialFrom = finops.MustDate("2026-11-01")
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil || !slices.Contains(res.Committed, payer+"|close") {
		t.Fatalf("month close %+v %v", res.Committed, err)
	}
	old, err := tables.FinOpsCostLines(ctx, h.adapter.cache.scope, storage.FinOpsCostQuery{From: "2024-11-01", Through: "2025-09-30"})
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != len(rows) {
		t.Fatalf("the month-close prune kept %d of %d stored lines older than the shown 13 months", len(old), len(rows))
	}
	if ps := mustStatus(t, h).Payers[payer]; len(ps.ClosedMonths) != 24 || !ps.closed("2024-11") || !ps.closed("2026-10") {
		t.Fatalf("closed-month marks after the close %v, want the 24 months 2024-11 through 2026-10", ps.ClosedMonths)
	}
}
