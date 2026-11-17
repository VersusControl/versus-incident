package cost

import (
	"slices"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

// TestOrganizationViewFields covers the page's organization state: the cost
// source chip, payer rows, the account column (rows span two or more
// accounts), scope choices and the learning and bill dates.
func TestOrganizationViewFields(t *testing.T) {
	h, _ := orgHarness(t)
	h.opts.RefreshSet, h.opts.RefreshHour = true, 9
	if err := h.adapter.Update(h.opts); err != nil {
		t.Fatal(err)
	}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	backfillOrganization(t, h)
	svc := finops.NewService(h.adapter.Source(), NewStore(h.provider, h.adapter.cache.scope), finops.Config{Now: h.clock.Now})
	sum, err := svc.Summary(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if cs := sum.CostSource; cs == nil || cs.Kind != "management" || cs.AccountID != "999999999999" {
		t.Fatalf("cost source %+v", sum.CostSource)
	}
	if n := sum.Sync.NextRefreshAt; n == nil || !n.Equal(time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("next refresh %v", sum.Sync.NextRefreshAt)
	}
	if sum.Sync.BillClosesOn != finops.MustDate("2026-12-05") {
		t.Fatalf("bill closes %s", sum.Sync.BillClosesOn)
	}
	so := sum.ScopeOptions
	kinds := map[string]string{}
	for _, a := range so.Accounts {
		kinds[a.ID] = a.Kind
	}
	if kinds["111111111111"] != finops.AccountConnected || kinds["333333333333"] != finops.AccountCostOnly ||
		!slices.Contains(so.Regions, "us-east-1") || !slices.Contains(so.Teams, "checkout") {
		t.Fatalf("scope options %+v", so)
	}
	// A filter never hides the other choices.
	filtered, _ := svc.Summary(ctx, finops.Query{Accounts: []string{"333333333333"}})
	if len(filtered.ScopeOptions.Accounts) != len(so.Accounts) || !slices.Contains(filtered.ScopeOptions.Teams, "checkout") {
		t.Fatalf("filtered scope options %+v", filtered.ScopeOptions)
	}
	br, err := svc.Drivers(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if !br.AccountColumn.Show {
		t.Fatal("rows spanning two or more accounts show the Account column")
	}
	for _, r := range br.Rows {
		want := kinds[r.Account]
		if r.Service == "commitment.unused" || r.Service == "billing.fee" {
			want = "payer"
		}
		if r.AccountKind != want {
			t.Fatalf("row %s account kind %q, want %q", r.Label, r.AccountKind, want)
		}
	}
}

func TestSingleAccountViewFields(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	svc := finops.NewService(h.adapter.Source(), nil, finops.Config{Now: h.clock.Now})
	sum, _ := svc.Summary(ctx, finops.Query{})
	if cs := sum.CostSource; cs == nil || cs.Kind != "account" || cs.AccountID != "111111111111" {
		t.Fatalf("cost source %+v", sum.CostSource)
	}
	if sum.Sync.NextRefreshAt != nil {
		t.Fatal("no refresh time configured, none shown")
	}
	br, _ := svc.Drivers(ctx, finops.Query{})
	if br.AccountColumn.Show {
		t.Fatal("one account: no Account column")
	}
	// A refresh that fails later names since when and which days are missing.
	h.clock.set(at("2026-11-12", 10))
	h.fake.failures[callTeam] = fakeFailure{Type: "ThrottlingException", Message: "slow"}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	sum, _ = svc.Summary(ctx, finops.Query{})
	sd := sum.Sync.StaleDetail
	if !sum.Sync.Stale || sd == nil || sd.Since == nil || !sd.Since.Equal(at("2026-11-09", 10)) ||
		!slices.Equal(sd.Days, []finops.Date{finops.MustDate("2026-11-08"), finops.MustDate("2026-11-09"), finops.MustDate("2026-11-10")}) {
		t.Fatalf("stale detail %+v", sd)
	}
}

// TestUnavailableCostSourceStillNamesTheAccount: state 09e-17 (no totals,
// ce_unavailable) still says which account cost is read through.
func TestUnavailableCostSourceStillNamesTheAccount(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) { o.AccountNames = map[string]string{"111111111111": "Production"} })
	h.fake.failures[callAccount] = fakeFailure{Type: "AccessDeniedException", Message: "User not enabled for cost explorer access"}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	v, err := finops.NewService(h.adapter.Source(), nil, finops.Config{Now: h.clock.Now}).Summary(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	cs := v.CostSource
	if v.Enabled || v.Reason != CodeUnavailable || cs == nil || cs.AccountID != "111111111111" || cs.Kind != "account" || cs.AccountName != "Production" {
		t.Fatalf("header %+v cost source %+v", v.Header, cs)
	}
}

// TestNotFinalDaysNeverMarkFiguresEstimated: AWS marks every day of the open
// month Estimated (not final); that is finality only. Without split cells
// in play, no view field says estimated.
func TestNotFinalDaysNeverMarkFiguresEstimated(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	svc := finops.NewService(h.adapter.Source(), nil, finops.Config{Now: h.clock.Now,
		Budgets: []finops.Budget{{ID: "b", Name: "Total", Scope: finops.ScopeDeployment, Monthly: finops.USD(50_000), Source: finops.BudgetFromConfig}}})
	sum, err := svc.Summary(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Sync.Estimated) != 0 || sum.Budget == nil || sum.Budget.Estimated {
		t.Fatalf("not-final days marked figures estimated: sync %v budget %+v", sum.Sync.Estimated, sum.Budget)
	}
	bd, _ := svc.Breakdown(ctx, finops.Query{GroupBy: finops.GroupService})
	for _, r := range bd.Rows {
		if r.Estimated {
			t.Fatalf("service row %s estimated without a split cell", r.Key)
		}
	}
}
