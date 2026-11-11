package fakeaws

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// The collector world: a small, coherent account that every collector
// operation answers from. Load balancer, target group, service, instances,
// alarms, stack and application all point at each other the way a real
// deployment does, so relations and the application resolver can be tested
// end to end.

// Collector scenarios. The probe scenarios keep their one-item probe pages;
// these answer every collector operation, including the probe ones, from
// the world.
const (
	ScenarioBadDeploy         = "bad-deploy"
	ScenarioHealthEvent       = "health-event"
	ScenarioAccessDeniedBurst = "access-denied-burst"
	ScenarioLoad5k            = "load-5k"
	// EnvLoadSize overrides how many extra resources load-5k generates, for
	// example 50000 for the large-account run.
	EnvLoadSize = "FAKEAWS_LOAD_SIZE"
	// DefaultLoadSize is the load-5k resource count.
	DefaultLoadSize = 5000
)

// World identifiers tests can assert on.
const (
	WorldVPC              = "vpc-0a1b2c3d"
	WorldSubnet           = "subnet-0a1b2c3d"
	WorldSecurityGroup    = "sg-0a1b2c3d"
	WorldAdminGroup       = "sg-0bad0ssh"
	WorldLoadBalancerID   = "app/versus/50dc6c495c0c9188"
	WorldTargetGroupID    = "targetgroup/versus-web/6d0ecf831eec9f09"
	WorldCluster          = "versus"
	WorldService          = "checkout"
	WorldTaskFamily       = "checkout"
	WorldGoodRevision     = 41
	WorldBadRevision      = 42
	WorldFunction         = "versus-worker"
	WorldQueue            = "versus-jobs"
	WorldStack            = "versus-stack"
	WorldApplication      = "checkout"
	WorldAlarm5xx         = "checkout-5xx"
	WorldAlarmCPU         = "versus-cpu"
	WorldCompositeAlarm   = "checkout-health"
	WorldPublicBucket     = "versus-public"
	WorldPrivateBucket    = "versus-logs"
	WorldHealthEventCode  = "AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED"
	WorldUnhealthyTargets = 3
	WorldTargets          = 4
	// WorldPersonalEmail is an SNS e-mail subscription endpoint that must
	// never be stored.
	WorldPersonalEmail = "oncall-person@example.com"
	// WorldRedactedTagValue is a tag value under a key outside the default
	// allowlist.
	WorldRedactedTagValue = "alice@example.com"
)

// serverPageSize caps every page the world serves so paging is exercised.
const serverPageSize = 100

var worldScenarios = map[string]bool{
	ScenarioBadDeploy:         true,
	ScenarioHealthEvent:       true,
	ScenarioAccessDeniedBurst: true,
	ScenarioLoad5k:            true,
}

// scenarioDenials are the services and single operations the
// access-denied-burst scenario refuses: whole units, and detail calls inside
// units whose list call still succeeds.
var scenarioDenials = map[string]bool{
	"lambda": true, "sqs": true, "sns": true, "events": true, "states": true, "kinesis": true, "tagging": true,
	"cloudformation:ListStackResources":         true,
	"elasticloadbalancing:DescribeTargetHealth": true,
	"s3:GetBucketEncryption":                    true,
	"servicecatalog:ListAssociatedResources":    true,
}

// world is the fixture state one request reads.
type world struct {
	account  string
	region   string
	scenario string
	now      time.Time
	load     int
	extra    []trailEvent
}

func (s *Server) worldFor(request awsRequest) world {
	load := 0
	if request.scenario == ScenarioLoad5k {
		load = s.LoadSize()
	}
	s.mu.Lock()
	extra := append([]trailEvent(nil), s.lateEvents...)
	s.mu.Unlock()
	return world{account: request.account.ID, region: request.region, scenario: request.scenario, now: s.clock(), load: load, extra: extra}
}

// AddLateEvent makes CloudTrail serve one more write event from now on,
// stamped at a past time: an event CloudTrail delivered late.
func (s *Server) AddLateEvent(id, name, source string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lateEvents = append(s.lateEvents, trailEvent{id: id, name: name, source: source, at: at.UTC().Truncate(time.Second),
		document: map[string]any{"userIdentity": map[string]any{"type": "IAMUser", "arn": "arn:aws:iam::111122223333:user/late"}}})
}

// SetLoadSize changes how many extra resources load-5k serves.
func (s *Server) SetLoadSize(size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadSize = size
}

// LoadSize is the number of extra resources load-5k serves: SetLoadSize,
// else FAKEAWS_LOAD_SIZE, else DefaultLoadSize.
func (s *Server) LoadSize() int {
	s.mu.Lock()
	size := s.loadSize
	s.mu.Unlock()
	if size > 0 {
		return size
	}
	if value, err := strconv.Atoi(os.Getenv(EnvLoadSize)); err == nil && value > 0 {
		return value
	}
	return DefaultLoadSize
}

