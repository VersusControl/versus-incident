package probe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

type fakeTransport func(*http.Request) (*http.Response, error)

func (transport fakeTransport) Do(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func s3Response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        http.Header{"Content-Type": []string{"application/xml"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func proberWith(t *testing.T, transport aws.HTTPClient) *Prober {
	t.Helper()
	return New("111111111111", credentials.NewStaticCredentialsProvider("AKIAEXAMPLE", "secret", ""), nil, WithHTTPClient(transport))
}

func s3Service(t *testing.T) catalog.Service {
	t.Helper()
	service, ok := catalog.ByID("s3")
	if !ok {
		t.Fatal("s3 missing from the catalog")
	}
	return service
}

func TestEveryCatalogServiceHasOneProbe(t *testing.T) {
	for _, service := range catalog.Services() {
		if _, ok := probes[service.ID]; !ok {
			t.Errorf("catalog service %q has no probe", service.ID)
		}
	}
	for id := range probes {
		if _, ok := catalog.ByID(id); !ok {
			t.Errorf("probe %q has no catalog entry", id)
		}
	}
}

func TestProbeReportsReadableAndNotUsed(t *testing.T) {
	oneBucket := `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Buckets><Bucket><Name>alpha</Name><CreationDate>2024-01-01T00:00:00.000Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>`
	prober := proberWith(t, fakeTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "s3.amazonaws.com" {
			t.Errorf("probe reached %s", request.URL)
		}
		return s3Response(http.StatusOK, oneBucket), nil
	}))

	result := prober.Probe(context.Background(), s3Service(t), "eu-west-1")
	if result.Status != StatusReadable || result.Items != 1 || result.ErrorCode != "" || result.Retryable {
		t.Fatalf("unexpected readable result: %#v", result)
	}

	empty := `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Buckets></Buckets></ListAllMyBucketsResult>`
	prober = proberWith(t, fakeTransport(func(*http.Request) (*http.Response, error) {
		return s3Response(http.StatusOK, empty), nil
	}))
	result = prober.Probe(context.Background(), s3Service(t), "eu-west-1")
	if result.Status != StatusNotUsed || result.Items != 0 || result.Retryable {
		t.Fatalf("unexpected not-used result: %#v", result)
	}
}

func TestProbeReportsDeniedWithFixActions(t *testing.T) {
	denied := `<Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>req</RequestId></Error>`
	prober := proberWith(t, fakeTransport(func(*http.Request) (*http.Response, error) {
		return s3Response(http.StatusForbidden, denied), nil
	}))

	result := prober.Probe(context.Background(), s3Service(t), "eu-west-1")
	if result.Status != StatusDenied || result.ErrorCode != CodeDenied || result.Retryable {
		t.Fatalf("unexpected denied result: %#v", result)
	}
	if len(result.FixActions) != 4 || result.FixActions[0] != "s3:ListAllMyBuckets" {
		t.Fatalf("denied result lost its fix actions: %#v", result.FixActions)
	}
}

func TestProbeReportsThrottlingAsNotChecked(t *testing.T) {
	throttled := `<Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message><RequestId>req</RequestId></Error>`
	prober := proberWith(t, fakeTransport(func(*http.Request) (*http.Response, error) {
		return s3Response(http.StatusServiceUnavailable, throttled), nil
	}))

	result := prober.Probe(context.Background(), s3Service(t), "eu-west-1")
	if result.Status != StatusNotChecked || result.ErrorCode != CodeThrottled || !result.Retryable {
		t.Fatalf("unexpected throttled result: %#v", result)
	}
	if len(result.FixActions) != 0 {
		t.Fatalf("throttling must not look like a denial: %#v", result.FixActions)
	}
}

func TestProbeWithoutCredentialsIsNotChecked(t *testing.T) {
	prober := New("111111111111", nil, nil)
	result := prober.Probe(context.Background(), s3Service(t), "eu-west-1")
	if result.Status != StatusNotChecked || result.ErrorCode != awsauth.ErrNoServerIdentity.Error() {
		t.Fatalf("unexpected result without credentials: %#v", result)
	}
}

func TestProbeUnknownServiceIsNotChecked(t *testing.T) {
	prober := proberWith(t, fakeTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("unknown service must not call AWS")
		return nil, nil
	}))
	result := prober.Probe(context.Background(), catalog.Service{ID: "unknown"}, "eu-west-1")
	if result.Status != StatusNotChecked || result.ErrorCode != CodeNetwork {
		t.Fatalf("unexpected result for an unknown service: %#v", result)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		status    Status
		code      string
		retryable bool
	}{
		{"access denied", &smithy.GenericAPIError{Code: "AccessDenied"}, StatusDenied, CodeDenied, false},
		{"unauthorized operation", &smithy.GenericAPIError{Code: "UnauthorizedOperation"}, StatusDenied, CodeDenied, false},
		{"authorization error", &smithy.GenericAPIError{Code: "AuthorizationError"}, StatusDenied, CodeDenied, false},
		{"not on plan", &smithy.GenericAPIError{Code: "SubscriptionRequiredException"}, StatusNotOnPlan, CodeNotOnPlan, false},
		{"throttled", &smithy.GenericAPIError{Code: "RequestLimitExceeded"}, StatusNotChecked, CodeThrottled, true},
		{"region disabled", &smithy.GenericAPIError{Code: "RegionDisabledException"}, StatusNotChecked, CodeRegionDisabled, false},
		{"timeout", context.DeadlineExceeded, StatusNotChecked, CodeTimeout, true},
		{"transport", awsauth.ErrNetwork, StatusNotChecked, CodeNetwork, true},
		{"no server identity", awsauth.ErrNoServerIdentity, StatusNotChecked, awsauth.ErrNoServerIdentity.Error(), false},
		{"unknown", errors.New("opaque provider text"), StatusNotChecked, CodeNetwork, true},
		{"no endpoint in the region", &url.Error{Op: "Post", URL: "https://servicecatalog-appregistry.ap-southeast-5.amazonaws.com",
			Err: &net.DNSError{Err: "no such host", Name: "servicecatalog-appregistry.ap-southeast-5.amazonaws.com", IsNotFound: true}},
			StatusNotUsed, CodeNotInRegion, false},
		{"region not supported", &smithy.GenericAPIError{Code: "UnsupportedAPIEndpointException"}, StatusNotUsed, CodeNotInRegion, false},
		{"resolver cannot answer", &net.DNSError{Err: "server misbehaving", IsTemporary: true}, StatusNotChecked, CodeNetwork, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, code, retryable := classify(testCase.err)
			if status != testCase.status || code != testCase.code || retryable != testCase.retryable {
				t.Fatalf("got (%s, %s, %v)", status, code, retryable)
			}
		})
	}
}

