package fakeaws

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type probe struct {
	service  string
	action   string
	protocol string
	target   string
	path     string
}

// probeCatalog is the 24-service read-only call catalog with the wire shape a
// real probe client uses for each.
func probeCatalog() []probe {
	return []probe{
		{service: "apigateway", action: "GetRestApis", protocol: protocolREST, path: "/restapis"},
		{service: "autoscaling", action: "DescribeAutoScalingGroups", protocol: protocolQuery},
		{service: "health", action: "DescribeEvents", protocol: protocolJSON, target: "AWSHealth_20160804"},
		{service: "cloudformation", action: "DescribeStacks", protocol: protocolQuery},
		{service: "cloudfront", action: "ListDistributions", protocol: protocolREST, path: "/2020-05-31/distribution"},
		{service: "cloudtrail", action: "LookupEvents", protocol: protocolJSON, target: "com.amazonaws.cloudtrail.v20131101.CloudTrail_20131101"},
		{service: "cloudwatch", action: "DescribeAlarms", protocol: protocolQuery},
		{service: "config", action: "DescribeConfigurationRecorders", protocol: protocolJSON, target: "StarlingDoveService"},
		{service: "dynamodb", action: "ListTables", protocol: protocolJSON, target: "DynamoDB_20120810"},
		{service: "ec2", action: "DescribeInstances", protocol: protocolQuery},
		{service: "ecs", action: "ListClusters", protocol: protocolJSON, target: "AmazonEC2ContainerServiceV20141113"},
		{service: "eks", action: "ListClusters", protocol: protocolREST, path: "/clusters"},
		{service: "elasticache", action: "DescribeCacheClusters", protocol: protocolQuery},
		{service: "elasticloadbalancing", action: "DescribeLoadBalancers", protocol: protocolQuery},
		{service: "events", action: "ListRules", protocol: protocolJSON, target: "AWSEvents"},
		{service: "iam", action: "ListRoles", protocol: protocolQuery},
		{service: "kinesis", action: "ListStreams", protocol: protocolJSON, target: "Kinesis_20131202"},
		{service: "lambda", action: "ListFunctions", protocol: protocolREST, path: "/2015-03-31/functions/"},
		{service: "rds", action: "DescribeDBInstances", protocol: protocolQuery},
		{service: "route53", action: "ListHealthChecks", protocol: protocolREST, path: "/2013-04-01/healthcheck"},
		{service: "s3", action: "ListBuckets", protocol: protocolREST, path: "/"},
		{service: "sns", action: "ListTopics", protocol: protocolQuery},
		{service: "sqs", action: "ListQueues", protocol: protocolQuery},
		{service: "states", action: "ListStateMachines", protocol: protocolJSON, target: "AWSStepFunctions"},
	}
}

type result struct {
	status      int
	body        string
	contentType string
}

