package finops

import (
	"context"
	"testing"
)

// TestBudgetAlertsOncePerTransition: At risk alerts once; Over budget
// alerts once more; nothing repeats; no budget, no alert.
func TestBudgetAlertsOncePerTransition(t *testing.T) {
	ctx := context.Background()
	src, cfg := natStory()
	store := newMemStore()
	svc := NewService(src, store, cfg)
	// November forecast $15,000 against $14,000: at risk.
	alerts, err := svc.SyncBudgetAlerts(ctx)
	if err != nil || len(alerts) != 1 || alerts[0].Status != BudgetAtRisk || alerts[0].Month != "2026-11" {
		t.Fatalf("at risk: %+v %v", alerts, err)
	}
	if err := svc.MarkBudgetAlerted(ctx, alerts[0]); err != nil {
		t.Fatal(err)
	}
	if again, _ := svc.SyncBudgetAlerts(ctx); len(again) != 0 {
		t.Fatalf("at risk alerts once: %+v", again)
	}
	// Nov 1 completes with $500 against a $100 budget: over budget.
	src.status.LastCompleteDay = MustDate("2026-11-01")
	cfg.Budgets[0].Monthly = usd(100)
	svc = NewService(src, store, cfg)
	over, err := svc.SyncBudgetAlerts(ctx)
	if err != nil || len(over) != 1 || over[0].Status != BudgetOver || over[0].BudgetID != alerts[0].BudgetID {
		t.Fatalf("over budget: %+v %v", over, err)
	}
	if err := svc.MarkBudgetAlerted(ctx, over[0]); err != nil {
		t.Fatal(err)
	}
	if again, _ := svc.SyncBudgetAlerts(ctx); len(again) != 0 {
		t.Fatalf("over budget alerts once: %+v", again)
	}
	cfg.Budgets = nil
	if none, err := NewService(src, newMemStore(), cfg).SyncBudgetAlerts(ctx); err != nil || len(none) != 0 {
		t.Fatalf("no budget, no alert: %+v %v", none, err)
	}
	if _, err := NewService(src, nil, cfg).SyncBudgetAlerts(ctx); err != ErrNoStore {
		t.Fatal("budget alerts need the decision store")
	}
}
