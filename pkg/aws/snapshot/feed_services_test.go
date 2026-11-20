package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// serviceChange is a successful change in one service by one principal.
func serviceChange(id string, at time.Time, service, principal string) model.Change {
	change := feedChange(id, at, model.ActorPipeline, principal)
	change.Service, change.EventSource = service, service+".amazonaws.com"
	change.Summary = service + " change " + id
	return change
}

// mixedServices records rds ×12, iam ×9, ecs ×20 and config ×15 changes,
// each service by its own principal, spread over the last 20 hours.
func mixedServices(fixture *feedFixture) {
	now := fixture.clock.Now()
	var found []model.Change
	for service, count := range map[string]int{"rds": 12, "iam": 9, "ecs": 20, "config": 15} {
		for index := 0; index < count; index++ {
			at := now.Add(-time.Duration(index*55+len(service)) * time.Minute)
			found = append(found, serviceChange(fmt.Sprintf("%s-%02d", service, index), at, service, "arn:aws:iam::111122223333:role/"+service+"-deployer"))
		}
	}
	fixture.pollSettled(account, region, found...)
	fixture.pollSettled(account, model.GlobalRegion)
}

func bucketTotal(answer ChangesAnswer) int64 {
	var total int64
	for _, bucket := range answer.Buckets {
		total += bucket.Pipeline + bucket.Person + bucket.AWSService + bucket.Unknown
	}
	return total
}

// A two-service filter narrows the totals, the grid, the top actors and
// every page; later pages keep the first page's filter from the cursor, so
// paging has no skip and no duplicate.
func TestServiceFilterNarrowsTotalsBucketsAndPaging(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		mixedServices(fixture)
		first := fixture.answer(FeedRequest{Services: []string{"rds", "IAM"}, ServiceFacet: true, Limit: 5})
		if first.Summary.Total != 21 || bucketTotal(first) != 21 {
			t.Fatalf("filtered total = %d, buckets = %d", first.Summary.Total, bucketTotal(first))
		}
		for _, actor := range first.Summary.TopActors {
			if actor.Actor != "rds-deployer" && actor.Actor != "iam-deployer" {
				t.Fatalf("top actor outside the filter: %+v", first.Summary.TopActors)
			}
		}
		seen := map[string]bool{}
		items, cursor, pages := first.Items, first.NextCursor, 1
		for cursor != nil {
			// The cursor alone carries the filter: the next request names
			// no service.
			next := fixture.answer(FeedRequest{Cursor: *cursor, Limit: 5, ServiceFacet: true})
			if next.Summary.Total != 21 {
				t.Fatalf("page %d total = %d", pages, next.Summary.Total)
			}
			items, cursor, pages = append(items, next.Items...), next.NextCursor, pages+1
		}
		for _, item := range items {
			if seen[item.ID] {
				t.Fatalf("item %s repeated", item.ID)
			}
			seen[item.ID] = true
			if item.Service != "rds" && item.Service != "iam" {
				t.Fatalf("item %s in service %s", item.ID, item.Service)
			}
		}
		if len(items) != 21 || pages != 5 {
			t.Fatalf("paged %d items over %d pages", len(items), pages)
		}
		all := fixture.answer(FeedRequest{Limit: 200})
		if all.Summary.Total != 56 || len(all.Items) != 56 {
			t.Fatalf("unfiltered = %d total, %d items", all.Summary.Total, len(all.Items))
		}
	})
}

