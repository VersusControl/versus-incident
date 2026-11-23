package snapshot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const serviceARN = "arn:aws:ecs:us-east-1:111122223333:service/main/web"

func ecsService(desired string, tags map[string]string) model.Resource {
	return model.Resource{ARN: serviceARN, Service: "ecs", Type: "service", Account: account, Region: region, Name: "web", State: "active",
		Tags: tags, Attributes: map[string]string{model.AttrDesired: desired}}
}

func scaled(id string, at time.Time, desired string) model.Change {
	change := feedChange(id, at, model.ActorPipeline, "arn:aws:iam::111122223333:role/deploy")
	change.Resources, change.Fields = []string{serviceARN}, map[string]string{"desired": desired, "service": "web"}
	return change
}

func fieldNamed(fields []FeedField, name string) FeedField {
	for _, field := range fields {
		if field.Field == name {
			return field
		}
	}
	return FeedField{}
}

// Two changes between two snapshots chain: 41 → 42, then 42 → 43. A
// snapshot that already holds the new value makes the before value
// ambiguous.
func TestBeforeValueChainsBetweenSnapshots(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		ecs := model.Unit{Account: account, Region: region, Service: "ecs"}
		if _, err := fixture.store.Commit(ecs, Candidate{Resources: []model.Resource{ecsService("41", nil)}}); err != nil {
			t.Fatal(err)
		}
		snapshotAt := fixture.clock.Now()
		first, second := scaled("s1", snapshotAt.Add(2*time.Minute), "42"), scaled("s2", snapshotAt.Add(4*time.Minute), "43")
		fixture.clock.Advance(20 * time.Minute)
		fixture.pollSettled(account, region, first, second)
		resolver := storeFields{store: fixture.store}
		for _, tc := range []struct {
			change model.Change
			before string
		}{{first, "41"}, {second, "42"}} {
			field := fieldNamed(resolver.Fields(context.Background(), tc.change), "desired")
			if field.BeforeStatus != BeforeKnown || field.Before == nil || *field.Before != tc.before || *field.After != tc.change.Fields["desired"] {
				t.Fatalf("%s desired = %+v", tc.change.ID, field)
			}
		}
		if _, err := fixture.store.Commit(ecs, Candidate{Resources: []model.Resource{ecsService("44", nil)}}); err != nil {
			t.Fatal(err)
		}
		late := scaled("s3", fixture.clock.Now().Add(time.Minute), "44")
		if field := fieldNamed(resolver.Fields(context.Background(), late), "desired"); field.BeforeStatus != BeforeAmbiguous {
			t.Fatalf("snapshot already new = %+v", field)
		}
	})
}

