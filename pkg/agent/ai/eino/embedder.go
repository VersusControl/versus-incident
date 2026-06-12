package eino

import (
	"context"
	"fmt"
	"net/http"
	"time"

	einoembedding "github.com/cloudwego/eino-ext/components/embedding/openai"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// NewEmbedder builds a core.Embedder backed by the same Eino/OpenAI
// model seam as NewChatModel — there is no second framework import and
// no fork of the model path. cfg supplies the model id and API key; the
// embeddings endpoint is OpenAI-compatible, so pointing opts.BaseURL at
// a local server (Ollama / vLLM / LocalAI) keeps embeddings fully in
// the operator's VPC with no code change. A "" BaseURL uses the OpenAI
// default (https://api.openai.com/v1).
//
// cfg.Model must be set (it is the embedding model id, e.g.
// "text-embedding-3-small"); APIKey may be empty (a local server that
// does not authenticate accepts it, and a remote one errors at call
// time, not construction time — convenient for tests).
func NewEmbedder(ctx context.Context, cfg config.AgentAIConfig, opts Options) (core.Embedder, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("eino: embedding model is empty")
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	emb, err := einoembedding.NewEmbedder(ctx, &einoembedding.EmbeddingConfig{
		APIKey:     cfg.APIKey,
		BaseURL:    opts.BaseURL,
		Model:      cfg.Model,
		HTTPClient: httpClient,
		Timeout:    timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("eino: build embedder: %w", err)
	}

	return &embedder{impl: emb}, nil
}

// embedder adapts the Eino OpenAI embedding component onto the
// leaf-level core.Embedder contract, narrowing the [][]float64 the
// component returns to the [][]float32 the vector index stores (half
// the memory; cosine similarity is unaffected at this precision).
type embedder struct {
	impl *einoembedding.Embedder
}

// Embed implements core.Embedder.
func (e *embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out, err := e.impl.EmbedStrings(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("eino: embed %d text(s): %w", len(texts), err)
	}
	vecs := make([][]float32, len(out))
	for i, v := range out {
		f := make([]float32, len(v))
		for j, x := range v {
			f[j] = float32(x)
		}
		vecs[i] = f
	}
	return vecs, nil
}
