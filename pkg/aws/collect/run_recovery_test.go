package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/grouping"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// gate refuses the requests deny matches with AccessDenied, the way AWS
// answers a missing permission, and passes the rest to the fake.
type gate struct {
	next doer
	mu   sync.Mutex
	deny func(request *http.Request) bool
}

type doer interface {
	Do(*http.Request) (*http.Response, error)
}

func (g *gate) set(deny func(*http.Request) bool) {
	g.mu.Lock()
	g.deny = deny
	g.mu.Unlock()
}

func (g *gate) Do(request *http.Request) (*http.Response, error) {
	g.mu.Lock()
	deny := g.deny
	g.mu.Unlock()
	if deny != nil && deny(request) {
		body := `{"__type":"AccessDeniedException","message":"not authorized"}`
		header := http.Header{"Content-Type": {"application/x-amz-json-1.1"}, "X-Amzn-Errortype": {"AccessDeniedException"}}
		return &http.Response{StatusCode: http.StatusForbidden, Header: header, Body: io.NopCloser(bytes.NewReader([]byte(body))),
			ContentLength: int64(len(body)), Request: request}, nil
	}
	return g.next.Do(request)
}

// signedFor reports a request signed for a service in a Region.
func signedFor(request *http.Request, service, region string) bool {
	return strings.Contains(request.Header.Get("Authorization"), "/"+region+"/"+service+"/aws4_request")
}

func lambdaList(request *http.Request) bool {
	return strings.HasPrefix(request.URL.Path, "/2015-03-31/functions")
}

// A run that fails retries on a short backoff, not a full interval later:
// a permission granted after a denied run shows at the next retry. A
// change seen while the unit fails stays marked until a run commits, and
// the failing unit is not retried on every pass meanwhile.
func TestFailedRunRetriesSoonAndKeepsItsChange(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	ledger := newLedgerRecorder()
	h := harnessAt(t, fakeaws.ScenarioBadDeploy, clock, WithCallLedger(ledger.record))
	g := &gate{next: h.fake.HTTPClient()}
	h.collector.httpClient = g
	g.set(lambdaList)
	feed := &changeFeed{}
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return []Target{target(productionAccount)} },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now), WithChangeFeed(feed.read))
	lambda := model.Unit{Account: productionAccount, Region: testRegion, Service: "lambda"}
	runs := func() int64 { return unitRuns(t, ledger)[testRegion+"/lambda"] }
	step := func(minutes int) {
		for range minutes {
			scheduler.RunDue(context.Background())
			clock.Advance(time.Minute)
		}
	}

	step(1)
	if runs() != 1 || syncState(t, h, productionAccount, "lambda").State != model.SyncFailed {
		t.Fatalf("first run = %d runs, state %+v", runs(), syncState(t, h, productionAccount, "lambda"))
	}
	// A change arrives while the unit is denied: it is kept, and the unit
	// is not hammered every pass.
	feed.add(model.Change{ID: "deploy", Time: clock.Now(), Account: productionAccount, Region: testRegion, Service: "lambda", EventName: "UpdateFunctionCode20150331v2"})
	step(13)
	if runs() != 1 {
		t.Fatalf("a failing unit ran %d times before its retry", runs())
	}
	// The retry at 15 minutes is denied again: the next waits 30.
	step(2)
	if runs() != 2 {
		t.Fatalf("no retry at 15 minutes: %d runs", runs())
	}
	g.set(nil)
	step(29)
	if runs() != 2 {
		t.Fatalf("retried before the 30-minute backoff: %d runs", runs())
	}
	step(2)
	if runs() != 3 || syncState(t, h, productionAccount, "lambda").State != model.SyncOK {
		t.Fatalf("the permission granted did not show at the retry: %d runs, %+v", runs(), syncState(t, h, productionAccount, "lambda"))
	}
	scheduler.mu.Lock()
	state := *scheduler.units[lambda.Key()]
	scheduler.mu.Unlock()
	if state.changed || state.failures != 0 {
		t.Fatalf("after a committed run the unit is %+v", state)
	}
	// Committed: back on the full read cadence.
	step(60)
	if runs() != 3 {
		t.Fatalf("a healthy unit ran %d times in the hour after it read", runs())
	}
}

func TestRetryBackoffGrowsAndIsCapped(t *testing.T) {
	intervals := Intervals{}
	health := model.Unit{Account: productionAccount, Region: testRegion, Service: "elbv2"}
	inventory := model.Unit{Account: productionAccount, Region: testRegion, Service: "lambda"}
	for failures, want := range map[int]time.Duration{1: 2 * time.Minute, 2: 4 * time.Minute, 3: 8 * time.Minute, 4: 10 * time.Minute, 9: 10 * time.Minute} {
		if got := retryBackoff(health, failures, intervals, 10*time.Minute); got != want {
			t.Errorf("health after %d failures waits %s, want %s", failures, got, want)
		}
	}
	for failures, want := range map[int]time.Duration{1: 15 * time.Minute, 2: 30 * time.Minute, 3: time.Hour, 7: time.Hour} {
		if got := retryBackoff(inventory, failures, intervals, 6*time.Hour); got != want {
			t.Errorf("inventory after %d failures waits %s, want %s", failures, got, want)
		}
	}
	if got := retryBackoff(inventory, 5, intervals, 20*time.Minute); got != 20*time.Minute {
		t.Errorf("a backoff longer than the unit's own interval = %s", got)
	}
}