// SetClock pins the time the world's events, alarms and rollouts are
// relative to.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

func (s *Server) clock() time.Time {
	s.mu.Lock()
	now := s.now
	s.mu.Unlock()
	if now != nil {
		return now().UTC()
	}
	return time.Now().UTC()
}

// BadDeployStart is when bad-deploy registers task definition :42, relative
// to now. The alarm and the unhealthy targets follow four minutes later.
func BadDeployStart(now time.Time) time.Time {
	return now.UTC().Add(-10 * time.Minute).Truncate(time.Second)
}

// share splits the load across services: instances, functions, queues,
// tables and topics.
func (w world) share(kind string) int {
	if w.load <= 0 {
		return 0
	}
	switch kind {
	case "instances":
		return w.load * 4 / 10
	case "functions", "queues":
		return w.load * 2 / 10
	default:
		return 0
	}
}

func (w world) tables() int { return (w.load - w.load*4/10 - 2*(w.load*2/10)) / 2 }

func (w world) topics() int {
	return w.load - w.load*4/10 - 2*(w.load*2/10) - w.tables()
}

func (w world) arn(service, resource string) string {
	return "arn:aws:" + service + ":" + w.region + ":" + w.account + ":" + resource
}

func (w world) loadBalancerARN() string {
	return w.arn("elasticloadbalancing", "loadbalancer/"+WorldLoadBalancerID)
}

func (w world) targetGroupARN() string { return w.arn("elasticloadbalancing", WorldTargetGroupID) }

func (w world) serviceARN() string {
	return w.arn("ecs", "service/"+WorldCluster+"/"+WorldService)
}

func (w world) clusterARN() string { return w.arn("ecs", "cluster/"+WorldCluster) }

func (w world) functionARN(name string) string { return w.arn("lambda", "function:"+name) }

func (w world) queueARN(name string) string { return w.arn("sqs", name) }

func (w world) queueURL(name string) string {
	return "https://sqs." + w.region + ".amazonaws.com/" + w.account + "/" + name
}

func (w world) stackARN() string {
	return w.arn("cloudformation", "stack/"+WorldStack+"/00000000-0000-0000-0000-00000000aaaa")
}

func (w world) badDeploy() bool { return w.scenario == ScenarioBadDeploy }

func (w world) revision() int {
	if w.badDeploy() {
		return WorldBadRevision
	}
	return WorldGoodRevision
}

func (w world) taskDefinitionARN() string {
	return w.arn("ecs", "task-definition/"+WorldTaskFamily+":"+strconv.Itoa(w.revision()))
}

// instanceIDs are the web instances behind the target group plus the load.
func (w world) instanceIDs() []string {
	ids := make([]string, 0, WorldTargets+w.share("instances"))
	for index := 0; index < WorldTargets; index++ {
		ids = append(ids, fmt.Sprintf("i-0123456789abcdef%d", index))
	}
	for index := 0; index < w.share("instances"); index++ {
		ids = append(ids, fmt.Sprintf("i-load%011d", index))
	}
	return ids
}

func (w world) functionNames() []string {
	names := []string{WorldFunction}
	for index := 0; index < w.share("functions"); index++ {
		names = append(names, fmt.Sprintf("load-fn-%05d", index))
	}
	return names
}

func (w world) queueNames() []string {
	names := []string{WorldQueue, WorldQueue + "-dlq"}
	for index := 0; index < w.share("queues"); index++ {
		names = append(names, fmt.Sprintf("load-queue-%05d", index))
	}
	return names
}

func (w world) tableNames() []string {
	names := []string{"versus-locks"}
	for index := 0; index < w.tables(); index++ {
		names = append(names, fmt.Sprintf("load-table-%05d", index))
	}
	return names
}

func (w world) topicNames() []string {
	names := []string{"versus-alarms"}
	for index := 0; index < w.topics(); index++ {
		names = append(names, fmt.Sprintf("load-topic-%05d", index))
	}
	return names
}

// pageOf returns one page of items from an offset token and the next token.
func pageOf(items []string, token string, requested int) ([]string, string) {
	size := serverPageSize
	if requested > 0 && requested < size {
		size = requested
	}
	start, err := strconv.Atoi(token)
	if err != nil || start < 0 || start > len(items) {
		start = 0
	}
	end := start + size
	if end >= len(items) {
		return items[start:], ""
	}
	return items[start:end], strconv.Itoa(end)
}

func joinItems(items []string) string { return strings.Join(items, "") }

func iso(value time.Time) string { return value.UTC().Format(time.RFC3339) }
