package prometheus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/signalsources"
)

type fakeReader struct {
	names      []string
	metadata   map[string]signalsources.MetricMeta
	labels     map[string][]string
	series     []signalsources.MetricSeries
	err        error
	labelCalls []labelCall
	metaCalls  []string
	query      string
	start      time.Time
	end        time.Time
	step       time.Duration
	block      bool
}

type labelCall struct {
	label    string
	limit    int
	matchers []string
}

func (reader *fakeReader) MetadataFor(_ context.Context, metric string, _, _ time.Time) (map[string]signalsources.MetricMeta, error) {
	reader.metaCalls = append(reader.metaCalls, metric)
	if reader.err != nil {
		return nil, reader.err
	}
	return map[string]signalsources.MetricMeta{metric: reader.metadata[metric]}, nil
}
func (reader *fakeReader) LabelValuesLimited(ctx context.Context, label string, _, _ time.Time, limit int, matchers ...string) ([]string, error) {
	reader.labelCalls = append(reader.labelCalls, labelCall{label: label, limit: limit, matchers: append([]string(nil), matchers...)})
	if reader.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if reader.err != nil {
		return nil, reader.err
	}
	if label == "__name__" {
		return append([]string(nil), reader.names...), nil
	}
	return append([]string(nil), reader.labels[label]...), nil
}
func (reader *fakeReader) QueryRange(_ context.Context, query string, start, end time.Time, step time.Duration) ([]signalsources.MetricSeries, error) {
	reader.query, reader.start, reader.end, reader.step = query, start, end, step
	if reader.err != nil {
		return nil, reader.err
	}
	return reader.series, nil
}

type replacingScrubber struct{}

func (replacingScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, "secret", "[redacted]")
}