// The service list counts every service in the window and scope, without
// the service filter, most first, with display names.
func TestServiceFacetCountsIgnoreFilter(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		mixedServices(fixture)
		want := []FeedServiceCount{{"ecs", "ECS", 20}, {"config", "Config", 15}, {"rds", "RDS", 12}, {"iam", "IAM", 9}}
		for _, filter := range [][]string{nil, {"rds"}, {"rds", "iam"}, {"athena"}} {
			answer := fixture.answer(FeedRequest{Services: filter, ServiceFacet: true})
			if !reflect.DeepEqual(answer.Services, want) {
				t.Fatalf("filter %v services = %+v", filter, answer.Services)
			}
		}
		if empty := fixture.answer(FeedRequest{Services: []string{"athena"}, ServiceFacet: true}); empty.Summary.Total != 0 || len(empty.Items) != 0 {
			t.Fatalf("filter with no changes = %+v", empty.Summary)
		}
		// The agent's view asks for no facet: its answer is unchanged.
		data, err := json.Marshal(fixture.answer(FeedRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var view map[string]any
		_ = json.Unmarshal(data, &view)
		if _, ok := view["services"]; ok {
			t.Fatalf("services present without the facet")
		}
		if _, ok := view["suspect"].(map[string]any)["in_filter"]; ok {
			t.Fatalf("in_filter present without the facet")
		}
	})
}

// Gaps, accounts, regions and sync are per account and Region: the service
// filter never hides missing data.
func TestServiceFilterLeavesGapsAlone(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		now := fixture.clock.Now()
		fixture.poll(account, region, ChangePoll{ErrorCode: "denied",
			Gaps: []ChangeGapLabel{{From: now.Add(-24 * time.Hour), To: now.Add(-22 * time.Hour), Reason: GapCloudTrailDenied, Detail: "cloudtrail:LookupEvents"}}})
		fixture.poll(account, region, ChangePoll{Found: []model.Change{serviceChange("ecs-1", now.Add(-2*time.Hour), "ecs", "arn:aws:iam::111122223333:role/deploy")},
			Covered: []changes.Interval{{From: now.Add(-22 * time.Hour), To: now.Add(-15 * time.Minute)}}, Read: []changes.Interval{{From: now.Add(-22 * time.Hour), To: now}}})
		fixture.pollSettled(account, model.GlobalRegion)
		all := fixture.answer(FeedRequest{ServiceFacet: true})
		filtered := fixture.answer(FeedRequest{Services: []string{"rds", "iam"}, ServiceFacet: true})
		if len(gapsOf(all, region)) == 0 {
			t.Fatalf("no gap to compare: %+v", all.Gaps)
		}
		if !reflect.DeepEqual(all.Gaps, filtered.Gaps) || !reflect.DeepEqual(all.Accounts, filtered.Accounts) ||
			!reflect.DeepEqual(all.Regions, filtered.Regions) || !reflect.DeepEqual(all.Sync, filtered.Sync) {
			t.Fatalf("filter changed coverage:\n all %+v\n filtered %+v", all.Gaps, filtered.Gaps)
		}
		if filtered.Summary.Total != 0 || all.Summary.Total != 1 || filtered.Complete {
			t.Fatalf("totals = %d / %d, filtered complete %v", all.Summary.Total, filtered.Summary.Total, filtered.Complete)
		}
	})
}

// newestSuspect fires one alarm and names the newest change it is given,
// so a suspect outside the filter proves the engine saw every service.
type newestSuspect struct{}

func (newestSuspect) AlarmLine(_ context.Context, scope AlarmScope) ([]FeedAlarm, *time.Time, error) {
	start := scope.Until.Add(-time.Minute)
	return []FeedAlarm{{ID: "alarm-1", Name: "db-errors", Account: account, Region: region, Start: &start, StartKnown: true, ResourceARNs: []string{}}}, nil, nil
}

func (newestSuspect) Suspect(_ context.Context, request SuspectRequest) FeedSuspect {
	if len(request.Changes) == 0 {
		return FeedSuspect{Status: SuspectNone, AlarmID: "alarm-1"}
	}
	newest := request.Changes[0]
	for _, change := range request.Changes {
		if change.Time.After(newest.Time) {
			newest = change
		}
	}
	return FeedSuspect{Status: SuspectFound, AlarmID: "alarm-1", ChangeID: newest.ID,
		Candidates: []FeedSuspectCandidate{{ChangeID: newest.ID, Confidence: "high"}}}
}

