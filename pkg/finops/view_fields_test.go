package finops

import (
	"context"
	"testing"
)

func TestLearningUntilAndBillCloseDates(t *testing.T) {
	src, cfg := natStory()
	src.status.HistoryStart = MustDate("2026-10-25")
	v, err := NewService(src, nil, cfg).Summary(context.Background(), october)
	if err != nil {
		t.Fatal(err)
	}
	s := v.Sync
	if !s.Learning || s.LearningUntil != MustDate("2026-11-10") {
		t.Fatalf("learning until %s (learning %v)", s.LearningUntil, s.Learning)
	}
	// Now is Nov 6: October closed on Nov 5, so the next close is December's 5th.
	if s.PreviousMonthOpen || s.BillClosesOn != MustDate("2026-12-05") {
		t.Fatalf("bill closes %s", s.BillClosesOn)
	}
}

func TestIncreaseViewCarriesSuspectsAndAlert(t *testing.T) {
	svc, _, _ := newStory(t)
	ctx := context.Background()
	alert, err := svc.SyncIncreases(ctx)
	if err != nil || len(alert) == 0 {
		t.Fatalf("increases to alert %d %v", len(alert), err)
	}
	if err := svc.MarkIncreaseAlerted(ctx, alert[0].ID); err != nil {
		t.Fatal(err)
	}
	list, err := svc.Increases(ctx, Query{Period: "2026-10", Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	var nat, alerted *IncreaseView
	for i := range list.Items {
		if list.Items[i].Service == "network.nat" {
			nat = &list.Items[i]
		}
		if list.Items[i].ID == alert[0].ID {
			alerted = &list.Items[i]
		}
	}
	if nat == nil || len(nat.Suspects) != 1 || nat.Suspects[0].ConfidenceWord != "Likely" || nat.Suspect.Summary != nat.Suspects[0].Summary {
		t.Fatalf("suspects %+v", nat)
	}
	if alerted == nil || alerted.Alert == nil || alerted.Alert.Channel != "alerts" || alerted.Alert.SentAt.IsZero() {
		t.Fatalf("alert %+v", alerted)
	}
	for _, it := range list.Items {
		if it.Suspects == nil {
			t.Fatal("suspects is always a list")
		}
	}
}