func requestFor(t *testing.T, item probe, account Account, region string) *http.Request {
	t.Helper()
	if region == "" {
		region = account.HomeRegion
	}
	endpoint := "https://" + item.service + "." + region + ".amazonaws.com" + item.path
	method := http.MethodGet
	var body io.Reader
	switch item.protocol {
	case protocolQuery:
		form := url.Values{"Action": {item.action}, "Version": {"2016-11-15"}}
		endpoint = "https://" + item.service + "." + region + ".amazonaws.com/"
		method = http.MethodPost
		body = strings.NewReader(form.Encode())
	case protocolJSON:
		endpoint = "https://" + item.service + "." + region + ".amazonaws.com/"
		method = http.MethodPost
		body = strings.NewReader("{}")
	}
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAVERSUS/20261104/"+region+"/"+item.service+"/aws4_request, SignedHeaders=host, Signature=abc")
	request.Header.Set(AccountHeader, account.ID)
	if item.protocol == protocolQuery {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if item.protocol == protocolJSON {
		request.Header.Set("Content-Type", "application/x-amz-json-1.1")
		request.Header.Set("X-Amz-Target", item.target+"."+item.action)
	}
	return request
}

func send(t *testing.T, server *Server, request *http.Request) result {
	t.Helper()
	response, err := server.HTTPClient().Do(request)
	if err != nil {
		t.Fatalf("request %s: %v", request.URL.Path, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return result{status: response.StatusCode, body: string(body), contentType: response.Header.Get("Content-Type")}
}

func (r result) assertValidPage(t *testing.T) {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", r.status, r.body)
	}
	if strings.Contains(r.contentType, "json") {
		if !json.Valid([]byte(r.body)) {
			t.Fatalf("invalid JSON page: %s", r.body)
		}
		return
	}
	var document struct{}
	if err := xml.Unmarshal([]byte(r.body), &document); err != nil {
		t.Fatalf("invalid XML page: %v (%s)", err, r.body)
	}
}

func (r result) contains(t *testing.T, want string) {
	t.Helper()
	if !strings.Contains(r.body, want) {
		t.Fatalf("body missing %q: %s", want, r.body)
	}
}

func TestNewAccountConnectServesEveryProbe(t *testing.T) {
	server := New(t, "new-account-connect")
	if len(probeCatalog()) != 24 {
		t.Fatalf("probe catalog has %d services, want 24", len(probeCatalog()))
	}
	for _, account := range server.Accounts() {
		for _, item := range probeCatalog() {
			got := send(t, server, requestFor(t, item, account, ""))
			switch {
			case account.Name == "Staging" && item.service == "elasticache":
				if got.status != http.StatusForbidden || !strings.Contains(got.body, "AccessDenied") {
					t.Fatalf("staging elasticache = %d %s", got.status, got.body)
				}
			case account.Name == "Staging" && item.service == "health":
				if got.status != http.StatusBadRequest || !strings.Contains(got.body, "SubscriptionRequiredException") {
					t.Fatalf("staging health = %d %s", got.status, got.body)
				}
			default:
				got.assertValidPage(t)
			}
		}
	}
}

func TestStagingEmptyServicesAnswerZeroItems(t *testing.T) {
	server := New(t, "new-account-connect")
	staging := accountByID("444455556666")
	want := map[string]string{
		"config":  `"ConfigurationRecorders":[]`,
		"kinesis": `"StreamNames":[]`,
		"states":  `"stateMachines":[]`,
	}
	for _, item := range probeCatalog() {
		expected, ok := want[item.service]
		if !ok {
			continue
		}
		got := send(t, server, requestFor(t, item, staging, ""))
		got.assertValidPage(t)
		got.contains(t, expected)
	}
}

func TestProductionProbePagesCarryOneItem(t *testing.T) {
	server := New(t, "new-account-connect")
	production := accountByID("111122223333")
	checks := map[string]string{
		"ec2":                  "i-0123456789abcdef0",
		"rds":                  "versus-db",
		"elasticache":          "versus-cache",
		"autoscaling":          "versus-web",
		"cloudformation":       "versus-stack",
		"cloudwatch":           "versus-health",
		"elasticloadbalancing": "versus",
		"iam":                  "versus-reader",
		"sns":                  "versus-alarms",
		"sqs":                  "versus-jobs",
		"apigateway":           "versus-api",
		"lambda":               "versus-worker",
		"eks":                  "versus-eks",
		"dynamodb":             "versus-locks",
		"events":               "versus-checkpoint",
		"kinesis":              "versus-events",
		"states":               "versus-checkpoint",
		"cloudfront":           "EDFDVBD6EXAMPLE",
		"route53":              "abcd1234",
		"s3":                   "versus-logs",
		"cloudtrail":           "RunInstances",
		"config":               "default",
		"ecs":                  "cluster/versus",
		"health":               "EC2",
	}
	for _, item := range probeCatalog() {
		want, ok := checks[item.service]
		if !ok {
			t.Fatalf("no expected item for %s", item.service)
		}
		got := send(t, server, requestFor(t, item, production, ""))
		got.assertValidPage(t)
		got.contains(t, want)
	}
}

func TestRegionPresenceCalls(t *testing.T) {
	server := New(t, "new-account-connect")
	regions := probe{service: "ec2", action: "DescribeRegions", protocol: protocolQuery}
	got := send(t, server, requestFor(t, regions, accountByID("111122223333"), "us-east-1"))
	got.assertValidPage(t)
	for _, region := range []string{"us-east-1", "eu-west-1", "ap-southeast-1"} {
		got.contains(t, "<regionName>"+region+"</regionName>")
	}
	for _, item := range probeCatalog() {
		switch item.service {
		case "ec2", "lambda", "ecs", "rds":
			got := send(t, server, requestFor(t, item, accountByID("111122223333"), "eu-west-1"))
			got.assertValidPage(t)
		}
	}
}

func TestAssumeRoleValidatesExternalIDAndReturnsCredentials(t *testing.T) {
	server := New(t, "new-account-connect")
	server.SetExternalID("vrs-ext-abcdefgh12345678")

	ok := assumeRole(t, server, "vrs-ext-abcdefgh12345678")
	ok.assertValidPage(t)
	ok.contains(t, "<AccessKeyId>")
	ok.contains(t, "<SecretAccessKey>")
	ok.contains(t, "<SessionToken>")
	ok.contains(t, "arn:aws:iam::444455556666:role/versus-reader")

	mismatch := assumeRole(t, server, "vrs-ext-wrongwrongwrong")
	if mismatch.status != http.StatusForbidden || !strings.Contains(mismatch.body, "AccessDenied") {
		t.Fatalf("mismatch = %d %s", mismatch.status, mismatch.body)
	}
	if strings.Contains(mismatch.body, "SecretAccessKey") {
		t.Fatalf("mismatch leaked credentials: %s", mismatch.body)
	}

	missing := assumeRole(t, server, "")
	if missing.status != http.StatusForbidden {
		t.Fatalf("missing external id = %d %s", missing.status, missing.body)
	}
}

func TestAssumeRoleRejectsMalformedExternalIDShape(t *testing.T) {
	server := New(t, "new-account-connect")
	got := assumeRole(t, server, "not-an-external-id")
	if got.status != http.StatusForbidden || !strings.Contains(got.body, "AccessDenied") {
		t.Fatalf("malformed external id = %d %s", got.status, got.body)
	}
}

func TestAssumeDeniedScenarioReturnsEachErrorCode(t *testing.T) {
	server := New(t, "assume-denied")
	cases := []struct {
		selector string
		status   int
		code     string
		contains []string
	}{
		{AssumeTrustDenied, http.StatusForbidden, "AccessDenied",
			[]string{"is not authorized to perform: sts:AssumeRole on resource: arn:aws:iam::444455556666:role/versus-reader"}},
		{AssumeExternalIDMismatch, http.StatusForbidden, "AccessDenied",
			[]string{"the external id is not authorized"}},
		{AssumeRoleNotFound, http.StatusForbidden, "AccessDenied",
			[]string{"The role with name versus-reader cannot be found"}},
		{AssumeSessionDurationInvalid, http.StatusBadRequest, "ValidationError", nil},
	}
	for _, tc := range cases {
		request := assumeRoleRequest(t, "vrs-ext-abcdefgh12345678", "versus-host")
		request.Header.Set(AssumeErrorHeader, tc.selector)
		got := send(t, server, request)
		if got.status != tc.status {
			t.Fatalf("%s status = %d, want %d", tc.selector, got.status, tc.status)
		}
		got.contains(t, "<Code>"+tc.code+"</Code>")
		got.contains(t, "<RequestId>")
		for _, want := range tc.contains {
			got.contains(t, want)
		}
		if strings.Contains(got.body, "<Code>"+tc.selector+"</Code>") {
			t.Fatalf("%s leaked the selector as the wire code: %s", tc.selector, got.body)
		}
		if tc.code == "ValidationError" {
			lower := strings.ToLower(got.body)
			if !strings.Contains(lower, "durationseconds") || !strings.Contains(lower, "duration") {
				t.Fatalf("%s message lacks the duration wording: %s", tc.selector, got.body)
			}
		}
	}

	form := url.Values{"Action": {"AssumeRole"}, "ExternalId": {"vrs-ext-abcdefgh12345678"},
		"RoleSessionName": {"versus-host"}, "DurationSeconds": {"600"}}
	short, _ := http.NewRequest(http.MethodPost, "https://sts.us-east-1.amazonaws.com/", strings.NewReader(form.Encode()))
	short.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	short.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAVERSUS/20261104/us-east-1/sts/aws4_request")
	got := send(t, server, short)
	if got.status != http.StatusBadRequest || !strings.Contains(got.body, "<Code>ValidationError</Code>") {
		t.Fatalf("short duration = %d %s", got.status, got.body)
	}
	if !strings.Contains(got.body, "DurationSeconds") {
		t.Fatalf("short duration message = %s", got.body)
	}
}

func assumeRole(t *testing.T, server *Server, externalID string) result {
	t.Helper()
	return send(t, server, assumeRoleRequest(t, externalID, "versus-host"))
}

func assumeRoleRequest(t *testing.T, externalID, sessionName string) *http.Request {
	t.Helper()
	form := url.Values{
		"Action":          {"AssumeRole"},
		"RoleArn":         {"arn:aws:iam::444455556666:role/versus-reader"},
		"RoleSessionName": {sessionName},
		"DurationSeconds": {"900"},
	}
	if externalID != "" {
		form.Set("ExternalId", externalID)
	}
	request, err := http.NewRequest(http.MethodPost, "https://sts.us-east-1.amazonaws.com/", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build assume request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAVERSUS/20261104/us-east-1/sts/aws4_request")
	return request
}

func TestGetCallerIdentity(t *testing.T) {
	server := New(t, "new-account-connect")
	form := url.Values{"Action": {"GetCallerIdentity"}}
	request, _ := http.NewRequest(http.MethodPost, "https://sts.us-east-1.amazonaws.com/", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set(AccountHeader, "777788889999")
	got := send(t, server, request)
	got.assertValidPage(t)
	got.contains(t, "<Account>777788889999</Account>")
}

func TestThrottledAPIReturnsThrottlingException(t *testing.T) {
	server := New(t, "throttled-api")
	production := accountByID("111122223333")
	throttled, succeeded := 0, 0
	for _, item := range probeCatalog() {
		got := send(t, server, requestFor(t, item, production, ""))
		switch got.status {
		case http.StatusOK:
			succeeded++
		case http.StatusBadRequest:
			got.contains(t, "ThrottlingException")
			throttled++
		default:
			t.Fatalf("%s status = %d", item.service, got.status)
		}
	}
	if throttled == 0 || succeeded == 0 {
		t.Fatalf("throttled = %d, succeeded = %d; want a chosen fraction", throttled, succeeded)
	}

	server.SetThrottle(1)
	got := send(t, server, requestFor(t, probe{service: "ec2", action: "DescribeInstances", protocol: protocolQuery}, production, ""))
	if got.status != http.StatusBadRequest {
		t.Fatalf("full throttle status = %d", got.status)
	}
	got.contains(t, "ThrottlingException")

	assume := assumeRole(t, server, "vrs-ext-abcdefgh12345678")
	if assume.status != http.StatusOK {
		t.Fatalf("sts must not be throttled by the probe fraction: %d %s", assume.status, assume.body)
	}
}

func TestRegionDisabledFixture(t *testing.T) {
	server := New(t, "region-disabled")
	if got := server.DisabledRegions(); len(got) != 1 || got[0] != "ap-southeast-1" {
		t.Fatalf("disabled regions = %v", got)
	}
	production := accountByID("111122223333")
	regions := send(t, server, requestFor(t, probe{service: "ec2", action: "DescribeRegions", protocol: protocolQuery}, production, "ap-southeast-1"))
	regions.assertValidPage(t)
	regions.contains(t, "<regionName>ap-southeast-1</regionName>")

	lambda := probe{service: "lambda", action: "ListFunctions", protocol: protocolREST, path: "/2015-03-31/functions/"}
	denied := send(t, server, requestFor(t, lambda, production, "ap-southeast-1"))
	if denied.status != http.StatusBadRequest || !strings.Contains(denied.body, "AuthFailure") {
		t.Fatalf("disabled region = %d %s", denied.status, denied.body)
	}
	enabled := send(t, server, requestFor(t, lambda, production, "eu-west-1"))
	enabled.assertValidPage(t)

	global := probe{service: "route53", action: "ListHealthChecks", protocol: protocolREST, path: "/2013-04-01/healthcheck"}
	send(t, server, requestFor(t, global, production, "ap-southeast-1")).assertValidPage(t)
}

func TestSecretCanaryFixturePlantsEveryCanary(t *testing.T) {
	server := New(t, "secret-canary")
	canary := accountByID(CanaryAccountID)
	var bodies []string
	for _, item := range probeCatalog() {
		got := send(t, server, requestFor(t, item, canary, ""))
		if got.status != http.StatusOK {
			t.Fatalf("%s = %d %s", item.service, got.status, got.body)
		}
		bodies = append(bodies, got.body)
	}
	joined := strings.Join(bodies, "\n")
	for _, secret := range CanarySecrets() {
		if !strings.Contains(joined, secret) {
			t.Fatalf("canary %q missing from the fixture surface", secret)
		}
	}
}

func TestCanariesAreAbsentFromNormalScenario(t *testing.T) {
	server := New(t, "new-account-connect")
	production := accountByID("111122223333")
	var bodies []string
	for _, item := range probeCatalog() {
		got := send(t, server, requestFor(t, item, production, ""))
		if got.status != http.StatusOK {
			continue
		}
		bodies = append(bodies, got.body)
	}
	joined := strings.Join(bodies, "\n")
	for _, secret := range CanarySecrets() {
		if strings.Contains(joined, secret) {
			t.Fatalf("normal fixture leaked canary %q", secret)
		}
	}
}

func TestScenarioHeaderOverridesDefault(t *testing.T) {
	server := New(t, "new-account-connect")
	request := requestFor(t, probe{service: "lambda", action: "ListFunctions", protocol: protocolREST, path: "/2015-03-31/functions/"}, accountByID(CanaryAccountID), "")
	request.Header.Set(ScenarioHeader, "secret-canary")
	got := send(t, server, request)
	got.assertValidPage(t)
	got.contains(t, CanaryEnvValue)
}

func TestEnvScenarioSelectsFixture(t *testing.T) {
	t.Setenv(EnvScenario, "region-disabled")
	server := New(t, "")
	if server.Scenario() != "region-disabled" {
		t.Fatalf("scenario = %q", server.Scenario())
	}
}

func TestRequestCountersTrackPathAndOperation(t *testing.T) {
	server := New(t, "new-account-connect")
	if server.TotalRequests() != 0 {
		t.Fatalf("total before any call = %d", server.TotalRequests())
	}
	production := accountByID("111122223333")
	ec2 := probe{service: "ec2", action: "DescribeInstances", protocol: protocolQuery}
	send(t, server, requestFor(t, ec2, production, ""))
	send(t, server, requestFor(t, ec2, production, ""))
	lambda := probe{service: "lambda", action: "ListFunctions", protocol: protocolREST, path: "/2015-03-31/functions/"}
	send(t, server, requestFor(t, lambda, production, ""))

	if got := server.TotalRequests(); got != 3 {
		t.Fatalf("total = %d, want 3", got)
	}
	if got := server.OperationCount("ec2:DescribeInstances"); got != 2 {
		t.Fatalf("ec2 probe count = %d, want 2", got)
	}
	if got := server.OperationCount("lambda:ListFunctions"); got != 1 {
		t.Fatalf("lambda probe count = %d, want 1", got)
	}
	if got := server.PathCount("/"); got != 2 {
		t.Fatalf("path / count = %d, want 2", got)
	}
	if got := server.PathCount("/2015-03-31/functions/"); got != 1 {
		t.Fatalf("lambda path count = %d, want 1", got)
	}
}

func TestHTTPClientRoutesRealSDKHostToFixture(t *testing.T) {
	server := New(t, "new-account-connect")
	form := url.Values{"Action": {"GetCallerIdentity"}}
	request, _ := http.NewRequest(http.MethodPost, "https://sts.ap-southeast-1.amazonaws.com/", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set(AccountHeader, "777788889999")
	got := send(t, server, request)
	got.assertValidPage(t)
	got.contains(t, "<Account>777788889999</Account>")
	if got := server.OperationCount("sts:GetCallerIdentity"); got != 1 {
		t.Fatalf("sts count = %d, want 1", got)
	}
}

func TestUnknownScenarioIsRejected(t *testing.T) {
	if _, err := NewServer("no-such-scenario"); err == nil {
		t.Fatal("expected an error for an unknown scenario")
	}
}
