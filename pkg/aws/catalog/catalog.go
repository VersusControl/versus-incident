// Package catalog is the single table of AWS services Versus can read. The
// settings grid, the probes, the fix text, and the future policy lint all
// derive from this list, so the UI and the read path cannot drift.
package catalog

// Scope describes where a service's probe endpoint lives.
type Scope string

const (
	// ScopeRegion means the probe runs in the account's home region.
	ScopeRegion Scope = "region"
	// ScopeGlobalHome means the service has a single global endpoint and the
	// probe runs through the us-east-1 limiter.
	ScopeGlobalHome Scope = "global-home"
)

// GlobalHomeRegion is the region every global service is reached through.
const GlobalHomeRegion = "us-east-1"

// Service is one readable AWS service and the smallest probe call that proves
// access to it.
type Service struct {
	// ID is the stable identifier used by API responses and storage.
	ID string
	// DisplayName is the label shown in the settings grid.
	DisplayName string
	// Operation is the read call the probe makes, in IAM notation.
	Operation string
	// Scope selects the region the probe runs in.
	Scope Scope
	// FixActions are the least-privilege IAM actions that clear a denial.
	FixActions []string
	// DenySafe reports whether AWS answers an unauthorized call with a
	// distinct authorization error. When false, an empty page cannot be told
	// apart from a denial, so the grid must present the result as not used
	// rather than as proof of an empty account.
	DenySafe bool
}

// Region returns the region the probe for this service must run in.
// A global service always goes through us-east-1; a regional service uses the
// account's home region and falls back to us-east-1 when none is known.
func (service Service) Region(homeRegion string) string {
	if service.Scope == ScopeGlobalHome || homeRegion == "" {
		return GlobalHomeRegion
	}
	return homeRegion
}

