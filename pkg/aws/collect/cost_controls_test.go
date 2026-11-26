package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// A health unit leaves the fast cadence at the exact instant its last
// problem is an hour old, not a tick later and not a tick sooner, and a
// change to its service seen after that puts it back on the fast cadence
// for an hour from the change. A change already an hour old does not.
func TestHealthFastCadenceEndsAtTheHourAndReturnsOnANewChange(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	feed := &changeFeed{}
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return nil },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now), WithChangeFeed(feed.read))
	elb := model.Unit{Account: productionAccount, Region: testRegion, Service: "elbv2"}
	intervals := Intervals{}.Effective()
	run := func(unhealthy bool) {
		scheduler.note(Report{Units: []UnitReport{{Unit: elb, Outcome: OutcomeCommitted, Unhealthy: unhealthy}}}, intervals, clock.Now())
	}
	at := func(when time.Time) UnitCadence {
		clock.Advance(when.Sub(clock.Now()))
		return scheduler.Cadence(elb)
	}

	run(true)
	if got := at(start.Add(time.Hour - time.Nanosecond)); !got.Hot || got.Interval != 2*time.Minute {
		t.Fatalf("a nanosecond before the hour = %+v, want hot at 2m", got)
	}
	if got := at(start.Add(time.Hour)); got.Hot || got.Interval != 10*time.Minute {
		t.Fatalf("at the hour = %+v, want 10m", got)
	}
	// A clear run does not extend the fast cadence.
	run(false)
	if got := at(start.Add(time.Hour + time.Minute)); got.Hot {
		t.Fatalf("a clear run kept the unit hot: %+v", got)
	}

	// A change an hour and a second old is seen: nothing to do.
	now := start.Add(90 * time.Minute)
	clock.Advance(now.Sub(clock.Now()))
	feed.add(model.Change{ID: "old", Time: now.Add(-time.Hour - time.Second), Account: productionAccount, Region: testRegion, Service: "elbv2", EventName: "ModifyTargetGroup"})
	scheduler.scanChanges(context.Background(), productionAccount, intervals)
	if got := scheduler.Cadence(elb); got.Hot {
		t.Fatalf("a change over an hour old made the unit hot: %+v", got)
	}
	// A new change to the load balancer: hot for an hour from the change.
	changed := now.Add(-3 * time.Minute)
	feed.add(model.Change{ID: "new", Time: changed, Account: productionAccount, Region: testRegion, Service: "elbv2", EventName: "ModifyTargetGroup"})
	scheduler.scanChanges(context.Background(), productionAccount, intervals)
	if got := scheduler.Cadence(elb); !got.Hot || got.Interval != 2*time.Minute {
		t.Fatalf("after a new change = %+v, want hot at 2m", got)
	}
	if got := at(changed.Add(time.Hour - time.Nanosecond)); !got.Hot {
		t.Fatalf("a nanosecond before an hour after the change = %+v", got)
	}
	if got := at(changed.Add(time.Hour)); got.Hot || got.Interval != 10*time.Minute {
		t.Fatalf("an hour after the change = %+v, want 10m", got)
	}
}

// On the scheduler, a quiet health unit runs every 10 minutes; a change to
// its service puts it on the 2-minute cadence until the change is an hour
// old, and then it runs every 10 minutes again.
func TestSchedulerHeatsAHealthyUnitOnAChangeAndCoolsItAnHourLater(t *testing.T) {
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	ledger := newLedgerRecorder()
	h := harnessAt(t, fakeaws.ScenarioEstateHubSpoke, clock, WithCallLedger(ledger.record))
	feed := &changeFeed{}
	tgt := target(fakeaws.EstateWorkloads, fakeaws.EstateHomeRegion)
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return []Target{tgt} },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now), WithChangeFeed(feed.read))
	key := fakeaws.EstateHomeRegion + "/cloudwatch"
	runsUntil := func(minute int) int64 {
		for clock.Now().Before(start.Add(time.Duration(minute) * time.Minute)) {
			scheduler.RunDue(context.Background())
			clock.Advance(time.Minute)
		}
		return unitRuns(t, ledger)[key]
	}
	// Minutes 0 to 19: the healthy unit runs at 0 and 10.
	if got := runsUntil(20); got != 2 {
		t.Fatalf("quiet cloudwatch ran %d times in 20 minutes, want 2", got)
	}
	// At minute 20 the unit runs on its own cadence, then the change poll
	// sees an alarm changed a minute ago: hot until minute 79, so it runs
	// at 22, 24, ... 78.
	feed.add(model.Change{ID: "alarm", Time: clock.Now().Add(-time.Minute), Account: fakeaws.EstateWorkloads, Region: fakeaws.EstateHomeRegion,
		Service: "cloudwatch", EventName: "PutMetricAlarm"})
	before := runsUntil(21)
	if got := runsUntil(40) - before; got != 9 {
		t.Errorf("hot cloudwatch ran %d times in minutes 21-39, want 9 (every 2m)", got)
	}
	before = runsUntil(80)
	if got := runsUntil(100) - before; got != 2 {
		t.Errorf("cooled cloudwatch ran %d times in minutes 80-99, want 2 (every 10m)", got)
	}
}

