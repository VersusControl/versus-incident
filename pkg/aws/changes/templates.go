package changes

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Field is one identifier a template may read from an event. Path is a dotted
// path rooted at requestParameters or responseElements; numeric segments
// index arrays. Only these fields survive normalization.
type Field struct {
	Name string
	Path string
}

// Template renders one event name as a plain-language change. Texts are tried
// in order; the first whose placeholders all resolve wins, so an event with
// fewer identifiers still reads as a sentence.
type Template struct {
	ID          string
	EventSource string
	EventName   string
	Texts       []string
	Fields      []Field
	// ResourceFields names the fields that identify the changed resource.
	ResourceFields []string
	// Tags is the path of the tags the call sets: a list of key/value
	// objects or a key-to-value map. Each becomes a "tag:<key>" field whose
	// value is never kept (see TagFieldPrefix).
	Tags string
}

// GenericTemplateID marks a change rendered by the fallback.
const GenericTemplateID = "generic"

var templates = []Template{
	{ID: "ecs.register_task_definition", EventSource: "ecs.amazonaws.com", EventName: "RegisterTaskDefinition",
		Texts:          []string{"New task definition {family}:{revision} registered", "New task definition for {family} registered", "New ECS task definition registered"},
		Fields:         []Field{{"family", "requestParameters.family"}, {"revision", "responseElements.taskDefinition.revision"}, {"task_definition_arn", "responseElements.taskDefinition.taskDefinitionArn"}},
		ResourceFields: []string{"task_definition_arn"}},
	{ID: "ecs.update_service", EventSource: "ecs.amazonaws.com", EventName: "UpdateService",
		Texts:          []string{"Service {service} updated to task definition {task_definition}", "Service {service} scaled to {desired} tasks", "Service {service} updated", "ECS service updated"},
		Fields:         []Field{{"service", "requestParameters.service"}, {"cluster", "requestParameters.cluster"}, {"task_definition", "requestParameters.taskDefinition"}, {"desired", "requestParameters.desiredCount"}, {"service_arn", "responseElements.service.serviceArn"}},
		ResourceFields: []string{"service_arn", "service"}},
	{ID: "ecs.create_service", EventSource: "ecs.amazonaws.com", EventName: "CreateService",
		Texts:          []string{"Service {service} created in cluster {cluster}", "Service {service} created", "ECS service created"},
		Fields:         []Field{{"service", "requestParameters.serviceName"}, {"cluster", "requestParameters.cluster"}, {"service_arn", "responseElements.service.serviceArn"}},
		ResourceFields: []string{"service_arn", "service"}},
	{ID: "ecs.delete_service", EventSource: "ecs.amazonaws.com", EventName: "DeleteService",
		Texts:          []string{"Service {service} deleted", "ECS service deleted"},
		Fields:         []Field{{"service", "requestParameters.service"}, {"cluster", "requestParameters.cluster"}},
		ResourceFields: []string{"service"}},
	{ID: "lambda.update_code", EventSource: "lambda.amazonaws.com", EventName: "UpdateFunctionCode20150331v2",
		Texts:          []string{"Code updated for function {function}", "Lambda function code updated"},
		Fields:         []Field{{"function", "requestParameters.functionName"}},
		ResourceFields: []string{"function"}},
	{ID: "lambda.update_configuration", EventSource: "lambda.amazonaws.com", EventName: "UpdateFunctionConfiguration20150331v2",
		Texts:          []string{"Configuration changed for function {function}", "Lambda function configuration changed"},
		Fields:         []Field{{"function", "requestParameters.functionName"}, {"environment", "requestParameters.environment.variables"}},
		ResourceFields: []string{"function"}},
	{ID: "lambda.publish_version", EventSource: "lambda.amazonaws.com", EventName: "PublishVersion20150331",
		Texts:          []string{"Version {version} published for function {function}", "New version published for function {function}", "Lambda version published"},
		Fields:         []Field{{"function", "requestParameters.functionName"}, {"version", "responseElements.version"}},
		ResourceFields: []string{"function"}},
	{ID: "lambda.delete_function", EventSource: "lambda.amazonaws.com", EventName: "DeleteFunction20150331",
		Texts:          []string{"Function {function} deleted", "Lambda function deleted"},
		Fields:         []Field{{"function", "requestParameters.functionName"}},
		ResourceFields: []string{"function"}},
	{ID: "lambda.put_concurrency", EventSource: "lambda.amazonaws.com", EventName: "PutFunctionConcurrency20171031",
		Texts:          []string{"Reserved concurrency for function {function} set to {concurrency}", "Reserved concurrency changed for function {function}", "Lambda reserved concurrency changed"},
		Fields:         []Field{{"function", "requestParameters.functionName"}, {"concurrency", "requestParameters.reservedConcurrentExecutions"}},
		ResourceFields: []string{"function"}},
	{ID: "ec2.run_instances", EventSource: "ec2.amazonaws.com", EventName: "RunInstances",
		Texts:          []string{"Instance {instance} launched ({instance_type})", "Instance {instance} launched", "EC2 instance launched"},
		Fields:         []Field{{"instance", "responseElements.instancesSet.items.0.instanceId"}, {"instance_type", "requestParameters.instanceType"}, {"user_data", "requestParameters.userData"}},
		ResourceFields: []string{"instance"}},
	{ID: "ec2.terminate_instances", EventSource: "ec2.amazonaws.com", EventName: "TerminateInstances",
		Texts:          []string{"Instance {instance} terminated", "EC2 instance terminated"},
		Fields:         []Field{{"instance", "requestParameters.instancesSet.items.0.instanceId"}},
		ResourceFields: []string{"instance"}},
	{ID: "ec2.stop_instances", EventSource: "ec2.amazonaws.com", EventName: "StopInstances",
		Texts:          []string{"Instance {instance} stopped", "EC2 instance stopped"},
		Fields:         []Field{{"instance", "requestParameters.instancesSet.items.0.instanceId"}},
		ResourceFields: []string{"instance"}},
	{ID: "ec2.start_instances", EventSource: "ec2.amazonaws.com", EventName: "StartInstances",
		Texts:          []string{"Instance {instance} started", "EC2 instance started"},
		Fields:         []Field{{"instance", "requestParameters.instancesSet.items.0.instanceId"}},
		ResourceFields: []string{"instance"}},
	{ID: "ec2.modify_instance_attribute", EventSource: "ec2.amazonaws.com", EventName: "ModifyInstanceAttribute",
		Texts:          []string{"Settings changed on instance {instance}", "EC2 instance settings changed"},
		Fields:         []Field{{"instance", "requestParameters.instanceId"}},
		ResourceFields: []string{"instance"}},
	{ID: "ec2.authorize_ingress", EventSource: "ec2.amazonaws.com", EventName: "AuthorizeSecurityGroupIngress",
		Texts:          []string{"Inbound rule added to security group {group}", "Inbound rule added to a security group"},
		Fields:         []Field{{"group", "requestParameters.groupId"}},
		ResourceFields: []string{"group"}},
	{ID: "ec2.revoke_ingress", EventSource: "ec2.amazonaws.com", EventName: "RevokeSecurityGroupIngress",
		Texts:          []string{"Inbound rule removed from security group {group}", "Inbound rule removed from a security group"},
		Fields:         []Field{{"group", "requestParameters.groupId"}},
		ResourceFields: []string{"group"}},
	{ID: "ec2.replace_route", EventSource: "ec2.amazonaws.com", EventName: "ReplaceRoute",
		Texts:          []string{"Route {destination} changed in route table {route_table}", "Route changed in route table {route_table}", "Route table entry changed"},
		Fields:         []Field{{"route_table", "requestParameters.routeTableId"}, {"destination", "requestParameters.destinationCidrBlock"}},
		ResourceFields: []string{"route_table"}},
	{ID: "autoscaling.update_group", EventSource: "autoscaling.amazonaws.com", EventName: "UpdateAutoScalingGroup",
		Texts:          []string{"Auto Scaling group {group} updated", "Auto Scaling group updated"},
		Fields:         []Field{{"group", "requestParameters.autoScalingGroupName"}},
		ResourceFields: []string{"group"}},
	{ID: "autoscaling.set_desired_capacity", EventSource: "autoscaling.amazonaws.com", EventName: "SetDesiredCapacity",
		Texts:          []string{"Auto Scaling group {group} set to {desired} instances", "Auto Scaling group {group} capacity changed", "Auto Scaling capacity changed"},
		Fields:         []Field{{"group", "requestParameters.autoScalingGroupName"}, {"desired", "requestParameters.desiredCapacity"}},
		ResourceFields: []string{"group"}},
	{ID: "elbv2.modify_target_group", EventSource: "elasticloadbalancing.amazonaws.com", EventName: "ModifyTargetGroup",
		Texts:          []string{"Health check settings changed on target group {target_group}", "Target group settings changed"},
		Fields:         []Field{{"target_group", "requestParameters.targetGroupArn"}},
		ResourceFields: []string{"target_group"}},
	{ID: "elbv2.register_targets", EventSource: "elasticloadbalancing.amazonaws.com", EventName: "RegisterTargets",
		Texts:          []string{"Targets registered with target group {target_group}", "Targets registered with a target group"},
		Fields:         []Field{{"target_group", "requestParameters.targetGroupArn"}},
		ResourceFields: []string{"target_group"}},
	{ID: "elbv2.deregister_targets", EventSource: "elasticloadbalancing.amazonaws.com", EventName: "DeregisterTargets",
		Texts:          []string{"Targets removed from target group {target_group}", "Targets removed from a target group"},
		Fields:         []Field{{"target_group", "requestParameters.targetGroupArn"}},
		ResourceFields: []string{"target_group"}},
	{ID: "elbv2.modify_listener", EventSource: "elasticloadbalancing.amazonaws.com", EventName: "ModifyListener",
		Texts:          []string{"Load balancer {listener} changed", "Load balancer listener changed"},
		Fields:         []Field{{"listener", "requestParameters.listenerArn"}},
		ResourceFields: []string{"listener"}},
	{ID: "rds.modify_instance", EventSource: "rds.amazonaws.com", EventName: "ModifyDBInstance",
		Texts:          []string{"Database {database} modified", "Database instance modified"},
		Fields:         []Field{{"database", "requestParameters.dBInstanceIdentifier"}},
		ResourceFields: []string{"database"}},
	{ID: "rds.reboot_instance", EventSource: "rds.amazonaws.com", EventName: "RebootDBInstance",
		Texts:          []string{"Database {database} rebooted", "Database instance rebooted"},
		Fields:         []Field{{"database", "requestParameters.dBInstanceIdentifier"}},
		ResourceFields: []string{"database"}},
	{ID: "rds.failover_cluster", EventSource: "rds.amazonaws.com", EventName: "FailoverDBCluster",
		Texts:          []string{"Failover started on database cluster {cluster}", "Database cluster failover started"},
		Fields:         []Field{{"cluster", "requestParameters.dBClusterIdentifier"}},
		ResourceFields: []string{"cluster"}},
	{ID: "cloudformation.create_stack", EventSource: "cloudformation.amazonaws.com", EventName: "CreateStack",
		Texts:          []string{"Stack {stack} created", "CloudFormation stack created"},
		Fields:         []Field{{"stack", "requestParameters.stackName"}},
		ResourceFields: []string{"stack"}},
	{ID: "cloudformation.update_stack", EventSource: "cloudformation.amazonaws.com", EventName: "UpdateStack",
		Texts:          []string{"Stack {stack} updated", "CloudFormation stack updated"},
		Fields:         []Field{{"stack", "requestParameters.stackName"}},
		ResourceFields: []string{"stack"}},
	{ID: "cloudformation.delete_stack", EventSource: "cloudformation.amazonaws.com", EventName: "DeleteStack",
		Texts:          []string{"Stack {stack} deleted", "CloudFormation stack deleted"},
		Fields:         []Field{{"stack", "requestParameters.stackName"}},
		ResourceFields: []string{"stack"}},
	{ID: "cloudformation.execute_change_set", EventSource: "cloudformation.amazonaws.com", EventName: "ExecuteChangeSet",
		Texts:          []string{"Change set {change_set} applied to stack {stack}", "Change set applied to stack {stack}", "CloudFormation change set applied"},
		Fields:         []Field{{"stack", "requestParameters.stackName"}, {"change_set", "requestParameters.changeSetName"}},
		ResourceFields: []string{"stack"}},
	{ID: "dynamodb.update_table", EventSource: "dynamodb.amazonaws.com", EventName: "UpdateTable",
		Texts:          []string{"Table {table} settings changed", "DynamoDB table settings changed"},
		Fields:         []Field{{"table", "requestParameters.tableName"}},
		ResourceFields: []string{"table"}},
	{ID: "sqs.set_queue_attributes", EventSource: "sqs.amazonaws.com", EventName: "SetQueueAttributes",
		Texts:          []string{"Settings changed on queue {queue}", "SQS queue settings changed"},
		Fields:         []Field{{"queue", "requestParameters.queueUrl"}},
		ResourceFields: []string{"queue"}},
	{ID: "iam.put_role_policy", EventSource: "iam.amazonaws.com", EventName: "PutRolePolicy",
		Texts:          []string{"Inline policy {policy} changed on role {role}", "Inline policy changed on role {role}", "IAM role policy changed"},
		Fields:         []Field{{"role", "requestParameters.roleName"}, {"policy", "requestParameters.policyName"}},
		ResourceFields: []string{"role"}},
	{ID: "iam.attach_role_policy", EventSource: "iam.amazonaws.com", EventName: "AttachRolePolicy",
		Texts:          []string{"Policy {policy} attached to role {role}", "Policy attached to role {role}", "IAM policy attached to a role"},
		Fields:         []Field{{"role", "requestParameters.roleName"}, {"policy", "requestParameters.policyArn"}},
		ResourceFields: []string{"role"}},
	{ID: "cloudwatch.put_metric_alarm", EventSource: "monitoring.amazonaws.com", EventName: "PutMetricAlarm",
		Texts:          []string{"Alarm {alarm} created or changed", "CloudWatch alarm created or changed"},
		Fields:         []Field{{"alarm", "requestParameters.alarmName"}},
		ResourceFields: []string{"alarm"}},
	{ID: "cloudwatch.disable_alarm_actions", EventSource: "monitoring.amazonaws.com", EventName: "DisableAlarmActions",
		Texts:          []string{"Actions muted on alarm {alarm}", "CloudWatch alarm actions muted"},
		Fields:         []Field{{"alarm", "requestParameters.alarmNames.0"}},
		ResourceFields: []string{"alarm"}},
	{ID: "s3.put_bucket_policy", EventSource: "s3.amazonaws.com", EventName: "PutBucketPolicy",
		Texts:          []string{"Bucket policy changed on {bucket}", "S3 bucket policy changed"},
		Fields:         []Field{{"bucket", "requestParameters.bucketName"}},
		ResourceFields: []string{"bucket"}},
	{ID: "s3.delete_public_access_block", EventSource: "s3.amazonaws.com", EventName: "DeleteBucketPublicAccessBlock",
		Texts:          []string{"Public access block removed from bucket {bucket}", "S3 public access block removed"},
		Fields:         []Field{{"bucket", "requestParameters.bucketName"}},
		ResourceFields: []string{"bucket"}},
	{ID: "eventbridge.put_rule", EventSource: "events.amazonaws.com", EventName: "PutRule",
		Texts:          []string{"Rule {rule} created or changed", "EventBridge rule created or changed"},
		Fields:         []Field{{"rule", "requestParameters.name"}, {"event_bus", "requestParameters.eventBusName"}},
		ResourceFields: []string{"rule"}},
	{ID: "ec2.create_tags", EventSource: "ec2.amazonaws.com", EventName: "CreateTags",
		Texts:          []string{"Tags set on {resource}", "EC2 tags set"},
		Fields:         []Field{{"resource", "requestParameters.resourcesSet.items.0.resourceId"}},
		ResourceFields: []string{"resource"}, Tags: "requestParameters.tagSet.items"},
	{ID: "lambda.tag_resource", EventSource: "lambda.amazonaws.com", EventName: "TagResource20170331v2",
		Texts:          []string{"Tags set on function {function}", "Lambda tags set"},
		Fields:         []Field{{"function", "requestParameters.resource"}},
		ResourceFields: []string{"function"}, Tags: "requestParameters.tags"},
	{ID: "ssm.put_parameter", EventSource: "ssm.amazonaws.com", EventName: "PutParameter",
		Texts:          []string{"Parameter {parameter} set", "SSM parameter set"},
		Fields:         []Field{{"parameter", "requestParameters.name"}, {"secret_value", "requestParameters.value"}},
		ResourceFields: []string{"parameter"}},
	{ID: "secretsmanager.put_secret_value", EventSource: "secretsmanager.amazonaws.com", EventName: "PutSecretValue",
		Texts:          []string{"New value stored for secret {name}", "Secrets Manager secret value changed"},
		Fields:         []Field{{"name", "requestParameters.secretId"}, {"secret_value", "requestParameters.secretString"}},
		ResourceFields: []string{"name"}},
	{ID: "iam.create_access_key", EventSource: "iam.amazonaws.com", EventName: "CreateAccessKey",
		Texts:          []string{"Access key created for user {user}", "IAM access key created"},
		Fields:         []Field{{"user", "requestParameters.userName"}, {"access_key", "responseElements.accessKey.secretAccessKey"}},
		ResourceFields: []string{"user"}},
	{ID: "eks.update_cluster_version", EventSource: "eks.amazonaws.com", EventName: "UpdateClusterVersion",
		Texts:          []string{"Cluster {cluster} upgrade to Kubernetes {version} started", "Cluster {cluster} upgrade started", "EKS cluster upgrade started"},
		Fields:         []Field{{"cluster", "requestParameters.name"}, {"version", "requestParameters.version"}},
		ResourceFields: []string{"cluster"}},
}