// services holds the v1 catalog in display order.
var services = []Service{
	{
		ID:          "api-gateway",
		DisplayName: "API Gateway",
		Operation:   "apigateway:GetRestApis",
		Scope:       ScopeRegion,
		FixActions:  []string{"apigateway:GET"},
		DenySafe:    true,
	},
	{
		ID:          "auto-scaling",
		DisplayName: "Auto Scaling",
		Operation:   "DescribeAutoScalingGroups",
		Scope:       ScopeRegion,
		FixActions:  []string{"autoscaling:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "aws-health",
		DisplayName: "AWS Health",
		Operation:   "health:DescribeEvents",
		Scope:       ScopeGlobalHome,
		FixActions:  []string{"health:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "cloudformation",
		DisplayName: "CloudFormation",
		Operation:   "DescribeStacks",
		Scope:       ScopeRegion,
		FixActions:  []string{"cloudformation:Describe*", "cloudformation:ListStackResources"},
		DenySafe:    true,
	},
	{
		ID:          "cloudfront",
		DisplayName: "CloudFront",
		Operation:   "ListDistributions",
		Scope:       ScopeGlobalHome,
		FixActions:  []string{"cloudfront:List*"},
		DenySafe:    true,
	},
	{
		ID:          "cloudtrail",
		DisplayName: "CloudTrail",
		Operation:   "LookupEvents",
		Scope:       ScopeRegion,
		FixActions:  []string{"cloudtrail:LookupEvents"},
		DenySafe:    true,
	},
	{
		ID:          "cloudwatch",
		DisplayName: "CloudWatch",
		Operation:   "DescribeAlarms",
		Scope:       ScopeRegion,
		FixActions:  []string{"cloudwatch:DescribeAlarms*"},
		DenySafe:    true,
	},
	{
		ID:          "config",
		DisplayName: "Config",
		Operation:   "DescribeConfigurationRecorders",
		Scope:       ScopeRegion,
		FixActions:  []string{"config:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "dynamodb",
		DisplayName: "DynamoDB",
		Operation:   "ListTables",
		Scope:       ScopeRegion,
		FixActions:  []string{"dynamodb:ListTables", "dynamodb:DescribeTable"},
		DenySafe:    true,
	},
	{
		ID:          "ec2",
		DisplayName: "EC2",
		Operation:   "DescribeInstances",
		Scope:       ScopeRegion,
		FixActions:  []string{"ec2:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "ecs",
		DisplayName: "ECS",
		Operation:   "ListClusters",
		Scope:       ScopeRegion,
		FixActions:  []string{"ecs:List*", "ecs:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "eks",
		DisplayName: "EKS",
		Operation:   "ListClusters",
		Scope:       ScopeRegion,
		FixActions:  []string{"eks:List*", "eks:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "elasticache",
		DisplayName: "ElastiCache",
		Operation:   "DescribeCacheClusters",
		Scope:       ScopeRegion,
		FixActions:  []string{"elasticache:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "elbv2",
		DisplayName: "ELBv2",
		Operation:   "DescribeLoadBalancers",
		Scope:       ScopeRegion,
		FixActions:  []string{"elasticloadbalancing:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "eventbridge",
		DisplayName: "EventBridge",
		Operation:   "ListRules",
		Scope:       ScopeRegion,
		FixActions:  []string{"events:List*", "events:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "iam",
		DisplayName: "IAM",
		Operation:   "ListRoles",
		Scope:       ScopeGlobalHome,
		FixActions:  []string{"iam:List*", "iam:GetRole"},
		DenySafe:    true,
	},
	{
		ID:          "kinesis",
		DisplayName: "Kinesis",
		Operation:   "ListStreams",
		Scope:       ScopeRegion,
		FixActions:  []string{"kinesis:List*", "kinesis:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "lambda",
		DisplayName: "Lambda",
		Operation:   "ListFunctions",
		Scope:       ScopeRegion,
		FixActions:  []string{"lambda:List*", "lambda:GetFunctionConfiguration"},
		DenySafe:    true,
	},
	{
		ID:          "rds",
		DisplayName: "RDS",
		Operation:   "DescribeDBInstances",
		Scope:       ScopeRegion,
		FixActions:  []string{"rds:Describe*"},
		DenySafe:    true,
	},
	{
		ID:          "route53",
		DisplayName: "Route 53",
		Operation:   "ListHealthChecks",
		Scope:       ScopeGlobalHome,
		FixActions:  []string{"route53:List*", "route53:GetHealthCheckStatus"},
		DenySafe:    true,
	},
	{
		ID:          "s3",
		DisplayName: "S3",
		Operation:   "ListBuckets",
		Scope:       ScopeGlobalHome,
		FixActions:  []string{"s3:ListAllMyBuckets", "s3:GetBucketPolicyStatus", "s3:GetBucketPublicAccessBlock", "s3:GetEncryptionConfiguration"},
		DenySafe:    true,
	},
	{
		ID:          "sns",
		DisplayName: "SNS",
		Operation:   "ListTopics",
		Scope:       ScopeRegion,
		FixActions:  []string{"sns:List*", "sns:GetTopicAttributes"},
		DenySafe:    true,
	},
	{
		ID:          "sqs",
		DisplayName: "SQS",
		Operation:   "ListQueues",
		Scope:       ScopeRegion,
		FixActions:  []string{"sqs:List*", "sqs:GetQueueAttributes"},
		DenySafe:    true,
	},
	{
		ID:          "step-functions",
		DisplayName: "Step Functions",
		Operation:   "ListStateMachines",
		Scope:       ScopeRegion,
		FixActions:  []string{"states:List*", "states:Describe*"},
		DenySafe:    true,
	},
}

// Services returns the catalog in display order. The slice and its
// FixActions are copies, so callers cannot mutate the table.
func Services() []Service {
	out := make([]Service, len(services))
	for index, service := range services {
		out[index] = service
		out[index].FixActions = append([]string(nil), service.FixActions...)
	}
	return out
}

// ByID looks up a service by its stable identifier.
func ByID(id string) (Service, bool) {
	for _, service := range services {
		if service.ID == id {
			copied := service
			copied.FixActions = append([]string(nil), service.FixActions...)
			return copied, true
		}
	}
	return Service{}, false
}
