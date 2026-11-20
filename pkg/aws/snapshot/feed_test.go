package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type feedFixture struct {
	t        *testing.T
	store    *Store
	provider inventoryProvider
	clock    *clock
	feed     *ChangeFeed
	scope    []ScopeAccount
}

func newFeedFixture(t *testing.T, scope []ScopeAccount, options ...FeedOption) *feedFixture {
	t.Helper()
	if openBackend == nil {
		openBackend = func(*testing.T) inventoryProvider { return storage.NewMemory().(inventoryProvider) }
		t.Cleanup(func() { openBackend = nil })
	}
	store, provider, c := newStore(t)
	fixture := &feedFixture{t: t, store: store, provider: provider.inventoryProvider, clock: c, scope: scope}
	base := []FeedOption{WithFeedClock(c.Now), WithScope(func(context.Context) ([]ScopeAccount, bool, error) { return fixture.scope, true, nil })}
	fixture.feed = NewChangeFeed(store, append(base, options...)...)
	return fixture
}

func scopeOf(regions ...string) []ScopeAccount {
	return []ScopeAccount{{Account: account, Name: "production", Regions: regions}}
}

func (fixture *feedFixture) poll(accountID, regionName string, poll ChangePoll) {
	fixture.t.Helper()
	if poll.HistoryFrom.IsZero() {
		poll.HistoryFrom = fixture.clock.Now().Add(-48 * time.Hour)
	}
	if err := fixture.store.RecordChangePoll(accountID, regionName, poll); err != nil {
		fixture.t.Fatalf("poll %s/%s: %v", accountID, regionName, err)
	}
}

// pollSettled records a clean poll that read everything up to the
// unsettled tail.
func (fixture *feedFixture) pollSettled(accountID, regionName string, found ...model.Change) {
	fixture.t.Helper()
	now := fixture.clock.Now()
	fixture.poll(accountID, regionName, ChangePoll{Found: found, Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: now.Add(-15 * time.Minute)}},
		Read: []changes.Interval{{From: now.Add(-26 * time.Hour), To: now}}})
}

func (fixture *feedFixture) answer(request FeedRequest) ChangesAnswer {
	fixture.t.Helper()
	answer, err := fixture.feed.Answer(context.Background(), request)
	if err != nil {
		fixture.t.Fatalf("answer: %v", err)
	}
	return answer
}

func feedChange(id string, at time.Time, class model.ActorClass, principal string) model.Change {
	return model.Change{ID: id, Time: at, Account: account, Region: region, Service: "ecs", EventSource: "ecs.amazonaws.com", EventName: "UpdateService",
		Template: "ecs.update_service", Summary: "Service " + id + " updated", Actor: model.Actor{Class: class, PrincipalARN: principal}}
}

func failedChange(id string, at time.Time, principal, code string) model.Change {
	change := feedChange(id, at, model.ActorPipeline, principal)
	change.EventSource, change.EventName, change.Service, change.ErrorCode = "iam.amazonaws.com", "PutRolePolicy", "iam", code
	change.Summary = "Failed attempt: role policy changed"
	return change
}

func gapsOf(answer ChangesAnswer, regionName string) []FeedGap {
	var out []FeedGap
	for _, gap := range answer.Gaps {
		if gap.Region == regionName {
			out = append(out, gap)
		}
	}
	return out
}

// A 24 h window whose every stretch was read up to the unsettled tail is
// complete: the tail is reported as arriving, not as a gap, so the page can
// say "no changes (last 15 min still arriving)".
func TestLiveWindowCompleteUpToSettledUntil(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		fixture.pollSettled(account, region)
		fixture.pollSettled(account, model.GlobalRegion)
		now := fixture.clock.Now()
		answer := fixture.answer(FeedRequest{Window: 24 * time.Hour})
		if !answer.Complete || answer.Summary.Total != 0 || answer.Summary.TotalIsLowerBound {
			t.Fatalf("read window = complete %v, summary %+v, gaps %+v", answer.Complete, answer.Summary, answer.Gaps)
		}
		if !answer.Window.SettledUntil.Equal(now.Add(-15 * time.Minute)) {
			t.Fatalf("settled_until = %s", answer.Window.SettledUntil)
		}
		for _, gap := range answer.Gaps {
			if gap.Reason != string(GapArriving) || gap.From.Before(answer.Window.SettledUntil) {
				t.Fatalf("real gap in a read window: %+v", gap)
			}
		}
		if last := answer.Buckets[len(answer.Buckets)-1]; last.State != BucketArriving {
			t.Fatalf("newest bucket = %+v", last)
		}
		if answer.Suspect.Status != SuspectNoAlarm {
			t.Fatalf("suspect = %+v", answer.Suspect)
		}
	})
}

// A throttle now must not relabel yesterday's denied stretch: each gap
// keeps the reason it was stored with.
func TestGapKeepsItsOwnReason(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	fixture.poll(account, region, ChangePoll{ErrorCode: "denied",
		Gaps: []ChangeGapLabel{{From: now.Add(-24 * time.Hour), To: now.Add(-22 * time.Hour), Reason: GapCloudTrailDenied, Detail: "cloudtrail:LookupEvents"}}})
	fixture.poll(account, region, ChangePoll{Covered: []changes.Interval{{From: now.Add(-22 * time.Hour), To: now.Add(-time.Hour)}}})
	fixture.poll(account, region, ChangePoll{ErrorCode: "throttled", Gaps: []ChangeGapLabel{{From: now.Add(-time.Hour), To: now, Reason: GapChangesThrottled}}})
	fixture.pollSettled(account, model.GlobalRegion)

	gaps := gapsOf(fixture.answer(FeedRequest{Window: 24 * time.Hour}), region)
	if len(gaps) != 2 || gaps[0].Reason != string(GapCloudTrailDenied) || gaps[1].Reason != string(GapChangesThrottled) {
		t.Fatalf("gaps = %+v", gaps)
	}
	if gaps[0].MissingPermission == nil || *gaps[0].MissingPermission != "cloudtrail:LookupEvents" {
		t.Fatalf("denied gap names no permission: %+v", gaps[0])
	}
}

