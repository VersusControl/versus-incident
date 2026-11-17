package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

func resourcesByARN(t *testing.T, h harness, account string) map[string]model.Resource {
	t.Helper()
	result, err := h.store.Resources(context.Background(), snapshot.Query{Account: account})
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]model.Resource, len(result.Resources))
	for _, resource := range result.Resources {
		out[resource.ARN] = resource
	}
	return out
}

func hasRelation(resource model.Resource, kind model.RelationType, target string) bool {
	for _, relation := range resource.Relations {
		if relation.Type == kind && relation.Target == target {
			return true
		}
	}
	return false
}

func arnOf(service, resource string) string {
	return model.ARN("aws", service, testRegion, productionAccount, resource)
}

// TestSyncNormalizesEveryCatalogService reads the whole world once and
// checks every unit committed with the resource types its service owns.
func TestSyncNormalizesEveryCatalogService(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	report := mustSync(t, h, target(productionAccount))
	if failed := report.Failed(); failed != 0 {
		t.Fatalf("failed units = %d: %+v", failed, report.Units)
	}
	if len(report.Units) != len(Services()) {
		t.Fatalf("units = %d, want %d", len(report.Units), len(Services()))
	}

	result, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]map[string]bool{}
	for _, resource := range result.Resources {
		if types[resource.Service] == nil {
			types[resource.Service] = map[string]bool{}
		}
		types[resource.Service][resource.Type] = true
	}
	want := map[string][]string{
		"cloudwatch":     {"alarm", "composite-alarm"},
		"ecs":            {"cluster", "service"},
		"elbv2":          {"load-balancer", "target-group"},
		"ec2":            {"instance"},
		"rds":            {"db-instance"},
		"cloudformation": {"stack"},
		"vpc":            {"vpc", "subnet", "route-table", "internet-gateway", "nat-gateway", "security-group", "network-interface", "vpc-endpoint"},
		"auto-scaling":   {"auto-scaling-group"},
		"lambda":         {"function"},
		"eks":            {"cluster", "node-group"},
		"dynamodb":       {"table"},
		"elasticache":    {"cache-cluster"},
		"sqs":            {"queue"},
		"sns":            {"topic"},
		"eventbridge":    {"rule"},
		"step-functions": {"state-machine"},
		"kinesis":        {"stream"},
		"api-gateway":    {"rest-api"},
		"appregistry":    {"application"},
		"s3":             {"bucket"},
		"cloudfront":     {"distribution"},
		"route53":        {"health-check"},
		"iam":            {"role"},
	}
	for service, kinds := range want {
		for _, kind := range kinds {
			if !types[service][kind] {
				t.Errorf("service %s has no %s resource (got %v)", service, kind, types[service])
			}
		}
	}
	if types[snapshot.TagService] != nil {
		t.Fatal("tag sets were listed as resources")
	}
	if len(result.Gaps) != 0 {
		t.Fatalf("clean sync has gaps: %+v", result.Gaps)
	}
	for _, unit := range result.Units {
		if !unit.HasData || unit.Sync.State != model.SyncOK || unit.Sync.LastGoodAt.IsZero() {
			t.Fatalf("unit not committed: %+v", unit)
		}
	}
}

