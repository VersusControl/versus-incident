package agent

import (
	"context"
	"fmt"
	"time"

	analyzetools "github.com/VersusControl/versus-incident/pkg/agent/ai/analyze/tools"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/runbook/vectorindex"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
)

// signalReaderAdapter wraps a set of core.SignalSource instances so they
// satisfy analyzetools.SignalReader without leaking pkg/agent (or the
// concrete source types) into the tools package. The wrapped sources are
// an independent set built solely for the read-only get_related_logs
// tool, so calling Pull here never advances the worker's cursors.
type signalReaderAdapter struct {
	sources map[string]core.SignalSource
	order   []string
}

func newSignalReaderAdapter(sources []core.SignalSource) analyzetools.SignalReader {
	if len(sources) == 0 {
		return nil
	}
	m := make(map[string]core.SignalSource, len(sources))
	order := make([]string, 0, len(sources))
	for _, s := range sources {
		if s == nil {
			continue
		}
		name := s.Name()
		if _, dup := m[name]; dup {
			continue
		}
		m[name] = s
		order = append(order, name)
	}
	if len(m) == 0 {
		return nil
	}
	return &signalReaderAdapter{sources: m, order: order}
}

func (a *signalReaderAdapter) Sources() []string {
	if a == nil {
		return nil
	}
	return append([]string(nil), a.order...)
}

func (a *signalReaderAdapter) Pull(ctx context.Context, source string, since time.Time) ([]core.Signal, error) {
	if a == nil {
		return nil, fmt.Errorf("signal reader not configured")
	}
	src, ok := a.sources[source]
	if !ok {
		return nil, fmt.Errorf("unknown source %q", source)
	}
	sigs, _, err := src.Pull(ctx, since)
	return sigs, err
}

// catalogAdapter wraps *Catalog so it satisfies the
// analyzetools.PatternCatalog interface without leaking the agent
// package into the tools package. This keeps the import graph
// one-way: pkg/agent -> tools.
type catalogAdapter struct{ c *Catalog }

func newCatalogAdapter(c *Catalog) analyzetools.PatternCatalog {
	if c == nil {
		return nil
	}
	return &catalogAdapter{c: c}
}

func (a *catalogAdapter) Get(id string) *analyzetools.PatternView {
	if a == nil || a.c == nil {
		return nil
	}
	p := a.c.Get(id)
	if p == nil {
		return nil
	}
	v := toView(p)
	return &v
}

func (a *catalogAdapter) All() []*analyzetools.PatternView {
	if a == nil || a.c == nil {
		return nil
	}
	all := a.c.All()
	out := make([]*analyzetools.PatternView, 0, len(all))
	for _, p := range all {
		v := toView(p)
		out = append(out, &v)
	}
	return out
}

func (a *catalogAdapter) AllServices() map[string]analyzetools.ServiceInfo {
	if a == nil || a.c == nil {
		return nil
	}
	src := a.c.AllServices()
	out := make(map[string]analyzetools.ServiceInfo, len(src))
	for k, v := range src {
		out[k] = analyzetools.ServiceInfo{FirstSeen: v.FirstSeen}
	}
	return out
}

func toView(p *Pattern) analyzetools.PatternView {
	tags := append([]string(nil), p.Tags...)
	return analyzetools.PatternView{
		ID:        p.ID,
		Template:  p.Template,
		Source:    p.Source,
		Service:   p.Service,
		RuleName:  p.RuleName,
		Verdict:   p.Verdict,
		Tags:      tags,
		Count:     p.Count,
		Baseline:  p.BaselineFrequency,
		FirstSeen: p.FirstSeen,
		LastSeen:  p.LastSeen,
		Samples:   append([]string(nil), p.Samples...),
	}
}

// buildDependencyGraph converts the operator-authored config service
// graph into the tools-package DependencyGraph used by the
// describe_dependencies tool. A nil/empty input yields a nil graph so
// the tool is omitted by analyzetools.Default.
func buildDependencyGraph(nodes []config.ServiceDependency) *analyzetools.DependencyGraph {
	if len(nodes) == 0 {
		return nil
	}
	dependsOn := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		if n.Name == "" {
			continue
		}
		dependsOn[n.Name] = append(dependsOn[n.Name], n.DependsOn...)
	}
	if len(dependsOn) == 0 {
		return nil
	}
	return analyzetools.NewDependencyGraph(dependsOn)
}

