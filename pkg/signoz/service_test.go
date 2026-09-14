package signoz

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type replacingScrubber struct{}

const testAPIKey = "test-api-key"

func newTLSTestServer(t *testing.T, handler http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())
	t.Cleanup(server.Close)
	return server, rootCAs
}

func testConfig(server *httptest.Server, rootCAs *x509.CertPool, apiKey string) Config {
	return Config{Address: server.URL, APIKey: apiKey, AllowLoopback: true, RootCAs: rootCAs}
}

func (replacingScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, "secret", "[REDACTED]")
}

func TestServiceSearchLogsUsesBoundedV5Payload(t *testing.T) {
	var payload map[string]any
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != QueryRangePath || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("SIGNOZ-API-KEY") != testAPIKey {
			t.Fatal("missing SigNoz auth header")
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(writer, `{"status":"success","data":{"data":{"results":[{"queryName":"A","rows":[{"data":{"body":"secret one"}},{"data":{"body":"two"}},{"data":{"body":"three"}}]}]}}}`)
	}))

	config := testConfig(server, rootCAs, testAPIKey)
	config.ScopeFilter = "deployment.environment = 'prod'"
	service, err := NewService(config, Policy{MaximumRows: 2})
	if err != nil {
		t.Fatal(err)
	}
	service.SetScrubber(replacingScrubber{})
	end := time.Now().UTC().Add(-time.Minute)
	result, err := service.SearchLogs(context.Background(), SearchRequest{Start: end.Add(-time.Hour), End: end, Service: "checkout", SearchText: "panic", Limit: 99})
	if err != nil {
		t.Fatal(err)
	}
	if result.Limit != 2 || !result.Truncated || !strings.Contains(fmt.Sprint(result.Data), "[REDACTED]") {
		t.Fatalf("result = %+v", result)
	}
	if payload["schemaVersion"] != "v1" || payload["requestType"] != "raw" {
		t.Fatalf("payload = %#v", payload)
	}
	spec := payload["compositeQuery"].(map[string]any)["queries"].([]any)[0].(map[string]any)["spec"].(map[string]any)
	filter := spec["filter"].(map[string]any)["expression"].(string)
	for _, required := range []string{"deployment.environment", "service.name", "body CONTAINS"} {
		if !strings.Contains(filter, required) {
			t.Fatalf("filter %q missing %q", filter, required)
		}
	}
}

func TestServiceRejectsUnsafeBoundsAndRedirects(t *testing.T) {
	targetCalled := false
	target, _ := newTLSTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalled = true }))
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	service, err := NewService(testConfig(server, rootCAs, testAPIKey), ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC()
	if _, err := service.SearchTraces(context.Background(), SearchRequest{Start: end.Add(-7 * time.Hour), End: end}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("window error = %v", err)
	}
	if _, err := service.SearchTraces(context.Background(), SearchRequest{Start: end.Add(-time.Hour), End: end}); err == nil || targetCalled || !strings.Contains(err.Error(), "configure the final address") {
		t.Fatalf("redirect error = %v targetCalled=%t", err, targetCalled)
	}
}

