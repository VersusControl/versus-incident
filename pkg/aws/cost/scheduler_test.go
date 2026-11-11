package cost

import (
	"errors"
	"fmt"
	"net/http"
	"os"
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

// A scheduled refresh whose only payer fails keeps the day open: the next
// tick retries that payer, and the first complete run saves the day.
func TestSchedulerRetriesAFailedPayerTheSameDay(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	s := NewScheduler(h.adapter,
		func() (bool, int, int, *time.Location) { return true, 9, 0, time.UTC },
		nil, WithClock(h.clock.Now))
	h.fake.failures[callAccount] = fakeFailure{Type: "ThrottlingException", Message: "Rate exceeded"}
	h.clock.set(at("2026-11-09", 10))
	if !s.RunDue(ctx) {
		t.Fatal("the scheduled refresh runs once its time has passed")
	}
	sent, used := h.fake.requests(), h.adapter.Meter().Used(ctx)
	if used == 0 {
		t.Fatal("the throttled call was paid and counted")
	}
	st, _ := h.adapter.Source().Status(ctx)
	if !st.Stale || st.StaleReason != CodeThrottled {
		t.Fatalf("the failure is named: stale %v %q", st.Stale, st.StaleReason)
	}
	if st := mustStatus(t, h); st.ScheduledOn != (finops.Date{}) {
		t.Fatal("a failed payer marked the day done")
	}

	delete(h.fake.failures, callAccount)
	h.clock.set(at("2026-11-09", 10).Add(time.Minute))
	if !s.RunDue(ctx) {
		t.Fatal("the next tick retries the failed payer")
	}
	if h.fake.requests() <= sent || h.adapter.Meter().Used(ctx) <= used {
		t.Fatalf("the retry spent nothing: sent %d -> %d, paid %d -> %d", sent, h.fake.requests(), used, h.adapter.Meter().Used(ctx))
	}
	ps := mustStatus(t, h).Payers["111111111111"]
	if ps == nil || !ps.hasData() {
		t.Fatal("the retry commits")
	}
	if st := mustStatus(t, h); st.ScheduledOn != h.adapter.options().today() {
		t.Fatal("the complete run saves the day")
	}
}

// postgresReplicas opens a harness on its own Postgres schema and a second
// adapter over the same storage, as a second replica of the deployment: it
// shares the database and the fake AWS endpoint and has its own meter and
// in-process locks. It skips when no database is set.
func postgresReplicas(t *testing.T, sc *scenario) (*harness, *Adapter) {
	t.Helper()
	if os.Getenv("TEST_POSTGRES_DSN") == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping the replica lock test")
	}
	t.Setenv(storagefix.EnvBackend, "postgres")
	h := newHarness(t, sc, nil)
	other, err := New(h.provider, h.adapter.cache.scope, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	return h, other
}

// TestTwoReplicasMakeOnlyOnePaidPlan: two replicas reach the refresh time on
// the same tick. One runs the day's plan; the other skips, either because
// the first holds the lock or because, once it has the lock, the day is
// already done. The paid requests are those of one run, and both meters
// report them.
func TestTwoReplicasMakeOnlyOnePaidPlan(t *testing.T) {
	h, other := postgresReplicas(t, loadScenario(t, "single-account"))
	single := newHarness(t, loadScenario(t, "single-account"), nil)
	if _, err := single.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	onePlan := paidCount(single.fake)
	if onePlan == 0 {
		t.Fatal("a single run spent nothing")
	}

	schedule := func() (bool, int, int, *time.Location) { return true, 9, 0, time.UTC }
	schedulers := []*Scheduler{
		NewScheduler(h.adapter, schedule, nil, WithClock(h.clock.Now)),
		NewScheduler(other, schedule, nil, WithClock(h.clock.Now)),
	}
	start := make(chan struct{})
	ran := make([]bool, len(schedulers))
	var wait sync.WaitGroup
	for i, s := range schedulers {
		wait.Add(1)
		go func(i int, s *Scheduler) {
			defer wait.Done()
			<-start
			ran[i] = s.RunDue(ctx)
		}(i, s)
	}
	close(start)
	wait.Wait()
	if ran[0] == ran[1] {
		t.Fatalf("replicas ran %v, want exactly one", ran)
	}
	if got := paidCount(h.fake); got != onePlan {
		t.Fatalf("two replicas sent %d paid requests, one plan is %d", got, onePlan)
	}
	for i, a := range []*Adapter{h.adapter, other} {
		if used := a.Meter().Used(ctx); used != onePlan {
			t.Fatalf("replica %d meter reads %d, want %d", i, used, onePlan)
		}
	}
	if st := mustStatus(t, h); st.ScheduledOn != h.adapter.options().today() {
		t.Fatal("the run did not save the day")
	}
	for i, s := range schedulers {
		if s.RunDue(ctx) {
			t.Fatalf("replica %d ran the day again", i)
		}
	}
	if got := paidCount(h.fake); got != onePlan {
		t.Fatalf("a later tick spent %d more", got-onePlan)
	}
}

// TestReplicaLockHeldElsewhereSpendsNothing: while another replica holds the
// cost lock, the scheduled tick is skipped, "Refresh now" and a drill-down
// answer ErrBusy, and nothing is sent. Once released, the tick runs.
func TestReplicaLockHeldElsewhereSpendsNothing(t *testing.T) {
	h, _ := postgresReplicas(t, loadScenario(t, "single-account"))
	locker, ok := h.provider.(storage.SessionLocker)
	if !ok {
		t.Fatalf("%T has no replica lock", h.provider)
	}
	unlock, acquired, err := locker.TryLockSession(ctx, replicaLockKey)
	if err != nil || !acquired {
		t.Fatalf("hold the lock: acquired=%v err=%v", acquired, err)
	}
	defer unlock()
	s := NewScheduler(h.adapter, func() (bool, int, int, *time.Location) { return true, 9, 0, time.UTC }, nil, WithClock(h.clock.Now))
	if s.RunDue(ctx) {
		t.Fatal("the tick ran while another replica held the lock")
	}
	if _, err := h.adapter.Refresh(ctx, KindManual); !errors.Is(err, ErrBusy) {
		t.Fatalf("manual refresh error = %v, want ErrBusy", err)
	}
	if err := h.adapter.DrillDown(ctx, "111111111111", "Amazon Elastic Compute Cloud - Compute"); !errors.Is(err, ErrBusy) {
		t.Fatalf("drill-down error = %v, want ErrBusy", err)
	}
	if sent := h.fake.requests(); sent != 0 {
		t.Fatalf("%d requests sent while the lock was held elsewhere", sent)
	}
	unlock()
	if !s.RunDue(ctx) {
		t.Fatal("the tick did not run once the lock was free")
	}
}

// gatedClient holds the first request until released, so a refresh can be
// caught mid-run while it holds its locks.
type gatedClient struct {
	next    aws.HTTPClient
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedClient(next aws.HTTPClient) *gatedClient {
	return &gatedClient{next: next, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedClient) Do(req *http.Request) (*http.Response, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.next.Do(req)
}

func (g *gatedClient) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never sent a request")
	}
}

// assertPurged checks that the payer's lines and state are gone and that
// its tombstone was kept with no purge left pending.
func assertPurged(t *testing.T, h *harness, sc *scenario, payer string) {
	t.Helper()
	lines, err := h.adapter.Source().Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("the removed payer still has %d lines", len(lines))
	}
	st := mustStatus(t, h)
	if st.Payers[payer] != nil {
		t.Fatal("the removed payer's refresh state is still stored")
	}
	if _, ok := st.Removed[payer]; !ok {
		t.Fatalf("the tombstone was dropped: %v", st.Removed)
	}
	if st.PurgePending[payer] {
		t.Fatal("the purge is still pending after it ran")
	}
}