// A Region whose change feed cannot drive re-reads (its CloudTrail reads
// are denied) falls back to the 15-minute full inventory read; a Region
// whose feed works keeps the 6-hour read. When the feed recovers, so does
// the cadence.
func TestBrokenChangeFeedFallsBackToFifteenMinutes(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	ledger := newLedgerRecorder()
	h := harnessAt(t, fakeaws.ScenarioBadDeploy, clock, WithCallLedger(ledger.record))
	g := &gate{next: h.fake.HTTPClient()}
	h.collector.httpClient = g
	g.set(func(request *http.Request) bool { return signedFor(request, "cloudtrail", "us-west-2") })
	tgt := target(productionAccount, testRegion, "us-west-2")
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return []Target{tgt} },
		func() Intervals { return Intervals{} }, nil, WithSchedulerClock(clock.Now))
	for range 60 {
		scheduler.RunDue(context.Background())
		clock.Advance(time.Minute)
	}
	runs := unitRuns(t, ledger)
	if runs["us-west-2/lambda"] != 4 || runs[testRegion+"/lambda"] != 1 {
		t.Fatalf("lambda runs in an hour: broken feed %d (want 4), working feed %d (want 1)", runs["us-west-2/lambda"], runs[testRegion+"/lambda"])
	}
	down := scheduler.Cadence(model.Unit{Account: productionAccount, Region: "us-west-2", Service: "lambda"})
	if !down.FeedDown || down.Interval != FeedFallbackInterval {
		t.Fatalf("cadence with the feed down = %+v", down)
	}
	g.set(nil)
	for range 6 {
		scheduler.RunDue(context.Background())
		clock.Advance(time.Minute)
	}
	if up := scheduler.Cadence(model.Unit{Account: productionAccount, Region: "us-west-2", Service: "lambda"}); up.FeedDown || up.Interval != 6*time.Hour {
		t.Fatalf("cadence once the feed works = %+v", up)
	}
}

// The stretch rises when the account is over its daily target and is
// released only below 80% of it, so the cadence does not flap.
func TestStretchHasHysteresis(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	var calls atomic.Int64
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return nil }, func() Intervals { return Intervals{} }, nil,
		WithSchedulerClock(clock.Now), WithCallTarget(func() int { return 20000 }, func(context.Context, time.Time) (map[string]int64, error) {
			return map[string]int64{productionAccount: calls.Load()}, nil
		}))
	for _, step := range []struct {
		calls int64
		want  int
	}{{19000, 1}, {25000, 2}, {19000, 2}, {16000, 2}, {15999, 1}, {19999, 1}, {20001, 2}} {
		calls.Store(step.calls)
		clock.Advance(totalsRefresh)
		if got := scheduler.stretchFactor(context.Background(), productionAccount); got != step.want {
			t.Fatalf("at %d calls the stretch is %d, want %d", step.calls, got, step.want)
		}
	}
	if got := scheduler.Stretched(); got[productionAccount] != 2 {
		t.Fatalf("stretched = %v", got)
	}
}

// With the fast cadence at or above the health interval, as when a user
// sets health and not health_hot, there is no fast cadence.
func TestNoFastCadenceWhenHealthIsAsFast(t *testing.T) {
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	state := &unitState{hotUntil: now.Add(time.Hour), hotSince: now}
	unit := model.Unit{Account: productionAccount, Region: testRegion, Service: "elbv2"}
	if got := cadenceOf(unit, state, Intervals{Health: 5 * time.Minute, HealthHot: 5 * time.Minute}, 1, now); got.Hot || got.Interval != 5*time.Minute {
		t.Fatalf("cadence = %+v, want 5m and not hot", got)
	}
}

// A unit whose fan-out is steadily over its budget still refreshes on
// every run: what it read is committed each time.
func TestSteadyOverBudgetFanOutRefreshesEachRun(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	h := harnessAt(t, fakeaws.ScenarioEstateHubSpoke, clock, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourceInventory: 4}}
	}))
	bucket := map[string]bool{model.Unit{Account: fakeaws.EstateData, Region: model.GlobalRegion, Service: "s3"}.Key(): true}
	var last time.Time
	for run := 1; run <= 3; run++ {
		report, err := h.collector.sync(context.Background(), target(fakeaws.EstateData, fakeaws.EstateHomeRegion), []Kind{KindInventory}, bucket)
		if err != nil {
			t.Fatal(err)
		}
		unit := report.Units[0]
		state := syncState(t, h, fakeaws.EstateData, "s3")
		if unit.Outcome != OutcomeCommitted || unit.ErrorCode != CodeCapped || !state.LastGoodAt.After(last) {
			t.Fatalf("run %d = %+v, last good %v (before %v)", run, unit, state.LastGoodAt, last)
		}
		last = state.LastGoodAt
		clock.Advance(time.Hour)
	}
}

// flakyLedger fails its first writes, then stores.
type flakyLedger struct {
	*ledgerRecorder
	failures atomic.Int64
}