// The expected scope comes from settings: a region added a moment ago is a
// not_checked gap until its first poll, never quiet.
func TestNewRegionBeforeFirstPollIsNotComplete(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	fixture.pollSettled(account, region)
	fixture.pollSettled(account, model.GlobalRegion)
	if !fixture.answer(FeedRequest{}).Complete {
		t.Fatal("read scope is not complete")
	}
	fixture.scope = scopeOf(region, "eu-west-1")
	answer := fixture.answer(FeedRequest{})
	gaps := gapsOf(answer, "eu-west-1")
	if answer.Complete || len(gaps) != 1 || gaps[0].Reason != string(GapChangesNotChecked) || !answer.Sync.FirstReadPending {
		t.Fatalf("new region = complete %v, gaps %+v, sync %+v", answer.Complete, gaps, answer.Sync)
	}
}

// Global-service events live in the global stream. With only eu-west-1
// selected, an unread global stream keeps the answer incomplete, a denied
// one is named global_unknown, and a read one shows IAM changes.
func TestGlobalStreamGapNamedWhenUnread(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf("eu-west-1"))
	now := fixture.clock.Now()
	fixture.poll(account, "eu-west-1", ChangePoll{Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: now.Add(-15 * time.Minute)}}})
	answer := fixture.answer(FeedRequest{})
	if answer.Complete || len(gapsOf(answer, model.GlobalRegion)) == 0 {
		t.Fatalf("unread global stream reads complete: %+v", answer.Gaps)
	}
	fixture.poll(account, model.GlobalRegion, ChangePoll{ErrorCode: "denied",
		Gaps: []ChangeGapLabel{{From: now.Add(-26 * time.Hour), To: now, Reason: GapCloudTrailDenied, Detail: "cloudtrail:LookupEvents"}}})
	if gaps := gapsOf(fixture.answer(FeedRequest{}), model.GlobalRegion); len(gaps) != 1 || gaps[0].Reason != string(GapGlobalUnknown) {
		t.Fatalf("denied global stream = %+v", gaps)
	}
	iam := feedChange("iam-1", now.Add(-time.Hour), model.ActorPerson, "arn:aws:iam::111122223333:user/dave")
	iam.Region, iam.Service, iam.EventSource, iam.EventName = model.GlobalRegion, "iam", "iam.amazonaws.com", "PutRolePolicy"
	fixture.pollSettled(account, model.GlobalRegion, iam)
	answer = fixture.answer(FeedRequest{})
	if !answer.Complete || len(answer.Items) != 1 || answer.Items[0].Region != model.GlobalRegion || answer.Items[0].EventName != "iam:PutRolePolicy" {
		t.Fatalf("global change = %+v / %+v", answer.Items, answer.Gaps)
	}
	var global bool
	for _, entry := range answer.Regions {
		global = global || entry.Region == model.GlobalRegion && entry.State == RegionGlobal
	}
	if !global {
		t.Fatalf("regions = %+v", answer.Regions)
	}
}

// Keyset paging over a fixed set of rows: an event stored while the pages
// are read neither shifts nor repeats an item.
func TestPagingStableUnderLateInsert(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		now := fixture.clock.Now()
		var found []model.Change
		for index := 0; index < 30; index++ {
			found = append(found, feedChange(fmt.Sprintf("e%02d", index), now.Add(-time.Duration(index+1)*10*time.Minute), model.ActorPipeline, "arn:aws:iam::111122223333:role/deploy"))
		}
		fixture.pollSettled(account, region, found...)
		first := fixture.answer(FeedRequest{Limit: 10})
		fixture.clock.Advance(time.Second)
		fixture.pollSettled(account, region, feedChange("late", now.Add(-55*time.Minute), model.ActorPipeline, "arn:aws:iam::111122223333:role/deploy"))
		seen := map[string]bool{}
		items := first.Items
		cursor := first.NextCursor
		for cursor != nil {
			next := fixture.answer(FeedRequest{Limit: 10, Cursor: *cursor})
			items = append(items, next.Items...)
			cursor = next.NextCursor
		}
		for _, item := range items {
			if seen[item.ID] {
				t.Fatalf("item %s repeated", item.ID)
			}
			seen[item.ID] = true
		}
		if len(items) != 30 || seen["late"] {
			t.Fatalf("paged %d items (late seen %v)", len(items), seen["late"])
		}
		if fresh := fixture.answer(FeedRequest{Limit: 50}); len(fresh.Items) != 31 {
			t.Fatalf("a new listing sees %d items", len(fresh.Items))
		}
	})
}

