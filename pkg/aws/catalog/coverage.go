package catalog

import "sort"

// Unit services the collectors read that are not probe rows of their own:
// VPC data comes from the EC2 API, and the tag and application sources
// have their own APIs.
const (
	unitVPC            = "vpc"
	unitTagging        = "tagging"
	unitAppRegistry    = "appregistry"
	unitTransitGateway = "transit-gateway"
	unitRAM            = "ram"
	unitOrganizations  = "organizations"
	unitKMS            = "kms"
	unitECR            = "ecr"
	unitS3Policy       = "s3-policy"
)

// linkUnits are the unit services that read cross-account links through
// an API without a probe row, with the actions each needs. Transit gateways
// read through EC2, whose describe actions the EC2 row already grants.
var linkUnits = map[string][]string{
	unitRAM:           {"ram:ListResources", "ram:GetResourceShareAssociations"},
	unitOrganizations: {"organizations:DescribeOrganization"},
	unitKMS:           {"kms:ListKeys", "kms:GetKeyPolicy"},
	unitECR:           {"ecr:Describe*", "ecr:GetRepositoryPolicy"},
	unitS3Policy:      {"s3:ListAllMyBuckets", "s3:GetBucketPolicy"},
}

// FixActionsFor returns the least-privilege IAM actions that clear a denial
// for a collector unit service: the catalog row's, or the API a source
// without its own row reads through. Nil when the service is unknown.
func FixActionsFor(unitService string) []string {
	if actions, ok := linkUnits[unitService]; ok {
		return append([]string(nil), actions...)
	}
	switch unitService {
	case unitVPC, unitTransitGateway:
		unitService = "ec2"
	case unitTagging:
		return []string{"tag:GetResources"}
	case unitAppRegistry:
		return []string{"servicecatalog:ListApplications", "servicecatalog:ListAssociatedResources"}
	}
	if service, ok := ByID(unitService); ok {
		return service.FixActions
	}
	return nil
}

// operationFixes are the IAM actions that allow one detail call, keyed by
// unit service and operation. A coverage row's fix is pasted into the
// reader policy, so it names the action the denied call needs, which is
// not always the operation's own name (GetPublicAccessBlock needs
// s3:GetBucketPublicAccessBlock).
var operationFixes = map[string][]string{
	"appregistry/ListAssociatedResources":               {"servicecatalog:ListAssociatedResources"},
	"ecr/GetRepositoryPolicy":                           {"ecr:GetRepositoryPolicy"},
	"kms/GetKeyPolicy":                                  {"kms:GetKeyPolicy"},
	"ram/GetResourceShareAssociations":                  {"ram:GetResourceShareAssociations"},
	"route53/ListHostedZones":                           {"route53:ListHostedZones"},
	"route53/ListResourceRecordSets":                    {"route53:ListResourceRecordSets"},
	"s3-policy/GetBucketPolicy":                         {"s3:GetBucketPolicy"},
	"transit-gateway/DescribeTransitGatewayAttachments": {"ec2:DescribeTransitGatewayAttachments"},
	"aws-health/DescribeAffectedEntities":               {"health:DescribeAffectedEntities"},
	"cloudformation/ListStackResources":                 {"cloudformation:ListStackResources"},
	"ec2/DescribeInstanceStatus":                        {"ec2:DescribeInstanceStatus"},
	"ecs/DescribeServices":                              {"ecs:DescribeServices"},
	"eks/DescribeCluster":                               {"eks:DescribeCluster"},
	"eks/DescribeNodegroup":                             {"eks:DescribeNodegroup"},
	"eks/ListNodegroups":                                {"eks:ListNodegroups"},
	"elbv2/DescribeTargetHealth":                        {"elasticloadbalancing:DescribeTargetHealth"},
	"eventbridge/ListTargetsByRule":                     {"events:ListTargetsByRule"},
	"lambda/ListEventSourceMappings":                    {"lambda:ListEventSourceMappings"},
	"rds/DescribeDBClusters":                            {"rds:DescribeDBClusters"},
	"rds/DescribeEvents":                                {"rds:DescribeEvents"},
	"s3/GetBucketEncryption":                            {"s3:GetEncryptionConfiguration"},
	"s3/GetBucketPolicyStatus":                          {"s3:GetBucketPolicyStatus"},
	"s3/GetPublicAccessBlock":                           {"s3:GetBucketPublicAccessBlock"},
	"sns/ListSubscriptions":                             {"sns:ListSubscriptions"},
	"sqs/GetQueueAttributes":                            {"sqs:GetQueueAttributes"},
}

