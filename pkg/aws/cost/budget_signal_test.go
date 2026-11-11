package cost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

type budgetSyncer struct {
	countingSyncer
	alerts []finops.BudgetAlert
	sent   map[string]bool
	fail   bool
}

func (b *budgetSyncer) SyncBudgetAlerts(context.Context) ([]finops.BudgetAlert, error) {
	var out []finops.BudgetAlert
	for _, a := range b.alerts {
		if !b.sent[a.BudgetID+a.Status] {
			out = append(out, a)
		}
	}
	return out, nil
}

func (b *budgetSyncer) MarkBudgetAlerted(_ context.Context, a finops.BudgetAlert) error {
	if b.fail {
		return errors.New("store down")
	}
	b.sent[a.BudgetID+a.Status] = true
	return nil
}

// TestBudgetSignalsAlertOncePerTransition: a budget that moved to At risk
// reaches the alert path once; a failed record re-sends next time.
func TestBudgetSignalsAlertOncePerTransition(t *testing.T) {
	syncer := &budgetSyncer{sent: map[string]bool{}, alerts: []finops.BudgetAlert{{BudgetID: "b1", Name: "AWS total", Status: finops.BudgetAtRisk, Month: "2026-11",
		Monthly: finops.USD(42_000), Forecast: finops.USD(41_000), Actual: finops.USD(9_000), Currency: "USD"}}}
	src := NewSignalSource("prod", syncer, func() time.Time { return at("2026-11-09", 10) })
	var got []core.Signal
	sink := func(_ context.Context, sig core.Signal) error { got = append(got, sig); return nil }
	syncer.fail = true
	if _, err := PumpAlerts(ctx, src, sink); err == nil {
		t.Fatal("an unrecorded alert reports the failure")
	}
	syncer.fail = false
	if n, err := PumpAlerts(ctx, src, sink); err != nil || n != 1 {
		t.Fatalf("re-sent after the failed record: %d %v", n, err)
	}
	if n, _ := PumpAlerts(ctx, src, sink); n != 0 {
		t.Fatal("one alert per transition")
	}
	last := got[len(got)-1]
	if last.Fields[core.FieldSignal] != SignalBudget || last.Fields["status"] != finops.BudgetAtRisk || last.Fields["budget_id"] != "b1" {
		t.Fatalf("budget signal %+v", last.Fields)
	}
	syncer.alerts[0].Status = finops.BudgetOver
	if n, _ := PumpAlerts(ctx, src, sink); n != 1 {
		t.Fatal("over budget alerts once more")
	}
}
