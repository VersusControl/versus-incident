package snapshot

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// One account's denial leaves what the other account read on the grid: a
// bucket with read counts and an unread account is partial, keeps its
// counts and names the unread account; it is gap only when nothing in it
// was read.
func TestOneAccountDenialLeavesOthersCountsInBuckets(t *testing.T) {
	const staging = "444455556666"
	fixture := newFeedFixture(t, []ScopeAccount{{Account: account, Regions: []string{region}}, {Account: staging, Regions: []string{region}}})
	now := fixture.clock.Now()
	fixture.pollSettled(account, region, feedChange("deploy", now.Add(-2*time.Hour+time.Minute), model.ActorPipeline, ""))
	fixture.pollSettled(account, model.GlobalRegion)
	for _, pair := range []string{region, model.GlobalRegion} {
		fixture.poll(staging, pair, ChangePoll{ErrorCode: "denied",
			Gaps: []ChangeGapLabel{{From: now.Add(-26 * time.Hour), To: now, Reason: GapCloudTrailDenied, Detail: "cloudtrail:LookupEvents"}}})
	}
	answer := fixture.answer(FeedRequest{})
	var counted *FeedBucket
	for index := range answer.Buckets {
		bucket := &answer.Buckets[index]
		if bucket.State == BucketGap {
			t.Fatalf("bucket %s is gap though Production was read: %+v", bucket.Start, bucket)
		}
		if bucket.Pipeline > 0 {
			counted = bucket
		}
	}
	if counted == nil || counted.State != BucketPartial || len(counted.GapAccounts) != 1 || counted.GapAccounts[0] != staging || !answer.Summary.TotalIsLowerBound {
		t.Fatalf("read bucket = %+v, lower bound %v", counted, answer.Summary.TotalIsLowerBound)
	}
	only := newFeedFixture(t, []ScopeAccount{{Account: staging, Regions: []string{region}}})
	only.poll(staging, region, ChangePoll{ErrorCode: "denied", Gaps: []ChangeGapLabel{{From: now.Add(-26 * time.Hour), To: now, Reason: GapCloudTrailDenied}}})
	for _, bucket := range only.answer(FeedRequest{}).Buckets {
		if bucket.State != BucketGap {
			t.Fatalf("nothing read, bucket %+v", bucket)
		}
	}
}

// Search matches the raw API name in service:Action form as well as the
// bare action and the sentence.
func TestSearchMatchesAPIName(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		tagging := feedChange("tag", fixture.clock.Now().Add(-time.Hour), model.ActorPerson, "")
		tagging.EventSource, tagging.EventName, tagging.Service, tagging.Summary = "s3.amazonaws.com", "PutBucketTagging", "s3", "Tags changed on bucket logs"
		fixture.pollSettled(account, region, tagging, feedChange("other", fixture.clock.Now().Add(-time.Hour), model.ActorPerson, ""))
		for _, q := range []string{"s3:PutBucketTagging", "PutBucketTagging", "tags changed"} {
			if answer := fixture.answer(FeedRequest{Text: q}); answer.Summary.Total != 1 || len(answer.Items) != 1 || answer.Items[0].ID != "tag" {
				t.Fatalf("q=%q: total %d, items %+v", q, answer.Summary.Total, answer.Items)
			}
		}
	})
}

// Compare's history_starts is the first time every unit in scope has a
// snapshot: comparing from it leaves no unit without history.
func TestCompareHistoryStartsIsFullCompareTime(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	first := fixture.clock.Now()
	if _, err := fixture.store.Commit(model.Unit{Account: account, Region: region, Service: "ec2"}, candidate("i-1")); err != nil {
		t.Fatal(err)
	}
	fixture.clock.Advance(10 * time.Minute)
	last := fixture.clock.Now()
	if _, err := fixture.store.Commit(model.Unit{Account: account, Region: model.GlobalRegion, Service: "iam"},
		Candidate{Resources: []model.Resource{{ARN: "arn:aws:iam::111122223333:role/deploy", Service: "iam", Type: "role", Account: account, Name: "deploy"}}}); err != nil {
		t.Fatal(err)
	}
	fixture.clock.Advance(time.Hour)
	early, err := fixture.feed.Compare(context.Background(), CompareRequest{From: first.Add(-time.Minute)})
	if err != nil || early.HistoryStarts == nil || !early.HistoryStarts.Equal(last) || early.Snapshots.Oldest == nil || !early.Snapshots.Oldest.Equal(first) {
		t.Fatalf("early compare = %+v, %v", early, err)
	}
	full, err := fixture.feed.Compare(context.Background(), CompareRequest{From: *early.HistoryStarts})
	if err != nil || full.Status != CompareOK || len(full.Partial) != 0 || !full.Complete {
		t.Fatalf("compare from history_starts = %+v, %v", full, err)
	}
}

