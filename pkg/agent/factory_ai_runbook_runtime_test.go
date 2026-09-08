package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/runbook"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type runbookRuntimeOrgKey struct{}

type runbookRuntimeResolver struct {
	mu       sync.RWMutex
	key      string
	provider string
}

func (resolver *runbookRuntimeResolver) EffectiveKey(ctx context.Context) (string, bool) {
	if _, ok := ctx.Value(runbookRuntimeOrgKey{}).(string); !ok {
		return "", false
	}
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	return resolver.key, true
}

func (*runbookRuntimeResolver) EffectiveEnabled(context.Context) (bool, bool) { return true, true }

func (resolver *runbookRuntimeResolver) EffectiveProvider(context.Context) (string, bool) {
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	return resolver.provider, true
}

func (resolver *runbookRuntimeResolver) EffectiveKeySet(context.Context) (bool, bool) {
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	return resolver.key != "", true
}

func (*runbookRuntimeResolver) DecorateAIContext(ctx context.Context, scope tenancy.OrgScope) context.Context {
	return context.WithValue(ctx, runbookRuntimeOrgKey{}, scope.Normalized().Write)
}

func (resolver *runbookRuntimeResolver) set(provider, key string) {
	resolver.mu.Lock()
	resolver.provider = provider
	resolver.key = key
	resolver.mu.Unlock()
}

type runbookRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (transport runbookRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clonedURL := *request.URL
	clonedURL.Scheme = transport.target.Scheme
	clonedURL.Host = transport.target.Host
	clone.URL = &clonedURL
	return transport.base.RoundTrip(clone)
}

func TestRunbookEmbeddingUsesScopedRuntimeSettings(t *testing.T) {
	const firstKey = "runbook-runtime-first"
	const rotatedKey = "runbook-runtime-rotated"
	var captureMu sync.Mutex
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var payload struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		captureMu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		captureMu.Unlock()
		data := make([]map[string]any, len(payload.Input))
		for index := range payload.Input {
			data[index] = map[string]any{"object": "embedding", "index": index, "embedding": []float64{1, 0, 0}}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": data, "model": payload.Model})
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })
	resolver := &runbookRuntimeResolver{provider: "openai", key: firstKey}
	SetAISettingsResolver(resolver)
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	catalog, store := newBuildCatalog(t)
	client := &http.Client{Transport: runbookRewriteTransport{target: target, base: http.DefaultTransport}}
	bundle := BuildAIsForScope(config.AgentConfig{
		AI:    config.AgentAIConfig{Enable: true, Provider: "openai", Model: "gpt-4o-mini"},
		Tools: config.ToolsConfig{FindRunbook: config.FindRunbookToolConfig{EmbeddingModel: "text-embedding-3-small"}},
	}, catalog, store, tenancy.NewOrgScope("org-a"), client)
	if bundle.Runbooks == nil || !bundle.Runbooks.HasEmbedder() {
		t.Fatal("runtime runbook embedder was not built")
	}
	ctx := DecorateAIContext(context.Background(), tenancy.NewOrgScope("org-a"))
	foreignUploadCtx := DecorateAIContext(context.Background(), tenancy.NewOrgScope("org-b"))
	if _, err := bundle.Runbooks.Upload(foreignUploadCtx, []runbook.UploadFile{{Name: "disk.md", Content: []byte("# Disk full\n\nFree space safely.")}}); err != nil {
		t.Fatal(err)
	}
	if records := bundle.Runbooks.List(); len(records) != 1 || records[0].OrgID != "org-a" {
		t.Fatalf("uploaded records = %+v, want manager boot org org-a", records)
	}
	if _, err := bundle.Runbooks.Embedder().Embed(ctx, []string{"disk full"}); err != nil {
		t.Fatal(err)
	}
	resolver.set("openai", rotatedKey)
	if _, err := bundle.Runbooks.Embedder().Embed(ctx, []string{"disk pressure"}); err != nil {
		t.Fatal(err)
	}
	resolver.set("claude", "claude-chat-only-secret")
	if _, err := bundle.Runbooks.Embedder().Embed(ctx, []string{"fallback query"}); err != nil {
		t.Fatal(err)
	}

	beforeForeign := len(authorizations)
	foreign := DecorateAIContext(context.Background(), tenancy.NewOrgScope("org-b"))
	if _, err := bundle.Runbooks.Embedder().Embed(foreign, []string{"must fail"}); err == nil {
		t.Fatal("foreign scope embedded a query")
	}
	captureMu.Lock()
	got := append([]string(nil), authorizations...)
	captureMu.Unlock()
	if len(got) != beforeForeign || len(got) != 4 {
		t.Fatalf("embedding calls = %q, want four and no foreign call", got)
	}
	want := []string{"Bearer " + firstKey, "Bearer " + firstKey, "Bearer " + rotatedKey, ""}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("Authorization call %d = %q, want %q", index+1, got[index], want[index])
		}
	}

	corpus, _ := json.Marshal(bundle.Runbooks.List())
	for _, secret := range []string{firstKey, rotatedKey, "claude-chat-only-secret"} {
		if strings.Contains(string(corpus), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("runtime embedding key leaked outside transport: %q", secret)
		}
	}
}