// A secret-shaped field and a tag outside the value allowlist never show a
// value, in the change list or in compare; the fact that they changed
// stays. Values are redacted at write, so the compare check matters when
// the allowlist narrows after values were stored.
func TestHiddenValuesNeverShown(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	allowed := []string{"team", "owner"}
	fixture.store.allowlist = func() []string { return allowed }
	ecs := model.Unit{Account: account, Region: region, Service: "ecs"}
	if _, err := fixture.store.Commit(ecs, Candidate{Resources: []model.Resource{ecsService("2", map[string]string{"team": "core", "owner": "alice-old"})}}); err != nil {
		t.Fatal(err)
	}
	from := fixture.clock.Now()
	fixture.clock.Advance(time.Hour)
	if _, err := fixture.store.Commit(ecs, Candidate{Resources: []model.Resource{ecsService("2", map[string]string{"team": "edge", "owner": "bob-new"})}}); err != nil {
		t.Fatal(err)
	}
	allowed = []string{"team"}
	secret := scaled("secret", fixture.clock.Now().Add(-time.Minute), "2")
	secret.Fields["db_password"] = "hunter2"
	fields := storeFields{store: fixture.store}.Fields(context.Background(), secret)
	if field := fieldNamed(fields, "db_password"); field.BeforeStatus != BeforeHidden || field.After != nil || field.Before != nil {
		t.Fatalf("secret field = %+v", field)
	}
	answer, err := fixture.feed.Compare(context.Background(), CompareRequest{From: from.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Changed) != 1 {
		t.Fatalf("compare = %+v", answer)
	}
	var owner, team CompareField
	for _, field := range answer.Changed[0].Fields {
		switch field.Field {
		case "tag:owner":
			owner = field
		case "tag:team":
			team = field
		}
	}
	if !owner.Hidden || owner.Before != nil || owner.After != nil || team.Hidden || team.After == nil || *team.After != "edge" {
		t.Fatalf("tag fields = owner %+v team %+v", owner, team)
	}
	data, _ := json.Marshal(struct {
		Fields  []FeedField
		Compare CompareAnswer
	}{fields, answer})
	for _, value := range []string{"hunter2", "bob-new", "alice-old"} {
		if strings.Contains(string(data), value) {
			t.Fatalf("hidden value %q shown: %s", value, data)
		}
	}
}

// A resource whose unit was never readable for a missing permission shows
// the new value only and names the permission.
func TestMissingPermissionDegradesToNewOnly(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	ecs := model.Unit{Account: account, Region: region, Service: "ecs"}
	if err := fixture.store.RecordFailure(ecs, "denied", model.Gap{Account: account, Region: region, Service: "ecs", Reason: model.GapDenied, Operation: "DescribeServices"}); err != nil {
		t.Fatal(err)
	}
	field := fieldNamed(storeFields{store: fixture.store}.Fields(context.Background(), scaled("d1", fixture.clock.Now(), "3")), "desired")
	if field.BeforeStatus != BeforeNewOnly || field.After == nil || field.MissingPermission == nil || *field.MissingPermission != "ecs:DescribeServices" {
		t.Fatalf("denied unit field = %+v", field)
	}
}

// Compare checks each unit's own oldest kept generation: a busy unit whose
// cap leaves less than the window is named in partial, never diffed
// against nothing; a From before every unit's history is unavailable with
// the earliest time that can be picked.
func TestCompareChecksEachUnitsOldestGeneration(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		fixture.store.options = func() Options {
			return Options{GenerationsFor: func(service string) int {
				if service == "auto-scaling" {
					return 5
				}
				return 0
			}}
		}
		start := fixture.clock.Now()
		ec2 := model.Unit{Account: account, Region: region, Service: "ec2"}
		if _, err := fixture.store.Commit(ec2, candidate("i-1")); err != nil {
			t.Fatal(err)
		}
		asg := model.Unit{Account: account, Region: region, Service: "auto-scaling"}
		for step := 0; step < 60; step++ {
			fixture.clock.Advance(24 * time.Minute)
			group := model.Resource{ARN: model.ARN("aws", "autoscaling", region, account, "autoScalingGroup:x:autoScalingGroupName/web"), Service: "auto-scaling",
				Type: "auto-scaling-group", Account: account, Region: region, Name: "web", Attributes: map[string]string{model.AttrDesired: string(rune('a' + step%26))}}
			if _, err := fixture.store.Commit(asg, Candidate{Resources: []model.Resource{group}}); err != nil {
				t.Fatal(err)
			}
		}
		fixture.store.SweepRetention(context.Background())
		answer, err := fixture.feed.Compare(context.Background(), CompareRequest{From: fixture.clock.Now().Add(-23 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		// The busy group is partial; so is the configured global stream,
		// which has no inventory history at all here.
		busy := false
		for _, partial := range answer.Partial {
			busy = busy || partial.Service == "auto-scaling" && partial.HistoryStarts != nil
		}
		if answer.Status != CompareOK || !busy || answer.Complete {
			t.Fatalf("busy group = %+v", answer)
		}
		for _, changed := range answer.Changed {
			if strings.Contains(changed.ResourceARN, "autoScalingGroup") {
				t.Fatalf("busy group diffed: %+v", changed)
			}
		}
		if answer.Snapshots.Oldest == nil || !answer.Snapshots.Oldest.Equal(start) {
			t.Fatalf("snapshots = %+v", answer.Snapshots)
		}

		fresh := newFeedFixture(t, scopeOf(region))
		if _, err := fresh.store.Commit(ec2, candidate("i-1")); err != nil {
			t.Fatal(err)
		}
		oldest := fresh.clock.Now()
		fresh.clock.Advance(2 * time.Hour)
		before, err := fresh.feed.Compare(context.Background(), CompareRequest{From: oldest.Add(-time.Hour)})
		if err != nil || before.Status != CompareUnavailable || before.HistoryStarts == nil || !before.HistoryStarts.Equal(oldest) {
			t.Fatalf("before history = %+v, %v", before, err)
		}
		if _, err := fresh.feed.Compare(context.Background(), CompareRequest{From: oldest.Add(-30 * time.Hour)}); err != ErrWindowLocked {
			t.Fatalf("beyond the tier = %v", err)
		}
	})
}

// Compare carries up to 20 changes between the two times and their total.
func TestCompareListsChangesBetween(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	ec2 := model.Unit{Account: account, Region: region, Service: "ec2"}
	if _, err := fixture.store.Commit(ec2, candidate("i-1")); err != nil {
		t.Fatal(err)
	}
	from := fixture.clock.Now()
	fixture.clock.Advance(3 * time.Hour)
	var found []model.Change
	for index := 0; index < 30; index++ {
		found = append(found, feedChange("c"+string(rune('a'+index)), from.Add(time.Duration(index+1)*time.Minute), model.ActorPipeline, ""))
	}
	fixture.poll(account, region, ChangePoll{Found: found, Covered: []changes.Interval{{From: from, To: fixture.clock.Now()}}})
	answer, err := fixture.feed.Compare(context.Background(), CompareRequest{From: from.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.ChangesBetween) != 20 || answer.ChangesBetweenTotal != 30 {
		t.Fatalf("between = %d of %d", len(answer.ChangesBetween), answer.ChangesBetweenTotal)
	}
}

// Each collection kind keeps its own generation cap: health units sized
// for a day of 2-minute flapping keep every generation while inventory
// units are capped lower.
func TestGenerationCapPerKind(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t, WithOptions(func() Options {
			return Options{MaxGenerations: 10, GenerationsFor: func(service string) int {
				if service == "cloudwatch" {
					return 1440
				}
				return 0
			}}
		}))
		health := model.Unit{Account: account, Region: region, Service: "cloudwatch"}
		inventory := model.Unit{Account: account, Region: region, Service: "ec2"}
		for step := 0; step < 40; step++ {
			c.Advance(2 * time.Minute)
			for _, unit := range []model.Unit{health, inventory} {
				resource := instance("i-" + string(rune('a'+step%26)) + string(rune('a'+step/26)))
				resource.Service = unit.Service
				if _, err := store.Commit(unit, Candidate{Resources: []model.Resource{resource}}); err != nil {
					t.Fatal(err)
				}
			}
		}
		store.SweepRetention(context.Background())
		history, err := store.UnitHistory(context.Background(), Query{Account: account})
		if err != nil {
			t.Fatal(err)
		}
		kept := map[string]int{}
		for _, unit := range history {
			kept[unit.Unit.Service] = unit.Generations
		}
		if kept["cloudwatch"] != 40 || kept["ec2"] != 10 {
			t.Fatalf("kept generations = %v", kept)
		}
	})
}