// Look-alike items (one name and type) carry an id_hint that tells them
// apart, and an incomplete compare always names why.
func TestCompareIDHintsAndIncompleteReason(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	ec2 := model.Unit{Account: account, Region: region, Service: "ec2"}
	var web []model.Resource
	for index := 0; index < 4; index++ {
		resource := instance(fmt.Sprintf("i-%d", index))
		resource.Name = "versus-web"
		web = append(web, resource)
	}
	if _, err := fixture.store.Commit(ec2, Candidate{Resources: web}); err != nil {
		t.Fatal(err)
	}
	from := fixture.clock.Now()
	fixture.clock.Advance(time.Hour)
	if _, err := fixture.store.Commit(ec2, candidate("i-other")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.RecordFailure(ec2, "denied", model.Gap{Account: account, Region: region, Service: "ec2", Reason: model.GapDenied}); err != nil {
		t.Fatal(err)
	}
	answer, err := fixture.feed.Compare(context.Background(), CompareRequest{From: from.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	hints := map[string]bool{}
	for _, removed := range answer.Removed {
		if removed.IDHint == "" || hints[removed.IDHint] {
			t.Fatalf("removed look-alikes = %+v", answer.Removed)
		}
		hints[removed.IDHint] = true
	}
	if len(hints) != 4 {
		t.Fatalf("hints = %v", hints)
	}
	named := false
	for _, partial := range answer.Partial {
		named = named || partial.Service == "ec2" && partial.Reason == string(model.GapDenied)
	}
	if answer.Complete || !named {
		t.Fatalf("incomplete without a reason: %+v", answer.Partial)
	}
}

var _ = changes.Interval{}

// Identifier fields (names, IDs, ARNs of the resource acted on) are not
// listed as changed fields; value fields are.
func TestIdentifierFieldsAreNotChangedFields(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	change := feedChange("scale", fixture.clock.Now().Add(-time.Hour), model.ActorPipeline, "")
	change.Resources = []string{serviceARN}
	change.Fields = map[string]string{"service": "web", "cluster": "main", "service_arn": serviceARN, "desired": "5"}
	fixture.pollSettled(account, region, change)
	answer := fixture.answer(FeedRequest{})
	if len(answer.Items) != 1 || len(answer.Items[0].Fields) != 1 || answer.Items[0].Fields[0].Field != "desired" {
		t.Fatalf("fields = %+v", answer.Items[0].Fields)
	}
}

// Every account-level outage carries as_of: the end of the account's
// change coverage, else its last successful check.
func TestEveryOutageCarriesAsOf(t *testing.T) {
	for _, reason := range []ChangeGapReason{GapAssumeRoleFailed, GapCredentialsInvalid, GapAccountMismatch, GapAccountDisconnected} {
		fixture := newFeedFixture(t, scopeOf(region))
		now := fixture.clock.Now()
		covered := now.Add(-2 * time.Hour)
		fixture.poll(account, region, ChangePoll{Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: covered}}})
		fixture.scope[0].Outage, fixture.scope[0].OutageSince = reason, covered.Add(10*time.Minute)
		answer := fixture.answer(FeedRequest{})
		if len(answer.Accounts) != 1 || answer.Accounts[0].AsOf == nil || !answer.Accounts[0].AsOf.Equal(covered) {
			t.Fatalf("%s: accounts = %+v", reason, answer.Accounts)
		}
		unread := newFeedFixture(t, scopeOf(region))
		checked := now.Add(-3 * time.Hour)
		unread.scope[0].Outage, unread.scope[0].OutageSince, unread.scope[0].LastReadAt = reason, now.Add(-time.Hour), checked
		if got := unread.answer(FeedRequest{}).Accounts; len(got) != 1 || got[0].AsOf == nil || !got[0].AsOf.Equal(checked) {
			t.Fatalf("%s without coverage: accounts = %+v", reason, got)
		}
	}
}

// A region_not_enabled reason a poll wrote is a real gap: the answer and
// its account are incomplete and the total is a lower bound. Only a region
// Settings recorded as not enabled is "not a gap", and even then a failed
// read it had before keeps its real reason.
func TestPollDerivedRegionNotEnabledIsARealGap(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf("eu-west-1"))
	now := fixture.clock.Now()
	fixture.pollSettled(account, model.GlobalRegion)
	fixture.poll(account, "eu-west-1", ChangePoll{ErrorCode: "region_disabled",
		Gaps: []ChangeGapLabel{{From: now.Add(-24 * time.Hour), To: now, Reason: ChangeGapReasonForCode("region_disabled")}}})
	fixture.clock.Advance(2 * time.Minute)
	answer := fixture.answer(FeedRequest{Window: time.Hour})
	if answer.Complete || !answer.Summary.TotalIsLowerBound || answer.Accounts[0].Complete {
		t.Fatalf("poll-derived not enabled = complete %v, lower %v, accounts %+v", answer.Complete, answer.Summary.TotalIsLowerBound, answer.Accounts)
	}
	for _, bucket := range answer.Buckets {
		if bucket.State == BucketComplete || bucket.State == BucketArriving {
			t.Fatalf("a bucket over an unread region reads as read: %+v", bucket)
		}
	}
	if ChangeGapReasonForCode("not_available_in_region") != GapCollectorDown {
		t.Fatal("a host that does not resolve reads as a disabled region")
	}

	// Settings now records the region as off: its opt-in refusals are the
	// confirmed state, not a gap; a read that failed for another reason
	// stays a real gap.
	fixture.scope[0].Regions, fixture.scope[0].NotEnabled = nil, []string{"eu-west-1"}
	if confirmed := fixture.answer(FeedRequest{Window: time.Hour}); !confirmed.Complete {
		t.Fatalf("opt-in refusals of a region confirmed off are still gaps: %+v", confirmed.Gaps)
	}
	fixture.poll(account, "eu-west-1", ChangePoll{ErrorCode: "throttled",
		Gaps: []ChangeGapLabel{{From: now.Add(-30 * time.Minute), To: now, Reason: GapChangesThrottled}}})
	if throttled := fixture.answer(FeedRequest{Window: time.Hour}); throttled.Complete {
		t.Fatalf("a throttled read of a region confirmed off reads complete: %+v", throttled.Gaps)
	}
	clean := newFeedFixture(t, []ScopeAccount{{Account: account, Regions: nil, NotEnabled: []string{"eu-west-1"}}})
	clean.pollSettled(account, model.GlobalRegion)
	if answer := clean.answer(FeedRequest{Window: time.Hour}); !answer.Complete {
		t.Fatalf("a region Settings recorded off and never polled is a gap: %+v", answer.Gaps)
	}
}