// The likely cause is chosen over every service: one outside the filter is
// still named and marked in_filter false.
func TestSuspectOutsideFilterIsMarked(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region), WithSuspectEngine(newestSuspect{}))
		now := fixture.clock.Now()
		fixture.pollSettled(account, region,
			serviceChange("rds-1", now.Add(-3*time.Hour), "rds", "arn:aws:iam::111122223333:role/dba"),
			serviceChange("config-1", now.Add(-20*time.Minute), "config", "arn:aws:iam::111122223333:role/config-recorder"))
		fixture.pollSettled(account, model.GlobalRegion)
		outside := fixture.answer(FeedRequest{Services: []string{"rds", "iam"}, ServiceFacet: true})
		if outside.Suspect.Status != SuspectFound || outside.Suspect.Change == nil || outside.Suspect.Change.ID != "config-1" ||
			outside.Suspect.InFilter == nil || *outside.Suspect.InFilter {
			t.Fatalf("suspect outside the filter = %+v", outside.Suspect)
		}
		if outside.Summary.Total != 1 || len(outside.Items) != 1 || outside.Items[0].ID != "rds-1" {
			t.Fatalf("filtered list = %d %+v", outside.Summary.Total, outside.Items)
		}
		inside := fixture.answer(FeedRequest{Services: []string{"config"}, ServiceFacet: true})
		if inside.Suspect.Change == nil || inside.Suspect.Change.ID != "config-1" || inside.Suspect.InFilter == nil || !*inside.Suspect.InFilter {
			t.Fatalf("suspect inside the filter = %+v", inside.Suspect)
		}
		everything := fixture.answer(FeedRequest{ServiceFacet: true})
		if everything.Suspect.InFilter == nil || !*everything.Suspect.InFilter {
			t.Fatalf("suspect without a filter = %+v", everything.Suspect)
		}
		if testing.Verbose() {
			data, _ := json.Marshal(struct {
				Services []FeedServiceCount `json:"services"`
				Suspect  FeedSuspect        `json:"suspect"`
			}{outside.Services, outside.Suspect})
			t.Logf("changes response parts: %s", data)
		}
	})
}

// A malformed service name or an oversized filter is a bad query.
func TestServiceFilterRejectsMalformedNames(t *testing.T) {
	for _, text := range []string{"rds,<script>", "rds,a b", "x,../etc"} {
		if _, err := ParseServices(text); err != ErrBadFeedQuery {
			t.Fatalf("ParseServices(%q) = %v", text, err)
		}
	}
	got, err := ParseServices(" RDS , iam,rds,, made-up-service ")
	if err != nil || !reflect.DeepEqual(got, []string{"rds", "iam", "made-up-service"}) {
		t.Fatalf("ParseServices = %v %v", got, err)
	}
	long := make([]string, MaxServiceFilter+1)
	for index := range long {
		long[index] = fmt.Sprintf("s%d", index)
	}
	if _, err := NormalizeServices(long); err != ErrBadFeedQuery {
		t.Fatalf("oversized filter = %v", err)
	}
	fixture := newFeedFixture(t, scopeOf(region))
	if _, err := fixture.feed.Answer(context.Background(), FeedRequest{Services: []string{"bad name"}}); err != ErrBadFeedQuery {
		t.Fatalf("answer with a bad name = %v", err)
	}
}