func TestServiceResponseLimitAndSafeErrors(t *testing.T) {
	const apiKeyCanary = "SIGNOZ_API_KEY_CANARY_7f3d"
	const bodyCanary = "SIGNOZ_UPSTREAM_BODY_CANARY_b91e"
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("signal") == "logs" {
			writer.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(writer, "%s contains %s", bodyCanary, apiKeyCanary)
			return
		}
		fmt.Fprint(writer, strings.Repeat("x", 257))
	}))
	service, err := NewService(testConfig(server, rootCAs, apiKeyCanary), Policy{MaximumBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := service.FieldKeys(context.Background(), SignalLogs, FieldRequest{})
	var statusErr *StatusError
	if readErr == nil || !errors.As(readErr, &statusErr) || statusErr.StatusCode != http.StatusBadRequest || strings.Contains(readErr.Error(), apiKeyCanary) || strings.Contains(readErr.Error(), bodyCanary) {
		t.Fatalf("safe error = %v", readErr)
	}
	wrapped := fmt.Errorf("ingestion wrapper: %w", readErr)
	if strings.Contains(wrapped.Error(), apiKeyCanary) || strings.Contains(wrapped.Error(), bodyCanary) || !strings.Contains(wrapped.Error(), "status 400") {
		t.Fatalf("safe wrapped error = %v", wrapped)
	}
	if _, err := service.ListMetrics(context.Background(), "", 1); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestServiceMetricAndFieldDiscoveryPaths(t *testing.T) {
	seen := map[string]bool{}
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen[request.URL.Path] = true
		switch request.URL.Path {
		case FieldKeysPath:
			fmt.Fprint(writer, `{"status":"success","data":{"keys":{"service.name":["string"],"deployment.environment":["string"]},"complete":true}}`)
		case FieldValuesPath:
			fmt.Fprint(writer, `{"status":"success","data":{"values":{"prod":12,"staging":4},"complete":true}}`)
		default:
			fmt.Fprint(writer, `{"status":"success","data":{"metrics":[{"metricName":"http.server.duration","type":"histogram"}]}}`)
		}
	}))
	service, err := NewService(testConfig(server, rootCAs, testAPIKey), ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(-time.Minute)
	if _, err := service.QueryMetrics(context.Background(), MetricRequest{Start: end.Add(-time.Hour), End: end, MetricName: "http.server.duration", Step: time.Minute}); err != nil {
		t.Fatal(err)
	}
	keys, err := service.FieldKeys(context.Background(), SignalTraces, FieldRequest{Context: "resource"})
	if err != nil || keys.Count != 2 {
		t.Fatalf("field keys = %+v, %v", keys, err)
	}
	values, err := service.FieldValues(context.Background(), SignalLogs, FieldRequest{Name: "severity_text"})
	if err != nil || values.Count != 2 {
		t.Fatalf("field values = %+v, %v", values, err)
	}
	if _, err := service.ListMetrics(context.Background(), "http", 10); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{QueryRangePath, FieldKeysPath, FieldValuesPath, MetricsPath} {
		if !seen[path] {
			t.Errorf("path %s not called", path)
		}
	}
}