// After a key is added, the default organization's snapshots stay part of
// the history: a compare from before the upgrade to after it reads each
// unit's newest generation at each time from either organization, so the
// same resources read under both compare as unchanged, and a real change
// across the boundary is still reported.
func TestCompareAcrossLicenseUpgradeReadsBothOrganizations(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		start := fixture.clock.Now()
		ec2 := model.Unit{Account: account, Region: region, Service: "ec2"}
		ecs := model.Unit{Account: account, Region: region, Service: "ecs"}
		for _, commit := range []struct {
			unit      model.Unit
			candidate Candidate
		}{{ec2, candidate("i-1", "i-2")}, {ecs, Candidate{Resources: []model.Resource{ecsService("2", nil)}}}} {
			if _, err := fixture.store.Commit(commit.unit, commit.candidate); err != nil {
				t.Fatal(err)
			}
		}
		fixture.clock.Advance(30 * time.Minute)
		beforeUpgrade := fixture.clock.Now()
		fixture.clock.Advance(30 * time.Minute)

		upgraded := tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
		licensed := New(fixture.provider, upgraded, WithClock(fixture.clock.Now))
		if _, err := licensed.Commit(ec2, candidate("i-1", "i-2")); err != nil {
			t.Fatal(err)
		}
		if _, err := licensed.Commit(ecs, Candidate{Resources: []model.Resource{ecsService("2", nil)}}); err != nil {
			t.Fatal(err)
		}
		fixture.clock.Advance(time.Hour)
		afterUpgrade := fixture.clock.Now()
		fixture.clock.Advance(30 * time.Minute)

		week := 7 * 24 * time.Hour
		feed := NewChangeFeed(licensed, WithFeedClock(fixture.clock.Now), WithScope(func(context.Context) ([]ScopeAccount, bool, error) { return fixture.scope, true, nil }),
			WithChangesLimits(func() ChangesLimits { return ChangesLimits{MaxWindow: week, MaxCompareAge: week} }))
		compare := func(request CompareRequest) CompareAnswer {
			t.Helper()
			answer, err := feed.Compare(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}
		for name, request := range map[string]CompareRequest{
			"across":           {From: beforeUpgrade},
			"across to a time": {From: beforeUpgrade, To: afterUpgrade},
			"before only":      {From: start.Add(time.Minute), To: beforeUpgrade},
			"after only":       {From: afterUpgrade},
		} {
			answer := compare(request)
			if answer.Status != CompareOK || len(answer.Added) != 0 || len(answer.Removed) != 0 || len(answer.Changed) != 0 {
				t.Fatalf("%s: added %v removed %v changed %v status %s", name, answer.Added, answer.Removed, answer.Changed, answer.Status)
			}
			for _, partial := range answer.Partial {
				if partial.Region == region && partial.Service != "" {
					t.Fatalf("%s: unit with history named partial: %+v", name, partial)
				}
			}
		}
		across := compare(CompareRequest{From: beforeUpgrade})
		if across.HistoryStarts == nil || !across.HistoryStarts.Equal(start) || across.Snapshots.Oldest == nil || !across.Snapshots.Oldest.Equal(start) || across.Snapshots.Count != 2 {
			t.Fatalf("history starts %v snapshots %+v", across.HistoryStarts, across.Snapshots)
		}

		if _, err := licensed.Commit(ec2, candidate("i-1", "i-3")); err != nil {
			t.Fatal(err)
		}
		if _, err := licensed.Commit(ecs, Candidate{Resources: []model.Resource{ecsService("3", nil)}}); err != nil {
			t.Fatal(err)
		}
		fixture.clock.Advance(time.Minute)
		changed := compare(CompareRequest{From: beforeUpgrade})
		if len(changed.Added) != 1 || changed.Added[0].Name != "i-3" || len(changed.Removed) != 1 || changed.Removed[0].Name != "i-2" ||
			len(changed.Changed) != 1 || changed.Changed[0].ResourceARN != serviceARN {
			t.Fatalf("real change across the upgrade: added %v removed %v changed %v", changed.Added, changed.Removed, changed.Changed)
		}
	})
}

