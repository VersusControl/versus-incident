package signalsources

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// -----------------------------------------------------------------------------
// PrometheusQuerier — a thin read-only client over Prometheus' HTTP query API.
//
// It is shared OSS infrastructure: the analyze `query_metrics` tool consumes it
// (via a bridge in pkg/agent), and the enterprise metric data source reuses the
// exact same client so detect-path and analyze-path both speak to Prometheus
// through one code path. It only ever issues GET query requests — there is no
// write surface.
// -----------------------------------------------------------------------------

// MetricSample is one (timestamp, value) point of a metric series.
type MetricSample struct {
	Timestamp time.Time
	Value     float64
}

// MetricSeries is one labelled series returned by a PromQL query.
type MetricSeries struct {
	Metric  map[string]string
	Samples []MetricSample
}

// PrometheusAuth carries the optional credentials for a Prometheus
// endpoint. BearerToken takes priority over Username/Password.
type PrometheusAuth struct {
	BearerToken string
	Username    string
	Password    string
}

// PrometheusQuerier issues instant and range PromQL queries against a
// Prometheus HTTP endpoint. Construct it once and reuse it.
type PrometheusQuerier struct {
	address string
	auth    PrometheusAuth
	client  *http.Client
}

// NewPrometheusQuerier validates the address and returns a ready querier.
func NewPrometheusQuerier(address string, auth PrometheusAuth, insecureSkipVerify bool) (*PrometheusQuerier, error) {
	if address == "" {
		return nil, fmt.Errorf("prometheus: address is required")
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify},
	}
	return &PrometheusQuerier{
		address: address,
		auth:    auth,
		client:  &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

// QueryRange runs a PromQL `query_range` over [start, end] at the given
// step resolution and returns the matrix result as a slice of series.
func (q *PrometheusQuerier) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]MetricSeries, error) {
	if step <= 0 {
		step = time.Minute
	}
	v := url.Values{}
	v.Set("query", query)
	v.Set("start", formatPromTime(start))
	v.Set("end", formatPromTime(end))
	v.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))

	u := q.address + "/api/v1/query_range?" + v.Encode()
	return q.do(ctx, u)
}

// QueryInstant runs a PromQL instant query at time t and returns the
// vector result as a slice of single-sample series.
func (q *PrometheusQuerier) QueryInstant(ctx context.Context, query string, t time.Time) ([]MetricSeries, error) {
	v := url.Values{}
	v.Set("query", query)
	v.Set("time", formatPromTime(t))

	u := q.address + "/api/v1/query?" + v.Encode()
	return q.do(ctx, u)
}

func (q *PrometheusQuerier) do(ctx context.Context, u string) ([]MetricSeries, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	q.applyAuth(req)

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("prometheus %s: %d %s", u, resp.StatusCode, truncate(string(body), 256))
	}

	var out promQueryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: %s %s", out.ErrorType, out.Error)
	}
	return out.Data.toSeries()
}

func (q *PrometheusQuerier) applyAuth(req *http.Request) {
	if q.auth.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+q.auth.BearerToken)
		return
	}
	if q.auth.Username != "" {
		token := base64.StdEncoding.EncodeToString([]byte(q.auth.Username + ":" + q.auth.Password))
		req.Header.Set("Authorization", "Basic "+token)
	}
}

// formatPromTime renders a time as a Unix timestamp with millisecond
// precision, which Prometheus accepts for start/end/time params.
func formatPromTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UTC().UnixNano())/1e9, 'f', 3, 64)
}

// -----------------------------------------------------------------------------
// Discovery reads (GET-only).
//
// These let a caller LEARN the shape of a Prometheus instance — what metrics
// exist, what values a label takes, which series match — without being handed a
// query up front. They are the read half of auto-configuration: an enterprise
// metric brain uses them to discover the golden-signal series per service,
// while OSS simply exposes them as helpers. Every one is a GET against the
// standard Prometheus metadata API; none accept or issue a write.
// -----------------------------------------------------------------------------

// MetricMeta is the type/help/unit metadata Prometheus records for a metric
// name (from /api/v1/metadata).
type MetricMeta struct {
	Type string // "counter" | "gauge" | "histogram" | "summary" | "untyped"
	Help string
	Unit string
}