// TestSyncRelationsApplicationsAndRedaction checks typed relations, the
// application resolver order and tag value redaction on the committed view.
func TestSyncRelationsApplicationsAndRedaction(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	mustSync(t, h, target(productionAccount))
	byARN := resourcesByARN(t, h, productionAccount)

	balancer := byARN[arnOf("elasticloadbalancing", "loadbalancer/"+fakeaws.WorldLoadBalancerID)]
	targetGroup := arnOf("elasticloadbalancing", fakeaws.WorldTargetGroupID)
	instance := arnOf("ec2", "instance/i-0123456789abcdef0")
	service := byARN[arnOf("ecs", "service/versus/checkout")]
	function := byARN[arnOf("lambda", "function:"+fakeaws.WorldFunction)]
	queue := arnOf("sqs", fakeaws.WorldQueue)
	topic := byARN[arnOf("sns", "versus-alarms")]

	checks := []struct {
		name string
		ok   bool
	}{
		{"load balancer routes to target group", hasRelation(balancer, model.RelationRoutesTo, targetGroup)},
		{"target group routes to instance", hasRelation(byARN[targetGroup], model.RelationRoutesTo, instance)},
		{"service attached to target group", hasRelation(service, model.RelationAttachedTo, targetGroup)},
		{"service member of cluster", hasRelation(service, model.RelationMemberOf, arnOf("ecs", "cluster/versus"))},
		{"service part of stack", hasRelation(service, model.RelationPartOfStack, arnOf("cloudformation", "stack/versus-stack/00000000-0000-0000-0000-00000000aaaa"))},
		{"function triggered by queue", hasRelation(function, model.RelationTriggeredBy, queue)},
		{"queue dead-letters to dlq", hasRelation(byARN[queue], model.RelationDeadLetter, queue+"-dlq")},
		{"topic subscribes queue", hasRelation(topic, model.RelationSubscribes, queue)},
		{"instance in vpc", hasRelation(byARN[instance], model.RelationInVPC, arnOf("ec2", "vpc/"+fakeaws.WorldVPC))},
		{"rule targets function", hasRelation(byARN[arnOf("events", "rule/versus-nightly")], model.RelationTargets, function.ARN)},
		{"alarm monitors load balancer", hasRelation(byARN[arnOf("cloudwatch", "alarm:"+fakeaws.WorldAlarm5xx)], model.RelationMonitors, balancer.ARN)},
	}
	for _, check := range checks {
		if !check.ok {
			t.Errorf("relation missing: %s", check.name)
		}
	}
	for _, relation := range topic.Relations {
		if strings.Contains(relation.Target, "@") {
			t.Fatalf("personal subscription endpoint kept: %+v", relation)
		}
	}

	// AppRegistry names the stack, so stack members resolve to it first.
	if service.App != fakeaws.WorldApplication || service.AppSource != snapshot.AppSourceAppRegistry {
		t.Fatalf("service app = %q from %q", service.App, service.AppSource)
	}
	// The function's tags come only from the tagging API, yet it is in the
	// stack too, so AppRegistry still wins.
	if function.AppSource != snapshot.AppSourceAppRegistry {
		t.Fatalf("function app source = %q", function.AppSource)
	}
	table := byARN[arnOf("dynamodb", "table/versus-locks")]
	if table.App != "platform" || table.AppSource != snapshot.AppSourceTags {
		t.Fatalf("table app from tagging API = %q/%q", table.App, table.AppSource)
	}

	tags := byARN[instance].Tags
	if tags["app"] != fakeaws.WorldApplication || tags["owner"] != model.RedactedValue {
		t.Fatalf("instance tags = %v", tags)
	}
	if function.Tags["cost-center"] != model.RedactedValue || function.Tags["team"] != "jobs" {
		t.Fatalf("function tags = %v", function.Tags)
	}
}