func TestServiceBoundsDiscoveryMapsDeterministically(t *testing.T) {
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("limit") != "2" {
			t.Fatalf("limit = %q", request.URL.Query().Get("limit"))
		}
		if request.URL.Path == FieldValuesPath {
			fmt.Fprint(writer, `{"data":{"values":{"zeta":1,"alpha":3,"middle":2},"complete":true}}`)
			return
		}
		fmt.Fprint(writer, `{"data":{"keys":{"zeta":["string"],"alpha":["string"],"middle":["string"]},"complete":true}}`)
	}))
	service, err := NewService(testConfig(server, rootCAs, testAPIKey), Policy{MaximumFieldKeys: 2})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := service.FieldKeys(context.Background(), SignalLogs, FieldRequest{})
	if err != nil {
		t.Fatal(err)
	}
	values, err := service.FieldValues(context.Background(), SignalLogs, FieldRequest{Name: "service.name"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		result Result
		key    string
		reason string
	}{{keys, "keys", "discovery_field_keys"}, {values, "values", "discovery_field_values"}} {
		result := test.result
		data := result.Data["data"].(map[string]any)
		items := data[test.key].(map[string]any)
		if result.Count != 3 || !result.Truncated || fmt.Sprint(result.Truncation) != "["+test.reason+"]" || data["complete"] != false || len(items) != 2 || items["alpha"] == nil || items["middle"] == nil {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestServiceMetricDatapointsUseSeparateNewestPointBudget(t *testing.T) {
	values := make([]any, 75)
	for index := range values {
		values[index] = []any{index, index * 10}
	}
	fixture := map[string]any{"data": map[string]any{"data": map[string]any{"results": []any{map[string]any{"series": []any{map[string]any{"labels": map[string]any{"service": "api"}, "values": values}}}}}}}
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(writer).Encode(fixture) }))
	service, err := NewService(testConfig(server, rootCAs, testAPIKey), Policy{MaximumSeries: 1, MaximumDatapointsPerSeries: 60, MaximumDatapoints: 60})
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(-time.Minute)
	result, err := service.QueryMetrics(context.Background(), MetricRequest{Start: end.Add(-time.Hour), End: end, Service: "api", MetricName: "requests", Step: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	series := result.Data["data"].(map[string]any)["data"].(map[string]any)["results"].([]any)[0].(map[string]any)["series"].([]any)
	points := series[0].(map[string]any)["values"].([]any)
	if result.Count != 1 || !result.Truncated || fmt.Sprint(result.Truncation) != "[datapoints]" || len(points) != 60 || int(points[0].([]any)[0].(float64)) != 15 || int(points[59].([]any)[0].(float64)) != 74 {
		t.Fatalf("result = %+v", result)
	}
}

func TestServiceMetricTotalDatapointsAcrossSeries(t *testing.T) {
	series := make([]any, 2)
	for seriesIndex := range series {
		values := make([]any, 3)
		for pointIndex := range values {
			values[pointIndex] = []any{seriesIndex*10 + pointIndex, pointIndex}
		}
		series[seriesIndex] = map[string]any{"labels": map[string]any{"instance": seriesIndex}, "values": values}
	}
	fixture := map[string]any{"data": map[string]any{"data": map[string]any{"results": []any{map[string]any{"series": series}}}}}
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(writer).Encode(fixture) }))
	service, err := NewService(testConfig(server, rootCAs, testAPIKey), Policy{MaximumSeries: 2, MaximumDatapointsPerSeries: 3, MaximumDatapoints: 4})
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(-time.Minute)
	result, err := service.QueryMetrics(context.Background(), MetricRequest{Start: end.Add(-time.Hour), End: end, MetricName: "requests.total", Step: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	gotSeries := result.Data["data"].(map[string]any)["data"].(map[string]any)["results"].([]any)[0].(map[string]any)["series"].([]any)
	first := gotSeries[0].(map[string]any)["values"].([]any)
	second := gotSeries[1].(map[string]any)["values"].([]any)
	if fmt.Sprint(result.Truncation) != "[datapoints]" || len(first) != 3 || len(second) != 1 || int(second[0].([]any)[0].(float64)) != 12 {
		t.Fatalf("total datapoint bound = %+v", result)
	}
}

func TestServiceUsesDistinctCollectionTruncationReasons(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		invoke  func(*Service) (Result, error)
		want    string
	}{
		{name: "rows", fixture: `{"data":{"data":{"results":[{"rows":[{},{}]}]}}}`, invoke: func(service *Service) (Result, error) {
			end := time.Now().UTC().Add(-time.Minute)
			return service.SearchLogs(context.Background(), SearchRequest{Start: end.Add(-time.Hour), End: end})
		}, want: "[rows]"},
		{name: "metrics", fixture: `{"data":{"metrics":[{"metricName":"one"},{"metricName":"two"}]}}`, invoke: func(service *Service) (Result, error) {
			return service.ListMetrics(context.Background(), "", 2)
		}, want: "[metrics]"},
		{name: "series", fixture: `{"data":{"data":{"results":[{"series":[{"values":[]},{"values":[]}]}]}}}`, invoke: func(service *Service) (Result, error) {
			end := time.Now().UTC().Add(-time.Minute)
			return service.QueryMetrics(context.Background(), MetricRequest{Start: end.Add(-time.Hour), End: end, MetricName: "requests.total"})
		}, want: "[series]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { fmt.Fprint(writer, test.fixture) }))
			service, err := NewService(testConfig(server, rootCAs, testAPIKey), Policy{MaximumRows: 1, MaximumSeries: 1})
			if err != nil {
				t.Fatal(err)
			}
			result, err := test.invoke(service)
			if err != nil || !result.Truncated || fmt.Sprint(result.Truncation) != test.want {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
		})
	}
}