// A denial that persists is one stable gap: the same reason every cycle,
// and other accounts stay complete.
func TestSteadyDenialDoesNotFlap(t *testing.T) {
	const other = "444455556666"
	fixture := newFeedFixture(t, []ScopeAccount{{Account: account, Regions: []string{region}}, {Account: other, Regions: []string{region}}})
	for _, accountID := range []string{account, other} {
		fixture.pollSettled(accountID, region)
		fixture.pollSettled(accountID, model.GlobalRegion)
	}
	for cycle := 0; cycle < 100; cycle++ {
		fixture.clock.Advance(5 * time.Minute)
		now := fixture.clock.Now()
		fixture.poll(account, region, ChangePoll{ErrorCode: "denied",
			Gaps: []ChangeGapLabel{{From: now.Add(-30 * time.Minute), To: now, Reason: GapCloudTrailDenied, Detail: "cloudtrail:LookupEvents"}}})
		fixture.pollSettled(account, model.GlobalRegion)
		fixture.pollSettled(other, region)
		fixture.pollSettled(other, model.GlobalRegion)
		answer := fixture.answer(FeedRequest{Window: time.Hour})
		for _, entry := range answer.Accounts {
			switch entry.Account {
			case account:
				if entry.Complete || entry.Reason == nil || *entry.Reason != string(GapCloudTrailDenied) {
					t.Fatalf("cycle %d denied account = %+v", cycle, entry)
				}
			case other:
				if !entry.Complete {
					t.Fatalf("cycle %d other account flapped: %+v", cycle, answer.Gaps)
				}
			}
		}
		if gaps := gapsOf(answer, region); len(gaps) == 0 {
			t.Fatalf("cycle %d lost the denied gap", cycle)
		}
	}
}

// Buckets are UTC steps from the window start, so a daylight-saving change
// in any zone never adds or drops an hour.
func TestBucketsAcrossDSTAreUTCSteps(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	fixture.clock.now = time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)
	answer := fixture.answer(FeedRequest{Window: 24 * time.Hour})
	if len(answer.Buckets) != 24 {
		t.Fatalf("buckets = %d", len(answer.Buckets))
	}
	for index := 1; index < len(answer.Buckets); index++ {
		if step := answer.Buckets[index].Start.Sub(answer.Buckets[index-1].Start); step != time.Hour || answer.Buckets[index].Start.Location() != time.UTC {
			t.Fatalf("bucket %d step %s", index, step)
		}
	}
}

// A 7-day window has 28 six-hour buckets in UTC steps from since, says so
// in bucket_seconds, and still counts every stored change exactly.
func TestWeekWindowBucketsAreSixHours(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		week := 7 * 24 * time.Hour
		fixture := newFeedFixture(t, scopeOf(region), WithChangesLimits(func() ChangesLimits { return ChangesLimits{MaxWindow: week, MaxCompareAge: week} }))
		now := fixture.clock.Now()
		var found []model.Change
		for index := 0; index < 40; index++ {
			found = append(found, feedChange(fmt.Sprintf("w%03d", index), now.Add(-time.Duration(index*37+20)*time.Minute), model.ActorPipeline, ""))
		}
		fixture.pollSettled(account, region, found...)
		fixture.pollSettled(account, model.GlobalRegion)
		answer := fixture.answer(FeedRequest{Window: week})
		if len(answer.Buckets) != 28 || answer.BucketSeconds != 21600 {
			t.Fatalf("7d = %d buckets of %d s, want 28 of 21600 s", len(answer.Buckets), answer.BucketSeconds)
		}
		var counted int64
		for index, bucket := range answer.Buckets {
			if want := answer.Window.Since.Add(time.Duration(index) * 6 * time.Hour); !bucket.Start.Equal(want) || bucket.Start.Location() != time.UTC {
				t.Fatalf("bucket %d starts %s, want %s", index, bucket.Start, want)
			}
			counted += bucket.Pipeline
		}
		if counted != 40 || answer.Summary.Total != 40 {
			t.Fatalf("7d bucket sum %d, summary %+v, want 40", counted, answer.Summary)
		}
	})
}

// The 1 h and 24 h windows keep five-minute and hourly buckets.
func TestShortWindowBucketsUnchanged(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	for _, want := range []struct {
		window  time.Duration
		buckets int
		seconds int64
	}{{time.Hour, 12, 300}, {24 * time.Hour, 24, 3600}} {
		answer := fixture.answer(FeedRequest{Window: want.window})
		if len(answer.Buckets) != want.buckets || answer.BucketSeconds != want.seconds {
			t.Fatalf("%s = %d buckets of %d s, want %d of %d s", want.window, len(answer.Buckets), answer.BucketSeconds, want.buckets, want.seconds)
		}
		for index := 1; index < len(answer.Buckets); index++ {
			if step := answer.Buckets[index].Start.Sub(answer.Buckets[index-1].Start); step != time.Duration(want.seconds)*time.Second {
				t.Fatalf("%s bucket %d step %s", want.window, index, step)
			}
		}
	}
}