func TestSyncSecurityFlags(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	mustSync(t, h, target(productionAccount))
	result, err := h.store.SecurityFlags(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, flag := range result.Flags {
		found[string(flag.Kind)+" "+flag.Resource] = true
	}
	for _, want := range []string{
		"admin_port_open " + arnOf("ec2", "security-group/"+fakeaws.WorldAdminGroup),
		"public_exposure " + arnOf("elasticloadbalancing", "loadbalancer/"+fakeaws.WorldLoadBalancerID),
		"public_exposure " + arnOf("ec2", "instance/i-0123456789abcdef0"),
		"public_exposure " + arnOf("eks", "cluster/versus-eks"),
		"public_exposure arn:aws:s3:::" + fakeaws.WorldPublicBucket,
		"encryption_off arn:aws:s3:::" + fakeaws.WorldPublicBucket,
		"encryption_off " + arnOf("sqs", fakeaws.WorldQueue+"-dlq"),
		"no_alarms " + arnOf("rds", "db:versus-db"),
	} {
		if !found[want] {
			t.Errorf("missing flag %s", want)
		}
	}
	for _, unwanted := range []string{
		"admin_port_open " + arnOf("ec2", "security-group/"+fakeaws.WorldSecurityGroup),
		"public_exposure arn:aws:s3:::" + fakeaws.WorldPrivateBucket,
		"no_alarms " + arnOf("ec2", "instance/i-0123456789abcdef0"),
		"no_tags " + arnOf("ec2", "instance/i-0123456789abcdef0"),
	} {
		if found[unwanted] {
			t.Errorf("unexpected flag %s", unwanted)
		}
	}
}

// TestBadDeployThroughStoreReadSurface is the acceptance path: the alarm,
// the unhealthy targets and the task-definition change, all read back from
// the committed store.
func TestBadDeployThroughStoreReadSurface(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	mustSync(t, h, target(productionAccount))
	ctx := context.Background()

	health, err := h.store.Health(ctx, snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	var alarm, targets, rollout *model.Health
	for index := range health.Health {
		item := &health.Health[index]
		switch {
		case item.Source == model.SourceAlarm && strings.Contains(item.Ref, fakeaws.WorldAlarm5xx):
			alarm = item
		case item.Source == model.SourceTargetHealth:
			targets = item
		case item.Source == model.SourceRollout:
			rollout = item
		}
	}
	if alarm == nil || alarm.Status != model.HealthCritical || alarm.Resource != arnOf("elasticloadbalancing", "loadbalancer/"+fakeaws.WorldLoadBalancerID) {
		t.Fatalf("5xx alarm = %+v", alarm)
	}
	if targets == nil || !strings.HasPrefix(targets.Summary, "3 of 4 targets unhealthy") {
		t.Fatalf("target health = %+v", targets)
	}
	if rollout == nil || !strings.Contains(rollout.Summary, "checkout:42") {
		t.Fatalf("rollout = %+v", rollout)
	}
	start := fakeaws.BadDeployStart(time.Now())
	if gap := alarm.Since.Sub(start); gap < 3*time.Minute || gap > 5*time.Minute {
		t.Fatalf("alarm fired %s after the deploy, want about four minutes", gap)
	}

	changes, err := h.store.Changes(ctx, snapshot.Query{Account: productionAccount, Service: "ecs"}, time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	summaries := map[string]model.Change{}
	for _, change := range changes.Changes {
		summaries[change.Summary] = change
	}
	registered, ok := summaries["New task definition checkout:42 registered"]
	if !ok {
		t.Fatalf("task definition change missing: %+v", changes.Changes)
	}
	if registered.Actor.Class != model.ActorPipeline {
		t.Fatalf("deploy actor = %+v", registered.Actor)
	}
	if _, ok := summaries["Service checkout updated to task definition checkout:42"]; !ok {
		t.Fatalf("service update missing: %+v", changes.Changes)
	}
	// Scoped to the service, the change history names the update.
	scoped, err := h.store.Changes(ctx, snapshot.Query{Account: productionAccount, ARN: arnOf("ecs", "service/versus/checkout")}, time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
	if err != nil || len(scoped.Changes) == 0 {
		t.Fatalf("changes for the service = %+v, %v", scoped.Changes, err)
	}
}

func TestHealthEventScenario(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioHealthEvent)
	mustSync(t, h, target(productionAccount), KindHealth)
	health, err := h.store.Health(context.Background(), snapshot.Query{Account: productionAccount, Service: "aws-health"})
	if err != nil {
		t.Fatal(err)
	}
	if len(health.Health) != 1 {
		t.Fatalf("aws health = %+v", health.Health)
	}
	event := health.Health[0]
	if event.Resource != arnOf("ec2", "instance/i-0123456789abcdef0") || event.Ref != fakeaws.HealthEventARN || event.Region != testRegion {
		t.Fatalf("health event = %+v", event)
	}
}

func TestHealthNotOnPlanIsANamedGap(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioHealthEvent)
	mustSync(t, h, target(stagingAccount, "eu-west-1"), KindHealth)
	statuses, err := h.store.Sync(context.Background(), snapshot.Query{Account: stagingAccount, Service: "aws-health"})
	if err != nil || len(statuses) != 1 {
		t.Fatalf("statuses = %+v, %v", statuses, err)
	}
	sync := statuses[0].Sync
	if sync.State != model.SyncFailed || sync.ErrorCode != CodeNotOnPlan || sync.Gaps[0].Reason != model.GapNotEnabled {
		t.Fatalf("not on plan = %+v", sync)
	}
}

func TestAccessDeniedBurstKeepsOtherUnits(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioAccessDeniedBurst)
	mustSync(t, h, target(productionAccount))
	statuses, err := h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	denied := map[string]bool{"lambda": true, "sqs": true, "sns": true, "eventbridge": true, "step-functions": true, "kinesis": true, snapshot.TagService: true}
	for _, status := range statuses {
		service := status.Unit.Service
		if denied[service] {
			if status.Sync.State != model.SyncFailed || status.Sync.ErrorCode != CodeDenied || status.HasData {
				t.Errorf("%s = %+v, want denied without data", service, status)
			}
			if len(status.Sync.Gaps) != 1 || status.Sync.Gaps[0].Reason != model.GapDenied {
				t.Errorf("%s gaps = %+v", service, status.Sync.Gaps)
			}
			continue
		}
		if status.Sync.State != model.SyncOK {
			t.Errorf("%s = %+v, want committed", service, status.Sync)
		}
	}
	// A denied detail call commits the list and names the missing part.
	elb, _ := h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount, Service: "elbv2"})
	gaps := elb[0].Sync.Gaps
	if !elb[0].HasData || len(gaps) != 1 || gaps[0].Operation != "DescribeTargetHealth" || gaps[0].Reason != model.GapDenied {
		t.Fatalf("elbv2 = %+v", elb[0])
	}
}

