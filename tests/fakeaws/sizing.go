package fakeaws

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The sizing scenario: one large account shaped like the sizing example the
// cost plan prices, so a day of collection can be checked against it. About
// 1,250 resources (IAM identities aside) in three Regions, 19 target groups
// behind load balancers, fifteen Regions that are not turned on, a busy
// CloudTrail, and a steady trickle of deploys. Everything is healthy: the
// example prices a quiet day.
//
// The primary Region runs the web tier: the load balancer and every target
// group, the ECS services, the database, the stack and the alarms. Each
// Region, the primary included, holds SizingLoadPerRegion generated
// resources: 92 instances (with the world's four, one page), 70 functions,
// 70 queues, 34 tables and 34 topics, plus SizingKeysPerRegion KMS keys.
// Global: SizingBuckets buckets and SizingRoles roles. AWS Health answers
// that the account's support plan lacks it.
const ScenarioSizingLarge = "sizing-large"

// SizingPrimaryRegion runs the web tier.
const SizingPrimaryRegion = "us-east-1"

// Sizing account shape.
const (
	SizingLoadPerRegion = 300
	SizingInstances     = 92
	SizingFunctions     = 70
	SizingQueues        = 70
	SizingTables        = 34
	SizingTargetGroups  = 19
	SizingBuckets       = 100
	SizingKeysPerRegion = 20
	SizingRoles         = 80
	// The primary Region's SSM agents check in twice a minute, the
	// others once (counted by the change poller, not stored); a function
	// is deployed every SizingDeployEvery and a queue changed every
	// SizingQueueEditEvery in each Region.
	SizingPrimaryRoutineEvery = 30 * time.Second
	SizingRoutineEvery        = time.Minute
	SizingDeployEvery         = 30 * time.Minute
	SizingQueueEditEvery      = 2 * time.Hour
)

// SizingRegions are the Regions that hold the account's resources.
func SizingRegions() []string { return []string{"us-east-1", "eu-west-1", "ap-southeast-1"} }

// SizingOffRegions are fifteen Regions that are not turned on.
func SizingOffRegions() []string {
	return []string{"af-south-1", "ap-east-1", "ap-south-2", "ap-southeast-3", "ap-southeast-4", "ca-west-1", "eu-central-2",
		"eu-south-1", "eu-south-2", "il-central-1", "me-central-1", "me-south-1", "mx-central-1", "ap-southeast-5", "ap-southeast-7"}
}

func init() {
	knownScenarios[ScenarioSizingLarge] = true
	worldScenarios[ScenarioSizingLarge] = true
}

func sizingOff(region string) bool {
	for _, off := range SizingOffRegions() {
		if off == region {
			return true
		}
	}
	return false
}

// sizingHandlers answer the operations the sizing account shapes beyond
// the collector world.
var sizingHandlers = map[string]worldHandler{
	"elasticloadbalancing:DescribeTargetGroups": (*Server).sizingTargetGroups,
	"s3:ListBuckets":        (*Server).sizingBuckets,
	"kms:ListKeys":          (*Server).sizingKeys,
	"kms:GetKeyPolicy":      (*Server).sizingKeyPolicy,
	"iam:ListRoles":         (*Server).sizingRoles,
	"health:DescribeEvents": (*Server).sizingHealthEvents,
	"elasticloadbalancing:DescribeLoadBalancers": sizingPrimaryOnly((*Server).worldLoadBalancers, func(writer http.ResponseWriter, _ awsRequest) {
		writeQuery(writer, "DescribeLoadBalancers", members("LoadBalancers", nil))
	}),
	"ecs:ListClusters": sizingPrimaryOnly((*Server).worldECSClusters, func(writer http.ResponseWriter, _ awsRequest) {
		writeJSONBody(writer, `{"clusterArns":[]}`)
	}),
	"rds:DescribeDBInstances": sizingPrimaryOnly((*Server).worldDBInstances, func(writer http.ResponseWriter, _ awsRequest) {
		writeQuery(writer, "DescribeDBInstances", `<DBInstances></DBInstances>`)
	}),
	"cloudformation:DescribeStacks": sizingPrimaryOnly((*Server).worldStacks, func(writer http.ResponseWriter, _ awsRequest) {
		writeQuery(writer, "DescribeStacks", members("Stacks", nil))
	}),
	"cloudwatch:DescribeAlarms": sizingPrimaryOnly((*Server).worldAlarms, func(writer http.ResponseWriter, _ awsRequest) {
		writeCBORBody(writer, http.StatusOK, map[string]any{"MetricAlarms": []any{}, "CompositeAlarms": []any{}})
	}),
}

// sizingTargetGroupsIn is the Region's share of the target groups: all in
// the primary Region.
func sizingTargetGroupsIn(region string) int {
	if region == SizingPrimaryRegion {
		return SizingTargetGroups
	}
	return 0
}

func sizingResources(region string) bool {
	for _, name := range SizingRegions() {
		if name == region {
			return true
		}
	}
	return false
}

// sizingPrimaryOnly answers an operation from the world in the primary
// Region and with nothing elsewhere.
func sizingPrimaryOnly(primary worldHandler, empty func(http.ResponseWriter, awsRequest)) worldHandler {
	return func(s *Server, writer http.ResponseWriter, request awsRequest, w world) {
		if w.region == SizingPrimaryRegion {
			primary(s, writer, request, w)
			return
		}
		empty(writer, request)
	}
}

