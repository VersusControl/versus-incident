package suspect

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

const (
	accountA = "111122223333"
	accountB = "444455556666"
	euWest1  = "eu-west-1"
)

var alarmStart = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

// history is an in-memory change store keyed by account and Region, with a
// settled flag per account and Region.
type history struct {
	changes  []model.Change
	unsettle map[string]bool
	reads    []string
	err      error
}

func (h *history) Changes(_ context.Context, account, region string, from, to time.Time) ([]model.Change, error) {
	h.reads = append(h.reads, account+"/"+region)
	if h.err != nil {
		return nil, h.err
	}
	var out []model.Change
	for _, change := range h.changes {
		if change.Account == account && (region == "" || change.Region == region) &&
			!change.Time.Before(from) && change.Time.Before(to) {
			out = append(out, change)
		}
	}
	return out, nil
}

func (h *history) Settled(account, region string, _, _ time.Time) bool {
	return !h.unsettle[account+"/"+region]
}

// trail normalizes a real-shaped CloudTrail event, as ingest does.
func trail(t *testing.T, account, region, id, source, name string, at time.Time, identity, request map[string]any, resources ...string) model.Change {
	t.Helper()
	document, err := json.Marshal(map[string]any{"userIdentity": identity, "requestParameters": request, "readOnly": false})
	if err != nil {
		t.Fatal(err)
	}
	change, ok := changes.Normalize(account, region, changes.Event{ID: id, Name: name, Source: source, Time: at, Resources: resources, Raw: string(document)})
	if !ok {
		t.Fatalf("event %s dropped", id)
	}
	return change
}

var (
	pipeline = map[string]any{"type": "AssumedRole", "arn": "arn:aws:sts::111122223333:assumed-role/github-actions-deploy/run-1",
		"sessionContext": map[string]any{"sessionIssuer": map[string]any{"arn": "arn:aws:iam::111122223333:role/github-actions-deploy", "userName": "github-actions-deploy"}}}
	autoscalingService = map[string]any{"type": "AWSService", "invokedBy": "autoscaling.amazonaws.com"}
)

