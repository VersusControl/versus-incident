package eino_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// This file is the per-provider proving suite for the four chat backends that
// ship in the registry alongside openai/gemini — deepseek, qwen, ollama and
// claude. It mirrors the egress-capture pattern of
// chatmodel_test.go / gemini_test.go: each provider either round-trips a
// finding through an httptest server (asserting the outbound auth header and
// JSON-mode knob) or, where the SDK cannot be hit via httptest, asserts the
// registry resolves and builds it (the strongest unit test the SDK allows).

// TestChatModel_RegistryBuildsAllProviders proves the registry resolves and
// constructs every non-openai/gemini provider via NewChatModel — including
// provider-casing normalisation — with no network at build time. This is the
// "provider registered + builds via registry" half of A.3 for the four
// backends flagged.
func TestChatModel_RegistryBuildsAllProviders(t *testing.T) {
	cases := []struct {
		provider string
		model    string
	}{
		{"deepseek", "deepseek-chat"},
		{"DeepSeek", "deepseek-chat"},
		{" deepseek ", "deepseek-chat"},
		{"qwen", "qwen-plus"},
		{"QWEN", "qwen-plus"},
		{"ollama", "llama3"},
		{"Ollama", "llama3"},
		{"claude", "claude-3-5-sonnet-20241022"},
		{"CLAUDE", "claude-3-5-sonnet-20241022"},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSpace(tc.provider), func(t *testing.T) {
			cfg := config.AgentAIConfig{Provider: tc.provider, APIKey: "k", Model: tc.model, MaxTokens: 64}
			cm, err := einowrap.NewChatModel(context.Background(), cfg, einowrap.Options{})
			if err != nil {
				t.Fatalf("NewChatModel(provider=%q): %v", tc.provider, err)
			}
			if cm == nil {
				t.Fatalf("NewChatModel(provider=%q): nil model", tc.provider)
			}
			// The tool-calling variant must resolve via the same registry.
			tcm, err := einowrap.NewToolCallingChatModel(context.Background(), cfg, einowrap.Options{})
			if err != nil {
				t.Fatalf("NewToolCallingChatModel(provider=%q): %v", tc.provider, err)
			}
			if tcm == nil {
				t.Fatalf("NewToolCallingChatModel(provider=%q): nil model", tc.provider)
			}
		})
	}
}

// TestSupportedProvidersExported proves the boundary validator the enterprise
// runtime AI-settings API gates writes on agrees with the construction-time
// registry: every supported name builds, and IsSupportedProvider mirrors it
// (case-insensitive, empty ⇒ the openai default; unknown ⇒ false).
func TestSupportedProvidersExported(t *testing.T) {
	got := einowrap.SupportedProviders()
	want := map[string]bool{"openai": true, "deepseek": true, "qwen": true, "ollama": true, "claude": true, "gemini": true}
	if len(got) != len(want) {
		t.Fatalf("SupportedProviders() = %v, want the %d registered providers", got, len(want))
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("SupportedProviders() returned unexpected provider %q", name)
		}
		if !einowrap.IsSupportedProvider(name) {
			t.Fatalf("IsSupportedProvider(%q) = false, want true", name)
		}
	}
	// Case-insensitive, and empty normalises to the openai default.
	if !einowrap.IsSupportedProvider("OpenAI") || !einowrap.IsSupportedProvider("  DeepSeek ") {
		t.Fatal("IsSupportedProvider should be case/space-insensitive")
	}
	if !einowrap.IsSupportedProvider("") {
		t.Fatal("IsSupportedProvider(\"\") = false, want true (empty => openai default)")
	}
	// An unknown value is rejected at the boundary.
	if einowrap.IsSupportedProvider("bogus-llm") {
		t.Fatal("IsSupportedProvider(\"bogus-llm\") = true, want false")
	}
}

// newOpenAICompatServer returns an httptest server that mimics an
// OpenAI-compatible /chat/completions endpoint (shared by the deepseek and
// qwen backends). It records the outbound Authorization header and the decoded
// request body of the first call, then replies with a JSON-mode finding.
func newOpenAICompatServer(t *testing.T, finding core.AIFinding, seenAuth *string, seenBody *map[string]any, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("decode request body: %v (raw: %s)", err, string(body))
		}
		mu.Lock()
		if *seenAuth == "" {
			*seenAuth = r.Header.Get("Authorization")
		}
		if *seenBody == nil {
			*seenBody = decoded
		}
		mu.Unlock()

		content, err := json.Marshal(finding)
		if err != nil {
			t.Fatalf("marshal finding: %v", err)
		}
		resp := map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "test-model",
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": string(content),
				},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// responseFormatType extracts the response_format.type knob from a decoded