func (ledger *flakyLedger) record(ctx context.Context, counts []storage.AWSCallCount) error {
	if ledger.failures.Add(-1) >= 0 {
		return errors.New("ledger down")
	}
	return ledger.ledgerRecorder.record(ctx, counts)
}

// Counts a failed ledger write kept are added to the next write: nothing
// is lost, rows still add by hour, and the collector says the ledger may
// be short meanwhile.
func TestFailedLedgerWritesAreKeptForTheNext(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	ledger := &flakyLedger{ledgerRecorder: newLedgerRecorder()}
	ledger.failures.Store(3)
	h := harnessAt(t, fakeaws.ScenarioBadDeploy, clock, WithCallLedger(ledger.record))
	before := h.fake.TotalRequests()
	mustSync(t, h, target(productionAccount), KindHealth)
	if !h.collector.LedgerFailedSince(clock.Now().Add(-time.Hour)) {
		t.Fatal("failed writes not reported")
	}
	mustSync(t, h, target(productionAccount), KindHealth)
	if got, sent := ledger.total(t), h.fake.TotalRequests()-before; got != sent {
		t.Fatalf("ledger holds %d calls, AWS answered %d", got, sent)
	}
	for _, row := range ledger.rows(t) {
		if row.Runs != 2 {
			t.Errorf("%s/%s: %d runs, want 2", row.Region, row.Source, row.Runs)
		}
	}
	clock.Advance(25 * time.Hour)
	if h.collector.LedgerFailedSince(clock.Now().Add(-24 * time.Hour)) {
		t.Fatal("a failure older than the window still reported")
	}
}

// Counts still unsent when the scheduler stops are written on the way
// out.
func TestSchedulerFlushesTheLedgerWhenItStops(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	ledger := &flakyLedger{ledgerRecorder: newLedgerRecorder()}
	ledger.failures.Store(1000)
	h := harnessAt(t, fakeaws.ScenarioBadDeploy, clock, WithCallLedger(ledger.record))
	report := mustSync(t, h, target(productionAccount), KindHealth)
	if ledger.total(t) != 0 {
		t.Fatal("the ledger stored counts while down")
	}
	ledger.failures.Store(0)
	scheduler := NewScheduler(h.collector, func(context.Context) []Target { return nil }, func() Intervals { return Intervals{} }, nil,
		WithSchedulerClock(clock.Now))
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Start(ctx)
	cancel()
	select {
	case <-scheduler.Stopped():
	case <-time.After(time.Minute):
		t.Fatal("scheduler did not stop")
	}
	var want int64
	for _, unit := range report.Units {
		want += int64(unit.Calls)
	}
	if got := ledger.total(t); got != want || want == 0 {
		t.Fatalf("after stop the ledger holds %d calls, want %d", got, want)
	}
}

// Trails are cost evidence, not inventory: resource lists and unit counts
// leave them out, and only a reader that asks for them sees them.
func TestTrailsAreNotInventory(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	only := map[string]bool{model.Unit{Account: productionAccount, Region: testRegion, Service: TrailsService}.Key(): true}
	report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindInventory}, only)
	if err != nil || report.Units[0].Resources != 0 {
		t.Fatalf("trails run = %+v, %v", report, err)
	}
	all, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range all.Resources {
		if resource.Type == TrailType {
			t.Fatalf("a trail is listed as a resource: %+v", resource)
		}
	}
	for _, unit := range all.Units {
		if unit.Unit.Service == TrailsService && unit.Count != 0 {
			t.Fatalf("the trails unit counts %d resources", unit.Count)
		}
	}
	asked, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount, Trails: true, Service: TrailsService})
	if err != nil || len(asked.Resources) != 1 {
		t.Fatalf("trails when asked = %+v, %v", asked.Resources, err)
	}
}

// bigECS answers the ECS calls of one account with clusters × services,
// every service in steady state, and passes everything else on.
type bigECS struct {
	next               doer
	clusters, services int
}

func (fake *bigECS) Do(request *http.Request) (*http.Response, error) {
	target := request.Header.Get("X-Amz-Target")
	if !strings.HasPrefix(target, "AmazonEC2ContainerServiceV20141113.") {
		return fake.next.Do(request)
	}
	var input map[string]any
	if request.Body != nil {
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &input)
	}
	arn := func(kind string, parts ...string) string {
		return "arn:aws:ecs:" + testRegion + ":" + productionAccount + ":" + kind + "/" + strings.Join(parts, "/")
	}
	var answer any
	switch strings.TrimPrefix(target, "AmazonEC2ContainerServiceV20141113.") {
	case "ListClusters":
		var arns []string
		for index := range fake.clusters {
			arns = append(arns, arn("cluster", fmt.Sprintf("c%02d", index)))
		}
		answer = map[string]any{"clusterArns": arns}
	case "DescribeClusters":
		var clusters []any
		for _, value := range input["clusters"].([]any) {
			name := value.(string)[strings.LastIndex(value.(string), "/")+1:]
			clusters = append(clusters, map[string]any{"clusterArn": value, "clusterName": name, "status": "ACTIVE"})
		}
		answer = map[string]any{"clusters": clusters}
	case "ListServices":
		cluster := input["cluster"].(string)
		name := cluster[strings.LastIndex(cluster, "/")+1:]
		start := 0
		if token, ok := input["nextToken"].(string); ok {
			start, _ = strconv.Atoi(token)
		}
		end := min(start+100, fake.services)
		var arns []string
		for index := start; index < end; index++ {
			arns = append(arns, arn("service", name, fmt.Sprintf("s%03d", index)))
		}
		page := map[string]any{"serviceArns": arns}
		if end < fake.services {
			page["nextToken"] = strconv.Itoa(end)
		}
		answer = page
	case "DescribeServices":
		var services []any
		for _, value := range input["services"].([]any) {
			name := value.(string)[strings.LastIndex(value.(string), "/")+1:]
			services = append(services, map[string]any{"serviceArn": value, "serviceName": name, "status": "ACTIVE",
				"desiredCount": 2, "runningCount": 2, "taskDefinition": "arn:aws:ecs:" + testRegion + ":" + productionAccount + ":task-definition/" + name + ":1",
				"deployments": []any{map[string]any{"status": "PRIMARY", "rolloutState": "COMPLETED", "desiredCount": 2, "runningCount": 2}}})
		}
		answer = map[string]any{"services": services}
	default:
		return fake.next.Do(request)
	}
	body, _ := json.Marshal(answer)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-amz-json-1.1"}},
		Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request}, nil
}