// FixFor returns the IAM actions that clear a denial of one read: the
// operation's own actions when it is a known detail call, otherwise the
// unit service's.
func FixFor(unitService, operation string) []string {
	if operation != "" {
		if actions, ok := operationFixes[unitService+"/"+operation]; ok {
			return append([]string(nil), actions...)
		}
	}
	return append([]string(nil), FixActionsFor(unitService)...)
}

// OperationFix returns the actions for one detail call and whether the
// call has its own entry.
func OperationFix(unitService, operation string) ([]string, bool) {
	actions, ok := operationFixes[unitService+"/"+operation]
	return append([]string(nil), actions...), ok
}

// OperationFixActions returns every action a detail-call fix names, sorted
// and without repeats, for checking the reader policy artefacts.
func OperationFixActions() []string {
	seen := map[string]bool{}
	var out []string
	for _, actions := range operationFixes {
		for _, action := range actions {
			if !seen[action] {
				seen[action] = true
				out = append(out, action)
			}
		}
	}
	sort.Strings(out)
	return out
}

// UnitFixActions returns every service-level fix action: the catalog rows
// and the unit services that read through another API. Sorted, no repeats.
func UnitFixActions() []string {
	seen := map[string]bool{}
	var out []string
	add := func(actions []string) {
		for _, action := range actions {
			if !seen[action] {
				seen[action] = true
				out = append(out, action)
			}
		}
	}
	for _, service := range services {
		add(service.FixActions)
	}
	for _, unit := range []string{unitVPC, unitTagging, unitAppRegistry, unitTransitGateway, unitRAM, unitOrganizations, unitKMS, unitECR, unitS3Policy} {
		add(FixActionsFor(unit))
	}
	sort.Strings(out)
	return out
}

// Impact is what a source that is not read leaves out: Hides is the data a
// Not read or Capped row is missing, CantRun the check an Off in AWS row
// cannot run. Both are short phrases the page prefixes with "Hides:" or
// "Can't run:".
type Impact struct {
	Hides   string `json:"hides,omitempty"`
	CantRun string `json:"cant_run,omitempty"`
}

