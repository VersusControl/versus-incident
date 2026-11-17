package cost

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// failPayerProvider makes one payer's day write fail, so a refresh stops
// after an earlier payer has already committed.
type failPayerProvider struct {
	storage.Provider
	storage.FinOpsStore
	failPayer string
	failing   atomic.Bool
	calls     atomic.Int64
	failed    atomic.Int64
}

func (p *failPayerProvider) ReplaceFinOpsCostDays(ctx context.Context, scope tenancy.OrgScope, payer string, days []string, lines []storage.FinOpsCostLine) error {
	p.calls.Add(1)
	if payer == p.failPayer && p.failing.Load() {
		p.failed.Add(1)
		return errors.New("storage: day write failed")
	}
	return p.FinOpsStore.ReplaceFinOpsCostDays(ctx, scope, payer, days, lines)
}

func TestPartialRefreshPersistsProgressAndRetrySkipsCompletedPayers(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.Payers = []Payer{
			{AccountID: "111111111111", Credentials: credsFor("AKIDONE")},
			{AccountID: "222222222222", Credentials: credsFor("AKIDTWO")},
		}
		o.MaxPaidRequests = 50
	})
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	store := &failPayerProvider{Provider: h.provider, FinOpsStore: tables, failPayer: "222222222222"}
	store.failing.Store(true)
	h.provider = store
	a, err := New(store, h.adapter.cache.scope, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	h.adapter = a

	if _, err := h.adapter.Refresh(ctx, KindScheduled); err == nil {
		t.Fatalf("the second payer's write fails; calls %d failed %d", store.calls.Load(), store.failed.Load())
	}
	count := func(payer string) int {
		rows, err := store.FinOpsCostLines(ctx, h.adapter.cache.scope, storage.FinOpsCostQuery{Payer: payer})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if count("111111111111") == 0 {
		t.Fatal("the first payer's committed day is stored")
	}
	if count("222222222222") != 0 {
		t.Fatal("the failed payer has no lines yet")
	}
	st := mustStatus(t, h)
	if st.ScheduledOn == h.adapter.options().today() {
		t.Fatal("a partial refresh must not end the day's retries")
	}
	if ps := st.Payers["111111111111"]; ps == nil || ps.ScheduledRefreshedOn != h.adapter.options().today() {
		t.Fatalf("the completed payer's progress is kept: %+v", ps)
	}

	store.failing.Store(false)
	first, second := h.fake.paidSigned("AKIDONE"), h.fake.paidSigned("AKIDTWO")
	used := h.adapter.Meter().Used(ctx)
	h.clock.set(h.clock.Now().Add(time.Minute))
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if h.fake.paidSigned("AKIDONE") != first {
		t.Fatal("the retry paid for the completed payer again")
	}
	if h.fake.paidSigned("AKIDTWO") == second {
		t.Fatal("the retry did not resume the missing payer")
	}
	if delta := h.adapter.Meter().Used(ctx) - used; delta != 6 {
		t.Fatalf("the retry took %d paid requests, want 6 (the missing payer only)", delta)
	}
	if len(res.Committed) != 1 || res.Committed[0] != "222222222222|setup" {
		t.Fatalf("retry committed %v", res.Committed)
	}
	if count("222222222222") == 0 {
		t.Fatal("the payer is stranded")
	}
	if st := mustStatus(t, h); st.ScheduledOn != h.adapter.options().today() {
		t.Fatalf("a complete retry saves the day: %+v", st.ScheduledOn)
	}
}

func TestStoredRowsRedactTeamsWhenNotAllowlisted(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) { o.TagAllowlisted = false })
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	store, _ := storage.FinOps(h.provider)
	rows, err := store.FinOpsCostLines(ctx, h.adapter.cache.scope, storage.FinOpsCostQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("the refresh stored no rows")
	}
	for _, r := range rows {
		if r.Team != "" {
			t.Fatalf("a team value reached storage: %+v", r)
		}
	}
	var stored int64
	for _, r := range rows {
		if r.ResourceID == "" {
			stored += r.NetAmortizedMicros
		}
	}
	want := sumRows(sc.rows(), nil)
	if stored != want {
		t.Fatalf("stored totals %d, want %d", stored, want)
	}
	lines, err := h.adapter.Source().Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	if got := sumLines(lines, nil); got != want {
		t.Fatalf("read totals %d, want %d", got, want)
	}
}