var templateIndex = func() map[string]Template {
	index := make(map[string]Template, len(templates))
	for _, template := range templates {
		index[template.EventSource+"\x00"+template.EventName] = template
	}
	return index
}()

// Templates returns a copy of the template table, sorted by ID.
func Templates() []Template {
	out := make([]Template, len(templates))
	copy(out, templates)
	sort.Slice(out, func(left, right int) bool { return out[left].ID < out[right].ID })
	return out
}

// Lookup finds the template for one event.
func Lookup(eventSource, eventName string) (Template, bool) {
	template, ok := templateIndex[eventSource+"\x00"+eventName]
	return template, ok
}

// Render fills the first text whose placeholders all resolve.
func (template Template) Render(fields map[string]string) string {
	for _, text := range template.Texts {
		if rendered, ok := fill(text, fields); ok {
			return rendered
		}
	}
	return template.Texts[len(template.Texts)-1]
}

func fill(text string, fields map[string]string) (string, bool) {
	var builder strings.Builder
	for {
		open := strings.IndexByte(text, '{')
		if open < 0 {
			builder.WriteString(text)
			return builder.String(), true
		}
		closing := strings.IndexByte(text[open:], '}')
		if closing < 0 {
			builder.WriteString(text)
			return builder.String(), true
		}
		name := text[open+1 : open+closing]
		value, ok := displayValue(fields[name])
		if !ok {
			return "", false
		}
		builder.WriteString(text[:open])
		builder.WriteString(value)
		text = text[open+closing+1:]
	}
}

