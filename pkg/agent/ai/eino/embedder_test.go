package eino_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
)

// TestEmbedder_EmbedsViaOpenAICompatibleEndpoint asserts NewEmbedder
// builds a core.Embedder that POSTs to an OpenAI-compatible /embeddings
// endpoint (honouring opts.BaseURL so a local server keeps embeddings in
// the VPC) and narrows the returned vectors to []float32.
func TestEmbedder_EmbedsViaOpenAICompatibleEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		var req struct {
			Input any    `json:"input"`
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		inputs, ok := req.Input.([]any)
		if !ok {
			t.Fatalf("input is not an array: %T", req.Input)
		}
		data := make([]map[string]any, len(inputs))
		for i := range inputs {
			data[i] = map[string]any{
				"object":    "embedding",
				"index":     i,
				"embedding": []float64{float64(i) + 0.1, 0.2, 0.3},
			}
		}
		resp := map[string]any{
			"object": "list",
			"data":   data,
			"model":  req.Model,
			"usage":  map[string]any{"prompt_tokens": 3, "total_tokens": 3},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ctx := context.Background()
	emb, err := einowrap.NewEmbedder(ctx, config.AgentAIConfig{
		APIKey: "test-key",
		Model:  "text-embedding-3-small",
	}, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}

	vecs, err := emb.Embed(ctx, []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	if len(vecs[0]) != 3 || vecs[1][0] != float32(1.1) {
		t.Errorf("vector narrowing wrong: %v", vecs)
	}
}

func TestEmbedder_EmptyInput(t *testing.T) {
	emb, err := einowrap.NewEmbedder(context.Background(), config.AgentAIConfig{Model: "m"}, einowrap.Options{})
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}
	vecs, err := emb.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed empty: %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("len = %d, want 0", len(vecs))
	}
}

func TestEmbedder_RequiresModel(t *testing.T) {
	if _, err := einowrap.NewEmbedder(context.Background(), config.AgentAIConfig{}, einowrap.Options{}); err == nil {
		t.Fatal("expected error for empty model, got nil")
	}
}

// TestEmbedder_FailsFast_ForProvidersWithoutEmbedding locks in the deliberate
// no-fallback contract (QA-022): a provider that ships a chat model but NO
// embedding component (deepseek, claude) must fail fast when RAG asks for an
// embedder — with a clear error naming the supported embedder providers — and
// must NOT silently fall back to openai. This is the most security-relevant
// embedder behaviour: refusing to embed via the wrong backend rather than
// leaking text to openai when the operator chose deepseek/claude.
func TestEmbedder_FailsFast_ForProvidersWithoutEmbedding(t *testing.T) {
	for _, provider := range []string{"deepseek", "claude", "DeepSeek", "CLAUDE"} {
		t.Run(strings.TrimSpace(provider), func(t *testing.T) {
			_, err := einowrap.NewEmbedder(context.Background(), config.AgentAIConfig{
				Provider: provider,
				APIKey:   "k",
				Model:    "some-embedding-model",
			}, einowrap.Options{})
			if err == nil {
				t.Fatalf("NewEmbedder(provider=%q): expected fail-fast error, got nil", provider)
			}
			msg := err.Error()
			if !strings.Contains(msg, "unsupported ai provider") || !strings.Contains(msg, "for embeddings") {
				t.Errorf("error %q missing the fail-fast 'unsupported ai provider ... for embeddings' wording", msg)
			}
			// The error must enumerate the supported embedder providers so the
			// operator knows the valid choices — and must NOT imply a silent
			// openai fallback happened.
			for _, want := range []string{"openai", "ollama", "gemini"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not list supported embedder provider %q", msg, want)
				}
			}
		})
	}
}

// TestEmbedder_Ollama_BuildsViaRegistry exercises the ollama embedder build
// path (QA-022): the registry resolves the "ollama" provider, the keyless
// native Ollama embeddings endpoint is hit at /api/embed, and the returned
// vectors are narrowed to []float32 — mirroring the openai/gemini embedder
// egress tests. No Authorization header is sent (ollama is keyless).
func TestEmbedder_Ollama_BuildsViaRegistry(t *testing.T) {
	var (
		mu       sync.Mutex
		seenPath string
		seenAuth bool
		called   bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		called = true
		seenPath = r.URL.Path
		if _, ok := r.Header["Authorization"]; ok {
			seenAuth = true
		}
		mu.Unlock()

		// Native Ollama /api/embed response shape: parallel `embeddings`.
		resp := map[string]any{
			"model":      "nomic-embed-text",
			"embeddings": [][]float64{{0.1, 0.2, 0.3}, {1.1, 1.2, 1.3}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ctx := context.Background()
	emb, err := einowrap.NewEmbedder(ctx, config.AgentAIConfig{
		Provider: "ollama",
		Model:    "nomic-embed-text",
	}, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewEmbedder(ollama): %v", err)
	}

	vecs, err := emb.Embed(ctx, []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatalf("ollama embedding backend was never called")
	}
	if !strings.Contains(seenPath, "/api/embed") {
		t.Errorf("request path = %q, want it to target /api/embed", seenPath)
	}
	if seenAuth {
		t.Errorf("unexpected Authorization header for keyless ollama embedder")
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	if len(vecs[0]) != 3 || vecs[1][0] != float32(1.1) {
		t.Errorf("vector narrowing wrong: %v", vecs)
	}
}