func TestClassifyNeverEchoesProviderText(t *testing.T) {
	_, code, _ := classify(&smithy.GenericAPIError{Code: "SomethingElse", Message: "account 123 secret detail"})
	if strings.Contains(code, "secret") || strings.Contains(code, "123") {
		t.Fatalf("provider text escaped: %q", code)
	}
}

func TestBuildFixPolicy(t *testing.T) {
	policy, err := BuildFixPolicy([]Result{
		{Service: "s3", Status: StatusDenied},
		{Service: "dynamodb", Status: StatusDenied},
		{Service: "iam", Status: StatusReadable},
	})
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect   string   `json:"Effect"`
			Action   []string `json:"Action"`
			Resource string   `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(policy), &decoded); err != nil {
		t.Fatalf("policy is not valid JSON: %v", err)
	}
	if decoded.Version != "2012-10-17" || len(decoded.Statement) != 1 {
		t.Fatalf("unexpected policy shape: %#v", decoded)
	}
	statement := decoded.Statement[0]
	if statement.Effect != "Allow" || statement.Resource != "*" {
		t.Fatalf("unexpected statement: %#v", statement)
	}
	expected := []string{"dynamodb:DescribeTable", "dynamodb:ListTables", "s3:GetBucketPolicyStatus", "s3:GetBucketPublicAccessBlock", "s3:GetEncryptionConfiguration", "s3:ListAllMyBuckets"}
	if len(statement.Action) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, statement.Action)
	}
	for index, action := range expected {
		if statement.Action[index] != action {
			t.Fatalf("expected sorted actions %v, got %v", expected, statement.Action)
		}
	}
}

func TestBuildFixPolicyWithoutDenials(t *testing.T) {
	if _, err := BuildFixPolicy([]Result{{Service: "s3", Status: StatusReadable}}); !errors.Is(err, ErrNoDeniedServices) {
		t.Fatalf("expected ErrNoDeniedServices, got %v", err)
	}
}

func TestEndpointHost(t *testing.T) {
	cases := map[string]string{
		"ec2":         "ec2.us-east-1.amazonaws.com",
		"cloudwatch":  "monitoring.eu-west-1.amazonaws.com",
		"eventbridge": "events.eu-west-1.amazonaws.com",
		"elbv2":       "elasticloadbalancing.ap-south-1.amazonaws.com",
		"s3":          "s3.amazonaws.com",
		"iam":         "iam.amazonaws.com",
		"aws-health":  "health.us-east-1.amazonaws.com",
	}
	for id, expected := range cases {
		region := "us-east-1"
		if id == "cloudwatch" || id == "eventbridge" {
			region = "eu-west-1"
		}
		if id == "elbv2" {
			region = "ap-south-1"
		}
		host, ok := endpointHost(id, region)
		if !ok || host != expected {
			t.Errorf("%s: expected %q, got %q (%v)", id, expected, host, ok)
		}
	}
	if _, ok := endpointHost("unknown", "us-east-1"); ok {
		t.Error("unknown service resolved an endpoint")
	}
	if _, ok := endpointHost("ec2", "not-a-region"); ok {
		t.Error("invalid region resolved an endpoint")
	}
}

func TestPinnedClientRejectsAnotherHost(t *testing.T) {
	pinned := pinnedHTTPClient{
		client: fakeTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("a rejected request must not reach the transport")
			return nil, nil
		}),
		host: "s3.amazonaws.com",
	}
	request, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pinned.Do(request); !errors.Is(err, awsauth.ErrNetwork) {
		t.Fatalf("expected the pinned transport to refuse, got %v", err)
	}
}

func TestPinnedClientCapsResponseBody(t *testing.T) {
	oversized := strings.Repeat("x", maxResponseBytes+1)
	pinned := pinnedHTTPClient{
		client: fakeTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          io.NopCloser(strings.NewReader(oversized)),
				ContentLength: -1,
			}, nil
		}),
		host: "s3.amazonaws.com",
	}
	request, err := http.NewRequest(http.MethodGet, "https://s3.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pinned.Do(request); !errors.Is(err, awsauth.ErrNetwork) {
		t.Fatalf("expected the body cap to trip, got %v", err)
	}
}
