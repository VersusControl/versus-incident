package finops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Over budget means actual above the budget; reaching it exactly is not
// over. An estimated forecast counts for "at risk" like the provider's.
func TestBudgetStatusBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		actual   Micros
		forecast Forecast
		want     string
	}{
		{"actual equal to the budget is not over", usd(100), Forecast{Source: ForecastNone}, BudgetOnTrack},
		{"actual equal with a high forecast is at risk", usd(100), Forecast{Source: ForecastProvider, Amount: usd(120)}, BudgetAtRisk},
		{"one micro-dollar over is over", usd(100) + 1, Forecast{Source: ForecastNone}, BudgetOver},
		{"over wins over at risk", usd(150), Forecast{Source: ForecastProvider, Amount: usd(200)}, BudgetOver},
		{"an estimate above 90% is at risk", usd(10), Forecast{Source: ForecastEstimate, Amount: usd(91)}, BudgetAtRisk},
		{"an estimate at 90% is on track", usd(10), Forecast{Source: ForecastEstimate, Amount: usd(90)}, BudgetOnTrack},
		{"no forecast never raises at risk", usd(10), Forecast{Source: ForecastNone, Amount: usd(500)}, BudgetOnTrack},
	}
	for _, c := range cases {
		if got := BudgetStatus(usd(100), c.actual, c.forecast); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// The run-rate estimate needs seven complete days of history: exactly seven
// is enough, six is not.
func TestForecastEstimateNeedsSevenDays(t *testing.T) {
	last := MustDate("2026-11-10")
	daily := func(Date) Micros { return usd(10) }
	f := MakeForecast(nil, last, last.AddDays(-(RunRateDays - 1)), daily, false)
	if f.Source != ForecastEstimate || f.Reason != "" {
		t.Fatalf("seven days make an estimate: %+v", f)
	}
	// 10 days actual + 20 days × the $10 average.
	if f.Amount != usd(300) || f.Month != "2026-11" {
		t.Fatalf("estimate %+v", f)
	}
	f = MakeForecast(nil, last, last.AddDays(-(RunRateDays - 2)), daily, false)
	if f.Source != ForecastNone || f.Reason != "not_enough_history" || f.Amount != 0 {
		t.Fatalf("six days are not enough and never show zero as a forecast: %+v", f)
	}
	if f = MakeForecast(nil, last, Date{}, daily, false); f.Source != ForecastNone {
		t.Fatalf("unknown history: %+v", f)
	}
}

func TestBudgetMatchesItsScope(t *testing.T) {
	cases := []struct {
		b             Budget
		account, team string
		want          bool
	}{
		{Budget{Scope: ScopeDeployment}, "111", "checkout", true},
		{Budget{Scope: ScopeAccount, Value: "111"}, "111", "", true},
		{Budget{Scope: ScopeAccount, Value: "111"}, "222", "checkout", false},
		{Budget{Scope: ScopeTeam, Value: "checkout"}, "222", "checkout", true},
		{Budget{Scope: ScopeTeam, Value: "checkout"}, "111", Unallocated, false},
	}
	for _, c := range cases {
		if got := BudgetMatches(c.b, c.account, c.team); got != c.want {
			t.Errorf("%+v account %s team %s: %v", c.b, c.account, c.team, got)
		}
	}
}

// Two standalone billing accounts each send a whole-bill budget of the same
// name. Each budget is measured on its own account's spend, and neither is
// the deployment's whole-bill budget on the overview.
func TestBillingAccountBudgetsMeasureTheirOwnSpend(t *testing.T) {
	const first, second = "111111111111", "222222222222"
	src := &fakeSource{status: SourceStatus{Enabled: true, Currency: "USD", LastCompleteDay: MustDate("2026-11-05"), HistoryStart: MustDate("2026-10-01"),
		Accounts: []Account{{ID: first, Kind: AccountConnected, Readable: true}, {ID: second, Kind: AccountConnected, Readable: true}},
		Budgets: []Budget{
			{ID: "aws-one", Name: "AWS total", Scope: ScopeDeployment, Monthly: usd(100), Accounts: []string{first}},
			{ID: "aws-two", Name: "AWS total", Scope: ScopeDeployment, Monthly: usd(100), Accounts: []string{second}},
		}}}
	for day := MustDate("2026-10-01"); !day.After(MustDate("2026-11-05")); day = day.AddDays(1) {
		src.lines = append(src.lines, line(day, first, "compute.vm", "BoxUsage", "Usage", "", usd(10), 1), line(day, second, "compute.vm", "BoxUsage", "Usage", "", usd(30), 1))
	}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	svc := NewService(src, nil, Config{Now: func() time.Time { return now }})
	view, err := svc.Budget(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]Micros{}
	for _, b := range view.Budgets {
		actual[b.ID] = b.Actual
	}
	if len(view.Budgets) != 2 || actual["aws-one"] != usd(50) || actual["aws-two"] != usd(150) {
		t.Fatalf("budget actuals = %v", actual)
	}
	overview, err := svc.Summary(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if overview.Budget != nil {
		t.Fatalf("one billing account's budget shown as the whole bill: %+v", overview.Budget)
	}
}

// Two standalone billing accounts each send their own budgets. A caller
// limited to the second account sees only budgets that measure it: in the
// budget list, its skipped names, the overview and the trend pace. A
// whole-bill budget and a team budget over every account stay hidden. An
// unscoped caller sees every budget.
func TestAccountScopeShowsOnlyBudgetsInsideTheScope(t *testing.T) {
	const first, second = "111111111111", "222222222222"
	src := &fakeSource{status: SourceStatus{Enabled: true, Currency: "USD", LastCompleteDay: MustDate("2026-11-05"), HistoryStart: MustDate("2026-10-01"),
		Accounts: []Account{{ID: first, Kind: AccountConnected, Readable: true}, {ID: second, Kind: AccountConnected, Readable: true}},
		Budgets: []Budget{
			{ID: "aws-one-total", Name: "First total", Scope: ScopeDeployment, Monthly: usd(100), Accounts: []string{first}},
			{ID: "aws-one-team", Name: "First checkout", Scope: ScopeTeam, Value: "checkout", Monthly: usd(50), Accounts: []string{first}},
			{ID: "aws-two-total", Name: "Second total", Scope: ScopeDeployment, Monthly: usd(300), Accounts: []string{second}},
			{ID: "aws-two-team", Name: "Second checkout", Scope: ScopeTeam, Value: "checkout", Monthly: usd(200), Accounts: []string{second}},
			{ID: "aws-both", Name: "Spans both", Scope: ScopeDeployment, Monthly: usd(400), Accounts: []string{first, second}},
		},
		SkippedBudgets: []SkippedBudget{
			{Name: "First skipped", Reason: SkipBudgetFilter, Accounts: []string{first}},
			{Name: "Second skipped", Reason: SkipBudgetFilter, Accounts: []string{second}},
		}}}
	for day := MustDate("2026-10-01"); !day.After(MustDate("2026-11-05")); day = day.AddDays(1) {
		src.lines = append(src.lines, line(day, first, "compute.vm", "BoxUsage", "Usage", "checkout", usd(10), 1),
			line(day, second, "compute.vm", "BoxUsage", "Usage", "checkout", usd(30), 1))
	}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	st := newMemStore()
	svc := NewService(src, st, Config{Now: func() time.Time { return now }, AccountScope: scopeFromContext,
		Budgets: []Budget{{Name: "Whole bill", Scope: ScopeDeployment, Monthly: usd(1000)},
			{Name: "All checkout", Scope: ScopeTeam, Value: "payments", Monthly: usd(500)}}})
	ctx := t.Context()
	if _, err := svc.PutBudgets(ctx, []Budget{
		{Name: "First account", Scope: ScopeAccount, Value: first, Monthly: usd(70)},
		{Name: "Second account", Scope: ScopeAccount, Value: second, Monthly: usd(90)},
	}); err != nil {
		t.Fatal(err)
	}
	month := Query{Period: "2026-11"}
	scoped := withAccounts(ctx, second)

	names := func(v BudgetView) (map[string]bool, map[string]bool) {
		b, s := map[string]bool{}, map[string]bool{}
		for _, l := range v.Budgets {
			b[l.Name] = true
		}
		for _, k := range v.Skipped {
			s[k.Name] = true
		}
		return b, s
	}
	all, err := svc.Budget(ctx, month)
	if err != nil {
		t.Fatal(err)
	}
	if b, s := names(all); len(b) != 9 || len(s) != 2 {
		t.Fatalf("unscoped budgets %v skipped %v", b, s)
	}
	view, err := svc.Budget(scoped, month)
	if err != nil {
		t.Fatal(err)
	}
	b, s := names(view)
	if len(b) != 3 || !b["Second total"] || !b["Second checkout"] || !b["Second account"] || len(s) != 1 || !s["Second skipped"] {
		t.Fatalf("scoped budgets %v skipped %v", b, s)
	}
	for _, l := range view.Budgets {
		if l.Name == "Second total" && l.Actual != usd(150) {
			t.Fatalf("an in-scope budget lost its spend: %+v", l)
		}
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), first) {
		t.Fatalf("the scoped budget view names the other account: %s", raw)
	}

	overview, err := svc.Summary(ctx, month)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Budget == nil || overview.Budget.Name != "Whole bill" {
		t.Fatalf("unscoped overview budget %+v", overview.Budget)
	}
	scopedOverview, err := svc.Summary(scoped, month)
	if err != nil {
		t.Fatal(err)
	}
	if scopedOverview.Budget != nil {
		t.Fatalf("the scoped overview shows a budget over other accounts: %+v", scopedOverview.Budget)
	}
	trend, err := svc.Trend(ctx, month)
	if err != nil {
		t.Fatal(err)
	}
	if len(trend.BudgetPace) == 0 {
		t.Fatal("the unscoped trend lost the whole-bill pace")
	}
	scopedTrend, err := svc.Trend(scoped, month)
	if err != nil {
		t.Fatal(err)
	}
	if scopedTrend.BudgetPace != nil {
		t.Fatalf("the scoped trend paces a budget over other accounts: %v", scopedTrend.BudgetPace)
	}
}
