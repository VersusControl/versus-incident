package collect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/health"
	healthtypes "github.com/aws/aws-sdk-go-v2/service/health/types"
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
		"region_disabled": &smithy.GenericAPIError{Code: "OptInRequired"},
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

// TestThrottlingIsRecognisedOnEveryProtocol reproduces a bug: the
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

// AWS answers some reads with an error code that only means a setting is
// not set. Those are answers, never gaps; a denial in any spelling is a
// denial, never a feature that is off.
func TestNotSetAnswersAndDenialVariants(t *testing.T) {
	for _, code := range []string{"NoSuchBucketPolicy", "NoSuchPublicAccessBlockConfiguration", "NoSuchTagSet",
		"NoSuchEntity", "RepositoryPolicyNotFoundException", "ReportNotPresent"} {
		if !notSetAnswer(&smithy.GenericAPIError{Code: code}) {
			t.Errorf("%s is not read as not set", code)
		}
	}
	for _, code := range []string{"AccessDenied", "NoSuchBucket", "ServerSideEncryptionConfigurationNotFoundError", ""} {
		if notSetAnswer(&smithy.GenericAPIError{Code: code}) {
			t.Errorf("%s is read as not set", code)
		}
	}
	if notSetAnswer(nil) {
		t.Error("a nil error is read as not set")
	}
	for _, code := range []string{"AccessDenied", "AccessDeniedException", "AccessDeniedFault", "AccessDeniedForDependencyException"} {
		if got := errorCode(&smithy.GenericAPIError{Code: code}); got != CodeDenied || model.GapReasonForCode(got) != model.GapDenied {
			t.Errorf("%s = %s, want denied", code, got)
		}
	}
}

// Failed calls of one per-item operation are one gap naming how many items
// were read, not one gap per call; a capped read keeps its own counts.
func TestDetailGapsMergeIntoOnePerOperation(t *testing.T) {
	c := &call{unit: model.Unit{Account: productionAccount, Region: testRegion, Service: "sqs"}}
	denied := &smithy.GenericAPIError{Code: "AccessDeniedException"}
	var gaps []model.Gap
	for index := range 35 {
		c.tried("GetQueueAttributes")
		if index < 7 {
			gaps = append(gaps, c.detailGap("GetQueueAttributes", denied))
		}
	}
	gaps = append(gaps, c.detailGap("ListEventSourceMappings", denied))
	gaps = append(gaps, c.capGap("GetQueueAttributes", 35, 0), c.capGap("GetQueueAttributes", 35, 0))
	merged := c.mergeDetailGaps(gaps)
	if len(merged) != 3 {
		t.Fatalf("merged = %+v", merged)
	}
	if got := merged[0]; got.Operation != "GetQueueAttributes" || got.Reason != model.GapDenied || got.Seen != 28 || got.Total != 35 {
		t.Fatalf("partial denial = %+v, want 28 of 35", got)
	}
	if got := merged[1]; got.Operation != "ListEventSourceMappings" || got.Seen != 0 || got.Total != 0 {
		t.Fatalf("single call = %+v, want no counts", got)
	}
	if got := merged[2]; got.Reason != model.GapCapped || got.Outcome() != model.OutcomeCapped || got.Seen != 35 || got.Total != 0 {
		t.Fatalf("capped = %+v, want more than 35", got)
	}
	if got := c.capGap("DescribeTargetHealth", 100, 140); got.Reason != model.GapCapped || got.Seen != 100 || got.Total != 140 {
		t.Fatalf("capped with a known total = %+v", got)
	}
	if got := c.capGap("DescribeTargetHealth", 100, 40); got.Total != 0 {
		t.Fatalf("a total below what was read = %+v, want unknown", got)
	}
}

// Only the first page of AWS Health events is read; when another page
// exists the number of events is not known, so a cap names no total.
func TestHealthEventTotalIsUnknownWithMorePages(t *testing.T) {
	events := make([]healthtypes.Event, 25)
	if got := healthEventTotal(&health.DescribeEventsOutput{Events: events}); got != 25 {
		t.Fatalf("last page total = %d, want 25", got)
	}
	if got := healthEventTotal(&health.DescribeEventsOutput{Events: events, NextToken: aws.String("more")}); got != 0 {
		t.Fatalf("total with more pages = %d, want unknown", got)
	}
	c := &call{unit: model.Unit{Account: productionAccount, Region: model.GlobalRegion, Service: "aws-health"}}
	if gap := c.capGap("DescribeAffectedEntities", maxHealthEvents, healthEventTotal(&health.DescribeEventsOutput{Events: events, NextToken: aws.String("more")})); gap.Seen != maxHealthEvents || gap.Total != 0 {
		t.Fatalf("health cap = %+v, want more than %d", gap, maxHealthEvents)
	}
}