// TestThrottledAPIShowsGapsNotZeros syncs cleanly, then again under
// throttling: no committed resource disappears and the failures are named.
func TestThrottledAPIShowsGapsNotZeros(t *testing.T) {
	h := newHarness(t, "throttled-api")
	h.fake.SetThrottle(0)
	mustSync(t, h, target(productionAccount))
	before, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Resources) == 0 || len(before.Gaps) != 0 {
		t.Fatalf("clean sync = %d resources, gaps %+v", len(before.Resources), before.Gaps)
	}

	h.fake.SetThrottle(2)
	report := mustSync(t, h, target(productionAccount))
	if report.Failed() == 0 {
		t.Fatal("throttling did not fail any unit")
	}
	after, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Resources) != len(before.Resources) {
		t.Fatalf("resources %d -> %d under throttling", len(before.Resources), len(after.Resources))
	}
	throttled := 0
	for _, gap := range after.Gaps {
		if gap.Reason == model.GapThrottled {
			throttled++
			if gap.LastGoodAt.IsZero() {
				t.Fatalf("throttled gap has no last good time: %+v", gap)
			}
		}
	}
	if throttled == 0 {
		t.Fatalf("no throttled gaps: %+v", after.Gaps)
	}
	for _, unit := range after.Units {
		if unit.Sync.State == model.SyncFailed && !unit.HasData {
			t.Fatalf("failed unit lost its data: %+v", unit)
		}
	}
}

// TestFirstSyncUnderThrottlingNamesUnreadUnits proves a unit that never
// committed reads as a gap, never as zero resources.
func TestFirstSyncUnderThrottlingNamesUnreadUnits(t *testing.T) {
	h := newHarness(t, "throttled-api")
	h.fake.SetThrottle(1)
	mustSync(t, h, target(productionAccount), KindInventory)
	result, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount, Service: "lambda"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 0 || len(result.Units) != 1 || result.Units[0].HasData {
		t.Fatalf("lambda = %+v", result)
	}
	if len(result.Gaps) != 1 || result.Gaps[0].Reason != model.GapThrottled {
		t.Fatalf("lambda gaps = %+v", result.Gaps)
	}
}