func (s *Server) sizingTargetGroups(writer http.ResponseWriter, _ awsRequest, w world) {
	groups := make([]string, 0, SizingTargetGroups)
	for index := 0; index < sizingTargetGroupsIn(w.region); index++ {
		name := fmt.Sprintf("web-%02d", index)
		groups = append(groups, xmlText("TargetGroupArn", w.arn("elasticloadbalancing", "targetgroup/"+name+"/0123456789abcdef"))+
			xmlText("TargetGroupName", name)+xmlText("VpcId", WorldVPC)+xmlText("TargetType", "instance")+xmlText("Protocol", "HTTP")+
			xmlText("Port", "80")+members("LoadBalancerArns", []string{xmlEscape(w.loadBalancerARN())}))
	}
	writeQuery(writer, "DescribeTargetGroups", members("TargetGroups", groups))
}

func (s *Server) sizingBuckets(writer http.ResponseWriter, _ awsRequest, _ world) {
	var buckets strings.Builder
	for index := 0; index < SizingBuckets; index++ {
		buckets.WriteString(`<Bucket>` + xmlText("Name", fmt.Sprintf("sizing-bucket-%03d", index)) +
			xmlText("CreationDate", "2026-01-01T00:00:00.000Z") + xmlText("BucketRegion", "us-east-1") + `</Bucket>`)
	}
	writeRESTXMLBody(writer, `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>versus</ID></Owner><Buckets>`+
		buckets.String()+`</Buckets></ListAllMyBucketsResult>`)
}

func (s *Server) sizingKeys(writer http.ResponseWriter, _ awsRequest, w world) {
	keys := make([]map[string]any, 0, SizingKeysPerRegion)
	for index := 0; index < SizingKeysPerRegion; index++ {
		id := fmt.Sprintf("0000%04d-1111-2222-3333-444455556666", index)
		keys = append(keys, map[string]any{"KeyId": id, "KeyArn": w.arn("kms", "key/"+id)})
	}
	writeJSONBody(writer, mustJSON(map[string]any{"Keys": keys, "Truncated": false}))
}

func (s *Server) sizingKeyPolicy(writer http.ResponseWriter, _ awsRequest, w world) {
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` + w.account +
		`:root"},"Action":"kms:*","Resource":"*"}]}`
	writeJSONBody(writer, mustJSON(map[string]any{"Policy": policy, "PolicyName": "default"}))
}

func (s *Server) sizingRoles(writer http.ResponseWriter, request awsRequest, w world) {
	names := make([]string, 0, SizingRoles)
	for index := 0; index < SizingRoles; index++ {
		names = append(names, fmt.Sprintf("service-role-%03d", index))
	}
	page, next := pageOf(names, request.params.Get("Marker"), queryInt(request, "MaxItems"))
	roles := make([]string, 0, len(page))
	for _, name := range page {
		roles = append(roles, xmlText("RoleName", name)+xmlText("Path", "/")+xmlText("RoleId", "AROASIZING"+strings.ToUpper(name[len(name)-3:]))+
			xmlText("Arn", "arn:aws:iam::"+w.account+":role/"+name)+xmlText("CreateDate", iso(w.now.Add(-90*24*time.Hour))))
	}
	tail := xmlText("IsTruncated", strconv.FormatBool(next != ""))
	if next != "" {
		tail += xmlText("Marker", next)
	}
	writeQuery(writer, "ListRoles", members("Roles", roles)+tail)
}

func (s *Server) sizingHealthEvents(writer http.ResponseWriter, request awsRequest, _ world) {
	writeErrorFor(writer, request, http.StatusBadRequest, "SubscriptionRequiredException",
		"The AWS Health API requires a Business, Enterprise On-Ramp or Enterprise support plan.")
}

// sizingTrail is the account's CloudTrail in [from, to) for one Region, on
// absolute time boundaries so every read sees the same events: an SSM
// agent check-in every SizingRoutineEvery, or SizingPrimaryRoutineEvery in
// the primary Region (counted, not stored), a Lambda
// deploy every SizingDeployEvery and a queue change every
// SizingQueueEditEvery. A Region without resources has none.
func sizingTrail(w world, from, to time.Time) []trailEvent {
	if !sizingResources(w.region) {
		return nil
	}
	identity := map[string]any{"userIdentity": map[string]any{"type": "AssumedRole",
		"arn": "arn:aws:sts::" + w.account + ":assumed-role/deployer/ci", "sessionContext": map[string]any{"sessionIssuer": map[string]any{
			"type": "Role", "arn": "arn:aws:iam::" + w.account + ":role/deployer"}}}}
	var events []trailEvent
	add := func(every time.Duration, name, source string, resource func(time.Time) string) {
		for at := from.Truncate(every); at.Before(to); at = at.Add(every) {
			if at.Before(from) {
				continue
			}
			event := trailEvent{id: fmt.Sprintf("sizing-%s-%s-%d", w.region, name, at.Unix()), exactID: true, name: name, source: source,
				at: at, document: identity}
			if resource != nil {
				event.resources = []string{resource(at)}
			}
			events = append(events, event)
		}
	}
	routine := SizingRoutineEvery
	if w.region == SizingPrimaryRegion {
		routine = SizingPrimaryRoutineEvery
	}
	add(routine, "UpdateInstanceInformation", "ssm.amazonaws.com", nil)
	add(SizingDeployEvery, "UpdateFunctionCode20150331v2", "lambda.amazonaws.com", func(at time.Time) string {
		return fmt.Sprintf("load-fn-%05d", at.Unix()/int64(SizingDeployEvery/time.Second)%int64(SizingFunctions))
	})
	add(SizingQueueEditEvery, "SetQueueAttributes", "sqs.amazonaws.com", func(time.Time) string { return WorldQueue })
	// Newest first, as LookupEvents answers.
	sort.Slice(events, func(left, right int) bool { return events[left].at.After(events[right].at) })
	return events
}
