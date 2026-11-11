package cost

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

// A persisted increase keeps the key it was detected with, so a page lists
// it only for an account that still bills.
func TestStoredIncreaseKeepsItsKey(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	store := NewStore(h.provider, h.adapter.cache.scope)
	rec := finops.IncreaseRecord{
		Detected: finops.Detected{ID: "inc-1", Key: finops.LineKey{Account: "111111111111", Service: "network.nat"},
			StartedOn: finops.MustDate("2026-11-01"), MonthlyImpact: 5_000_000},
		Status: finops.StatusOpen,
	}
	if err := store.PutIncrease(ctx, rec); err != nil {
		t.Fatal(err)
	}
	recs, err := store.ListIncreases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Key.Account != "111111111111" || recs[0].Key.Service != "network.nat" {
		t.Fatalf("read back %+v", recs)
	}
	svc := finops.NewService(h.adapter.Source(), store, finops.Config{Now: h.clock.Now})
	view, err := svc.Increases(ctx, finops.Query{Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 1 || view.Items[0].Account != "111111111111" {
		t.Fatalf("listed %+v", view.Items)
	}
}