// TestRemovalDuringARefreshDoesNotWaitAndThatRefreshPurges: a payer is
// removed while a refresh in this process holds the adapter lock. The purge
// gives up within its bound instead of waiting for the refresh, and leaves
// the purge pending; the running refresh then deletes the payer's data and
// stores its state without dropping the tombstone it never read.
func TestRemovalDuringARefreshDoesNotWaitAndThatRefreshPurges(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	const payer = "111111111111"
	gate := newGatedClient(h.fake)
	opts := h.opts
	opts.HTTPClient = gate
	if err := h.adapter.Update(opts); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-10", 10))
	refreshed := make(chan error, 1)
	go func() {
		_, err := h.adapter.Refresh(ctx, KindScheduled)
		refreshed <- err
	}()
	gate.waitEntered(t)

	if err := h.adapter.MarkPayerRemoved(ctx, payer, 0); err != nil {
		t.Fatal(err)
	}
	const wait = 300 * time.Millisecond
	start := time.Now()
	purged, err := h.adapter.PurgePayer(ctx, payer, wait)
	if elapsed := time.Since(start); err != nil || purged || elapsed > wait+time.Second {
		t.Fatalf("purge during a refresh = %v %v after %s; want deferred within %s", purged, err, elapsed, wait)
	}
	if st := mustStatus(t, h); !st.PurgePending[payer] {
		t.Fatal("the deferred purge is not recorded as pending")
	}

	close(gate.release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	assertPurged(t, h, sc, payer)
}

// TestRemovalWhileAnotherReplicaRefreshesIsPurgedByThatRefresh: another
// replica is refreshing, holding the replica lock, with options that still
// name the payer. Removing the payer here answers within the bound with the
// purge deferred, and this replica's adapter lock stays free meanwhile (a
// refresh here answers ErrBusy at once instead of queueing behind the
// purge). The other replica's refresh then purges the payer from the
// tombstone it finds in storage and keeps the tombstone when it stores its
// state.
func TestRemovalWhileAnotherReplicaRefreshesIsPurgedByThatRefresh(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h, _ := postgresReplicas(t, sc)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	const payer = "111111111111"
	gate := newGatedClient(h.fake)
	otherOpts := h.opts
	otherOpts.HTTPClient = gate
	other, err := New(h.provider, h.adapter.cache.scope, otherOpts)
	if err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-10", 10))
	refreshed := make(chan error, 1)
	go func() {
		_, err := other.Refresh(ctx, KindScheduled)
		refreshed <- err
	}()
	gate.waitEntered(t)

	if err := h.adapter.MarkPayerRemoved(ctx, payer, 0); err != nil {
		t.Fatal(err)
	}
	const wait = 2 * time.Second
	type outcome struct {
		purged  bool
		err     error
		elapsed time.Duration
	}
	purging := make(chan outcome, 1)
	go func() {
		start := time.Now()
		purged, err := h.adapter.PurgePayer(ctx, payer, wait)
		purging <- outcome{purged, err, time.Since(start)}
	}()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if _, err := h.adapter.Refresh(ctx, KindManual); !errors.Is(err, ErrBusy) {
		t.Fatalf("a refresh here while the other replica refreshes = %v, want ErrBusy", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a refresh here waited %s: the waiting purge held the adapter lock", elapsed)
	}
	got := <-purging
	if got.err != nil || got.purged || got.elapsed > wait+time.Second {
		t.Fatalf("purge while another replica refreshes = %v %v after %s; want deferred within %s", got.purged, got.err, got.elapsed, wait)
	}

	close(gate.release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	assertPurged(t, h, sc, payer)
}

// TestRemovalDeferredByAHeldLockIsPurgedByTheNextRefresh: another replica
// holds the cost lock without refreshing (a drill-down, say). The removal
// is deferred; the next refresh, on a replica whose options still name the
// payer, purges it from the pending purge and its tombstone.
func TestRemovalDeferredByAHeldLockIsPurgedByTheNextRefresh(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h, other := postgresReplicas(t, sc)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	const payer = "111111111111"
	unlock, acquired, err := h.provider.(storage.SessionLocker).TryLockSession(ctx, replicaLockKey)
	if err != nil || !acquired {
		t.Fatalf("hold the lock: acquired=%v err=%v", acquired, err)
	}
	defer unlock()
	if err := h.adapter.MarkPayerRemoved(ctx, payer, 0); err != nil {
		t.Fatal(err)
	}
	if purged, err := h.adapter.PurgePayer(ctx, payer, 300*time.Millisecond); err != nil || purged {
		t.Fatalf("purge while the lock is held = %v %v, want deferred", purged, err)
	}
	unlock()
	h.clock.set(at("2026-11-10", 10))
	if _, err := other.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	assertPurged(t, h, sc, payer)
}

// TestDeferredPurgeRunsForAPayerTheStatusNeverListed: the removed payer has
// cost lines but no refresh state (its state write was lost). The purge is
// deferred while this process is busy; the next refresh, on a replica whose
// options still name the payer, deletes the lines from the pending purge.
func TestDeferredPurgeRunsForAPayerTheStatusNeverListed(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	other, err := New(h.provider, h.adapter.cache.scope, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	const payer = "111111111111"
	tables, err := h.adapter.cache.tables()
	if err != nil {
		t.Fatal(err)
	}
	scope := h.adapter.cache.scope
	line := storage.FinOpsCostLine{Day: "2026-11-01", Account: payer, Service: "compute", ServiceName: "Amazon EC2", RecordType: "Usage", NetAmortizedMicros: 10}
	if err := tables.ReplaceFinOpsCostDays(ctx, scope, payer, []string{"2026-11-01"}, []storage.FinOpsCostLine{line}); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.MarkPayerRemoved(ctx, payer, 0); err != nil {
		t.Fatal(err)
	}
	h.adapter.mu.Lock()
	purged, err := h.adapter.PurgePayer(ctx, payer, 200*time.Millisecond)
	h.adapter.mu.Unlock()
	if err != nil || purged {
		t.Fatalf("purge while this process is busy = %v %v, want deferred", purged, err)
	}
	if _, err := other.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	lines, err := tables.FinOpsCostLines(ctx, scope, storage.FinOpsCostQuery{Payer: payer})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("the deferred purge never ran: %d lines left", len(lines))
	}
	if st := mustStatus(t, h); st.PurgePending[payer] || st.Payers[payer] != nil {
		t.Fatalf("after the purge: pending %v, state %v", st.PurgePending, st.Payers[payer])
	}
}

// TestRemovalDuringADrillDownKeepsItsTombstone: a drill-down read the
// refresh state before a payer was removed and stores it after. The
// removal did not wait for it, so the drill-down's write must keep the
// tombstone and the pending purge it never read.
func TestRemovalDuringADrillDownKeepsItsTombstone(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	const payer = "111111111111"
	gate := newGatedClient(h.fake)
	opts := h.opts
	opts.HTTPClient = gate
	if err := h.adapter.Update(opts); err != nil {
		t.Fatal(err)
	}
	drilled := make(chan error, 1)
	go func() { drilled <- h.adapter.DrillDown(ctx, payer, "Amazon Elastic Compute Cloud - Compute") }()
	gate.waitEntered(t)
	if err := h.adapter.MarkPayerRemoved(ctx, payer, 0); err != nil {
		t.Fatal(err)
	}
	if purged, err := h.adapter.PurgePayer(ctx, payer, 200*time.Millisecond); err != nil || purged {
		t.Fatalf("purge during a drill-down = %v %v, want deferred", purged, err)
	}
	close(gate.release)
	if err := <-drilled; err != nil {
		t.Fatal(err)
	}
	st := mustStatus(t, h)
	if _, ok := st.Removed[payer]; !ok || !st.PurgePending[payer] {
		t.Fatalf("the drill-down's write dropped the removal: tombstones %v, pending %v", st.Removed, st.PurgePending)
	}
}

// losingSwap stores and reads through the wrapped provider but loses every
// compare-and-swap, standing in for replicas that keep moving the count.
type losingSwap struct{ storage.Provider }

func (losingSwap) CompareAndSwapBlob(string, []byte, []byte) (bool, error) { return false, nil }

// TestLastPayerPurgeRunsOnTheTickWithCostDataOff: the last payer is removed
// while another holder keeps the cost lock past the bound, and cost data is
// then off with no payer left, so no refresh will run. The next scheduler
// tick purges the payer's lines, increases and decisions anyway, without a
// request to AWS and without spending from the meter.
func TestLastPayerPurgeRunsOnTheTickWithCostDataOff(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	const payer = "111111111111"
	tables, err := h.adapter.cache.tables()
	if err != nil {
		t.Fatal(err)
	}
	scope := h.adapter.cache.scope
	increase := storage.FinOpsIncrease{ID: "inc-1", Payer: payer, Account: payer, Service: "network.nat",
		StartedOn: "2026-11-01", Status: "open", MonthlyImpactMicros: 5_000_000, Suspects: []byte("{}"), Detail: []byte("{}")}
	if err := tables.PutFinOpsIncrease(ctx, scope, increase); err != nil {
		t.Fatal(err)
	}
	if err := tables.PutFinOpsSavingDecision(ctx, scope, storage.FinOpsSavingDecision{SavingID: "s-1", Payer: payer, Account: payer, State: "dismissed", At: h.clock.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	if err := h.adapter.MarkPayerRemoved(ctx, payer, 0); err != nil {
		t.Fatal(err)
	}
	h.adapter.mu.Lock()
	purged, err := h.adapter.PurgePayer(ctx, payer, 200*time.Millisecond)
	h.adapter.mu.Unlock()
	if err != nil || purged {
		t.Fatalf("purge while the lock is held = %v %v, want deferred", purged, err)
	}
	// prepare after the removal: the revision is newer than the tombstone,
	// cost data is off and the settings list no account.
	off := h.opts
	off.Payers, off.Connected = nil, nil
	off.SettingsRevision, off.SettingsAccounts = 1, []string{}
	off.Enabled = func() bool { return false }
	if err := h.adapter.Update(off); err != nil {
		t.Fatal(err)
	}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); !errors.Is(err, ErrDisabled) {
		t.Fatalf("refresh with cost data off = %v, want ErrDisabled", err)
	}
	sent, used := h.fake.requests(), h.adapter.Meter().Used(ctx)

	s := NewScheduler(h.adapter, func() (bool, int, int, *time.Location) { return false, 9, 0, time.UTC }, nil, WithClock(h.clock.Now))
	if s.RunDue(ctx) {
		t.Fatal("a refresh ran with cost data off")
	}
	if lines, err := tables.FinOpsCostLines(ctx, scope, storage.FinOpsCostQuery{Payer: payer}); err != nil || len(lines) != 0 {
		t.Fatalf("lines after the tick = %d %v", len(lines), err)
	}
	if increases, err := tables.FinOpsIncreases(ctx, scope, storage.FinOpsIncreaseQuery{}); err != nil || len(increases) != 0 {
		t.Fatalf("increases after the tick = %d %v", len(increases), err)
	}
	if decisions, err := tables.FinOpsSavingDecisions(ctx, scope); err != nil || len(decisions) != 0 {
		t.Fatalf("decisions after the tick = %d %v", len(decisions), err)
	}
	if st := mustStatus(t, h); st.PurgePending[payer] || st.Payers[payer] != nil {
		t.Fatalf("after the tick: pending %v, state %v", st.PurgePending, st.Payers[payer])
	}
	if h.fake.requests() != sent || h.adapter.Meter().Used(ctx) != used {
		t.Fatalf("the purge-only tick sent %d requests and spent %d", h.fake.requests()-sent, h.adapter.Meter().Used(ctx)-used)
	}
}

// TestDrillDownWithAContendedMeterAnswersBusy: the day's count keeps
// changing under other replicas, so the meter cannot reserve the drill's
// request. The drill-down answers ErrBusy (refresh_in_progress at the API)
// and sends nothing.
func TestDrillDownWithAContendedMeterAnswersBusy(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	h.adapter.meter.cache.provider = losingSwap{h.provider}
	sent := h.fake.requests()
	err := h.adapter.DrillDown(ctx, "111111111111", "Amazon Elastic Compute Cloud - Compute")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("drill-down with a contended meter = %v, want ErrBusy", err)
	}
	if h.fake.requests() != sent {
		t.Fatal("a request was sent without a reservation")
	}
}

// TestDrillDownSkipsAPayerRemovedOnAnotherReplica: another replica removed
// the payer; this replica's options still name it. The drill-down reads
// the tombstone from storage, answers the account as not covered and
// spends nothing.
func TestDrillDownSkipsAPayerRemovedOnAnotherReplica(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	other, err := New(h.provider, h.adapter.cache.scope, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.MarkPayerRemoved(ctx, "111111111111", 0); err != nil {
		t.Fatal(err)
	}
	sent, used := h.fake.requests(), h.adapter.Meter().Used(ctx)
	if err := h.adapter.DrillDown(ctx, "111111111111", "Amazon Elastic Compute Cloud - Compute"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("drill-down on a removed payer = %v, want ErrUnknownAccount", err)
	}
	if h.fake.requests() != sent || h.adapter.Meter().Used(ctx) != used {
		t.Fatalf("the drill-down on a removed payer sent %d requests", h.fake.requests()-sent)
	}
}

// TestPendingPurgeKeepsAPayerAddedAgainWhileCostDataIsOff: P is removed
// while the lock is held, so its purge is pending; P's account is then
// added again (a newer settings revision lists it), a saving of P is
// dismissed, and cost data is turned off, which leaves the options with no
// payers. The tick must not purge P: its new decision survives and the
// pending mark is cleared.
func TestPendingPurgeKeepsAPayerAddedAgainWhileCostDataIsOff(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	const payer = "111111111111"
	if err := h.adapter.MarkPayerRemoved(ctx, payer, 1); err != nil {
		t.Fatal(err)
	}
	h.adapter.mu.Lock()
	purged, err := h.adapter.PurgePayer(ctx, payer, 200*time.Millisecond)
	h.adapter.mu.Unlock()
	if err != nil || purged {
		t.Fatalf("purge while the lock is held = %v %v, want deferred", purged, err)
	}

	tables, err := h.adapter.cache.tables()
	if err != nil {
		t.Fatal(err)
	}
	scope := h.adapter.cache.scope
	decision := storage.FinOpsSavingDecision{SavingID: "s-after-readd", Payer: payer, Account: payer, State: "dismissed", Reason: "kept", At: h.clock.Now().UTC()}
	if err := tables.PutFinOpsSavingDecision(ctx, scope, decision); err != nil {
		t.Fatal(err)
	}
	// prepare with cost data off: no payers, but the settings at the newer
	// revision list the account added again.
	off := h.opts
	off.Payers, off.Connected = nil, nil
	off.SettingsRevision, off.SettingsAccounts = 3, []string{payer}
	off.Enabled = func() bool { return false }
	if err := h.adapter.Update(off); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(h.adapter, func() (bool, int, int, *time.Location) { return false, 9, 0, time.UTC }, nil, WithClock(h.clock.Now))
	s.RunDue(ctx)

	decisions, err := tables.FinOpsSavingDecisions(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].SavingID != decision.SavingID {
		t.Fatalf("the tick purged a payer added again: decisions %+v", decisions)
	}
	if lines, err := tables.FinOpsCostLines(ctx, scope, storage.FinOpsCostQuery{Payer: payer}); err != nil || len(lines) == 0 {
		t.Fatalf("the tick purged the lines of a payer added again: %d %v", len(lines), err)
	}
	if st := mustStatus(t, h); st.PurgePending[payer] {
		t.Fatal("the pending mark of a payer added again was not cleared")
	}
}

// TestMeterConflictDuringARefreshIsReportedAsBusy: the paid-request meter
// keeps losing its compare-and-swap during a refresh, so no call is sent.
// The run records refresh_in_progress, not a timeout.
func TestMeterConflictDuringARefreshIsReportedAsBusy(t *testing.T) {
	if got := ErrorCode(errMeterContended); got != "refresh_in_progress" {
		t.Fatalf("meter conflict code = %q, want refresh_in_progress", got)
	}
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	h.adapter.meter.cache.provider = losingSwap{h.provider}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if paid := paidCount(h.fake); paid != 0 {
		t.Fatalf("%d paid requests sent without a reservation", paid)
	}
	ps := mustStatus(t, h).Payers["111111111111"]
	if ps == nil || ps.ErrorCode != "refresh_in_progress" {
		t.Fatalf("the run records %+v, want refresh_in_progress", ps)
	}
}

// With cost data on but no account ready to read cost through, the
// scheduled tick says so; it never logs "cost data is off".
func TestScheduledTickNamesWhyCostIsNotRead(t *testing.T) {
	adapter, err := New(storage.NewMemory(), tenancy.DefaultOrgScope(), Options{Deployment: "default",
		Enabled: func() bool { return false }, DisabledReason: ReasonNoConnectedAccounts})
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	s := NewScheduler(adapter, func() (bool, int, int, *time.Location) { return true, 9, 0, time.UTC }, nil,
		WithClock(func() time.Time { return now }),
		WithLogger(func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }))
	if s.RunDue(ctx) {
		t.Fatal("a refresh ran with no account to read cost through")
	}
	want := "aws cost: scheduled refresh skipped: no account is ready to read cost through yet"
	if !slices.Contains(logs, want) {
		t.Fatalf("logs = %q, want %q", logs, want)
	}
}

// TestUnreachableRemovedPayerIsStillPurged (QA-411 T1): a payer the
// settings list as unreachable is kept as configured, so losing access
// never deletes its stored cost. A tombstone overrides that: a payer
// removed at a revision the options are not newer than is purged by the
// refresh purge and by the scheduler's pending purge, even while it is
// listed in Options.Unreachable. An unreachable payer with no effective
// tombstone keeps its data on both paths.
func TestUnreachableRemovedPayerIsStillPurged(t *testing.T) {
	const payer = "111111111111"
	unreachable := []UnreachablePayer{{AccountID: payer, Cause: "credentials_invalid", Since: at("2026-11-09", 10)}}
	// stored refreshes once so the payer has cost lines and refresh state.
	stored := func(t *testing.T) (*harness, storage.FinOpsStore, tenancy.OrgScope) {
		t.Helper()
		h := newHarness(t, loadScenario(t, "single-account"), nil)
		if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
			t.Fatal(err)
		}
		tables, err := h.adapter.cache.tables()
		if err != nil {
			t.Fatal(err)
		}
		return h, tables, h.adapter.cache.scope
	}
	lineCount := func(t *testing.T, h *harness, tables storage.FinOpsStore, scope tenancy.OrgScope) int {
		t.Helper()
		lines, err := tables.FinOpsCostLines(ctx, scope, storage.FinOpsCostQuery{Payer: payer})
		if err != nil {
			t.Fatal(err)
		}
		return len(lines)
	}
	// update applies options that name the payer only as unreachable, read
	// at revision. enabled keeps cost data on so a refresh still runs.
	update := func(t *testing.T, h *harness, revision uint64, enabled bool) {
		t.Helper()
		o := h.opts
		o.Payers, o.Connected = nil, nil
		o.Unreachable = unreachable
		o.SettingsRevision, o.SettingsAccounts = revision, []string{}
		if !enabled {
			o.Enabled, o.DisabledReason = func() bool { return false }, ReasonNoConnectedAccounts
		}
		if err := h.adapter.Update(o); err != nil {
			t.Fatal(err)
		}
	}
	purged := func(t *testing.T, h *harness, tables storage.FinOpsStore, scope tenancy.OrgScope) {
		t.Helper()
		if n := lineCount(t, h, tables, scope); n != 0 {
			t.Fatalf("the removed unreachable payer still has %d cost lines", n)
		}
		st := mustStatus(t, h)
		if st.Payers[payer] != nil {
			t.Fatal("the removed unreachable payer's refresh state is still stored")
		}
		if _, ok := st.Removed[payer]; !ok {
			t.Fatalf("the tombstone was dropped: %v", st.Removed)
		}
		if st.PurgePending[payer] {
			t.Fatal("the purge is still pending after it ran")
		}
	}
	kept := func(t *testing.T, h *harness, tables storage.FinOpsStore, scope tenancy.OrgScope, before int) {
		t.Helper()
		if n := lineCount(t, h, tables, scope); n != before {
			t.Fatalf("an unreachable payer lost cost lines: %d, want %d", n, before)
		}
		st := mustStatus(t, h)
		if st.Payers[payer] == nil {
			t.Fatal("an unreachable payer lost its refresh state")
		}
		if st.PurgePending[payer] {
			t.Fatal("the pending mark of a kept payer was not cleared")
		}
	}

	t.Run("refresh purges a removed unreachable payer", func(t *testing.T) {
		h, tables, scope := stored(t)
		if err := h.adapter.MarkPayerRemoved(ctx, payer, 5); err != nil {
			t.Fatal(err)
		}
		update(t, h, 5, true)
		h.clock.set(at("2026-11-10", 10))
		if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
			t.Fatal(err)
		}
		purged(t, h, tables, scope)
	})
	t.Run("refresh keeps an unreachable payer without a tombstone", func(t *testing.T) {
		h, tables, scope := stored(t)
		before := lineCount(t, h, tables, scope)
		if before == 0 {
			t.Fatal("no cost lines stored before the check")
		}
		update(t, h, 5, true)
		h.clock.set(at("2026-11-10", 10))
		if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
			t.Fatal(err)
		}
		kept(t, h, tables, scope, before)
	})
	t.Run("pending purge removes a removed unreachable payer", func(t *testing.T) {
		h, tables, scope := stored(t)
		if err := h.adapter.MarkPayerRemoved(ctx, payer, 5); err != nil {
			t.Fatal(err)
		}
		update(t, h, 5, false)
		n, err := h.adapter.purgePending(ctx)
		if err != nil || n != 1 {
			t.Fatalf("pending purge = %d %v, want 1 payer purged", n, err)
		}
		purged(t, h, tables, scope)
	})
	t.Run("pending purge keeps an unreachable payer whose tombstone is superseded", func(t *testing.T) {
		h, tables, scope := stored(t)
		before := lineCount(t, h, tables, scope)
		if before == 0 {
			t.Fatal("no cost lines stored before the check")
		}
		// Removed at revision 1, listed again (now unreachable) at revision 2.
		if err := h.adapter.MarkPayerRemoved(ctx, payer, 1); err != nil {
			t.Fatal(err)
		}
		update(t, h, 2, false)
		n, err := h.adapter.purgePending(ctx)
		if err != nil || n != 0 {
			t.Fatalf("pending purge = %d %v, want nothing purged", n, err)
		}
		kept(t, h, tables, scope, before)
	})
}