// Each six-hour bucket holds exactly the sum of its six hourly buckets,
// changes by class and failures alike.
func TestSixHourBucketCountsEqualHourlySum(t *testing.T) {
	since := time.Date(2026, 11, 13, 14, 27, 0, 0, time.UTC)
	until := since.Add(7 * 24 * time.Hour)
	classes := []model.ActorClass{model.ActorPipeline, model.ActorPerson, model.ActorAWSService, model.ActorUnknown}
	var stored storage.AWSChangeFeed
	for index := 0; index < 500; index++ {
		at := since.Add(time.Duration(index*23) * time.Minute).Truncate(time.Minute)
		stored.Minutes = append(stored.Minutes, storage.AWSChangeMinute{Minute: at, ActorClass: string(classes[index%len(classes)]), Events: int64(index%7 + 1)})
		if index%5 == 0 {
			stored.Counts = append(stored.Counts, storage.AWSChangeCount{Kind: storage.AWSChangeCountFailure, Minute: at, Events: int64(index%3 + 1)})
		}
	}
	view := coverageView{since: since, until: until, settled: until, pairs: 1}
	hourly := buckets(since, until, time.Hour, stored, nil, view)
	sixHour := buckets(since, until, bucketWidth(7*24*time.Hour), stored, nil, view)
	if len(hourly) != 168 || len(sixHour) != 28 {
		t.Fatalf("buckets = %d hourly, %d six-hour", len(hourly), len(sixHour))
	}
	for index, bucket := range sixHour {
		var sum FeedBucket
		for _, hour := range hourly[index*6 : index*6+6] {
			sum.Pipeline += hour.Pipeline
			sum.Person += hour.Person
			sum.AWSService += hour.AWSService
			sum.Unknown += hour.Unknown
			sum.Errors += hour.Errors
		}
		if bucket.Pipeline != sum.Pipeline || bucket.Person != sum.Person || bucket.AWSService != sum.AWSService || bucket.Unknown != sum.Unknown || bucket.Errors != sum.Errors {
			t.Fatalf("six-hour bucket %d = %+v, hourly sum %+v", index, bucket, sum)
		}
	}
}

// A six-hour bucket takes its worst part: one unread hour makes it
// partial, and it is gap only when none of it was read.
func TestSixHourBucketStateIsWorstPart(t *testing.T) {
	since := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	until := since.Add(7 * 24 * time.Hour)
	width := bucketWidth(7 * 24 * time.Hour)
	gap := func(from, to time.Time) FeedGap {
		return FeedGap{Account: account, Region: region, From: from, To: to, Reason: string(model.GapDenied)}
	}
	view := coverageView{since: since, until: until, settled: until, pairs: 1, gaps: []FeedGap{
		// One hour unread inside the second bucket.
		gap(since.Add(8*time.Hour), since.Add(9*time.Hour)),
		// The whole fourth bucket unread.
		gap(since.Add(18*time.Hour), since.Add(24*time.Hour)),
	}}
	out := buckets(since, until, width, storage.AWSChangeFeed{}, nil, view)
	if len(out) != 28 {
		t.Fatalf("buckets = %d", len(out))
	}
	if out[0].State != BucketComplete || out[2].State != BucketComplete {
		t.Fatalf("read buckets = %s, %s", out[0].State, out[2].State)
	}
	if out[1].State != BucketPartial || len(out[1].GapAccounts) != 1 || out[1].GapAccounts[0] != account {
		t.Fatalf("bucket with one unread hour = %+v, want partial", out[1])
	}
	if out[3].State != BucketGap {
		t.Fatalf("fully unread bucket = %+v, want gap", out[3])
	}
}

// Only canonical ARNs match: a Lambda function and an ECS service both
// named "api" in two accounts never match each other.
func TestShortNameNeverMatchesAcrossResources(t *testing.T) {
	const other = "444455556666"
	fixture := newFeedFixture(t, []ScopeAccount{{Account: account, Regions: []string{region}}, {Account: other, Regions: []string{region}}})
	now := fixture.clock.Now()
	lambdaARN := "arn:aws:lambda:us-east-1:111122223333:function:api"
	ecsARN := "arn:aws:ecs:us-east-1:444455556666:service/main/api"
	function := feedChange("lambda", now.Add(-time.Hour), model.ActorPipeline, "")
	function.Resources = []string{lambdaARN}
	service := feedChange("ecs", now.Add(-time.Hour), model.ActorPipeline, "")
	service.Account, service.Resources = other, []string{ecsARN}
	fixture.pollSettled(account, region, function)
	fixture.pollSettled(other, region, service)

	result, err := fixture.store.Changes(context.Background(), Query{ARN: lambdaARN}, now.Add(-2*time.Hour), now)
	if err != nil || len(result.Changes) != 1 || result.Changes[0].ID != "lambda" {
		t.Fatalf("by ARN = %+v, %v", result.Changes, err)
	}
	if short, _ := fixture.store.Changes(context.Background(), Query{ARN: "api"}, now.Add(-2*time.Hour), now); len(short.Changes) != 0 {
		t.Fatalf("a short name matched %d changes", len(short.Changes))
	}
	answer := fixture.answer(FeedRequest{Resource: ecsARN})
	if len(answer.Items) != 1 || answer.Items[0].ID != "ecs" {
		t.Fatalf("resource filter = %+v", answer.Items)
	}
}

// A sweep stopped by its deadline after its first batch has already trimmed
// coverage, so the expired stretch reads beyond_retention, never complete
// with rows missing.
func TestSweepMidwayReadsBeyondRetention(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, provider, c := newStore(t)
		sweeper, ok := provider.inventoryProvider.(storage.AWSRetentionSweeper)
		if !ok {
			t.Skip("backend prunes inline")
		}
		now := c.Now()
		var found []model.Change
		for index := 0; index < 5; index++ {
			found = append(found, feedChange(fmt.Sprintf("old%d", index), now.Add(-3*time.Hour+time.Duration(index)*time.Minute), model.ActorPipeline, ""))
		}
		if err := store.RecordChangePoll(account, region, ChangePoll{Found: found, Covered: []changes.Interval{{From: now.Add(-4 * time.Hour), To: now}}}); err != nil {
			t.Fatal(err)
		}
		cutoff := now.Add(-2 * time.Hour)
		result, err := sweeper.SweepAWSRetention(context.Background(), storage.AWSRetentionSweep{Scope: tenancy.DefaultOrgScope(), ChangeBefore: cutoff, Batch: 1, KeepGenerations: DefaultMaxGenerations,
			Deadline: time.Now().Add(-time.Second), Cursor: storage.AWSRetentionCursor{Stage: "changes"}})
		if err != nil || !result.More {
			t.Fatalf("sweep = %+v, %v", result, err)
		}
		window, err := store.Changes(context.Background(), Query{Account: account}, now.Add(-4*time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		if window.Complete || len(window.Changes) == 0 {
			t.Fatalf("mid-sweep window = complete %v with %d of 5 rows", window.Complete, len(window.Changes))
		}
		if len(window.Gaps) == 0 || window.Gaps[0].To.After(cutoff) {
			t.Fatalf("mid-sweep gaps = %+v", window.Gaps)
		}
	})
}

