package finops

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
)

// TestFinOpsAmountOverflowIsClassed proves an unrepresentable total is a
// closed error, never a wrapped number.
func TestFinOpsAmountOverflowIsClassed(t *testing.T) {
	svc, src, _ := newStory(t)
	day := MustDate("2026-10-05")
	src.lines = append(src.lines,
		CostLine{Day: day, Account: acctProd, Service: "compute.ec2", UsageType: "max-a", RecordType: RecordUsage, NetAmortizedMicros: math.MaxInt64},
		CostLine{Day: day, Account: acctProd, Service: "compute.ec2", UsageType: "max-b", RecordType: RecordUsage, NetAmortizedMicros: math.MaxInt64},
	)
	ctx := context.Background()
	if v, err := svc.Summary(ctx, october); !errors.Is(err, ErrAmountOverflow) || !errors.Is(err, ErrMoneyOverflow) || v.Total != 0 {
		t.Fatalf("summary total %d, error %v: want the classed overflow and no total", v.Total, err)
	}
	if _, err := svc.Drivers(ctx, october); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("drivers error = %v", err)
	}
	if _, err := svc.Trend(ctx, october); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("trend error = %v", err)
	}
	// One extreme line is representable: it is answered exactly, not refused.
	src.lines = []CostLine{{Day: day, Account: acctProd, Service: "compute.ec2", UsageType: "min", RecordType: RecordUsage, NetAmortizedMicros: math.MinInt64}}
	v, err := svc.Summary(ctx, october)
	if err != nil || v.Total != math.MinInt64 {
		t.Fatalf("summary with one extreme line = %d, %v", v.Total, err)
	}
}

// TestFinOpsIncreasesSkipStoredWithoutCostLines keeps the list and the drill
// in agreement: a stored increase whose account has no cost lines left is
// not listed and is not found.
func TestFinOpsIncreasesSkipStoredWithoutCostLines(t *testing.T) {
	svc, _, st := newStory(t)
	old := MustDate("2026-01-15")
	orphan := IncreaseRecord{Detected: Detected{ID: "inc-orphan", Key: LineKey{Account: "999999999999", Service: "compute.ec2", RecordType: RecordUsage},
		StartedOn: old, MonthlyImpact: usd(480)}, Status: StatusOpen}
	kept := IncreaseRecord{Detected: Detected{ID: "inc-kept", Key: LineKey{Account: acctProd, Service: "compute.ec2", RecordType: RecordUsage},
		StartedOn: old, MonthlyImpact: usd(10)}, Status: StatusOpen}
	st.increases[orphan.ID] = orphan
	st.increases[kept.ID] = kept
	ctx := context.Background()
	q := Query{Period: "2026-10", Status: "all"}
	v, err := svc.Increases(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, item := range v.Items {
		listed = append(listed, item.ID)
	}
	if !slices.Contains(listed, "inc-kept") || slices.Contains(listed, "inc-orphan") {
		t.Fatalf("listed increases = %v", listed)
	}
	if _, err := svc.Increase(ctx, q, "inc-orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drill for the orphaned increase = %v, want not found", err)
	}
	if _, err := svc.Increase(ctx, q, "inc-kept"); err != nil {
		t.Fatalf("drill for a stored increase with cost lines = %v", err)
	}
}
