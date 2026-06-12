package eino_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