// An ECS estate past the health budget (10 clusters of 190 services: 212
// list calls against 200) is read in full and committed: a budget never
// stops a list, so the services are exact. Ten times that (10 clusters of
// 1,000 services) runs the lists past five times the budget: the unit
// stops, keeps its whole last good read and says it is capped.
func TestECSListsRunToTheEndUnderTheCeiling(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	h := harnessAt(t, fakeaws.ScenarioBadDeploy, clock, WithOptions(func() Options {
		return Options{CallBudgets: DefaultCallBudgets()}
	}))
	ecsFake := &bigECS{next: h.fake.HTTPClient(), clusters: 10, services: 190}
	h.collector.httpClient = ecsFake
	only := map[string]bool{model.Unit{Account: productionAccount, Region: testRegion, Service: "ecs"}.Key(): true}
	services := func() int {
		count := 0
		for _, resource := range resourcesByARN(t, h, productionAccount) {
			if resource.Service == "ecs" && resource.Type == "service" {
				count++
			}
		}
		return count
	}
	report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindHealth}, only)
	if err != nil {
		t.Fatal(err)
	}
	if unit := report.Units[0]; unit.Outcome != OutcomeCommitted || unit.ErrorCode != "" || unit.Calls != 212 || services() != 1900 {
		t.Fatalf("a list past its budget = %+v, %d services", unit, services())
	}
	good := syncState(t, h, productionAccount, "ecs").LastGoodAt

	clock.Advance(time.Hour)
	ecsFake.services = 1000
	report, err = h.collector.sync(context.Background(), target(productionAccount), []Kind{KindHealth}, only)
	if err != nil {
		t.Fatal(err)
	}
	unit := report.Units[0]
	if unit.Outcome != OutcomeKept || unit.ErrorCode != CodeCapped || unit.Calls != listCeiling*healthCallBudget {
		t.Fatalf("a list past the ceiling = %+v", unit)
	}
	state := syncState(t, h, productionAccount, "ecs")
	capped := false
	for _, gap := range state.Gaps {
		capped = capped || gap.Reason == model.GapCapped && gap.Seen == listCeiling*healthCallBudget
	}
	if !capped || !state.LastGoodAt.Equal(good) || services() != 1900 {
		t.Fatalf("past the ceiling the unit is %+v with %d services; want the last good read kept, capped", state, services())
	}
}

// rewrite passes requests to the fake, sending each with a scenario and
// editing an answer's body when edit is set.
type rewrite struct {
	next     doer
	mu       sync.Mutex
	scenario string
	edit     func(request *http.Request, body string) string
	// throttle, when it matches a query request's form, answers it with a
	// throttling error instead.
	throttle func(form string) bool
}

func (client *rewrite) Do(request *http.Request) (*http.Response, error) {
	client.mu.Lock()
	scenario, edit, throttle := client.scenario, client.edit, client.throttle
	client.mu.Unlock()
	if scenario != "" {
		request.Header.Set(fakeaws.ScenarioHeader, scenario)
	}
	if throttle != nil && request.Body != nil {
		form, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(form))
		if throttle(string(form)) {
			body := `<ErrorResponse><Error><Type>Sender</Type><Code>Throttling</Code><Message>Rate exceeded</Message></Error><RequestId>x</RequestId></ErrorResponse>`
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"text/xml"}},
				Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: request}, nil
		}
	}
	response, err := client.next.Do(request)
	if err != nil || edit == nil {
		return response, err
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	edited := edit(request, string(body))
	response.Body = io.NopCloser(strings.NewReader(edited))
	response.ContentLength = int64(len(edited))
	response.Header.Set("Content-Length", strconv.Itoa(len(edited)))
	return response, nil
}

