//go:build awsemulator

package awsemulator

import (
	"slices"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	"github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// An account listed in connectors.yaml with default_chain credentials
// connects: STS names the account, the identity matches it, and the
// permission probes read every service the seed populated without a denial.
func TestAccountCheckIdentityAndProbes(t *testing.T) {
	w := seeded(t)
	if w.identityID != emulatorAccount {
		t.Errorf("caller identity account = %q, want %q", w.identityID, emulatorAccount)
	}
	if !strings.HasPrefix(w.identityARN, "arn:aws:iam::"+emulatorAccount+":") {
		t.Errorf("caller identity ARN = %q, want an IAM ARN in account %s", w.identityARN, emulatorAccount)
	}
	if w.health.ErrorCode != "" || w.health.ConsecutiveFailures != 0 {
		t.Errorf("account check recorded error %q after %d failures", w.health.ErrorCode, w.health.ConsecutiveFailures)
	}
	results := make(map[string]probe.Result, len(w.health.ProbeResults))
	for _, result := range w.health.ProbeResults {
		results[result.Service] = result
	}
	readable := []string{"cloudformation", "cloudtrail", "cloudwatch", "dynamodb", "ec2", "ecs", "eks",
		"elbv2", "iam", "lambda", "rds", "s3", "sns", "sqs"}
	for _, service := range readable {
		result, ok := results[service]
		switch {
		case !ok:
			t.Errorf("probe %s: no result", service)
		case result.Status != probe.StatusReadable || result.Items < 1:
			t.Errorf("probe %s: status %q, %d items, error %q; want readable with the seeded items", service, result.Status, result.Items, result.ErrorCode)
		}
	}
	// Seeded nothing: the probe must answer "not used", never a failure.
	for _, service := range []string{"api-gateway", "auto-scaling", "cloudfront", "config", "elasticache", "eventbridge", "kinesis", "route53", "step-functions"} {
		if result := results[service]; result.Status != probe.StatusNotUsed {
			t.Errorf("probe %s: status %q, error %q; want not_used", service, result.Status, result.ErrorCode)
		}
	}
	counts := settings.CountResults(w.health.ProbeResults)
	if len(counts.DeniedServices) != 0 || len(counts.NotOnPlanServices) != 0 {
		t.Errorf("probe grid: denied %v, not on plan %v; want none", counts.DeniedServices, counts.NotOnPlanServices)
	}
}

// With no tags configured, the account uses the shipped tag-value allowlist.
func TestDefaultTagValueAllowlistApplies(t *testing.T) {
	w := seeded(t)
	if want := settings.DefaultTagValueAllowlist(); !slices.Equal(w.allowlist, want) {
		t.Fatalf("effective allowlist = %v, want the default %v", w.allowlist, want)
	}
}

// Every unit whose API the emulator serves faithfully commits on the first
// sync. AppRegistry and RAM are the known failures (see
// TestAppRegistryApplications and TestRAMResourceShares).
func TestEveryFaithfulUnitCommits(t *testing.T) {
	w := seeded(t)
	t.Logf("first sync of %d units took %s", len(w.report.Units), w.syncTime)
	known := map[string]bool{snapshot.AppRegistryService: true, ramService: true}
	for _, unit := range w.report.Units {
		if known[unit.Unit.Service] {
			continue
		}
		if unit.Outcome != collect.OutcomeCommitted {
			t.Errorf("%s/%s: outcome %q, error %q; want committed", unit.Unit.Service, unit.Unit.Region, unit.Outcome, unit.ErrorCode)
		}
	}
	for _, service := range collect.Services() {
		found := false
		for _, unit := range w.report.Units {
			found = found || unit.Unit.Service == service
		}
		if !found {
			t.Errorf("unit %s was not run", service)
		}
	}
}

// EC2 inventory: the two tagged instances keep their IDs, type, zone, VPC,
// subnet and security group; allowlisted tag values are kept and the Name
// and contact values are stored redacted, so the display name falls back to
// the instance ID. The 1024-instance fleet makes DescribeInstances page.
func TestEC2InstancesTagsAndRedaction(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "ec2")
	w.requireNoGaps(t, "ec2")
	vpc := w.tagged(t, "vpc", "vpc", "team", "platform")
	group := w.named(t, "vpc", "security-group", "contract-web")

	cases := []struct {
		app, team, size string
		contact         bool
	}{
		{app: "checkout", team: "payments", size: "t3.micro", contact: true},
		{app: "billing", team: "finance", size: "m5.large"},
	}
	for _, want := range cases {
		instance := w.tagged(t, "ec2", "instance", "app", want.app)
		id := instance.Attributes[model.AttrID]
		if !strings.HasPrefix(id, "i-") || instance.ARN != "arn:aws:ec2:"+testRegion+":"+emulatorAccount+":instance/"+id {
			t.Errorf("%s instance: id %q, ARN %q", want.app, id, instance.ARN)
		}
		if instance.Name != id {
			t.Errorf("%s instance: name %q, want the instance ID %q (Name tag is not allowlisted)", want.app, instance.Name, id)
		}
		if instance.State != "running" {
			t.Errorf("%s instance: state %q, want running", want.app, instance.State)
		}
		if instance.Attributes[model.AttrSize] != want.size || instance.Attributes[model.AttrZone] != "us-east-1a" {
			t.Errorf("%s instance: size %q zone %q, want %s in us-east-1a", want.app, instance.Attributes[model.AttrSize], instance.Attributes[model.AttrZone], want.size)
		}
		if instance.Attributes[model.AttrVPC] != vpc.Attributes[model.AttrID] || !hasRelation(instance, model.RelationInVPC, vpc.ARN) {
			t.Errorf("%s instance: not in the seeded VPC %s (attributes %v)", want.app, vpc.ARN, instance.Attributes)
		}
		if !hasRelation(instance, model.RelationSecurityGroup, group.ARN) {
			t.Errorf("%s instance: no security-group relation to %s", want.app, group.ARN)
		}
		w.requireRelationResolves(t, instance, model.RelationInSubnet)
		tags := map[string]string{"app": want.app, "team": want.team, model.NameTagKey: redacted}
		if want.contact {
			tags["contact"] = redacted
		}
		requireTags(t, instance, tags)
		if instance.App != want.app || instance.AppSource != snapshot.AppSourceTags || instance.AppTag != "app" {
			t.Errorf("%s instance: application %q from %q/%q, want %q from the app tag", want.app, instance.App, instance.AppSource, instance.AppTag, want.app)
		}
	}

	fleet := 0
	for _, instance := range w.ofType("ec2", "instance") {
		if instance.Tags["env"] == "fleet" {
			fleet++
		}
	}
	if fleet != fleetInstances {
		t.Errorf("fleet instances stored = %d, want %d (DescribeInstances must follow NextToken past 1000)", fleet, fleetInstances)
	}
}