// TestTransientDetailFailureKeepsLastGood runs one unit whose detail call is
// throttled after a good commit: the unit keeps the good generation.
func TestTransientDetailFailureKeepsLastGood(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	unit := model.Unit{Account: productionAccount, Region: testRegion, Service: "elbv2"}
	throttle := false
	entry := spec{service: "elbv2", kind: KindHealth, run: func(_ context.Context, c *call) (result, error) {
		resource := c.newResource(arnOf("elasticloadbalancing", "targetgroup/web/1"), "target-group", "web", "healthy")
		out := result{resources: []model.Resource{resource}}
		if !throttle {
			out.health = []model.Health{c.healthItem(resource.ARN, model.SourceTargetHealth, model.HealthWarning, "1 of 2 targets unhealthy", time.Time{}, resource.ARN)}
			return out, nil
		}
		out.gaps = append(out.gaps, c.detailGap("DescribeTargetHealth", errLocalBackoff))
		return out, nil
	}}
	run := func() UnitReport {
		c := &call{env: env{account: productionAccount, region: testRegion, credentials: testCredentials(productionAccount)}, unit: unit, partition: "aws", endpointRegion: testRegion, now: time.Now(), writer: h.store.Writer(productionAccount)}
		return h.collector.runUnit(context.Background(), c, entry)
	}
	if report := run(); report.Outcome != OutcomeCommitted {
		t.Fatalf("first run = %+v", report)
	}
	throttle = true
	if report := run(); report.Outcome != OutcomeKept || report.ErrorCode != codeThrottledWord {
		t.Fatalf("throttled run = %+v", report)
	}
	health, _ := h.store.Health(context.Background(), snapshot.Query{Account: productionAccount, Service: "elbv2"})
	if len(health.Health) != 1 {
		t.Fatalf("unhealthy signal was dropped: %+v", health.Health)
	}
	if len(health.Gaps) != 1 || health.Gaps[0].Operation != "DescribeTargetHealth" || health.Gaps[0].Reason != model.GapThrottled {
		t.Fatalf("gaps = %+v", health.Gaps)
	}
}

// TestSecretCanaryLeaksNothing plants canaries in tags, Lambda environment,
// CloudTrail request parameters and tagging values, then scans everything the
// store wrote, every read answer and every log line.
func TestSecretCanaryLeaksNothing(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	logger := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(&logs, format+"\n", args...)
	}
	h := newHarness(t, "secret-canary", WithLogger(logger))
	mustSync(t, h, target(productionAccount))
	h.fake.SetThrottle(2)
	mustSync(t, h, target(productionAccount))

	ctx := context.Background()
	var surface bytes.Buffer
	blobs, err := h.provider.ListBlobs("")
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range blobs {
		surface.Write(blob.Data)
	}
	// Every inventory row as stored, not as assembled for readers.
	inventory, _ := storage.AWSInventory(h.provider)
	scope := tenancy.DefaultOrgScope()
	units, _ := inventory.AWSUnits(ctx, scope, storage.AWSUnitQuery{})
	if len(units) == 0 {
		t.Fatal("nothing was stored")
	}
	storedResources, _ := inventory.AWSResources(ctx, scope, storage.AWSResourceQuery{})
	storedHealth, _ := inventory.AWSHealth(ctx, scope, storage.AWSHealthQuery{})
	storedFlags, _ := inventory.AWSSecurityFlags(ctx, scope, storage.AWSFlagQuery{})
	storedChanges, _ := inventory.AWSChanges(ctx, scope, storage.AWSChangeQuery{})
	for _, rows := range []any{units, storedResources, storedHealth, storedFlags, storedChanges} {
		encoded, _ := json.Marshal(rows)
		surface.Write(encoded)
	}
	resources, _ := h.store.Resources(ctx, snapshot.Query{})
	health, _ := h.store.Health(ctx, snapshot.Query{})
	flags, _ := h.store.SecurityFlags(ctx, snapshot.Query{})
	changes, _ := h.store.Changes(ctx, snapshot.Query{}, time.Unix(0, 0), time.Now().Add(time.Hour))
	for _, answer := range []any{resources, health, flags, changes} {
		encoded, _ := json.Marshal(answer)
		surface.Write(encoded)
	}
	logMu.Lock()
	surface.Write(logs.Bytes())
	logMu.Unlock()
	if !bytes.Contains(surface.Bytes(), []byte(model.RedactedValue)) {
		t.Fatal("canary tags were not seen at all; the scan proves nothing")
	}
	for _, secret := range fakeaws.CanarySecrets() {
		if bytes.Contains(surface.Bytes(), []byte(secret)) {
			t.Fatalf("canary %q leaked", secret)
		}
	}
}