// bareAccountID finds a 12-digit AWS account ID standing on its own.
var bareAccountID = regexp.MustCompile(`(^|[^0-9A-Za-z])[0-9]{12}([^0-9A-Za-z]|$)`)

// displayValue turns a field value into the name a sentence shows. An ARN,
// a partial Lambda ARN or an SQS queue URL becomes the resource name: the
// full reference, account ID included, stays in the change's resources and
// fields. A value that still carries an account ID or an ARN after that is
// refused, so the template falls back to a sentence without it.
func displayValue(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	switch {
	case strings.HasPrefix(value, "arn:"):
		value = arnDisplayName(value)
	case strings.HasPrefix(value, "https://sqs.") || strings.HasPrefix(value, "http://sqs."):
		value = value[strings.LastIndexByte(value, '/')+1:]
	default:
		// A partial Lambda ARN: "<account>:function:<name>".
		if parts := strings.SplitN(value, ":", 3); len(parts) == 3 && parts[1] == "function" && bareAccountID.MatchString(parts[0]) {
			value = parts[2]
		}
	}
	if value == "" || strings.Contains(value, "arn:") || bareAccountID.MatchString(value) {
		return "", false
	}
	return value, true
}

// arnDisplayName returns the readable name inside an ARN: a target group's
// name, "<load balancer> listener <id>" for a listener, a task definition's
// family and revision, a function's name, and otherwise the last part of the
// resource path. An ARN it cannot read yields "".
func arnDisplayName(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[5] == "" {
		return ""
	}
	service, resource := parts[2], parts[5]
	kind, rest := resource, ""
	if index := strings.IndexAny(resource, "/:"); index >= 0 {
		kind, rest = resource[:index], resource[index+1:]
	}
	if rest == "" {
		// Resources named directly, such as an SQS queue or an S3 bucket.
		return resource
	}
	segments := strings.Split(rest, "/")
	switch {
	case service == "elasticloadbalancing" && kind == "targetgroup":
		return segments[0]
	case service == "elasticloadbalancing" && (kind == "listener" || kind == "listener-rule"):
		// listener/<app|net|gwy>/<load balancer>/<lb id>/<listener id>
		if len(segments) >= 4 {
			return segments[1] + " listener " + segments[3]
		}
		return ""
	case service == "elasticloadbalancing" && kind == "loadbalancer":
		// loadbalancer/<app|net|gwy>/<name>/<id>, or a classic name.
		if len(segments) >= 3 {
			return segments[1]
		}
		return segments[0]
	case service == "cloudformation" && kind == "stack":
		// stack/<name>/<generated id>
		return segments[0]
	case service == "ssm" && kind == "parameter":
		return "/" + rest
	case service == "lambda", service == "secretsmanager", service == "ecs" && kind == "task-definition":
		return rest
	}
	return segments[len(segments)-1]
}