// Cursors and group tokens are not signed: one whose window starts before
// the tier window plus the paging allowance is expired, judged against the
// real now.
func TestForgedCursorAndGroupTokenExpire(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	old := now.Add(-7 * 24 * time.Hour)
	cursor := encodeCursor(feedCursor{RecordedBefore: old.Add(23 * time.Hour), Anchor: old.Add(2 * time.Hour), ID: "~", Since: old, Until: old.Add(time.Hour)})
	if _, err := fixture.feed.Answer(context.Background(), FeedRequest{Cursor: cursor}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("forged cursor = %v", err)
	}
	group := encodeGroup(groupKey{Kind: ItemBurst, Account: account, Region: region, From: old, To: old.Add(time.Hour)})
	if _, err := fixture.feed.Group(context.Background(), group, "", 10); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("forged group token = %v", err)
	}
	recent := encodeCursor(feedCursor{RecordedBefore: now, Anchor: now, ID: "~", Since: now.Add(-24*time.Hour - 30*time.Minute), Until: now.Add(-30 * time.Minute)})
	if _, err := fixture.feed.Answer(context.Background(), FeedRequest{Cursor: recent}); err != nil {
		t.Fatalf("cursor inside the paging allowance = %v", err)
	}
}

// A hand-made cursor without its window, sent with an old until and a short
// window, never serves week-old changes: a cursor must carry its window, and
// every request is held to the tier against the real now.
func TestCursorWithoutWindowCannotBypassTheTier(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	old := now.Add(-6 * 24 * time.Hour)
	fixture.poll(account, region, ChangePoll{Found: []model.Change{feedChange("week-old", old.Add(-30*time.Minute), model.ActorPipeline, "")},
		Covered: []changes.Interval{{From: old.Add(-2 * time.Hour), To: now.Add(-15 * time.Minute)}}})
	bare := encodeCursor(feedCursor{RecordedBefore: now, Anchor: now, ID: "~"})
	if _, err := fixture.feed.Answer(context.Background(), FeedRequest{Cursor: bare, Until: old, Window: time.Hour}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("cursor without a window = %v", err)
	}
	if _, err := fixture.feed.Answer(context.Background(), FeedRequest{Until: old, Window: time.Hour}); !errors.Is(err, ErrWindowLocked) {
		t.Fatalf("old until without a cursor = %v", err)
	}
}

