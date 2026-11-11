package cost

import (
	"errors"
	"testing"
)

// TestDrillDownSpendsOnlyOnKnownPairsAndLeavesTheScheduledRefreshWhole: a
// click cannot invent pairs to spend paid requests, and before today's
// scheduled refresh it may only use what that refresh leaves.
func TestDrillDownSpendsOnlyOnKnownPairsAndLeavesTheScheduledRefreshWhole(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	sent := h.fake.requests()
	for _, account := range []string{"12345", "abcdefghijkl", "1111111111111"} {
		if err := h.adapter.DrillDown(ctx, account, "EC2 - Other"); !errors.Is(err, ErrInvalidAccount) {
			t.Fatalf("account %q: %v", account, err)
		}
	}
	if err := h.adapter.DrillDown(ctx, "111111111111", "Invented Service 1"); !errors.Is(err, ErrUnknownPair) {
		t.Fatalf("invented service: %v", err)
	}
	if h.fake.requests() != sent {
		t.Fatal("a refused drill-down sends nothing")
	}

	// Next morning, before the scheduled refresh: the daily plan costs 6.
	h.clock.set(at("2026-11-10", 8))
	h.opts.MaxPaidRequests = 6
	if err := h.adapter.Update(h.opts); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.DrillDown(ctx, "111111111111", "EC2 - Other"); !errors.Is(err, ErrReservedForRefresh) {
		t.Fatalf("reserved budget: %v", err)
	}
	if h.adapter.Meter().Used(ctx) != 0 || h.fake.requests() != sent {
		t.Fatal("a reserved click spends nothing")
	}
	// A drill reads about 40 days: until learned it is priced at 2 pages,
	// so one request left is not enough.
	h.opts.MaxPaidRequests = 7
	if err := h.adapter.Update(h.opts); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.DrillDown(ctx, "111111111111", "EC2 - Other"); !errors.Is(err, ErrReservedForRefresh) {
		t.Fatalf("one request left, drill priced at two: %v", err)
	}
	h.opts.MaxPaidRequests = 8
	if err := h.adapter.Update(h.opts); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.DrillDown(ctx, "111111111111", "EC2 - Other"); err != nil {
		t.Fatalf("two requests left after the refresh: %v", err)
	}
	if used := h.adapter.Meter().Used(ctx); used != 1 {
		t.Fatalf("click spent %d", used)
	}
	if st := mustStatus(t, h).Payers["111111111111"].Pages[callDrill]; st.Pages != 1 || !st.Done {
		t.Fatalf("drill pages learned %+v", st)
	}
	if res, err := h.adapter.Refresh(ctx, KindScheduled); err != nil || len(res.Committed) != 1 || len(res.Plan.Skipped) != 0 {
		t.Fatalf("the scheduled refresh still commits its day: %+v %v", res, err)
	}
	// Next morning the learned single page fits in one request left.
	h.clock.set(at("2026-11-11", 8))
	h.opts.MaxPaidRequests = 7
	if err := h.adapter.Update(h.opts); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.DrillDown(ctx, "111111111111", "Amazon Simple Storage Service"); err != nil {
		t.Fatalf("learned one-page drill: %v", err)
	}
}