// Compare with a service filter lists only that service's added resources
// and changes between.
func TestCompareWithServiceFilter(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		ec2 := model.Unit{Account: account, Region: region, Service: "ec2"}
		rds := model.Unit{Account: account, Region: region, Service: "rds"}
		database := func(name string) model.Resource {
			return model.Resource{ARN: "arn:aws:rds:us-east-1:111122223333:db:" + name, Service: "rds", Type: "db-instance", Account: account, Region: region, Name: name, State: "available"}
		}
		if _, err := fixture.store.Commit(ec2, candidate("i-1")); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.Commit(rds, Candidate{Resources: []model.Resource{database("orders")}}); err != nil {
			t.Fatal(err)
		}
		from := fixture.clock.Now()
		fixture.clock.Advance(3 * time.Hour)
		if _, err := fixture.store.Commit(ec2, candidate("i-1", "i-2")); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.Commit(rds, Candidate{Resources: []model.Resource{database("orders"), database("billing")}}); err != nil {
			t.Fatal(err)
		}
		var found []model.Change
		for index := 0; index < 5; index++ {
			found = append(found, serviceChange(fmt.Sprintf("ec2-%d", index), from.Add(time.Duration(index+1)*time.Minute), "ec2", "arn:aws:iam::111122223333:role/ops"))
		}
		for index := 0; index < 3; index++ {
			found = append(found, serviceChange(fmt.Sprintf("rds-%d", index), from.Add(time.Duration(index+10)*time.Minute), "rds", "arn:aws:iam::111122223333:role/dba"))
		}
		fixture.poll(account, region, ChangePoll{Found: found, Covered: []changes.Interval{{From: from, To: fixture.clock.Now()}}})
		all, err := fixture.feed.Compare(context.Background(), CompareRequest{From: from.Add(time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		if len(all.Added) != 2 || all.ChangesBetweenTotal != 8 {
			t.Fatalf("unfiltered compare = %d added, %d between", len(all.Added), all.ChangesBetweenTotal)
		}
		filtered, err := fixture.feed.Compare(context.Background(), CompareRequest{From: from.Add(time.Second), Services: []string{"rds", "iam"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(filtered.Added) != 1 || filtered.Added[0].Name != "billing" || filtered.ChangesBetweenTotal != 3 || len(filtered.ChangesBetween) != 3 {
			t.Fatalf("filtered compare = %+v, %d between", filtered.Added, filtered.ChangesBetweenTotal)
		}
		for _, item := range filtered.ChangesBetween {
			if item.Service != "rds" {
				t.Fatalf("change between outside the filter: %+v", item)
			}
		}
	})
}

// Preferences default to RDS and IAM, save per user, read back, keep
// unknown names and store an empty list as "every service".
func TestChangePreferencesPerUser(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store := NewChangePreferenceStore(openBackend(t))
		// Postgres keeps blobs between runs: each run signs in fresh users.
		alice, bob := fmt.Sprintf("alice-%d@example.com", time.Now().UnixNano()), fmt.Sprintf("bob-%d@example.com", time.Now().UnixNano())
		got, err := store.Get(alice)
		if err != nil || !reflect.DeepEqual(got.Services, []string{"rds", "iam"}) {
			t.Fatalf("default = %+v %v", got, err)
		}
		saved, err := store.Save(alice, ChangePreferences{Services: []string{"EC2", "lambda", "not-a-service-yet"}})
		if err != nil || !reflect.DeepEqual(saved.Services, []string{"ec2", "lambda", "not-a-service-yet"}) {
			t.Fatalf("save = %+v %v", saved, err)
		}
		if got, _ := store.Get(alice); !reflect.DeepEqual(got.Services, saved.Services) {
			t.Fatalf("read back = %+v", got)
		}
		if got, _ := store.Get(bob); !reflect.DeepEqual(got.Services, []string{"rds", "iam"}) {
			t.Fatalf("another user = %+v", got)
		}
		if _, err := store.Save(bob, ChangePreferences{Services: []string{}}); err != nil {
			t.Fatal(err)
		}
		got, err = store.Get(bob)
		if err != nil || got.Services == nil || len(got.Services) != 0 {
			t.Fatalf("empty list = %#v %v", got.Services, err)
		}
		if data, _ := json.Marshal(got); string(data) != `{"services":[]}` {
			t.Fatalf("empty list JSON = %s", data)
		}
		if _, err := store.Save("", ChangePreferences{}); err != ErrNoUser {
			t.Fatalf("save without a user = %v", err)
		}
		if _, err := store.Save(alice, ChangePreferences{Services: []string{"no spaces"}}); err != ErrBadFeedQuery {
			t.Fatalf("bad name = %v", err)
		}
	})
}
