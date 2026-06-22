package signalsources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// promRangeBody marshals a fake `query_range` matrix response.
func promRangeBody(t *testing.T, result []promSeriesEnvelope) []byte {
	t.Helper()
	raws := make([]json.RawMessage, 0, len(result))
	for _, r := range result {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal series: %v", err)
		}
		raws = append(raws, b)
	}
	resp := promQueryResponse{Status: "success"}
	resp.Data.ResultType = "matrix"
	resp.Data.Result = raws
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func unixFloat(t time.Time) float64 { return float64(t.UTC().UnixNano()) / 1e9 }

func TestPrometheusQuerier_QueryRangeParsesMatrix(t *testing.T) {
	t1 := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)
	t2 := t1.Add(time.Minute)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("querier must be GET-only, got %s", r.Method)
		}
		if r.URL.Query().Get("query") == "" {
			t.Error("missing query")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(promRangeBody(t, []promSeriesEnvelope{
			{
				Metric: map[string]string{"__name__": "http_5xx_rate", "service": "api"},
				Values: [][]interface{}{
					{unixFloat(t1), "0"},
					{unixFloat(t2), "0.42"},
				},
			},
		}))
	}))
	defer ts.Close()

	q, err := NewPrometheusQuerier(ts.URL, PrometheusAuth{}, false)
	if err != nil {
		t.Fatalf("new querier: %v", err)
	}
	series, err := q.QueryRange(context.Background(), "http_5xx_rate", t1, t2, time.Minute)
	if err != nil {
		t.Fatalf("query range: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	if series[0].Metric["service"] != "api" {
		t.Errorf("service label = %q", series[0].Metric["service"])
	}
	if len(series[0].Samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(series[0].Samples))
	}
	if series[0].Samples[1].Value != 0.42 {
		t.Errorf("last value = %v", series[0].Samples[1].Value)
	}
}

func TestPrometheusQuerier_QueryInstantParsesVector(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write(promRangeBody(t, []promSeriesEnvelope{
			{
				Metric: map[string]string{"__name__": "up", "job": "api"},
				Value:  []interface{}{unixFloat(now), "1"},
			},
		}))
	}))
	defer ts.Close()

	q, _ := NewPrometheusQuerier(ts.URL, PrometheusAuth{}, false)
	series, err := q.QueryInstant(context.Background(), "up", now)
	if err != nil {
		t.Fatalf("query instant: %v", err)
	}
	if len(series) != 1 || len(series[0].Samples) != 1 {
		t.Fatalf("expected 1 series with 1 sample, got %+v", series)
	}
	if series[0].Samples[0].Value != 1 {
		t.Errorf("value = %v", series[0].Samples[0].Value)
	}
}

func TestPrometheusQuerier_Auth(t *testing.T) {
	// Bearer auth.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("bearer auth = %q", r.Header.Get("Authorization"))
		}
		w.Write(promRangeBody(t, nil))
	}))
	defer ts.Close()
	q, _ := NewPrometheusQuerier(ts.URL, PrometheusAuth{BearerToken: "tok"}, false)
	if _, err := q.QueryInstant(context.Background(), "x", time.Now()); err != nil {
		t.Fatalf("instant: %v", err)
	}

	// Basic auth.
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			t.Errorf("basic auth = %q", r.Header.Get("Authorization"))
		}
		w.Write(promRangeBody(t, nil))
	}))
	defer ts2.Close()
	q2, _ := NewPrometheusQuerier(ts2.URL, PrometheusAuth{Username: "u", Password: "p"}, false)
	if _, err := q2.QueryInstant(context.Background(), "x", time.Now()); err != nil {
		t.Fatalf("instant: %v", err)
	}
}

func TestPrometheusQuerier_HTTPErrorSurfaces(t *testing.T) {
	tsErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer tsErr.Close()
	q, _ := NewPrometheusQuerier(tsErr.URL, PrometheusAuth{}, false)
	_, err := q.QueryRange(context.Background(), "x", time.Now().Add(-time.Hour), time.Now(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "prometheus") {
		t.Fatalf("expected prometheus error, got %v", err)
	}
}

func TestPrometheusQuerier_AddressRequired(t *testing.T) {
	if _, err := NewPrometheusQuerier("", PrometheusAuth{}, false); err == nil {
		t.Error("expected error for empty address")
	}
}

// -----------------------------------------------------------------------------
// Discovery reads (GET-only).
// -----------------------------------------------------------------------------

func TestPrometheusQuerier_MetadataParses(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery must be GET-only, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/metadata" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{` +
			`"http_requests_total":[{"type":"counter","help":"total reqs","unit":""}],` +
			`"node_memory_bytes":[{"type":"gauge","help":"mem","unit":"bytes"}]}}`))
	}))
	defer ts.Close()

	q, _ := NewPrometheusQuerier(ts.URL, PrometheusAuth{}, false)
	meta, err := q.Metadata(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if len(meta) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(meta))
	}
	if meta["http_requests_total"].Type != "counter" {
		t.Errorf("http_requests_total type = %q", meta["http_requests_total"].Type)
	}
	if meta["node_memory_bytes"].Unit != "bytes" {
		t.Errorf("node_memory_bytes unit = %q", meta["node_memory_bytes"].Unit)
	}
}

