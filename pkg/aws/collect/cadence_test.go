package collect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// The cadence table: a healthy health unit every 10 minutes, an unhealthy
// one every 2 until it has been clear for an hour; full inventory every 6
// hours, identity hourly, trails and Regions that are off daily; the daily
// target stretches inventory and policies and never health, changes or
// identity.
func TestCadenceTable(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	unit := func(service string) model.Unit {
		region := testRegion
		for _, entry := range specs {
			if entry.service == service && entry.global {
				region = model.GlobalRegion
			}
		}
		return model.Unit{Account: productionAccount, Region: region, Service: service}
	}
	hot := &unitState{hotUntil: now.Add(time.Minute), hotSince: now.Add(-time.Hour)}
	tooLong := &unitState{hotUntil: now.Add(time.Minute), hotSince: now.Add(-MaxHot)}
	cleared := &unitState{hotUntil: now.Add(-time.Second)}
	off := &unitState{off: true}
	for name, tc := range map[string]struct {
		service string
		state   *unitState
		factor  int
		want    time.Duration
	}{
		"healthy health unit":           {service: "elbv2", want: 10 * time.Minute},
		"unhealthy health unit":         {service: "elbv2", state: hot, want: 2 * time.Minute},
		"an hour clear":                 {service: "elbv2", state: cleared, want: 10 * time.Minute},
		"six hours hot":                 {service: "elbv2", state: tooLong, want: 10 * time.Minute},
		"health is never stretched":     {service: "cloudwatch", state: hot, factor: 4, want: 2 * time.Minute},
		"full inventory":                {service: "lambda", want: 6 * time.Hour},
		"inventory stretched":           {service: "lambda", factor: 2, want: 12 * time.Hour},
		"identity":                      {service: "iam", factor: 4, want: time.Hour},
		"policies":                      {service: "kms", want: 6 * time.Hour},
		"policies stretched":            {service: "s3-policy", factor: 2, want: 12 * time.Hour},
		"trails":                        {service: TrailsService, want: 24 * time.Hour},
		"changes":                       {service: snapshot.ChangeService, factor: 8, want: 5 * time.Minute},
		"a Region that is off":          {service: "elbv2", state: off, want: 24 * time.Hour},
		"an inventory unit that is off": {service: "lambda", state: off, factor: 2, want: 24 * time.Hour},
	} {
		got := cadenceOf(unit(tc.service), tc.state, Intervals{}, tc.factor, now)
		if got.Interval != tc.want {
			t.Errorf("%s: %s, want %s", name, got.Interval, tc.want)
		}
	}
	// A user's explicit health interval wins, and the fast cadence is
	// never slower than it.
	if got := cadenceOf(unit("ecs"), hot, Intervals{Health: time.Minute}, 1, now); got.Interval != time.Minute {
		t.Errorf("explicit health of 1m, hot = %s", got.Interval)
	}
	if got := cadenceOf(unit("lambda"), nil, Intervals{Inventory: 15 * time.Minute}, 1, now); got.Interval != 15*time.Minute {
		t.Errorf("explicit inventory of 15m = %s", got.Interval)
	}
}

// A unit that reports a problem is hot from that run for an hour: it stays
// on the 2-minute cadence while unhealthy and for an hour after its last
// unhealthy run, then goes back to 10 minutes.
func TestUnhealthyUnitRunsFastUntilAnHourClear(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return nil },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now))
	elb := model.Unit{Account: productionAccount, Region: testRegion, Service: "elbv2"}
	run := func(unhealthy bool) {
		scheduler.note(Report{Units: []UnitReport{{Unit: elb, Outcome: OutcomeCommitted, Unhealthy: unhealthy}}}, Intervals{}.Effective(), clock.Now())
	}
	run(false)
	if got := scheduler.Cadence(elb); got.Interval != 10*time.Minute || got.Hot {
		t.Fatalf("healthy = %+v", got)
	}
	run(true)
	for minute := 2; minute <= 20; minute += 2 {
		clock.Advance(2 * time.Minute)
		run(minute < 10)
	}
	// Last unhealthy at +8m; at +20m it has been clear for 12 minutes.
	if got := scheduler.Cadence(elb); got.Interval != 2*time.Minute || !got.Hot {
		t.Fatalf("12 minutes clear = %+v", got)
	}
	clock.Advance(47 * time.Minute)
	if got := scheduler.Cadence(elb); got.Interval != 2*time.Minute {
		t.Fatalf("59 minutes clear = %+v", got)
	}
	clock.Advance(2 * time.Minute)
	if got := scheduler.Cadence(elb); got.Interval != 10*time.Minute || got.Hot {
		t.Fatalf("an hour clear = %+v", got)
	}
}

// harnessAt is a harness whose collector, store and fake all read one
// clock, for scheduler runs over compressed time.
func harnessAt(t *testing.T, scenario string, clock *fakeClock, options ...Option) harness {
	t.Helper()
	h := newHarness(t, scenario, append([]Option{WithClock(clock.Now)}, options...)...)
	// Compressed time: the CloudTrail lane must not pace polls in real
	// seconds. Creating it first with a high ceiling keeps it that way.
	h.limiter.Lane(limit.CloudTrailLane, 1000)
	h.fake.SetClock(clock.Now)
	h.store = snapshot.New(h.provider, h.store.Scope(), snapshot.WithClock(clock.Now))
	h.collector.store = h.store
	return h
}