// An as-of time before the window starts is unavailable, never an empty
// complete answer.
func TestAsOfBeforeWindowIsUnavailable(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	fixture.pollSettled(account, region)
	result, err := fixture.store.Changes(context.Background(), Query{Account: account, AsOf: now.Add(-3 * time.Hour)}, now.Add(-time.Hour), now)
	if err != nil || result.Complete || result.Unavailable != UnavailableBeforeWindow || !result.Until.Equal(now.Add(-3*time.Hour)) {
		t.Fatalf("as of before window = %+v, %v", result, err)
	}
}

// An account-level outage takes its reason from the account's health now:
// a revoked key reads "credentials not valid" on every pair after it began.
func TestAccountOutageNamesCredentialsReason(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	fixture.poll(account, region, ChangePoll{Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: now.Add(-2 * time.Hour)}}})
	fixture.poll(account, model.GlobalRegion, ChangePoll{Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: now.Add(-2 * time.Hour)}}})
	fixture.scope[0].Outage, fixture.scope[0].OutageSince = GapCredentialsInvalid, now.Add(-2*time.Hour)
	answer := fixture.answer(FeedRequest{})
	for _, gap := range answer.Gaps {
		if gap.Reason != string(GapCredentialsInvalid) {
			t.Fatalf("outage gap = %+v", gap)
		}
	}
	if len(answer.Accounts) != 1 || answer.Accounts[0].Reason == nil || *answer.Accounts[0].Reason != string(GapCredentialsInvalid) || answer.Complete {
		t.Fatalf("accounts = %+v", answer.Accounts)
	}
}