// targetGroupHealth reads the stored target groups of the sizing account
// and the ones whose health was not read.
func targetGroupHealth(t *testing.T, h harness) (groups, notRead map[string]bool) {
	t.Helper()
	groups, notRead = map[string]bool{}, map[string]bool{}
	for arn, resource := range resourcesByARN(t, h, productionAccount) {
		if resource.Service == "elbv2" && strings.Contains(arn, ":targetgroup/web-") {
			groups[arn] = true
		}
	}
	health, err := h.store.Health(context.Background(), snapshot.Query{Account: productionAccount, Service: "elbv2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range health.Health {
		if item.Source == model.SourceTargetHealth && item.Status == model.HealthUnknown {
			notRead[item.Resource] = true
		}
	}
	return groups, notRead
}

// sizingELB reads the sizing account's 19 target groups in us-east-1
// under a health budget, 7 unless the test changes it: two list calls,
// which the budget never cuts, and seven target groups a run.
func sizingELB(t *testing.T) (harness, *rewrite, func() UnitReport) {
	h, client, run, _ := sizingELBBudget(t)
	return h, client, run
}

func sizingELBBudget(t *testing.T) (harness, *rewrite, func() UnitReport, *atomic.Int64) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	budget := &atomic.Int64{}
	budget.Store(7)
	h := harnessAt(t, fakeaws.ScenarioSizingLarge, clock, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourceHealth: int(budget.Load())}}
	}))
	client := &rewrite{next: h.fake.HTTPClient()}
	h.collector.httpClient = client
	only := map[string]bool{model.Unit{Account: productionAccount, Region: fakeaws.SizingPrimaryRegion, Service: "elbv2"}.Key(): true}
	run := func() UnitReport {
		t.Helper()
		report, err := h.collector.sync(context.Background(), target(productionAccount, fakeaws.SizingPrimaryRegion), []Kind{KindHealth}, only)
		if err != nil || len(report.Units) != 1 {
			t.Fatalf("elbv2 run = %+v, %v", report, err)
		}
		clock.Advance(10 * time.Minute)
		return report.Units[0]
	}
	return h, client, run, budget
}

// The detail reads rotate: a budget that reads seven of 19 target groups a
// run reads each of them within three runs, seven new ones a run while
// there are, and the ones a run skips read "not read", never healthy.
func TestDetailReadsRotateAcrossRuns(t *testing.T) {
	h, _, run := sizingELB(t)
	read := map[string]int{}
	for index := 1; index <= 3; index++ {
		unit := run()
		if unit.Outcome != OutcomeCommitted || unit.ErrorCode != CodeCapped || unit.Calls != 9 {
			t.Fatalf("run %d = %+v", index, unit)
		}
		groups, notRead := targetGroupHealth(t, h)
		if len(groups) != fakeaws.SizingTargetGroups {
			t.Fatalf("run %d stored %d target groups", index, len(groups))
		}
		fresh := 0
		for arn := range groups {
			if !notRead[arn] {
				read[arn]++
				fresh++
			}
		}
		if fresh != 7 {
			t.Fatalf("run %d read %d target groups, want 7", index, fresh)
		}
	}
	if len(read) != fakeaws.SizingTargetGroups {
		t.Fatalf("three runs read %d of %d target groups", len(read), fakeaws.SizingTargetGroups)
	}
}

// A target group deleted in AWS is gone after a run its budget limited:
// the list runs to the end, and nothing of the last read is carried over.
func TestDeletedTargetGroupStaysDeletedOnACappedRun(t *testing.T) {
	h, client, run := sizingELB(t)
	run()
	before, _ := targetGroupHealth(t, h)
	client.mu.Lock()
	client.edit = func(request *http.Request, body string) string {
		if !strings.Contains(body, "DescribeTargetGroupsResponse") {
			return body
		}
		start := strings.Index(body, "<member><TargetGroupArn>arn:aws:elasticloadbalancing:"+fakeaws.SizingPrimaryRegion+":"+productionAccount+":targetgroup/web-03/")
		if start < 0 {
			return body
		}
		closing := "</LoadBalancerArns></member>"
		end := strings.Index(body[start:], closing) + start + len(closing)
		return body[:start] + body[end:]
	}
	client.mu.Unlock()
	unit := run()
	after, notRead := targetGroupHealth(t, h)
	deleted := "arn:aws:elasticloadbalancing:" + fakeaws.SizingPrimaryRegion + ":" + productionAccount + ":targetgroup/web-03/0123456789abcdef"
	if unit.ErrorCode != CodeCapped || !before[deleted] || after[deleted] || notRead[deleted] || len(after) != len(before)-1 {
		t.Fatalf("capped run %+v: before %d groups, after %d, deleted kept %t", unit, len(before), len(after), after[deleted])
	}
}