// unitRuns counts how often each unit attempted a read, from the ledger
// runs.
func unitRuns(t *testing.T, ledger *ledgerRecorder) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, row := range ledger.rows(t) {
		out[row.Region+"/"+row.Source] += row.Runs
	}
	return out
}

// On the scheduler: the unhealthy load balancer unit of the estate's
// workloads account runs every 2 minutes, a healthy unit every 10, and the
// full inventory once, in half an hour.
func TestSchedulerRunsUnhealthyUnitsOnTheFastCadence(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	ledger := newLedgerRecorder()
	h := harnessAt(t, fakeaws.ScenarioEstateHubSpoke, clock, WithCallLedger(ledger.record))
	tgt := target(fakeaws.EstateWorkloads, fakeaws.EstateHomeRegion)
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return []Target{tgt} },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now))
	for minute := 0; minute < 30; minute++ {
		scheduler.RunDue(context.Background())
		clock.Advance(time.Minute)
	}
	runs := unitRuns(t, ledger)
	home := fakeaws.EstateHomeRegion + "/"
	if runs[home+"elbv2"] != 15 {
		t.Errorf("unhealthy elbv2 ran %d times in 30 minutes, want 15", runs[home+"elbv2"])
	}
	if runs[home+"ecs"] != 3 {
		t.Errorf("healthy ecs ran %d times in 30 minutes, want 3", runs[home+"ecs"])
	}
	if runs[home+"cloudwatch"] != 3 {
		t.Errorf("healthy cloudwatch ran %d times in 30 minutes, want 3", runs[home+"cloudwatch"])
	}
	if runs[home+"lambda"] != 1 || runs[home+TrailsService] != 1 {
		t.Errorf("inventory ran lambda %d, trails %d times, want once", runs[home+"lambda"], runs[home+TrailsService])
	}
}

// changeFeed serves changes the test adds, as the change feed would.
type changeFeed struct {
	mu      sync.Mutex
	changes []model.Change
}

func (feed *changeFeed) add(change model.Change) {
	feed.mu.Lock()
	feed.changes = append(feed.changes, change)
	feed.mu.Unlock()
}

func (feed *changeFeed) read(_ context.Context, account string, since, until time.Time) ([]model.Change, error) {
	feed.mu.Lock()
	defer feed.mu.Unlock()
	var out []model.Change
	for _, change := range feed.changes {
		if change.Account == account && !change.Time.Before(since) && change.Time.Before(until) {
			out = append(out, change)
		}
	}
	return out, nil
}

// Between full reads the change feed re-reads only what changed: a changed
// function re-reads the function unit at the next pass and no other, a
// change to a load balancer puts its health unit on the fast cadence for
// an hour, a tag change to a bucket re-reads the bucket unit but not the
// metered policy read, and a policy change re-reads the policy unit.
func TestChangeFeedDrivesReReads(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	ledger := newLedgerRecorder()
	h := harnessAt(t, fakeaws.ScenarioBadDeploy, clock, WithCallLedger(ledger.record))
	feed := &changeFeed{}
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return []Target{target(productionAccount)} },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now), WithChangeFeed(feed.read))
	scheduler.RunDue(context.Background())
	start := unitRuns(t, ledger)
	change := func(id, service, name, region string) {
		feed.add(model.Change{ID: id, Time: clock.Now().Add(-time.Minute), Account: productionAccount, Region: region, Service: service, EventName: name})
	}
	clock.Advance(5 * time.Minute)
	change("1", "lambda", "UpdateFunctionConfiguration20150331v2", testRegion)
	change("2", "s3", "PutBucketTagging", testRegion)
	change("3", "rds", "ModifyDBInstance", testRegion)
	scheduler.RunDue(context.Background())
	after := unitRuns(t, ledger)
	diff := func(key string) int64 { return after[key] - start[key] }
	if diff(testRegion+"/lambda") != 1 || diff(model.GlobalRegion+"/s3") != 1 || diff(testRegion+"/tagging") != 1 {
		t.Errorf("changed units re-read: lambda %d, s3 %d, tagging %d", diff(testRegion+"/lambda"), diff(model.GlobalRegion+"/s3"), diff(testRegion+"/tagging"))
	}
	for _, key := range []string{testRegion + "/sqs", testRegion + "/vpc", model.GlobalRegion + "/s3-policy", model.GlobalRegion + "/iam"} {
		if diff(key) != 0 {
			t.Errorf("%s ran %d times with no change", key, diff(key))
		}
	}
	rds := model.Unit{Account: productionAccount, Region: testRegion, Service: "rds"}
	if got := scheduler.Cadence(rds); !got.Hot || got.Interval != 2*time.Minute {
		t.Errorf("rds after a change = %+v", got)
	}
	// The same change seen again on the next scan is not acted on twice.
	clock.Advance(5 * time.Minute)
	scheduler.RunDue(context.Background())
	again := unitRuns(t, ledger)
	if again[testRegion+"/lambda"] != after[testRegion+"/lambda"] {
		t.Error("a change was acted on twice")
	}
	clock.Advance(5 * time.Minute)
	change("4", "s3", "PutBucketPolicy", testRegion)
	scheduler.RunDue(context.Background())
	if got := unitRuns(t, ledger)[model.GlobalRegion+"/s3-policy"]; got != start[model.GlobalRegion+"/s3-policy"]+1 {
		t.Errorf("a bucket policy change re-read the policy unit %d times", got-start[model.GlobalRegion+"/s3-policy"])
	}
}