func TestServiceDoesNotTruncateAtExactLimits(t *testing.T) {
	service := &Service{policy: Policy{MaximumDatapointsPerSeries: 2, MaximumDatapoints: 2}}
	tests := []struct {
		name  string
		shape responseShape
		value any
	}{
		{name: "rows", shape: responseRows, value: map[string]any{"rows": []any{map[string]any{"id": "one"}}}},
		{name: "metrics", shape: responseMetricList, value: map[string]any{"metrics": []any{map[string]any{"metricName": "one"}}}},
		{name: "series and datapoints", shape: responseSeries, value: map[string]any{"series": []any{map[string]any{"values": []any{[]any{1, 10}, []any{2, 20}}}}}},
		{name: "discovery field keys", shape: responseDiscovery, value: map[string]any{"keys": map[string]any{"service.name": []any{"string"}}}},
		{name: "discovery field values", shape: responseDiscovery, value: map[string]any{"values": map[string]any{"api": 1}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, reasons := service.boundValue(test.value, test.shape, 1)
			if len(reasons) != 0 {
				t.Fatalf("exact limit reported truncation: %v", reasons)
			}
		})
	}
}

func TestServiceRejectsUnsafeRequestLiteralsBeforeTransport(t *testing.T) {
	requests := 0
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		fmt.Fprint(writer, `{"data":{"data":{"results":[]}}}`)
	}))
	config := testConfig(server, rootCAs, testAPIKey)
	config.ScopeFilter = "deployment.environment = 'prod'"
	service, err := NewService(config, ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(-time.Minute)
	unsafeMetricNames := []string{"requests'bad", `requests"bad`, `requests\bad`, "requests\rbad", "requests\nbad", "requests\x00bad", "requests\x7fbad"}
	for _, metricName := range unsafeMetricNames {
		if _, err := service.QueryMetrics(context.Background(), MetricRequest{Start: end.Add(-time.Hour), End: end, MetricName: metricName}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("metric name %q error = %v", metricName, err)
		}
	}
	cases := []struct {
		name   string
		invoke func() error
	}{
		{"metric service", func() error {
			_, err := service.QueryMetrics(context.Background(), MetricRequest{Start: end.Add(-time.Hour), End: end, MetricName: "requests.total", Service: "api\\prod"})
			return err
		}},
		{"metric temporality", func() error {
			_, err := service.QueryMetrics(context.Background(), MetricRequest{Start: end.Add(-time.Hour), End: end, MetricName: "requests.total", Temporality: "delta\n"})
			return err
		}},
		{"metric time aggregation", func() error {
			_, err := service.QueryMetrics(context.Background(), MetricRequest{Start: end.Add(-time.Hour), End: end, MetricName: "requests.total", TimeAggregation: "avg'"})
			return err
		}},
		{"metric space aggregation", func() error {
			_, err := service.QueryMetrics(context.Background(), MetricRequest{Start: end.Add(-time.Hour), End: end, MetricName: "requests.total", SpaceAggregation: `avg"`})
			return err
		}},
		{"log search", func() error {
			_, err := service.SearchLogs(context.Background(), SearchRequest{Start: end.Add(-time.Hour), End: end, SearchText: "panic\nnext"})
			return err
		}},
		{"trace operation", func() error {
			_, err := service.SearchTraces(context.Background(), SearchRequest{Start: end.Add(-time.Hour), End: end, Operation: "GET\\cart"})
			return err
		}},
		{"field key search", func() error {
			_, err := service.FieldKeys(context.Background(), SignalLogs, FieldRequest{SearchText: "service'"})
			return err
		}},
		{"field context", func() error {
			_, err := service.FieldKeys(context.Background(), SignalLogs, FieldRequest{Context: "resource\n"})
			return err
		}},
		{"field data type", func() error {
			_, err := service.FieldKeys(context.Background(), SignalLogs, FieldRequest{DataType: `string"`})
			return err
		}},
		{"field metric name", func() error {
			_, err := service.FieldKeys(context.Background(), SignalMetrics, FieldRequest{MetricName: "requests\\total"})
			return err
		}},
		{"field value name", func() error {
			_, err := service.FieldValues(context.Background(), SignalLogs, FieldRequest{Name: "service.name'"})
			return err
		}},
		{"field value search", func() error {
			_, err := service.FieldValues(context.Background(), SignalLogs, FieldRequest{Name: "service.name", SearchText: "api\x7f"})
			return err
		}},
		{"metric discovery", func() error { _, err := service.ListMetrics(context.Background(), "requests\x00bad", 10); return err }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := test.invoke(); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("unsafe literals made %d transport requests", requests)
	}
}

func TestServiceTraceSelectFieldsAndUnsafeFilters(t *testing.T) {
	requests := 0
	var payload map[string]any
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(writer, `{"data":{"data":{"results":[{"rows":[{"data":{"timestamp":1720000000000,"trace_id":"trace-1","span_id":"span-1","name":"GET /cart","duration_nano":2000,"has_error":true,"service.name":"checkout"}}]}]}}}`)
	}))
	config := testConfig(server, rootCAs, testAPIKey)
	config.ScopeFilter = "deployment.environment = 'prod'"
	service, err := NewService(config, ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(-time.Minute)
	request := SearchRequest{Start: end.Add(-time.Hour), End: end, Service: "checkout", Operation: "GET /cart"}
	result, err := service.SearchTraces(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	spec := payload["compositeQuery"].(map[string]any)["queries"].([]any)[0].(map[string]any)["spec"].(map[string]any)
	fields := spec["selectFields"].([]any)
	for _, required := range []string{"service.name", "trace_id", "span_id", "duration_nano", "has_error", "timestamp"} {
		if !strings.Contains(fmt.Sprint(fields), required) {
			t.Fatalf("selectFields %#v missing %q", fields, required)
		}
	}
	if result.Count != 1 || !strings.Contains(fmt.Sprint(result.Data), "trace-1") || !strings.Contains(fmt.Sprint(result.Data), "checkout") {
		t.Fatalf("trace response = %+v", result)
	}
	for _, unsafe := range []string{"checkout' OR true", `checkout\\prod`, "checkout\nprod", `checkout"prod`} {
		request.Service = unsafe
		if _, err := service.SearchTraces(context.Background(), request); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("unsafe service %q error = %v", unsafe, err)
		}
	}
	if requests != 1 {
		t.Fatalf("requests = %d, unsafe values reached backend", requests)
	}
}