// EC2 network: VPC, both subnets, the route table with its IGW route and
// subnet association, the attached IGW, the NAT gateway and the instance
// network interfaces, all linked by the IDs the other calls return.
func TestVPCNetworkTopology(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "vpc")
	w.requireNoGaps(t, "vpc")

	vpc := w.tagged(t, "vpc", "vpc", "team", "platform")
	vpcID := vpc.Attributes[model.AttrID]
	if !strings.HasPrefix(vpcID, "vpc-") || vpc.Name != vpcID || vpc.State != "available" {
		t.Errorf("vpc: id %q name %q state %q", vpcID, vpc.Name, vpc.State)
	}
	requireTags(t, vpc, map[string]string{"team": "platform", model.NameTagKey: redacted})

	var subnets []model.Resource
	for _, subnet := range w.ofType("vpc", "subnet") {
		if hasRelation(subnet, model.RelationInVPC, vpc.ARN) {
			subnets = append(subnets, subnet)
		}
	}
	zones := map[string]bool{}
	for _, subnet := range subnets {
		zones[subnet.Attributes[model.AttrZone]] = true
		requireTags(t, subnet, map[string]string{"env": "test", model.NameTagKey: redacted})
	}
	if len(subnets) != 2 || !zones["us-east-1a"] || !zones["us-east-1b"] {
		t.Errorf("subnets in the seeded VPC: %d in zones %v, want 2 in us-east-1a and us-east-1b", len(subnets), zones)
	}

	var igw model.Resource
	for _, gateway := range w.ofType("vpc", "internet-gateway") {
		if hasRelation(gateway, model.RelationAttachedTo, vpc.ARN) {
			igw = gateway
		}
	}
	if igw.ARN == "" {
		t.Fatalf("no internet gateway attached to %s", vpc.ARN)
	}
	requireTags(t, igw, map[string]string{model.NameTagKey: redacted})

	routed := false
	for _, table := range w.ofType("vpc", "route-table") {
		if !hasRelation(table, model.RelationRoutesTo, igw.ARN) {
			continue
		}
		routed = true
		if !hasRelation(table, model.RelationInVPC, vpc.ARN) {
			t.Errorf("route table %s: not in the seeded VPC", table.ARN)
		}
		w.requireRelationResolves(t, table, model.RelationAttachedTo)
	}
	if !routed {
		t.Errorf("no route table routes to %s", igw.ARN)
	}

	nats := 0
	for _, nat := range w.ofType("vpc", "nat-gateway") {
		if !hasRelation(nat, model.RelationInVPC, vpc.ARN) {
			continue
		}
		nats++
		if nat.State != "available" {
			t.Errorf("nat gateway %s: state %q, want available", nat.ARN, nat.State)
		}
		w.requireRelationResolves(t, nat, model.RelationInSubnet)
	}
	if nats != 1 {
		t.Errorf("nat gateways in the seeded VPC = %d, want 1", nats)
	}

	web := w.tagged(t, "ec2", "instance", "app", "checkout")
	attached := 0
	for _, eni := range w.ofType("vpc", "network-interface") {
		if hasRelation(eni, model.RelationAttachedTo, web.ARN) {
			attached++
			w.requireRelationResolves(t, eni, model.RelationInSubnet)
		}
	}
	if attached != 1 {
		t.Errorf("network interfaces attached to the web instance = %d, want 1", attached)
	}
	if enis := len(w.ofType("vpc", "network-interface")); enis < fleetInstances+2 {
		t.Errorf("network interfaces stored = %d, want at least %d (DescribeNetworkInterfaces must page)", enis, fleetInstances+2)
	}
}