func TestRefreshAfterPayerRemovalDoesNotRestoreIt(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	// prepare loaded the live configuration with the payer before the
	// account was deleted.
	if err := h.adapter.Update(h.opts); err != nil {
		t.Fatal(err)
	}
	if err := h.removePayer("111111111111", 0); err != nil {
		t.Fatal(err)
	}
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) != 0 {
		t.Fatalf("a deleted payer was refreshed again: %v", res.Committed)
	}
	lines, err := h.adapter.Source().Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("a deleted payer's lines came back: %d", len(lines))
	}
}

func TestStaleUpdateKeepsRemovedPayerOut(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	// prepare read the settings at revision 4, while the account was still
	// a cost source.
	stale := h.opts
	stale.SettingsRevision = 4

	// The operator deletes the account at the same revision: its cost data
	// is purged and the adapter is told, while prepare's snapshot is still in
	// flight.
	if err := h.removePayer("111111111111", 4); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.Update(stale); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-10", 10))
	assertPayerOut(t, h, sc, "a stale update")

	// Adding the account back raises the revision; options read after it
	// make the payer collectable again.
	readded := h.opts
	readded.SettingsRevision = 6
	if err := h.adapter.Update(readded); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-11", 10))
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) == 0 {
		t.Fatal("a re-added payer is never collected again")
	}
	if lines, err := h.adapter.Source().Lines(ctx, sc.Range); err != nil || len(lines) == 0 {
		t.Fatalf("a re-added payer has no cost lines: %d, %v", len(lines), err)
	}

	// An older snapshot arriving after the re-add still cannot stand for a
	// read after the removal.
	if err := h.adapter.Update(stale); err != nil {
		t.Fatal(err)
	}
	if removed := h.adapter.removedPayers(mustStatus(t, h)); !removed["111111111111"] {
		t.Fatal("an older snapshot after the re-add cleared the removal")
	}
}

// TestBackwardClockStepKeepsRemovedPayerOut steps the wall clock back
// between the settings read and the delete, by more than the gap between
// them, as an NTP correction or a resumed host can. The removal is ordered by
// the settings revision, not by time, so the stale snapshot still cannot
// bring the payer back.
func TestBackwardClockStepKeepsRemovedPayerOut(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	read := at("2026-11-09", 12)
	h.clock.set(read)
	stale := h.opts
	stale.SettingsRevision = 9

	h.clock.set(read.Add(-6 * time.Hour))
	if err := h.removePayer("111111111111", 9); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.Update(stale); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-10", 10))
	assertPayerOut(t, h, sc, "a stale update after a backward clock step")
}

// TestRemovedPayerSurvivesRestart builds a fresh adapter over the same
// store, as a restarted process does. The removal is read back from the
// status record: options at the removal's revision keep the payer out, and
// options at a newer revision bring it back.
func TestRemovedPayerSurvivesRestart(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if err := h.removePayer("111111111111", 3); err != nil {
		t.Fatal(err)
	}
	if got := mustStatus(t, h).Removed["111111111111"]; got != 3 {
		t.Fatalf("the removal is not stored with its revision: %d", got)
	}

	stale := h.opts
	stale.SettingsRevision = 3
	restarted, err := New(h.provider, tenancy.DefaultOrgScope(), stale)
	if err != nil {
		t.Fatal(err)
	}
	h.adapter = restarted
	h.clock.set(at("2026-11-10", 10))
	assertPayerOut(t, h, sc, "options read before the removal, after a restart,")

	readded := h.opts
	readded.SettingsRevision = 4
	if err := h.adapter.Update(readded); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-11", 10))
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) == 0 {
		t.Fatal("a payer re-added after the restart is never collected")
	}
}

// assertPayerOut refreshes and requires the removed payer to stay out: no
// call planned, nothing committed, no line read back.
func assertPayerOut(t *testing.T, h *harness, sc *scenario, what string) {
	t.Helper()
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plan.Run) != 0 || len(res.Committed) != 0 {
		t.Fatalf("%s revived the deleted payer: %d calls, %v", what, len(res.Plan.Run), res.Committed)
	}
	lines, err := h.adapter.Source().Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("%s rewrote the deleted payer's %d lines", what, len(lines))
	}
}

