package fakeaws

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func worldRequest(t *testing.T, server *Server, method, target, body string, headers map[string]string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, "https://fixture.amazonaws.com"+target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKID/20261109/us-east-1/"+headers["service"]+"/aws4_request, SignedHeaders=host, Signature=x")
	for key, value := range headers {
		if key != "service" {
			request.Header.Set(key, value)
		}
	}
	response, err := server.HTTPClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func TestPageOfWalksEveryItemOnce(t *testing.T) {
	items := make([]string, 250)
	for index := range items {
		items[index] = string(rune('a' + index%26))
	}
	seen, token, pages := 0, "", 0
	for {
		page, next := pageOf(items, token, 0)
		seen += len(page)
		pages++
		if next == "" {
			break
		}
		token = next
	}
	if seen != 250 || pages != 3 {
		t.Fatalf("seen %d items in %d pages", seen, pages)
	}
	if page, next := pageOf(items, "bogus", 10); len(page) != 10 || next != "10" {
		t.Fatalf("bad token page = %d, %q", len(page), next)
	}
}

func TestLoadSizeSplitsAcrossServices(t *testing.T) {
	server := New(t, ScenarioLoad5k)
	if server.LoadSize() != DefaultLoadSize {
		t.Fatalf("load = %d", server.LoadSize())
	}
	t.Setenv(EnvLoadSize, "50000")
	if server.LoadSize() != 50000 {
		t.Fatal("environment did not raise the load")
	}
	server.SetLoadSize(10)
	w := world{load: server.LoadSize()}
	total := len(w.instanceIDs()) - WorldTargets + len(w.functionNames()) - 1 + len(w.queueNames()) - 2 + len(w.tableNames()) - 1 + len(w.topicNames()) - 1
	if total != 10 {
		t.Fatalf("generated %d extra resources, want 10", total)
	}
}

func TestBadDeployTrailIsFilteredByWindow(t *testing.T) {
	server := New(t, ScenarioBadDeploy)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	server.SetClock(func() time.Time { return now })
	start := BadDeployStart(now)
	body := `{"StartTime":` + jsonNumber(start.Add(-time.Second)) + `,"EndTime":` + jsonNumber(start.Add(time.Minute)) + `}`
	status, answer := worldRequest(t, server, http.MethodPost, "/", body, map[string]string{"service": "cloudtrail", "X-Amz-Target": "CloudTrail_20131101.LookupEvents"})
	if status != http.StatusOK {
		t.Fatalf("lookup = %d %s", status, answer)
	}
	var decoded struct {
		Events []struct {
			EventName string
			EventTime int64
		}
	}
	if err := json.Unmarshal([]byte(answer), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Events) != 2 || decoded.Events[0].EventName != "UpdateService" || decoded.Events[1].EventName != "RegisterTaskDefinition" {
		t.Fatalf("events = %+v", decoded.Events)
	}
}

func jsonNumber(value time.Time) string {
	encoded, _ := json.Marshal(value.Unix())
	return string(encoded)
}

func TestAccessDeniedBurstRefusesUnitsAndDetailCalls(t *testing.T) {
	server := New(t, ScenarioAccessDeniedBurst)
	status, _ := worldRequest(t, server, http.MethodPost, "/", "{}", map[string]string{"service": "sqs", "X-Amz-Target": "AmazonSQS.ListQueues"})
	if status != http.StatusForbidden {
		t.Fatalf("denied list = %d", status)
	}
	status, body := worldRequest(t, server, http.MethodPost, "/", "Action=DescribeTargetHealth&Version=2015-12-01", map[string]string{"service": "elasticloadbalancing", "Content-Type": "application/x-www-form-urlencoded"})
	if status != http.StatusForbidden || !strings.Contains(body, "AccessDenied") {
		t.Fatalf("denied detail = %d %s", status, body)
	}
	status, _ = worldRequest(t, server, http.MethodPost, "/", "Action=DescribeLoadBalancers&Version=2015-12-01", map[string]string{"service": "elasticloadbalancing", "Content-Type": "application/x-www-form-urlencoded"})
	if status != http.StatusOK {
		t.Fatalf("allowed list = %d", status)
	}
}

func TestProbeScenariosKeepTheirProbePages(t *testing.T) {
	server := New(t, "new-account-connect")
	status, body := worldRequest(t, server, http.MethodPost, "/", "Action=DescribeLoadBalancers&Version=2015-12-01", map[string]string{"service": "elasticloadbalancing", "Content-Type": "application/x-www-form-urlencoded"})
	if status != http.StatusOK || !strings.Contains(body, "111122223333") || strings.Contains(body, "AvailabilityZones") {
		t.Fatalf("probe page changed: %s", body)
	}
	// A collector-only operation is answered from the world in every scenario.
	status, body = worldRequest(t, server, http.MethodPost, "/", "Action=DescribeTargetGroups&Version=2015-12-01", map[string]string{"service": "elasticloadbalancing", "Content-Type": "application/x-www-form-urlencoded"})
	if status != http.StatusOK || !strings.Contains(body, WorldTargetGroupID) {
		t.Fatalf("world page = %d %s", status, body)
	}
}

func TestCollectorOperationsAreListed(t *testing.T) {
	operations := CollectorOperations()
	if len(operations) < 50 {
		t.Fatalf("collector operations = %d", len(operations))
	}
	for _, operation := range operations {
		service, action, ok := strings.Cut(operation, ":")
		if !ok || service == "" || action == "" {
			t.Fatalf("bad operation %q", operation)
		}
	}
}

func TestCBORTimestampsUseTagOne(t *testing.T) {
	var buffer strings.Builder
	if err := encodeCBOR(&buffer, cborTime{time.Unix(1700000000, 0)}); err != nil {
		t.Fatal(err)
	}
	encoded := []byte(buffer.String())
	if encoded[0] != 0xc1 || encoded[1] != 0x1a {
		t.Fatalf("timestamp encoding = % x", encoded)
	}
}
