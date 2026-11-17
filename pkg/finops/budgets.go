package finops

import (
	"errors"
	"fmt"
	"strings"
)

// Budget scopes. A deployment has one budget set; there are no per-org
// budgets.
const (
	ScopeDeployment = "deployment"
	ScopeAccount    = "account"
	ScopeTeam       = "team"
)

// Budget sources.
const (
	BudgetFromConfig   = "config"
	BudgetFromUI       = "ui"
	BudgetFromProvider = "provider" // read-only from the provider's budgets
)

// Budget statuses.
const (
	BudgetOnTrack = "on_track"
	BudgetAtRisk  = "at_risk"
	BudgetOver    = "over_budget"
)

// atRiskBP: a forecast above 90% of the budget is at risk.
const atRiskBP = 9_000

// Budget is a monthly budget for the deployment, one account or one team.
type Budget struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Scope   string `json:"scope"`                 // deployment | account | team
	Value   string `json:"scope_value,omitempty"` // account ID or team; empty for deployment
	Monthly Micros `json:"monthly_micros"`
	Source  string `json:"source"`
}

// Reasons a provider budget is not shown.
const (
	SkipBudgetNotMonthlyCost = "not_monthly_cost"     // not a monthly cost budget
	SkipBudgetMeasure        = "not_net_amortized"    // measured in another cost measure
	SkipBudgetFilter         = "filter_not_supported" // filter is not one account or one team
)

// SkippedBudget names a provider budget left out, and why, so the page can
// say so instead of silently dropping it.
type SkippedBudget struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ReadOnly reports whether the UI may edit the budget.
func (b Budget) ReadOnly() bool { return b.Source != BudgetFromUI }

// ErrBadBudget is returned for an invalid budget.
var ErrBadBudget = errors.New("finops: invalid budget")

// ValidateBudgets checks UI or config budgets: a known scope, a value only
// for account and team scopes, a positive amount, a name, and no duplicate
// scope.
func ValidateBudgets(bs []Budget) error {
	seen := map[string]bool{}
	for i, b := range bs {
		if strings.TrimSpace(b.Name) == "" {
			return fmt.Errorf("%w: budgets[%d].name is required", ErrBadBudget, i)
		}
		switch b.Scope {
		case ScopeDeployment:
			if b.Value != "" {
				return fmt.Errorf("%w: budgets[%d] deployment scope takes no value", ErrBadBudget, i)
			}
		case ScopeAccount, ScopeTeam:
			if strings.TrimSpace(b.Value) == "" {
				return fmt.Errorf("%w: budgets[%d] %s scope needs a value", ErrBadBudget, i, b.Scope)
			}
		default:
			return fmt.Errorf("%w: budgets[%d].scope must be deployment, account or team", ErrBadBudget, i)
		}
		if b.Monthly <= 0 {
			return fmt.Errorf("%w: budgets[%d] amount must be positive", ErrBadBudget, i)
		}
		k := b.Scope + "|" + b.Value
		if seen[k] {
			return fmt.Errorf("%w: budgets[%d] duplicates another budget scope", ErrBadBudget, i)
		}
		seen[k] = true
	}
	return nil
}

// BudgetID is stable for a scope.
func BudgetID(scope, value string) string { return shortHash("budget", scope, value) }

// Forecast sources.
const (
	ForecastProvider = "provider"
	ForecastEstimate = "estimate"
	ForecastNone     = "none"
)

// RunRateDays is the window of the linear estimate.
const RunRateDays = 7

// ProviderForecast is the provider's month-end forecast with its interval.
type ProviderForecast struct {
	Month string `json:"month"` // "2026-11"
	Mean  Micros `json:"mean_micros"`
	Low   Micros `json:"low_micros"`
	High  Micros `json:"high_micros"`
}

// Forecast is the month-end forecast the page shows.
type Forecast struct {
	Month  string `json:"month"`
	Amount Micros `json:"amount_micros"`
	Low    Micros `json:"low_micros,omitempty"`
	High   Micros `json:"high_micros,omitempty"`
	Source string `json:"source"` // provider | estimate | none
	// Reason explains a missing forecast ("not_enough_history",
	// "forecast_skipped").
	Reason string `json:"reason,omitempty"`
}

// ForecastMonth is the month the forecast is for: the month of the first
// day not yet complete.
func ForecastMonth(lastComplete Date) Date { return lastComplete.AddDays(1).MonthStart() }

// MakeForecast uses the provider forecast when it is for the right month,
// else a linear run-rate: actual month to date plus the average of the last
// RunRateDays complete days for each remaining day. Without RunRateDays of
// history it reports source "none". daily returns the cost of one day.
func MakeForecast(pf *ProviderForecast, lastComplete, historyStart Date, daily func(Date) Micros, skipped bool) Forecast {
	month := ForecastMonth(lastComplete)
	f := Forecast{Month: month.MonthKey()}
	if pf != nil && pf.Month == f.Month {
		f.Amount, f.Low, f.High, f.Source = pf.Mean, pf.Low, pf.High, ForecastProvider
		return f
	}
	if historyStart.IsZero() || historyStart.After(lastComplete.AddDays(-(RunRateDays - 1))) {
		f.Source, f.Reason = ForecastNone, "not_enough_history"
		return f
	}
	var recent Micros
	for i := range RunRateDays {
		recent += daily(lastComplete.AddDays(-i))
	}
	var mtd Micros
	for d := month; !d.After(lastComplete); d = d.AddDays(1) {
		mtd += daily(d)
	}
	remaining := month.DaysUntil(month.MonthEnd()) + 1
	if !lastComplete.Before(month) {
		remaining = lastComplete.DaysUntil(month.MonthEnd())
	}
	f.Amount = mtd + mulDiv(recent, int64(remaining), RunRateDays)
	f.Source = ForecastEstimate
	if skipped {
		f.Reason = "forecast_skipped"
	}
	return f
}

// BudgetStatus compares actual and forecast with a budget: over budget when
// actual is above it, at risk when the forecast is above 90% of it.
func BudgetStatus(budget, actual Micros, f Forecast) string {
	switch {
	case actual > budget:
		return BudgetOver
	case f.Source != ForecastNone && basisPoints(f.Amount, budget) > atRiskBP:
		return BudgetAtRisk
	}
	return BudgetOnTrack
}

// BudgetMatches reports whether a line counts toward a budget. team is the
// resolved owner of the line.
func BudgetMatches(b Budget, account, team string) bool {
	switch b.Scope {
	case ScopeAccount:
		return account == b.Value
	case ScopeTeam:
		return team == b.Value
	}
	return true
}
