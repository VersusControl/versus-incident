package collect

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// sizingDay is the day of the sizing account (1,250 resources, 19 target
// groups, 3 Regions with resources, 15 Regions off) as the cost plan
// records it, measured on the default cadences: about 15,500 calls. The
// plan's first estimate was 14,000; health came in at about 7,600 against
// 5,000, because each health unit lists what it watches on every run.
const sizingDay = 15500

// A compressed day of the sizing account on the default cadences: the day
// lands within 20% of the recorded sizing and under the account's daily
// target, and no run of any source goes past its per-run budget.
func TestLargeAccountDayMatchesTheSizingTable(t *testing.T) {
	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	ledger := newLedgerRecorder()
	h := harnessAt(t, fakeaws.ScenarioSizingLarge, clock, WithCallLedger(ledger.record),
		WithOptions(func() Options { return Options{CallBudgets: DefaultCallBudgets()} }))
	regions := append(fakeaws.SizingRegions(), fakeaws.SizingOffRegions()...)
	tgt := target(productionAccount, regions...)
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return []Target{tgt} },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now))
	for minute := 0; minute < 24*60; minute++ {
		scheduler.RunDue(context.Background())
		clock.Advance(time.Minute)
	}

	off := map[string]bool{}
	for _, region := range fakeaws.SizingOffRegions() {
		off[region] = true
	}
	bySource := map[string]int64{}
	peak := map[string]int64{}
	var day int64
	for _, row := range ledger.rows(t) {
		if row.Hour.Before(start) || !row.Hour.Before(start.Add(24*time.Hour)) {
			continue
		}
		source := SourceOf(row.Source)
		peak[source] = max(peak[source], row.Peak)
		if off[row.Region] {
			source = "regions_off"
		}
		bySource[source] += row.Calls
		day += row.Calls
	}
	t.Logf("sizing account day: %d calls, by source %v, largest run by source %v", day, bySource, peak)
	if math.Abs(float64(day-sizingDay)) > 0.2*sizingDay {
		t.Errorf("the day's %d calls are more than 20%% from the recorded %d", day, sizingDay)
	}
	if day > 20000 {
		t.Errorf("%d calls a day, over the 20,000 daily target", day)
	}
	for source, budget := range DefaultCallBudgets() {
		if peak[source] > int64(budget) {
			t.Errorf("a %s run made %d calls, over its budget of %d", source, peak[source], budget)
		}
	}
	// Every Region with resources read cleanly; only AWS Health is off the
	// support plan.
	statuses, err := h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Sync.ErrorCode != "" && !off[status.Unit.Region] && status.Unit.Service != "aws-health" {
			t.Errorf("%s failed: %s", status.Unit.Key(), status.Sync.ErrorCode)
		}
	}
}
