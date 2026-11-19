package snapshot

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The resource filter matches the resources list only: a short name is a
// bad query, and an ARN that a change only mentions in a field value or an
// unresolved name never selects it.
func TestFeedResourceFilterMatchesResourcesOnly(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		now := fixture.clock.Now()
		ecsARN := "arn:aws:ecs:us-east-1:111122223333:service/main/api"
		owned := feedChange("owned", now.Add(-time.Hour), model.ActorPipeline, "")
		owned.Resources, owned.Fields = []string{ecsARN}, map[string]string{"service": "api"}
		mention := feedChange("mention", now.Add(-time.Hour), model.ActorPipeline, "")
		mention.Resources, mention.Fields = []string{"api"}, map[string]string{"target": ecsARN}
		fixture.pollSettled(account, region, owned, mention)
		if _, err := fixture.feed.Answer(context.Background(), FeedRequest{Resource: "api"}); !errors.Is(err, ErrBadFeedQuery) {
			t.Fatalf("short name filter = %v", err)
		}
		answer := fixture.answer(FeedRequest{Resource: ecsARN})
		if len(answer.Items) != 1 || answer.Items[0].ID != "owned" {
			t.Fatalf("ARN filter = %+v", answer.Items)
		}
	})
}

// feedFailing is a backend whose change reads fail on demand.
type feedFailing struct {
	inventoryProvider
	fail bool
}

func (provider *feedFailing) AWSChangeFeed(ctx context.Context, scope tenancy.OrgScope, query storage.AWSChangeFeedQuery) (storage.AWSChangeFeed, error) {
	if provider.fail {
		return storage.AWSChangeFeed{}, errors.New("change read failed")
	}
	return provider.inventoryProvider.AWSChangeFeed(ctx, scope, query)
}

// When the changes since the snapshot cannot be read, an earlier change may
// sit between: the before value is ambiguous, never known.
func TestBeforeValueAmbiguousWhenChangesUnreadable(t *testing.T) {
	provider := &feedFailing{inventoryProvider: storage.NewMemory().(inventoryProvider)}
	c := &clock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	store := New(provider, tenancy.DefaultOrgScope(), WithClock(c.Now))
	if _, err := store.Commit(model.Unit{Account: account, Region: region, Service: "ecs"}, Candidate{Resources: []model.Resource{ecsService("41", nil)}}); err != nil {
		t.Fatal(err)
	}
	change := scaled("s1", c.Now().Add(5*time.Minute), "43")
	provider.fail = true
	field := fieldNamed(storeFields{store: store}.Fields(context.Background(), change), "desired")
	if field.BeforeStatus != BeforeAmbiguous || field.Before != nil {
		t.Fatalf("unreadable changes = %+v", field)
	}
}

// An account with no selected regions still expects its global stream: it
// is never complete and empty.
func TestAccountWithoutRegionsIsNeverComplete(t *testing.T) {
	fixture := newFeedFixture(t, []ScopeAccount{{Account: account, Name: "auto"}})
	answer := fixture.answer(FeedRequest{})
	if answer.Complete || len(gapsOf(answer, model.GlobalRegion)) != 1 || answer.Gaps[0].Reason != string(GapChangesNotChecked) {
		t.Fatalf("account without regions = complete %v, gaps %+v", answer.Complete, answer.Gaps)
	}
}

// A pair whose reads keep failing keeps the failure's reason up to now
// while it is still attempted; a skewed coverage end alone is not a
// collector outage either.
func TestFailingPairKeepsReasonToNow(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	start := fixture.clock.Now()
	fixture.pollSettled(account, region)
	fixture.pollSettled(account, model.GlobalRegion)
	for cycle := 0; cycle < 6; cycle++ {
		fixture.clock.Advance(5 * time.Minute)
		now := fixture.clock.Now()
		fixture.poll(account, region, ChangePoll{ErrorCode: "denied",
			Gaps: []ChangeGapLabel{{From: start.Add(-15 * time.Minute), To: now, Reason: GapCloudTrailDenied, Detail: "cloudtrail:LookupEvents"}}})
		// The global stream reads, but a fast local clock capped its
		// coverage 20 minutes short of now.
		fixture.poll(account, model.GlobalRegion, ChangePoll{Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: now.Add(-20 * time.Minute)}},
			Read: []changes.Interval{{From: now.Add(-26 * time.Hour), To: now.Add(-20 * time.Minute)}}})
		fixture.clock.Advance(2 * time.Minute)
		answer := fixture.answer(FeedRequest{Window: time.Hour})
		for _, gap := range answer.Gaps {
			if gap.Reason == string(GapCollectorDown) {
				t.Fatalf("cycle %d: collector_down while polling: %+v", cycle, answer.Gaps)
			}
		}
		gaps := gapsOf(answer, region)
		if last := gaps[len(gaps)-1]; last.Reason != string(GapCloudTrailDenied) || !last.To.Equal(fixture.clock.Now()) {
			t.Fatalf("cycle %d: newest denied gap = %+v", cycle, last)
		}
	}
	// Polling stopped: only then is the tail collector_down.
	fixture.clock.Advance(2 * time.Hour)
	stopped := false
	for _, gap := range fixture.answer(FeedRequest{Window: time.Hour}).Gaps {
		stopped = stopped || gap.Reason == string(GapCollectorDown)
	}
	if !stopped {
		t.Fatal("a pair not polled for two hours is not collector_down")
	}
}

