package eino_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

func unsetGeminiAPIKeyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"} {
		value, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, value)
				return
			}
			_ = os.Unsetenv(name)
		})
	}
}

// TestChatModel_Gemini_EgressUsesAPIKeyHeader proves the Gemini provider path
// end to end against a canned Generative Language API backend: the request
// authenticates with the api key via the x-goog-api-key header (NOT a Bearer
// token), and the model's reply round-trips through detect.ParseFinding. This
// is the Gemini analogue of TestChatModel_ProducesParseableFinding; it also
// proves req.baseURL is honoured as the genai HTTPOptions.BaseURL seam.
func TestChatModel_Gemini_EgressUsesAPIKeyHeader(t *testing.T) {
	expected := core.AIFinding{
		Title:      "Cache eviction storm",
		Summary:    "The redis cache is shedding hot keys under load.",
		Severity:   "medium",
		Category:   "cache",
		Confidence: 0.71,
		Suggestions: []string{
			"Raise the maxmemory ceiling",
			"Review the eviction policy",
		},
	}

	var (
		mu        sync.Mutex
		seenKey   string
		seenBear  string
		gotPath   string
		callCount int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenKey = r.Header.Get("x-goog-api-key")
		seenBear = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		callCount++
		mu.Unlock()

		content, err := json.Marshal(expected)
		if err != nil {
			t.Fatalf("marshal finding: %v", err)
		}
		resp := map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"role":  "model",
					"parts": []map[string]any{{"text": string(content)}},
				},
				"finishReason": "STOP",
				"index":        0,
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount":     10,
				"candidatesTokenCount": 20,
				"totalTokenCount":      30,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := config.AgentAIConfig{
		Provider:    "gemini",
		APIKey:      "gemini-test-key",
		Model:       "gemini-1.5-flash",
		Temperature: 0.2,
		MaxTokens:   256,
	}

	ctx := context.Background()
	cm, err := einowrap.NewChatModel(ctx, cfg, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewChatModel(gemini): %v", err)
	}

	msg, err := cm.Generate(ctx, []*schema.Message{
		schema.SystemMessage("system"),
		schema.UserMessage("user"),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg == nil || msg.Content == "" {
		t.Fatalf("empty response")
	}

	mu.Lock()
	defer mu.Unlock()
	if callCount == 0 {
		t.Fatalf("backend was never called")
	}
	if seenKey != "gemini-test-key" {
		t.Errorf("x-goog-api-key = %q, want gemini-test-key", seenKey)
	}
	// Gemini does not use a Bearer token, so no Authorization header is set
	// from the api key (unlike the OpenAI-family providers).
	if seenBear != "" {
		t.Errorf("unexpected Authorization header for gemini: %q", seenBear)
	}
	if !strings.Contains(gotPath, ":generateContent") {
		t.Errorf("request path = %q, want it to target :generateContent", gotPath)
	}

	got, err := detect.ParseFinding(msg.Content)
	if err != nil {
		t.Fatalf("ParseFinding: %v\nraw: %s", err, msg.Content)
	}
	if got.Title != expected.Title {
		t.Errorf("Title = %q, want %q", got.Title, expected.Title)
	}
	if got.Severity != expected.Severity {
		t.Errorf("Severity = %q, want %q", got.Severity, expected.Severity)
	}
	if len(got.Suggestions) != 2 {
		t.Fatalf("Suggestions len = %d, want 2", len(got.Suggestions))
	}
}

// TestChatModel_Gemini_BuildsViaRegistry asserts the registry resolves the
// "gemini" provider and constructs a model without error (no network at build
// time). Provider casing is normalised, mirroring the other backends.
func TestChatModel_Gemini_BuildsViaRegistry(t *testing.T) {
	for _, prov := range []string{"gemini", "GEMINI", " Gemini "} {
		cfg := config.AgentAIConfig{Provider: prov, APIKey: "k", Model: "gemini-1.5-flash"}
		cm, err := einowrap.NewChatModel(context.Background(), cfg, einowrap.Options{})
		if err != nil {
			t.Fatalf("NewChatModel(provider=%q): %v", prov, err)
		}
		if cm == nil {
			t.Fatalf("NewChatModel(provider=%q): nil model", prov)
		}
	}
}

// TestNewChatModel_UnknownProvider_FailFastListsGemini proves the no-fallback
// contract: an unsupported provider fails fast, and the error enumerates the
// supported providers — including the newly registered gemini.
func TestNewChatModel_UnknownProvider_FailFastListsGemini(t *testing.T) {
	cfg := config.AgentAIConfig{Provider: "no-such-llm", APIKey: "k", Model: "m"}
	_, err := einowrap.NewChatModel(context.Background(), cfg, einowrap.Options{})
	if err == nil {
		t.Fatalf("expected fail-fast error for unknown provider")
	}
	if !strings.Contains(err.Error(), "gemini") {
		t.Errorf("error %q does not list gemini in the supported providers", err.Error())
	}
}

// TestEmbedder_Gemini_EgressUsesAPIKeyHeader proves the Gemini embedding path:
// the genai client authenticates with x-goog-api-key, and the returned values
// are narrowed to []float32. Gemini IS wired into the embedder registry because
// eino-ext ships a Gemini embedding component.
func TestEmbedder_Gemini_EgressUsesAPIKeyHeader(t *testing.T) {
	var (
		mu      sync.Mutex
		seenKey string
		called  bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenKey = r.Header.Get("x-goog-api-key")
		called = true
		mu.Unlock()

		resp := map[string]any{
			"embeddings": []map[string]any{
				{"values": []float64{0.1, 0.2, 0.3}},
				{"values": []float64{1.1, 1.2, 1.3}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ctx := context.Background()
	emb, err := einowrap.NewEmbedder(ctx, config.AgentAIConfig{
		Provider: "gemini",
		APIKey:   "gemini-emb-key",
		Model:    "text-embedding-004",
	}, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewEmbedder(gemini): %v", err)
	}

	vecs, err := emb.Embed(ctx, []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatalf("embedding backend was never called")
	}
	if seenKey != "gemini-emb-key" {
		t.Errorf("x-goog-api-key = %q, want gemini-emb-key", seenKey)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	if len(vecs[0]) != 3 || vecs[1][0] != float32(1.1) {
		t.Errorf("vector narrowing wrong: %v", vecs)
	}
}

func TestGeminiRuntimeOnlyCredentialsRotateAndClearPerRequest(t *testing.T) {
	unsetGeminiAPIKeyEnv(t)
	var mu sync.Mutex
	var captures []capturedEgress
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		captures = append(captures, capturedEgress{header: request.Header.Clone(), url: request.URL.String(), body: string(body)})
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.URL.Path, "embedContent") {
			_ = json.NewEncoder(writer).Encode(map[string]any{"embeddings": []map[string]any{{"values": []float64{1, 0, 0}}}})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"candidates": []map[string]any{{"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": "ok"}}}, "finishReason": "STOP"}}})
	}))
	defer server.Close()

	key := "runtime-gemini-first"
	keyOK := true
	keyFn := func(context.Context) (string, bool) { return key, keyOK }
	chatModel, err := einowrap.NewChatModel(context.Background(), config.AgentAIConfig{
		Provider: "gemini", Model: "gemini-test", MaxTokens: 16,
	}, einowrap.Options{BaseURL: server.URL, RuntimeKeyFunc: keyFn})
	if err != nil {
		t.Fatalf("runtime-only Gemini chat construction: %v", err)
	}
	embedder, err := einowrap.NewEmbedder(context.Background(), config.AgentAIConfig{
		Provider: "gemini", Model: "gemini-embedding-test",
	}, einowrap.Options{BaseURL: server.URL, RuntimeKeyFunc: keyFn})
	if err != nil {
		t.Fatalf("runtime-only Gemini embedder construction: %v", err)
	}

	callChat := func() {
		if _, callErr := chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")}); callErr != nil {
			t.Fatalf("Gemini chat call: %v", callErr)
		}
	}
	callEmbed := func() {
		if _, callErr := embedder.Embed(context.Background(), []string{"hello"}); callErr != nil {
			t.Fatalf("Gemini embedding call: %v", callErr)
		}
	}
	callChat()
	callEmbed()
	key = "runtime-gemini-rotated"
	callChat()
	callEmbed()
	key = ""
	callChat()
	callEmbed()

	mu.Lock()
	defer mu.Unlock()
	if len(captures) != 6 {
		t.Fatalf("captured %d requests, want 6", len(captures))
	}
	for index, want := range []string{"runtime-gemini-first", "runtime-gemini-first", "runtime-gemini-rotated", "runtime-gemini-rotated", "", ""} {
		assertCredentialEgress(t, captures[index], "gemini", want)
	}
}