func TestServiceBoundsReferenceV5RowsFixture(t *testing.T) {
	logValues := make([]any, 75)
	for index := range logValues {
		logValues[index] = index
	}
	fixture := map[string]any{
		"status": "success",
		"data": map[string]any{"data": map[string]any{"results": []any{
			map[string]any{"queryName": "A", "nextCursor": "page-2", "rows": []any{
				map[string]any{"timestamp": "2026-09-11T10:00:00Z", "data": map[string]any{"body": "first", "values": logValues}},
				map[string]any{"timestamp": "2026-09-11T10:00:01Z", "data": map[string]any{"body": "second"}},
			}},
		}}},
	}
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(writer).Encode(fixture) }))
	service, err := NewService(testConfig(server, rootCAs, testAPIKey), Policy{MaximumRows: 1, MaximumDatapointsPerSeries: 1, MaximumDatapoints: 1})
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(-time.Minute)
	result, err := service.SearchLogs(context.Background(), SearchRequest{Start: end.Add(-time.Hour), End: end, Service: "api", Offset: 5, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 2 || !result.Truncated || result.Offset != 5 || result.Limit != 1 {
		t.Fatalf("result = %+v", result)
	}
	rows := result.Data["data"].(map[string]any)["data"].(map[string]any)["results"].([]any)[0].(map[string]any)["rows"].([]any)
	values := rows[0].(map[string]any)["data"].(map[string]any)["values"].([]any)
	if len(values) != len(logValues) || fmt.Sprint(result.Truncation) != "[rows]" {
		t.Fatalf("raw values were treated as datapoints: result=%+v", result)
	}
}

func TestNewServiceValidatesConnection(t *testing.T) {
	for _, config := range []Config{{Address: "", APIKey: testAPIKey}, {Address: "ftp://host", APIKey: testAPIKey}, {Address: "https://user@host", APIKey: testAPIKey}, {Address: "https://host?token=x", APIKey: testAPIKey}, {Address: "https://host"}, {Address: "https://host", APIKey: "short"}} {
		if _, err := NewService(config, ToolPolicy()); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("config %+v error = %v", config, err)
		}
	}
}