// TestWorldSecretsStayOut checks the bad-deploy world's own sensitive
// values: an environment secret in a task definition, user data, a personal
// e-mail endpoint and a session name.
func TestWorldSecretsStayOut(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	mustSync(t, h, target(productionAccount))
	blobs, err := h.provider.ListBlobs("")
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range blobs {
		for _, secret := range []string{"vrs-env-secret-do-not-store", "c2VjcmV0LXVzZXItZGF0YQ==", fakeaws.WorldPersonalEmail, fakeaws.WorldRedactedTagValue, "GitHubActions"} {
			if bytes.Contains(blob.Data, []byte(secret)) {
				t.Fatalf("%s stored %q", blob.Name, secret)
			}
		}
	}
}

func TestLoadAccountPagesEveryList(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioLoad5k)
	report := mustSync(t, h, target(productionAccount), KindInventory, KindHealth)
	if report.Failed() != 0 {
		t.Fatalf("failed units: %+v", report.Units)
	}
	counts := map[string]int{}
	result, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range result.Resources {
		counts[resource.Service+"/"+resource.Type]++
	}
	load := fakeaws.DefaultLoadSize
	want := map[string]int{
		"ec2/instance":    fakeaws.WorldTargets + load*4/10,
		"lambda/function": 1 + load*2/10,
		"sqs/queue":       2 + load*2/10,
		"dynamodb/table":  1 + (load-load*4/10-2*(load*2/10))/2,
	}
	for key, expected := range want {
		if counts[key] != expected {
			t.Errorf("%s = %d, want %d", key, counts[key], expected)
		}
	}
	if len(result.Resources) < load {
		t.Fatalf("resources = %d, want at least %d", len(result.Resources), load)
	}
	// Queue detail beyond the fan-out cap is a named gap, not a false
	// "encrypted" or "unencrypted".
	sqs, _ := h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount, Service: "sqs"})
	if len(sqs) != 1 || len(sqs[0].Sync.Gaps) != 1 || sqs[0].Sync.Gaps[0].Operation != "GetQueueAttributes" {
		t.Fatalf("sqs = %+v", sqs)
	}
}

func TestPlanCoversGlobalOnceAndRegionsEach(t *testing.T) {
	units := Plan(productionAccount, []string{"eu-west-1", "us-east-1", "us-east-1", "bogus"})
	perService := map[string]int{}
	for _, unit := range units {
		perService[unit.Service]++
	}
	if perService["s3"] != 1 || perService["iam"] != 1 || perService["ec2"] != 2 || perService[snapshot.ChangeService] != 2 {
		t.Fatalf("plan = %v", perService)
	}
	health := Plan(productionAccount, []string{"us-east-1"}, KindHealth)
	for _, unit := range health {
		if unit.Service == "lambda" || unit.Service == snapshot.ChangeService {
			t.Fatalf("health plan has %s", unit.Service)
		}
	}
}

func TestSyncRejectsConcurrentRunAndBadAccount(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	if _, err := h.collector.Sync(context.Background(), Target{AccountID: "not-an-account"}); err == nil {
		t.Fatal("invalid account accepted")
	}
	h.collector.mu.Lock()
	h.collector.running[productionAccount] = true
	h.collector.mu.Unlock()
	if _, err := h.collector.Sync(context.Background(), target(productionAccount)); err != ErrBusy {
		t.Fatalf("err = %v, want busy", err)
	}
}

func TestMissingCredentialsKeepUnits(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	report := mustSync(t, h, Target{AccountID: productionAccount, Regions: []string{testRegion}}, KindHealth)
	if report.Failed() != len(report.Units) || report.Units[0].ErrorCode != "no_server_identity" {
		t.Fatalf("report = %+v", report)
	}
	if h.fake.TotalRequests() != 0 {
		t.Fatalf("requests without credentials = %d", h.fake.TotalRequests())
	}
}

// nameTagResources are the EC2 and VPC resources whose display name comes
// from the Name tag, with their ID and the Name tag value the fake world
// gives them.
var nameTagResources = []struct{ arn, id, name string }{
	{arnOf("ec2", "instance/i-0123456789abcdef0"), "i-0123456789abcdef0", "versus-web"},
	{arnOf("ec2", "vpc/"+fakeaws.WorldVPC), fakeaws.WorldVPC, "versus-vpc"},
	{arnOf("ec2", "subnet/"+fakeaws.WorldSubnet), fakeaws.WorldSubnet, "versus-subnet"},
	{arnOf("ec2", "route-table/rtb-0a1b2c3d"), "rtb-0a1b2c3d", "versus-routes"},
	{arnOf("ec2", "internet-gateway/igw-0a1b2c3d"), "igw-0a1b2c3d", "versus-igw"},
	{arnOf("ec2", "natgateway/nat-0a1b2c3d"), "nat-0a1b2c3d", "versus-nat"},
}

