package fakeaws

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// legacyOperations are the probe calls the probe scenarios answer with their
// one-item pages. Every other collector operation is answered by the world.
func legacyOperation(operation string) bool {
	service, action, _ := strings.Cut(operation, ":")
	if service == "ec2" && action == "DescribeRegions" {
		return true
	}
	return probeActions[service] == action
}

// worldOperation names the operation a request carries, including the REST
// services whose operation is only in the method and path.
func worldOperation(request awsRequest) string {
	service := request.service
	if service == "" {
		service = restService(request.path)
	}
	path := strings.TrimSuffix(request.path, "/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	action := request.action
	switch service {
	case "lambda":
		switch {
		case strings.HasPrefix(path, "/2015-03-31/functions"):
			action = "ListFunctions"
		case strings.HasPrefix(path, "/2015-03-31/event-source-mappings"):
			action = "ListEventSourceMappings"
		}
	case "eks":
		switch {
		case len(parts) == 1 && parts[0] == "clusters":
			action = "ListClusters"
		case len(parts) == 2 && parts[0] == "clusters":
			action = "DescribeCluster"
		case len(parts) == 3 && parts[2] == "node-groups":
			action = "ListNodegroups"
		case len(parts) == 4 && parts[2] == "node-groups":
			action = "DescribeNodegroup"
		}
	case "apigateway":
		if strings.HasPrefix(path, "/restapis") {
			action = "GetRestApis"
		}
	case "servicecatalog":
		switch {
		case len(parts) == 1 && parts[0] == "applications":
			action = "ListApplications"
		case len(parts) == 3 && parts[2] == "resources":
			action = "ListAssociatedResources"
		}
	case "s3":
		query := request.params
		switch {
		case path == "" || path == "/":
			action = "ListBuckets"
		case query.Has("publicAccessBlock"):
			action = "GetPublicAccessBlock"
		case query.Has("policyStatus"):
			action = "GetBucketPolicyStatus"
		case query.Has("encryption"):
			action = "GetBucketEncryption"
		}
	case "cloudfront":
		if strings.HasPrefix(path, "/2020-05-31/distribution") {
			action = "ListDistributions"
		}
	case "route53":
		if strings.HasPrefix(path, "/2013-04-01/healthcheck") {
			action = "ListHealthChecks"
		}
	}
	if service == "" || action == "" {
		return ""
	}
	return service + ":" + action
}

// writeWorldOperation answers a collector operation from the world. It
// reports false when the request is left to the probe fixture.
func (s *Server) writeWorldOperation(writer http.ResponseWriter, request awsRequest) bool {
	operation := worldOperation(request)
	handler, ok := worldHandlers[operation]
	if !ok {
		return false
	}
	if !worldScenarios[request.scenario] && legacyOperation(operation) {
		return false
	}
	if request.scenario == ScenarioAccessDeniedBurst && (scenarioDenials[request.service] || scenarioDenials[operation]) {
		writeWorldError(writer, request, http.StatusForbidden, "AccessDenied")
		return true
	}
	handler(s, writer, request, s.worldFor(request))
	return true
}

type worldHandler func(*Server, http.ResponseWriter, awsRequest, world)

var worldHandlers = map[string]worldHandler{
	"ec2:DescribeInstances":                      (*Server).worldInstances,
	"ec2:DescribeInstanceStatus":                 (*Server).worldInstanceStatus,
	"ec2:DescribeVpcs":                           (*Server).worldVPCs,
	"ec2:DescribeSubnets":                        (*Server).worldSubnets,
	"ec2:DescribeRouteTables":                    (*Server).worldRouteTables,
	"ec2:DescribeInternetGateways":               (*Server).worldInternetGateways,
	"ec2:DescribeNatGateways":                    (*Server).worldNATGateways,
	"ec2:DescribeSecurityGroups":                 (*Server).worldSecurityGroups,
	"ec2:DescribeNetworkInterfaces":              (*Server).worldNetworkInterfaces,
	"ec2:DescribeVpcEndpoints":                   (*Server).worldVPCEndpoints,
	"autoscaling:DescribeAutoScalingGroups":      (*Server).worldAutoScaling,
	"elasticloadbalancing:DescribeLoadBalancers": (*Server).worldLoadBalancers,
	"elasticloadbalancing:DescribeTargetGroups":  (*Server).worldTargetGroups,
	"elasticloadbalancing:DescribeTargetHealth":  (*Server).worldTargetHealth,
	"ecs:ListClusters":                           (*Server).worldECSClusters,
	"ecs:DescribeClusters":                       (*Server).worldECSDescribeClusters,
	"ecs:ListServices":                           (*Server).worldECSServices,
	"ecs:DescribeServices":                       (*Server).worldECSDescribeServices,
	"eks:ListClusters":                           (*Server).worldEKSClusters,
	"eks:DescribeCluster":                        (*Server).worldEKSCluster,
	"eks:ListNodegroups":                         (*Server).worldEKSNodegroups,
	"eks:DescribeNodegroup":                      (*Server).worldEKSNodegroup,
	"lambda:ListFunctions":                       (*Server).worldFunctions,
	"lambda:ListEventSourceMappings":             (*Server).worldEventSourceMappings,
	"rds:DescribeDBInstances":                    (*Server).worldDBInstances,
	"rds:DescribeDBClusters":                     (*Server).worldDBClusters,
	"rds:DescribeEvents":                         (*Server).worldDBEvents,
	"cloudwatch:DescribeAlarms":                  (*Server).worldAlarms,
	"cloudformation:DescribeStacks":              (*Server).worldStacks,
	"cloudformation:ListStackResources":          (*Server).worldStackResources,
	"health:DescribeEvents":                      (*Server).worldHealthEvents,
	"health:DescribeAffectedEntities":            (*Server).worldAffectedEntities,
	"tagging:GetResources":                       (*Server).worldTags,
	"servicecatalog:ListApplications":            (*Server).worldApplications,
	"servicecatalog:ListAssociatedResources":     (*Server).worldAssociatedResources,
	"dynamodb:ListTables":                        (*Server).worldTables,
	"elasticache:DescribeCacheClusters":          (*Server).worldCacheClusters,
	"sqs:ListQueues":                             (*Server).worldQueues,
	"sqs:GetQueueAttributes":                     (*Server).worldQueueAttributes,
	"sns:ListTopics":                             (*Server).worldTopics,
	"sns:ListSubscriptions":                      (*Server).worldSubscriptions,
	"events:ListRules":                           (*Server).worldRules,
	"events:ListTargetsByRule":                   (*Server).worldRuleTargets,
	"states:ListStateMachines":                   (*Server).worldStateMachines,
	"kinesis:ListStreams":                        (*Server).worldStreams,
	"apigateway:GetRestApis":                     (*Server).worldRestAPIs,
	"s3:ListBuckets":                             (*Server).worldBuckets,
	"s3:GetPublicAccessBlock":                    (*Server).worldPublicAccessBlock,
	"s3:GetBucketPolicyStatus":                   (*Server).worldPolicyStatus,
	"s3:GetBucketEncryption":                     (*Server).worldEncryption,
	"cloudfront:ListDistributions":               (*Server).worldDistributions,
	"route53:ListHealthChecks":                   (*Server).worldHealthChecks,
	"iam:ListRoles":                              (*Server).worldRoles,
	"cloudtrail:LookupEvents":                    (*Server).worldLookupEvents,
}

// CollectorOperations lists every operation the world answers, sorted. A
// test can compare it with the operations the collectors call.
func CollectorOperations() []string {
	operations := make([]string, 0, len(worldHandlers))
	for operation := range worldHandlers {
		operations = append(operations, operation)
	}
	sort.Strings(operations)
	return operations
}

func writeWorldError(writer http.ResponseWriter, request awsRequest, status int, code string) {
	writeErrorFor(writer, request, status, code, "fixture")
}

// jsonBody decodes a JSON request body; an empty or invalid body is empty.
func jsonBody(request awsRequest) map[string]any {
	fields := map[string]any{}
	_ = json.Unmarshal(request.body, &fields)
	return fields
}

func jsonString(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}

func jsonInt(fields map[string]any, key string) int {
	value, _ := fields[key].(float64)
	return int(value)
}

func queryInt(request awsRequest, keys ...string) int {
	for _, key := range keys {
		if value, err := strconv.Atoi(request.params.Get(key)); err == nil {
			return value
		}
	}
	return 0
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// ---- EC2 (ec2 query protocol) ----

func writeEC2(writer http.ResponseWriter, action, inner, next string) {
	if next != "" {
		inner += xmlText("nextToken", next)
	}
	writeXMLBody(writer, `<`+action+`Response xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>`+requestID+`</requestId>`+inner+`</`+action+`Response>`)
}

func ec2TagSet(tags [][2]string) string {
	inner := ""
	for _, tag := range tags {
		inner += "<item>" + xmlText("key", tag[0]) + xmlText("value", tag[1]) + "</item>"
	}
	return "<tagSet>" + inner + "</tagSet>"
}

func (s *Server) worldInstances(writer http.ResponseWriter, request awsRequest, w world) {
	page, next := pageOf(w.instanceIDs(), request.params.Get("NextToken"), queryInt(request, "MaxResults"))
	items := make([]string, 0, len(page))
	for _, id := range page {
		web := strings.HasPrefix(id, "i-0123456789abcdef")
		item := xmlText("instanceId", id) + xmlText("instanceType", "t3.micro") +
			`<instanceState><code>16</code><name>running</name></instanceState>` +
			`<placement>` + xmlText("availabilityZone", w.region+"a") + `</placement>` +
			xmlText("vpcId", WorldVPC) + xmlText("subnetId", WorldSubnet) +
			`<groupSet><item>` + xmlText("groupId", WorldSecurityGroup) + xmlText("groupName", "web") + `</item></groupSet>`
		if id == "i-0123456789abcdef0" {
			item += xmlText("ipAddress", "203.0.113.10")
		}
		if web {
			item += ec2TagSet([][2]string{{"Name", "versus-web"}, {"app", WorldApplication}, {"owner", WorldRedactedTagValue}})
		}
		items = append(items, "<item>"+item+"</item>")
	}
	inner := "<reservationSet>"
	if len(items) > 0 {
		inner += "<item>" + xmlText("reservationId", "r-0123456789abcdef0") + xmlText("ownerId", w.account) +
			"<instancesSet>" + joinItems(items) + "</instancesSet></item>"
	}
	writeEC2(writer, "DescribeInstances", inner+"</reservationSet>", next)
}

func (s *Server) worldInstanceStatus(writer http.ResponseWriter, request awsRequest, w world) {
	page, next := pageOf(w.instanceIDs(), request.params.Get("NextToken"), queryInt(request, "MaxResults"))
	items := ""
	for _, id := range page {
		item := xmlText("instanceId", id) + xmlText("availabilityZone", w.region+"a") +
			`<instanceState><code>16</code><name>running</name></instanceState>` +
			`<systemStatus><status>ok</status></systemStatus><instanceStatus><status>ok</status></instanceStatus>`
		if w.scenario == ScenarioHealthEvent && id == "i-0123456789abcdef0" {
			item += `<eventsSet><item>` + xmlText("code", "system-reboot") + xmlText("description", "scheduled reboot") +
				xmlText("notBefore", iso(w.now.Add(48*time.Hour))) + `</item></eventsSet>`
		}
		items += "<item>" + item + "</item>"
	}
	writeEC2(writer, "DescribeInstanceStatus", "<instanceStatusSet>"+items+"</instanceStatusSet>", next)
}

func (s *Server) worldVPCs(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeEC2(writer, "DescribeVpcs", `<vpcSet><item>`+xmlText("vpcId", WorldVPC)+xmlText("state", "available")+
		xmlText("cidrBlock", "10.0.0.0/16")+ec2TagSet([][2]string{{"Name", "versus-vpc"}})+`</item></vpcSet>`, "")
}

func (s *Server) worldSubnets(writer http.ResponseWriter, _ awsRequest, w world) {
	writeEC2(writer, "DescribeSubnets", `<subnetSet><item>`+xmlText("subnetId", WorldSubnet)+xmlText("vpcId", WorldVPC)+
		xmlText("state", "available")+xmlText("availabilityZone", w.region+"a")+xmlText("cidrBlock", "10.0.1.0/24")+ec2TagSet([][2]string{{"Name", "versus-subnet"}})+`</item></subnetSet>`, "")
}

func (s *Server) worldRouteTables(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeEC2(writer, "DescribeRouteTables", `<routeTableSet><item>`+xmlText("routeTableId", "rtb-0a1b2c3d")+xmlText("vpcId", WorldVPC)+
		`<associationSet><item>`+xmlText("subnetId", WorldSubnet)+`</item></associationSet>`+
		`<routeSet><item>`+xmlText("destinationCidrBlock", "0.0.0.0/0")+xmlText("gatewayId", "igw-0a1b2c3d")+`</item>`+
		`<item>`+xmlText("destinationCidrBlock", "10.1.0.0/16")+xmlText("natGatewayId", "nat-0a1b2c3d")+`</item></routeSet>`+
		ec2TagSet([][2]string{{"Name", "versus-routes"}})+`</item></routeTableSet>`, "")
}

func (s *Server) worldInternetGateways(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeEC2(writer, "DescribeInternetGateways", `<internetGatewaySet><item>`+xmlText("internetGatewayId", "igw-0a1b2c3d")+
		`<attachmentSet><item>`+xmlText("vpcId", WorldVPC)+xmlText("state", "available")+`</item></attachmentSet>`+ec2TagSet([][2]string{{"Name", "versus-igw"}})+`</item></internetGatewaySet>`, "")
}

func (s *Server) worldNATGateways(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeEC2(writer, "DescribeNatGateways", `<natGatewaySet><item>`+xmlText("natGatewayId", "nat-0a1b2c3d")+
		xmlText("vpcId", WorldVPC)+xmlText("subnetId", WorldSubnet)+xmlText("state", "available")+ec2TagSet([][2]string{{"Name", "versus-nat"}})+`</item></natGatewaySet>`, "")
}

func ingress(port int) string {
	return `<item>` + xmlText("ipProtocol", "tcp") + xmlText("fromPort", strconv.Itoa(port)) + xmlText("toPort", strconv.Itoa(port)) +
		`<ipRanges><item>` + xmlText("cidrIp", "0.0.0.0/0") + `</item></ipRanges></item>`
}

func (s *Server) worldSecurityGroups(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeEC2(writer, "DescribeSecurityGroups", `<securityGroupInfo>`+
		`<item>`+xmlText("groupId", WorldSecurityGroup)+xmlText("groupName", "web")+xmlText("vpcId", WorldVPC)+`<ipPermissions>`+ingress(443)+`</ipPermissions></item>`+
		`<item>`+xmlText("groupId", WorldAdminGroup)+xmlText("groupName", "admin-ssh")+xmlText("vpcId", WorldVPC)+`<ipPermissions>`+ingress(22)+`</ipPermissions></item>`+
		`</securityGroupInfo>`, "")
}

func (s *Server) worldNetworkInterfaces(writer http.ResponseWriter, _ awsRequest, w world) {
	writeEC2(writer, "DescribeNetworkInterfaces", `<networkInterfaceSet><item>`+xmlText("networkInterfaceId", "eni-0a1b2c3d")+
		xmlText("subnetId", WorldSubnet)+xmlText("vpcId", WorldVPC)+xmlText("availabilityZone", w.region+"a")+xmlText("status", "in-use")+
		`<groupSet><item>`+xmlText("groupId", WorldSecurityGroup)+`</item></groupSet>`+
		`<attachment>`+xmlText("instanceId", "i-0123456789abcdef0")+`</attachment>`+
		`<association>`+xmlText("publicIp", "203.0.113.10")+`</association>`+
		`</item></networkInterfaceSet>`, "")
}

func (s *Server) worldVPCEndpoints(writer http.ResponseWriter, _ awsRequest, w world) {
	writeEC2(writer, "DescribeVpcEndpoints", `<vpcEndpointSet><item>`+xmlText("vpcEndpointId", "vpce-0a1b2c3d")+
		xmlText("vpcId", WorldVPC)+xmlText("serviceName", "com.amazonaws."+w.region+".s3")+xmlText("state", "available")+
		`<subnetIdSet><item>`+WorldSubnet+`</item></subnetIdSet></item></vpcEndpointSet>`, "")
}

// ---- Query protocol services ----

func writeQuery(writer http.ResponseWriter, action, inner string) {
	writeXMLBody(writer, queryResult(action, inner))
}

func members(element string, items []string) string {
	inner := ""
	for _, item := range items {
		inner += "<member>" + item + "</member>"
	}
	return "<" + element + ">" + inner + "</" + element + ">"
}

func (s *Server) worldAutoScaling(writer http.ResponseWriter, _ awsRequest, w world) {
	instances := make([]string, 0, WorldTargets)
	for index := 0; index < WorldTargets; index++ {
		instances = append(instances, xmlText("InstanceId", fmt.Sprintf("i-0123456789abcdef%d", index))+
			xmlText("LifecycleState", "InService")+xmlText("HealthStatus", "Healthy")+
			xmlText("AvailabilityZone", w.region+"a")+xmlText("ProtectedFromScaleIn", "false"))
	}
	group := xmlText("AutoScalingGroupName", "versus-web") +
		xmlText("AutoScalingGroupARN", w.arn("autoscaling", "autoScalingGroup:00000000-0000-0000-0000-000000000000:autoScalingGroupName/versus-web")) +
		xmlText("MinSize", "2") + xmlText("MaxSize", "8") + xmlText("DesiredCapacity", strconv.Itoa(WorldTargets)) +
		xmlText("VPCZoneIdentifier", WorldSubnet) + members("TargetGroupARNs", []string{xmlEscape(w.targetGroupARN())}) +
		members("Instances", instances) + members("Tags", []string{xmlText("Key", "app") + xmlText("Value", WorldApplication)})
	writeQuery(writer, "DescribeAutoScalingGroups", members("AutoScalingGroups", []string{group}))
}

func (s *Server) worldLoadBalancers(writer http.ResponseWriter, _ awsRequest, w world) {
	balancer := xmlText("LoadBalancerArn", w.loadBalancerARN()) + xmlText("LoadBalancerName", "versus") +
		xmlText("DNSName", "versus-1234567890."+w.region+".elb.amazonaws.com") + xmlText("Scheme", "internet-facing") +
		xmlText("Type", "application") + xmlText("VpcId", WorldVPC) + `<State><Code>active</Code></State>` +
		members("AvailabilityZones", []string{xmlText("ZoneName", w.region+"a") + xmlText("SubnetId", WorldSubnet)}) +
		members("SecurityGroups", []string{WorldSecurityGroup})
	writeQuery(writer, "DescribeLoadBalancers", members("LoadBalancers", []string{balancer}))
}

func (s *Server) worldTargetGroups(writer http.ResponseWriter, _ awsRequest, w world) {
	group := xmlText("TargetGroupArn", w.targetGroupARN()) + xmlText("TargetGroupName", "versus-web") +
		xmlText("VpcId", WorldVPC) + xmlText("TargetType", "instance") + xmlText("Protocol", "HTTP") + xmlText("Port", "80") +
		members("LoadBalancerArns", []string{xmlEscape(w.loadBalancerARN())})
	writeQuery(writer, "DescribeTargetGroups", members("TargetGroups", []string{group}))
}

func (s *Server) worldTargetHealth(writer http.ResponseWriter, _ awsRequest, w world) {
	targets := make([]string, 0, WorldTargets)
	for index := 0; index < WorldTargets; index++ {
		state := `<TargetHealth><State>healthy</State></TargetHealth>`
		if w.badDeploy() && index < WorldUnhealthyTargets {
			state = `<TargetHealth><State>unhealthy</State><Reason>Target.ResponseCodeMismatch</Reason><Description>Health checks failed with these codes: [502]</Description></TargetHealth>`
		}
		targets = append(targets, `<Target>`+xmlText("Id", fmt.Sprintf("i-0123456789abcdef%d", index))+xmlText("Port", "80")+`</Target>`+state)
	}
	writeQuery(writer, "DescribeTargetHealth", members("TargetHealthDescriptions", targets))
}

func (s *Server) worldDBInstances(writer http.ResponseWriter, _ awsRequest, w world) {
	instance := xmlText("DBInstanceIdentifier", "versus-db") + xmlText("DBInstanceArn", w.arn("rds", "db:versus-db")) +
		xmlText("DBInstanceClass", "db.t3.micro") + xmlText("Engine", "postgres") + xmlText("DBInstanceStatus", "available") +
		xmlText("AvailabilityZone", w.region+"a") + xmlText("PubliclyAccessible", "false") + xmlText("StorageEncrypted", "true") +
		`<DBSubnetGroup>` + xmlText("VpcId", WorldVPC) + `<Subnets><Subnet>` + xmlText("SubnetIdentifier", WorldSubnet) + `</Subnet></Subnets></DBSubnetGroup>` +
		`<VpcSecurityGroups><VpcSecurityGroupMembership>` + xmlText("VpcSecurityGroupId", WorldSecurityGroup) + xmlText("Status", "active") + `</VpcSecurityGroupMembership></VpcSecurityGroups>`
	writeQuery(writer, "DescribeDBInstances", `<DBInstances><DBInstance>`+instance+`</DBInstance></DBInstances>`)
}

func (s *Server) worldDBClusters(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeQuery(writer, "DescribeDBClusters", `<DBClusters></DBClusters>`)
}

func (s *Server) worldDBEvents(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeQuery(writer, "DescribeEvents", `<Events></Events>`)
}

func (s *Server) worldStacks(writer http.ResponseWriter, _ awsRequest, w world) {
	stack := xmlText("StackName", WorldStack) + xmlText("StackId", w.stackARN()) + xmlText("StackStatus", "CREATE_COMPLETE") +
		xmlText("CreationTime", iso(w.now.Add(-30*24*time.Hour))) + members("Tags", []string{xmlText("Key", "team") + xmlText("Value", "payments")})
	writeQuery(writer, "DescribeStacks", members("Stacks", []string{stack}))
}

func (s *Server) worldStackResources(writer http.ResponseWriter, _ awsRequest, w world) {
	resource := func(logical, physical, kind string) string {
		return xmlText("LogicalResourceId", logical) + xmlText("PhysicalResourceId", physical) + xmlText("ResourceType", kind) +
			xmlText("ResourceStatus", "CREATE_COMPLETE") + xmlText("LastUpdatedTimestamp", iso(w.now.Add(-30*24*time.Hour)))
	}
	writeQuery(writer, "ListStackResources", members("StackResourceSummaries", []string{
		resource("Service", w.serviceARN(), "AWS::ECS::Service"),
		resource("Queue", w.queueURL(WorldQueue), "AWS::SQS::Queue"),
		resource("Worker", WorldFunction, "AWS::Lambda::Function"),
		resource("Balancer", w.loadBalancerARN(), "AWS::ElasticLoadBalancingV2::LoadBalancer"),
	}))
}

func (s *Server) worldCacheClusters(writer http.ResponseWriter, _ awsRequest, w world) {
	cluster := xmlText("CacheClusterId", "versus-cache") + xmlText("ARN", w.arn("elasticache", "cluster:versus-cache")) +
		xmlText("CacheNodeType", "cache.t3.micro") + xmlText("Engine", "redis") + xmlText("CacheClusterStatus", "available") +
		xmlText("PreferredAvailabilityZone", w.region+"a") + xmlText("AtRestEncryptionEnabled", "true") +
		members("SecurityGroups", []string{xmlText("SecurityGroupId", WorldSecurityGroup) + xmlText("Status", "active")})
	writeQuery(writer, "DescribeCacheClusters", `<CacheClusters><CacheCluster>`+cluster+`</CacheCluster></CacheClusters>`)
}

func (s *Server) worldTopics(writer http.ResponseWriter, request awsRequest, w world) {
	names, next := pageOf(w.topicNames(), request.params.Get("NextToken"), 0)
	items := make([]string, 0, len(names))
	for _, name := range names {
		items = append(items, xmlText("TopicArn", w.arn("sns", name)))
	}
	inner := members("Topics", items)
	if next != "" {
		inner += xmlText("NextToken", next)
	}
	writeQuery(writer, "ListTopics", inner)
}

func (s *Server) worldSubscriptions(writer http.ResponseWriter, _ awsRequest, w world) {
	topic := w.arn("sns", "versus-alarms")
	writeQuery(writer, "ListSubscriptions", members("Subscriptions", []string{
		xmlText("TopicArn", topic) + xmlText("Protocol", "sqs") + xmlText("Endpoint", w.queueARN(WorldQueue)) + xmlText("SubscriptionArn", topic+":1"),
		xmlText("TopicArn", topic) + xmlText("Protocol", "email") + xmlText("Endpoint", WorldPersonalEmail) + xmlText("SubscriptionArn", topic+":2"),
	}))
}

func (s *Server) worldRoles(writer http.ResponseWriter, _ awsRequest, w world) {
	role := func(name string) string {
		return xmlText("RoleName", name) + xmlText("Path", "/") + xmlText("RoleId", "AROAEXAMPLE"+strings.ToUpper(name[:3])) +
			xmlText("Arn", "arn:aws:iam::"+w.account+":role/"+name) + xmlText("CreateDate", iso(w.now.Add(-90*24*time.Hour)))
	}
	writeQuery(writer, "ListRoles", members("Roles", []string{role("versus-reader"), role("github-actions-deploy")})+xmlText("IsTruncated", "false"))
}

// ---- JSON protocol services ----

func (s *Server) worldECSClusters(writer http.ResponseWriter, _ awsRequest, w world) {
	writeJSONBody(writer, mustJSON(map[string]any{"clusterArns": []string{w.clusterARN()}}))
}

func (s *Server) worldECSDescribeClusters(writer http.ResponseWriter, _ awsRequest, w world) {
	writeJSONBody(writer, mustJSON(map[string]any{"clusters": []any{map[string]any{
		"clusterArn": w.clusterARN(), "clusterName": WorldCluster, "status": "ACTIVE",
		"tags": []any{map[string]any{"key": "app", "value": WorldApplication}},
	}}}))
}

func (s *Server) worldECSServices(writer http.ResponseWriter, _ awsRequest, w world) {
	writeJSONBody(writer, mustJSON(map[string]any{"serviceArns": []string{w.serviceARN()}}))
}

func (s *Server) worldECSDescribeServices(writer http.ResponseWriter, _ awsRequest, w world) {
	deployment := map[string]any{
		"id": "ecs-svc/0000000000000000041", "status": "PRIMARY", "taskDefinition": w.taskDefinitionARN(),
		"desiredCount": WorldTargets, "runningCount": WorldTargets, "rolloutState": "COMPLETED",
		"createdAt": w.now.Add(-48 * time.Hour).Unix(), "updatedAt": w.now.Add(-48 * time.Hour).Unix(),
	}
	if w.badDeploy() {
		start := BadDeployStart(w.now).Add(20 * time.Second)
		deployment["id"] = "ecs-svc/0000000000000000042"
		deployment["runningCount"] = 1
		deployment["rolloutState"] = "IN_PROGRESS"
		deployment["createdAt"] = start.Unix()
		deployment["updatedAt"] = start.Unix()
	}
	writeJSONBody(writer, mustJSON(map[string]any{"services": []any{map[string]any{
		"serviceArn": w.serviceARN(), "serviceName": WorldService, "clusterArn": w.clusterARN(), "status": "ACTIVE",
		"taskDefinition": w.taskDefinitionARN(), "desiredCount": WorldTargets, "runningCount": deployment["runningCount"],
		"loadBalancers": []any{map[string]any{"targetGroupArn": w.targetGroupARN(), "containerName": "web", "containerPort": 80}},
		"networkConfiguration": map[string]any{"awsvpcConfiguration": map[string]any{
			"subnets": []string{WorldSubnet}, "securityGroups": []string{WorldSecurityGroup},
		}},
		"deployments": []any{deployment},
		"tags":        []any{map[string]any{"key": "app", "value": WorldApplication}},
	}}}))
}

func (s *Server) worldTables(writer http.ResponseWriter, request awsRequest, w world) {
	fields := jsonBody(request)
	names := w.tableNames()
	token := ""
	if start := jsonString(fields, "ExclusiveStartTableName"); start != "" {
		for index, name := range names {
			if name == start {
				token = strconv.Itoa(index + 1)
			}
		}
	}
	page, next := pageOf(names, token, jsonInt(fields, "Limit"))
	body := map[string]any{"TableNames": page}
	if next != "" && len(page) > 0 {
		body["LastEvaluatedTableName"] = page[len(page)-1]
	}
	writeBody(writer, "application/x-amz-json-1.0", http.StatusOK, mustJSON(body))
}

func (s *Server) worldQueues(writer http.ResponseWriter, request awsRequest, w world) {
	fields := jsonBody(request)
	names, next := pageOf(w.queueNames(), jsonString(fields, "NextToken"), jsonInt(fields, "MaxResults"))
	urls := make([]string, 0, len(names))
	for _, name := range names {
		urls = append(urls, w.queueURL(name))
	}
	body := map[string]any{"QueueUrls": urls}
	if next != "" {
		body["NextToken"] = next
	}
	writeBody(writer, "application/x-amz-json-1.0", http.StatusOK, mustJSON(body))
}

func (s *Server) worldQueueAttributes(writer http.ResponseWriter, request awsRequest, w world) {
	url := jsonString(jsonBody(request), "QueueUrl")
	name := url[strings.LastIndex(url, "/")+1:]
	attributes := map[string]string{"QueueArn": w.queueARN(name), "SqsManagedSseEnabled": "true"}
	if name == WorldQueue {
		attributes["RedrivePolicy"] = mustJSON(map[string]any{"deadLetterTargetArn": w.queueARN(WorldQueue + "-dlq"), "maxReceiveCount": 5})
	}
	if name == WorldQueue+"-dlq" {
		attributes["SqsManagedSseEnabled"] = "false"
	}
	writeBody(writer, "application/x-amz-json-1.0", http.StatusOK, mustJSON(map[string]any{"Attributes": attributes}))
}

func (s *Server) worldRules(writer http.ResponseWriter, _ awsRequest, w world) {
	writeJSONBody(writer, mustJSON(map[string]any{"Rules": []any{map[string]any{
		"Name": "versus-nightly", "Arn": w.arn("events", "rule/versus-nightly"), "State": "ENABLED", "ScheduleExpression": "rate(1 day)",
	}}}))
}

func (s *Server) worldRuleTargets(writer http.ResponseWriter, _ awsRequest, w world) {
	writeJSONBody(writer, mustJSON(map[string]any{"Targets": []any{map[string]any{"Id": "worker", "Arn": w.functionARN(WorldFunction)}}}))
}

func (s *Server) worldStateMachines(writer http.ResponseWriter, _ awsRequest, w world) {
	writeBody(writer, "application/x-amz-json-1.0", http.StatusOK, mustJSON(map[string]any{"stateMachines": []any{map[string]any{
		"stateMachineArn": w.arn("states", "stateMachine:versus-checkpoint"), "name": "versus-checkpoint", "type": "STANDARD",
		"creationDate": w.now.Add(-60 * 24 * time.Hour).Unix(),
	}}}))
}

func (s *Server) worldStreams(writer http.ResponseWriter, _ awsRequest, w world) {
	writeJSONBody(writer, mustJSON(map[string]any{
		"StreamNames":    []string{"versus-events"},
		"HasMoreStreams": false,
		"StreamSummaries": []any{map[string]any{
			"StreamName": "versus-events", "StreamARN": w.arn("kinesis", "stream/versus-events"), "StreamStatus": "ACTIVE",
			"StreamCreationTimestamp": w.now.Add(-60 * 24 * time.Hour).Unix(),
		}},
	}))
}

func (s *Server) worldTags(writer http.ResponseWriter, request awsRequest, w world) {
	tag := func(key, value string) map[string]string { return map[string]string{"Key": key, "Value": value} }
	worker := []any{tag("app", "worker"), tag("team", "jobs"), tag("cost-center", "cc-1234")}
	if request.scenario == "secret-canary" {
		worker = append(worker, tag("notes", CanaryTagValue))
	}
	mappings := []any{
		map[string]any{"ResourceARN": w.functionARN(WorldFunction), "Tags": worker},
		map[string]any{"ResourceARN": w.queueARN(WorldQueue), "Tags": []any{tag("app", WorldApplication)}},
		map[string]any{"ResourceARN": w.arn("dynamodb", "table/versus-locks"), "Tags": []any{tag("app", "platform")}},
	}
	writeJSONBody(writer, mustJSON(map[string]any{"ResourceTagMappingList": mappings, "PaginationToken": ""}))
}

// ---- REST JSON services ----

func (s *Server) worldFunctions(writer http.ResponseWriter, request awsRequest, w world) {
	names, next := pageOf(w.functionNames(), request.params.Get("Marker"), queryInt(request, "MaxItems"))
	functions := make([]any, 0, len(names))
	for _, name := range names {
		functions = append(functions, map[string]any{
			"FunctionName": name, "FunctionArn": w.functionARN(name), "Runtime": "provided.al2023", "State": "Active",
		})
	}
	body := map[string]any{"Functions": functions}
	if next != "" {
		body["NextMarker"] = next
	}
	writeRESTJSONBody(writer, mustJSON(body))
}

func (s *Server) worldEventSourceMappings(writer http.ResponseWriter, _ awsRequest, w world) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"EventSourceMappings": []any{map[string]any{
		"UUID": "00000000-0000-0000-0000-0000000000e5", "EventSourceArn": w.queueARN(WorldQueue),
		"FunctionArn": w.functionARN(WorldFunction), "State": "Enabled",
	}}}))
}