// A service Settings records as denied or not on the account's plan is not
// "not read yet" in a compare: it is named with that reason, excluded, and
// does not keep the answer from being complete.
func TestCompareExcludesServicesSettingsKnowsUnreadable(t *testing.T) {
	scope := scopeOf(region)
	scope[0].DeniedServices, scope[0].NotOnPlanServices = []string{"elasticache"}, []string{"aws-health"}
	fixture := newFeedFixture(t, scope)
	ec2 := model.Unit{Account: account, Region: region, Service: "ec2"}
	iam := model.Unit{Account: account, Region: model.GlobalRegion, Service: "iam"}
	role := Candidate{Resources: []model.Resource{{ARN: "arn:aws:iam::111122223333:role/deploy", Service: "iam", Type: "role", Account: account, Name: "deploy"}}}
	for _, step := range []time.Duration{0, time.Hour} {
		fixture.clock.Advance(step)
		if _, err := fixture.store.Commit(ec2, candidate("i-1")); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.Commit(iam, role); err != nil {
			t.Fatal(err)
		}
	}
	from := fixture.clock.Now().Add(-time.Hour)
	for _, unit := range []model.Unit{{Account: account, Region: region, Service: "elasticache"}, {Account: account, Region: model.GlobalRegion, Service: "aws-health"}} {
		if err := fixture.store.RecordFailure(unit, "denied", model.Gap{Account: unit.Account, Region: unit.Region, Service: unit.Service, Reason: model.GapNotChecked}); err != nil {
			t.Fatal(err)
		}
	}
	answer, err := fixture.feed.Compare(context.Background(), CompareRequest{From: from.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"elasticache": CompareServiceDenied, "aws-health": CompareNotOnPlan}
	for _, partial := range answer.Partial {
		if reason, ok := want[partial.Service]; ok && partial.Reason == reason && partial.Excluded {
			delete(want, partial.Service)
			continue
		}
		t.Fatalf("partial entry not excluded with its Settings reason: %+v", partial)
	}
	if len(want) != 0 || !answer.Complete || answer.Status != CompareOK {
		t.Fatalf("missing %v; complete %v, status %s, partial %+v", want, answer.Complete, answer.Status, answer.Partial)
	}
}

// A service AWS does not offer in a Region, and AWS Health off the plan
// when Settings has not recorded it, are excluded with their own reason. A
// unit with no history at from, and a configured Region with none at all,
// read not_checked: no partial entry is ever without a reason.
func TestComparePartialReasons(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region, "ap-southeast-5"))
		ec2 := model.Unit{Account: account, Region: region, Service: "ec2"}
		for _, step := range []time.Duration{0, time.Hour} {
			fixture.clock.Advance(step)
			if _, err := fixture.store.Commit(ec2, candidate("i-1")); err != nil {
				t.Fatal(err)
			}
		}
		from := fixture.clock.Now().Add(-time.Hour).Add(time.Minute)
		appregistry := model.Unit{Account: account, Region: region, Service: "appregistry"}
		if err := fixture.store.RecordSuccess(appregistry, []model.Gap{{Account: account, Region: region, Service: "appregistry", Reason: model.GapNotInRegion}}); err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.RecordFailure(model.Unit{Account: account, Region: model.GlobalRegion, Service: "aws-health"}, "not_on_plan"); err != nil {
			t.Fatal(err)
		}
		fixture.clock.Advance(time.Minute)
		iam := model.Unit{Account: account, Region: model.GlobalRegion, Service: "iam"}
		if _, err := fixture.store.Commit(iam, Candidate{Resources: []model.Resource{{ARN: "arn:aws:iam::111122223333:role/deploy", Service: "iam", Type: "role", Account: account, Name: "deploy"}}}); err != nil {
			t.Fatal(err)
		}
		answer, err := fixture.feed.Compare(context.Background(), CompareRequest{From: from})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]struct {
			reason   string
			excluded bool
		}{
			"appregistry": {string(model.GapNotInRegion), true},
			"aws-health":  {CompareNotOnPlan, true},
			"iam":         {string(model.GapNotChecked), false},
			"":            {string(model.GapNotChecked), false},
		}
		for _, partial := range answer.Partial {
			if partial.Reason == "" {
				t.Fatalf("partial entry without a reason: %+v", partial)
			}
			expected, ok := want[partial.Service]
			if !ok || partial.Reason != expected.reason || partial.Excluded != expected.excluded {
				t.Fatalf("partial = %+v, want %+v", partial, expected)
			}
			delete(want, partial.Service)
		}
		if len(want) != 0 || answer.Complete {
			t.Fatalf("missing %v; complete %v, partial %+v", want, answer.Complete, answer.Partial)
		}
	})
}