var _ = storage.AWSUnitHistory{}

// TestSyncAgreesWithInventoryWhileALicensedUnitIsPending: after a key is
// added, a unit the licensed organization has not read yet is answered by
// the default organization in both the sync list and the inventory, so
// Settings does not say "not checked" beside resources the page lists.
func TestSyncAgreesWithInventoryWhileALicensedUnitIsPending(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		ec2 := model.Unit{Account: account, Region: region, Service: "ec2"}
		if _, err := fixture.store.Commit(ec2, candidate("i-1", "i-2")); err != nil {
			t.Fatal(err)
		}
		fixture.clock.Advance(time.Hour)
		upgraded := tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
		inventory, ok := storage.AWSInventory(fixture.provider)
		if !ok {
			t.Fatal("no inventory store")
		}
		if err := inventory.RegisterAWSPending(context.Background(), upgraded, []storage.AWSUnit{{Account: account, Region: region, Service: "ec2"}}, nil); err != nil {
			t.Fatal(err)
		}
		licensed := New(fixture.provider, upgraded, WithClock(fixture.clock.Now))
		sync, err := licensed.Sync(context.Background(), Query{Account: account, Service: "ec2"})
		if err != nil {
			t.Fatal(err)
		}
		resources, err := licensed.Resources(context.Background(), Query{Account: account, Service: "ec2"})
		if err != nil {
			t.Fatal(err)
		}
		if len(sync) != 1 || !sync[0].HasData || sync[0].Sync.State != model.SyncOK {
			t.Fatalf("sync = %+v", sync)
		}
		if len(resources.Units) != 1 || resources.Units[0].Sync.State != sync[0].Sync.State || len(resources.Resources) != 2 {
			t.Fatalf("inventory units %+v resources %d, sync %+v", resources.Units, len(resources.Resources), sync)
		}
	})
}

