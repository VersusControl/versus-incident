package fakeaws

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The estate scenario: four accounts of one AWS Organization in a
// hub-and-spoke landing zone, shaped after a real estate. A network hub owns
// the transit gateway; a workloads account and a data account run one EKS
// cluster each; a shared-services account holds the registry, the DNS zones
// and CI. Every cross-account link is visible from one side only, as in AWS:
// the DNS account sees alias names but not the load balancers behind them,
// the hub sees every attachment, a spoke sees its own, and a resource policy
// shows only on the resource owner.
//
// The responses keep the forms real AWS uses and a tidy model would miss:
// the dualstack prefix and trailing dot on alias names, octal-escaped
// wildcard record names, URL-encoded IAM policy documents, an empty
// RoleLastUsed element, a deleted principal shown as its unique ID, pod IPs
// as load-balancer targets, the same Security Hub finding served from its
// own Region and from the aggregation Region, and the "not configured" and
// "denied" answers that are not failures.
const ScenarioEstateHubSpoke = "estate-hub-spoke"

// Estate accounts and the outside accounts their policies name.
const (
	EstateHub       = "123400000001"
	EstateWorkloads = "123400000002"
	EstateShared    = "123400000003"
	EstateData      = "123400000004"
	// EstateManagement is the organization's management account: the only
	// outside account a member can label, from DescribeOrganization.
	EstateManagement = "123400000090"
	// EstateSecurityTooling is the delegated GuardDuty and Security Hub
	// administrator.
	EstateSecurityTooling = "123400000091"
	// EstateVendor is a third party trusted with an ExternalId.
	EstateVendor = "555500006666"
	// EstateOutside owns a VPC associated with a private zone and the
	// peer of a transit gateway peering; no estate account can see it.
	EstateOutside = "555500007777"
	EstateOrgID   = "o-a1b2c3d4e5"
)

// Estate Regions: the opt-in Region that holds every workload, the
// default-enabled Region holding the landing-zone baseline and the Security
// Hub aggregator, and us-east-1 for global-service leftovers.
const (
	EstateHomeRegion        = "eu-south-2"
	EstateAggregationRegion = "eu-west-1"
	EstateGlobalRegion      = "us-east-1"
)

// Estate identifiers tests assert on.
const (
	EstateTransitGateway  = "tgw-0e1f2a3b4c5d6e7f8"
	EstatePublicZoneID    = "Z0100000PUBLICZONE01"
	EstatePrivateZoneID   = "Z0100000PRIVATEZONE1"
	EstatePublicZone      = "shop.example.com."
	EstatePrivateZone     = "svc.example.internal."
	EstateCloudFrontZone  = "Z2FDTNDATAQYW2"
	EstateDeletedRoleID   = "AROAEXAMPLEDELETED001"
	EstateDeletedUserID   = "AIDAEXAMPLEDELETED002"
	EstateSecurityFinding = "arn:aws:securityhub:" + EstateHomeRegion + ":" + EstateWorkloads +
		":subscription/aws-foundational-security-best-practices/v/1.0.0/EKS.1/finding/0b1c2d3e-0002-4000-8000-00000000eks1"
	// EstateExternalIDValue is the ExternalId the vendor trust carries. A
	// collector must never store it.
	EstateExternalIDValue = "estate-vendor-external-id-0001"
)

func init() { knownScenarios[ScenarioEstateHubSpoke] = true }

// EstateAccounts lists the four connected estate accounts.
func EstateAccounts() []string {
	return []string{EstateHub, EstateWorkloads, EstateShared, EstateData}
}

func isEstateAccount(id string) bool {
	switch id {
	case EstateHub, EstateWorkloads, EstateShared, EstateData:
		return true
	}
	return false
}

// estateVPC is the one workload VPC of each account, and the VPC of the
// outside account a private zone names.
func estateVPC(account string) string {
	switch account {
	case EstateHub:
		return "vpc-0100000000000a001"
	case EstateWorkloads:
		return "vpc-0200000000000b002"
	case EstateShared:
		return "vpc-0300000000000c003"
	case EstateData:
		return "vpc-0400000000000d004"
	}
	return "vpc-0f00000000000e0ff"
}