// CloudFormation: the stack, its status and tags, and the queue and topic it
// owns, as physical IDs that match what the SQS and SNS collectors store.
func TestCloudFormationStack(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "cloudformation")
	w.requireNoGaps(t, "cloudformation")
	stack := w.named(t, "cloudformation", "stack", "contract-stack")
	if !strings.HasPrefix(stack.ARN, "arn:aws:cloudformation:"+testRegion+":"+emulatorAccount+":stack/contract-stack/") {
		t.Errorf("stack ARN = %q", stack.ARN)
	}
	if stack.State != "create_complete" {
		t.Errorf("stack state = %q, want create_complete", stack.State)
	}
	requireTags(t, stack, map[string]string{"app": "checkout", "contact": redacted})

	members := relationTargets(stack, model.RelationAssociated)
	if len(members) != 2 {
		t.Fatalf("stack members = %v, want the queue and the topic", members)
	}
	queues, topics := w.ofType("sqs", "queue"), w.ofType("sns", "topic")
	for _, member := range members {
		found := false
		for _, resource := range append(queues, topics...) {
			found = found || resource.Name == member || resource.ARN == member
		}
		if !found {
			t.Errorf("stack member %q is not a stored queue or topic", member)
		}
	}
}

// CloudTrail: the seed's write calls come back as normalized changes. Read
// events never do, RunInstances renders its template with the instance ID,
// and more than one LookupEvents page is read.
func TestCloudTrailChanges(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, snapshot.ChangeService)
	events := map[string]int{}
	for _, change := range w.changes.Changes {
		events[change.EventName]++
		if strings.HasPrefix(change.EventName, "Describe") || strings.HasPrefix(change.EventName, "List") || strings.HasPrefix(change.EventName, "Get") {
			t.Errorf("read-only event stored as a change: %s %s", change.EventSource, change.EventName)
		}
		// Global services (IAM, STS and the rest) are stored under the
		// global pseudo-Region; every other service keeps its own Region.
		wantRegion := testRegion
		if collect.IsGlobalSource(change.EventSource) {
			wantRegion = model.GlobalRegion
		}
		if change.Account != emulatorAccount || change.Region != wantRegion || change.ID == "" || change.Time.IsZero() {
			t.Errorf("change %s: account %q region %q id %q time %s", change.EventName, change.Account, change.Region, change.ID, change.Time)
		}
	}
	// The emulator records CloudTrail events for EC2, ELBv2, RDS,
	// CloudFormation, SQS and IAM writes only; CloudWatch, SNS, S3, DynamoDB,
	// Lambda, ECS and EKS writes leave no event there (see the fidelity
	// record), so only the recorded services are asserted.
	for _, name := range []string{"CreateVpc", "CreateSubnet", "RunInstances", "AuthorizeSecurityGroupIngress",
		"CreateStack", "CreateDBInstance", "CreateLoadBalancer", "RegisterTargets", "CreateQueue", "CreateRole"} {
		if events[name] == 0 {
			t.Errorf("no %s change stored (stored %v)", name, events)
		}
	}
	if total := len(w.changes.Changes); total <= 50 {
		t.Errorf("changes stored = %d, want more than one 50-event LookupEvents page", total)
	}
	if events["RunInstances"] != 2+fleetInstances/64 {
		t.Errorf("RunInstances changes = %d, want one per seed call (%d)", events["RunInstances"], 2+fleetInstances/64)
	}

	// Request fields: each inbound-rule change names the seeded group.
	group := w.named(t, "vpc", "security-group", "contract-web")
	groupID := group.Attributes[model.AttrID]
	rules := 0
	for _, change := range w.changes.Changes {
		if change.EventName != "AuthorizeSecurityGroupIngress" {
			continue
		}
		rules++
		if change.Template != "ec2.authorize_ingress" || !slices.Contains(change.Resources, model.ARN("aws", "ec2", testRegion, emulatorAccount, "security-group/"+groupID)) || !strings.Contains(change.Summary, groupID) {
			t.Errorf("inbound rule change: template %q resources %v summary %q, want the group %s", change.Template, change.Resources, change.Summary, groupID)
		}
	}
	if rules != 12 {
		t.Errorf("inbound rule changes = %d, want 12", rules)
	}

	// Response fields: the RunInstances change names the launched instance.
	t.Run("RunInstancesNamesLaunchedInstance", func(t *testing.T) {
		web := w.tagged(t, "ec2", "instance", "app", "checkout")
		webID := web.Attributes[model.AttrID]
		found := false
		for _, change := range w.changes.Changes {
			if change.EventName != "RunInstances" {
				continue
			}
			if change.Template != "ec2.run_instances" {
				t.Errorf("RunInstances change: template %q, want ec2.run_instances", change.Template)
			}
			if !slices.Contains(change.Resources, model.ARN("aws", "ec2", testRegion, emulatorAccount, "instance/"+webID)) {
				continue
			}
			found = true
			if !strings.Contains(change.Summary, webID) {
				t.Errorf("RunInstances change for %s: summary %q", webID, change.Summary)
			}
		}
		if !found {
			t.Skip("the emulator's CloudTrail RunInstances event carries responseElements.instances[] (the SDK shape), not responseElements.instancesSet.items[] as AWS CloudTrail does (upstream defect), so no change can name the instance; tests/fakeaws covers the AWS shape")
		}
	})
}

