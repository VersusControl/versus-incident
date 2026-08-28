package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeTraceReader struct {
	traces     []TraceSummary
	err        error
	gotService string
	gotTraceID string
	gotStart   time.Time
	gotEnd     time.Time
	gotLimit   int
}

func (f *fakeTraceReader) QueryTraces(_ context.Context, service, traceID string, start, end time.Time, limit int) ([]TraceSummary, error) {
	f.gotService = service
	f.gotTraceID = traceID
	f.gotStart = start
	f.gotEnd = end
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	if len(f.traces) > limit {
		return f.traces[:limit], nil
	}
	return f.traces, nil
}

// upperRedactor is a trivial LineRedactor that uppercases input, so tests
// can prove the scrub path is exercised on trace strings.
type upperRedactor struct{}

func (upperRedactor) Scrub(s string) string { return strings.ToUpper(s) }

func invokeTraces(t *testing.T, qt QueryTraces, args map[string]any) (*toolResultView, error) {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, _ := json.Marshal(args)
		raw = b
	}
	res, err := qt.Invoke(context.Background(), raw)
	if err != nil {
		return nil, err
	}
	return newToolResultView(t, res), nil
}

func TestQueryTraces_Metadata(t *testing.T) {
	qt := QueryTraces{}
	if qt.Name() != "query_traces" {
		t.Fatalf("Name = %q", qt.Name())
	}
	if qt.Description() == "" {
		t.Fatal("empty description")
	}
	props := qt.ArgsSchema()["properties"].(map[string]any)
	for _, k := range []string{"service", "trace_id", "time_range_minutes", "window_minutes", "start", "end", "limit"} {
		if _, ok := props[k]; !ok {
			t.Errorf("schema missing %q", k)
		}
	}
}

func TestQueryTraces_RedactsAndClamps(t *testing.T) {
	reader := &fakeTraceReader{traces: []TraceSummary{
		{TraceID: "t1", Service: "api", Operation: "GET /orders", DurationMs: 50, Start: time.Now().UTC(), Error: true},
		{TraceID: "t2", Service: "web", Operation: "GET /home", DurationMs: 900, Start: time.Now().UTC()},
	}}
	qt := QueryTraces{Reader: reader, Redactor: upperRedactor{}}
	res, err := invokeTraces(t, qt, map[string]any{"window_minutes": 99999, "limit": 9999})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if got := reader.gotEnd.Sub(reader.gotStart); got != queryTracesMaxWindow*time.Minute {
		t.Errorf("window not clamped: %s", got)
	}
	if reader.gotLimit != queryTracesMaxLimit+1 {
		t.Errorf("limit over-fetch wrong: %d", reader.gotLimit)
	}
	if !res.Found {
		t.Fatal("expected Found")
	}
	traces := res.Data["traces"].([]any)
	if len(traces) != 2 {
		t.Fatalf("expected 2 traces, got %d", len(traces))
	}
	// Longest duration first + redaction applied (uppercased).
	first := traces[0].(map[string]any)
	if first["service"] != "WEB" {
		t.Errorf("expected redacted (uppercased) service, got %v", first["service"])
	}
	if first["duration_ms"].(float64) != 900 {
		t.Errorf("expected longest-duration trace first, got %v", first["duration_ms"])
	}
}

func TestQueryTraces_FiltersForwarded(t *testing.T) {
	reader := &fakeTraceReader{}
	qt := QueryTraces{Reader: reader}
	if _, err := invokeTraces(t, qt, map[string]any{"service": "api", "trace_id": "abc"}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if reader.gotService != "api" || reader.gotTraceID != "abc" {
		t.Errorf("filters not forwarded: service=%q trace=%q", reader.gotService, reader.gotTraceID)
	}
	if reader.gotEnd.Sub(reader.gotStart) != queryTracesDefaultWindow*time.Minute || reader.gotLimit != queryTracesDefaultLimit+1 {
		t.Errorf("defaults wrong: window=%s limit=%d", reader.gotEnd.Sub(reader.gotStart), reader.gotLimit)
	}
}

func TestQueryTracesReportsTruncation(t *testing.T) {
	reader := &fakeTraceReader{traces: []TraceSummary{
		{TraceID: "slow", DurationMs: 30},
		{TraceID: "medium", DurationMs: 20},
		{TraceID: "fast", DurationMs: 10},
	}}
	result, err := (QueryTraces{Reader: reader}).Invoke(context.Background(), mustArgs(t, queryTracesArgs{Limit: 2}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(result.Data["traces"].([]traceSummaryOut)) != 2 || result.Data["truncated"] != true {
		t.Fatalf("data = %+v", result.Data)
	}
	if _, ok := result.Data["traces_total"]; ok {
		t.Fatal("traces_total must be omitted when the backend cannot provide an exact total")
	}
}

func TestQueryTraces_EmptyAndErrors(t *testing.T) {
	// Clean miss.
	qt := QueryTraces{Reader: &fakeTraceReader{}}
	res, err := invokeTraces(t, qt, nil)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Found {
		t.Error("empty should be Found=false")
	}
	// Nil reader.
	unavailable, unavailableErr := (QueryTraces{}).Invoke(context.Background(), nil)
	assertUnavailable(t, unavailable, unavailableErr)
	// Reader error propagates.
	qtErr := QueryTraces{Reader: &fakeTraceReader{err: errors.New("boom")}}
	if _, err := invokeTraces(t, qtErr, nil); err == nil {
		t.Error("expected reader error to propagate")
	}
}
