package core

import "context"

// Embedder is the leaf-level contract for turning text into vector
// embeddings. It is the single embeddings seam every RAG-bearing agent
// in the suite reuses (the runbook-RAG find_runbook tool is the first
// consumer). Like AnalyzeTool / ToolResult, it lives in pkg/core so the
// tools layer can depend on it without importing the concrete Eino
// wrapper (pkg/agent/ai/eino) that constructs it.
//
// Implementations are an external trust boundary: callers MUST scrub
// any incident-derived text through the redactor before passing it to
// Embed, exactly as the analyze tools scrub log lines before the
// chat-completion call. The runbook corpus that is embedded at
// ingestion time is operator-authored content (an accepted boundary,
// documented in the runbook-RAG docs).
type Embedder interface {
	// Embed returns one vector per input string, in the same order. An
	// empty input yields an empty (non-nil) slice and a nil error.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