// Email-shaped IAM user names never leave the API, even from rows stored
// before ingest masked them.
func TestEmailNeverInAnswer(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	person := feedChange("p1", now.Add(-time.Hour), model.ActorPerson, "arn:aws:iam::111122223333:user/jane@corp.com")
	fixture.pollSettled(account, region, person)
	data, err := json.Marshal(fixture.answer(FeedRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "jane@corp.com") {
		t.Fatalf("email in answer: %s", data)
	}
}

// Routine agent events are counted per minute: the stored count rows do
// not grow with the number of instances, and the change list stays empty.
func TestRoutineEventsCountedNotStored(t *testing.T) {
	rowsFor := func(instances int) (int, int64) {
		fixture := newFeedFixture(t, scopeOf(region))
		start := fixture.clock.Now().Add(-6 * time.Hour)
		var total int64
		for poll := 0; poll < 72; poll++ {
			pollStart := start.Add(time.Duration(poll) * 5 * time.Minute)
			var counts []storage.AWSChangeCount
			for minute := 0; minute < 5; minute++ {
				// Every instance reports once per five minutes.
				events := int64(instances / 5)
				total += events
				counts = append(counts, storage.AWSChangeCount{Kind: storage.AWSChangeCountRoutine, EventName: "UpdateInstanceInformation", EventSource: "ssm.amazonaws.com",
					Service: "ssm", ActorClass: string(model.ActorAWSService), Minute: pollStart.Add(time.Duration(minute) * time.Minute), Events: events})
			}
			fixture.poll(account, region, ChangePoll{Counts: counts, Covered: []changes.Interval{{From: pollStart, To: pollStart.Add(5 * time.Minute)}}})
		}
		stored, err := fixture.provider.(storage.AWSChangeFeedStore).AWSChangeFeed(context.Background(), tenancy.DefaultOrgScope(), storage.AWSChangeFeedQuery{Since: start, Until: fixture.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		answer := fixture.answer(FeedRequest{})
		if answer.Summary.Routine != total || len(answer.Items) != 0 || answer.Summary.Total != 0 {
			t.Fatalf("%d instances: routine %d of %d, items %d", instances, answer.Summary.Routine, total, len(answer.Items))
		}
		return len(stored.Counts) + len(stored.Rows), total
	}
	small, _ := rowsFor(10)
	large, _ := rowsFor(1000)
	if small != large {
		t.Fatalf("stored rows grew with fleet size: %d -> %d", small, large)
	}
}

// A job failing every second for a day keeps a bounded number of rows; the
// burst still shows the full count.
func TestFailureStormRowsBoundedBurstFullCount(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		end := fixture.clock.Now().Add(-15 * time.Minute)
		start := end.Add(-23 * time.Hour)
		principal := "arn:aws:iam::111122223333:role/nightly-job"
		var total int64
		for pollStart := start; pollStart.Before(end); pollStart = pollStart.Add(5 * time.Minute) {
			var found []model.Change
			counts := map[time.Time]int64{}
			for second := 0; second < 300; second += 10 {
				at := pollStart.Add(time.Duration(second) * time.Second)
				found = append(found, failedChange(fmt.Sprintf("f-%d", at.Unix()), at, principal, "AccessDenied"))
				counts[at.Truncate(time.Minute)]++
				total++
			}
			var list []storage.AWSChangeCount
			for minute, events := range counts {
				list = append(list, storage.AWSChangeCount{Kind: storage.AWSChangeCountFailure, EventName: "PutRolePolicy", EventSource: "iam.amazonaws.com", Service: "iam",
					Actor: principal, ErrorCode: "AccessDenied", ActorClass: string(model.ActorPipeline), Minute: minute, Events: events})
			}
			fixture.poll(account, region, ChangePoll{Found: found, Counts: list, Covered: []changes.Interval{{From: pollStart, To: pollStart.Add(5 * time.Minute)}}})
		}
		stored, err := fixture.provider.(storage.AWSChangeFeedStore).AWSChangeFeed(context.Background(), tenancy.DefaultOrgScope(), storage.AWSChangeFeedQuery{Since: start, Until: end})
		if err != nil {
			t.Fatal(err)
		}
		if limit := 24 * storage.DefaultAWSChangeFailureKeep; len(stored.Rows) > limit {
			t.Fatalf("rows = %d, want at most %d", len(stored.Rows), limit)
		}
		answer := fixture.answer(FeedRequest{})
		if answer.Summary.Errors != total {
			t.Fatalf("errors = %d, want %d", answer.Summary.Errors, total)
		}
		var bursts []FeedItem
		for _, item := range answer.Items {
			if item.Kind != ItemBurst {
				t.Fatalf("a failed attempt escaped the burst: %+v", item)
			}
			bursts = append(bursts, item)
		}
		if len(bursts) != 1 || *bursts[0].Count != total || bursts[0].Actor != "nightly-job" || bursts[0].EventName != "iam:PutRolePolicy" {
			t.Fatalf("bursts = %+v", bursts)
		}
		group, err := fixture.feed.Group(context.Background(), bursts[0].ID, "", 200)
		if err != nil || len(group.Items) == 0 || group.Items[0].ErrorCode == nil {
			t.Fatalf("expand = %+v, %v", group, err)
		}
	})
}

// A read that hits the row cap is truncated and incomplete, while the
// buckets, counted in the database, stay exact.
func TestReadCapTruncatesButBucketsStayExact(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region), WithFeedOptions(func() FeedOptions { return FeedOptions{ReadCap: 50} }))
		now := fixture.clock.Now()
		var found []model.Change
		for index := 0; index < 120; index++ {
			found = append(found, feedChange(fmt.Sprintf("c%03d", index), now.Add(-time.Duration(index+20)*time.Minute), model.ActorPipeline, ""))
		}
		fixture.pollSettled(account, region, found...)
		fixture.pollSettled(account, model.GlobalRegion)
		answer := fixture.answer(FeedRequest{})
		var counted int64
		for _, bucket := range answer.Buckets {
			counted += bucket.Pipeline
		}
		if !answer.Summary.Truncated || answer.Complete || !answer.Summary.TotalIsLowerBound || counted != 120 || answer.Summary.Total != 120 {
			t.Fatalf("capped answer = complete %v, summary %+v, bucket sum %d", answer.Complete, answer.Summary, counted)
		}
	})
}

// One malformed event never stops a poll: the rest is stored, coverage
// advances and the drop is counted.
func TestMalformedRowDroppedAndCounted(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		now := fixture.clock.Now()
		bad := feedChange("bad", now.Add(-time.Hour), model.ActorPipeline, "")
		bad.Summary = "bad\x00summary"
		fixture.pollSettled(account, region, feedChange("good", now.Add(-time.Hour), model.ActorPipeline, ""), bad)
		fixture.pollSettled(account, model.GlobalRegion)
		answer := fixture.answer(FeedRequest{})
		if !answer.Complete || len(answer.Items) != 1 || answer.Items[0].ID != "good" || answer.Sync.Dropped != 1 {
			t.Fatalf("answer = complete %v, items %+v, sync %+v", answer.Complete, answer.Items, answer.Sync)
		}
	})
}

// Events left out because they change nothing (sign-ins, tokens, reads)
// are summed per pair into sync.not_changes and never listed.
func TestNotChangesCountedInSync(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		now := fixture.clock.Now()
		covered := []changes.Interval{{From: now.Add(-26 * time.Hour), To: now.Add(-15 * time.Minute)}}
		read := []changes.Interval{{From: now.Add(-26 * time.Hour), To: now}}
		fixture.poll(account, region, ChangePoll{Found: []model.Change{feedChange("kept", now.Add(-time.Hour), model.ActorPipeline, "")},
			Covered: covered, Read: read, NotChanges: 3})
		fixture.poll(account, model.GlobalRegion, ChangePoll{Covered: covered, Read: read, NotChanges: 2})
		answer := fixture.answer(FeedRequest{})
		if len(answer.Items) != 1 || answer.Sync.NotChanges != 5 {
			t.Fatalf("items %+v, sync %+v", answer.Items, answer.Sync)
		}
	})
}

