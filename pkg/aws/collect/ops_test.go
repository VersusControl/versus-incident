package collect

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/suspect"
)

func alarmCall() *call {
	return &call{unit: model.Unit{Account: productionAccount, Region: testRegion, Service: "cloudwatch"}, partition: "aws", endpointRegion: testRegion}
}

func asgAlarm(name, group string, transitioned, updated *time.Time) cwtypes.MetricAlarm {
	return cwtypes.MetricAlarm{
		AlarmName: aws.String(name), AlarmArn: aws.String(arnOf("cloudwatch", "alarm:"+name)),
		StateValue: cwtypes.StateValueAlarm, MetricName: aws.String("CPUUtilization"),
		StateTransitionedTimestamp: transitioned, StateUpdatedTimestamp: updated,
		Dimensions: []cwtypes.Dimension{{Name: aws.String("AutoScalingGroupName"), Value: aws.String(group)}},
	}
}

// TestAlarmStartIsTheStateTransition: the alarm start is
// StateTransitionedTimestamp. StateUpdatedTimestamp moves 40 minutes later
// on an evaluation update while the alarm keeps firing; a scale-up 20
// minutes after the transition is then never the suspect. A missing
// transition stays unknown and picks nothing.
func TestAlarmStartIsTheStateTransition(t *testing.T) {
	transition := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	updated := transition.Add(40 * time.Minute)
	out := alarmCall().alarms([]cwtypes.MetricAlarm{
		asgAlarm("web-cpu", "web", &transition, &updated),
		asgAlarm("no-start", "web", nil, &updated),
	}, nil)
	byRef := map[string]model.Health{}
	for _, item := range out.health {
		byRef[item.Ref] = item
	}
	started := byRef[arnOf("cloudwatch", "alarm:web-cpu")]
	if !started.Since.Equal(transition) {
		t.Fatalf("since = %v, want the transition %v", started.Since, transition)
	}
	if missing := byRef[arnOf("cloudwatch", "alarm:no-start")]; !missing.Since.IsZero() {
		t.Fatalf("missing start became %v", missing.Since)
	}

	scaleUp := model.Change{ID: "scale-up", Time: transition.Add(20 * time.Minute), Account: productionAccount, Region: testRegion,
		EventName: "SetDesiredCapacity", Resources: []string{started.Resource}, Actor: model.Actor{Class: model.ActorAWSService}}
	source := staticChanges{scaleUp}
	for _, alarm := range suspect.Alarms(out.health, nil) {
		result, err := suspect.Find(context.Background(), alarm.Anchor(nil), source, source)
		if err != nil {
			t.Fatal(err)
		}
		want := suspect.StatusNone
		if !alarm.StartKnown {
			want = suspect.StatusStartUnknown
		}
		if result.Status != want || len(result.Candidates) != 0 {
			t.Fatalf("%s: %+v", alarm.Name, result)
		}
	}
}

// TestAlarmKeepsEveryMonitoredResource: a metric alarm keeps every target
// its dimensions name, and a composite alarm over two Auto Scaling group
// alarms monitors both groups, so both are suspect candidates.
func TestAlarmKeepsEveryMonitoredResource(t *testing.T) {
	transition := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	multi := cwtypes.MetricAlarm{
		AlarmName: aws.String("db-and-fn"), AlarmArn: aws.String(arnOf("cloudwatch", "alarm:db-and-fn")),
		StateValue: cwtypes.StateValueAlarm, StateTransitionedTimestamp: &transition,
		Metrics: []cwtypes.MetricDataQuery{
			{MetricStat: &cwtypes.MetricStat{Metric: &cwtypes.Metric{Dimensions: []cwtypes.Dimension{{Name: aws.String("DBInstanceIdentifier"), Value: aws.String("orders")}}}}},
			{MetricStat: &cwtypes.MetricStat{Metric: &cwtypes.Metric{Dimensions: []cwtypes.Dimension{{Name: aws.String("FunctionName"), Value: aws.String("api")}}}}},
		},
	}
	composite := cwtypes.CompositeAlarm{
		AlarmName: aws.String("checkout-down"), AlarmArn: aws.String(arnOf("cloudwatch", "alarm:checkout-down")),
		StateValue: cwtypes.StateValueAlarm, StateTransitionedTimestamp: &transition,
		AlarmRule: aws.String(`ALARM("web-a-cpu") OR ALARM("web-b-cpu")`),
	}
	okA, okB := asgAlarm("web-a-cpu", "web-a", &transition, nil), asgAlarm("web-b-cpu", "web-b", &transition, nil)
	okA.StateValue, okB.StateValue = cwtypes.StateValueOk, cwtypes.StateValueOk
	out := alarmCall().alarms([]cwtypes.MetricAlarm{multi, okA, okB}, []cwtypes.CompositeAlarm{composite})

	resources := map[string][]string{}
	for _, item := range out.health {
		resources[item.Ref] = append(resources[item.Ref], item.Resource)
	}
	if got := resources[arnOf("cloudwatch", "alarm:db-and-fn")]; !slices.Equal(got, []string{arnOf("lambda", "function:api"), arnOf("rds", "db:orders")}) {
		t.Fatalf("metric alarm targets = %v", got)
	}
	groupA := arnOf("autoscaling", "autoScalingGroup:*:autoScalingGroupName/web-a")
	groupB := arnOf("autoscaling", "autoScalingGroup:*:autoScalingGroupName/web-b")
	if got := resources[arnOf("cloudwatch", "alarm:checkout-down")]; !slices.Equal(got, []string{groupA, groupB}) {
		t.Fatalf("composite targets = %v", got)
	}

	source := staticChanges{
		{ID: "a", Time: transition.Add(-10 * time.Minute), Account: productionAccount, Region: testRegion, Resources: []string{groupA}, Actor: model.Actor{Class: model.ActorPipeline}},
		{ID: "b", Time: transition.Add(-20 * time.Minute), Account: productionAccount, Region: testRegion, Resources: []string{groupB}, Actor: model.Actor{Class: model.ActorPipeline}},
	}
	for _, alarm := range suspect.Alarms(out.health, nil) {
		if alarm.Name != "checkout-down" {
			continue
		}
		result, err := suspect.Find(context.Background(), alarm.Anchor(nil), source, source)
		if err != nil || result.Status != suspect.StatusFound || len(result.Candidates) != 2 {
			t.Fatalf("composite candidates = %+v %v", result, err)
		}
		return
	}
	t.Fatal("composite alarm missing from the alarm line")
}

// staticChanges is a settled change history.
type staticChanges []model.Change

func (s staticChanges) Changes(_ context.Context, account, region string, from, to time.Time) ([]model.Change, error) {
	var out []model.Change
	for _, change := range s {
		if change.Account == account && change.Region == region && !change.Time.Before(from) && change.Time.Before(to) {
			out = append(out, change)
		}
	}
	return out, nil
}

func (staticChanges) Settled(string, string, time.Time, time.Time) bool { return true }
