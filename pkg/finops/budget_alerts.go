package finops

import (
	"context"
	"time"
)

// BudgetAlertState records the last budget alert sent in a month.
type BudgetAlertState struct {
	Month  string    `json:"month"`
	Status string    `json:"status"` // at_risk | over_budget
	At     time.Time `json:"at"`
}

// BudgetAlert is a budget that changed to a status worth one alert.
type BudgetAlert struct {
	BudgetID       string `json:"budget_id"`
	Name           string `json:"name"`
	Scope          string `json:"scope"`
	Value          string `json:"scope_value,omitempty"`
	Status         string `json:"status"`
	Month          string `json:"month"`
	Monthly        Micros `json:"monthly_micros"`
	Actual         Micros `json:"actual_micros"`
	Forecast       Micros `json:"forecast_micros"`
	ForecastSource string `json:"forecast_source"`
	Currency       string `json:"currency,omitempty"`
}

// budgetRank orders statuses: a later alert needs a higher rank.
func budgetRank(status string) int {
	switch status {
	case BudgetAtRisk:
		return 1
	case BudgetOver:
		return 2
	}
	return 0
}

// SyncBudgetAlerts returns the budgets that moved this month to At risk
// (one alert) or to Over budget (one more alert); a budget already alerted
// at that level, or on track, returns nothing. Callers send each through
// the alert path and then call MarkBudgetAlerted.
func (s *Service) SyncBudgetAlerts(ctx context.Context) ([]BudgetAlert, error) {
	if s.store == nil {
		return nil, ErrNoStore
	}
	sc, err := s.load(ctx, Query{})
	if err != nil || !sc.header.Enabled {
		return nil, err
	}
	budgets, err := s.allBudgets(ctx, sc)
	if err != nil || len(budgets) == 0 {
		return nil, err
	}
	states, err := s.store.BudgetAlertStates(ctx)
	if err != nil {
		return nil, err
	}
	month := ForecastMonth(sc.st.LastCompleteDay).MonthKey()
	var out []BudgetAlert
	for _, b := range budgets {
		line := s.budgetLine(sc, b)
		sent := states[b.ID]
		if sent.Month != month {
			sent = BudgetAlertState{}
		}
		if budgetRank(line.Status) <= budgetRank(sent.Status) {
			continue
		}
		out = append(out, BudgetAlert{BudgetID: b.ID, Name: b.Name, Scope: b.Scope, Value: b.Value, Status: line.Status,
			Month: month, Monthly: b.Monthly, Actual: line.Actual, Forecast: line.Forecast, ForecastSource: line.ForecastSource,
			Currency: sc.st.Currency})
	}
	return out, nil
}

// MarkBudgetAlerted records that a budget alert was sent.
func (s *Service) MarkBudgetAlerted(ctx context.Context, a BudgetAlert) error {
	if s.store == nil {
		return ErrNoStore
	}
	return s.store.PutBudgetAlertState(ctx, a.BudgetID, BudgetAlertState{Month: a.Month, Status: a.Status, At: s.now().UTC()})
}
