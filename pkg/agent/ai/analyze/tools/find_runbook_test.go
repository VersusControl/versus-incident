package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// recordingEmbedder is a core.Embedder that records the exact texts it
// was asked to embed (so a test can assert redaction happened BEFORE the
// embeddings egress) and returns a fixed vector per input.
type recordingEmbedder struct {
	seen []string
	vec  []float32
}

func (e *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.seen = append(e.seen, texts...)
	out := make([][]float32, len(texts))
	v := e.vec
	if len(v) == 0 {
		v = []float32{1, 0, 0}
	}
	for i := range texts {
		out[i] = v
	}
	return out, nil
}

// fakeSearcher is a scripted RunbookSearcher. It records the query
// vector / service / limit it received and returns canned matches.
type fakeSearcher struct {
	gotVec     []float32
	gotService string
	gotLimit   int
	matches    []RunbookMatch
}

func (s *fakeSearcher) Search(_ context.Context, query []float32, service string, limit int) ([]RunbookMatch, error) {
	s.gotVec = query
	s.gotService = service
	s.gotLimit = limit
	return s.matches, nil
}

func TestFindRunbook_RedactsQueryBeforeEmbedding(t *testing.T) {
	emb := &recordingEmbedder{}
	idx := &fakeSearcher{matches: []RunbookMatch{
		{ID: "r1", Title: "Pool exhaustion", Service: "api", Score: 0.9, Excerpt: "do the thing", Source: "r1.md"},
	}}
	tool := FindRunbook{Embedder: emb, Index: idx, Redactor: fakeRedactor{}}

	res, err := tool.Invoke(context.Background(), mustArgs(t, findRunbookArgs{
		Query:   "login password=hunter2 then pool exhausted",
		Service: "api",
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if len(emb.seen) != 1 {
		t.Fatalf("embedder saw %d texts, want 1", len(emb.seen))
	}
	if strings.Contains(emb.seen[0], "hunter2") {
		t.Errorf("raw secret reached embedder: %q", emb.seen[0])
	}
	if !strings.Contains(emb.seen[0], "[redacted]") {
		t.Errorf("query not redacted before embedding: %q", emb.seen[0])
	}
	if idx.gotService != "api" {
		t.Errorf("service = %q, want api", idx.gotService)
	}
	if !res.Found {
		t.Error("Found = false, want true")
	}
	if res.Tool != "find_runbook" {
		t.Errorf("Tool = %q, want find_runbook", res.Tool)
	}
	if got := res.Data["count"]; got != 1 {
		t.Errorf("count = %v, want 1", got)
	}
}

func TestFindRunbook_RedactsExcerptsOnOutput(t *testing.T) {
	emb := &recordingEmbedder{}
	idx := &fakeSearcher{matches: []RunbookMatch{
		{ID: "r1", Title: "t", Excerpt: "creds password=hunter2 inside"},
	}}
	tool := FindRunbook{Embedder: emb, Index: idx, Redactor: fakeRedactor{}}

	res, err := tool.Invoke(context.Background(), mustArgs(t, findRunbookArgs{Query: "q"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	matches := res.Data["matches"].([]RunbookMatch)
	if len(matches) != 1 {
		t.Fatalf("len(matches) = %d, want 1", len(matches))
	}
	if strings.Contains(matches[0].Excerpt, "hunter2") {
		t.Errorf("secret leaked in excerpt: %q", matches[0].Excerpt)
	}
}

func TestFindRunbook_EmptyQueryErrors(t *testing.T) {
	tool := FindRunbook{Embedder: &recordingEmbedder{}, Index: &fakeSearcher{}}
	if _, err := tool.Invoke(context.Background(), mustArgs(t, findRunbookArgs{Query: "   "})); err == nil {
		t.Fatal("expected error for empty query, got nil")
	}
}

func TestFindRunbook_LimitClamp(t *testing.T) {
	emb := &recordingEmbedder{}
	idx := &fakeSearcher{}
	tool := FindRunbook{Embedder: emb, Index: idx}

	if _, err := tool.Invoke(context.Background(), mustArgs(t, findRunbookArgs{Query: "q", Limit: 9999})); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if idx.gotLimit != findRunbookMaxLimit {
		t.Errorf("limit = %d, want clamp to %d", idx.gotLimit, findRunbookMaxLimit)
	}

	if _, err := tool.Invoke(context.Background(), mustArgs(t, findRunbookArgs{Query: "q", Limit: 0})); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if idx.gotLimit != findRunbookDefaultLimit {
		t.Errorf("limit = %d, want default %d", idx.gotLimit, findRunbookDefaultLimit)
	}
}

func TestFindRunbook_NoMatchesFoundFalse(t *testing.T) {
	tool := FindRunbook{Embedder: &recordingEmbedder{}, Index: &fakeSearcher{matches: nil}}
	res, err := tool.Invoke(context.Background(), mustArgs(t, findRunbookArgs{Query: "q"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Found {
		t.Error("Found = true, want false for empty corpus")
	}
	if got := res.Data["count"]; got != 0 {
		t.Errorf("count = %v, want 0", got)
	}
}

func TestFindRunbook_RequiresDeps(t *testing.T) {
	tool := FindRunbook{} // no embedder/index
	if _, err := tool.Invoke(context.Background(), mustArgs(t, findRunbookArgs{Query: "q"})); err == nil {
		t.Fatal("expected error when embedder/index missing, got nil")
	}
}

func TestFindRunbook_Schema(t *testing.T) {
	var _ core.AnalyzeTool = FindRunbook{Embedder: &recordingEmbedder{}, Index: &fakeSearcher{}}
	schema := FindRunbook{}.ArgsSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	for _, k := range []string{"query", "service", "limit"} {
		if _, ok := props[k]; !ok {
			t.Errorf("schema missing property %q", k)
		}
	}
}