// serviceNames maps a CloudTrail event source to the catalog service ID and
// the label the generic sentence uses.
var serviceNames = map[string][2]string{
	"apigateway.amazonaws.com":           {"api-gateway", "API Gateway"},
	"autoscaling.amazonaws.com":          {"auto-scaling", "Auto Scaling"},
	"cloudformation.amazonaws.com":       {"cloudformation", "CloudFormation"},
	"cloudfront.amazonaws.com":           {"cloudfront", "CloudFront"},
	"dynamodb.amazonaws.com":             {"dynamodb", "DynamoDB"},
	"ec2.amazonaws.com":                  {"ec2", "EC2"},
	"ecs.amazonaws.com":                  {"ecs", "ECS"},
	"eks.amazonaws.com":                  {"eks", "EKS"},
	"elasticache.amazonaws.com":          {"elasticache", "ElastiCache"},
	"elasticloadbalancing.amazonaws.com": {"elbv2", "Elastic Load Balancing"},
	"events.amazonaws.com":               {"eventbridge", "EventBridge"},
	"health.amazonaws.com":               {"aws-health", "AWS Health"},
	"iam.amazonaws.com":                  {"iam", "IAM"},
	"kinesis.amazonaws.com":              {"kinesis", "Kinesis"},
	"lambda.amazonaws.com":               {"lambda", "Lambda"},
	"monitoring.amazonaws.com":           {"cloudwatch", "CloudWatch"},
	"rds.amazonaws.com":                  {"rds", "RDS"},
	"route53.amazonaws.com":              {"route53", "Route 53"},
	"s3.amazonaws.com":                   {"s3", "S3"},
	"secretsmanager.amazonaws.com":       {"secrets-manager", "Secrets Manager"},
	"ssm.amazonaws.com":                  {"ssm", "Systems Manager"},
	"sns.amazonaws.com":                  {"sns", "SNS"},
	"sqs.amazonaws.com":                  {"sqs", "SQS"},
	"states.amazonaws.com":               {"step-functions", "Step Functions"},
}

