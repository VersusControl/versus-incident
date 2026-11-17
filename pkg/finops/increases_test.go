package finops

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The band is the wider of 3×MAD and 20% of the median. A noisy baseline
// ($100/$140 alternating: median $120, MAD $20) has a 3×MAD band of $60,
// wider than 20% ($24): a day at $170 is inside the band, $181 is above it.
func TestIncreaseBandUsesTheWiderOfMADAndRelative(t *testing.T) {
	start := MustDate("2026-09-01")
	noisy := func(step Micros) []CostLine {
		vals := make([]Micros, 17)
		for i := range vals {
			vals[i] = usd(100)
			if i%2 == 1 {
				vals[i] = usd(140)
			}
			if i >= 14 {
				vals[i] = step
			}
		}
		return seriesLines(start, vals, RecordUsage)
	}
	// Day 16 is the first day with 14 baseline days and 3 trigger days.
	last := start.AddDays(16)
	if got := DetectIncreases(noisy(usd(170)), last, hist(start), DetectOptions{}); len(got) != 0 {
		t.Fatalf("$170 is inside the 3×MAD band of a noisy line: %+v", got)
	}
	got := DetectIncreases(noisy(usd(181)), last, hist(start), DetectOptions{})
	if len(got) != 1 {
		t.Fatalf("$181 is above the 3×MAD band: %+v", got)
	}
	if got[0].DailyBefore != usd(120) || got[0].Threshold != usd(180) || got[0].StartedOn != start.AddDays(14) {
		t.Fatalf("baseline median $120, threshold $180: %+v", got[0])
	}
}

// Three days above the band must be consecutive: a day back inside the band
// restarts the count, and the increase starts on the first day of the run
// that completes it.
func TestIncreaseNeedsThreeConsecutiveDays(t *testing.T) {
	start := MustDate("2026-09-01")
	vals := flatThenStep(usd(20), usd(20), 26, 0)
	for _, i := range []int{20, 21, 23, 24, 25} {
		vals[i] = usd(85)
	}
	lines := seriesLines(start, vals, RecordUsage)
	if got := DetectIncreases(lines, start.AddDays(24), hist(start), DetectOptions{}); len(got) != 0 {
		t.Fatalf("up, up, back, up, up is not three in a row: %+v", got)
	}
	got := DetectIncreases(lines, start.AddDays(25), hist(start), DetectOptions{})
	if len(got) != 1 || got[0].StartedOn != start.AddDays(23) {
		t.Fatalf("the run that completes three days starts the increase: %+v", got)
	}
}

// The added cost must reach the minimum: equal to it triggers, one
// micro-dollar a day less does not.
func TestIncreaseMinimumImpactBoundary(t *testing.T) {
	start := MustDate("2026-09-01")
	detect := func(after Micros, o DetectOptions) []Detected {
		lines := seriesLines(start, flatThenStep(usd(10), after, 20, 17), RecordUsage)
		return DetectIncreases(lines, start.AddDays(19), hist(start), o)
	}
	// 25 projection days × $4/day = exactly $100/month.
	exact := DetectOptions{ProjectionDays: 25}
	if got := detect(usd(14), exact); len(got) != 1 || got[0].MonthlyImpact != usd(100) {
		t.Fatalf("an impact equal to the minimum triggers: %+v", got)
	}
	if got := detect(usd(14)-1, exact); len(got) != 0 {
		t.Fatalf("an impact just under the minimum does not: %+v", got)
	}
	// Defaults: $100/month over 30 days.
	if got := detect(13_333_334, DetectOptions{}); len(got) != 1 {
		t.Fatal("$3.333334/day × 30 is over $100")
	}
	if got := detect(13_333_333, DetectOptions{}); len(got) != 0 {
		t.Fatal("$3.333333/day × 30 is under $100")
	}
}

