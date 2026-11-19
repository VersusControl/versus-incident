package alarmsuspect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/aws/suspect"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	account  = "111122223333"
	region   = "us-east-1"
	alarmARN = "arn:aws:cloudwatch:us-east-1:111122223333:alarm:checkout-5xx"
)

var (
	webARN = "arn:aws:ecs:us-east-1:111122223333:service/main/web"
	apiARN = "arn:aws:ecs:us-east-1:111122223333:service/main/api"
)

type fixture struct {
	store *snapshot.Store
	feed  *snapshot.ChangeFeed
	now   time.Time
}

func newFixture(t *testing.T, options ...snapshot.Option) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return f.now }
	f.store = snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope(), append([]snapshot.Option{snapshot.WithClock(clock)}, options...)...)
	f.feed = snapshot.NewChangeFeed(f.store, snapshot.WithFeedClock(clock), snapshot.WithSuspectEngine(New(f.store)),
		snapshot.WithScope(func(context.Context) ([]snapshot.ScopeAccount, bool, error) {
			return []snapshot.ScopeAccount{{Account: account, Regions: []string{region}}}, true, nil
		}))
	return f
}

func service(arn, name string) model.Resource {
	return model.Resource{ARN: arn, Service: "ecs", Type: "service", Account: account, Region: region, Name: name}
}

// commitAlarm stores the inventory and one firing alarm on the given
// resources (none: the alarm's resource is unknown).
func (f *fixture) commitAlarm(t *testing.T, start time.Time, resources ...string) {
	t.Helper()
	if _, err := f.store.Commit(model.Unit{Account: account, Region: region, Service: "ecs"},
		snapshot.Candidate{Resources: []model.Resource{service(webARN, "web"), service(apiARN, "api")}}); err != nil {
		t.Fatal(err)
	}
	var health []model.Health
	for _, arn := range resources {
		health = append(health, model.Health{Resource: arn, Account: account, Region: region, Source: model.SourceAlarm, Status: model.HealthCritical,
			Summary: "Alarm checkout-5xx is in ALARM", Since: start, Ref: alarmARN})
	}
	if len(resources) == 0 {
		health = append(health, model.Health{Account: account, Region: region, Source: model.SourceAlarm, Status: model.HealthCritical,
			Summary: "Alarm checkout-5xx is in ALARM", Since: start, Ref: alarmARN})
	}
	if _, err := f.store.Commit(model.Unit{Account: account, Region: region, Service: AlarmService}, snapshot.Candidate{Health: health}); err != nil {
		t.Fatal(err)
	}
}

func change(id string, at time.Time, arn string) model.Change {
	return model.Change{ID: id, Time: at, Account: account, Region: region, Service: "ecs", EventSource: "ecs.amazonaws.com", EventName: "UpdateService",
		Summary: "Service updated", Resources: []string{arn}, Actor: model.Actor{Class: model.ActorPipeline, PrincipalARN: "arn:aws:iam::111122223333:role/deploy"}}
}