func TestPrometheusQuerier_LabelValuesParsesAndSendsMatchers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery must be GET-only, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/label/service/values" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query()["match[]"]; len(got) != 1 || got[0] != `up{job="api"}` {
			t.Errorf("match[] = %v", got)
		}
		w.Write([]byte(`{"status":"success","data":["api","web","worker"]}`))
	}))
	defer ts.Close()

	q, _ := NewPrometheusQuerier(ts.URL, PrometheusAuth{}, false)
	vals, err := q.LabelValues(context.Background(), "service", time.Time{}, time.Time{}, `up{job="api"}`)
	if err != nil {
		t.Fatalf("label values: %v", err)
	}
	if len(vals) != 3 || vals[0] != "api" {
		t.Fatalf("values = %v", vals)
	}
}

func TestPrometheusQuerier_LabelValuesRequiresName(t *testing.T) {
	q, _ := NewPrometheusQuerier("http://example", PrometheusAuth{}, false)
	if _, err := q.LabelValues(context.Background(), "", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error for empty label name")
	}
}

func TestPrometheusQuerier_SeriesParsesLabelSets(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery must be GET-only, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/series" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query()["match[]"]; len(got) != 1 || got[0] != "up" {
			t.Errorf("match[] = %v", got)
		}
		w.Write([]byte(`{"status":"success","data":[` +
			`{"__name__":"up","job":"api","instance":"a:9090"},` +
			`{"__name__":"up","job":"web","instance":"b:9090"}]}`))
	}))
	defer ts.Close()

	q, _ := NewPrometheusQuerier(ts.URL, PrometheusAuth{}, false)
	series, err := q.Series(context.Background(), time.Time{}, time.Time{}, "up")
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 label sets, got %d", len(series))
	}
	if series[0]["job"] != "api" {
		t.Errorf("first job = %q", series[0]["job"])
	}
}

func TestPrometheusQuerier_SeriesRequiresSelector(t *testing.T) {
	q, _ := NewPrometheusQuerier("http://example", PrometheusAuth{}, false)
	if _, err := q.Series(context.Background(), time.Time{}, time.Time{}); err == nil {
		t.Error("expected error when no selector is given")
	}
}

// TestPrometheusQuerier_DiscoveryReadsSendTimeRange proves the discovery reads
// thread a bounded start/end window onto the GET (the fix for Mimir/Cortex/
// Thanos/VictoriaMetrics/Grafana Cloud, which return empty without it) and that
// the zero time omits the param (vanilla Prometheus "all data" behavior).
func TestPrometheusQuerier_DiscoveryReadsSendTimeRange(t *testing.T) {
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	end := time.Now().UTC().Truncate(time.Second)

	var gotStart, gotEnd string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery must be GET-only, got %s", r.Method)
		}
		gotStart = r.URL.Query().Get("start")
		gotEnd = r.URL.Query().Get("end")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/label/"):
			w.Write([]byte(`{"status":"success","data":["api"]}`))
		case r.URL.Path == "/api/v1/series":
			w.Write([]byte(`{"status":"success","data":[{"__name__":"up"}]}`))
		default:
			w.Write([]byte(`{"status":"success","data":{}}`))
		}
	}))
	defer ts.Close()
	q, _ := NewPrometheusQuerier(ts.URL, PrometheusAuth{}, false)

	// Windowed reads must carry both params.
	if _, err := q.LabelValues(context.Background(), "service", start, end); err != nil {
		t.Fatalf("label values: %v", err)
	}
	if gotStart == "" || gotEnd == "" {
		t.Errorf("LabelValues did not send start/end (start=%q end=%q)", gotStart, gotEnd)
	}
	if _, err := q.Series(context.Background(), start, end, "up"); err != nil {
		t.Fatalf("series: %v", err)
	}
	if gotStart == "" || gotEnd == "" {
		t.Errorf("Series did not send start/end (start=%q end=%q)", gotStart, gotEnd)
	}
	if _, err := q.Metadata(context.Background(), start, end); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if gotStart == "" || gotEnd == "" {
		t.Errorf("Metadata did not send start/end (start=%q end=%q)", gotStart, gotEnd)
	}

	// Zero time omits the params (backward-compatible vanilla behavior).
	gotStart, gotEnd = "x", "x"
	if _, err := q.LabelValues(context.Background(), "service", time.Time{}, time.Time{}); err != nil {
		t.Fatalf("label values (zero): %v", err)
	}
	if gotStart != "" || gotEnd != "" {
		t.Errorf("zero-time LabelValues sent start/end (start=%q end=%q)", gotStart, gotEnd)
	}
}