// ServiceForSource returns the catalog service ID for an event source, or
// the source's first DNS label when it is not in the catalog.
func ServiceForSource(eventSource string) string {
	if names, ok := serviceNames[eventSource]; ok {
		return names[0]
	}
	if index := strings.IndexByte(eventSource, '.'); index > 0 {
		return eventSource[:index]
	}
	return eventSource
}

// GenericSummary is the fallback sentence: the event name split into words
// plus the service label, for example "Put bucket tagging (S3)".
func GenericSummary(eventSource, eventName string) string {
	label := eventSource
	if names, ok := serviceNames[eventSource]; ok {
		label = names[1]
	}
	words := splitWords(trimVersionSuffix(eventName))
	if len(words) == 0 {
		return "Change recorded (" + label + ")"
	}
	return strings.Join(words, " ") + " (" + label + ")"
}

// BaseEventName is an event name without the API date some services append,
// so UpdateFunctionConfiguration20150331v2 reads UpdateFunctionConfiguration.
func BaseEventName(name string) string { return trimVersionSuffix(name) }

// trimVersionSuffix removes the API date some services append to event
// names, such as UpdateFunctionCode20150331v2.
func trimVersionSuffix(name string) string {
	end := len(name)
	for end > 0 && (unicode.IsDigit(rune(name[end-1])) || name[end-1] == 'v') {
		end--
	}
	if end < len(name)-4 {
		return name[:end]
	}
	return name
}