// An account Settings records as disconnected reads one story: reason
// account_disconnected with its disconnect time; as_of is the earliest
// settled end across its Regions; each Region's gap starts at its own
// settled end, a stored row newer than as_of shrinks no gap, and the
// unsettled tail is an account_disconnected gap, not still arriving.
func TestDisconnectedAccountAsOfIsFirstGapStart(t *testing.T) {
	trailing := func(t *testing.T, answer ChangesAnswer, pair string) FeedGap {
		t.Helper()
		var first FeedGap
		for _, gap := range answer.Gaps {
			if gap.Account != account || gap.Region != pair {
				continue
			}
			if gap.Reason == string(GapArriving) {
				t.Fatalf("disconnected account has an arriving gap: %+v", answer.Gaps)
			}
			if first.From.IsZero() || gap.From.Before(first.From) {
				first = gap
			}
		}
		if first.From.IsZero() {
			t.Fatalf("no gap for %s: %+v", pair, answer.Gaps)
		}
		return first
	}

	t.Run("earliest Region end with an untrimmed gap per Region", func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		now := fixture.clock.Now()
		regionalEnd, globalEnd, rowAt := now.Add(-50*time.Minute), now.Add(-30*time.Minute), now.Add(-20*time.Minute)
		fixture.poll(account, region, ChangePoll{Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: regionalEnd}},
			Read: []changes.Interval{{From: now.Add(-26 * time.Hour), To: regionalEnd}}})
		row := feedChange("deploy", rowAt, model.ActorPipeline, "")
		row.Region = model.GlobalRegion
		fixture.poll(account, model.GlobalRegion, ChangePoll{Found: []model.Change{row},
			Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: globalEnd}}, Read: []changes.Interval{{From: now.Add(-26 * time.Hour), To: now.Add(-15 * time.Minute)}}})
		fixture.clock.Advance(time.Minute)
		now = fixture.clock.Now()
		fixture.poll(account, region, ChangePoll{ErrorCode: "trust_denied", Gaps: []ChangeGapLabel{{From: regionalEnd, To: now, Reason: GapAssumeRoleFailed}}})
		fixture.poll(account, model.GlobalRegion, ChangePoll{ErrorCode: "trust_denied", Gaps: []ChangeGapLabel{{From: globalEnd, To: now, Reason: GapAssumeRoleFailed}}})
		disconnectedAt := now.Add(-10 * time.Minute)
		fixture.scope[0].Outage, fixture.scope[0].OutageSince, fixture.scope[0].Disconnected = GapAssumeRoleFailed, disconnectedAt, true
		answer := fixture.answer(FeedRequest{})
		if len(answer.Accounts) != 1 {
			t.Fatalf("accounts = %+v", answer.Accounts)
		}
		got := answer.Accounts[0]
		if got.Complete || answer.Complete || got.Reason == nil || *got.Reason != string(GapAccountDisconnected) || got.DisconnectedAt == nil || !got.DisconnectedAt.Equal(disconnectedAt) {
			t.Fatalf("disconnected account = %+v, answer complete %v", got, answer.Complete)
		}
		if got.AsOf == nil || !got.AsOf.Equal(regionalEnd) {
			t.Fatalf("as_of = %v, want earliest Region end %s; gaps %+v", got.AsOf, regionalEnd, answer.Gaps)
		}
		if gap := trailing(t, answer, region); !gap.From.Equal(regionalEnd) {
			t.Fatalf("%s gap from %s, want %s", region, gap.From, regionalEnd)
		}
		if gap := trailing(t, answer, model.GlobalRegion); !gap.From.Equal(globalEnd) {
			t.Fatalf("global gap from %s, want %s (row at %s must not trim it)", gap.From, globalEnd, rowAt)
		}
		if len(answer.Items) != 1 || answer.Items[0].Region != model.GlobalRegion || !answer.Items[0].Time.Equal(rowAt) {
			t.Fatalf("row newer than as_of must stay listed: %+v", answer.Items)
		}
	})

	t.Run("unsettled tail is account_disconnected", func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		fixture.pollSettled(account, region)
		fixture.pollSettled(account, model.GlobalRegion)
		now := fixture.clock.Now()
		settledEnd := now.Add(-15 * time.Minute)
		fixture.scope[0].Outage, fixture.scope[0].OutageSince, fixture.scope[0].Disconnected = GapAssumeRoleFailed, now, true
		answer := fixture.answer(FeedRequest{})
		got := answer.Accounts[0]
		if got.Complete || answer.Complete || got.AsOf == nil || !got.AsOf.Equal(settledEnd) {
			t.Fatalf("account = %+v, answer complete %v, want as_of %s; gaps %+v", got, answer.Complete, settledEnd, answer.Gaps)
		}
		for _, pair := range []string{region, model.GlobalRegion} {
			if gap := trailing(t, answer, pair); !gap.From.Equal(settledEnd) || gap.Reason != string(GapAccountDisconnected) {
				t.Fatalf("%s tail = %+v, want account_disconnected from %s", pair, gap, settledEnd)
			}
		}
		for _, entry := range answer.Regions {
			if entry.Complete {
				t.Fatalf("region %s reads complete for a disconnected account: %+v", entry.Region, answer.Regions)
			}
		}
	})
}

