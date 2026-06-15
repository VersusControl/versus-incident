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
	gotWindow  int
	gotLimit   int
}

func (f *fakeTraceReader) QueryTraces(_ context.Context, service, traceID string, windowMinutes, limit int) ([]TraceSummary, error) {
	f.gotService = service
	f.gotTraceID = traceID
	f.gotWindow = windowMinutes
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
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
	for _, k := range []string{"service", "trace_id", "window_minutes", "limit"} {
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
	if reader.gotWindow != queryTracesMaxWindow {
		t.Errorf("window not clamped: %d", reader.gotWindow)
	}
	if reader.gotLimit != queryTracesMaxLimit {
		t.Errorf("limit not clamped: %d", reader.gotLimit)
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
	if reader.gotWindow != queryTracesDefaultWindow || reader.gotLimit != queryTracesDefaultLimit {
		t.Errorf("defaults wrong: window=%d limit=%d", reader.gotWindow, reader.gotLimit)
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
	if _, err := (QueryTraces{}).Invoke(context.Background(), nil); err == nil {
		t.Error("expected error with nil reader")
	}
	// Reader error propagates.
	qtErr := QueryTraces{Reader: &fakeTraceReader{err: errors.New("boom")}}
	if _, err := invokeTraces(t, qtErr, nil); err == nil {
		t.Error("expected reader error to propagate")
	}
}