// CloudTrail paging: LookupEvents pages are read to the end without a
// repeat or a skip, every stored resource is a canonical ARN (short names
// are kept apart as unresolved).
func TestCloudTrailPagingAndCanonicalResources(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, snapshot.ChangeService)
	seen := map[string]bool{}
	for _, change := range w.changes.Changes {
		if seen[change.ID] {
			t.Errorf("event %s stored twice across pages", change.ID)
		}
		seen[change.ID] = true
		for _, resource := range change.Resources {
			if !strings.HasPrefix(resource, "arn:aws:") {
				t.Errorf("change %s %s stores the short name %q as a resource", change.EventName, change.ID, resource)
			}
		}
	}
	if len(seen) <= 50 {
		t.Errorf("changes stored = %d, want more than one 50-event page", len(seen))
	}
}

// ELBv2: the ALB in both subnets of the seeded VPC routes to the target
// group, which routes to the registered web instance.
func TestELBv2LoadBalancerAndTargets(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "elbv2")
	w.requireNoGaps(t, "elbv2")
	vpc := w.tagged(t, "vpc", "vpc", "team", "platform")
	balancer := w.named(t, "elbv2", "load-balancer", "contract-alb")
	group := w.named(t, "elbv2", "target-group", "contract-tg")
	web := w.tagged(t, "ec2", "instance", "app", "checkout")

	if !strings.HasPrefix(balancer.ARN, "arn:aws:elasticloadbalancing:"+testRegion+":"+emulatorAccount+":loadbalancer/app/contract-alb/") {
		t.Errorf("load balancer ARN = %q", balancer.ARN)
	}
	if balancer.State != "active" || balancer.Attributes[model.AttrDNSName] == "" {
		t.Errorf("load balancer: state %q, DNS %q", balancer.State, balancer.Attributes[model.AttrDNSName])
	}
	if !hasRelation(balancer, model.RelationInVPC, vpc.ARN) || len(relationTargets(balancer, model.RelationInSubnet)) != 2 {
		t.Errorf("load balancer: relations %v, want the seeded VPC and both subnets", balancer.Relations)
	}
	w.requireRelationResolves(t, balancer, model.RelationInSubnet)
	if !hasRelation(balancer, model.RelationRoutesTo, group.ARN) {
		t.Errorf("load balancer does not route to %s (relations %v)", group.ARN, balancer.Relations)
	}
	if !hasRelation(group, model.RelationInVPC, vpc.ARN) {
		t.Errorf("target group not in the seeded VPC")
	}
	if !hasRelation(group, model.RelationRoutesTo, web.ARN) {
		t.Errorf("target group does not route to the web instance %s (relations %v)", web.ARN, group.Relations)
	}
}