// Under steady overload every poll leaves a new covered island. The pair
// keeps a bounded interval count; the dropped islands read too_busy.
func TestCoverageBoundedUnderSteadyOverload(t *testing.T) {
	if testing.Short() {
		t.Skip("seven simulated days of polls; skipped under -short")
	}
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		start := fixture.clock.Now().Add(-7 * 24 * time.Hour)
		for poll := 0; poll < 7*288; poll++ {
			at := start.Add(time.Duration(poll) * 5 * time.Minute)
			fixture.poll(account, region, ChangePoll{Covered: []changes.Interval{{From: at.Add(3 * time.Minute), To: at.Add(5 * time.Minute)}},
				Gaps: []ChangeGapLabel{{From: at, To: at.Add(3 * time.Minute), Reason: GapTooBusy}}})
		}
		checkpoint, err := fixture.store.Checkpoint(account, region)
		if err != nil {
			t.Fatal(err)
		}
		if len(checkpoint.Covered) > storage.DefaultAWSChangeIntervalCap {
			t.Fatalf("intervals = %d", len(checkpoint.Covered))
		}
		answer := fixture.answer(FeedRequest{})
		for _, gap := range gapsOf(answer, region) {
			if gap.Reason != string(GapTooBusy) && gap.Reason != string(GapArriving) {
				t.Fatalf("overload gap = %+v", gap)
			}
		}
	})
}