// A bucket policy entry is stored beside its bucket, not as a resource:
// listings, unit counts and Compare leave it out; only a topology reader
// that asks for policy entries sees it.
func TestPolicyEntriesAreNotResources(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	policyUnit := model.Unit{Account: account, Region: model.GlobalRegion, Service: model.PolicyEntryService}
	if _, err := fixture.store.Commit(model.Unit{Account: account, Region: region, Service: "ec2"}, candidate("i-1")); err != nil {
		t.Fatal(err)
	}
	from := fixture.clock.Now()
	fixture.clock.Advance(3 * time.Hour)
	entry := model.Resource{ARN: "arn:aws:s3:::exports#policy", Service: model.PolicyEntryService, Type: model.ResourcePolicyType,
		Account: account, Region: region, Name: "exports", Attributes: map[string]string{model.AttrSubject: "arn:aws:s3:::exports"},
		Relations: []model.Relation{{Type: model.RelationMayAccess, Kind: model.RelationKindMayAccess, Target: "123400000004", Access: model.AccessRead}}}
	if _, err := fixture.store.Commit(policyUnit, Candidate{Resources: []model.Resource{entry}}); err != nil {
		t.Fatal(err)
	}
	listed, err := fixture.store.Resources(context.Background(), Query{Account: account})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range listed.Resources {
		if resource.Type == model.ResourcePolicyType {
			t.Fatalf("listing carries %s", resource.ARN)
		}
	}
	for _, unit := range listed.Units {
		if unit.Unit.Service == model.PolicyEntryService && unit.Count != 0 {
			t.Fatalf("policy unit count = %d", unit.Count)
		}
	}
	withEntries, err := fixture.store.Resources(context.Background(), Query{Account: account, PolicyEntries: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, resource := range withEntries.Resources {
		found = found || resource.ARN == entry.ARN
	}
	if !found {
		t.Fatal("a topology read does not see the policy entry")
	}
	answer, err := fixture.feed.Compare(context.Background(), CompareRequest{From: from.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range append(answer.Added, answer.Removed...) {
		if strings.Contains(item.ResourceARN, "#policy") {
			t.Fatalf("compare lists the policy entry: %+v", item)
		}
	}
	// The policy unit's history starts after From, which would make any
	// other unit partial; it is not named.
	for _, partial := range answer.Partial {
		if partial.Service == model.PolicyEntryService {
			t.Fatalf("compare names the policy unit as partial: %+v", partial)
		}
	}
}

// The cluster markers come from tag values: a change in them is listed,
// but hidden like a tag, whatever the allowlist.
func TestCompareHidesClusterMarkers(t *testing.T) {
	old := model.Resource{ARN: "arn:aws:ec2:us-east-1:111122223333:security-group/sg-1",
		Attributes: map[string]string{model.AttrClusterTags: "prod=owned", model.AttrID: "sg-1"}}
	current := old
	current.Attributes = map[string]string{model.AttrClusterTags: "prod=shared", model.AttrID: "sg-1"}
	fields := diffResource(old, current, map[string]bool{"kubernetes.io/cluster/prod": true})
	if len(fields) != 1 || fields[0].Field != model.AttrClusterTags || !fields[0].Hidden || fields[0].Before != nil || fields[0].After != nil {
		t.Fatalf("fields = %+v", fields)
	}
}