// RDS: the database with its engine, class and tags.
func TestRDSDatabase(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "rds")
	db := w.named(t, "rds", "db-instance", "contract-db")
	if db.ARN != "arn:aws:rds:"+testRegion+":"+emulatorAccount+":db:contract-db" {
		t.Errorf("database ARN = %q", db.ARN)
	}
	if db.State != "available" || db.Attributes[model.AttrEngine] != "postgres" || db.Attributes[model.AttrSize] != "db.t3.micro" {
		t.Errorf("database: state %q attributes %v", db.State, db.Attributes)
	}
	requireTags(t, db, map[string]string{"app": "billing", "contact": redacted})
	if db.App != "billing" {
		t.Errorf("database application = %q, want billing", db.App)
	}
}

// EKS: the cluster with its version, VPC, subnets, security group and tags.
func TestEKSCluster(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "eks")
	w.requireNoGaps(t, "eks")
	vpc := w.tagged(t, "vpc", "vpc", "team", "platform")
	group := w.named(t, "vpc", "security-group", "contract-web")
	cluster := w.named(t, "eks", "cluster", "contract-eks")
	if cluster.ARN != "arn:aws:eks:"+testRegion+":"+emulatorAccount+":cluster/contract-eks" {
		t.Errorf("cluster ARN = %q", cluster.ARN)
	}
	if cluster.State != "active" || !strings.HasPrefix(cluster.Attributes[model.AttrEngine], "kubernetes 1.") {
		t.Errorf("cluster: state %q engine %q", cluster.State, cluster.Attributes[model.AttrEngine])
	}
	if !hasRelation(cluster, model.RelationInVPC, vpc.ARN) || !hasRelation(cluster, model.RelationSecurityGroup, group.ARN) {
		t.Errorf("cluster relations %v, want the seeded VPC and security group", cluster.Relations)
	}
	if subnets := relationTargets(cluster, model.RelationInSubnet); len(subnets) != 2 {
		t.Errorf("cluster subnets = %v, want 2", subnets)
	}
	w.requireRelationResolves(t, cluster, model.RelationInSubnet)
	requireTags(t, cluster, map[string]string{"app": "checkout", "contact": redacted})
}

