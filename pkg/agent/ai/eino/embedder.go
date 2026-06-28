package eino

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	einogeminiemb "github.com/cloudwego/eino-ext/components/embedding/gemini"
	einoollamaemb "github.com/cloudwego/eino-ext/components/embedding/ollama"
	einoopenaiemb "github.com/cloudwego/eino-ext/components/embedding/openai"
	"github.com/cloudwego/eino/components/embedding"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// embedderRequest is the normalized input each embedder builder receives. As
// with the chat path, HTTPClient is already auth-wrapped so a Bearer-keyed
// provider honours the runtime key override.
type embedderRequest struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
	timeout    time.Duration
}

// embedderBuilder constructs a provider-specific eino embedding client behind
// the framework-neutral embedding.Embedder interface.
type embedderBuilder func(ctx context.Context, req embedderRequest) (embedding.Embedder, error)

// embedderBuilders is the embedding-provider registry. It is intentionally a
// subset of the chat registry: only providers that actually expose an eino-ext
// embedding component are wired. Providers without embeddings (e.g. deepseek,
// claude) deliberately have no entry and fail fast when RAG asks for an
// embedder — there is NO silent fallback to openai. Gemini IS wired here because
// eino-ext ships a Gemini embedding component (gemini-embedding-001 /
// text-embedding-004); it authenticates with the api key via x-goog-api-key, not
// a Bearer token, so the runtime override does not apply (see buildGeminiEmbedder).
var embedderBuilders = map[string]embedderBuilder{
	"openai": buildOpenAIEmbedder,
	"ollama": buildOllamaEmbedder,
	"gemini": buildGeminiEmbedder,
}

func supportedEmbedderProviders() []string {
	names := make([]string, 0, len(embedderBuilders))
	for k := range embedderBuilders {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// NewEmbedder builds a core.Embedder for the configured provider. cfg.Provider
// selects the backend (empty defaults to openai); an unsupported provider fails
// fast with a clear error. cfg.Model must be set (the embedding model id, e.g.
// "text-embedding-3-small"); APIKey may be empty (a local/unauthenticated
// server accepts it, a remote one errors at call time — convenient for tests).
//
// The openai path is OpenAI-compatible, so pointing opts.BaseURL at a local
// server (Ollama / vLLM / LocalAI) keeps embeddings in the operator's VPC; the
// dedicated ollama provider does the same against a native Ollama daemon.
func NewEmbedder(ctx context.Context, cfg config.AgentAIConfig, opts Options) (core.Embedder, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("eino: embedding model is empty")
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	name := resolveProvider(cfg.Provider)
	build, ok := embedderBuilders[name]
	if !ok {
		return nil, fmt.Errorf("eino: unsupported ai provider %q for embeddings (supported: %s)", name, strings.Join(supportedEmbedderProviders(), ", "))
	}

	emb, err := build(ctx, embedderRequest{
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		baseURL:    opts.BaseURL,
		httpClient: withAuthRoundTripper(opts.HTTPClient, timeout, opts.AuthKeyFunc),
		timeout:    timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("eino: build embedder: %w", err)
	}

	return &embedder{impl: emb}, nil
}

// buildOpenAIEmbedder is the default path — the historical OpenAI embedding
// wiring, unchanged.
func buildOpenAIEmbedder(ctx context.Context, req embedderRequest) (embedding.Embedder, error) {
	httpClient := req.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: req.timeout}
	}
	return einoopenaiemb.NewEmbedder(ctx, &einoopenaiemb.EmbeddingConfig{
		APIKey:     req.apiKey,
		BaseURL:    req.baseURL,
		Model:      req.model,
		HTTPClient: httpClient,
		Timeout:    req.timeout,
	})
}

// buildOllamaEmbedder wires a local Ollama embedding model. Ollama is keyless;
// BaseURL defaults to the local daemon when unset.
func buildOllamaEmbedder(ctx context.Context, req embedderRequest) (embedding.Embedder, error) {
	baseURL := req.baseURL
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return einoollamaemb.NewEmbedder(ctx, &einoollamaemb.EmbeddingConfig{
		BaseURL:    baseURL,
		Model:      req.model,
		HTTPClient: req.httpClient,
		Timeout:    req.timeout,
	})
}

// buildGeminiEmbedder wires Google Gemini embeddings (e.g. gemini-embedding-001
// or text-embedding-004) via the genai client. Like the Gemini chat path, the
// api key is sent through the x-goog-api-key header rather than a Bearer token,
// so the AuthKeyFunc override on req.httpClient does not apply; req.apiKey is
// passed straight to the client. req.baseURL is the test-only endpoint override.
func buildGeminiEmbedder(ctx context.Context, req embedderRequest) (embedding.Embedder, error) {
	client, err := newGeminiClient(ctx, req.apiKey, req.baseURL, req.httpClient, req.timeout)
	if err != nil {
		return nil, err
	}
	return einogeminiemb.NewEmbedder(ctx, &einogeminiemb.EmbeddingConfig{
		Client: client,
		Model:  req.model,
	})
}

// embedder adapts an eino embedding.Embedder onto the leaf-level core.Embedder
// contract, narrowing the [][]float64 the component returns to the [][]float32
// the vector index stores (half the memory; cosine similarity is unaffected at
// this precision).
type embedder struct {
	impl embedding.Embedder
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
