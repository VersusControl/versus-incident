package collect

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// estateRegions are the three Regions the cost run reads: the workload
// Region, the baseline Region and us-east-1.
var estateRegions = []string{fakeaws.EstateHomeRegion, fakeaws.EstateAggregationRegion, fakeaws.EstateGlobalRegion}

// A day of the estate, compressed: four accounts in three Regions on the
// default cadences, one scheduler pass a minute. The ledger's calls for the
// day land within 20% of the estimate the cadence table gives from one
// steady pass of each unit, and every account stays under the daily
// target.
func TestEstateDayOfCallsMatchesTheEstimate(t *testing.T) {
	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	ledger := newLedgerRecorder()
	h := harnessAt(t, fakeaws.ScenarioEstateHubSpoke, clock, WithCallLedger(ledger.record))
	var targets []Target
	for _, account := range fakeaws.EstateAccounts() {
		targets = append(targets, target(account, estateRegions...))
	}
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return targets },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now))

	day := 24 * time.Hour
	for minute := 0; minute < 24*60; minute++ {
		scheduler.RunDue(context.Background())
		clock.Advance(time.Minute)
	}

	// The estimate: each unit's calls on a steady run times how often its
	// cadence runs it in a day. A run's calls are the ledger's calls over
	// runs in an hour; the first run reads a day of CloudTrail history,
	// so each unit's smallest hour counts.
	perRun := map[string]float64{}
	for _, row := range ledger.rows(t) {
		key := model.Unit{Account: row.AccountID, Region: row.Region, Service: row.Source}.Key()
		calls := float64(row.Calls) / float64(row.Runs)
		if known, ok := perRun[key]; !ok || calls < known {
			perRun[key] = calls
		}
	}
	var estimate float64
	for _, account := range fakeaws.EstateAccounts() {
		for _, unit := range Plan(account, estateRegions) {
			estimate += perRun[unit.Key()] * float64(day) / float64(scheduler.Cadence(unit).Interval)
		}
	}

	perAccount := map[string]int64{}
	var total int64
	for _, row := range ledger.rows(t) {
		if row.Hour.Before(start) || !row.Hour.Before(start.Add(day)) {
			continue
		}
		perAccount[row.AccountID] += row.Calls
		total += row.Calls
	}
	t.Logf("estate day: %d calls (%v per account), estimate %.0f", total, perAccount, estimate)
	if math.Abs(float64(total)-estimate) > 0.2*estimate {
		t.Errorf("the day's ledger has %d calls, more than 20%% from the estimate %.0f", total, estimate)
	}
	for account, calls := range perAccount {
		if calls > 20000 {
			t.Errorf("account %s made %d calls in a day, over the 20,000 target", account, calls)
		}
	}
	if len(perAccount) != 4 {
		t.Fatalf("ledger accounts = %v", perAccount)
	}
}

// A source far larger than any budget stays bounded by it: 40,000
// generated resources, served in pages of 100. A list runs to the end up
// to five times its budget, and past that the unit keeps its last read; a
// per-item detail call past the budget is skipped and named as capped
// with counts. No run, and no hour of the ledger, goes past the ceiling.
// The gate is written for 40,000 imported findings; no findings collector
// exists yet, so the inventory stands in for it until one does.
func TestFortyThousandItemsStayInsideTheBudgetOnInventoryUntilFindingsAreRead(t *testing.T) {
	const budget = 50
	ceiling := listCeiling * budget
	ledger := newLedgerRecorder()
	h := newHarness(t, fakeaws.ScenarioLoad5k, WithCallLedger(ledger.record),
		WithOptions(func() Options { return Options{CallBudgets: map[string]int{SourceInventory: budget}} }))
	h.fake.SetLoadSize(40000)
	report := mustSync(t, h, target(productionAccount), KindInventory)
	capped := 0
	for _, unit := range report.Units {
		if unit.Calls > ceiling {
			t.Errorf("%s made %d calls past the ceiling of %d", unit.Unit.Key(), unit.Calls, ceiling)
		}
		if unit.ErrorCode != CodeCapped {
			continue
		}
		capped++
		state := syncState(t, h, productionAccount, unit.Unit.Service)
		found := false
		for _, gap := range state.Gaps {
			found = found || gap.Reason == model.GapCapped && gap.Seen > 0
		}
		if !found {
			t.Errorf("%s capped without a counted gap: %+v", unit.Unit.Key(), state.Gaps)
		}
	}
	if capped == 0 {
		t.Fatal("no unit reached its budget on 40,000 resources")
	}
	for _, row := range ledger.rows(t) {
		if row.Peak > int64(ceiling) {
			t.Errorf("ledger %s/%s: a run of %d calls past the ceiling", row.Region, row.Source, row.Peak)
		}
	}
}

// Nothing changes for 96 compressed health cycles (16 hours): the calls of
// every cycle repeat with the 6-hour period of the slowest cadence, and
// every cycle without an hourly or full read costs the same. The estate's
// load balancer stays unhealthy throughout, so it runs on the fast cadence
// for its first six hours and on the normal one after: two flat stretches,
// one step down.
func TestQuietEstateCallsStayFlatPerCycle(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)}
	h := harnessAt(t, fakeaws.ScenarioEstateHubSpoke, clock)
	tgt := target(fakeaws.EstateWorkloads, estateRegions...)
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return []Target{tgt} },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now))
	const cycles = 96
	perCycle := make([]int64, cycles)
	for cycle := 0; cycle < cycles; cycle++ {
		before := h.fake.TotalRequests()
		for minute := 0; minute < 10; minute++ {
			scheduler.RunDue(context.Background())
			clock.Advance(time.Minute)
		}
		perCycle[cycle] = h.fake.TotalRequests() - before
	}
	const period = 36 // 6 hours of 10-minute cycles
	// The fast spell ends at 6 hours, in cycle 36.
	for cycle := period + 1; cycle+period < cycles; cycle++ {
		if perCycle[cycle] != perCycle[cycle+period] {
			t.Errorf("cycle %d made %d calls, cycle %d made %d", cycle, perCycle[cycle], cycle+period, perCycle[cycle+period])
		}
	}
	quiet := func(from, to int) int64 {
		plain := int64(-1)
		for cycle := from; cycle < to; cycle++ {
			if cycle%6 == 0 {
				continue // an hourly identity read, or a full read at 6 h
			}
			if plain < 0 {
				plain = perCycle[cycle]
			}
			if perCycle[cycle] != plain {
				t.Errorf("quiet cycle %d made %d calls, others %d", cycle, perCycle[cycle], plain)
			}
		}
		return plain
	}
	hot, cooled := quiet(1, period), quiet(period+1, cycles)
	if cooled >= hot {
		t.Errorf("after six hours hot a quiet cycle costs %d calls, before %d: the fast cadence did not end", cooled, hot)
	}
	t.Logf("calls per cycle: first %d, quiet %d while hot, %d after, all %v", perCycle[0], hot, cooled, perCycle)
}