func TestPayerRemovalDeletesIncreasesAndIsIdempotent(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	store, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	scope := h.adapter.cache.scope
	// The row is written the way PutIncrease writes it: the whole record in
	// Detail, so a read gives the key back and the page can match it to the
	// account's cost.
	detail, err := json.Marshal(finops.IncreaseRecord{
		Detected: finops.Detected{ID: "inc-1", Key: finops.LineKey{Account: "111111111111", Service: "network.nat"},
			StartedOn: finops.MustDate("2026-11-01"), MonthlyImpact: 5_000_000},
		Status: finops.StatusOpen,
	})
	if err != nil {
		t.Fatal(err)
	}
	increase := storage.FinOpsIncrease{
		ID: "inc-1", Payer: "111111111111", Account: "111111111111", Service: "network.nat",
		StartedOn: "2026-11-01", Status: "open", MonthlyImpactMicros: 5_000_000,
		Suspects: json.RawMessage("{}"), Detail: detail,
		Events: []storage.FinOpsIncreaseEvent{{At: at("2026-11-02", 10), Actor: "alice", FromStatus: "open", ToStatus: "investigating"}},
	}
	if err := store.PutFinOpsIncrease(ctx, scope, increase); err != nil {
		t.Fatal(err)
	}
	if err := store.PutFinOpsSavingDecision(ctx, scope, storage.FinOpsSavingDecision{SavingID: "inc-1", State: "dismissed", Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutFinOpsSavingDecision(ctx, scope, storage.FinOpsSavingDecision{SavingID: "s-keep", State: "open"}); err != nil {
		t.Fatal(err)
	}
	svc := finops.NewService(h.adapter.Source(), NewStore(h.provider, scope), finops.Config{Now: h.clock.Now})
	before, err := svc.Increases(ctx, finops.Query{Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Items) == 0 {
		t.Fatal("the payer has no increase listed before the delete")
	}

	if err := h.removePayer("111111111111", 0); err != nil {
		t.Fatal(err)
	}
	lines, err := store.FinOpsCostLines(ctx, scope, storage.FinOpsCostQuery{Payer: "111111111111"})
	if err != nil || len(lines) != 0 {
		t.Fatalf("lines left %d, %v", len(lines), err)
	}
	if _, ok := mustStatus(t, h).Payers["111111111111"]; ok {
		t.Fatal("the payer's drill and refresh state is left")
	}
	incs, err := store.FinOpsIncreases(ctx, scope, storage.FinOpsIncreaseQuery{})
	if err != nil || len(incs) != 0 {
		t.Fatalf("increases or events left: %+v %v", incs, err)
	}
	after, err := svc.Increases(ctx, finops.Query{Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Items) != 0 {
		t.Fatalf("the deleted payer is still listed: %+v", after.Items)
	}

	// A repeated delete is an idempotent no-op.
	if err := h.removePayer("111111111111", 0); err != nil {
		t.Fatalf("second removal: %v", err)
	}
}

func TestCapLabelIsPerPayerAndDeterministic(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.Payers = []Payer{
			{AccountID: "111111111111", Credentials: credsFor("AKIDONE")},
			{AccountID: "222222222222", Credentials: credsFor("AKIDTWO")},
		}
		o.MaxPaidRequests = 4
	})
	// Before the bill close day, so the first setup does not close October.
	h.clock.set(at("2026-11-04", 10))
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-09", 10))
	primer := mustStatus(t, h)
	primer.Payers["111111111111"].HistoryStart = prevMonth(h.adapter.options().today())
	if err := h.adapter.cache.putStatus(primer); err != nil {
		t.Fatal(err)
	}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	st := mustStatus(t, h)
	if ps := st.Payers["111111111111"]; ps == nil || ps.ErrorCode != finops.SkipCapMidRun {
		t.Fatalf("the payer that hit the cap is labelled %+v", ps)
	}
	if ps := st.Payers["222222222222"]; ps == nil || ps.ErrorCode != finops.SkipNotAttempted {
		t.Fatalf("the deferred payer is labelled %+v", ps)
	}
}

func TestUnrefreshedPayerKeepsTheDayOpenAndIsRetried(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.Payers = []Payer{
			{AccountID: "111111111111", Credentials: credsFor("AKIDONE")},
			{AccountID: "222222222222", Credentials: credsFor("AKIDTWO")},
		}
		o.MaxPaidRequests = 50
	})
	h.fake.denyPayer = "AKIDTWO"
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if st := mustStatus(t, h); st.ScheduledOn == h.adapter.options().today() {
		t.Fatal("a refresh with a failed payer marked the day done")
	}
	done := h.fake.paidSigned("AKIDONE")
	h.fake.denyPayer = ""
	s := NewScheduler(h.adapter,
		func() (bool, int, int, *time.Location) { return true, 9, 0, time.UTC },
		nil, WithClock(h.clock.Now))
	if !s.RunDue(ctx) {
		t.Fatal("the next tick did not retry the missing payer")
	}
	rows, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	lines, err := rows.FinOpsCostLines(ctx, h.adapter.cache.scope, storage.FinOpsCostQuery{Payer: "222222222222"})
	if err != nil || len(lines) == 0 {
		t.Fatalf("the failed payer was not refreshed: %d lines, %v", len(lines), err)
	}
	if h.fake.paidSigned("AKIDONE") != done {
		t.Fatal("the retry paid for the completed payer again")
	}
	if st := mustStatus(t, h); st.ScheduledOn != h.adapter.options().today() {
		t.Fatal("the complete retry did not save the day")
	}
}