// estateCall is one request the estate answers.
type estateCall struct {
	request   awsRequest
	account   string
	region    string
	service   string
	operation string
	now       time.Time
}

func (c estateCall) home() bool { return c.region == EstateHomeRegion }

func (c estateCall) arn(service, resource string) string {
	return "arn:aws:" + service + ":" + c.region + ":" + c.account + ":" + resource
}

type estateHandler func(*Server, http.ResponseWriter, estateCall)

// estateHandlers maps service:Operation to the handler that answers it.
var estateHandlers = map[string]estateHandler{}

func registerEstate(handlers map[string]estateHandler) {
	for operation, handler := range handlers {
		estateHandlers[operation] = handler
	}
}

// EstateOperations lists every operation the estate scenario answers,
// sorted.
func EstateOperations() []string {
	operations := make([]string, 0, len(estateHandlers))
	for operation := range estateHandlers {
		operations = append(operations, operation)
	}
	sortStrings(operations)
	return operations
}

// writeEstateOperation answers a request in the estate scenario from one of
// the four estate accounts. Anything else is left to the other fixtures.
func (s *Server) writeEstateOperation(writer http.ResponseWriter, request awsRequest) bool {
	if request.scenario != ScenarioEstateHubSpoke || !isEstateAccount(request.account.ID) {
		return false
	}
	service, operation := estateOperation(request)
	handler, ok := estateHandlers[service+":"+operation]
	if !ok {
		return false
	}
	handler(s, writer, estateCall{request: request, account: request.account.ID, region: request.region,
		service: service, operation: operation, now: s.clock().Truncate(time.Hour)})
	return true
}

// estateQueryServices names the service of the query actions the estate
// answers when the request is not signed.
var estateQueryServices = map[string]string{
	"GetAccountAuthorizationDetails":       "iam",
	"GetRole":                              "iam",
	"GetCredentialReport":                  "iam",
	"DescribeTransitGateways":              "ec2",
	"DescribeTransitGatewayAttachments":    "ec2",
	"DescribeTransitGatewayVpcAttachments": "ec2",
	"DescribeLoadBalancers":                "elasticloadbalancing",
	"DescribeTargetGroups":                 "elasticloadbalancing",
	"DescribeTargetHealth":                 "elasticloadbalancing",
	"DescribeEvents":                       "rds",
}

// estateTargetServices names the service of an X-Amz-Target prefix.
var estateTargetServices = map[string]string{
	"TrentService":                         "kms",
	"AmazonEC2ContainerRegistry_V20150921": "ecr",
	"AWSOrganizationsV20161128":            "organizations",
	"CertificateManager":                   "acm",
	"ServiceQuotasV20190624":               "servicequotas",
}

// estateOperation resolves the service and operation of a request, from
// the Action parameter, the X-Amz-Target header or the REST method and path.
func estateOperation(request awsRequest) (string, string) {
	switch request.protocol {
	case protocolQuery:
		action := request.params.Get("Action")
		if service := estateQueryServices[action]; service != "" {
			return service, action
		}
		return request.service, action
	case protocolJSON:
		if service := estateTargetServices[request.target]; service != "" {
			return service, request.action
		}
		return request.service, request.action
	}
	return estateRESTOperation(request)
}