// Golden edges of the burst rule: the chain window boundary, mixed error
// codes and mixed principals.
func TestBurstRuleEdges(t *testing.T) {
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	count := func(minute int, actor, code string, events int64) storage.AWSChangeCount {
		return storage.AWSChangeCount{Kind: storage.AWSChangeCountFailure, Account: account, Region: region, EventName: "PutRolePolicy", Actor: actor,
			ErrorCode: code, Minute: base.Add(time.Duration(minute) * time.Minute), Events: events}
	}
	cases := []struct {
		name   string
		counts []storage.AWSChangeCount
		want   []int64
	}{
		{"exactly ten minutes apart chains", []storage.AWSChangeCount{count(0, "a", "AccessDenied", 3), count(10, "a", "AccessDenied", 2)}, []int64{5}},
		{"eleven minutes apart splits", []storage.AWSChangeCount{count(0, "a", "AccessDenied", 3), count(11, "a", "AccessDenied", 3)}, nil},
		{"below five is no burst", []storage.AWSChangeCount{count(0, "a", "AccessDenied", 4)}, nil},
		{"mixed codes never merge", []storage.AWSChangeCount{count(0, "a", "AccessDenied", 3), count(1, "a", "Throttling", 3)}, nil},
		{"mixed principals never merge", []storage.AWSChangeCount{count(0, "a", "AccessDenied", 3), count(1, "b", "AccessDenied", 3)}, nil},
		{"two bursts for one key", []storage.AWSChangeCount{count(0, "a", "AccessDenied", 5), count(30, "a", "AccessDenied", 6)}, []int64{5, 6}},
	}
	for _, tc := range cases {
		var got []int64
		for _, spans := range formBursts(tc.counts, DefaultBurstWindow, DefaultBurstMin) {
			for _, span := range spans {
				got = append(got, span.events)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: bursts %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A pipeline's stack update and the calls CloudFormation made for it are
// one group; expanding it lists them.
func TestStackRunGroupsCloudFormationCalls(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	pipeline := "arn:aws:iam::111122223333:role/github-deploy"
	stack := feedChange("stack", now.Add(-2*time.Hour), model.ActorPipeline, pipeline)
	stack.Template, stack.EventSource, stack.EventName, stack.Service, stack.Fields = "cloudformation.update_stack", "cloudformation.amazonaws.com", "UpdateStack", "cloudformation", map[string]string{"stack": "network-prod"}
	found := []model.Change{stack}
	for index := 0; index < 3; index++ {
		member := feedChange(fmt.Sprintf("m%d", index), now.Add(-2*time.Hour+time.Duration(index+1)*time.Minute), model.ActorPipeline, pipeline)
		member.Actor.Via = "cloudformation"
		found = append(found, member)
	}
	found = append(found, feedChange("alone", now.Add(-time.Hour), model.ActorPerson, "arn:aws:iam::111122223333:user/dave"))
	fixture.pollSettled(account, region, found...)
	answer := fixture.answer(FeedRequest{})
	if len(answer.Items) != 2 || answer.Items[1].Kind != ItemGroup || *answer.Items[1].Count != 3 || answer.Items[1].Summary != "Applied 3 changes to stack network-prod" {
		t.Fatalf("items = %+v", answer.Items)
	}
	group, err := fixture.feed.Group(context.Background(), answer.Items[1].ID, "", 50)
	if err != nil || len(group.Items) != 4 {
		t.Fatalf("group = %+v, %v", group, err)
	}
}

// Adding a key moves writes to the licensed organization. The history the
// default organization read before stays in the answer: its changes are
// listed (a change both read is listed once), and the stretch it read is
// never labelled as before the connection.
func TestUpgradeKeepsDefaultOrgChangeHistory(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		before := fixture.clock.Now()
		fixture.pollSettled(account, region, feedChange("old-1", before.Add(-25*time.Hour), model.ActorPipeline, "arn:aws:iam::111122223333:role/deploy"),
			feedChange("old-2", before.Add(-2*time.Hour), model.ActorPipeline, "arn:aws:iam::111122223333:role/deploy"))
		fixture.pollSettled(account, model.GlobalRegion)

		fixture.clock.Advance(time.Hour)
		now := fixture.clock.Now()
		upgraded := tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
		licensed := New(fixture.provider, upgraded, WithClock(fixture.clock.Now))
		if err := licensed.RecordChangePoll(account, region, ChangePoll{HistoryFrom: now.Add(-24 * time.Hour),
			Found:   []model.Change{feedChange("old-2", before.Add(-2*time.Hour), model.ActorPipeline, "arn:aws:iam::111122223333:role/deploy"), feedChange("new-1", now.Add(-30*time.Minute), model.ActorPipeline, "arn:aws:iam::111122223333:role/deploy")},
			Covered: []changes.Interval{{From: now.Add(-24 * time.Hour), To: now.Add(-15 * time.Minute)}}, Read: []changes.Interval{{From: now.Add(-24 * time.Hour), To: now}}}); err != nil {
			t.Fatal(err)
		}

		state, found, err := fixture.provider.AWSChangePairState(context.Background(), upgraded, account, region)
		if err != nil || !found {
			t.Fatalf("pair state found=%v err=%v", found, err)
		}
		if !state.HistoryFrom.Equal(before.Add(-48*time.Hour)) || !state.ConnectedAt.Equal(before) || !state.LastPollAt.Equal(now) || state.Org != "acme" {
			t.Fatalf("pair state after the upgrade = %+v", state)
		}

		week := 7 * 24 * time.Hour
		feed := NewChangeFeed(licensed, WithFeedClock(fixture.clock.Now), WithScope(func(context.Context) ([]ScopeAccount, bool, error) { return fixture.scope, true, nil }),
			WithChangesLimits(func() ChangesLimits { return ChangesLimits{MaxWindow: week, MaxCompareAge: week} }))
		answer, err := feed.Answer(context.Background(), FeedRequest{Window: week})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]int{}
		for _, item := range answer.Items {
			seen[item.ID]++
		}
		if len(answer.Items) != 3 || seen["old-1"] != 1 || seen["old-2"] != 1 || seen["new-1"] != 1 {
			t.Fatalf("items after the upgrade = %v", seen)
		}
		for _, gap := range gapsOf(answer, region) {
			if gap.Reason == string(GapBeforeConnect) && gap.To.After(before.Add(-48*time.Hour)) {
				t.Fatalf("read history labelled before connect: %+v", gap)
			}
		}
	})
}

// With since and until off minute boundaries the store's first minute row
// starts before since; the buckets still add up to the summary, by class
// and for failures, at every window.
func TestBucketsSumToSummaryOffMinuteBoundaries(t *testing.T) {
	until := time.Date(2026, 11, 20, 13, 44, 37, 0, time.UTC)
	classes := []model.ActorClass{model.ActorPipeline, model.ActorPerson, model.ActorAWSService, model.ActorUnknown}
	for _, window := range []struct {
		name   string
		since  time.Time
		until  time.Time
		window time.Duration
	}{
		{"1h", until.Add(-FeedWindowHour), until, FeedWindowHour},
		{"24h", until.Add(-FeedWindowDay), until, FeedWindowDay},
		{"7d", until.Add(-7 * 24 * time.Hour), until, 7 * 24 * time.Hour},
		{"custom", time.Date(2026, 11, 19, 2, 13, 41, 0, time.UTC), time.Date(2026, 11, 19, 9, 51, 8, 0, time.UTC), 0},
	} {
		t.Run(window.name, func(t *testing.T) {
			since, end := window.since, window.until
			span := end.Sub(since)
			if window.window == 0 {
				window.window = span
			}
			// Change times as the store keeps them: in [since, until), with
			// one change in the first and last partial minutes each.
			times := []time.Time{since.Add(13 * time.Second), end.Add(-2 * time.Second)}
			for at := since.Add(time.Minute); at.Before(end); at = at.Add(span / 97) {
				times = append(times, at)
			}
			byMinute := map[time.Time]int64{}
			var stored storage.AWSChangeFeed
			for index, at := range times {
				minute := at.Truncate(time.Minute)
				events := int64(index%4 + 1)
				stored.Minutes = append(stored.Minutes, storage.AWSChangeMinute{Minute: minute, ActorClass: string(classes[index%len(classes)]), Events: events})
				if index%3 == 0 {
					byMinute[minute]++
				}
			}
			for minute, events := range byMinute {
				stored.Counts = append(stored.Counts, storage.AWSChangeCount{Kind: storage.AWSChangeCountFailure, Minute: minute, Events: events})
			}
			view := coverageView{since: since, until: end, settled: end, pairs: 1}
			out := buckets(since, end, bucketWidth(window.window), stored, nil, view)
			summary := summarize(stored, stored.Counts, nil)
			var sum FeedBucket
			for _, bucket := range out {
				sum.Pipeline += bucket.Pipeline
				sum.Person += bucket.Person
				sum.AWSService += bucket.AWSService
				sum.Unknown += bucket.Unknown
				sum.Errors += bucket.Errors
			}
			if out[0].Pipeline == 0 {
				t.Fatalf("first bucket misses the first partial minute: %+v", out[0])
			}
			if sum.Pipeline != summary.ByActorClass[string(model.ActorPipeline)] || sum.Person != summary.ByActorClass[string(model.ActorPerson)] ||
				sum.AWSService != summary.ByActorClass[string(model.ActorAWSService)] || sum.Unknown != summary.ByActorClass[string(model.ActorUnknown)] {
				t.Fatalf("bucket classes %+v, summary %+v", sum, summary.ByActorClass)
			}
			if total := sum.Pipeline + sum.Person + sum.AWSService + sum.Unknown; total != summary.Total {
				t.Fatalf("bucket total %d, summary total %d", total, summary.Total)
			}
			if sum.Errors != summary.Errors || summary.Errors == 0 {
				t.Fatalf("bucket errors %d, summary errors %d", sum.Errors, summary.Errors)
			}
		})
	}
}