// Later pages answer the first page's window: read half an hour later,
// they neither drop the oldest items nor shift.
func TestCursorKeepsTheWindow(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		now := fixture.clock.Now()
		var found []model.Change
		for index := 0; index < 30; index++ {
			// The ten oldest sit in the first half hour of the window.
			found = append(found, feedChange(fmt.Sprintf("e%02d", index), now.Add(-24*time.Hour+time.Duration(index+1)*2*time.Minute), model.ActorPipeline, ""))
		}
		fixture.pollSettled(account, region, found...)
		first := fixture.answer(FeedRequest{Limit: 10})
		fixture.clock.Advance(30 * time.Minute)
		items, cursor := first.Items, first.NextCursor
		for cursor != nil {
			next := fixture.answer(FeedRequest{Limit: 10, Cursor: *cursor})
			if !next.Window.Since.Equal(first.Window.Since) || !next.Window.Until.Equal(first.Window.Until) {
				t.Fatalf("page window %+v, first %+v", next.Window, first.Window)
			}
			items, cursor = append(items, next.Items...), next.NextCursor
		}
		if len(items) != 30 {
			t.Fatalf("paged %d of 30 items", len(items))
		}
	})
}

// Two valid ARNs of one Auto Scaling group (two generated IDs, one name)
// match in the resource filter and in before values.
func TestFoldedARNsMatchInventoryAndFilter(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	inventoryARN := "arn:aws:autoscaling:us-east-1:111122223333:autoScalingGroup:6d5af669-eb3b-4530-894b-e3a7ae9ad1da:autoScalingGroupName/web"
	changeARN := "arn:aws:autoscaling:us-east-1:111122223333:autoScalingGroup:11111111-2222-3333-4444-555555555555:autoScalingGroupName/web"
	group := model.Resource{ARN: inventoryARN, Service: "auto-scaling", Type: "auto-scaling-group", Account: account, Region: region, Name: "web",
		Attributes: map[string]string{model.AttrDesired: "3"}}
	if _, err := fixture.store.Commit(model.Unit{Account: account, Region: region, Service: "auto-scaling"}, Candidate{Resources: []model.Resource{group}}); err != nil {
		t.Fatal(err)
	}
	scale := feedChange("scale", fixture.clock.Now().Add(5*time.Minute), model.ActorPipeline, "")
	scale.Resources, scale.Fields = []string{changeARN}, map[string]string{"desired": "5"}
	fixture.clock.Advance(30 * time.Minute)
	fixture.pollSettled(account, region, scale)
	field := fieldNamed(storeFields{store: fixture.store}.Fields(context.Background(), scale), "desired")
	if field.BeforeStatus != BeforeKnown || field.Before == nil || *field.Before != "3" {
		t.Fatalf("folded before value = %+v", field)
	}
	if answer := fixture.answer(FeedRequest{Resource: inventoryARN}); len(answer.Items) != 1 || answer.Items[0].ID != "scale" {
		t.Fatalf("folded filter = %+v", answer.Items)
	}
}

// Unit history never reaches past max_age, where history rows are pruned.
func TestUnitHistoryClampedToMaxAge(t *testing.T) {
	openBackend = func(*testing.T) inventoryProvider { return storage.NewMemory().(inventoryProvider) }
	defer func() { openBackend = nil }()
	store, _, c := newStore(t, WithOptions(func() Options { return Options{MaxAge: 2 * time.Hour} }))
	if _, err := store.Commit(model.Unit{Account: account, Region: region, Service: "ec2"}, candidate("i-1")); err != nil {
		t.Fatal(err)
	}
	c.Advance(5 * time.Hour)
	history, err := store.UnitHistory(context.Background(), Query{Account: account})
	if err != nil || len(history) != 1 || !history[0].Oldest.Equal(c.Now().Add(-2*time.Hour)) {
		t.Fatalf("history = %+v, %v", history, err)
	}
}

// Compare covers the configured scope: a removed region's leftover units
// are not compared, and a configured region with no history is named.
func TestCompareUsesConfiguredScope(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region, "eu-west-1"))
	for _, unit := range []model.Unit{{Account: account, Region: region, Service: "ec2"}, {Account: account, Region: "ap-south-1", Service: "ec2"}} {
		resource := instance("i-1")
		resource.Region, resource.ARN = unit.Region, model.ARN("aws", "ec2", unit.Region, account, "instance/i-1")
		if _, err := fixture.store.Commit(unit, Candidate{Resources: []model.Resource{resource}}); err != nil {
			t.Fatal(err)
		}
	}
	fixture.clock.Advance(time.Hour)
	answer, err := fixture.feed.Compare(context.Background(), CompareRequest{From: fixture.clock.Now().Add(-30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, partial := range answer.Partial {
		named[partial.Region] = true
	}
	if !named["eu-west-1"] || named["ap-south-1"] || answer.Complete {
		t.Fatalf("scoped compare = %+v", answer)
	}
}

// The global stream and the us-east-1 pair read one CloudTrail, so they
// share one poller lock.
func TestGlobalStreamSharesUsEast1Lock(t *testing.T) {
	store := New(storage.NewMemory(), tenancy.DefaultOrgScope())
	if store.changeLockKey(account, model.GlobalRegion) != store.changeLockKey(account, "us-east-1") ||
		store.changeLockKey(account, "eu-west-1") == store.changeLockKey(account, "us-east-1") {
		t.Fatal("lock keys do not share the us-east-1 CloudTrail")
	}
}