func splitWords(name string) []string {
	words := make([]string, 0, 4)
	current := make([]rune, 0, len(name))
	runes := []rune(name)
	for index, character := range runes {
		// An underscore or hyphen separates words (Token_POST reads
		// "Token POST") and is not kept.
		if character == '_' || character == '-' {
			if len(current) > 0 {
				words = append(words, string(current))
				current = current[:0]
			}
			continue
		}
		boundary := unicode.IsUpper(character) && len(current) > 0 &&
			(!unicode.IsUpper(runes[index-1]) || (index+1 < len(runes) && unicode.IsLower(runes[index+1])))
		if boundary {
			words = append(words, string(current))
			current = current[:0]
		}
		current = append(current, character)
	}
	if len(current) > 0 {
		words = append(words, string(current))
	}
	for index := range words {
		if index == 0 {
			continue
		}
		if !isAcronym(words[index]) {
			words[index] = strings.ToLower(words[index])
		}
	}
	return words
}

func isAcronym(word string) bool {
	if len(word) < 2 {
		return false
	}
	for _, character := range word {
		if !unicode.IsUpper(character) && !unicode.IsDigit(character) {
			return false
		}
	}
	return true
}

// arnScope is what a template needs to turn a name into a canonical ARN:
// the partition, the Region the event was recorded in and the account.
type arnScope struct {
	partition string
	region    string
	account   string
}

// regional reports whether the scope names a real Region. Events of the
// global stream carry the pseudo-Region "global", which no regional ARN may
// hold.
func (scope arnScope) regional() bool {
	return scope.region != "" && scope.region != globalRegion
}

func (scope arnScope) arn(service, resource string) (string, bool) {
	if !scope.regional() || scope.account == "" {
		return "", false
	}
	return "arn:" + scope.partition + ":" + service + ":" + scope.region + ":" + scope.account + ":" + resource, true
}

// globalRegion is the pseudo-Region of global-service events.
const globalRegion = "global"

// partitionFor picks the partition from the principal's ARN when the event
// has one, otherwise from the Region name.
func partitionFor(principalARN, region string) string {
	if strings.HasPrefix(principalARN, "arn:") {
		if parts := strings.SplitN(principalARN, ":", 3); len(parts) == 3 && parts[1] != "" {
			return parts[1]
		}
	}
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	case strings.HasPrefix(region, "us-iso-"):
		return "aws-iso"
	case strings.HasPrefix(region, "us-isob-"):
		return "aws-iso-b"
	}
	return "aws"
}