func TestManualRefreshDoesNotSatisfyTheScheduledPlan(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	manual, err := h.adapter.Refresh(ctx, KindManual)
	if err != nil {
		t.Fatal(err)
	}
	if len(manual.Committed) != 1 || manual.Committed[0] != "111111111111|manual" {
		t.Fatalf("manual refresh committed %v", manual.Committed)
	}
	st := mustStatus(t, h)
	if st.ScheduledOn == h.adapter.options().today() {
		t.Fatal("a manual refresh marked the scheduled day done")
	}
	ps := st.Payers["111111111111"]
	if ps == nil || ps.ManualRefreshedOn != h.adapter.options().today() || !ps.ScheduledRefreshedOn.IsZero() {
		t.Fatalf("manual and scheduled progress are mixed: %+v", ps)
	}

	scheduled, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(scheduled.Committed) != 1 || scheduled.Committed[0] != "111111111111|setup" {
		t.Fatalf("the scheduled plan did not run: %v", scheduled.Committed)
	}
	st = mustStatus(t, h)
	if st.ScheduledOn != h.adapter.options().today() {
		t.Fatal("the completed scheduled run did not save the day")
	}
	if ps := st.Payers["111111111111"]; ps == nil || ps.ScheduledRefreshedOn != h.adapter.options().today() {
		t.Fatalf("scheduled progress missing: %+v", ps)
	}
	if _, err := h.adapter.Refresh(ctx, KindManual); !errors.Is(err, ErrManualUsed) {
		t.Fatalf("a second manual refresh is refused: %v", err)
	}
}

