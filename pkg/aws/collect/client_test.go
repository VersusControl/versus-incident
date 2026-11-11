package collect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

func TestErrorCodeIsClosedVocabulary(t *testing.T) {
	cases := map[string]error{
		"throttled":       &smithy.GenericAPIError{Code: "ThrottlingException", Message: "provider text"},
		"denied":          &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "provider text"},
		"not_on_plan":     &smithy.GenericAPIError{Code: "SubscriptionRequiredException"},
		"region_disabled": &smithy.GenericAPIError{Code: "AuthFailure"},
		"timeout":         context.DeadlineExceeded,
		"network":         errors.New("provider text"),
	}
	for want, err := range cases {
		if got := errorCode(err); got != want {
			t.Errorf("errorCode(%v) = %q, want %q", err, got, want)
		}
	}
	if errorCode(nil) != "" || !isThrottle(errLocalBackoff) {
		t.Fatal("nil or local backoff misclassified")
	}
	if transientCode(CodeDenied) || transientCode(CodeNotOnPlan) || !transientCode("throttled") || !transientCode("timeout") {
		t.Fatal("transient classification wrong")
	}
}

func TestHostsArePinned(t *testing.T) {
	if got, ok := host("ec2", "eu-west-1"); !ok || got != "ec2.eu-west-1.amazonaws.com" {
		t.Fatalf("ec2 host = %q", got)
	}
	if got, _ := host("cloudwatch", "cn-north-1"); got != "monitoring.cn-north-1.amazonaws.com.cn" {
		t.Fatalf("china host = %q", got)
	}
	if got, ok := host("iam", "eu-west-1"); !ok || got != "iam.amazonaws.com" {
		t.Fatalf("global host = %q", got)
	}
	if _, ok := host("ec2", "not a region"); ok {
		t.Fatal("bad region accepted")
	}
	if _, ok := host("unknown", "us-east-1"); ok {
		t.Fatal("unknown client accepted")
	}
}

type staticTransport struct{ body string }

func (transport staticTransport) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(transport.body)), ContentLength: -1}, nil
}

func TestPinnedClientRejectsOtherHostsAndHugeBodies(t *testing.T) {
	pinned := pinnedHTTPClient{client: staticTransport{body: "ok"}, host: "ec2.us-east-1.amazonaws.com"}
	request, _ := http.NewRequest(http.MethodGet, "https://evil.example.com/", nil)
	if _, err := pinned.Do(request); !errors.Is(err, awsauth.ErrNetwork) {
		t.Fatalf("other host = %v", err)
	}
	plain, _ := http.NewRequest(http.MethodGet, "http://ec2.us-east-1.amazonaws.com/", nil)
	if _, err := pinned.Do(plain); !errors.Is(err, awsauth.ErrNetwork) {
		t.Fatalf("plain http = %v", err)
	}
	huge := pinnedHTTPClient{client: staticTransport{body: strings.Repeat("x", maxPageBytes+1)}, host: "ec2.us-east-1.amazonaws.com"}
	good, _ := http.NewRequest(http.MethodGet, "https://ec2.us-east-1.amazonaws.com/", nil)
	if _, err := huge.Do(good); !errors.Is(err, awsauth.ErrNetwork) {
		t.Fatalf("huge body = %v", err)
	}
	if response, err := pinned.Do(good); err != nil || response.StatusCode != 200 {
		t.Fatalf("pinned request = %v", err)
	}
}

func TestServicesListsEveryUnitService(t *testing.T) {
	services := Services()
	if services[len(services)-1] != "cloudtrail" || len(services) != len(specs)+1 {
		t.Fatalf("services = %v", services)
	}
	for _, service := range services {
		if service == "cloudtrail" || service == "tagging" || service == "appregistry" {
			continue
		}
		if len(fixActions(service)) == 0 {
			t.Errorf("%s has no fix actions", service)
		}
	}
}

// TestThrottlingIsRecognisedOnEveryProtocol is the QA-372 repro: the
// fixture's throttle replies use each service's real error shape, so EC2
// (EC2 query), Lambda and EKS (REST-JSON), S3 (REST-XML), SQS (JSON) and
// CloudWatch (CBOR) all record throttled, never network.
func TestThrottlingIsRecognisedOnEveryProtocol(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	h.fake.SetThrottle(1)
	writer := h.store.Writer(productionAccount)
	for _, entry := range specs {
		switch entry.service {
		case "ec2", "lambda", "eks", "s3", "sqs", "cloudwatch", "elbv2", "api-gateway":
		default:
			continue
		}
		region := testRegion
		unit := model.Unit{Account: productionAccount, Region: region, Service: entry.service}
		if entry.global {
			unit.Region = model.GlobalRegion
		}
		// Each unit starts outside the shared backoff, so the code comes
		// from parsing the reply, not from the limiter.
		h.limiter.Succeeded(productionAccount, region)
		c := &call{env: env{account: productionAccount, region: region, credentials: testCredentials(productionAccount),
			httpClient: h.fake.HTTPClient(), limiter: h.limiter, sleep: noSleep},
			unit: unit, partition: "aws", endpointRegion: region, now: time.Now(), writer: writer}
		report := h.collector.runUnit(context.Background(), c, entry)
		if report.ErrorCode != codeThrottledWord {
			t.Errorf("%s throttle recorded as %q", entry.service, report.ErrorCode)
		}
	}
}
