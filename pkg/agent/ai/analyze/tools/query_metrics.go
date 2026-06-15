package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// MetricSample is one (timestamp, value) point of a metric series,
// declared locally so the tools package stays decoupled from
// pkg/signalsources and pkg/agent (a bridge converts the concrete
// types). Mirrors signalsources.MetricSample.
type MetricSample struct {
	Timestamp time.Time
	Value     float64
}

// MetricSeries is one labelled series returned by the MetricReader.
type MetricSeries struct {
	Labels  map[string]string
	Samples []MetricSample
}

// MetricReader is the read-only slice of a metric backend the
// query_metrics tool depends on. Declared as a local interface (not
// importing pkg/signalsources) to keep the import graph one-directional.
// The bridge in pkg/agent wraps a Prometheus querier so an on-demand
// analyze query never touches the detect-path source cursors.
type MetricReader interface {
	// QueryRange runs a PromQL range query over the last `windowMinutes`
	// and returns the matching series. An empty result is a clean miss,
	// not an error.
	QueryRange(ctx context.Context, query string, windowMinutes int) ([]MetricSeries, error)
}

// QueryMetrics runs an on-demand PromQL range query against the
// configured metric backend so the analyze agent can inspect a metric's
// recent behaviour. It is strictly read-only.
type QueryMetrics struct {
	Reader MetricReader
}

const (
	queryMetricsDefaultWindow = 30
	queryMetricsMaxWindow     = 1440
	queryMetricsMaxSeries     = 50
)

// Name implements core.AnalyzeTool.
func (QueryMetrics) Name() string { return "query_metrics" }

// Description implements core.AnalyzeTool.
func (QueryMetrics) Description() string {
	return "Run a PromQL range query against the configured Prometheus backend and return per-series summaries (last, min, max, delta) over the window. Use it to inspect a metric's recent behaviour while investigating an incident."
}

// ArgsSchema implements core.AnalyzeTool.
func (QueryMetrics) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "PromQL expression to evaluate, e.g. `rate(http_requests_total{job=\"api\"}[5m])`. Required unless 'service' is given.",
			},
			"service": map[string]any{
				"type":        "string",
				"description": "Optional service name. When 'query' is omitted, a default request-rate query is built for this service (sum(rate(http_requests_total{service=\"<svc>\"}[5m]))).",
			},
			"window_minutes": map[string]any{
				"type":        "integer",
				"description": "Look back this many minutes from now. Default 30, max 1440.",
			},
		},
	}
}

type queryMetricsArgs struct {
	Query         string `json:"query"`
	Service       string `json:"service"`
	WindowMinutes int    `json:"window_minutes"`
}

type metricSeriesSummary struct {
	Labels map[string]string `json:"labels,omitempty"`
	Count  int               `json:"count"`
	Last   float64           `json:"last"`
	Min    float64           `json:"min"`
	Max    float64           `json:"max"`
	Delta  float64           `json:"delta"`
}

// Invoke implements core.AnalyzeTool.
func (qm QueryMetrics) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if qm.Reader == nil {
		return nil, fmt.Errorf("query_metrics: no metric backend configured")
	}
	var a queryMetricsArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("query_metrics: parse args: %w", err)
		}
	}
	if a.WindowMinutes <= 0 {
		a.WindowMinutes = queryMetricsDefaultWindow
	}
	if a.WindowMinutes > queryMetricsMaxWindow {
		a.WindowMinutes = queryMetricsMaxWindow
	}

	query := a.Query
	if query == "" {
		if a.Service == "" {
			return nil, fmt.Errorf("query_metrics: either 'query' or 'service' is required")
		}
		query = fmt.Sprintf("sum(rate(http_requests_total{service=%q}[5m]))", a.Service)
	}

	series, err := qm.Reader.QueryRange(ctx, query, a.WindowMinutes)
	if err != nil {
		return nil, fmt.Errorf("query_metrics: %w", err)
	}

	summaries := make([]metricSeriesSummary, 0, len(series))
	for _, s := range series {
		if sum, ok := summarize(s); ok {
			summaries = append(summaries, sum)
		}
	}
	// Stable order: largest absolute delta first so the most-changed
	// series surface to the model.
	sort.SliceStable(summaries, func(i, j int) bool {
		return math.Abs(summaries[i].Delta) > math.Abs(summaries[j].Delta)
	})
	if len(summaries) > queryMetricsMaxSeries {
		summaries = summaries[:queryMetricsMaxSeries]
	}

	return &core.ToolResult{
		Tool:  QueryMetrics{}.Name(),
		Found: len(summaries) > 0,
		Data: map[string]any{
			"query":          query,
			"window_minutes": a.WindowMinutes,
			"series_count":   len(summaries),
			"series":         summaries,
		},
	}, nil
}

// summarize reduces a series' samples to last/min/max/delta. A series
// with no samples is skipped (ok=false).
func summarize(s MetricSeries) (metricSeriesSummary, bool) {
	if len(s.Samples) == 0 {
		return metricSeriesSummary{}, false
	}
	first := s.Samples[0].Value
	last := s.Samples[len(s.Samples)-1].Value
	min, max := first, first
	for _, sm := range s.Samples {
		if sm.Value < min {
			min = sm.Value
		}
		if sm.Value > max {
			max = sm.Value
		}
	}
	return metricSeriesSummary{
		Labels: s.Labels,
		Count:  len(s.Samples),
		Last:   last,
		Min:    min,
		Max:    max,
		Delta:  last - first,
	}, true
}