// Every real gap in a disconnected account's trailing run reads
// account_disconnected from as_of on, whatever the collector recorded there
// (a role failure, or a tail still arriving). A gap before the trailing run
// keeps its own reason.
func TestDisconnectedAccountTrailingGapsReadAccountDisconnected(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	start := now.Add(-26 * time.Hour)
	throttledFrom, throttledTo := now.Add(-3*time.Hour), now.Add(-2*time.Hour)
	regionalEnd, failedTo := now.Add(-50*time.Minute), now.Add(-20*time.Minute)
	fixture.poll(account, region, ChangePoll{
		Covered: []changes.Interval{{From: start, To: throttledFrom}, {From: throttledTo, To: regionalEnd}},
		Read:    []changes.Interval{{From: start, To: throttledFrom}, {From: throttledTo, To: regionalEnd}},
		Gaps:    []ChangeGapLabel{{From: throttledFrom, To: throttledTo, Reason: GapChangesThrottled}},
	})
	fixture.pollSettled(account, model.GlobalRegion)
	globalEnd := now.Add(-15 * time.Minute)
	fixture.poll(account, region, ChangePoll{ErrorCode: "trust_denied", Gaps: []ChangeGapLabel{{From: regionalEnd, To: failedTo, Reason: GapAssumeRoleFailed}}})
	fixture.clock.Advance(2 * time.Minute)

	before := fixture.answer(FeedRequest{})
	seen := map[string]bool{}
	for _, gap := range before.Gaps {
		seen[gap.Reason] = true
	}
	if !seen[string(GapAssumeRoleFailed)] || !seen[string(GapArriving)] {
		t.Fatalf("setup: want an assume_role_failed and an arriving tail before the disconnect: %+v", before.Gaps)
	}

	fixture.scope[0].Outage, fixture.scope[0].OutageSince, fixture.scope[0].Disconnected = GapAssumeRoleFailed, now, true
	answer := fixture.answer(FeedRequest{})
	got := answer.Accounts[0]
	if got.AsOf == nil || !got.AsOf.Equal(regionalEnd) {
		t.Fatalf("as_of = %v, want %s; gaps %+v", got.AsOf, regionalEnd, answer.Gaps)
	}
	throttled, tails := 0, map[string]time.Time{}
	for _, gap := range answer.Gaps {
		if gap.Account != account {
			continue
		}
		switch {
		case gap.From.Before(regionalEnd):
			if gap.Reason != string(GapChangesThrottled) || !gap.From.Equal(throttledFrom) {
				t.Fatalf("gap before the trailing run lost its reason: %+v", gap)
			}
			throttled++
		default:
			if gap.Reason != string(GapAccountDisconnected) {
				t.Fatalf("trailing gap %+v, want account_disconnected; gaps %+v", gap, answer.Gaps)
			}
			if first, ok := tails[gap.Region]; !ok || gap.From.Before(first) {
				tails[gap.Region] = gap.From
			}
		}
	}
	if throttled != 1 {
		t.Fatalf("throttled gaps = %d, want 1: %+v", throttled, answer.Gaps)
	}
	if !tails[region].Equal(regionalEnd) || !tails[model.GlobalRegion].Equal(globalEnd) {
		t.Fatalf("trailing runs start %v, want %s and %s: %+v", tails, regionalEnd, globalEnd, answer.Gaps)
	}
}

