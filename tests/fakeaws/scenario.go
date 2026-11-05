package fakeaws

import (
	"net/http"
	"strconv"
)

// probeActions maps a service to the single read-only call the probe catalog
// uses for it.
var probeActions = map[string]string{
	"apigateway":           "GetRestApis",
	"autoscaling":          "DescribeAutoScalingGroups",
	"health":               "DescribeEvents",
	"cloudformation":       "DescribeStacks",
	"cloudfront":           "ListDistributions",
	"cloudtrail":           "LookupEvents",
	"cloudwatch":           "DescribeAlarms",
	"config":               "DescribeConfigurationRecorders",
	"dynamodb":             "ListTables",
	"ec2":                  "DescribeInstances",
	"ecs":                  "ListClusters",
	"eks":                  "ListClusters",
	"elasticache":          "DescribeCacheClusters",
	"elasticloadbalancing": "DescribeLoadBalancers",
	"events":               "ListRules",
	"iam":                  "ListRoles",
	"kinesis":              "ListStreams",
	"lambda":               "ListFunctions",
	"rds":                  "DescribeDBInstances",
	"route53":              "ListHealthChecks",
	"s3":                   "ListBuckets",
	"sns":                  "ListTopics",
	"sqs":                  "ListQueues",
	"states":               "ListStateMachines",
}

func (s *Server) emptyForAccount(request awsRequest) bool {
	return request.account.Empty[request.service]
}

// serve routes a parsed request to the operation that answers it.
func (s *Server) serve(writer http.ResponseWriter, request awsRequest) {
	if s.serveAssumeDenied(writer, request) {
		return
	}
	if request.account.Denied[request.service] {
		s.writeError(writer, request.protocol, http.StatusForbidden, "AccessDenied",
			"User is not authorized to perform: "+request.operation())
		return
	}
	s.writeOperation(writer, request)
}

// assumeErrorForRequest resolves which assume-role failure a request should
// see, if any.
func assumeErrorForRequest(request awsRequest) string {
	if request.scenario != "assume-denied" {
		return ""
	}
	if code := request.params.Get("__assume_error"); code != "" {
		return code
	}
	switch request.params.Get("RoleSessionName") {
	case AssumeTrustDenied:
		return AssumeTrustDenied
	case AssumeExternalIDMismatch:
		return AssumeExternalIDMismatch
	case AssumeRoleNotFound:
		return AssumeRoleNotFound
	case AssumeSessionDurationInvalid:
		return AssumeSessionDurationInvalid
	}
	if duration, err := strconv.Atoi(request.params.Get("DurationSeconds")); err == nil && duration < 900 {
		return AssumeSessionDurationInvalid
	}
	return ""
}