// A unit whose own lists run past five times its budget keeps its whole
// last good read: the instances and their status signals stay as last
// read, the run is not reported as a healthy commit, and the unit says it
// is capped.
func TestInstanceStatusPastTheCeilingIsNotReadNotHealthy(t *testing.T) {
	budgets := map[string]int{}
	h := newHarness(t, fakeaws.ScenarioLoad5k, WithOptions(func() Options { return Options{CallBudgets: budgets} }))
	h.fake.SetLoadSize(10000)
	ec2 := map[string]bool{model.Unit{Account: productionAccount, Region: testRegion, Service: "ec2"}.Key(): true}
	if _, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindHealth}, ec2); err != nil {
		t.Fatal(err)
	}
	good := syncState(t, h, productionAccount, "ec2").LastGoodAt
	instances := len(resourcesByARN(t, h, productionAccount))
	budgets[SourceHealth] = 2
	report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindHealth}, ec2)
	if err != nil {
		t.Fatal(err)
	}
	unit := report.Units[0]
	if unit.Outcome == OutcomeCommitted || unit.Unhealthy || unit.ErrorCode != CodeCapped || unit.Calls != 10 {
		t.Fatalf("instance status past the ceiling = %+v", unit)
	}
	state := syncState(t, h, productionAccount, "ec2")
	capped := false
	for _, gap := range state.Gaps {
		capped = capped || gap.Reason == model.GapCapped
	}
	if !capped || !state.LastGoodAt.Equal(good) || len(resourcesByARN(t, h, productionAccount)) != instances {
		t.Fatalf("state = %+v, %d resources (had %d)", state, len(resourcesByARN(t, h, productionAccount)), instances)
	}
}

// Alarm signals come from the alarm list, which a budget never stops: an
// alarm that is OK now clears on a run whose budget is spent.
func TestAlarmThatIsOKNowClearsOnACappedBudget(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourceHealth: 1}}
	}))
	client := &rewrite{next: h.fake.HTTPClient(), scenario: fakeaws.ScenarioBadDeploy}
	h.collector.httpClient = client
	only := map[string]bool{model.Unit{Account: productionAccount, Region: testRegion, Service: "cloudwatch"}.Key(): true}
	firing := func() int {
		health, err := h.store.Health(context.Background(), snapshot.Query{Account: productionAccount, Service: "cloudwatch"})
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, item := range health.Health {
			if item.Status == model.HealthCritical || item.Status == model.HealthWarning {
				count++
			}
		}
		return count
	}
	if _, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindHealth}, only); err != nil {
		t.Fatal(err)
	}
	if firing() == 0 {
		t.Fatal("the alarm is not firing on the first read")
	}
	client.mu.Lock()
	client.scenario = fakeaws.ScenarioHealthEvent
	client.mu.Unlock()
	report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindHealth}, only)
	if err != nil || report.Units[0].Outcome != OutcomeCommitted {
		t.Fatalf("second read = %+v, %v", report, err)
	}
	if got := firing(); got != 0 {
		t.Fatalf("%d alarm signals stay after the alarm went back to OK", got)
	}
}

// A budget raised to cover every item reads them all on the next run: the
// window follows this run's budget, not the last run's reads.
func TestRaisedBudgetReadsEverythingNextRun(t *testing.T) {
	h, _, run, budget := sizingELBBudget(t)
	if unit := run(); unit.ErrorCode != CodeCapped {
		t.Fatalf("budget 7 = %+v", unit)
	}
	budget.Store(1000)
	unit := run()
	if unit.Outcome != OutcomeCommitted || unit.ErrorCode != "" || unit.Calls != 2+fakeaws.SizingTargetGroups {
		t.Fatalf("budget 1000 = %+v", unit)
	}
	if _, notRead := targetGroupHealth(t, h); len(notRead) != 0 {
		t.Fatalf("%d target groups still not read", len(notRead))
	}
}

// The budget counts detail calls only: a unit whose two list calls alone
// pass a budget of 1 still reads one target group in full.
func TestListsDoNotUseUpTheDetailBudget(t *testing.T) {
	h, _, run, budget := sizingELBBudget(t)
	budget.Store(1)
	unit := run()
	if unit.ErrorCode != CodeCapped || unit.Calls != 3 {
		t.Fatalf("budget 1 = %+v", unit)
	}
	groups, notRead := targetGroupHealth(t, h)
	if len(groups)-len(notRead) != 1 {
		t.Fatalf("read %d target groups in full, want 1", len(groups)-len(notRead))
	}
}

// A run where the budget skipped some detail and then a call throttled
// keeps the last good read: the throttle is a failure that may pass, and
// a skipped call never turns it into a commit.
func TestThrottleAfterARefusalKeepsTheLastGoodRead(t *testing.T) {
	h, client, run, budget := sizingELBBudget(t)
	budget.Store(3)
	run() // reads target groups 0 to 2; the next run starts at 3
	good := syncState(t, h, productionAccount, "elbv2").LastGoodAt
	throttled := "targetgroup/web-03/"
	client.mu.Lock()
	client.throttle = func(form string) bool {
		return strings.Contains(form, "Action=DescribeTargetHealth") && strings.Contains(form, url.QueryEscape(throttled))
	}
	client.mu.Unlock()
	unit := run()
	if unit.Outcome != OutcomeKept || unit.ErrorCode != codeThrottledWord {
		t.Fatalf("refused then throttled = %+v", unit)
	}
	if state := syncState(t, h, productionAccount, "elbv2"); !state.LastGoodAt.Equal(good) {
		t.Fatalf("the last good read moved: %v -> %v", good, state.LastGoodAt)
	}
}