// TestPayerRemovalDeletesItsDecisionsWithoutAPayerOnly: a decision stored
// without a payer on an account the removed payer alone covers goes with
// the payer. One on another payer's account, one on an account both payers
// list, and another payer's decision on the removed payer's account stay.
func TestPayerRemovalDeletesItsDecisionsWithoutAPayerOnly(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	const payer, otherPayer, linked, shared = "111111111111", "999999999999", "222222222222", "333333333333"
	st := mustStatus(t, h)
	ps := st.Payers[payer]
	if ps == nil || ps.Accounts[payer] == nil {
		t.Fatalf("the refresh did not record the payer's accounts: %+v", ps)
	}
	ps.Accounts[linked] = &accountState{}
	ps.Accounts[shared] = &accountState{}
	st.Payers[otherPayer] = &payerState{Accounts: map[string]*accountState{otherPayer: {}, shared: {}}}
	if err := h.adapter.cache.putStatus(st); err != nil {
		t.Fatal(err)
	}
	tables, err := h.adapter.cache.tables()
	if err != nil {
		t.Fatal(err)
	}
	scope := h.adapter.cache.scope
	decided := h.clock.Now().UTC()
	for _, d := range []storage.FinOpsSavingDecision{
		{SavingID: "unfilled-on-payer", Account: payer},
		{SavingID: "unfilled-on-linked", Account: linked},
		{SavingID: "with-payer", Payer: payer, Account: linked},
		{SavingID: "unfilled-on-other", Account: otherPayer},
		{SavingID: "unfilled-on-shared", Account: shared},
		{SavingID: "other-payer-on-linked", Payer: otherPayer, Account: linked},
		// A saving of the whole deployment, decided while both payers were
		// connected: the other payer can still produce it.
		{SavingID: "deployment-wide", Kind: finops.SavingCommitment},
	} {
		d.State, d.At = "dismissed", decided
		if err := tables.PutFinOpsSavingDecision(ctx, scope, d); err != nil {
			t.Fatal(err)
		}
	}

	if err := h.removePayer(payer, 0); err != nil {
		t.Fatal(err)
	}
	left, err := tables.FinOpsSavingDecisions(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, d := range left {
		kept[d.SavingID] = true
	}
	for _, id := range []string{"unfilled-on-payer", "unfilled-on-linked", "with-payer"} {
		if kept[id] {
			t.Fatalf("decision %s of the removed payer survived: %v", id, kept)
		}
	}
	for _, id := range []string{"unfilled-on-other", "unfilled-on-shared", "other-payer-on-linked", "deployment-wide"} {
		if !kept[id] {
			t.Fatalf("decision %s was deleted with another payer: %v", id, kept)
		}
	}
}

// TestLastPayerRemovalDeletesDeploymentDecisions: a saving without an
// account, such as the unused Savings Plan commitment, is decided for the
// deployment. With one payer the decision is stored with that payer; one
// stored with no payer, while several payers were connected, belongs to the
// last payer. Removing the last payer deletes both, and leaves a decision
// another payer stored.
func TestLastPayerRemovalDeletesDeploymentDecisions(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	const payer, otherPayer = "111111111111", "999999999999"
	if st := mustStatus(t, h); len(st.Payers) != 1 || st.Payers[payer] == nil {
		t.Fatalf("the refresh state should hold the one payer: %v", st.Payers)
	}
	tables, err := h.adapter.cache.tables()
	if err != nil {
		t.Fatal(err)
	}
	scope := h.adapter.cache.scope
	decided := h.clock.Now().UTC()
	if err := NewStore(h.provider, scope).PutSavingState(ctx, "commitment", finops.SavingState{
		Dismissed: true, Reason: "bought already", By: "alice", At: decided, Kind: finops.SavingCommitment, Title: "Raise discount coverage"}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []storage.FinOpsSavingDecision{
		{SavingID: "commitment-before", Kind: finops.SavingCommitment},
		{SavingID: "other-payers", Payer: otherPayer, Account: otherPayer},
	} {
		d.State, d.At = "dismissed", decided
		if err := tables.PutFinOpsSavingDecision(ctx, scope, d); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := tables.FinOpsSavingDecisions(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range stored {
		if d.SavingID == "commitment" && (d.Payer != payer || d.Account != "") {
			t.Fatalf("the commitment decided with one payer is stored as payer %q account %q, want the payer", d.Payer, d.Account)
		}
	}

	if err := h.removePayer(payer, 0); err != nil {
		t.Fatal(err)
	}
	left, err := tables.FinOpsSavingDecisions(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].SavingID != "other-payers" || left[0].Payer != otherPayer {
		t.Fatalf("after the last payer's removal: %+v, want only the other payer's decision", left)
	}
}

// TestRemovingAMemberAccountKeepsItsPayersDecisions: in an organization the
// member account M is listed under the live payer O and has no state of
// its own. Removing M's account purges nothing of O: a decision stored
// without a payer on M or on O belongs to O, and the purge leaves it.
func TestRemovingAMemberAccountKeepsItsPayersDecisions(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	const organization, member = "111111111111", "222222222222"
	st := mustStatus(t, h)
	st.Payers[organization].Accounts[member] = &accountState{}
	if err := h.adapter.cache.putStatus(st); err != nil {
		t.Fatal(err)
	}
	tables, err := h.adapter.cache.tables()
	if err != nil {
		t.Fatal(err)
	}
	scope := h.adapter.cache.scope
	for _, d := range []storage.FinOpsSavingDecision{
		{SavingID: "unfilled-on-member", Account: member},
		{SavingID: "unfilled-on-organization", Account: organization},
	} {
		d.State, d.At = "dismissed", h.clock.Now().UTC()
		if err := tables.PutFinOpsSavingDecision(ctx, scope, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.removePayer(member, 0); err != nil {
		t.Fatal(err)
	}
	left, err := tables.FinOpsSavingDecisions(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("removing member %s deleted the live payer's decisions; left %+v", member, left)
	}
	if got := payerAccounts(mustStatus(t, h), member); len(got) != 0 {
		t.Fatalf("a member listed under a live payer names %v", got)
	}
}
