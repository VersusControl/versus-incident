package awsfix

// The NAT spike story: one production account whose checkout-api ECS service
// moved from task definition :41 to :42 on Oct 20 (deployed by the
// github-actions-prod pipeline), after which its traffic through NAT gateway
// nat-1a stepped from $20 to $85 a day.

const (
	prodAccount = "111111111111"
	usEast1     = "us-east-1"
	arnEC2      = "arn:aws:ec2:us-east-1:111111111111:"
	arnELB      = "arn:aws:elasticloadbalancing:us-east-1:111111111111:"
	arnECS      = "arn:aws:ecs:us-east-1:111111111111:service/prod/"
	arnLambda   = "arn:aws:lambda:us-east-1:111111111111:function:"
	pipelineARN = "arn:aws:iam::111111111111:role/github-actions-prod"
	vpcProdARN  = arnEC2 + "vpc/vpc-prod"
)

// CostNatSpike is the FinOps story. The inventory has the VPC, the private
// subnet routed through nat-1a, an empty load balancer, a DynamoDB endpoint
// but no S3 or ECR endpoint. The change history also holds a route change
// outside the window, an unrelated function update and a failed call, and
// covers the whole window. The hub reports an unattached volume, both a
// rightsize and a stop for the same instance i-0abc, and a Savings Plans
// purchase; every day of November is marked Estimated.
func CostNatSpike() *CostScenario {
	nat := series(prodAccount, "EC2 - Other", "USE1-NatGateway-Bytes", "Usage", "checkout", "GB",
		step("2026-10-01", "20", "444"), step("2026-10-20", "85", "1889"))
	nat.ResourceID = "nat-1a"
	return &CostScenario{
		Name:        CostNatSpikeName,
		Currency:    "USD",
		Range:       DateRange{Start: "2026-10-01", End: "2026-11-09"},
		PartialFrom: "2026-11-01",
		Names:       map[string]string{prodAccount: "Production"},
		Series: []CostSeries{
			nat,
			series(prodAccount, "Amazon Elastic Compute Cloud - Compute", "USE1-BoxUsage:m6i.large", "Usage", "checkout", "Hrs",
				step("2026-10-01", "300", "48")),
			series(prodAccount, "Amazon Virtual Private Cloud", "USE1-PublicIPv4:IdleAddress", "Usage", "", "Hrs",
				step("2026-10-01", "0.12", "24")),
			series(prodAccount, "Amazon Elastic Load Balancing", "USE1-LoadBalancerUsage", "Usage", "", "Hrs",
				step("2026-10-01", "1.08", "48")),
		},
		Forecast:  Forecast{Mean: "9000", Low: "8800", High: "9200"},
		Coverage:  Coverage{Covered: "710", Total: "1000"},
		TagActive: true,
		Budgets:   []Budget{},
		Recommendations: []HubRecommendation{
			{AccountID: prodAccount, ActionType: "Rightsize", CurrentResourceType: "Ec2Instance", ResourceID: "i-0abc", Region: usEast1,
				CurrentResourceSummary: "m6i.large", RecommendedResourceSummary: "m6i.medium", EstimatedMonthlySavings: 61.32},
			{AccountID: prodAccount, ActionType: "Stop", CurrentResourceType: "Ec2Instance", ResourceID: "i-0abc", Region: usEast1,
				CurrentResourceSummary: "m6i.large", EstimatedMonthlySavings: 90},
			{AccountID: prodAccount, ActionType: "Delete", CurrentResourceType: "EbsVolume", ResourceID: "vol-9", Region: usEast1,
				EstimatedMonthlySavings: 8},
			{AccountID: prodAccount, ActionType: "PurchaseSavingsPlans", CurrentResourceType: "ComputeSavingsPlans", ResourceID: "sp-1", Region: usEast1,
				RecommendedResourceSummary: "$4/hour with one year term (NoUpfront) in AWS account " + prodAccount + " (Linked account level)", EstimatedMonthlySavings: 610},
		},
		ComputeOptimizer: []ComputeOptimizerItem{
			{Account: prodAccount, InstanceARN: arnEC2 + "instance/i-0abc", Name: "reporting", Current: "m6i.large", Target: "m6i.medium",
				Monthly: 42.5, Finding: "Overprovisioned"},
			{Account: prodAccount, InstanceARN: arnEC2 + "instance/i-0def", Name: "api", Current: "c6i.large", Target: "c6i.large",
				Monthly: 0, Finding: "Optimized"},
		},
		Inventory: []Resource{
			resource(vpcProdARN, "vpc", "vpc", "vpc-prod", map[string]string{"id": "vpc-prod"}),
			resource(arnEC2+"subnet/subnet-private-a", "vpc", "subnet", "private-a", map[string]string{"id": "subnet-private-a", "vpc_id": "vpc-prod"},
				Relation{Type: "in_vpc", Target: vpcProdARN}),
			resource(arnEC2+"route-table/rtb-private", "vpc", "route-table", "rtb-private", map[string]string{"id": "rtb-private"},
				Relation{Type: "attached_to", Target: arnEC2 + "subnet/subnet-private-a"},
				Relation{Type: "routes_to", Target: arnEC2 + "natgateway/nat-1a"}),
			available(resource(arnEC2+"natgateway/nat-1a", "vpc", "nat-gateway", "nat-1a", map[string]string{"id": "nat-1a", "vpc_id": "vpc-prod"},
				Relation{Type: "in_vpc", Target: vpcProdARN})),
			available(resource(arnEC2+"vpc-endpoint/vpce-ddb", "vpc", "vpc-endpoint", "com.amazonaws.us-east-1.dynamodb",
				map[string]string{"id": "vpce-ddb", "vpc_id": "vpc-prod"},
				Relation{Type: "in_vpc", Target: vpcProdARN})),
			resource(arnECS+"checkout-api", "ecs", "service", "checkout-api", map[string]string{"task_definition": "checkout-api:42"},
				Relation{Type: "in_subnet", Target: arnEC2 + "subnet/subnet-private-a"}),
			resource(arnLambda+"other-fn", "lambda", "function", "other-fn", nil),
			tagged(resource(arnELB+"loadbalancer/app/lb-empty/1", "elbv2", "load-balancer", "lb-empty", nil,
				Relation{Type: "routes_to", Target: arnELB + "targetgroup/tg-empty/1"}), map[string]string{"team": "platform"}),
			resource(arnELB+"targetgroup/tg-empty/1", "elbv2", "target-group", "tg-empty", nil),
			resource(arnELB+"loadbalancer/app/lb-web/2", "elbv2", "load-balancer", "lb-web", nil,
				Relation{Type: "routes_to", Target: arnELB + "targetgroup/tg-web/2"}),
			resource(arnELB+"targetgroup/tg-web/2", "elbv2", "target-group", "tg-web", nil,
				Relation{Type: "routes_to", Target: arnEC2 + "instance/i-0def"}),
		},
		Changes: []Change{
			{ID: "evt-checkout-42", Time: mustTime("2026-10-20T14:02:00Z"), Account: prodAccount, Region: usEast1, Service: "ecs",
				EventSource: "ecs.amazonaws.com", EventName: "UpdateService", Template: "ecs.update_service",
				Summary:   "ECS service checkout-api updated to task definition :42 (was :41)",
				Resources: []string{arnECS + "checkout-api"},
				Actor:     Actor{Class: "pipeline", PrincipalType: "AssumedRole", PrincipalARN: pipelineARN}},
			{ID: "evt-route", Time: mustTime("2026-10-15T20:00:00Z"), Account: prodAccount, Region: usEast1, Service: "ec2",
				EventSource: "ec2.amazonaws.com", EventName: "CreateRoute", Template: "ec2.create_route",
				Summary:   "Route added to route table rtb-private",
				Resources: []string{arnEC2 + "route-table/rtb-private"},
				Actor:     Actor{Class: "person", PrincipalType: "AssumedRole", PrincipalARN: "arn:aws:iam::111111111111:role/network-admin"}},
			{ID: "evt-noise", Time: mustTime("2026-10-20T10:00:00Z"), Account: prodAccount, Region: usEast1, Service: "lambda",
				EventSource: "lambda.amazonaws.com", EventName: "UpdateFunctionConfiguration", Template: "lambda.update_function",
				Summary:   "Function other-fn configuration updated",
				Resources: []string{arnLambda + "other-fn"},
				Actor:     Actor{Class: "person"}},
			{ID: "evt-failed", Time: mustTime("2026-10-20T09:00:00Z"), Account: prodAccount, Region: usEast1, Service: "ec2",
				EventSource: "ec2.amazonaws.com", EventName: "DeleteNatGateway", Template: "ec2.delete_nat_gateway",
				Summary:   "Attempt to delete NAT gateway nat-1a",
				Resources: []string{arnEC2 + "natgateway/nat-1a"},
				Actor:     Actor{Class: "person"}, ErrorCode: "AccessDenied"},
		},
		ChangeCoverage: []Interval{{From: mustTime("2026-09-25T00:00:00Z"), To: mustTime("2026-11-10T00:00:00Z")}},
	}
}