type geminiOrgContextKey struct{}

func TestGeminiCachedHolderDoesNotCarryCredentialAcrossOrgs(t *testing.T) {
	unsetGeminiAPIKeyEnv(t)
	var mu sync.Mutex
	var captures []capturedEgress
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		captures = append(captures, capturedEgress{header: request.Header.Clone(), url: request.URL.String(), body: string(body)})
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"candidates": []map[string]any{{"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": "ok"}}}, "finishReason": "STOP"}}})
	}))
	defer server.Close()

	runtimeKey := func(ctx context.Context) (string, bool) {
		if ctx.Value(geminiOrgContextKey{}) == "org-a" {
			return "org-a-runtime-secret", true
		}
		return "", false
	}
	holder := einowrap.NewChatModelHolder(config.AgentAIConfig{
		Provider: "gemini", APIKey: "yaml-fallback", Model: "gemini-test", MaxTokens: 16,
	}, einowrap.Options{BaseURL: server.URL, HTTPClient: server.Client(), RuntimeKeyFunc: runtimeKey}, einowrap.RuntimeAI{
		KeySet: func(context.Context) (bool, bool) { return true, true },
	})
	call := func(org string) {
		ctx := context.WithValue(context.Background(), geminiOrgContextKey{}, org)
		chatModel, err := holder.Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := chatModel.Generate(ctx, []*schema.Message{schema.UserMessage("hello")}); err != nil {
			t.Fatal(err)
		}
	}
	call("org-a")
	call("org-b")

	mu.Lock()
	defer mu.Unlock()
	if len(captures) != 2 {
		t.Fatalf("captured %d requests, want 2", len(captures))
	}
	assertCredentialEgress(t, captures[0], "gemini", "org-a-runtime-secret")
	assertCredentialEgress(t, captures[1], "gemini", "yaml-fallback")
	if got := captures[1].header.Get("x-goog-api-key"); got == "org-a-runtime-secret" {
		t.Fatal("org A runtime credential egressed for org B")
	}
}