func TestServiceScrubsExactAPIKeyFromNestedSuccessfulOutput(t *testing.T) {
	const apiKey = "opaque-signoz-key-7f3d"
	fixture := map[string]any{"data": map[string]any{"data": map[string]any{"results": []any{map[string]any{"rows": []any{map[string]any{
		apiKey: "map-key", "nested": []any{apiKey, map[string]any{"value": "prefix " + apiKey + " suffix"}},
	}}}}}}}
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(writer).Encode(fixture) }))
	service, err := NewService(testConfig(server, rootCAs, apiKey), ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(-time.Minute)
	result, err := service.SearchLogs(context.Background(), SearchRequest{Start: end.Add(-time.Hour), End: end})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), apiKey) {
		t.Fatalf("successful result contains exact API key: %s", encoded)
	}
}

func TestServiceExactKeyScrubReturnsFreshStructures(t *testing.T) {
	const apiKey = "opaque-signoz-key-7f3d"
	original := map[string]any{apiKey: []any{apiKey, map[string]any{"value": apiKey}}}
	service := &Service{apiKey: apiKey, policy: ToolPolicy()}
	bounded, _, _ := service.boundValue(original, responseRows, MaximumLimit)
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), apiKey) {
		t.Fatalf("bounded output contains exact API key: %s", encoded)
	}
	if original[apiKey].([]any)[0] != apiKey || original[apiKey].([]any)[1].(map[string]any)["value"] != apiKey {
		t.Fatalf("input was mutated: %#v", original)
	}
}

func TestNewClientCredentialTransportPolicy(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{name: "plain HTTP", config: Config{Address: "http://signoz.example", APIKey: testAPIKey}},
		{name: "insecure TLS", config: Config{Address: "https://signoz.example", APIKey: testAPIKey, InsecureSkipVerify: true}},
		{name: "URL userinfo", config: Config{Address: "https://user@signoz.example", APIKey: testAPIKey}},
		{name: "loopback by default", config: Config{Address: "https://127.0.0.1", APIKey: testAPIKey}},
		{name: "private by default", config: Config{Address: "https://10.0.0.1", APIKey: testAPIKey}},
		{name: "metadata", config: Config{Address: "https://169.254.169.254", APIKey: testAPIKey, AllowPrivate: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewClient(test.config, ToolPolicy()); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { fmt.Fprint(writer, `{}`) }))
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client, err := NewClient(testConfig(server, rootCAs, testAPIKey), ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	transport := client.httpClient.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("unsafe transport: proxy_set=%t tls=%+v", transport.Proxy != nil, transport.TLSClientConfig)
	}
	if _, err := client.Do(context.Background(), http.MethodGet, MetricsPath, nil, nil); err != nil {
		t.Fatalf("ambient proxy environment intercepted request: %v", err)
	}
}

func TestResolvedAddressPolicy(t *testing.T) {
	tests := []struct {
		name                        string
		address                     string
		allowLoopback, allowPrivate bool
		want                        bool
	}{
		{name: "public", address: "203.0.113.10", want: true},
		{name: "loopback denied", address: "127.0.0.1"},
		{name: "loopback allowed", address: "127.0.0.1", allowLoopback: true, want: true},
		{name: "private denied", address: "10.0.0.1"},
		{name: "private allowed", address: "10.0.0.1", allowPrivate: true, want: true},
		{name: "metadata link local", address: "169.254.169.254", allowPrivate: true},
		{name: "unspecified", address: "0.0.0.0", allowLoopback: true, allowPrivate: true},
		{name: "multicast", address: "224.0.0.1", allowLoopback: true, allowPrivate: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validResolvedAddress(net.ParseIP(test.address), test.allowLoopback, test.allowPrivate); got != test.want {
				t.Fatalf("validResolvedAddress(%s) = %t, want %t", test.address, got, test.want)
			}
		})
	}
}

func TestGuardedDialRejectsUnsafeDestinationBeforeConnect(t *testing.T) {
	dial := guardedDialContext(&net.Dialer{}, true, true)
	for _, address := range []string{"169.254.169.254:443", "0.0.0.0:443", "224.0.0.1:443"} {
		if _, err := dial(context.Background(), "tcp", address); err == nil || !strings.Contains(err.Error(), "destination is not permitted") {
			t.Fatalf("dial %s error = %v", address, err)
		}
	}
}