func TestStretchFactor(t *testing.T) {
	for _, tc := range []struct {
		calls  int64
		target int
		want   int
	}{{0, 20000, 1}, {20000, 20000, 1}, {20001, 20000, 2}, {30000, 20000, 2}, {30001, 20000, 4}, {60000, 20000, 4}, {60001, 20000, 8}, {50000, 0, 1}} {
		if got := StretchFactor(tc.calls, tc.target); got != tc.want {
			t.Errorf("StretchFactor(%d, %d) = %d, want %d", tc.calls, tc.target, got, tc.want)
		}
	}
}

// Over the daily call target the inventory and policy reads slow down
// first; health keeps its cadence.
func TestDailyTargetStretchesInventoryBeforeHealth(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)}
	ledger := newLedgerRecorder()
	h := harnessAt(t, fakeaws.ScenarioBadDeploy, clock, WithCallLedger(ledger.record))
	over := map[string]int64{productionAccount: 25000}
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return []Target{target(productionAccount)} },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now),
		WithCallTarget(func() int { return 20000 }, func(context.Context, time.Time) (map[string]int64, error) { return over, nil }))
	for minute := 0; minute < 13*60; minute += 10 {
		scheduler.RunDue(context.Background())
		clock.Advance(10 * time.Minute)
	}
	runs := unitRuns(t, ledger)
	if runs[testRegion+"/lambda"] != 2 {
		t.Errorf("stretched inventory ran %d times in 13 hours, want 2 (every 12h)", runs[testRegion+"/lambda"])
	}
	if runs[testRegion+"/ec2"] != 78 {
		t.Errorf("health ran %d times in 13 hours, want 78 (every 10m)", runs[testRegion+"/ec2"])
	}
	if runs[model.GlobalRegion+"/iam"] != 13 {
		t.Errorf("identity ran %d times in 13 hours, want 13", runs[model.GlobalRegion+"/iam"])
	}
	lambda := model.Unit{Account: productionAccount, Region: testRegion, Service: "lambda"}
	if got := scheduler.Cadence(lambda); !got.Stretched || got.Interval != 12*time.Hour {
		t.Errorf("lambda cadence = %+v", got)
	}
	over[productionAccount] = 100
	clock.Advance(totalsRefresh)
	if got := scheduler.Cadence(lambda); got.Stretched || got.Interval != 6*time.Hour {
		t.Errorf("under the target = %+v", got)
	}
}

// A change re-reads the units its API answers for, and only for events
// that can change them: instance churn does not re-read the network.
func TestUnitsForChange(t *testing.T) {
	services := func(service, name string) map[string]bool {
		out := map[string]bool{}
		for _, unit := range unitsForChange(model.Change{Account: productionAccount, Region: testRegion, Service: service, EventName: name}) {
			out[unit.Region+"/"+unit.Service] = true
		}
		return out
	}
	if got := services("ec2", "RunInstances"); len(got) != 1 || !got[testRegion+"/ec2"] {
		t.Errorf("RunInstances = %v", got)
	}
	if got := services("ec2", "AuthorizeSecurityGroupIngress"); !got[testRegion+"/vpc"] || got[testRegion+"/transit-gateway"] {
		t.Errorf("security group change = %v", got)
	}
	if got := services("ec2", "CreateTransitGatewayVpcAttachment"); !got[testRegion+"/transit-gateway"] || !got[testRegion+"/vpc"] {
		t.Errorf("attachment = %v", got)
	}
	if got := services("s3", "PutBucketTagging"); !got[model.GlobalRegion+"/s3"] || got[model.GlobalRegion+"/s3-policy"] || !got[testRegion+"/tagging"] {
		t.Errorf("bucket tags = %v", got)
	}
	if got := services("kms", "PutKeyPolicy"); !got[testRegion+"/kms"] {
		t.Errorf("key policy = %v", got)
	}
	if got := services("kms", "Encrypt"); len(got) != 0 {
		t.Errorf("a key use re-read %v", got)
	}
	// A service the change list names after its API reaches the unit
	// that reads it.
	if got := services("servicecatalog", "AssociateResource"); !got[testRegion+"/appregistry"] {
		t.Errorf("an application change = %v", got)
	}
	if got := services("cloudtrail", "PutEventSelectors"); !got[testRegion+"/"+TrailsService] {
		t.Errorf("a trail change = %v", got)
	}
	if got := services("ssm", "PutParameter"); len(got) != 0 {
		t.Errorf("a service Versus does not read re-read %v", got)
	}
}