// The daily target slows only inventory and policy reads. Two schedulers
// run the same account for 13 hours, one far over its target: every
// health, change, identity and trail unit runs exactly as often in both,
// and the full inventory and the policy reads run less often.
func TestDailyTargetLeavesHealthChangesIdentityAndTrailsAlone(t *testing.T) {
	runDay := func(over bool) map[string]int64 {
		clock := &fakeClock{now: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)}
		ledger := newLedgerRecorder()
		h := harnessAt(t, fakeaws.ScenarioBadDeploy, clock, WithCallLedger(ledger.record))
		options := []SchedulerOption{WithSchedulerClock(clock.Now)}
		if over {
			options = append(options, WithCallTarget(func() int { return 20000 },
				func(context.Context, time.Time) (map[string]int64, error) {
					return map[string]int64{productionAccount: 200000}, nil
				}))
		}
		scheduler := NewScheduler(h.collector, func(context.Context) []Target { return []Target{target(productionAccount)} },
			func() Intervals { return Intervals{} }, nil, options...)
		for step := 0; step < 13*60; step += 5 {
			scheduler.RunDue(context.Background())
			clock.Advance(5 * time.Minute)
		}
		return unitRuns(t, ledger)
	}
	normal, stretched := runDay(false), runDay(true)
	if len(normal) == 0 || !reflect.DeepEqual(keysOf(normal), keysOf(stretched)) {
		t.Fatalf("unit sets differ: %v vs %v", keysOf(normal), keysOf(stretched))
	}
	slowed := 0
	for key, runs := range normal {
		_, service := splitRunKey(key)
		protected := service == serviceTrails
		switch SourceOf(service) {
		case SourceHealth, SourceChanges, SourceIdentity:
			protected = true
		}
		switch {
		case protected && stretched[key] != runs:
			t.Errorf("%s ran %d times over the target, %d under it", key, stretched[key], runs)
		case !protected && stretched[key] >= runs:
			t.Errorf("%s ran %d times over the target, %d under it: not stretched", key, stretched[key], runs)
		case !protected:
			slowed++
		}
	}
	if normal[testRegion+"/"+snapshot.ChangeService] < 100 || normal[testRegion+"/ec2"] < 60 {
		t.Fatalf("the run did not exercise changes and health: %v", normal)
	}
	if slowed == 0 {
		t.Fatal("nothing was stretched")
	}
}

func keysOf(runs map[string]int64) map[string]bool {
	out := make(map[string]bool, len(runs))
	for key := range runs {
		out[key] = true
	}
	return out
}

func splitRunKey(key string) (region, service string) {
	for index := 0; index < len(key); index++ {
		if key[index] == '/' {
			return key[:index], key[index+1:]
		}
	}
	return "", key
}

// A per-item fan-out over its budget, on a unit that read well before,
// commits this run's list with the detail it could read: the bucket set
// is the same, the run is reported as capped, the unread calls are named
// with counts, and the ledger counts only the calls the budget let
// through.
func TestCappedFanOutKeepsTheLastGoodRead(t *testing.T) {
	budgets := map[string]int{}
	ledger := newLedgerRecorder()
	h := newHarness(t, fakeaws.ScenarioEstateHubSpoke, WithCallLedger(ledger.record),
		WithOptions(func() Options { return Options{CallBudgets: budgets} }))
	bucket := model.Unit{Account: fakeaws.EstateData, Region: model.GlobalRegion, Service: "s3"}
	only := map[string]bool{bucket.Key(): true}
	tgt := target(fakeaws.EstateData, fakeaws.EstateHomeRegion)
	first, err := h.collector.sync(context.Background(), tgt, []Kind{KindInventory}, only)
	if err != nil || first.Units[0].Outcome != OutcomeCommitted {
		t.Fatalf("first read = %+v, %v", first, err)
	}
	good := resourcesByARN(t, h, fakeaws.EstateData)
	if first.Units[0].Calls <= 4 || len(good) == 0 {
		t.Fatalf("the first read made %d calls for %d resources; the budget below would not bite", first.Units[0].Calls, len(good))
	}
	beforeRows := ledger.total(t)

	budgets[SourceInventory] = 4
	sent := h.fake.TotalRequests()
	report, err := h.collector.sync(context.Background(), tgt, []Kind{KindInventory}, only)
	if err != nil {
		t.Fatal(err)
	}
	// The bucket list and one bucket's three detail calls: the budget
	// counts detail calls only, and splits evenly across the three detail
	// reads of a bucket, one bucket each.
	if got := h.fake.TotalRequests() - sent; got != 4 {
		t.Fatalf("a budget of 4 sent %d requests", got)
	}
	unit := report.Units[0]
	if unit.Outcome != OutcomeCommitted || unit.ErrorCode != CodeCapped || unit.Calls != 4 {
		t.Fatalf("capped run = %+v", unit)
	}
	// The same buckets; one whose detail the budget skipped says so, and
	// is otherwise the bucket as listed.
	kept := resourcesByARN(t, h, fakeaws.EstateData)
	if !reflect.DeepEqual(keysOfResources(kept), keysOfResources(good)) {
		t.Fatalf("the capped run changed the bucket set:\nbefore %v\nafter  %v", keysOfResources(good), keysOfResources(kept))
	}
	skipped := 0
	for arn, resource := range kept {
		if len(model.NotReadOperations(resource)) > 0 {
			skipped++
			continue
		}
		if !reflect.DeepEqual(resource, good[arn]) {
			t.Fatalf("a bucket read in full changed: %+v -> %+v", good[arn], resource)
		}
	}
	if skipped == 0 {
		t.Fatal("no bucket reads as not read")
	}
	state := syncState(t, h, fakeaws.EstateData, "s3")
	counted := false
	for _, gap := range state.Gaps {
		if gap.Reason == model.GapCapped && gap.Total > 0 && gap.Seen < gap.Total {
			counted = true
		}
	}
	if !counted {
		t.Fatalf("capped state = %+v", state)
	}
	if got := ledger.total(t) - beforeRows; got != 4 {
		t.Fatalf("the ledger counted %d calls for the capped run, want 4", got)
	}
}