// The Name tag value is a tag value: each collector that names a resource
// from it does so only while Name is on the tag-value allowlist, and shows
// the resource ID otherwise, with the value itself redacted.
func TestNameTagNamesAResourceOnlyWhenAllowlisted(t *testing.T) {
	for _, allowName := range []bool{false, true} {
		allowlist := []string{"app", "team"}
		if allowName {
			allowlist = append(allowlist, model.NameTagKey)
		}
		h := newHarness(t, fakeaws.ScenarioBadDeploy, WithOptions(func() Options { return Options{TagAllowlist: allowlist} }))
		mustSync(t, h, target(productionAccount))
		byARN := resourcesByARN(t, h, productionAccount)
		for _, want := range nameTagResources {
			t.Run(fmt.Sprintf("%s/name-allowlisted=%v", want.id, allowName), func(t *testing.T) {
				resource, ok := byARN[want.arn]
				if !ok {
					t.Fatalf("no resource %s", want.arn)
				}
				name, tag := want.id, model.RedactedValue
				if allowName {
					name, tag = want.name, want.name
				}
				if resource.Name != name || resource.Tags[model.NameTagKey] != tag {
					t.Fatalf("name = %q, Name tag = %q; want %q, %q", resource.Name, resource.Tags[model.NameTagKey], name, tag)
				}
			})
		}
	}
}

// Names stored while Name was allowlisted stop showing as soon as the
// allowlist narrows: every read applies the live allowlist again, without
// waiting for the next sync.
func TestStoredNameTagValuesHideWhenTheAllowlistNarrows(t *testing.T) {
	allowlist := []string{model.NameTagKey, "app"}
	fake := fakeaws.New(t, fakeaws.ScenarioBadDeploy)
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope(),
		snapshot.WithTagAllowlist(func() []string { return allowlist }))
	collector := New(store, limit.NewManager(1000), WithHTTPClient(fake.HTTPClient()), WithSleep(noSleep), WithRetries(0),
		WithOptions(func() Options { return Options{TagAllowlist: allowlist} }))
	h := harness{fake: fake, store: store, collector: collector}
	mustSync(t, h, target(productionAccount))
	if got := resourcesByARN(t, h, productionAccount)[nameTagResources[0].arn].Name; got != "versus-web" {
		t.Fatalf("allowlisted name = %q", got)
	}

	allowlist = []string{"app"}
	byARN := resourcesByARN(t, h, productionAccount)
	for _, want := range nameTagResources {
		resource := byARN[want.arn]
		if resource.Name != want.id || resource.Tags[model.NameTagKey] != model.RedactedValue {
			t.Errorf("%s after narrowing: name = %q, Name tag = %q", want.id, resource.Name, resource.Tags[model.NameTagKey])
		}
	}
	instance := byARN[nameTagResources[0].arn]
	if instance.Tags["app"] != fakeaws.WorldApplication {
		t.Fatalf("allowlisted tags must stay: %v", instance.Tags)
	}
	got, _, ok, err := store.Resource(context.Background(), nameTagResources[0].arn, time.Time{})
	if err != nil || !ok || got.Name != nameTagResources[0].id {
		t.Fatalf("single read after narrowing: %q %v %v", got.Name, ok, err)
	}
}