// ECS: the cluster, read with its tags.
func TestECSCluster(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "ecs")
	w.requireNoGaps(t, "ecs")
	cluster := w.named(t, "ecs", "cluster", "contract-ecs")
	if cluster.ARN != "arn:aws:ecs:"+testRegion+":"+emulatorAccount+":cluster/contract-ecs" {
		t.Errorf("cluster ARN = %q", cluster.ARN)
	}
	if cluster.State != "active" {
		t.Errorf("cluster state = %q, want active", cluster.State)
	}
	requireTags(t, cluster, map[string]string{"app": "checkout", "contact": redacted})
}

// Lambda: the function with its runtime.
func TestLambdaFunction(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "lambda")
	w.requireNoGaps(t, "lambda")
	function := w.named(t, "lambda", "function", "contract-fn")
	if function.ARN != "arn:aws:lambda:"+testRegion+":"+emulatorAccount+":function:contract-fn" {
		t.Errorf("function ARN = %q", function.ARN)
	}
	if function.Attributes[model.AttrRuntime] != "python3.12" {
		t.Errorf("function runtime = %q, want python3.12", function.Attributes[model.AttrRuntime])
	}
}

// SQS, SNS and DynamoDB: the queues (seeded and stack-owned), topics and
// table, with ARNs in the emulator account.
func TestQueuesTopicsAndTables(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "sqs", "sns", "dynamodb")
	w.requireNoGaps(t, "sqs", "sns", "dynamodb")
	queue := w.named(t, "sqs", "queue", "contract-queue")
	if queue.ARN != "arn:aws:sqs:"+testRegion+":"+emulatorAccount+":contract-queue" {
		t.Errorf("queue ARN = %q", queue.ARN)
	}
	if queues := w.ofType("sqs", "queue"); len(queues) != 2 {
		t.Errorf("queues = %s, want the seeded queue and the stack queue", names(queues))
	}
	topic := w.named(t, "sns", "topic", "contract-topic")
	if topic.ARN != "arn:aws:sns:"+testRegion+":"+emulatorAccount+":contract-topic" {
		t.Errorf("topic ARN = %q", topic.ARN)
	}
	table := w.named(t, "dynamodb", "table", "contract-table")
	if table.ARN != "arn:aws:dynamodb:"+testRegion+":"+emulatorAccount+":table/contract-table" {
		t.Errorf("table ARN = %q", table.ARN)
	}
}

