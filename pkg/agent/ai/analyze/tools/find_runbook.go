package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// RunbookMatch is one runbook hit returned to the model. It is the
// tools-package mirror of the vector index's result so the tool stays
// decoupled from pkg/runbook (the write path): the bridge in
// pkg/agent/analyze_adapter.go converts the concrete index results into
// this shape, keeping the import graph one-directional and the
// read-only guard green.
type RunbookMatch struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Service string  `json:"service,omitempty"`
	Score   float32 `json:"score"`
	Excerpt string  `json:"excerpt,omitempty"`
	Source  string  `json:"source,omitempty"`
}

// RunbookSearcher is the read-only vector-search seam the find_runbook
// tool depends on. Declared as a local interface (not an import of
// pkg/runbook) so the tools package never pulls in the ingestion/write
// path — the import-graph guard enforces this. Search takes an already
// embedded query vector, an optional service filter, and a result cap.
type RunbookSearcher interface {
	Search(ctx context.Context, query []float32, service string, limit int) ([]RunbookMatch, error)
}

// FindRunbook is the read-only runbook-RAG tool. During an
// investigation it embeds a redacted query derived from the incident,
// runs a top-K similarity search over the operator-supplied runbook
// corpus, and returns the best-matching excerpts so the model can
// ground its finding in the team's own remediation docs. It performs NO
// writes, NO ingestion, NO on-call trigger, and NO notification — it is
// search-only.
//
// The query MUST be scrubbed through the redactor before it reaches the
// embedder: the embeddings call is the same external trust boundary as
// the chat-completion call, so incident-derived text never egresses raw.
type FindRunbook struct {
	Embedder core.Embedder
	Index    RunbookSearcher
	Redactor LineRedactor
}

const (
	findRunbookDefaultLimit = 5
	findRunbookMaxLimit     = 20
)

// Name implements core.AnalyzeTool.
func (FindRunbook) Name() string { return "find_runbook" }

// Description implements core.AnalyzeTool.
func (FindRunbook) Description() string {
	return "Search the team's runbook corpus for the remediation docs most relevant to this incident. Provide a natural-language query (and optionally a service) and get back the best-matching runbook excerpts to ground your analysis. Read-only: it never executes any remediation."
}

// ArgsSchema implements core.AnalyzeTool.
func (FindRunbook) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language description of the problem to find runbooks for (e.g. \"postgres connection pool exhausted on the api service\").",
			},
			"service": map[string]any{
				"type":        "string",
				"description": "Optional service name to restrict matches to (case-insensitive exact match).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Cap the number of runbook matches returned. Default 5, max 20.",
			},
		},
	}
}

type findRunbookArgs struct {
	Query   string `json:"query"`
	Service string `json:"service"`
	Limit   int    `json:"limit"`
}

// Invoke implements core.AnalyzeTool. Flow: scrub query -> embed ->
// top-K search -> scrub excerpts -> return.
func (fr FindRunbook) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if fr.Embedder == nil || fr.Index == nil {
		// Defensive: Default never registers the tool without both, so
		// this only fires if a caller constructs it directly.
		return nil, fmt.Errorf("find_runbook: embedder and runbook index are required")
	}

	var a findRunbookArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("find_runbook: parse args: %w", err)
		}
	}
	a.Query = strings.TrimSpace(a.Query)
	if a.Query == "" {
		return nil, fmt.Errorf("find_runbook: query is required")
	}
	if a.Limit <= 0 {
		a.Limit = findRunbookDefaultLimit
	}
	if a.Limit > findRunbookMaxLimit {
		a.Limit = findRunbookMaxLimit
	}

	// Redactor before the embeddings egress — the incident-derived query
	// may carry raw payload, and the embeddings call is an external trust
	// boundary (mirrors how get_related_logs scrubs before any AI call).
	query := fr.scrub(a.Query)

	vecs, err := fr.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("find_runbook: embed query: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("find_runbook: embedder returned no vector")
	}

	matches, err := fr.Index.Search(ctx, vecs[0], a.Service, a.Limit)
	if err != nil {
		return nil, fmt.Errorf("find_runbook: search: %w", err)
	}

	// Scrub excerpts on the way out for consistency with the inbound
	// redaction posture.
	out := make([]RunbookMatch, 0, len(matches))
	for _, m := range matches {
		m.Excerpt = fr.scrub(m.Excerpt)
		out = append(out, m)
	}

	return &core.ToolResult{
		Tool:  FindRunbook{}.Name(),
		Found: len(out) > 0,
		Data: map[string]any{
			"count":   len(out),
			"service": a.Service,
			"matches": out,
		},
	}, nil
}

func (fr FindRunbook) scrub(s string) string {
	if fr.Redactor == nil {
		return s
	}
	return fr.Redactor.Scrub(s)
}