// runbookSearcherAdapter wraps a read-only vector index
// (vectorindex.Index) so it satisfies analyzetools.RunbookSearcher
// without leaking pkg/runbook (the ingestion/write path) into the tools
// package. This keeps the import graph one-way (pkg/agent -> tools) and
// the analyze read-only guard green: the tool only ever sees a search
// seam, never the write path. A nil index yields a nil searcher so
// analyzetools.Default omits the find_runbook tool.
func newRunbookSearcherAdapter(idx vectorindex.Index) analyzetools.RunbookSearcher {
	if idx == nil {
		return nil
	}
	return &runbookSearcherAdapter{idx: idx}
}

type runbookSearcherAdapter struct{ idx vectorindex.Index }

// Search implements analyzetools.RunbookSearcher by delegating to the
// vector index and converting its results into the tools-package match
// shape. The context is accepted for interface symmetry; the in-memory
// index does not block on it.
func (a *runbookSearcherAdapter) Search(_ context.Context, query []float32, service string, limit int) ([]analyzetools.RunbookMatch, error) {
	if a == nil || a.idx == nil {
		return nil, nil
	}
	hits := a.idx.Search(query, service, limit)
	out := make([]analyzetools.RunbookMatch, 0, len(hits))
	for _, h := range hits {
		out = append(out, analyzetools.RunbookMatch{
			ID:      h.ID,
			Title:   h.Title,
			Service: h.Service,
			Score:   h.Score,
			Excerpt: h.Excerpt,
			Source:  h.Source,
		})
	}
	return out, nil
}

// buildGitRepos converts the operator-authored config repo list into the
// tools-package GitRepo slice used by the recent_changes change feed.
// Each repo's auth falls back to the global default (git.auth) when its
// own auth fields are empty.
func buildGitRepos(git config.RecentChangesGitConfig) []analyzetools.GitRepo {
	if len(git.Repos) == 0 {
		return nil
	}
	out := make([]analyzetools.GitRepo, 0, len(git.Repos))
	for _, r := range git.Repos {
		token := r.Auth.Token
		if token == "" {
			token = git.Auth.Token
		}
		sshKey := r.Auth.SSHKeyPath
		if sshKey == "" {
			sshKey = git.Auth.SSHKeyPath
		}
		out = append(out, analyzetools.GitRepo{
			URL:        r.URL,
			Branch:     r.Branch,
			Service:    r.Service,
			Token:      token,
			SSHKeyPath: sshKey,
		})
	}
	return out
}

// metricReaderAdapter wraps a *signalsources.PrometheusQuerier so it
// satisfies analyzetools.MetricReader without leaking pkg/signalsources
// into the tools package. The querier is built from the tools.yaml
// query_metrics config, independent of any detect-path prometheus
// SignalSource, so an on-demand analyze query never advances a worker
// cursor.
type metricReaderAdapter struct {
	querier *signalsources.PrometheusQuerier
}

// newMetricReaderAdapter builds the query_metrics reader from config. A
// blank address yields a nil reader so analyzetools.Default omits the
// tool (community installs without a metric backend are unaffected).
func newMetricReaderAdapter(cfg config.QueryMetricsPrometheusConfig) analyzetools.MetricReader {
	if cfg.Address == "" {
		return nil
	}
	q, err := signalsources.NewPrometheusQuerier(cfg.Address, signalsources.PrometheusAuth{
		BearerToken: cfg.BearerToken,
		Username:    cfg.Username,
		Password:    cfg.Password,
	}, cfg.InsecureSkipVerify)
	if err != nil {
		return nil
	}
	return &metricReaderAdapter{querier: q}
}

// NewMetricReader wraps an already-constructed shared PrometheusQuerier as
// an analyze-tool MetricReader. It is the programmatic seam an out-of-tree
// module (e.g. the enterprise metric data source) uses to point the
// query_metrics tool at its own backend client without re-deriving the
// adapter:
//
//	q, _ := signalsources.NewPrometheusQuerier(addr, auth, insecure)
//	tool := analyzetools.QueryMetrics{Reader: agent.NewMetricReader(q)}
//
// A nil querier yields a nil reader so the caller can pass it straight to
// analyzetools.Default (which omits the tool when the reader is nil).
func NewMetricReader(q *signalsources.PrometheusQuerier) analyzetools.MetricReader {
	if q == nil {
		return nil
	}
	return &metricReaderAdapter{querier: q}
}