func (s *Server) worldEKSClusters(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"clusters": []string{"versus-eks"}}))
}

func (s *Server) worldEKSCluster(writer http.ResponseWriter, _ awsRequest, w world) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"cluster": map[string]any{
		"name": "versus-eks", "arn": w.arn("eks", "cluster/versus-eks"), "status": "ACTIVE", "version": "1.30",
		"resourcesVpcConfig": map[string]any{
			"vpcId": WorldVPC, "subnetIds": []string{WorldSubnet}, "securityGroupIds": []string{WorldSecurityGroup},
			"endpointPublicAccess": true, "endpointPrivateAccess": true, "publicAccessCidrs": []string{"0.0.0.0/0"},
		},
		"tags": map[string]string{"app": "platform"},
	}}))
}

func (s *Server) worldEKSNodegroups(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"nodegroups": []string{"default"}}))
}

func (s *Server) worldEKSNodegroup(writer http.ResponseWriter, _ awsRequest, w world) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"nodegroup": map[string]any{
		"nodegroupName": "default", "clusterName": "versus-eks", "status": "ACTIVE",
		"nodegroupArn":  w.arn("eks", "nodegroup/versus-eks/default/00000000-0000-0000-0000-000000000000"),
		"scalingConfig": map[string]any{"minSize": 1, "maxSize": 5, "desiredSize": 3},
		"subnets":       []string{WorldSubnet},
	}}))
}

