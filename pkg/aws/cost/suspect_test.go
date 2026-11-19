package cost

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

const (
	goldenAccount = "111111111111"
	goldenRegion  = "us-east-1"
)

func goldenARN(service, resource string) string {
	return model.ARN("aws", service, goldenRegion, goldenAccount, resource)
}

func goldenResource(arn, service, kind, name string, attributes map[string]string, relations ...model.Relation) model.Resource {
	return model.Resource{ARN: arn, Service: service, Type: kind, Account: goldenAccount, Region: goldenRegion, Name: name,
		Attributes: attributes, Relations: relations}
}

// goldenEvent is a LookupEvents record shaped as AWS writes it: the
// deploy pipeline's session, short names in requestParameters and the
// lookup's resource names.
func goldenEvent(t *testing.T, id, source, name string, at time.Time, request map[string]any, resources ...string) cloudTrailEvent {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"eventVersion": "1.09", "eventSource": source, "eventName": name, "awsRegion": goldenRegion, "readOnly": false,
		"userIdentity": map[string]any{"type": "AssumedRole", "arn": "arn:aws:sts::111111111111:assumed-role/terraform-apply/run-77",
			"sessionContext": map[string]any{"sessionIssuer": map[string]any{"type": "Role", "arn": "arn:aws:iam::111111111111:role/terraform-apply", "userName": "terraform-apply"}}},
		"userAgent": "APN/1.0 HashiCorp/1.0 Terraform/1.9.5", "requestParameters": request, "responseElements": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cloudTrailEvent{Account: goldenAccount, Region: goldenRegion, ID: id, Name: name, Source: source, Time: at, Resources: resources, Raw: string(raw)}
}

