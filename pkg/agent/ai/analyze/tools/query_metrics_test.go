package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeMetricReader struct {
	series    []MetricSeries
	err       error
	gotQuery  string
	gotWindow int
	callCount int
}

func (f *fakeMetricReader) QueryRange(_ context.Context, query string, windowMinutes int) ([]MetricSeries, error) {
	f.callCount++
	f.gotQuery = query
	f.gotWindow = windowMinutes
	if f.err != nil {
		return nil, f.err
	}
	return f.series, nil
}

func invokeMetrics(t *testing.T, qm QueryMetrics, args map[string]any) (*toolResultView, error) {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, _ := json.Marshal(args)
		raw = b
	}
	res, err := qm.Invoke(context.Background(), raw)
	if err != nil {
		return nil, err
	}
	return newToolResultView(t, res), nil
}

// toolResultView re-marshals the ToolResult through JSON so tests inspect
// the wire shape the model actually receives.
type toolResultView struct {
	Tool  string
	Found bool
	Data  map[string]any
}

func newToolResultView(t *testing.T, res interface{}) *toolResultView {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var v struct {
		Tool  string         `json:"tool"`
		Found bool           `json:"found"`
		Data  map[string]any `json:"data"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return &toolResultView{Tool: v.Tool, Found: v.Found, Data: v.Data}
}

func TestQueryMetrics_Metadata(t *testing.T) {
	qm := QueryMetrics{}
	if qm.Name() != "query_metrics" {
		t.Fatalf("Name = %q", qm.Name())
	}
	if qm.Description() == "" {
		t.Fatal("empty description")
	}
	props := qm.ArgsSchema()["properties"].(map[string]any)
	for _, k := range []string{"query", "service", "window_minutes"} {
		if _, ok := props[k]; !ok {
			t.Errorf("schema missing %q", k)
		}
	}
}

func TestQueryMetrics_SummariesAndWindowClamp(t *testing.T) {
	now := time.Now().UTC()
	reader := &fakeMetricReader{series: []MetricSeries{
		{
			Labels: map[string]string{"service": "api"},
			Samples: []MetricSample{
				{Timestamp: now.Add(-3 * time.Minute), Value: 1},
				{Timestamp: now.Add(-2 * time.Minute), Value: 5},
				{Timestamp: now.Add(-1 * time.Minute), Value: 3},
			},
		},
	}}
	qm := QueryMetrics{Reader: reader}
	res, err := invokeMetrics(t, qm, map[string]any{"query": "up", "window_minutes": 99999})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if reader.gotWindow != queryMetricsMaxWindow {
		t.Errorf("window not clamped: got %d", reader.gotWindow)
	}
	if !res.Found {
		t.Fatal("expected Found")
	}
	series := res.Data["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	s := series[0].(map[string]any)
	if s["last"].(float64) != 3 || s["min"].(float64) != 1 || s["max"].(float64) != 5 || s["delta"].(float64) != 2 {
		t.Errorf("bad summary: %+v", s)
	}
}

func TestQueryMetrics_ServiceDefaultQuery(t *testing.T) {
	reader := &fakeMetricReader{}
	qm := QueryMetrics{Reader: reader}
	if _, err := invokeMetrics(t, qm, map[string]any{"service": "checkout"}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if reader.gotQuery == "" || !strings.Contains(reader.gotQuery, "checkout") {
		t.Errorf("expected default query for service, got %q", reader.gotQuery)
	}
}

func TestQueryMetrics_EmptyIsCleanMiss(t *testing.T) {
	qm := QueryMetrics{Reader: &fakeMetricReader{series: nil}}
	res, err := invokeMetrics(t, qm, map[string]any{"query": "up"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Found {
		t.Error("empty result should be Found=false")
	}
}

func TestQueryMetrics_Errors(t *testing.T) {
	// No reader.
	if _, err := (QueryMetrics{}).Invoke(context.Background(), nil); err == nil {
		t.Error("expected error with nil reader")
	}
	// Neither query nor service.
	qm := QueryMetrics{Reader: &fakeMetricReader{}}
	if _, err := qm.Invoke(context.Background(), nil); err == nil {
		t.Error("expected error when neither query nor service given")
	}
	// Reader error propagates.
	qmErr := QueryMetrics{Reader: &fakeMetricReader{err: errors.New("boom")}}
	if _, err := invokeMetrics(t, qmErr, map[string]any{"query": "up"}); err == nil {
		t.Error("expected reader error to propagate")
	}
}