func (s *Server) worldRestAPIs(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"item": []any{map[string]any{
		"id": "abc123", "name": "versus-api", "tags": map[string]string{"app": WorldApplication},
	}}}))
}

func (s *Server) worldApplications(writer http.ResponseWriter, _ awsRequest, w world) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"applications": []any{map[string]any{
		"id": "0abcdefghijklmnopqrstuvwxy", "name": WorldApplication, "arn": w.arn("servicecatalog", "/applications/0abcdefghijklmnopqrstuvwxy"),
	}}}))
}

func (s *Server) worldAssociatedResources(writer http.ResponseWriter, _ awsRequest, w world) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"resources": []any{map[string]any{"name": WorldStack, "arn": w.stackARN()}}}))
}

// ---- REST XML services ----

func (s *Server) worldBuckets(writer http.ResponseWriter, _ awsRequest, _ world) {
	bucket := func(name string) string {
		return `<Bucket>` + xmlText("Name", name) + xmlText("CreationDate", "2026-01-01T00:00:00.000Z") + xmlText("BucketRegion", "us-east-1") + `</Bucket>`
	}
	writeRESTXMLBody(writer, `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>versus</ID></Owner><Buckets>`+
		bucket(WorldPrivateBucket)+bucket(WorldPublicBucket)+`</Buckets></ListAllMyBucketsResult>`)
}