// TestSuspectGoldensFromRealShapedEvents: each FinOps line explained by a
// change ingested from a real-shaped CloudTrail event. The stored change
// carries only what Normalize resolves (an Auto Scaling group by name, a
// route table by ID, a database identifier in mixed case, a Lambda function
// with a versioned event name), never a fixture ARN, and the suspect is
// still found. Each case also has a quiet day with a complete window: no
// change found.
func TestSuspectGoldensFromRealShapedEvents(t *testing.T) {
	startDay := finops.MustDate("2026-10-20")
	dayStart := startDay.Time()
	asgARN := goldenARN("autoscaling", "autoScalingGroup:6d1f2a3b-1c2d-4e5f-8a9b-0c1d2e3f4a5b:autoScalingGroupName/web")
	instanceARN := goldenARN("ec2", "instance/i-0a1b2c3d4e5f67890")
	natARN := goldenARN("ec2", "natgateway/nat-1a")
	cases := []struct {
		name       string
		inventory  []model.Resource
		event      cloudTrailEvent
		line       finops.Detected
		confidence string
	}{
		{
			name: "auto scaling group",
			inventory: []model.Resource{
				goldenResource(instanceARN, "ec2", "instance", "web-1", map[string]string{model.AttrSize: "m6i.large"}),
				goldenResource(asgARN, "auto-scaling", "auto-scaling-group", "web", nil, model.Relation{Type: model.RelationContains, Target: instanceARN}),
			},
			event: goldenEvent(t, "evt-asg", "autoscaling.amazonaws.com", "UpdateAutoScalingGroup", dayStart.Add(-10*time.Hour),
				map[string]any{"autoScalingGroupName": "web", "minSize": 6, "desiredCapacity": 6}),
			line: finops.Detected{Key: finops.LineKey{Account: goldenAccount, Service: "compute.vm", UsageType: "USE1-BoxUsage:m6i.large", RecordType: "usage"},
				ServiceName: "Amazon Elastic Compute Cloud - Compute", StartedOn: startDay},
			confidence: "likely",
		},
		{
			name: "route table",
			inventory: []model.Resource{
				goldenResource(natARN, "vpc", "nat-gateway", "nat-1a", map[string]string{model.AttrID: "nat-1a"}),
				goldenResource(goldenARN("ec2", "route-table/rtb-0abc"), "vpc", "route-table", "private", nil,
					model.Relation{Type: model.RelationRoutesTo, Target: natARN}),
			},
			event: goldenEvent(t, "evt-route", "ec2.amazonaws.com", "ReplaceRoute", dayStart.Add(3*time.Hour),
				map[string]any{"routeTableId": "rtb-0abc", "destinationCidrBlock": "0.0.0.0/0", "natGatewayId": "nat-1a"}, "rtb-0abc"),
			line: finops.Detected{Key: finops.LineKey{Account: goldenAccount, Service: "network.nat", UsageType: "USE1-NatGateway-Bytes", RecordType: "usage"},
				ServiceName: "EC2 - Other", StartedOn: startDay},
			confidence: "likely",
		},
		{
			name: "database",
			inventory: []model.Resource{
				goldenResource(goldenARN("rds", "db:orders"), "rds", "db-instance", "orders", nil),
			},
			event: goldenEvent(t, "evt-rds", "rds.amazonaws.com", "ModifyDBInstance", dayStart.Add(-30*time.Hour),
				map[string]any{"dBInstanceIdentifier": "Orders", "dBInstanceClass": "db.r6g.2xlarge", "applyImmediately": true}),
			line: finops.Detected{Key: finops.LineKey{Account: goldenAccount, Service: "database.relational", UsageType: "USE1-InstanceUsage:db.r6g.2xl", RecordType: "usage"},
				ServiceName: "Amazon Relational Database Service", StartedOn: startDay},
			confidence: "likely",
		},
		{
			name: "lambda function",
			inventory: []model.Resource{
				goldenResource(goldenARN("lambda", "function:api"), "lambda", "function", "api", nil),
			},
			event: goldenEvent(t, "evt-lambda", "lambda.amazonaws.com", "UpdateFunctionConfiguration20150331v2", dayStart.Add(-2*time.Hour),
				map[string]any{"functionName": "api", "memorySize": 3008}),
			line: finops.Detected{Key: finops.LineKey{Account: goldenAccount, Service: "compute.function", UsageType: "USE1-Lambda-GB-Second", RecordType: "usage"},
				ServiceName: "AWS Lambda", StartedOn: startDay},
			confidence: "likely",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			change, ok := changes.Normalize(tc.event.Account, tc.event.Region, changes.Event{ID: tc.event.ID, Name: tc.event.Name,
				Source: tc.event.Source, Time: tc.event.Time, Resources: tc.event.Resources, Raw: tc.event.Raw})
			if !ok || len(change.Resources) == 0 {
				t.Fatalf("normalized = %+v", change)
			}
			for _, arn := range change.Resources {
				for _, resource := range tc.inventory {
					if arn == resource.Name || arn == resource.Attributes[model.AttrID] {
						t.Fatalf("short name %q stored as a resource", arn)
					}
				}
			}
			sc := &scenario{Inventory: tc.inventory, CloudTrail: []cloudTrailEvent{tc.event},
				ChangeCoverage: []changes.Interval{{From: dayStart.Add(-10 * 24 * time.Hour), To: dayStart.Add(10 * 24 * time.Hour)}}}
			finder := NewSuspectFinder(loadInventory(t, sc))
			list, _, err := finder.Suspects(ctx, tc.line)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 || list[0].State != "found" || list[0].Ref != "change:"+tc.event.ID || list[0].Confidence != tc.confidence ||
				list[0].Actor != "terraform-apply" || !list[0].At.Equal(tc.event.Time) {
				t.Fatalf("suspects = %+v", list)
			}
			quiet := tc.line
			quiet.StartedOn = finops.MustDate("2026-10-25")
			if list, _, err := finder.Suspects(ctx, quiet); err != nil || len(list) != 1 || list[0].State != "none_found" {
				t.Fatalf("quiet day = %+v %v", list, err)
			}
		})
	}
}

// TestCostSuspectNeverAfterTheStartDay: a change after the end of the
// increase's first day did not start it.
func TestCostSuspectNeverAfterTheStartDay(t *testing.T) {
	startDay := finops.MustDate("2026-10-20")
	sc := &scenario{
		Inventory: []model.Resource{goldenResource(goldenARN("lambda", "function:api"), "lambda", "function", "api", nil)},
		CloudTrail: []cloudTrailEvent{goldenEvent(t, "evt-late", "lambda.amazonaws.com", "UpdateFunctionConfiguration20150331v2",
			startDay.Time().Add(26*time.Hour), map[string]any{"functionName": "api"})},
		ChangeCoverage: []changes.Interval{{From: startDay.Time().Add(-10 * 24 * time.Hour), To: startDay.Time().Add(10 * 24 * time.Hour)}},
	}
	line := finops.Detected{Key: finops.LineKey{Account: goldenAccount, Service: "compute.function", UsageType: "USE1-Lambda-GB-Second", RecordType: "usage"},
		ServiceName: "AWS Lambda", StartedOn: startDay}
	list, _, err := NewSuspectFinder(loadInventory(t, sc)).Suspects(ctx, line)
	if err != nil || len(list) != 1 || list[0].State != "none_found" {
		t.Fatalf("late change picked: %+v %v", list, err)
	}
}