// The allowlist reaches the collector and the read path through the
// settings sanitiser. Tag keys are case-sensitive: "Name" there shows the
// Name tag as the resource name, "name" does not.
func TestNameAllowlistedThroughSettingsIsCaseSensitive(t *testing.T) {
	for _, c := range []struct {
		key, want string
	}{
		{"Name", "versus-web"},
		{"name", nameTagResources[0].id},
	} {
		provider := storage.NewMemory()
		configured := config.AWSConnectorConfig{Enable: true, Tags: config.AWSTagsConfig{ValueAllowlist: []string{" " + c.key + " ", "app"}}}
		service := settings.NewService(settings.NewStore(provider, tenancy.DefaultOrgScope()),
			settings.WithConfig(func() config.AWSConnectorConfig { return configured }))
		fake := fakeaws.New(t, fakeaws.ScenarioBadDeploy)
		store := snapshot.New(provider, tenancy.DefaultOrgScope(), snapshot.WithTagAllowlist(service.TagValueAllowlist))
		collector := New(store, limit.NewManager(1000), WithHTTPClient(fake.HTTPClient()), WithSleep(noSleep), WithRetries(0),
			WithOptions(func() Options { return Options{TagAllowlist: service.TagValueAllowlist()} }))
		h := harness{fake: fake, store: store, collector: collector}
		mustSync(t, h, target(productionAccount))
		if got := resourcesByARN(t, h, productionAccount)[nameTagResources[0].arn].Name; got != c.want {
			t.Errorf("allowlist key %q: instance name = %q, want %q", c.key, got, c.want)
		}
	}
}

// Expired temporary credentials are a credentials cause on every unit
// (no_server_identity, as the health check names it), never "network", and
// the units keep their last good data.
func TestExpiredCredentialsAreNamedAsCredentialsNotNetwork(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	mustSync(t, h, target(productionAccount))
	h.fake.SetExpiredToken(true)
	report := mustSync(t, h, target(productionAccount))
	if report.Failed() == 0 {
		t.Fatal("expired credentials did not fail any unit")
	}
	for _, unit := range report.Units {
		if unit.Outcome == OutcomeCommitted {
			continue
		}
		if unit.ErrorCode != awsauth.ErrNoServerIdentity.Error() {
			t.Errorf("%s/%s failed with %q, want %q", unit.Unit.Service, unit.Unit.Region, unit.ErrorCode, awsauth.ErrNoServerIdentity.Error())
		}
	}
}

// An application read from a tag is that tag's value: it shows only while
// the tag key it came from is on the tag-value allowlist (exact,
// case-sensitive key). The read applies the live allowlist again, and a
// value collected while its key was not allowlisted is never stored.
func TestTagApplicationShowsOnlyWhenItsKeyIsAllowlisted(t *testing.T) {
	table := arnOf("dynamodb", "table/versus-locks")
	allowlist := []string{"app"}
	fake := fakeaws.New(t, fakeaws.ScenarioBadDeploy)
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope(),
		snapshot.WithTagAllowlist(func() []string { return allowlist }))
	collector := New(store, limit.NewManager(1000), WithHTTPClient(fake.HTTPClient()), WithSleep(noSleep), WithRetries(0),
		WithOptions(func() Options { return Options{TagAllowlist: allowlist} }))
	h := harness{fake: fake, store: store, collector: collector}
	mustSync(t, h, target(productionAccount))
	if got := resourcesByARN(t, h, productionAccount)[table]; got.App != "platform" || got.AppSource != snapshot.AppSourceTags || got.AppTag != "app" {
		t.Fatalf("allowlisted tag application = %q from %q (%q)", got.App, got.AppSource, got.AppTag)
	}
	for _, narrowed := range [][]string{{"team"}, {"App"}} {
		allowlist = narrowed
		if got := resourcesByARN(t, h, productionAccount)[table]; got.App != "" || got.AppSource != "" {
			t.Errorf("allowlist %v: tag application still shown: %q from %q", narrowed, got.App, got.AppSource)
		}
	}

	allowlist = []string{"team"}
	fresh := fakeaws.New(t, fakeaws.ScenarioBadDeploy)
	unlisted := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope())
	h = harness{fake: fresh, store: unlisted, collector: New(unlisted, limit.NewManager(1000), WithHTTPClient(fresh.HTTPClient()),
		WithSleep(noSleep), WithRetries(0), WithOptions(func() Options { return Options{TagAllowlist: []string{"team"}} }))}
	mustSync(t, h, target(productionAccount))
	if got := resourcesByARN(t, h, productionAccount)[table]; got.App != "" {
		t.Fatalf("a tag application whose key is not allowlisted was stored: %q", got.App)
	}
}