func TestDiscoverScopesServerReadsAndPaginatesDeterministically(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		names:    []string{"z_metric", "a_metric", "a_metric", "other"},
		metadata: map[string]signalsources.MetricMeta{"a_metric": {Type: "gauge", Help: "credential secret help"}, "z_metric": {Type: "counter"}},
		labels:   map[string][]string{"service": {"web", "api", "api"}},
	}
	service, err := NewService(reader, Config{ScopeFilter: `{cluster="prod"}`, Scrubber: replacingScrubber{}, Secrets: []string{"credential"}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Discover(context.Background(), DiscoveryRequest{Search: "metric", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || result.Metrics[0].Name != "a_metric" || result.Metrics[0].Help != "[redacted] [redacted] help" || !result.Truncated || result.NextOffset != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(reader.metaCalls) != 1 || reader.metaCalls[0] != "a_metric" {
		t.Fatalf("metadata calls = %v", reader.metaCalls)
	}
	if len(reader.labelCalls) < 2 || reader.labelCalls[0].label != "__name__" || len(reader.labelCalls[0].matchers) != 1 || reader.labelCalls[0].matchers[0] != `{__name__=~"(?i:.*metric.*)",cluster="prod"}` {
		t.Fatalf("label calls = %+v", reader.labelCalls)
	}
	if reader.labelCalls[0].limit != 2 {
		t.Fatalf("server discovery limit = %d", reader.labelCalls[0].limit)
	}
	if got := reader.labelCalls[1].matchers[0]; got != `a_metric{cluster="prod"}` {
		t.Fatalf("service matcher = %q", got)
	}
	if strings.Join(result.Metrics[0].Services, ",") != "api,web" {
		t.Fatalf("services = %v", result.Metrics[0].Services)
	}
}

func TestReadBuildsSelectorAndAppliesIndependentCaps(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	reader := &fakeReader{labels: map[string][]string{"service": {`api"} or up{`}}, series: []signalsources.MetricSeries{
		{Metric: map[string]string{"service": "secret-api"}, Samples: []signalsources.MetricSample{{Timestamp: now.Add(-time.Minute), Value: 1}, {Timestamp: now, Value: 2}}},
		{Metric: map[string]string{"service": "other"}, Samples: []signalsources.MetricSample{{Timestamp: now, Value: 3}}},
	}}
	policy := ToolPolicy()
	policy.MaxLookback = 30 * time.Minute
	policy.MinStep = 30 * time.Second
	policy.MaxSeries = 1
	policy.MaxDatapoints = 1
	service, err := NewService(reader, Config{ScopeFilter: `{cluster="prod"}`, Policy: policy, Scrubber: replacingScrubber{}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Read(context.Background(), ReadRequest{MetricName: "http_requests_total", Service: `api"} or up{`, Lookback: time.Hour, Step: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	wantQuery := `http_requests_total{cluster="prod",service="api\"} or up{"}`
	if reader.query != wantQuery {
		t.Fatalf("query = %q, want %q", reader.query, wantQuery)
	}
	if reader.start != now.Add(-30*time.Minute) || reader.end != now || reader.step != 30*time.Second {
		t.Fatalf("range = %s..%s step=%s", reader.start, reader.end, reader.step)
	}
	if result.Count != 1 || result.Datapoints != 1 || result.Series[0].Labels["service"] != "[redacted]-api" || !result.Truncated {
		t.Fatalf("result = %+v", result)
	}
	if strings.Join(result.Truncation, ",") != "datapoint_limit" {
		t.Fatalf("truncation = %v", result.Truncation)
	}
}

func TestReadIntersectsConfiguredServiceMatcher(t *testing.T) {
	for _, test := range []struct {
		name   string
		filter string
		want   string
	}{
		{name: "exact", filter: `{service="platform"}`, want: `up{service="platform",service="api"}`},
		{name: "regex", filter: `{service=~"api|worker"}`, want: `up{service=~"api|worker",service="api"}`},
		{name: "negative", filter: `{service!="admin"}`, want: `up{service!="admin",service="api"}`},
		{name: "negative regex", filter: `{service!~"internal-.*"}`, want: `up{service!~"internal-.*",service="api"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &fakeReader{labels: map[string][]string{"service": {"api"}}}
			service, err := NewService(reader, Config{ScopeFilter: test.filter})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Read(context.Background(), ReadRequest{MetricName: "up", Service: "api"}); err != nil {
				t.Fatal(err)
			}
			if reader.query != test.want {
				t.Fatalf("query = %q, want %q", reader.query, test.want)
			}
		})
	}
}

func TestReadQuotesRequestedServiceInIntersection(t *testing.T) {
	serviceName := `api"} or up{service="admin`
	reader := &fakeReader{labels: map[string][]string{"service": {serviceName}}}
	service, err := NewService(reader, Config{ScopeFilter: `{service=~".*"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Read(context.Background(), ReadRequest{MetricName: "up", Service: serviceName}); err != nil {
		t.Fatal(err)
	}
	want := `up{service=~".*",service="api\"} or up{service=\"admin"}`
	if reader.query != want {
		t.Fatalf("query = %q, want %q", reader.query, want)
	}
}

func TestServiceNormalizesMaximumStepToMinimum(t *testing.T) {
	reader := &fakeReader{labels: map[string][]string{"service": {"api"}}}
	policy := ToolPolicy()
	policy.MinStep = 2 * time.Minute
	policy.MaxStep = time.Minute
	service, err := NewService(reader, Config{Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Read(context.Background(), ReadRequest{MetricName: "up", Service: "api", Step: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if reader.step != 2*time.Minute || result.Step != "2m0s" {
		t.Fatalf("step = %s, result step = %q", reader.step, result.Step)
	}
}

func TestServiceRejectsUnsafeInputsAndConfiguration(t *testing.T) {
	reader := &fakeReader{}
	for _, filter := range []string{`cluster="prod"`, `{__name__=~".*"}`, `{cluster="prod",cluster="other"}`, `{cluster="prod"`} {
		if _, err := NewService(reader, Config{ScopeFilter: filter}); err == nil {
			t.Errorf("filter %q accepted", filter)
		}
	}
	service, _ := NewService(reader, Config{})
	for _, request := range []ReadRequest{{MetricName: `up or vector(1)`, Service: "api"}, {MetricName: "up", Service: " api"}, {MetricName: "up", Service: "api\nother"}} {
		if _, err := service.Read(context.Background(), request); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("request %+v error = %v", request, err)
		}
	}
}

func TestServiceReturnsSafeTimeoutAndBackendErrors(t *testing.T) {
	for _, test := range []struct {
		upstream, want error
		text           string
	}{
		{context.DeadlineExceeded, context.DeadlineExceeded, "context deadline exceeded"},
		{signalsources.ErrPrometheusResponseTooLarge, ErrResponseTooLarge, "prometheus: response too large"},
		{errors.New("https://user:secret@example.test body-canary"), nil, "prometheus read failed"},
	} {
		reader := &fakeReader{err: test.upstream}
		service, _ := NewService(reader, Config{})
		_, err := service.Discover(context.Background(), DiscoveryRequest{})
		if test.want != nil && !errors.Is(err, test.want) {
			t.Fatalf("error = %v", err)
		}
		if err == nil || err.Error() != test.text || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "body-canary") {
			t.Fatalf("unsafe error = %v", err)
		}
	}
}

func TestReadTruncatesAtByteLimit(t *testing.T) {
	reader := &fakeReader{labels: map[string][]string{"service": {"api"}}, series: []signalsources.MetricSeries{{Metric: map[string]string{"payload": strings.Repeat("x", 200)}, Samples: []signalsources.MetricSample{{Timestamp: time.Now(), Value: 1}}}}}
	policy := ToolPolicy()
	policy.MaxBytes = 64
	service, _ := NewService(reader, Config{Policy: policy})
	result, err := service.Read(context.Background(), ReadRequest{MetricName: "up", Service: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 0 || !result.Truncated || strings.Join(result.Truncation, ",") != "byte_limit" {
		t.Fatalf("result = %+v", result)
	}
}

func TestReadTruncatesAtSeriesLimitIndependently(t *testing.T) {
	reader := &fakeReader{labels: map[string][]string{"service": {"api"}}, series: []signalsources.MetricSeries{
		{Metric: map[string]string{"instance": "one"}, Samples: []signalsources.MetricSample{{Timestamp: time.Now(), Value: 1}}},
		{Metric: map[string]string{"instance": "two"}, Samples: []signalsources.MetricSample{{Timestamp: time.Now(), Value: 2}}},
	}}
	policy := ToolPolicy()
	policy.MaxSeries = 1
	service, _ := NewService(reader, Config{Policy: policy})
	result, err := service.Read(context.Background(), ReadRequest{MetricName: "up", Service: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || result.Datapoints != 1 || !result.Truncated || strings.Join(result.Truncation, ",") != "series_limit" {
		t.Fatalf("result = %+v", result)
	}
}

func TestDiscoverEnforcesExecutionTimeout(t *testing.T) {
	policy := ToolPolicy()
	policy.Timeout = time.Millisecond
	service, _ := NewService(&fakeReader{block: true}, Config{Policy: policy})
	_, err := service.Discover(context.Background(), DiscoveryRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscoverReportsServiceAndByteTruncationWithoutSkipping(t *testing.T) {
	services := make([]string, MaximumLimit+1)
	for index := range services {
		services[index] = fmt.Sprintf("s%03d", index)
	}
	reader := &fakeReader{names: []string{"a", "b"}, metadata: map[string]signalsources.MetricMeta{"a": {}, "b": {}}, labels: map[string][]string{"service": services}}
	policy := ToolPolicy()
	policy.MaxBytes = 1000
	service, _ := NewService(reader, Config{Policy: policy})
	result, err := service.Discover(context.Background(), DiscoveryRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || result.NextOffset != 1 || !result.Metrics[0].ServicesTruncated || !slices.Contains(result.Truncation, "service_limit") || !slices.Contains(result.Truncation, "byte_limit") {
		t.Fatalf("result = %+v", result)
	}
}