func TestGeminiEmptyYAMLDoesNotUseAmbientCredentials(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "ambient-google-secret")
	t.Setenv("GEMINI_API_KEY", "ambient-gemini-secret")
	var mu sync.Mutex
	var captures []capturedEgress
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		captures = append(captures, capturedEgress{header: request.Header.Clone(), url: request.URL.String(), body: string(body)})
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.URL.Path, "embedContent") {
			_ = json.NewEncoder(writer).Encode(map[string]any{"embeddings": []map[string]any{{"values": []float64{1, 0, 0}}}})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"candidates": []map[string]any{{"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": "ok"}}}, "finishReason": "STOP"}}})
	}))
	defer server.Close()

	chatModel, err := einowrap.NewChatModel(context.Background(), config.AgentAIConfig{
		Provider: "gemini", Model: "gemini-test", MaxTokens: 16,
	}, einowrap.Options{BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")}); err != nil {
		t.Fatal(err)
	}
	embedder, err := einowrap.NewEmbedder(context.Background(), config.AgentAIConfig{
		Provider: "gemini", Model: "gemini-embedding-test",
	}, einowrap.Options{BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captures) != 2 {
		t.Fatalf("captured %d requests, want 2", len(captures))
	}
	for index, capture := range captures {
		assertCredentialEgress(t, capture, "gemini", "")
		for _, secret := range []string{"ambient-google-secret", "ambient-gemini-secret"} {
			if strings.Contains(capture.url, secret) || strings.Contains(capture.body, secret) {
				t.Fatalf("request %d leaked ambient credential outside headers", index+1)
			}
		}
	}
}

func TestGeminiRuntimeClearWithEmptyYAMLOnCleanEnvironment(t *testing.T) {
	unsetGeminiAPIKeyEnv(t)
	var mu sync.Mutex
	var captures []capturedEgress
	server := newCredentialCaptureServer(t, &captures, &mu)
	defer server.Close()
	clearKey := func(context.Context) (string, bool) { return "", true }

	chatModel, err := einowrap.NewChatModel(context.Background(), config.AgentAIConfig{
		Provider: "gemini", Model: "gemini-test", MaxTokens: 16,
	}, einowrap.Options{BaseURL: server.URL, HTTPClient: server.Client(), RuntimeKeyFunc: clearKey})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	embedder, err := einowrap.NewEmbedder(context.Background(), config.AgentAIConfig{
		Provider: "gemini", Model: "gemini-embedding-test",
	}, einowrap.Options{BaseURL: server.URL, HTTPClient: server.Client(), RuntimeKeyFunc: clearKey})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = embedder.Embed(context.Background(), []string{"hello"})

	mu.Lock()
	defer mu.Unlock()
	if len(captures) != 2 {
		t.Fatalf("captured %d requests, want 2", len(captures))
	}
	for _, capture := range captures {
		assertCredentialEgress(t, capture, "gemini", "")
	}
}