// Metadata returns the metric → metadata map advertised by the target
// (GET /api/v1/metadata). A metric may carry several metadata entries; Prometheus
// guarantees they are consistent, so we keep the first.
func (q *PrometheusQuerier) Metadata(ctx context.Context) (map[string]MetricMeta, error) {
	body, err := q.get(ctx, q.address+"/api/v1/metadata")
	if err != nil {
		return nil, err
	}
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   map[string][]struct {
			Type string `json:"type"`
			Help string `json:"help"`
			Unit string `json:"unit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode prometheus metadata: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus metadata failed: %s", out.Error)
	}
	meta := make(map[string]MetricMeta, len(out.Data))
	for name, entries := range out.Data {
		if len(entries) == 0 {
			continue
		}
		meta[name] = MetricMeta{Type: entries[0].Type, Help: entries[0].Help, Unit: entries[0].Unit}
	}
	return meta, nil
}

// LabelValues returns the observed values of a label
// (GET /api/v1/label/<name>/values), optionally constrained by series selectors
// (e.g. `up`, `http_requests_total{job="api"}`).
func (q *PrometheusQuerier) LabelValues(ctx context.Context, label string, matchers ...string) ([]string, error) {
	if label == "" {
		return nil, fmt.Errorf("prometheus: label name is required")
	}
	v := url.Values{}
	for _, m := range matchers {
		if m != "" {
			v.Add("match[]", m)
		}
	}
	u := q.address + "/api/v1/label/" + url.PathEscape(label) + "/values"
	if enc := v.Encode(); enc != "" {
		u += "?" + enc
	}
	body, err := q.get(ctx, u)
	if err != nil {
		return nil, err
	}
	var out struct {
		Status string   `json:"status"`
		Error  string   `json:"error"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode prometheus label values: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus label values failed: %s", out.Error)
	}
	return out.Data, nil
}

// Series returns the label sets of series matching the given selectors
// (GET /api/v1/series). At least one selector is required by Prometheus.
func (q *PrometheusQuerier) Series(ctx context.Context, matchers ...string) ([]map[string]string, error) {
	sel := make([]string, 0, len(matchers))
	for _, m := range matchers {
		if m != "" {
			sel = append(sel, m)
		}
	}
	if len(sel) == 0 {
		return nil, fmt.Errorf("prometheus: at least one series selector is required")
	}
	v := url.Values{}
	for _, m := range sel {
		v.Add("match[]", m)
	}
	body, err := q.get(ctx, q.address+"/api/v1/series?"+v.Encode())
	if err != nil {
		return nil, err
	}
	var out struct {
		Status string              `json:"status"`
		Error  string              `json:"error"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode prometheus series: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus series failed: %s", out.Error)
	}
	return out.Data, nil
}

// get issues a GET with auth and returns the raw body, failing on non-2xx. It
// is the read primitive shared by the discovery helpers; the query methods keep
// their own decode-specialised path in do().
func (q *PrometheusQuerier) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	q.applyAuth(req)

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("prometheus %s: %d %s", u, resp.StatusCode, truncate(string(body), 256))
	}
	return body, nil
}

// -----------------------------------------------------------------------------
// response shape
// -----------------------------------------------------------------------------

type promQueryResponse struct {
	Status    string        `json:"status"`
	ErrorType string        `json:"errorType"`
	Error     string        `json:"error"`
	Data      promQueryData `json:"data"`
}

type promQueryData struct {
	ResultType string            `json:"resultType"`
	Result     []json.RawMessage `json:"result"`
}

// promSeriesEnvelope handles both vector (single "value") and matrix
// (list of "values") result shapes.
type promSeriesEnvelope struct {
	Metric map[string]string `json:"metric"`
	Value  []interface{}     `json:"value"`
	Values [][]interface{}   `json:"values"`
}

func (d promQueryData) toSeries() ([]MetricSeries, error) {
	out := make([]MetricSeries, 0, len(d.Result))
	for _, raw := range d.Result {
		var env promSeriesEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("decode prometheus series: %w", err)
		}
		samples := make([]MetricSample, 0, len(env.Values)+1)
		if len(env.Value) == 2 {
			if sm, ok := parsePromSample(env.Value); ok {
				samples = append(samples, sm)
			}
		}
		for _, pair := range env.Values {
			if sm, ok := parsePromSample(pair); ok {
				samples = append(samples, sm)
			}
		}
		out = append(out, MetricSeries{Metric: env.Metric, Samples: samples})
	}
	return out, nil
}

// parsePromSample parses a Prometheus [<unix_float>, "<value_str>"] pair.
func parsePromSample(pair []interface{}) (MetricSample, bool) {
	if len(pair) != 2 {
		return MetricSample{}, false
	}
	tsFloat, ok := pair[0].(float64)
	if !ok {
		return MetricSample{}, false
	}
	valStr, ok := pair[1].(string)
	if !ok {
		return MetricSample{}, false
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		// NaN / +Inf etc. — skip the sample rather than fail the query.
		return MetricSample{}, false
	}
	sec := int64(tsFloat)
	nsec := int64((tsFloat - float64(sec)) * 1e9)
	return MetricSample{Timestamp: time.Unix(sec, nsec).UTC(), Value: val}, true
}
