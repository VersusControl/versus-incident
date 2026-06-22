package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// TraceSummary is one trace returned by the TraceReader, flattened to the
// fields the analyze agent reasons over. Declared locally so the tools
// package stays decoupled from pkg/signalsources; a bridge in pkg/agent
// converts the concrete signalsources.TraceSummary.
type TraceSummary struct {
	TraceID    string
	Service    string
	Operation  string
	DurationMs float64
	Start      time.Time
	Error      bool
}

// TraceReader is the read-only slice of a trace backend the query_traces
// tool depends on. Declared as a local interface (not importing
// pkg/signalsources) to keep the import graph one-directional. The bridge
// in pkg/agent wraps a Tempo querier so an on-demand analyze search never
// touches the detect-path source cursors.
type TraceReader interface {
	// QueryTraces searches the backend over the last `windowMinutes`,
	// optionally narrowing by service and/or trace_id, and returns up to
	// `limit` summaries. An empty result is a clean miss, not an error.
	QueryTraces(ctx context.Context, service, traceID string, windowMinutes, limit int) ([]TraceSummary, error)
}

// QueryTraces searches the configured trace backend for recent error /
// latency-outlier traces so the analyze agent can correlate an incident
// with distributed-tracing evidence. It is strictly read-only and scrubs
// every service/operation string through the redactor before returning.
type QueryTraces struct {
	Reader   TraceReader
	Redactor LineRedactor
}

const (
	queryTracesDefaultWindow = 15
	queryTracesMaxWindow     = 1440
	queryTracesDefaultLimit  = 20
	queryTracesMaxLimit      = 100
)

// Name implements core.AnalyzeTool.
func (QueryTraces) Name() string { return "query_traces" }

// Description implements core.AnalyzeTool.
func (QueryTraces) Description() string {
	return "Search the configured trace backend (Tempo) for recent error / latency-outlier traces, optionally filtered by service or trace_id. Returns redacted span summaries (service, operation, duration, error) so you can correlate an incident with distributed traces."
}

// ArgsSchema implements core.AnalyzeTool.
func (QueryTraces) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"service": map[string]any{
				"type":        "string",
				"description": "Optional service name to filter traces by.",
			},
			"trace_id": map[string]any{
				"type":        "string",
				"description": "Optional trace ID to fetch a specific trace's summary.",
			},
			"window_minutes": map[string]any{
				"type":        "integer",
				"description": "Look back this many minutes from now. Default 15, max 1440.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Cap the number of traces returned. Default 20, max 100.",
			},
		},
	}
}

type queryTracesArgs struct {
	Service       string `json:"service"`
	TraceID       string `json:"trace_id"`
	WindowMinutes int    `json:"window_minutes"`
	Limit         int    `json:"limit"`
}

type traceSummaryOut struct {
	TraceID    string  `json:"trace_id"`
	Service    string  `json:"service,omitempty"`
	Operation  string  `json:"operation,omitempty"`
	DurationMs float64 `json:"duration_ms"`
	Error      bool    `json:"error"`
	Start      string  `json:"start,omitempty"`
}

// Invoke implements core.AnalyzeTool.
func (qt QueryTraces) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if qt.Reader == nil {
		return nil, fmt.Errorf("query_traces: no trace backend configured")
	}
	var a queryTracesArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("query_traces: parse args: %w", err)
		}
	}
	if a.WindowMinutes <= 0 {
		a.WindowMinutes = queryTracesDefaultWindow
	}
	if a.WindowMinutes > queryTracesMaxWindow {
		a.WindowMinutes = queryTracesMaxWindow
	}
	if a.Limit <= 0 {
		a.Limit = queryTracesDefaultLimit
	}
	if a.Limit > queryTracesMaxLimit {
		a.Limit = queryTracesMaxLimit
	}

	traces, err := qt.Reader.QueryTraces(ctx, a.Service, a.TraceID, a.WindowMinutes, a.Limit)
	if err != nil {
		return nil, fmt.Errorf("query_traces: %w", err)
	}

	out := make([]traceSummaryOut, 0, len(traces))
	for _, t := range traces {
		start := ""
		if !t.Start.IsZero() {
			start = t.Start.UTC().Format(time.RFC3339)
		}
		out = append(out, traceSummaryOut{
			TraceID:    t.TraceID,
			Service:    qt.scrub(t.Service),
			Operation:  qt.scrub(t.Operation),
			DurationMs: t.DurationMs,
			Error:      t.Error,
			Start:      start,
		})
	}

	// Longest duration first so the slowest traces surface to the model.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].DurationMs > out[j].DurationMs
	})

	return &core.ToolResult{
		Tool:  QueryTraces{}.Name(),
		Found: len(out) > 0,
		Data: map[string]any{
			"count":          len(out),
			"window_minutes": a.WindowMinutes,
			"service":        a.Service,
			"trace_id":       a.TraceID,
			"traces":         out,
		},
	}, nil
}

func (qt QueryTraces) scrub(s string) string {
	if qt.Redactor == nil {
		return s
	}
	return qt.Redactor.Scrub(s)
}
