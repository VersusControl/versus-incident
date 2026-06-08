// Package tools holds the read-only tool catalog exposed to the
// analyze-kind AI agent. Every tool in this package MUST be safely
// read-only: no Save*, Update*, Delete*, http POST/PUT, on-call
// trigger, or notification dispatch. The import-graph guard test in
// pkg/agent/ai/analyze enforces that this package does not transitively
// depend on services.CreateIncident or any provider in pkg/common.
package tools

import (
	"context"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// PatternCatalog is the read-only slice of *agent.Catalog that the
// analyze tools depend on. Declaring it as a local interface (instead
// of importing pkg/agent) keeps the import graph one-directional:
// pkg/agent imports this package via the factory, not the reverse.
type PatternCatalog interface {
	Get(id string) *PatternView
	All() []*PatternView
	AllServices() map[string]ServiceInfo
}

// PatternView mirrors agent.Pattern with the fields the analyze tools
// surface. agent.Catalog implements the interface via a thin adapter
// in the factory (see pkg/agent/ai/analyze/tools/adapter.go on the
// agent side — implemented as a private wrapper).
type PatternView struct {
	ID        string
	Template  string
	Source    string
	Service   string
	RuleName  string
	Verdict   string
	Tags      []string
	Count     int
	Baseline  float64
	FirstSeen time.Time
	LastSeen  time.Time
}

// ServiceInfo mirrors agent.ServiceInfo for the tools layer.
type ServiceInfo struct {
	FirstSeen time.Time
}

// SignalReader is the read-only slice of the configured signal sources
// the analyze tools depend on. Declaring it as a local interface (not
// importing pkg/agent or pkg/signalsources) keeps the import graph
// one-directional. The bridge in pkg/agent/analyze_adapter.go wraps an
// independent source set so pulling logs during an analysis never
// advances the worker's polling cursors.
type SignalReader interface {
	// Sources returns the names of every configured source.
	Sources() []string
	// Pull returns signals from the named source at or after `since`.
	// The reader does no windowing or capping; callers filter and cap
	// client-side. An unknown source name yields an error.
	Pull(ctx context.Context, source string, since time.Time) ([]core.Signal, error)
}

// LineRedactor scrubs sensitive substrings from a single log line
// before it is handed to the model. *agent.Redactor satisfies this
// interface directly via its Scrub method.
type LineRedactor interface {
	Scrub(s string) string
}

// ServiceExtractor pulls a service name out of a raw log message using
// the operator-configured `agent.service_patterns`. *agent.ServiceMatcher
// satisfies this interface directly via its Extract method, so the
// get_related_logs service filter matches lines the exact same way the
// worker attributes signals to services. A nil extractor (or an empty
// result) makes the tool fall back to structured fields / source name.
type ServiceExtractor interface {
	Extract(message string) string
}

// Default returns the production tool set wired to the given storage
// and catalog. Callers can also assemble a custom set by constructing
// the individual tool structs.
//
// reader/redactor power the get_related_logs tool. When reader is nil
// (no sources configured) that tool is omitted; when redactor is nil
// log lines are returned unscrubbed (callers should always pass a
// redactor in production). services is the same ServiceMatcher the
// worker uses so the get_related_logs service filter matches lines
// consistently; it may be nil (the filter then falls back to fields).
//
// graph powers the describe_dependencies tool. When nil or empty (no
// service-dependency graph configured) that tool is omitted.
//
// changes powers the recent_changes tool. When nil (no change feed
// configured) that tool is omitted; the tool itself still treats a
// missing or empty feed as a clean Found=false rather than an error.
//
// embedder + runbooks power the find_runbook tool. The tool is opt-in:
// it is registered ONLY when BOTH are non-nil (an embedder is
// configured AND a runbook index is built). A community install with no
// runbook config leaves both nil, so the tool is omitted and behaviour
// is unchanged. An empty (but configured) corpus still registers and
// returns Found:false rather than an error.
func Default(store storage.Provider, cat PatternCatalog, reader SignalReader, redactor LineRedactor, services ServiceExtractor, graph *DependencyGraph, changes ChangeFeed, embedder core.Embedder, runbooks RunbookSearcher) []core.AnalyzeTool {
	out := make([]core.AnalyzeTool, 0, 8)
	if store != nil {
		out = append(out, RecentIncidents{Store: store})
	}
	if cat != nil {
		out = append(out, PatternHistory{Catalog: cat})
		out = append(out, PatternSearch{Catalog: cat})
		out = append(out, DescribeService{Catalog: cat})
	}
	if reader != nil {
		out = append(out, RelatedLogs{Reader: reader, Redactor: redactor, Services: services})
	}
	if graph != nil && graph.Len() > 0 {
		out = append(out, DescribeDependencies{Graph: graph, Store: store})
	}
	if changes != nil {
		out = append(out, RecentChanges{Feed: changes})
	}
	if embedder != nil && runbooks != nil {
		out = append(out, FindRunbook{Embedder: embedder, Index: runbooks, Redactor: redactor})
	}
	return out
}