// Top actors merge one principal across accounts and Regions: the same
// role name and class reads once, with the summed count.
func TestTopActorsMergeAcrossAccounts(t *testing.T) {
	const staging = "444455556666"
	fixture := newFeedFixture(t, []ScopeAccount{{Account: account, Regions: []string{region}}, {Account: staging, Regions: []string{region}}})
	now := fixture.clock.Now()
	production := feedChange("p1", now.Add(-time.Hour), model.ActorPipeline, "arn:aws:iam::111122223333:role/terraform-apply")
	again := feedChange("p2", now.Add(-50*time.Minute), model.ActorPipeline, "arn:aws:iam::111122223333:role/terraform-apply")
	other := feedChange("s1", now.Add(-40*time.Minute), model.ActorPipeline, "arn:aws:iam::444455556666:role/terraform-apply")
	other.Account = staging
	fixture.pollSettled(account, region, production, again)
	fixture.pollSettled(staging, region, other)
	actors := fixture.answer(FeedRequest{}).Summary.TopActors
	seen := map[string]bool{}
	for _, actor := range actors {
		if seen[actor.Actor] {
			t.Fatalf("top actors repeat %q: %+v", actor.Actor, actors)
		}
		seen[actor.Actor] = true
	}
	if len(actors) != 1 || actors[0].Actor != "terraform-apply" || actors[0].Count != 3 {
		t.Fatalf("top actors = %+v", actors)
	}
}