// TestRealShapedAutoScalingEventMatchesInventory: CloudTrail logs only the
// group name; the inventory holds the group's real ARN with its generated
// ID. The change still matches, through the canonical name form, and the
// answer is found. Matching the stored short name against the ARN, as the
// FinOps finder used to, never matched.
func TestRealShapedAutoScalingEventMatchesInventory(t *testing.T) {
	change := trail(t, accountA, euWest1, "evt-asg", "autoscaling.amazonaws.com", "UpdateAutoScalingGroup", alarmStart.Add(-20*time.Minute),
		pipeline, map[string]any{"autoScalingGroupName": "web", "maxSize": 12})
	inventoryARN := "arn:aws:autoscaling:eu-west-1:111122223333:autoScalingGroup:6d1f2a3b-0000-4000-8000-000000000001:autoScalingGroupName/web"
	for _, stored := range change.Resources {
		if stored == "web" {
			t.Fatal("short name stored as a resource")
		}
	}
	source := &history{changes: []model.Change{change}}
	result, err := Find(context.Background(), Anchor{Kind: AnchorAlarm, ID: "alarm", Account: accountA, Regions: []string{euWest1},
		Start: alarmStart, Targets: map[string]Scope{inventoryARN: ScopeResource}}, source, source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusFound || len(result.Candidates) != 1 || result.Candidates[0].Change.ID != "evt-asg" || result.Candidates[0].Confidence != "likely" {
		t.Fatalf("result = %+v", result)
	}
}

// TestShortNamesNeverCrossMatch: a Lambda function "api" and an ECS
// service "api" in two accounts share a last name part. A change to one is
// never a candidate for the other.
func TestShortNamesNeverCrossMatch(t *testing.T) {
	lambda := trail(t, accountA, euWest1, "evt-lambda", "lambda.amazonaws.com", "UpdateFunctionConfiguration20150331v2", alarmStart.Add(-10*time.Minute),
		pipeline, map[string]any{"functionName": "api"})
	ecs := trail(t, accountB, euWest1, "evt-ecs", "ecs.amazonaws.com", "UpdateService", alarmStart.Add(-10*time.Minute),
		pipeline, map[string]any{"cluster": "prod", "service": "api"})
	source := &history{changes: []model.Change{lambda, ecs}}

	ecsAlarm := Anchor{Kind: AnchorAlarm, ID: "ecs-5xx", Account: accountB, Regions: []string{euWest1}, Start: alarmStart,
		Targets: map[string]Scope{"arn:aws:ecs:eu-west-1:444455556666:service/prod/api": ScopeResource}}
	result, err := Find(context.Background(), ecsAlarm, source, source)
	if err != nil || result.Status != StatusFound || len(result.Candidates) != 1 || result.Candidates[0].Change.ID != "evt-ecs" {
		t.Fatalf("ecs alarm = %+v %v", result, err)
	}
	// The same names in the other account: the Lambda change does not match
	// an ECS service, and the ECS change belongs to another account.
	crossed := Anchor{Kind: AnchorAlarm, ID: "ecs-in-a", Account: accountA, Regions: []string{euWest1}, Start: alarmStart,
		Targets: map[string]Scope{"arn:aws:ecs:eu-west-1:111122223333:service/prod/api": ScopeResource}}
	if result, err := Find(context.Background(), crossed, source, source); err != nil || result.Status != StatusNone {
		t.Fatalf("cross match = %+v %v", result, err)
	}
	if Key("api") != "" || Key("service/prod/api") != "" {
		t.Fatal("a short name has a match key")
	}
}

// TestAlarmStartNeverPicksLaterChanges: the start is the state transition.
// A scale-up 20 minutes after it is never a candidate, even while the alarm
// keeps firing and its evaluation timestamp moves 40 minutes later. With
// nothing before the start the answer is none only over a settled window.
func TestAlarmStartNeverPicksLaterChanges(t *testing.T) {
	asg := "arn:aws:autoscaling:eu-west-1:111122223333:autoScalingGroup:*:autoScalingGroupName/web"
	scaleUp := trail(t, accountA, euWest1, "evt-scale-up", "autoscaling.amazonaws.com", "SetDesiredCapacity", alarmStart.Add(20*time.Minute),
		autoscalingService, map[string]any{"autoScalingGroupName": "web", "desiredCapacity": 9})
	atStart := trail(t, accountA, euWest1, "evt-at-start", "autoscaling.amazonaws.com", "SetDesiredCapacity", alarmStart,
		pipeline, map[string]any{"autoScalingGroupName": "web", "desiredCapacity": 8})
	source := &history{changes: []model.Change{scaleUp, atStart}}
	anchor := Anchor{Kind: AnchorAlarm, ID: "web-5xx", Account: accountA, Regions: []string{euWest1}, Start: alarmStart,
		Targets: map[string]Scope{asg: ScopeResource}}
	result, err := Find(context.Background(), anchor, source, source)
	if err != nil || result.Status != StatusNone || len(result.Candidates) != 0 {
		t.Fatalf("later changes picked: %+v %v", result, err)
	}
	if !result.Searched.To.Equal(alarmStart) || !result.Searched.From.Equal(alarmStart.Add(-AlarmLookback)) {
		t.Fatalf("searched = %+v", result.Searched)
	}

	// The same window with its global stream still arriving: incomplete,
	// never "no change matches".
	source.unsettle = map[string]bool{accountA + "/global": true}
	if result, _ := Find(context.Background(), anchor, source, source); result.Status != StatusIncomplete {
		t.Fatalf("unsettled window = %+v", result)
	}

	// No start time: no window, nothing picked, nothing read.
	source.reads = nil
	anchor.Start = time.Time{}
	if result, _ := Find(context.Background(), anchor, source, source); result.Status != StatusStartUnknown || len(source.reads) != 0 {
		t.Fatalf("unknown start = %+v reads=%v", result, source.reads)
	}

	// No resource: never a forced suspect.
	anchor.Start, anchor.Targets = alarmStart, nil
	if result, _ := Find(context.Background(), anchor, source, source); result.Status != StatusResourceUnknown || len(result.Candidates) != 0 {
		t.Fatalf("unknown resource = %+v", result)
	}

	// A read error is an error, never an empty answer.
	source.err = errors.New("boom")
	anchor.Targets = map[string]Scope{asg: ScopeResource}
	if _, err := Find(context.Background(), anchor, source, source); err == nil {
		t.Fatal("read error swallowed")
	}
}

// TestGlobalStreamIsSearched: an IAM change lives in the account's global
// stream. An alarm in eu-west-1 on a service running as that role finds it,
// and the answer names the Regions and time it searched.
func TestGlobalStreamIsSearched(t *testing.T) {
	role := "arn:aws:iam::111122223333:role/service-role/checkout-task"
	iam := trail(t, accountA, model.GlobalRegion, "evt-iam", "iam.amazonaws.com", "PutRolePolicy", alarmStart.Add(-30*time.Minute),
		pipeline, map[string]any{"roleName": "checkout-task", "policyName": "s3-read"})
	source := &history{changes: []model.Change{iam}}
	anchor := Anchor{Kind: AnchorAlarm, ID: "checkout-5xx", Account: accountA, Regions: []string{euWest1}, Start: alarmStart,
		Targets: map[string]Scope{"arn:aws:ecs:eu-west-1:111122223333:service/prod/checkout": ScopeResource, role: ScopeApplication}}
	result, err := Find(context.Background(), anchor, source, source)
	if err != nil || result.Status != StatusFound || result.Candidates[0].Change.ID != "evt-iam" {
		t.Fatalf("global change missed: %+v %v", result, err)
	}
	if !slices.Equal(result.Searched.Regions, []string{euWest1, model.GlobalRegion}) || !slices.Equal(result.Searched.Accounts, []string{accountA}) ||
		result.Searched.AllRegions || !slices.Equal(source.reads, []string{accountA + "/" + euWest1, accountA + "/global"}) {
		t.Fatalf("searched = %+v reads = %v", result.Searched, source.reads)
	}
}

// TestCompositeAlarmUsesEveryChildResource: a composite alarm over two
// Auto Scaling groups anchors on both, so a change to either group is a
// candidate; two at the top rank tie and neither is singled out.
func TestCompositeAlarmUsesEveryChildResource(t *testing.T) {
	composite := "arn:aws:cloudwatch:eu-west-1:111122223333:alarm:checkout-down"
	nameForm := func(name string) string {
		return "arn:aws:autoscaling:eu-west-1:111122223333:autoScalingGroup:*:autoScalingGroupName/" + name
	}
	items := []model.Health{
		{Resource: nameForm("web-a"), Account: accountA, Region: euWest1, Source: model.SourceAlarm, Since: alarmStart, Ref: composite},
		{Resource: nameForm("web-b"), Account: accountA, Region: euWest1, Source: model.SourceAlarm, Since: alarmStart, Ref: composite},
	}
	inventory := []model.Resource{{ARN: "arn:aws:autoscaling:eu-west-1:111122223333:autoScalingGroup:aaaa:autoScalingGroupName/web-a",
		Type: "auto-scaling-group", Account: accountA, Region: euWest1, Name: "web-a"}}
	alarms := Alarms(items, inventory)
	if len(alarms) != 1 || len(alarms[0].ResourceARNs) != 2 || !alarms[0].StartKnown || alarms[0].Name != "checkout-down" {
		t.Fatalf("alarms = %+v", alarms)
	}
	if !slices.Contains(alarms[0].ResourceARNs, inventory[0].ARN) {
		t.Fatalf("name form not resolved to the inventory ARN: %v", alarms[0].ResourceARNs)
	}
	source := &history{changes: []model.Change{
		trail(t, accountA, euWest1, "evt-a", "autoscaling.amazonaws.com", "UpdateAutoScalingGroup", alarmStart.Add(-15*time.Minute),
			pipeline, map[string]any{"autoScalingGroupName": "web-a"}),
		trail(t, accountA, euWest1, "evt-b", "autoscaling.amazonaws.com", "UpdateAutoScalingGroup", alarmStart.Add(-5*time.Minute),
			pipeline, map[string]any{"autoScalingGroupName": "web-b"}),
	}}
	result, err := Find(context.Background(), alarms[0].Anchor(inventory), source, source)
	if err != nil || result.Status != StatusFound || len(result.Candidates) != 2 {
		t.Fatalf("result = %+v %v", result, err)
	}
	for _, candidate := range result.Candidates {
		if !candidate.Tied || candidate.Confidence != "possible" {
			t.Fatalf("tied candidate singled out: %+v", candidate)
		}
	}
	if result.Candidates[0].Change.ID != "evt-b" {
		t.Fatalf("most recent first: %+v", result.Candidates)
	}

	// An alarm with no resolvable resource and no start: unknown, never a
	// forced suspect.
	unknown := Alarms([]model.Health{{Account: accountA, Region: euWest1, Source: model.SourceAlarm, Ref: "arn:aws:cloudwatch:eu-west-1:111122223333:alarm:x"}}, nil)
	if len(unknown) != 1 || unknown[0].StartKnown || len(unknown[0].ResourceARNs) != 0 {
		t.Fatalf("unknown alarm = %+v", unknown)
	}
	if result, _ := Find(context.Background(), unknown[0].Anchor(nil), source, source); result.Status != StatusStartUnknown {
		t.Fatalf("unknown alarm result = %+v", result)
	}
}

// TestAlarmHistoryStartsAtTheShallowestUnit: the alarm line names where
// its history starts, the latest oldest generation across the alarm units,
// so a flapping Region with short history is never read as quiet.
func TestAlarmHistoryStartsAtTheShallowestUnit(t *testing.T) {
	deep, shallow := alarmStart.Add(-24*time.Hour), alarmStart.Add(-96*time.Minute)
	if got := AlarmHistoryStarts([]time.Time{deep, shallow, {}}); !got.Equal(shallow) {
		t.Fatalf("history starts = %v", got)
	}
	if !AlarmHistoryStarts(nil).IsZero() {
		t.Fatal("no unit has history")
	}
}

func TestKeyFoldsEquivalentARNs(t *testing.T) {
	pairs := [][2]string{
		{"arn:aws:autoscaling:eu-west-1:1:autoScalingGroup:6d1f:autoScalingGroupName/web", "arn:aws:autoscaling:eu-west-1:1:autoScalingGroup:*:autoScalingGroupName/web"},
		{"arn:aws:cloudformation:eu-west-1:1:stack/net/abc-123", "arn:aws:cloudformation:eu-west-1:1:stack/net/*"},
		{"arn:aws:iam::1:role/service-role/task", "arn:aws:iam::1:role/task"},
		{"arn:aws:lambda:eu-west-1:1:function:api:live", "arn:aws:lambda:eu-west-1:1:function:api"},
		{"arn:aws:rds:eu-west-1:1:db:Orders", "arn:aws:rds:eu-west-1:1:db:orders"},
	}
	for _, pair := range pairs {
		if Key(pair[0]) != Key(pair[1]) || Key(pair[0]) == "" {
			t.Errorf("%s and %s do not fold", pair[0], pair[1])
		}
	}
	if Key("arn:aws:autoscaling:eu-west-1:1:autoScalingGroup:*:autoScalingGroupName/web") == Key("arn:aws:autoscaling:eu-west-1:1:autoScalingGroup:*:autoScalingGroupName/web-2") {
		t.Fatal("different groups fold")
	}
}

func TestCostShapingReadsVersionedEventNames(t *testing.T) {
	if !CostShaping("UpdateFunctionConfiguration20150331v2") || !CostShaping("PutProvisionedConcurrencyConfig20190930") || CostShaping("PutBucketTagging") {
		t.Fatal("cost shaping event names")
	}
}