func estateRESTOperation(request awsRequest) (string, string) {
	path := request.path
	parts := strings.Split(strings.Trim(path, "/"), "/")
	get, post := request.method == http.MethodGet, request.method == http.MethodPost
	switch {
	case get && path == "/detector":
		return "guardduty", "ListDetectors"
	case post && len(parts) == 3 && parts[0] == "detector" && parts[2] == "findings":
		return "guardduty", "ListFindings"
	case post && len(parts) == 4 && parts[0] == "detector" && parts[3] == "get":
		return "guardduty", "GetFindings"
	case post && path == "/findings":
		return "securityhub", "GetFindings"
	case post && path == "/findings/list":
		return "inspector2", "ListFindings"
	case post && path == "/status/batch/get":
		return "inspector2", "BatchGetAccountStatus"
	case get && path == "/analyzer":
		return "access-analyzer", "ListAnalyzers"
	case post && path == "/getresourceshares":
		return "ram", "GetResourceShares"
	case post && path == "/getresourceshareassociations":
		return "ram", "GetResourceShareAssociations"
	case post && path == "/listresources":
		return "ram", "ListResources"
	case get && path == "/2013-04-01/hostedzone":
		return "route53", "ListHostedZones"
	case get && len(parts) == 3 && parts[1] == "hostedzone":
		return "route53", "GetHostedZone"
	case get && len(parts) == 4 && parts[1] == "hostedzone" && parts[3] == "rrset":
		return "route53", "ListResourceRecordSets"
	case get && strings.HasPrefix(path, "/2020-05-31/distribution"):
		return "cloudfront", "ListDistributions"
	case get && path == "/clusters":
		return "eks", "ListClusters"
	case get && path == "/findingAggregator/list":
		return "securityhub", "ListFindingAggregators"
	case get && len(parts) == 2 && parts[0] == "clusters":
		return "eks", "DescribeCluster"
	case post && len(parts) == 3 && parts[0] == "clusters" && parts[2] == "insights":
		return "eks", "ListInsights"
	case get && len(parts) == 4 && parts[0] == "clusters" && parts[2] == "insights":
		return "eks", "DescribeInsight"
	case get && strings.HasPrefix(path, "/2015-03-31/functions"):
		return "lambda", "ListFunctions"
	case get && len(parts) == 4 && parts[0] == "2021-10-31" && parts[3] == "url":
		return "lambda", "GetFunctionUrlConfig"
	case get && len(parts) == 4 && parts[0] == "2021-10-31" && parts[3] == "urls":
		return "lambda", "ListFunctionUrlConfigs"
	case get && path == "/":
		return "s3", "ListBuckets"
	case get && len(parts) == 1 && request.params.Has("policy"):
		return "s3", "GetBucketPolicy"
	}
	return request.service, request.action
}

// estateError answers an error in the shape the service's SDK parses:
// JSON 1.1 for the X-Amz-Target services, REST-JSON with X-Amzn-Errortype
// for the path-routed JSON services, S3's bare <Error>, EC2's
// <Response><Errors> and the query <ErrorResponse> for the rest.
func estateError(writer http.ResponseWriter, call estateCall, status int, code, message string) {
	switch call.service {
	case "kms", "ecr", "organizations", "acm", "servicequotas":
		writeBody(writer, "application/x-amz-json-1.1", status, `{"__type":`+quote(code)+`,"message":`+quote(message)+`}`)
	case "lambda":
		writer.Header().Set("X-Amzn-Errortype", code)
		writeBody(writer, "application/json", status, `{"Type":"User","Message":`+quote(message)+`}`)
	case "guardduty", "securityhub", "inspector2", "access-analyzer", "ram", "eks":
		writer.Header().Set("X-Amzn-Errortype", code)
		writeBody(writer, "application/json", status, `{"message":`+quote(message)+`}`)
	case "route53", "cloudfront":
		writeBody(writer, "application/xml", status, `<ErrorResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/">`+
			`<Error><Type>Sender</Type>`+xmlText("Code", code)+xmlText("Message", message)+`</Error>`+
			xmlText("RequestId", requestID)+`</ErrorResponse>`)
	default:
		writeErrorFor(writer, call.request, status, code, message)
	}
}

// estateTime formats a time the way each protocol family writes it.
func isoMillis(value time.Time) string { return value.UTC().Format("2006-01-02T15:04:05.000Z") }

func epochSeconds(value time.Time) float64 {
	return float64(value.UnixMilli()) / 1000
}

// lambdaTime is Lambda's LastModified form, offset without a colon.
func lambdaTime(value time.Time) string { return value.UTC().Format("2006-01-02T15:04:05.000-0700") }

// policyEncode is how IAM's query API returns a policy document: the JSON
// percent-encoded as RFC 3986, inside the XML element.
func policyEncode(document string) string {
	return strings.ReplaceAll(url.QueryEscape(document), "+", "%20")
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func itoa(n int) string { return strconv.Itoa(n) }