// Lists that read per item but always run to the end (hosted zones and
// their record pages, transit gateway attachments, share associations)
// are never cut by the budget, even one of a single call.
func TestPerItemListsAreNeverCutByTheBudget(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioEstateHubSpoke, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourceInventory: 1}}
	}))
	lists := map[string]string{
		"route53":         "ListHostedZones,ListResourceRecordSets",
		"transit-gateway": "DescribeTransitGatewayAttachments",
		"ram":             "GetResourceShareAssociations",
	}
	for _, account := range []string{fakeaws.EstateShared, fakeaws.EstateHub} {
		report := mustSync(t, h, target(account, fakeaws.EstateHomeRegion), KindInventory)
		for _, unit := range report.Units {
			operations, ok := lists[unit.Unit.Service]
			if !ok {
				continue
			}
			for _, gap := range syncState(t, h, account, unit.Unit.Service).Gaps {
				if gap.Reason == model.GapCapped && strings.Contains(operations, gap.Operation) {
					t.Errorf("%s: the budget cut the list %s", unit.Unit.Key(), gap.Operation)
				}
			}
			for _, resource := range resourcesByARN(t, h, account) {
				if resource.Service == unit.Unit.Service && len(model.NotReadOperations(resource)) > 0 {
					t.Errorf("%s reads as not read: %s", resource.ARN, resource.Attributes[model.AttrNotRead])
				}
			}
		}
	}
}

// Skipped detail reads "not read", never clear: with a budget of three
// calls (one bucket's three detail reads), each bucket of each run is
// either read with its real flags or marked not read, the flag reader and
// coverage say so, and the rotation reads every bucket within two runs.
func TestSkippedBucketDetailIsNotReadNeverClear(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioLoad5k, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourceInventory: 3}}
	}))
	s3 := map[string]bool{model.Unit{Account: productionAccount, Region: model.GlobalRegion, Service: "s3"}.Key(): true}
	public := model.ARN("aws", "s3", "", "", fakeaws.WorldPublicBucket)
	read := map[string]bool{}
	for run := 1; run <= 4; run++ {
		if _, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindInventory}, s3); err != nil {
			t.Fatal(err)
		}
		flags, err := h.store.SecurityFlags(context.Background(), snapshot.Query{Account: productionAccount, Service: "s3"})
		if err != nil {
			t.Fatal(err)
		}
		notRead := map[string]bool{}
		for _, item := range flags.NotRead {
			notRead[item.ARN] = true
		}
		flagged := map[string]bool{}
		for _, flag := range flags.Flags {
			if flag.Kind == model.FlagPublicExposure {
				flagged[flag.Resource] = true
			}
		}
		buckets := 0
		for arn, resource := range resourcesByARN(t, h, productionAccount) {
			if resource.Service != "s3" || resource.Type != "bucket" {
				continue
			}
			buckets++
			skipped := len(model.NotReadOperations(resource)) > 0
			if skipped != notRead[arn] {
				t.Fatalf("run %d: %s marked %t, flag reader says %t", run, arn, skipped, notRead[arn])
			}
			if skipped {
				continue
			}
			read[arn] = true
			if arn == public && !flagged[arn] {
				t.Fatalf("run %d: the public bucket was read and reads clear", run)
			}
		}
		if buckets != 2 || len(notRead) != 1 {
			t.Fatalf("run %d: %d buckets, %d not read; want one of two", run, buckets, len(notRead))
		}
		coverage, err := h.store.Coverage(context.Background(), snapshot.CoverageScope{Account: productionAccount})
		if err != nil || coverage.Summary.ResourcesNotRead != 1 {
			t.Fatalf("run %d coverage = %+v, %v", run, coverage.Summary, err)
		}
	}
	if len(read) != 2 {
		t.Fatalf("four runs read %d of 2 buckets in full", len(read))
	}
}

// A bucket whose policy read the budget skipped gets a policy entry that
// says so, so its grants read "not read", never "no grant".
func TestSkippedBucketPolicyIsNotRead(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioLoad5k, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourcePolicies: 1}}
	}))
	only := map[string]bool{model.Unit{Account: productionAccount, Region: model.GlobalRegion, Service: model.PolicyEntryService}.Key(): true}
	report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindPolicy}, only)
	if err != nil || report.Units[0].ErrorCode != CodeCapped {
		t.Fatalf("policy run = %+v, %v", report, err)
	}
	entries, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount, Service: model.PolicyEntryService, PolicyEntries: true})
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, entry := range entries.Resources {
		if entry.Type == model.ResourcePolicyType && entry.Attributes[model.AttrNotRead] == "GetBucketPolicy" && entry.Attributes[model.AttrSubject] != "" {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("not-read policy entries = %d, want 1: %+v", marked, entries.Resources)
	}
}