// OpenAI-compatible request body, returning "" when absent.
func responseFormatType(body map[string]any) string {
	rf, ok := body["response_format"].(map[string]any)
	if !ok {
		return ""
	}
	typ, _ := rf["type"].(string)
	return typ
}

// TestChatModel_DeepSeek_EgressBearerAndJSONMode proves the DeepSeek path end
// to end: it is OpenAI-compatible, so the runtime key rides the Bearer
// Authorization header, the JSON-mode request carries
// response_format:{type:json_object}, and the reply round-trips through
// detect.ParseFinding. DeepSeek is the OpenAI-family analogue of the gemini
// egress test.
func TestChatModel_DeepSeek_EgressBearerAndJSONMode(t *testing.T) {
	expected := core.AIFinding{
		Title:    "Disk pressure on node",
		Summary:  "kubelet is evicting pods under disk pressure.",
		Severity: "high",
		Category: "kubernetes",
	}
	var (
		mu       sync.Mutex
		seenAuth string
		seenBody map[string]any
	)
	srv := newOpenAICompatServer(t, expected, &seenAuth, &seenBody, &mu)
	defer srv.Close()

	cfg := config.AgentAIConfig{
		Provider:    "deepseek",
		APIKey:      "deepseek-key",
		Model:       "deepseek-chat",
		Temperature: 0.2,
		MaxTokens:   256,
	}
	ctx := context.Background()
	cm, err := einowrap.NewChatModel(ctx, cfg, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewChatModel(deepseek): %v", err)
	}
	msg, err := cm.Generate(ctx, []*schema.Message{schema.SystemMessage("system"), schema.UserMessage("user")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if seenAuth != "Bearer deepseek-key" {
		t.Errorf("Authorization = %q, want Bearer deepseek-key", seenAuth)
	}
	if got := responseFormatType(seenBody); got != "json_object" {
		t.Errorf("response_format.type = %q, want json_object (deepseek structured JSON-mode)", got)
	}
	got, err := detect.ParseFinding(msg.Content)
	if err != nil {
		t.Fatalf("ParseFinding: %v\nraw: %s", err, msg.Content)
	}
	if got.Title != expected.Title || got.Severity != expected.Severity {
		t.Errorf("finding = %+v, want title=%q severity=%q", got, expected.Title, expected.Severity)
	}
}

// TestChatModel_Qwen_EgressBearerAndJSONMode is the Qwen analogue: Qwen runs in
// DashScope's OpenAI-compatible mode, so it authenticates with a Bearer token
// and accepts response_format:{type:json_object}. The test also proves
// opts.BaseURL overrides the SDK's required DashScope default endpoint.
func TestChatModel_Qwen_EgressBearerAndJSONMode(t *testing.T) {
	expected := core.AIFinding{
		Title:    "Latency spike on checkout",
		Summary:  "p99 latency tripled after the last deploy.",
		Severity: "medium",
		Category: "latency",
	}
	var (
		mu       sync.Mutex
		seenAuth string
		seenBody map[string]any
	)
	srv := newOpenAICompatServer(t, expected, &seenAuth, &seenBody, &mu)
	defer srv.Close()

	cfg := config.AgentAIConfig{
		Provider:    "qwen",
		APIKey:      "qwen-key",
		Model:       "qwen-plus",
		Temperature: 0.2,
		MaxTokens:   256,
	}
	ctx := context.Background()
	cm, err := einowrap.NewChatModel(ctx, cfg, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewChatModel(qwen): %v", err)
	}
	msg, err := cm.Generate(ctx, []*schema.Message{schema.SystemMessage("system"), schema.UserMessage("user")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if seenAuth != "Bearer qwen-key" {
		t.Errorf("Authorization = %q, want Bearer qwen-key", seenAuth)
	}
	if got := responseFormatType(seenBody); got != "json_object" {
		t.Errorf("response_format.type = %q, want json_object (qwen structured JSON-mode)", got)
	}
	got, err := detect.ParseFinding(msg.Content)
	if err != nil {
		t.Fatalf("ParseFinding: %v\nraw: %s", err, msg.Content)
	}
	if got.Title != expected.Title {
		t.Errorf("Title = %q, want %q", got.Title, expected.Title)
	}
}

// TestChatModel_Ollama_KeylessAndNativeFormat proves the Ollama path: Ollama is
// keyless, so NO Authorization header is sent (the AuthKeyFunc transport is a
// harmless no-op with no resolver), and JSON-mode is requested via the native
// `format` field rather than an OpenAI response_format object. The reply
// round-trips through detect.ParseFinding.
func TestChatModel_Ollama_KeylessAndNativeFormat(t *testing.T) {
	expected := core.AIFinding{
		Title:    "OOMKilled container",
		Summary:  "the worker container exceeded its memory limit.",
		Severity: "high",
		Category: "memory",
	}
	var (
		mu        sync.Mutex
		seenAuth  string
		seenPath  string
		seenFmt   json.RawMessage
		called    bool
		seenAuthY bool // whether an Authorization header was present at all
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]json.RawMessage
		_ = json.Unmarshal(body, &decoded)
		mu.Lock()
		called = true
		seenPath = r.URL.Path
		if _, ok := r.Header["Authorization"]; ok {
			seenAuthY = true
			seenAuth = r.Header.Get("Authorization")
		}
		seenFmt = decoded["format"]
		mu.Unlock()

		content, err := json.Marshal(expected)
		if err != nil {
			t.Fatalf("marshal finding: %v", err)
		}
		resp := map[string]any{
			"model":       "llama3",
			"created_at":  time.Now().Format(time.RFC3339),
			"message":     map[string]any{"role": "assistant", "content": string(content)},
			"done":        true,
			"done_reason": "stop",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := config.AgentAIConfig{
		Provider:    "ollama",
		Model:       "llama3",
		Temperature: 0.2,
		MaxTokens:   256,
	}
	ctx := context.Background()
	cm, err := einowrap.NewChatModel(ctx, cfg, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewChatModel(ollama): %v", err)
	}
	msg, err := cm.Generate(ctx, []*schema.Message{schema.SystemMessage("system"), schema.UserMessage("user")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatalf("ollama backend was never called")
	}
	if !strings.Contains(seenPath, "/api/chat") {
		t.Errorf("request path = %q, want it to target /api/chat", seenPath)
	}
	if seenAuthY {
		t.Errorf("unexpected Authorization header for keyless ollama: %q", seenAuth)
	}
	if string(seenFmt) != `"json"` {
		t.Errorf("native format = %s, want \"json\" (ollama JSON-mode)", string(seenFmt))
	}
	got, err := detect.ParseFinding(msg.Content)
	if err != nil {
		t.Fatalf("ParseFinding: %v\nraw: %s", err, msg.Content)
	}
	if got.Title != expected.Title {
		t.Errorf("Title = %q, want %q", got.Title, expected.Title)
	}
}

// TestChatModel_Claude_EgressUsesAPIKeyHeader proves the Claude path: Anthropic
// authenticates with the x-api-key header, NOT a Bearer token, so — exactly
// like the gemini x-goog-api-key analogue — the Bearer Authorization override
// does not apply. Anthropic has no response_format knob, so JSON-mode is
// advisory only; detect.ParseFinding tolerates the fenced/plain JSON Claude
// returns, which the test confirms round-trips.
func TestChatModel_Claude_EgressUsesAPIKeyHeader(t *testing.T) {
	expected := core.AIFinding{
		Title:    "Certificate expiry imminent",
		Summary:  "the ingress TLS cert expires in under 24h.",
		Severity: "high",
		Category: "tls",
	}
	var (
		mu       sync.Mutex
		seenKey  string
		seenBear string
		seenPath string
		called   bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenKey = r.Header.Get("x-api-key")
		seenBear = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		called = true
		mu.Unlock()

		content, err := json.Marshal(expected)
		if err != nil {
			t.Fatalf("marshal finding: %v", err)
		}
		resp := map[string]any{
			"id":            "msg_test",
			"type":          "message",
			"role":          "assistant",
			"model":         "claude-3-5-sonnet-20241022",
			"content":       []map[string]any{{"type": "text", "text": string(content)}},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 10, "output_tokens": 20},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := config.AgentAIConfig{
		Provider:    "claude",
		APIKey:      "claude-test-key",
		Model:       "claude-3-5-sonnet-20241022",
		Temperature: 0.2,
		MaxTokens:   256,
	}
	ctx := context.Background()
	cm, err := einowrap.NewChatModel(ctx, cfg, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewChatModel(claude): %v", err)
	}
	msg, err := cm.Generate(ctx, []*schema.Message{schema.SystemMessage("system"), schema.UserMessage("user")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatalf("claude backend was never called")
	}
	if seenKey != "claude-test-key" {
		t.Errorf("x-api-key = %q, want claude-test-key", seenKey)
	}
	// Claude does not use a Bearer token, so no Authorization header is set.
	if seenBear != "" {
		t.Errorf("unexpected Authorization header for claude: %q", seenBear)
	}
	if !strings.Contains(seenPath, "/messages") {
		t.Errorf("request path = %q, want it to target /v1/messages", seenPath)
	}
	got, err := detect.ParseFinding(msg.Content)
	if err != nil {
		t.Fatalf("ParseFinding: %v\nraw: %s", err, msg.Content)
	}
	if got.Title != expected.Title {
		t.Errorf("Title = %q, want %q", got.Title, expected.Title)
	}
}