func keysOfResources(resources map[string]model.Resource) []string {
	keys := make([]string, 0, len(resources))
	for key := range resources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// A run that starts a second before the hour turns counts in the hour it
// started; the next run counts in the new hour. Runs of different units in
// the same hour keep their own rows.
func TestLedgerHourIsTheHourTheRunStarted(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 59, 59, 0, time.UTC)}
	ledger := newLedgerRecorder()
	h := harnessAt(t, fakeaws.ScenarioBadDeploy, clock, WithCallLedger(ledger.record))
	tgt := target(productionAccount)
	mustSync(t, h, tgt, KindHealth)
	clock.Advance(time.Second)
	mustSync(t, h, tgt, KindHealth)
	byHour := map[time.Time]map[string]int64{}
	for _, row := range ledger.rows(t) {
		if byHour[row.Hour] == nil {
			byHour[row.Hour] = map[string]int64{}
		}
		byHour[row.Hour][row.Region+"/"+row.Source] += row.Runs
	}
	nine, ten := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	if len(byHour) != 2 || len(byHour[nine]) == 0 || !reflect.DeepEqual(byHour[nine], byHour[ten]) {
		t.Fatalf("ledger hours = %v", byHour)
	}
	for key, runs := range byHour[nine] {
		if runs != 1 {
			t.Errorf("%s ran %d times in the 09:00 hour, want 1", key, runs)
		}
	}
}

// trailRequests remembers the bodies of the DescribeTrails requests a
// collector sends, then passes every request on to the fake.
type trailRequests struct {
	next   *http.Client
	mu     sync.Mutex
	bodies []string
}

func (client *trailRequests) Do(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.Header.Get("X-Amz-Target"), ".DescribeTrails") && request.Body != nil {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		client.mu.Lock()
		client.bodies = append(client.bodies, string(body))
		client.mu.Unlock()
	}
	return client.next.Do(request)
}

// The trails read asks for shadow trails: a multi-Region trail homed in
// another Region and the organization trail only show in a Region when
// the request includes them, and each one is a copy of every call.
func TestTrailsReadAsksForShadowTrails(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioEstateHubSpoke)
	requests := &trailRequests{next: h.fake.HTTPClient()}
	h.collector.httpClient = requests
	only := map[string]bool{model.Unit{Account: fakeaws.EstateWorkloads, Region: fakeaws.EstateHomeRegion, Service: TrailsService}.Key(): true}
	if _, err := h.collector.sync(context.Background(), target(fakeaws.EstateWorkloads, fakeaws.EstateHomeRegion), []Kind{KindInventory}, only); err != nil {
		t.Fatal(err)
	}
	requests.mu.Lock()
	defer requests.mu.Unlock()
	if len(requests.bodies) != 1 {
		t.Fatalf("DescribeTrails sent %d times, want once", len(requests.bodies))
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(requests.bodies[0]), &input); err != nil {
		t.Fatalf("DescribeTrails body %q: %v", requests.bodies[0], err)
	}
	if input["includeShadowTrails"] != true {
		t.Fatalf("DescribeTrails asked %s, want includeShadowTrails true", requests.bodies[0])
	}
}

// The trail read keeps its daily cadence whatever the daily target says,
// like health, changes and identity.
func TestDailyTargetNeverStretchesTheTrailsRead(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	trails := model.Unit{Account: productionAccount, Region: testRegion, Service: TrailsService}
	for _, factor := range []int{1, 2, 4, 8} {
		if got := cadenceOf(trails, nil, Intervals{}, factor, now); got.Interval != 24*time.Hour || got.Stretched {
			t.Errorf("trails at stretch %d = %+v, want 24h", factor, got)
		}
	}
}
