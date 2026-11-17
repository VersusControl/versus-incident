package collect

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

func TestChangePollCoversWindowWithoutGaps(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	report := mustSync(t, h, target(productionAccount), KindChanges)
	if report.Failed() != 0 || report.Units[0].Resources != 4 {
		t.Fatalf("change poll = %+v", report.Units)
	}
	now := time.Now()
	settled := now.Add(-DefaultChangeOverlap - time.Second)
	result, err := h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount}, now.Add(-12*time.Hour), settled)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Gaps) != 0 {
		t.Fatalf("clean run has gaps: %+v", result.Gaps)
	}
	for _, change := range result.Changes {
		if change.EventName == "DescribeInstances" {
			t.Fatal("read-only event stored")
		}
	}
	// A second poll re-reads the overlap without duplicating changes.
	mustSync(t, h, target(productionAccount), KindChanges)
	again, _ := h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount}, now.Add(-12*time.Hour), settled)
	if len(again.Changes) != len(result.Changes) {
		t.Fatalf("changes %d -> %d after a repeat poll", len(result.Changes), len(again.Changes))
	}
	// Beyond the lookback nothing was read, so that stretch is a gap.
	older, _ := h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount}, now.Add(-48*time.Hour), settled)
	if older.Complete || len(older.Gaps) != 1 || older.Gaps[0].Reason != model.GapNotChecked {
		t.Fatalf("pre-lookback window = %+v", older.Gaps)
	}
}

func TestFailedChangePollIsAGapNotEmpty(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	h.fake.SetThrottle(1)
	report := mustSync(t, h, target(productionAccount), KindChanges)
	if report.Units[0].Outcome != OutcomeKept || report.Units[0].ErrorCode != codeThrottledWord {
		t.Fatalf("throttled poll = %+v", report.Units[0])
	}
	now := time.Now()
	result, err := h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount}, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || len(result.Changes) != 0 || len(result.Gaps) != 1 || result.Gaps[0].Reason != model.GapThrottled {
		t.Fatalf("throttled window = %+v", result)
	}
	// Once the shared backoff has passed, the next clean poll fills the
	// window.
	h.fake.SetThrottle(0)
	h.limiter.Succeeded(productionAccount, testRegion)
	mustSync(t, h, target(productionAccount), KindChanges)
	result, _ = h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount}, now.Add(-time.Hour), now.Add(-DefaultChangeOverlap-time.Second))
	if !result.Complete {
		t.Fatalf("recovered window = %+v", result)
	}
	if recent, _ := h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount}, now.Add(-time.Hour), time.Now()); len(recent.Changes) == 0 {
		t.Fatal("recovered poll stored no changes")
	}
}

// TestPartialChangeReadCoversOnlyWhatWasRead stops a window part-way and
// checks only the newest stretch that was read is claimed.
func TestPartialChangeReadCoversOnlyWhatWasRead(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	c := &call{env: env{account: productionAccount, region: testRegion, credentials: testCredentials(productionAccount), httpClient: h.fake.HTTPClient()},
		unit: model.Unit{Account: productionAccount, Region: testRegion, Service: snapshot.ChangeService}, partition: "aws", endpointRegion: testRegion}
	api, err := cloudTrailClient(c)
	if err != nil {
		t.Fatal(err)
	}
	pageSize, pages := changePageSize, maxChangePages
	t.Cleanup(func() { changePageSize, maxChangePages = pageSize, pages })
	changePageSize = 2

	now := time.Now()
	window := changes.Interval{From: now.Add(-6 * time.Hour), To: now}
	events, readFrom, err := readWindow(context.Background(), api, window)
	if err != nil || !readFrom.Equal(window.From) || len(events) != 5 {
		t.Fatalf("full read = %d events from %s, %v", len(events), readFrom, err)
	}

	// The second page is throttled: only the first page's stretch counts.
	h.fake.SetThrottle(2)
	events, readFrom, err = readWindow(context.Background(), api, window)
	if err == nil || len(events) != 2 {
		t.Fatalf("throttled read = %d events, %v", len(events), err)
	}
	oldest := events[1].Time
	if !readFrom.After(oldest) || !readFrom.Before(window.To) {
		t.Fatalf("partial coverage starts %s, oldest read event %s", readFrom, oldest)
	}

	// The page cap ends a read without an error and with the same rule.
	h.fake.SetThrottle(0)
	maxChangePages = 1
	events, readFrom, err = readWindow(context.Background(), api, window)
	if err != nil || len(events) != 2 || !readFrom.After(events[1].Time) {
		t.Fatalf("capped read = %d events from %s, %v", len(events), readFrom, err)
	}
}

