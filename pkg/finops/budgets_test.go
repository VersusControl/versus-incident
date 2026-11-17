package finops

import "testing"

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