func bucketOf(request awsRequest) string { return strings.Trim(request.path, "/") }

func (s *Server) worldPublicAccessBlock(writer http.ResponseWriter, request awsRequest, _ world) {
	if bucketOf(request) == WorldPublicBucket {
		writeWorldError(writer, request, http.StatusNotFound, "NoSuchPublicAccessBlockConfiguration")
		return
	}
	writeRESTXMLBody(writer, `<PublicAccessBlockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
		`<BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls>`+
		`<BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`)
}

func (s *Server) worldPolicyStatus(writer http.ResponseWriter, request awsRequest, _ world) {
	if bucketOf(request) != WorldPublicBucket {
		writeWorldError(writer, request, http.StatusNotFound, "NoSuchBucketPolicy")
		return
	}
	writeRESTXMLBody(writer, `<PolicyStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsPublic>true</IsPublic></PolicyStatus>`)
}

func (s *Server) worldEncryption(writer http.ResponseWriter, request awsRequest, _ world) {
	if bucketOf(request) == WorldPublicBucket {
		writeWorldError(writer, request, http.StatusNotFound, "ServerSideEncryptionConfigurationNotFoundError")
		return
	}
	writeRESTXMLBody(writer, `<ServerSideEncryptionConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule>`+
		`<ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`)
}

func (s *Server) worldDistributions(writer http.ResponseWriter, _ awsRequest, w world) {
	writeRESTXMLBody(writer, `<DistributionList xmlns="http://cloudfront.amazonaws.com/doc/2020-05-31/">`+
		`<Marker></Marker><MaxItems>100</MaxItems><IsTruncated>false</IsTruncated><Quantity>1</Quantity><Items><DistributionSummary>`+
		xmlText("Id", "EDFDVBD6EXAMPLE")+xmlText("ARN", "arn:aws:cloudfront::"+w.account+":distribution/EDFDVBD6EXAMPLE")+
		xmlText("Status", "Deployed")+xmlText("DomainName", "d111111abcdef8.cloudfront.net")+xmlText("Enabled", "true")+
		`<Origins><Quantity>1</Quantity><Items><Origin>`+xmlText("Id", "logs")+xmlText("DomainName", WorldPrivateBucket+".s3.amazonaws.com")+
		`</Origin></Items></Origins></DistributionSummary></Items></DistributionList>`)
}

func (s *Server) worldHealthChecks(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeRESTXMLBody(writer, `<ListHealthChecksResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><HealthChecks><HealthCheck>`+
		xmlText("Id", "abcd1234-0000-0000-0000-000000000000")+xmlText("CallerReference", "versus")+
		`<HealthCheckConfig><Type>HTTPS</Type><FullyQualifiedDomainName>checkout.example.com</FullyQualifiedDomainName></HealthCheckConfig>`+
		xmlText("HealthCheckVersion", "1")+`</HealthCheck></HealthChecks><Marker></Marker><IsTruncated>false</IsTruncated><MaxItems>100</MaxItems></ListHealthChecksResponse>`)
}

// ---- CBOR services ----

func (s *Server) worldAlarms(writer http.ResponseWriter, _ awsRequest, w world) {
	dimension := func(name, value string) map[string]any { return map[string]any{"Name": name, "Value": value} }
	cpu := map[string]any{
		"AlarmName": WorldAlarmCPU, "AlarmArn": w.arn("cloudwatch", "alarm:"+WorldAlarmCPU), "StateValue": "OK",
		"MetricName": "CPUUtilization", "Namespace": "AWS/EC2",
		"Dimensions":            []any{dimension("InstanceId", "i-0123456789abcdef0")},
		"StateUpdatedTimestamp": cborTime{w.now.Add(-24 * time.Hour)},
	}
	targetErrors := map[string]any{
		"AlarmName": WorldAlarm5xx, "AlarmArn": w.arn("cloudwatch", "alarm:"+WorldAlarm5xx), "StateValue": "OK",
		"MetricName": "HTTPCode_Target_5XX_Count", "Namespace": "AWS/ApplicationELB",
		"Dimensions":            []any{dimension("LoadBalancer", WorldLoadBalancerID)},
		"StateUpdatedTimestamp": cborTime{w.now.Add(-24 * time.Hour)},
	}
	composite := map[string]any{
		"AlarmName": WorldCompositeAlarm, "AlarmArn": w.arn("cloudwatch", "alarm:"+WorldCompositeAlarm), "StateValue": "OK",
		"AlarmRule":             `ALARM("` + WorldAlarm5xx + `") OR ALARM("` + WorldAlarmCPU + `")`,
		"StateUpdatedTimestamp": cborTime{w.now.Add(-24 * time.Hour)},
	}
	if w.badDeploy() {
		fired := cborTime{BadDeployStart(w.now).Add(4 * time.Minute)}
		targetErrors["StateValue"], targetErrors["StateUpdatedTimestamp"] = "ALARM", fired
		composite["StateValue"], composite["StateUpdatedTimestamp"] = "ALARM", fired
	}
	writeCBORBody(writer, http.StatusOK, map[string]any{"MetricAlarms": []any{cpu, targetErrors}, "CompositeAlarms": []any{composite}})
}

// HealthEventARN is the AWS Health event the health-event scenario serves.
const HealthEventARN = "arn:aws:health:us-east-1::event/EC2/" + WorldHealthEventCode + "/" + WorldHealthEventCode + "_0001"

func (s *Server) worldHealthEvents(writer http.ResponseWriter, request awsRequest, w world) {
	if request.account.HealthNotOnPlan {
		writeCBORError(writer, http.StatusBadRequest, "SubscriptionRequiredException", "AWS Health requires a support plan")
		return
	}
	events := []any{}
	if w.scenario == ScenarioHealthEvent {
		events = append(events, map[string]any{
			"arn": HealthEventARN, "service": "EC2", "eventTypeCode": WorldHealthEventCode, "eventTypeCategory": "scheduledChange",
			"region": "us-east-1", "startTime": cborTime{w.now.Add(-time.Hour)}, "statusCode": "upcoming", "eventScopeCode": "ACCOUNT_SPECIFIC",
		})
	}
	writeCBORBody(writer, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) worldAffectedEntities(writer http.ResponseWriter, _ awsRequest, w world) {
	entities := []any{}
	if w.scenario == ScenarioHealthEvent {
		entities = append(entities, map[string]any{"entityValue": "i-0123456789abcdef0", "eventArn": HealthEventARN, "awsAccountId": w.account})
	}
	writeCBORBody(writer, http.StatusOK, map[string]any{"entities": entities})
}

// ---- CloudTrail ----

type trailEvent struct {
	id, name, source string
	at               time.Time
	readOnly         bool
	resources        []string
	document         map[string]any
}

func assumedRole(account, role, session string) map[string]any {
	return map[string]any{
		"type": "AssumedRole",
		"arn":  "arn:aws:sts::" + account + ":assumed-role/" + role + "/" + session,
		"sessionContext": map[string]any{"sessionIssuer": map[string]any{
			"type": "Role", "arn": "arn:aws:iam::" + account + ":role/" + role, "userName": role,
		}},
	}
}

// WorldEventIDs are the CloudTrail event IDs the world serves.
const (
	EventRunInstances           = "00000000-0000-0000-0000-0000000000a1"
	EventPutBucketTagging       = "00000000-0000-0000-0000-0000000000a2"
	EventDescribeInstances      = "00000000-0000-0000-0000-0000000000a3"
	EventRegisterTaskDefinition = "00000000-0000-0000-0000-0000000000b1"
	EventUpdateService          = "00000000-0000-0000-0000-0000000000b2"
)

func (w world) trail() []trailEvent {
	events := []trailEvent{
		{id: EventRunInstances, name: "RunInstances", source: "ec2.amazonaws.com", at: w.now.Add(-3 * time.Hour).Truncate(time.Second),
			resources: []string{"i-0123456789abcdef0"},
			document: map[string]any{
				"userIdentity":      assumedRole(w.account, "terraform-apply", "ci-run-1234"),
				"userAgent":         "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
				"requestParameters": map[string]any{"instanceType": "t3.micro", "userData": "c2VjcmV0LXVzZXItZGF0YQ=="},
				"responseElements":  map[string]any{"instancesSet": map[string]any{"items": []any{map[string]any{"instanceId": "i-0123456789abcdef0"}}}},
			}},
		{id: EventPutBucketTagging, name: "PutBucketTagging", source: "s3.amazonaws.com", at: w.now.Add(-time.Hour).Truncate(time.Second),
			resources: []string{WorldPrivateBucket},
			document: map[string]any{
				"userIdentity":      assumedRole(w.account, "AWSReservedSSO_Admin_0123456789abcdef", WorldRedactedTagValue),
				"userAgent":         "[S3Console/0.4]",
				"requestParameters": map[string]any{"bucketName": WorldPrivateBucket, "Tagging": map[string]any{"owner": WorldRedactedTagValue}},
			}},
		{id: EventDescribeInstances, name: "DescribeInstances", source: "ec2.amazonaws.com", at: w.now.Add(-30 * time.Minute).Truncate(time.Second),
			readOnly: true, document: map[string]any{"userIdentity": assumedRole(w.account, "versus-reader", "versus")}},
	}
	if w.badDeploy() {
		start := BadDeployStart(w.now)
		family := WorldTaskFamily
		events = append(events,
			trailEvent{id: EventRegisterTaskDefinition, name: "RegisterTaskDefinition", source: "ecs.amazonaws.com", at: start,
				document: map[string]any{
					"userIdentity": assumedRole(w.account, "github-actions-deploy", "GitHubActions"),
					"userAgent":    "aws-cli/2.17.0 md/command#ecs.register-task-definition",
					"requestParameters": map[string]any{"family": family, "containerDefinitions": []any{map[string]any{
						"name": "web", "environment": []any{map[string]any{"name": "DB_PASSWORD", "value": "vrs-env-secret-do-not-store"}},
					}}},
					"responseElements": map[string]any{"taskDefinition": map[string]any{
						"revision": WorldBadRevision, "taskDefinitionArn": w.arn("ecs", "task-definition/"+family+":"+strconv.Itoa(WorldBadRevision)),
					}},
				}},
			trailEvent{id: EventUpdateService, name: "UpdateService", source: "ecs.amazonaws.com", at: start.Add(20 * time.Second),
				resources: []string{w.serviceARN()},
				document: map[string]any{
					"userIdentity":      assumedRole(w.account, "github-actions-deploy", "GitHubActions"),
					"userAgent":         "aws-cli/2.17.0 md/command#ecs.update-service",
					"requestParameters": map[string]any{"cluster": WorldCluster, "service": WorldService, "taskDefinition": family + ":" + strconv.Itoa(WorldBadRevision)},
					"responseElements":  map[string]any{"service": map[string]any{"serviceArn": w.serviceARN()}},
				}},
		)
	}
	events = append(events, w.extra...)
	sort.Slice(events, func(left, right int) bool { return events[left].at.After(events[right].at) })
	return events
}

func jsonTime(fields map[string]any, key string) (time.Time, bool) {
	value, ok := fields[key].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, int64(value*float64(time.Second))).UTC(), true
}

func (s *Server) worldLookupEvents(writer http.ResponseWriter, request awsRequest, w world) {
	fields := jsonBody(request)
	start, hasStart := jsonTime(fields, "StartTime")
	end, hasEnd := jsonTime(fields, "EndTime")
	var selected []string
	for _, event := range w.trail() {
		if (hasStart && event.at.Before(start)) || (hasEnd && !event.at.Before(end)) {
			continue
		}
		document := map[string]any{"eventVersion": "1.10", "eventName": event.name, "eventSource": event.source, "readOnly": event.readOnly}
		for key, value := range event.document {
			document[key] = value
		}
		resources := make([]any, 0, len(event.resources))
		for _, name := range event.resources {
			resources = append(resources, map[string]any{"ResourceName": name})
		}
		selected = append(selected, mustJSON(map[string]any{
			"EventId": event.id, "EventName": event.name, "EventSource": event.source, "EventTime": event.at.Unix(),
			"ReadOnly": strconv.FormatBool(event.readOnly), "Resources": resources, "CloudTrailEvent": mustJSON(document),
		}))
	}
	page, next := pageOf(selected, jsonString(fields, "NextToken"), jsonInt(fields, "MaxResults"))
	body := `{"Events":[` + strings.Join(page, ",") + `]`
	if next != "" {
		body += `,"NextToken":` + quote(next)
	}
	writeJSONBody(writer, body+"}")
}