// QueryRange implements analyzetools.MetricReader by running a PromQL
// range query over the last windowMinutes and converting the concrete
// signalsources series into the tools-package shape.
func (a *metricReaderAdapter) QueryRange(ctx context.Context, query string, windowMinutes int) ([]analyzetools.MetricSeries, error) {
	if a == nil || a.querier == nil {
		return nil, fmt.Errorf("metric reader not configured")
	}
	end := time.Now().UTC()
	start := end.Add(-time.Duration(windowMinutes) * time.Minute)
	// Pick a step that keeps the point count bounded (~120 points max).
	step := time.Duration(windowMinutes) * time.Minute / 120
	if step < time.Minute {
		step = time.Minute
	}
	series, err := a.querier.QueryRange(ctx, query, start, end, step)
	if err != nil {
		return nil, err
	}
	out := make([]analyzetools.MetricSeries, 0, len(series))
	for _, s := range series {
		samples := make([]analyzetools.MetricSample, 0, len(s.Samples))
		for _, sm := range s.Samples {
			samples = append(samples, analyzetools.MetricSample{Timestamp: sm.Timestamp, Value: sm.Value})
		}
		out = append(out, analyzetools.MetricSeries{Labels: s.Metric, Samples: samples})
	}
	return out, nil
}

// traceReaderAdapter wraps a *signalsources.TempoQuerier so it satisfies
// analyzetools.TraceReader without leaking pkg/signalsources into the
// tools package. Built from the tools.yaml query_traces config,
// independent of any detect-path traces SignalSource.
type traceReaderAdapter struct {
	querier *signalsources.TempoQuerier
}

// newTraceReaderAdapter builds the query_traces reader from config. A
// blank address yields a nil reader so analyzetools.Default omits the
// tool.
func newTraceReaderAdapter(cfg config.QueryTracesTempoConfig) analyzetools.TraceReader {
	if cfg.Address == "" {
		return nil
	}
	q, err := signalsources.NewTempoQuerier(cfg.Address, signalsources.PrometheusAuth{
		BearerToken: cfg.BearerToken,
		Username:    cfg.Username,
		Password:    cfg.Password,
	}, cfg.InsecureSkipVerify)
	if err != nil {
		return nil
	}
	return &traceReaderAdapter{querier: q}
}

// NewTraceReader wraps an already-constructed shared TempoQuerier as an
// analyze-tool TraceReader. It is the programmatic seam an out-of-tree
// module (e.g. the enterprise trace data source) uses to point the
// query_traces tool at its own backend client:
//
//	q, _ := signalsources.NewTempoQuerier(addr, auth, insecure)
//	tool := analyzetools.QueryTraces{Reader: agent.NewTraceReader(q), Redactor: red}
//
// A nil querier yields a nil reader so the caller can pass it straight to
// analyzetools.Default (which omits the tool when the reader is nil).
func NewTraceReader(q *signalsources.TempoQuerier) analyzetools.TraceReader {
	if q == nil {
		return nil
	}
	return &traceReaderAdapter{querier: q}
}

// QueryTraces implements analyzetools.TraceReader by building a TraceQL
// query from the optional service / trace_id filters, searching the last
// windowMinutes, and converting the concrete signalsources summaries into
// the tools-package shape.
func (a *traceReaderAdapter) QueryTraces(ctx context.Context, service, traceID string, windowMinutes, limit int) ([]analyzetools.TraceSummary, error) {
	if a == nil || a.querier == nil {
		return nil, fmt.Errorf("trace reader not configured")
	}
	end := time.Now().UTC()
	start := end.Add(-time.Duration(windowMinutes) * time.Minute)

	query := buildTraceQL(service, traceID)
	summaries, err := a.querier.Search(ctx, query, start, end, limit)
	if err != nil {
		return nil, err
	}
	out := make([]analyzetools.TraceSummary, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, analyzetools.TraceSummary{
			TraceID:    s.TraceID,
			Service:    s.Service,
			Operation:  s.Operation,
			DurationMs: s.DurationMs,
			Start:      s.Start,
			Error:      s.Error,
		})
	}
	return out, nil
}

// buildTraceQL assembles a TraceQL selector from the optional filters.
// trace_id takes priority (a direct lookup); otherwise it filters by
// service and falls back to the error-trace default.
func buildTraceQL(service, traceID string) string {
	if traceID != "" {
		return fmt.Sprintf("{ trace:id = %q }", traceID)
	}
	if service != "" {
		return fmt.Sprintf("{ resource.service.name = %q }", service)
	}
	return "{ status = error }"
}
