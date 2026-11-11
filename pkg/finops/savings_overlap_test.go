package finops

import (
	"context"
	"testing"
	"time"
)

// TestOneActionPerResourceAndCommitmentOutsideTheTotal: two providers name
// the same resources — a stop and a rightsize for i-1, a delete and a
// rightsize for vol-1, a lone rightsize for i-2 — and a Savings Plans
// purchase is suggested. Two actions on one resource are never both counted:
// the larger one is the item and the other an alternative. The commitment,
// computed before other savings, is shown but not added to the total.
func TestOneActionPerResourceAndCommitmentOutsideTheTotal(t *testing.T) {
	idle := func(kind, id string, monthly Micros) IdleResource {
		return IdleResource{Kind: kind, ID: id, Account: "111111111111", Region: "us-east-1", Monthly: monthly, Basis: "provider"}
	}
	rightsize := func(id, current, target string, monthly Micros) Recommendation {
		return Recommendation{ResourceID: id, Account: "111111111111", Region: "us-east-1", Current: current, Target: target,
			Monthly: monthly, Source: "cost_optimization_hub"}
	}
	fx := struct {
		Inputs    SavingsInputs
		WantTotal Micros
		WantItems int
	}{
		Inputs: SavingsInputs{
			Idle: []IdleResource{idle("idle_instance", "i-1", 90_000_000), idle("unattached_volume", "vol-1", 8_000_000)},
			Recommendations: []Recommendation{
				rightsize("i-1", "m6i.large", "m6i.medium", 61_320_000),
				rightsize("vol-1", "gp2 500 GB", "gp3 500 GB", 10_000_000),
				rightsize("i-2", "c6i.xlarge", "c6i.large", 40_000_000),
			},
			Coverage: &Coverage{CoveredBP: 7_100, Suggestion: "a 1-year Compute Savings Plan of $4/hour", EstimatedMonthly: 610_000_000},
		},
		WantTotal: 140_000_000,
		WantItems: 3,
	}
	items := EvaluateSavings(fx.Inputs, SavingsOptions{})
	var views []SavingView
	byResource := map[string]Saving{}
	for _, it := range items {
		views = append(views, SavingView{Saving: it, Status: "open"})
		for _, id := range it.ResourceIDs {
			if _, dup := byResource[id]; dup {
				t.Fatalf("resource %s counted twice", id)
			}
			byResource[id] = it
		}
	}
	i1 := byResource["i-1"]
	if i1.Kind != SavingIdle || i1.Monthly != 90_000_000 || len(i1.Alternatives) != 1 || i1.Alternatives[0].Monthly != 61_320_000 || i1.Alternatives[0].Kind != SavingRightsize {
		t.Fatalf("i-1 %+v", i1)
	}
	if v := byResource["vol-1"]; v.Kind != SavingRightsize || v.Monthly != 10_000_000 || len(v.Alternatives) != 1 || v.Alternatives[0].Kind != SavingIdle {
		t.Fatalf("vol-1 %+v", v)
	}
	var commitment *Saving
	for i := range items {
		if items[i].Kind == SavingCommitment {
			commitment = &items[i]
		}
	}
	if commitment == nil || !commitment.BeforeOtherSavings {
		t.Fatalf("commitment %+v", commitment)
	}
	total := savingsTotal(views)
	if total.Monthly != fx.WantTotal || total.Count != fx.WantItems {
		t.Fatalf("total %+v, want %d over %d items", total, fx.WantTotal, fx.WantItems)
	}
}

// A commitment alone leaves the savings total at zero (it is never added),
// but the total names it on its own so the zero is not read as "nothing
// found".
func TestSavingsTotalNamesTheCommitmentItLeavesOut(t *testing.T) {
	last := MustDate("2026-11-10")
	src := &fakeSource{
		status: SourceStatus{Enabled: true, Currency: "USD", LastCompleteDay: last, HistoryStart: MustDate("2026-09-01"),
			Accounts: []Account{{ID: "a", Name: "Harness", Kind: "connected", Readable: true}}},
		lines: []CostLine{{Day: last, Account: "a", Service: "compute.vm", UsageType: "BoxUsage", RecordType: RecordUsage, NetAmortizedMicros: usd(30)}},
		savings: SavingsInputs{Coverage: &Coverage{CoveredBP: 0, EstimatedMonthly: usd(441),
			Suggestion: "a 3-year compute commitment of $0.626/h"}},
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	v, err := NewService(src, newMemStore(), Config{Now: func() time.Time { return now }}).Savings(context.Background(), Query{})
	if err != nil || len(v.Items) != 1 || !v.Items[0].BeforeOtherSavings {
		t.Fatalf("want the commitment item: %v %+v", err, v.Items)
	}
	want := SavingsTotal{Monthly: 0, Count: 0, CommitmentMonthly: usd(441), CommitmentCount: 1}
	if v.Total != want {
		t.Fatalf("total = %+v, want %+v", v.Total, want)
	}
}
