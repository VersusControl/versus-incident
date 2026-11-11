package cost

import (
	"context"
	"errors"
	"net/http"
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