// AutoScalingGroupARN is the canonical reference of an Auto Scaling group.
// A real group ARN embeds a generated ID that neither CloudTrail requests
// nor alarm dimensions carry, so the canonical form names the group instead:
// "autoScalingGroup:*:autoScalingGroupName/<name>". Group names are unique per
// account and Region, so this names exactly one group; the "*" is the ID
// slot, not a pattern.
func AutoScalingGroupARN(partition, region, account, name string) string {
	return "arn:" + partition + ":autoscaling:" + region + ":" + account + ":autoScalingGroup:*:autoScalingGroupName/" + name
}

// CanonicalARN trims an ARN to the form changes are matched on: a Lambda
// function ARN loses its version or alias qualifier. Anything that is not a
// well-formed ARN yields false.
func CanonicalARN(value string) (string, bool) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] == "" || parts[2] == "" || parts[5] == "" {
		return "", false
	}
	if parts[2] == "lambda" && strings.HasPrefix(parts[5], "function:") {
		name := strings.TrimPrefix(parts[5], "function:")
		if index := strings.IndexByte(name, ':'); index >= 0 {
			name = name[:index]
		}
		if name == "" {
			return "", false
		}
		parts[5] = "function:" + name
	}
	return strings.Join(parts, ":"), true
}

// ec2Kinds maps an EC2 resource ID prefix to the ARN resource type. The
// prefix is part of the ID's syntax, so this is a lookup, not a guess.
var ec2Kinds = map[string]string{
	"i":        "instance",
	"sg":       "security-group",
	"rtb":      "route-table",
	"subnet":   "subnet",
	"vpc":      "vpc",
	"vpce":     "vpc-endpoint",
	"nat":      "natgateway",
	"igw":      "internet-gateway",
	"eni":      "network-interface",
	"vol":      "volume",
	"lt":       "launch-template",
	"eipalloc": "elastic-ip",
	"tgw":      "transit-gateway",
	"acl":      "network-acl",
}

func ec2ARN(id string, scope arnScope) (string, bool) {
	prefix, rest, found := strings.Cut(id, "-")
	if !found || rest == "" {
		return "", false
	}
	kind, ok := ec2Kinds[prefix]
	if !ok {
		return "", false
	}
	return scope.arn("ec2", kind+"/"+id)
}

// lambdaARN resolves a functionName parameter: a name, a name with a
// qualifier, a partial ARN ("<account>:function:<name>") or a full ARN.
func lambdaARN(value string, scope arnScope) (string, bool) {
	if strings.HasPrefix(value, "arn:") {
		return CanonicalARN(value)
	}
	if account, name, found := strings.Cut(value, ":function:"); found {
		scope.account = account
		value = name
	}
	if index := strings.IndexByte(value, ':'); index >= 0 {
		value = value[:index]
	}
	if value == "" || strings.Contains(value, "/") {
		return "", false
	}
	return scope.arn("lambda", "function:"+value)
}

// ecsServiceARN resolves a service and its cluster, each a name or an ARN.
// A request without a cluster runs on the "default" cluster.
func ecsServiceARN(service, cluster string, scope arnScope) (string, bool) {
	if strings.HasPrefix(service, "arn:") {
		return CanonicalARN(service)
	}
	if service == "" || strings.Contains(service, "/") {
		return "", false
	}
	if strings.HasPrefix(cluster, "arn:") {
		_, cluster, _ = strings.Cut(cluster, ":cluster/")
	}
	if cluster == "" {
		cluster = "default"
	}
	if strings.Contains(cluster, "/") {
		return "", false
	}
	return scope.arn("ecs", "service/"+cluster+"/"+service)
}

// sqsARN resolves a queue URL (https://sqs.<region>.amazonaws.com/<account>/<name>).
func sqsARN(value string, scope arnScope) (string, bool) {
	if strings.HasPrefix(value, "arn:") {
		return CanonicalARN(value)
	}
	rest, found := strings.CutPrefix(value, "https://")
	if !found {
		return "", false
	}
	host, path, _ := strings.Cut(rest, "/")
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return "", false
	}
	if labels := strings.Split(host, "."); len(labels) >= 4 && labels[0] == "sqs" {
		scope.region = labels[1]
	}
	scope.account = segments[0]
	return scope.arn("sqs", segments[1])
}

// nameARN builds "<prefix><name>" in the scope, or keeps a value that is
// already an ARN.
func nameARN(service, prefix, value string, scope arnScope) (string, bool) {
	if strings.HasPrefix(value, "arn:") {
		return CanonicalARN(value)
	}
	if value == "" || strings.ContainsAny(value, "/:") {
		return "", false
	}
	return scope.arn(service, prefix+value)
}