// Resolution needs three consecutive days back inside the band; a day up
// again restarts the count.
func TestIncreaseResolveNeedsThreeConsecutiveDays(t *testing.T) {
	start := MustDate("2026-09-01")
	vals := flatThenStep(usd(20), usd(85), 36, 20)
	for _, i := range []int{30, 31, 33, 34, 35} {
		vals[i] = usd(20)
	}
	lines := seriesLines(start, vals, RecordUsage)
	got := DetectIncreases(lines, start.AddDays(34), hist(start), DetectOptions{})
	if len(got) != 1 || !got[0].ResolvedOn.IsZero() {
		t.Fatalf("back, back, up, back, back is still open: %+v", got)
	}
	got = DetectIncreases(lines, start.AddDays(35), hist(start), DetectOptions{})
	if len(got) != 1 || got[0].ResolvedOn != start.AddDays(35) {
		t.Fatalf("three days back in a row resolves on the third: %+v", got)
	}
}

// A partial day neither triggers nor resolves: AWS may report a low,
// unfinished figure for today.
func TestPartialDaysNeverResolve(t *testing.T) {
	start := MustDate("2026-09-01")
	vals := flatThenStep(usd(20), usd(85), 30, 20)
	vals = append(vals, usd(1), usd(1), usd(1)) // days 30..32 are partial
	lines := seriesLines(start, vals, RecordUsage)
	got := DetectIncreases(lines, start.AddDays(29), hist(start), DetectOptions{})
	if len(got) != 1 || !got[0].ResolvedOn.IsZero() || got[0].DailyNow != usd(85) {
		t.Fatalf("partial days must not resolve or lower the increase: %+v", got)
	}
}

// An increase that started and resolved last month is read from this
// month: its title and sentence state its own change over its own days and
// that it resolved, never "went up $0 this month"; its monthly impact is
// the level it ran at, not the below-baseline window that resolved it.
func TestResolvedIncreaseFromLastMonthStatesItsOwnChange(t *testing.T) {
	start := MustDate("2026-09-01")
	stepOn, backOn, last := MustDate("2026-10-20"), MustDate("2026-10-27"), MustDate("2026-11-10")
	var lines []CostLine
	for d := start; !d.After(last); d = d.AddDays(1) {
		cost := usd(20)
		switch {
		case !d.Before(backOn):
			cost = usd(15)
		case !d.Before(stepOn):
			cost = usd(85)
		}
		lines = append(lines, CostLine{Day: d, Account: "a", Region: "us-east-1", Service: "compute.vm",
			UsageType: "BoxUsage", RecordType: RecordUsage, NetAmortizedMicros: cost})
	}
	src := &fakeSource{lines: lines, status: SourceStatus{Enabled: true, Currency: "USD", LastCompleteDay: last,
		HistoryStart: start, Accounts: []Account{{ID: "a", Name: "Harness", Kind: "connected", Readable: true}}}}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	svc := NewService(src, newMemStore(), Config{Now: func() time.Time { return now }})
	ctx := context.Background()
	if _, err := svc.SyncIncreases(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := svc.Increases(ctx, Query{Period: PeriodMonthToDate, Status: "all"})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("want one increase: %v %+v", err, list.Items)
	}
	it := list.Items[0]
	if it.Status != StatusResolved || it.ResolvedOn != MustDate("2026-10-29") || it.PeriodChange != 0 {
		t.Fatalf("fixture: resolved Oct 29 with no November change: %+v", it)
	}
	// Oct 20..26 ran $65 a day above the $20 baseline; Oct 27..29 ran $5 below.
	own := usd(65*7 - 5*3)
	if it.StatedChange != own {
		t.Fatalf("stated change = %d, want the increase's own %d", it.StatedChange, own)
	}
	wantTitle := "went up $440 over 10 days before it resolved on Oct 29"
	if it.Title != wantTitle {
		t.Fatalf("title = %q, want %q", it.Title, wantTitle)
	}
	if strings.Contains(it.Sentence, "$0") || strings.Contains(it.Sentence, "this month") ||
		!strings.Contains(it.Sentence, wantTitle) {
		t.Fatalf("sentence = %q", it.Sentence)
	}
	if it.Monthly != usd(65*30) || it.DailyNow != usd(85) {
		t.Fatalf("resolved impact = %d (daily now %d), want the level it ran at", it.Monthly, it.DailyNow)
	}
}