// TestRecentTailStaysAGapUntilReread proves coverage stops short of now:
// with one fixed clock, the first poll claims nothing newer than
// now − overlap, the tail reads as a gap, and an event CloudTrail delivers
// late into that tail is picked up by the next poll.
func TestRecentTailStaysAGapUntilReread(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	at := time.Now().UTC().Truncate(time.Second)
	h.fake.SetClock(func() time.Time { return at })
	poll := func(now time.Time) {
		t.Helper()
		collector := New(h.store, h.limiter, WithHTTPClient(h.fake.HTTPClient()), WithSleep(noSleep), WithRetries(0),
			WithClock(func() time.Time { return now }))
		if report, err := collector.Sync(context.Background(), target(productionAccount), KindChanges); err != nil || report.Failed() != 0 {
			t.Fatalf("poll at %s = %+v, %v", now, report, err)
		}
	}
	settled := at.Add(-DefaultChangeOverlap)

	poll(at)
	checkpoint, err := h.store.Checkpoint(productionAccount, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if until := checkpoint.CoveredUntil(); until.IsZero() || until.After(settled) {
		t.Fatalf("first poll claimed coverage until %s, after %s", until, settled)
	}
	tail, _ := h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount}, at.Add(-20*time.Minute), at)
	if tail.Complete || len(tail.Gaps) != 1 || tail.Gaps[0].From.After(settled) || !tail.Gaps[0].To.Equal(at) {
		t.Fatalf("unsettled tail = %+v", tail.Gaps)
	}

	h.fake.AddLateEvent("00000000-0000-0000-0000-0000000000c1", "PutRule", "events.amazonaws.com", at.Add(-2*time.Minute))
	poll(at.Add(20 * time.Minute))
	window, _ := h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount, Service: "eventbridge"}, at.Add(-5*time.Minute), at)
	if !window.Complete || len(window.Changes) != 1 || window.Changes[0].EventName != "PutRule" {
		t.Fatalf("late event after re-read = %+v", window)
	}
}

// TestLookbackNeverPassesRetention reads with a 1h change retention: the
// poll starts at the retention edge, so the 3h-old event is never fetched
// and coverage is not claimed for history the store would prune.
func TestLookbackNeverPassesRetention(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	h.store = snapshot.New(h.provider, h.store.Scope(), snapshot.WithOptions(func() snapshot.Options { return snapshot.Options{ChangeRetention: time.Hour} }))
	h.collector.store = h.store
	report := mustSync(t, h, target(productionAccount), KindChanges)
	if report.Failed() != 0 {
		t.Fatalf("poll = %+v", report.Units)
	}
	checkpoint, err := h.store.Checkpoint(productionAccount, testRegion)
	if err != nil || len(checkpoint.Covered) != 1 {
		t.Fatalf("checkpoint = %+v, %v", checkpoint, err)
	}
	if checkpoint.Covered[0].From.Before(time.Now().Add(-time.Hour - time.Minute)) {
		t.Fatalf("coverage starts %s, before the retention edge", checkpoint.Covered[0].From)
	}
	changes, _ := h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount}, time.Now().Add(-24*time.Hour), time.Now())
	for _, change := range changes.Changes {
		if change.EventName == "RunInstances" {
			t.Fatal("an event older than retention was fetched")
		}
	}
	if h.fake.OperationCount("cloudtrail:LookupEvents") != 1 {
		t.Fatalf("lookup pages = %d, want 1", h.fake.OperationCount("cloudtrail:LookupEvents"))
	}
}