// resolveField resolves one template resource field to its canonical ARN.
// It returns false when the field cannot name exactly one resource.
func resolveField(source, field string, fields map[string]string, scope arnScope) (string, bool) {
	value := fields[field]
	if value == "" {
		return "", false
	}
	switch source {
	case "ecs.amazonaws.com":
		if field == "service" {
			return ecsServiceARN(value, fields["cluster"], scope)
		}
	case "lambda.amazonaws.com":
		return lambdaARN(value, scope)
	case "ec2.amazonaws.com":
		if strings.HasPrefix(value, "arn:") {
			return CanonicalARN(value)
		}
		return ec2ARN(value, scope)
	case "autoscaling.amazonaws.com":
		if strings.HasPrefix(value, "arn:") {
			return CanonicalARN(value)
		}
		if !scope.regional() || strings.ContainsAny(value, "/:") {
			return "", false
		}
		return AutoScalingGroupARN(scope.partition, scope.region, scope.account, value), true
	case "rds.amazonaws.com":
		// RDS stores identifiers in lower case.
		prefix := "db:"
		if field == "cluster" {
			prefix = "cluster:"
		}
		if strings.HasPrefix(value, "arn:") {
			return CanonicalARN(value)
		}
		return nameARN("rds", prefix, strings.ToLower(value), scope)
	case "cloudformation.amazonaws.com":
		// A stack ARN embeds a generated ID; by name the canonical form keeps
		// the ID slot as "*", as for Auto Scaling groups.
		if strings.HasPrefix(value, "arn:") {
			return CanonicalARN(value)
		}
		if strings.ContainsAny(value, "/:") {
			return "", false
		}
		return scope.arn("cloudformation", "stack/"+value+"/*")
	case "dynamodb.amazonaws.com":
		return nameARN("dynamodb", "table/", value, scope)
	case "sqs.amazonaws.com":
		return sqsARN(value, scope)
	case "iam.amazonaws.com":
		if strings.HasPrefix(value, "arn:") {
			return CanonicalARN(value)
		}
		if strings.ContainsAny(value, "/:") || scope.account == "" {
			return "", false
		}
		kind := "role/"
		if field == "user" {
			kind = "user/"
		}
		return "arn:" + scope.partition + ":iam::" + scope.account + ":" + kind + value, true
	case "ssm.amazonaws.com":
		if strings.HasPrefix(value, "arn:") {
			return CanonicalARN(value)
		}
		name := strings.TrimPrefix(value, "/")
		if name == "" || strings.Contains(name, ":") {
			return "", false
		}
		return scope.arn("ssm", "parameter/"+name)
	case "secretsmanager.amazonaws.com":
		// A secret ARN ends in a generated suffix the name does not carry,
		// so only a full ARN resolves.
		if strings.HasPrefix(value, "arn:") {
			return CanonicalARN(value)
		}
		return "", false
	case "monitoring.amazonaws.com":
		return nameARN("cloudwatch", "alarm:", value, scope)
	case "s3.amazonaws.com":
		return bucketARN(value, scope)
	case "events.amazonaws.com":
		if strings.HasPrefix(value, "arn:") {
			return CanonicalARN(value)
		}
		bus := fields["event_bus"]
		if strings.HasPrefix(bus, "arn:") {
			_, bus, _ = strings.Cut(bus, ":event-bus/")
		}
		if bus == "" || bus == "default" {
			return nameARN("events", "rule/", value, scope)
		}
		if strings.ContainsAny(bus, "/:") {
			return "", false
		}
		return nameARN("events", "rule/"+bus+"/", value, scope)
	case "eks.amazonaws.com":
		return nameARN("eks", "cluster/", value, scope)
	}
	if strings.HasPrefix(value, "arn:") {
		return CanonicalARN(value)
	}
	return "", false
}

func bucketARN(value string, scope arnScope) (string, bool) {
	if strings.HasPrefix(value, "arn:") {
		return CanonicalARN(value)
	}
	if value == "" || strings.ContainsAny(value, "/:") {
		return "", false
	}
	return "arn:" + scope.partition + ":s3:::" + value, true
}

// resolveName resolves one name from the event's own resource list, which
// carries no type: an ARN is kept, an EC2 ID resolves by its prefix, an S3
// name is a bucket and a Lambda name is a function. Anything else stays
// unresolved.
func resolveName(source, value string, scope arnScope) (string, bool) {
	if strings.HasPrefix(value, "arn:") {
		return CanonicalARN(value)
	}
	switch source {
	case "ec2.amazonaws.com":
		return ec2ARN(value, scope)
	case "s3.amazonaws.com":
		return bucketARN(value, scope)
	case "lambda.amazonaws.com":
		return lambdaARN(value, scope)
	}
	return "", false
}