// The billed usage signal's label names the quantity and its unit the
// measure per day, in plain words: "instances" with "hours/day", never
// "instances billed per day" beside the provider's "Hrs/day".
func TestBilledUsageSignalLabelAndUnitAgree(t *testing.T) {
	start := MustDate("2026-09-01")
	stepOn, last := MustDate("2026-10-20"), MustDate("2026-11-10")
	var lines []CostLine
	for d := start; !d.After(last); d = d.AddDays(1) {
		cost, hours := usd(20), int64(93)
		if !d.Before(stepOn) {
			cost, hours = usd(85), 122
		}
		lines = append(lines, CostLine{Day: d, Account: "a", Region: "us-east-1", Service: "compute.vm",
			UsageType: "BoxUsage:m7i.large", RecordType: RecordUsage, UsageGroup: "instances", Unit: "Hrs",
			NetAmortizedMicros: cost, QuantityMicros: hours * MicrosPerUnit})
	}
	src := &fakeSource{lines: lines, status: SourceStatus{Enabled: true, Currency: "USD", LastCompleteDay: last,
		HistoryStart: start, Accounts: []Account{{ID: "a", Name: "Harness", Kind: "connected", Readable: true}}}}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	svc := NewService(src, newMemStore(), Config{Now: func() time.Time { return now }})
	ctx := context.Background()
	recs, err := svc.SyncIncreases(ctx)
	if err != nil || len(recs) != 1 {
		t.Fatalf("want one increase: %v %+v", err, recs)
	}
	v, err := svc.Increase(ctx, Query{Period: PeriodMonthToDate}, recs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	want := UsageSignal{Label: "instances", Before: 93 * MicrosPerUnit, After: 122 * MicrosPerUnit, Unit: "hours/day"}
	if v.UsageSignal == nil || *v.UsageSignal != want {
		t.Fatalf("usage signal = %+v, want %+v", v.UsageSignal, want)
	}
}

// An increase whose change inside the period would print as "$0" (here
// $0.004 on the day it resolved) is worded over its own days instead, and
// the stated amount is its own change, never a near-zero period amount.
func TestIncreaseWithAPeriodChangeThatPrintsAsZeroUsesItsOwnDays(t *testing.T) {
	start := MustDate("2026-09-01")
	stepOn, backOn, resolvedOn, last := MustDate("2026-10-20"), MustDate("2026-10-30"), MustDate("2026-11-01"), MustDate("2026-11-10")
	var lines []CostLine
	for d := start; !d.After(last); d = d.AddDays(1) {
		cost := usd(20)
		switch {
		case d == resolvedOn:
			cost = usd(20) + 4_000
		case !d.Before(backOn):
		case !d.Before(stepOn):
			cost = usd(85)
		}
		lines = append(lines, CostLine{Day: d, Account: "a", Region: "us-east-1", Service: "compute.vm",
			UsageType: "BoxUsage", RecordType: RecordUsage, NetAmortizedMicros: cost})
	}
	src := &fakeSource{lines: lines, status: SourceStatus{Enabled: true, Currency: "USD", LastCompleteDay: last,
		HistoryStart: start, Accounts: []Account{{ID: "a", Name: "Harness", Kind: "connected", Readable: true}}}}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	svc := NewService(src, newMemStore(), Config{Now: func() time.Time { return now }})
	ctx := context.Background()
	if _, err := svc.SyncIncreases(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := svc.Increases(ctx, Query{Period: PeriodMonthToDate, Status: "all"})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("want one increase: %v %+v", err, list.Items)
	}
	it := list.Items[0]
	if it.ResolvedOn != resolvedOn || it.PeriodChange != 4_000 {
		t.Fatalf("fixture: resolved Nov 1 with $0.004 in November: %+v", it)
	}
	if own := usd(65*10) + 4_000; it.StatedChange != own {
		t.Fatalf("stated change = %d, want the increase's own %d", it.StatedChange, own)
	}
	if want := "went up $650 over 13 days before it resolved on Nov 1"; it.Title != want {
		t.Fatalf("title = %q, want %q", it.Title, want)
	}
}