// pollAll records clean polls of the region and the global stream that read
// everything up to the unsettled tail.
func (f *fixture) pollAll(t *testing.T, found ...model.Change) {
	t.Helper()
	for _, pair := range []string{region, model.GlobalRegion} {
		var mine []model.Change
		if pair == region {
			mine = found
		}
		poll := snapshot.ChangePoll{Found: mine, HistoryFrom: f.now.Add(-48 * time.Hour),
			Covered: []changes.Interval{{From: f.now.Add(-26 * time.Hour), To: f.now.Add(-15 * time.Minute)}},
			Read:    []changes.Interval{{From: f.now.Add(-26 * time.Hour), To: f.now}}}
		if err := f.store.RecordChangePoll(account, pair, poll); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) answer(t *testing.T) snapshot.ChangesAnswer {
	t.Helper()
	answer, err := f.feed.Answer(context.Background(), snapshot.FeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// The shared engine picks the change on the alarm's resource from the hour
// before the alarm, and the answer carries the full item.
func TestEngineFindsSuspectForAlarm(t *testing.T) {
	f := newFixture(t)
	start := f.now.Add(-3 * time.Hour)
	f.commitAlarm(t, start, webARN)
	f.pollAll(t, change("deploy", start.Add(-20*time.Minute), webARN), change("after", start.Add(20*time.Minute), webARN))
	answer := f.answer(t)
	if len(answer.Alarms) != 1 || answer.Alarms[0].ID != alarmARN || len(answer.Alarms[0].ResourceARNs) != 1 {
		t.Fatalf("alarms = %+v", answer.Alarms)
	}
	got := answer.Suspect
	if got.Status != snapshot.SuspectFound || got.ChangeID != "deploy" || got.Change == nil || got.Change.ID != "deploy" || got.AlarmID != alarmARN {
		t.Fatalf("suspect = %+v", got)
	}
}

// Without a matching change in a fully read window the answer is "none";
// with the hour before the alarm unread it is "incomplete", never "none".
func TestEngineNoneOnlyWhenSettled(t *testing.T) {
	f := newFixture(t)
	start := f.now.Add(-3 * time.Hour)
	f.commitAlarm(t, start, webARN)
	f.pollAll(t, change("other", start.Add(-20*time.Minute), apiARN+"-elsewhere"))
	if got := f.answer(t).Suspect; got.Status != snapshot.SuspectNone || got.Searched == nil {
		t.Fatalf("read window = %+v", got)
	}
	unread := newFixture(t)
	unread.commitAlarm(t, start, webARN)
	if got := unread.answer(t).Suspect; got.Status != snapshot.SuspectIncomplete {
		t.Fatalf("unread window = %+v", got)
	}
}

// A read error is incomplete, never "none".
func TestEngineReadErrorIsIncomplete(t *testing.T) {
	if got := mapResult(alarmARN, suspect.Result{Status: suspect.StatusNone}, errors.New("read failed")); got.Status != snapshot.SuspectIncomplete {
		t.Fatalf("read error = %+v", got)
	}
}

// Two changes on the alarm's two resources tie: both are listed, the most
// recent first, neither singled out.
func TestTiedSuspectsNewestFirst(t *testing.T) {
	f := newFixture(t)
	start := f.now.Add(-3 * time.Hour)
	f.commitAlarm(t, start, webARN, apiARN)
	f.pollAll(t, change("older", start.Add(-40*time.Minute), webARN), change("newer", start.Add(-10*time.Minute), apiARN))
	got := f.answer(t).Suspect
	if got.Status != snapshot.SuspectFound || len(got.Candidates) != 2 || got.Candidates[0].ChangeID != "newer" || !got.Candidates[0].Tied || !got.Candidates[1].Tied {
		t.Fatalf("tied = %+v", got)
	}
}

// An alarm whose resource is unknown never gets a forced suspect.
func TestResourceUnknownAlarmPicksNothing(t *testing.T) {
	f := newFixture(t)
	start := f.now.Add(-3 * time.Hour)
	f.commitAlarm(t, start)
	f.pollAll(t, change("deploy", start.Add(-20*time.Minute), webARN))
	if got := f.answer(t).Suspect; got.Status != snapshot.SuspectResourceUnknown || got.ChangeID != "" {
		t.Fatalf("unknown resource = %+v", got)
	}
	empty := newFixture(t)
	empty.pollAll(t)
	if got := empty.answer(t).Suspect; got.Status != snapshot.SuspectNoAlarm {
		t.Fatalf("no alarm = %+v", got)
	}
}

// The alarm line names where its history starts: the oldest kept
// generation of the alarm units in scope, which the health cap bounds.
func TestAlarmHistoryStartsFromOldestCloudWatchGeneration(t *testing.T) {
	f := newFixture(t, snapshot.WithOptions(func() snapshot.Options {
		return snapshot.Options{MaxGenerations: 3}
	}))
	var starts []time.Time
	for step := 0; step < 10; step++ {
		f.now = f.now.Add(2 * time.Minute)
		starts = append(starts, f.now)
		f.commitAlarm(t, f.now, webARN)
	}
	f.pollAll(t)
	answer := f.answer(t)
	if answer.AlarmHistoryStarts == nil || !answer.AlarmHistoryStarts.Equal(starts[len(starts)-3]) {
		t.Fatalf("alarm history starts %v, want %s", answer.AlarmHistoryStarts, starts[len(starts)-3])
	}
}

// The change list folds ARNs exactly as the suspect engine does, so an
// inventory ARN and a change ARN of one resource match on both sides.
func TestResourceKeyMatchesSuspectKey(t *testing.T) {
	for _, arn := range []string{
		"arn:aws:autoscaling:us-east-1:111122223333:autoScalingGroup:6d5af669-eb3b-4530-894b-e3a7ae9ad1da:autoScalingGroupName/web",
		"arn:aws:cloudformation:us-east-1:111122223333:stack/network-prod/abc-123",
		"arn:aws:iam::111122223333:role/service/deploy",
		"arn:aws:rds:us-east-1:111122223333:db:Orders",
		"arn:aws:lambda:us-east-1:111122223333:function:api:7",
		"api",
	} {
		if got, want := changes.ResourceKey(arn), suspect.Key(arn); got != want {
			t.Errorf("%s: change list key %q, suspect key %q", arn, got, want)
		}
	}
}