// impacts maps a unit service onto its impact. The per-operation entries
// (service + "/" + operation) describe one detail call when it says more
// than the service line.
var impacts = map[string]Impact{
	"api-gateway":      {Hides: "which APIs exist and what they call"},
	"auto-scaling":     {Hides: "scaling groups and their instances"},
	"aws-health":       {Hides: "AWS events and scheduled maintenance"},
	"cloudformation":   {Hides: "stack status and which resources each stack owns"},
	"cloudfront":       {Hides: "distributions and their origins"},
	"cloudtrail":       {Hides: "who changed what, and the likely cause of a problem"},
	"cloudwatch":       {Hides: "alarms and what is alarming now"},
	"dynamodb":         {Hides: "tables and their settings"},
	"ec2":              {Hides: "instances and their status checks"},
	"ecs":              {Hides: "clusters, services and rollouts"},
	"eks":              {Hides: "clusters and node groups", CantRun: "who called the cluster API"},
	"elasticache":      {Hides: "cache clusters"},
	"elbv2":            {Hides: "load balancers, target groups and target health"},
	"eventbridge":      {Hides: "rules and what they trigger"},
	"iam":              {Hides: "roles and who can assume them"},
	"kinesis":          {Hides: "streams"},
	"lambda":           {Hides: "functions and what triggers them"},
	"rds":              {Hides: "databases and their events"},
	"route53":          {Hides: "health checks and which names point at load balancers"},
	"s3":               {Hides: "buckets and whether they are public"},
	"sns":              {Hides: "topics and their subscribers"},
	"sqs":              {Hides: "queues and their dead-letter queues"},
	"step-functions":   {Hides: "state machines"},
	unitVPC:            {Hides: "networks, subnets, routes and security groups", CantRun: "traffic evidence for exposure"},
	unitTagging:        {Hides: "tags and the applications they name"},
	unitAppRegistry:    {Hides: "applications registered in AWS"},
	"account":          {Hides: "everything in these accounts"},
	unitTransitGateway: {Hides: "which networks are joined through transit gateways"},
	unitRAM:            {Hides: "what is shared with or by other accounts"},
	unitOrganizations:  {Hides: "which organization these accounts belong to"},
	unitKMS:            {Hides: "keys and which accounts may use them"},
	unitECR:            {Hides: "image repositories and which accounts may pull from them"},
	unitS3Policy:       {Hides: "which accounts bucket policies let in"},

	"elbv2/DescribeTargetHealth":                        {Hides: "which targets are unhealthy"},
	"s3/GetPublicAccessBlock":                           {Hides: "whether those buckets are public"},
	"s3/GetBucketPolicyStatus":                          {Hides: "whether those bucket policies make them public"},
	"s3/GetBucketEncryption":                            {Hides: "whether those buckets are encrypted"},
	"cloudformation/ListStackResources":                 {Hides: "which resources those stacks own"},
	"aws-health/DescribeAffectedEntities":               {Hides: "which resources those AWS events affect"},
	"appregistry/ListAssociatedResources":               {Hides: "which resources belong to those applications"},
	"ecs/DescribeServices":                              {Hides: "services and rollouts in those clusters"},
	"eks/DescribeNodegroup":                             {Hides: "node groups and their sizes"},
	"eventbridge/ListTargetsByRule":                     {Hides: "what those rules trigger"},
	"sqs/GetQueueAttributes":                            {Hides: "encryption and dead-letter queues of those queues"},
	"sns/ListSubscriptions":                             {Hides: "who receives those topics"},
	"lambda/ListEventSourceMappings":                    {Hides: "which queues and streams trigger functions"},
	"ec2/DescribeInstanceStatus":                        {Hides: "instance status checks"},
	"rds/DescribeEvents":                                {Hides: "recent database events"},
	"rds/DescribeDBClusters":                            {Hides: "database clusters"},
	"eks/DescribeCluster":                               {Hides: "cluster settings and network exposure"},
	"eks/ListNodegroups":                                {Hides: "node groups"},
	"route53/ListHostedZones":                           {Hides: "which names point at load balancers and distributions"},
	"route53/ListResourceRecordSets":                    {Hides: "which names point at load balancers and distributions"},
	"transit-gateway/DescribeTransitGatewayAttachments": {Hides: "which networks are attached to transit gateways"},
	"ram/GetResourceShareAssociations":                  {Hides: "who those shares are shared with"},
	"kms/GetKeyPolicy":                                  {Hides: "which accounts those key policies let in"},
	"ecr/GetRepositoryPolicy":                           {Hides: "which accounts may pull from those repositories"},
	"s3-policy/GetBucketPolicy":                         {Hides: "which accounts those bucket policies let in"},
}

// ImpactFor returns what a source that is not read leaves out, preferring
// the operation's own line. An unknown source has an empty impact.
func ImpactFor(unitService, operation string) Impact {
	general := impacts[unitService]
	if operation == "" {
		return general
	}
	specific, ok := impacts[unitService+"/"+operation]
	if !ok {
		return general
	}
	if specific.CantRun == "" {
		specific.CantRun = general.CantRun
	}
	return specific
}