func TestRunbookBootIngestUsesRuntimeCredentialFromRealDirectory(t *testing.T) {
	const runtimeKey = "runbook-boot-runtime-secret"
	var seenHeader string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenHeader = request.Header.Get("Authorization")
		var payload struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		data := make([]map[string]any, len(payload.Input))
		for index := range payload.Input {
			data[index] = map[string]any{"object": "embedding", "index": index, "embedding": []float64{1, 0, 0}}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": data})
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.md"), []byte("# Disk full\n\nFree space safely."), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := &runbookRuntimeResolver{provider: "openai", key: runtimeKey}
	SetAISettingsResolver(resolver)
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	runtime := einowrap.RuntimeAI{Provider: resolver.EffectiveProvider, KeySet: resolver.EffectiveKeySet}
	client := &http.Client{Transport: runbookRewriteTransport{target: target, base: http.DefaultTransport}}
	manager := buildRunbookManagerFromDir(config.AgentConfig{
		AI:    config.AgentAIConfig{Provider: "openai", Model: "gpt-4o-mini"},
		Tools: config.ToolsConfig{FindRunbook: config.FindRunbookToolConfig{EmbeddingModel: "text-embedding-3-small"}},
	}, storage.NewMemory(), tenancy.NewOrgScope("org-a"), client, resolver.EffectiveKey, runtime, dir)
	if manager == nil {
		t.Fatal("boot ingest manager is nil")
	}
	if got := len(manager.List()); got != 1 {
		t.Fatalf("boot ingest runbooks = %d, want one", got)
	}
	if seenHeader != "Bearer "+runtimeKey {
		t.Fatalf("boot ingest Authorization = %q, want runtime credential", seenHeader)
	}
}

func TestRuntimeEmbedderErrorReportsResolvedProviderSafely(t *testing.T) {
	const runtimeKey = "runbook-reflected-runtime-secret"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":{"message":"reflected ` + runtimeKey + `\r\nforged=true"}}`))
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &runbookRuntimeResolver{provider: "gemini", key: runtimeKey}
	SetAISettingsResolver(resolver)
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	runtime := einowrap.RuntimeAI{Provider: resolver.EffectiveProvider, KeySet: resolver.EffectiveKeySet}
	client := &http.Client{Transport: runbookRewriteTransport{target: target, base: http.DefaultTransport}}
	manager := buildRunbookManagerFromDir(config.AgentConfig{
		AI:    config.AgentAIConfig{Provider: "gemini", Model: "unused"},
		Tools: config.ToolsConfig{FindRunbook: config.FindRunbookToolConfig{EmbeddingModel: "unsafe\r\n" + runtimeKey}},
	}, storage.NewMemory(), tenancy.NewOrgScope("org-a"), client, resolver.EffectiveKey, runtime, t.TempDir())
	ctx := DecorateAIContext(context.Background(), tenancy.NewOrgScope("org-a"))
	_, err = manager.Embedder().Embed(ctx, []string{"disk full"})
	var providerErr *einowrap.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("embed error = %T %v, want ProviderError", err, err)
	}
	if providerErr.Provider != "gemini" || providerErr.Model != "configured model" {
		t.Fatalf("provider error identifiers = %+v", providerErr)
	}
	if strings.Contains(providerErr.Diagnostic, runtimeKey) || strings.Contains(providerErr.Diagnostic, "forged=true") {
		t.Fatalf("provider error leaked reflected runtime key: %+v", providerErr)
	}
}

func TestRunbookBootLogsActionableUnsupportedEmbedderProvider(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	manager := buildRunbookManagerFromDir(config.AgentConfig{
		AI: config.AgentAIConfig{Provider: "claude", Model: "chat-model"},
		Tools: config.ToolsConfig{FindRunbook: config.FindRunbookToolConfig{
			EmbeddingModel: "embedding-model",
		}},
	}, storage.NewMemory(), tenancy.DefaultOrgScope(), nil, nil, einowrap.RuntimeAI{}, t.TempDir())
	if manager == nil || manager.HasEmbedder() {
		t.Fatalf("runbook manager/embedder = %v/%v, want manager without embedder", manager != nil, manager != nil && manager.HasEmbedder())
	}

	got := logs.String()
	for _, want := range []string{
		"find_runbook disabled: embedder configuration error",
		`unsupported ai provider "claude" for embeddings`,
		"supported: gemini, ollama, openai",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("runbook boot log %q missing %q", got, want)
		}
	}
}