// An application whose member read the budget skipped keeps the members it
// last read, still marked not read, so grouping places its resources where
// they were rather than moving them down the ladder.
func TestSkippedApplicationLinksKeepTheGrouping(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	function := model.Resource{ARN: model.ARN("aws", "lambda", testRegion, productionAccount, "function:checkout-api"), Service: "lambda",
		Type: "function", Account: productionAccount, Region: testRegion, Name: "checkout-api"}
	application := model.Resource{ARN: model.ARN("aws", "servicecatalog", testRegion, productionAccount, "/applications/0abc"), Service: snapshot.AppRegistryService,
		Type: "application", Account: productionAccount, Region: testRegion, Name: "shop", Attributes: map[string]string{model.AttrApplicationID: "0abc"},
		Relations: []model.Relation{{Type: model.RelationAssociated, Target: function.ARN}}}
	if _, err := h.store.Commit(model.Unit{Account: productionAccount, Region: testRegion, Service: "lambda"}, snapshot.Candidate{Resources: []model.Resource{function}}); err != nil {
		t.Fatal(err)
	}
	appUnit := model.Unit{Account: productionAccount, Region: testRegion, Service: snapshot.AppRegistryService}
	if _, err := h.store.Commit(appUnit, snapshot.Candidate{Resources: []model.Resource{application}}); err != nil {
		t.Fatal(err)
	}
	placement := func() string {
		result, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount})
		if err != nil {
			t.Fatal(err)
		}
		for _, group := range grouping.Build(result.Resources, nil, nil, grouping.Options{}).Groups {
			if slices.Contains(group.Members, function.ARN) {
				return group.ID
			}
		}
		return ""
	}
	before := placement()
	if before == "" || !strings.Contains(before, "shop") {
		t.Fatalf("the function is placed in %q before", before)
	}
	// This run listed the application, but the budget skipped its members.
	listed := application
	listed.Relations = nil
	listed.Attributes = map[string]string{model.AttrApplicationID: "0abc", model.AttrNotRead: "ListAssociatedResources"}
	out := result{resources: []model.Resource{listed}}
	h.collector.keepLinks(context.Background(), &call{unit: appUnit}, &out)
	if _, err := h.store.Commit(appUnit, snapshot.Candidate{Resources: out.resources}); err != nil {
		t.Fatal(err)
	}
	if after := placement(); after != before {
		t.Fatalf("the function moved from %q to %q because a read was skipped", before, after)
	}
	if model.NotReadOperations(out.resources[0])[0] != "ListAssociatedResources" {
		t.Fatalf("the kept links lost their not-read marker: %+v", out.resources[0])
	}
}

// A node group whose describe the budget skipped keeps the ARN it was
// last read under: the list names it without the ID its ARN ends in, and
// a skipped describe must not read as one node group removed and another
// added.
func TestSkippedNodegroupKeepsItsARN(t *testing.T) {
	budget := &atomic.Int64{}
	h := newHarness(t, fakeaws.ScenarioBadDeploy, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourceInventory: int(budget.Load())}}
	}))
	only := map[string]bool{model.Unit{Account: productionAccount, Region: testRegion, Service: "eks"}.Key(): true}
	nodegroups := func() []model.Resource {
		var out []model.Resource
		for _, resource := range resourcesByARN(t, h, productionAccount) {
			if resource.Service == "eks" && resource.Type == "node-group" {
				out = append(out, resource)
			}
		}
		return out
	}
	if _, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindInventory}, only); err != nil {
		t.Fatal(err)
	}
	before := nodegroups()
	budget.Store(1)
	report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindInventory}, only)
	if err != nil || report.Units[0].ErrorCode != CodeCapped {
		t.Fatalf("budget 1 = %+v, %v", report, err)
	}
	after := nodegroups()
	if len(before) != 1 || len(after) != 1 || after[0].ARN != before[0].ARN || len(model.NotReadOperations(after[0])) == 0 {
		t.Fatalf("node group before %+v, after %+v", before, after)
	}
}

// An AWS Health event whose affected resources the budget did not read
// says so: its signal reads "affected resources not read", and the event
// carries the not-read marker the read side shows, so no resource it may
// affect reads clear.
func TestSkippedAffectedEntitiesReadAsNotRead(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioHealthEvent, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourceHealth: 1}}
	}))
	unit := model.Unit{Account: productionAccount, Region: model.GlobalRegion, Service: "aws-health"}
	// The last run read the other event: this run's window starts past
	// the first one, so its affected-entities read is skipped.
	h.collector.keepRotations(unit, map[string]rotation{"DescribeAffectedEntities": {start: 1, items: 2}})
	report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindHealth}, map[string]bool{unit.Key(): true})
	if err != nil || report.Units[0].Outcome != OutcomeCommitted || report.Units[0].ErrorCode != CodeCapped {
		t.Fatalf("aws-health run = %+v, %v", report, err)
	}
	health, err := h.store.Health(context.Background(), snapshot.Query{Account: productionAccount, Service: "aws-health"})
	if err != nil {
		t.Fatal(err)
	}
	if len(health.Health) != 1 || health.Health[0].Resource != "" || !strings.Contains(health.Health[0].Summary, "affected resources not read") ||
		health.Health[0].Ref != fakeaws.HealthEventARN {
		t.Fatalf("event signal = %+v", health.Health)
	}
	flags, err := h.store.SecurityFlags(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	marked := false
	for _, item := range flags.NotRead {
		marked = marked || item.ARN == fakeaws.HealthEventARN && slices.Contains(item.Operations, "DescribeAffectedEntities")
	}
	coverage, err := h.store.Coverage(context.Background(), snapshot.CoverageScope{Account: productionAccount})
	if err != nil || !marked || coverage.Summary.ResourcesNotRead != 1 {
		t.Fatalf("not-read marker: flags %+v, coverage %+v, %v", flags.NotRead, coverage.Summary, err)
	}
	listed, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range listed.Resources {
		if resource.Type == model.HealthEventType {
			t.Fatalf("the event record is listed as inventory: %+v", resource)
		}
	}
}