// S3: the bucket list, plus the three metadata reads. A bucket without a
// policy answers NoSuchBucketPolicy, which is not a gap, and the default
// encryption means no encryption flag.
func TestS3Buckets(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "s3")
	w.requireNoGaps(t, "s3")
	bucket := w.named(t, "s3", "bucket", "contract-bucket")
	if bucket.ARN != "arn:aws:s3:::contract-bucket" {
		t.Errorf("bucket ARN = %q", bucket.ARN)
	}
	if region := bucket.Attributes[model.AttrBucketRegion]; region != "" && region != testRegion {
		t.Errorf("bucket region = %q, want %s", region, testRegion)
	}
}

// IAM: the seeded role.
func TestIAMRoles(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "iam")
	role := w.named(t, "iam", "role", "contract-lambda-role")
	if role.ARN != "arn:aws:iam::"+emulatorAccount+":role/contract-lambda-role" {
		t.Errorf("role ARN = %q", role.ARN)
	}
}

// CloudWatch: the metric alarm and the instance it monitors.
func TestCloudWatchAlarms(t *testing.T) {
	w := seeded(t)
	w.requireCommitted(t, "cloudwatch")
	alarm := w.named(t, "cloudwatch", "alarm", "contract-web-cpu")
	if alarm.ARN != "arn:aws:cloudwatch:"+testRegion+":"+emulatorAccount+":alarm:contract-web-cpu" {
		t.Errorf("alarm ARN = %q", alarm.ARN)
	}
	if alarm.State == "" {
		t.Errorf("alarm has no state")
	}
	web := w.tagged(t, "ec2", "instance", "app", "checkout")
	if !hasRelation(alarm, model.RelationMonitors, web.ARN) {
		t.Errorf("alarm does not monitor the web instance %s (relations %v)", web.ARN, alarm.Relations)
	}
}

// --- APIs the emulator cannot answer faithfully: covered by tests/fakeaws ---

func TestCostExplorer(t *testing.T) {
	requireEmulator(t)
	t.Skip("Cost Explorer: every operation Versus calls answers NotImplemented in the emulator; cost data stays on tests/fakeaws")
}

func TestCostOptimizationHub(t *testing.T) {
	requireEmulator(t)
	t.Skip("Cost Optimization Hub: ListRecommendations answers no items and the emulator has no way to create any; stays on tests/fakeaws")
}

func TestComputeOptimizer(t *testing.T) {
	requireEmulator(t)
	t.Skip("Compute Optimizer: the emulator returns three canned recommendations for fixed instance IDs, not the seeded instances; stays on tests/fakeaws")
}

func TestBudgetsSpend(t *testing.T) {
	requireEmulator(t)
	t.Skip("Budgets: DescribeBudgets lists the budget but CalculatedSpend is always empty; spend stays on tests/fakeaws")
}

func TestTaggingGetResources(t *testing.T) {
	requireEmulator(t)
	t.Skip("Tagging GetResources: the emulator indexes only SQS and SNS, never the tagged EC2 instances, VPC or Lambda (upstream defect); stays on tests/fakeaws")
}

func TestAppRegistryApplications(t *testing.T) {
	requireEmulator(t)
	t.Skip("AppRegistry: ListApplications sends creationTime as epoch seconds, which SDK Go v2 cannot decode, and ListAssociatedResources answers NotImplemented (upstream defects); stays on tests/fakeaws")
}

// ramService is the collector unit that reads resource shares.
const ramService = "ram"

func TestRAMResourceShares(t *testing.T) {
	requireEmulator(t)
	t.Skip("RAM: ListResources answers HTTP 501 NotImplemented (\"scaffolded but not implemented\") for every resource owner, so the unit cannot read the shares it owns or receives (upstream defect); GetResourceShareAssociations is served. Resource shares stay on tests/fakeaws")
}

func TestAWSHealthEvents(t *testing.T) {
	requireEmulator(t)
	t.Skip("AWS Health: the emulator returns three synthetic events to SDK Go v2 but SubscriptionRequiredException to the AWS CLI for the same key (upstream inconsistency); stays on tests/fakeaws")
}
