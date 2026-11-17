package changes

import (
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
		Fields:         []Field{{"function", "requestParameters.functionName"}},
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
		Fields:         []Field{{"instance", "responseElements.instancesSet.items.0.instanceId"}, {"instance_type", "requestParameters.instanceType"}},
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
		Texts:          []string{"Listener {listener} changed", "Load balancer listener changed"},
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
		Fields:         []Field{{"rule", "requestParameters.name"}},
		ResourceFields: []string{"rule"}},
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
		value := fields[name]
		if value == "" {
			return "", false
		}
		builder.WriteString(text[:open])
		builder.WriteString(value)
		text = text[open+closing+1:]
	}
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