// CostNatSpikeTwoNATs is the NAT spike story with a second NAT gateway nat-1b
// in the same VPC serving the inventory app, which deployed at nearly the
// same time as checkout-api. Without resource-level cost the NAT line cannot
// be narrowed to nat-1a, so the two deploys tie and neither is Likely.
func CostNatSpikeTwoNATs() *CostScenario {
	s := CostNatSpike()
	s.Name = CostNatSpikeTwoNATsName
	s.Inventory = append(s.Inventory,
		resource(arnEC2+"subnet/subnet-private-b", "vpc", "subnet", "private-b", map[string]string{"id": "subnet-private-b", "vpc_id": "vpc-prod"},
			Relation{Type: "in_vpc", Target: vpcProdARN}),
		resource(arnEC2+"route-table/rtb-private-b", "vpc", "route-table", "rtb-private-b", map[string]string{"id": "rtb-private-b"},
			Relation{Type: "attached_to", Target: arnEC2 + "subnet/subnet-private-b"},
			Relation{Type: "routes_to", Target: arnEC2 + "natgateway/nat-1b"}),
		available(resource(arnEC2+"natgateway/nat-1b", "vpc", "nat-gateway", "nat-1b", map[string]string{"id": "nat-1b", "vpc_id": "vpc-prod"},
			Relation{Type: "in_vpc", Target: vpcProdARN})),
		withApp(resource(arnECS+"inventory-svc", "ecs", "service", "inventory-svc", map[string]string{"task_definition": "inventory-svc:7"},
			Relation{Type: "in_subnet", Target: arnEC2 + "subnet/subnet-private-b"}), "inventory"),
	)
	s.Changes = append(s.Changes, Change{ID: "evt-inventory-7", Time: mustTime("2026-10-20T13:30:00Z"), Account: prodAccount, Region: usEast1,
		Service: "ecs", EventSource: "ecs.amazonaws.com", EventName: "UpdateService", Template: "ecs.update_service",
		Summary:   "ECS service inventory-svc updated to task definition :7 (was :6)",
		Resources: []string{arnECS + "inventory-svc"},
		Actor:     Actor{Class: "pipeline", PrincipalType: "AssumedRole", PrincipalARN: pipelineARN}})
	return s
}

// resource is a production-account us-east-1 resource.
func resource(arn, service, kind, name string, attributes map[string]string, relations ...Relation) Resource {
	return Resource{ARN: arn, Service: service, Type: kind, Account: prodAccount, Region: usEast1, Name: name,
		Attributes: attributes, Relations: relations}
}

func available(r Resource) Resource { r.State = "available"; return r }

func tagged(r Resource, tags map[string]string) Resource { r.Tags = tags; return r }

func withApp(r Resource, app string) Resource { r.App = app; return r }
