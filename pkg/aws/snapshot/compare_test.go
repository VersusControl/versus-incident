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

var _ = storage.AWSUnitHistory{}
