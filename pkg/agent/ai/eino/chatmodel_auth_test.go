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

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
)

// newAuthCaptureServer returns an httptest server that records the
// Authorization header of every chat/completions request and replies with
// a minimal, parseable chat completion.
func newAuthCaptureServer(t *testing.T, seen *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = append(*seen, r.Header.Get("Authorization"))
		mu.Unlock()

		resp := map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o-mini",
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": `{"title":"t","summary":"s","severity":"low"}`,
				},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestChatModel_AuthOverride_NoFunc_UsesYAMLKey proves the OSS path: with
// no RuntimeKeyFunc the outbound Authorization header is the YAML key, exactly
// as before the seam (byte-for-byte pass-through transport).
func TestChatModel_AuthOverride_NoFunc_UsesYAMLKey(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := newAuthCaptureServer(t, &seen, &mu)
	defer srv.Close()

	cfg := config.AgentAIConfig{APIKey: "yaml-key", Model: "gpt-4o-mini", MaxTokens: 16}
	cm, err := einowrap.NewChatModel(context.Background(), cfg, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewChatModel: %v", err)
	}
	if _, err := cm.Generate(context.Background(), []*schema.Message{schema.UserMessage("u")}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "Bearer yaml-key" {
		t.Fatalf("Authorization headers = %v, want [Bearer yaml-key]", seen)
	}
}

// TestChatModel_AuthOverride_FuncWins proves that when RuntimeKeyFunc returns
// ok the outbound Authorization header is the resolver key (it overrides
// the YAML-keyed header the SDK set), and that ok=false falls back to the
// YAML key. Both go through ONE model instance — no rebuild — proving the
// override is read live per request.
func TestChatModel_AuthOverride_FuncWins(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := newAuthCaptureServer(t, &seen, &mu)
	defer srv.Close()

	// The func answer is flipped between the two Generate calls to prove the
	// transport re-reads it every request without rebuilding the client.
	var giveKey bool
	keyFn := func(context.Context) (string, bool) {
		if giveKey {
			return "resolver-key", true
		}
		return "", false // no opinion -> YAML key stands
	}

	cfg := config.AgentAIConfig{APIKey: "yaml-key", Model: "gpt-4o-mini", MaxTokens: 16}
	cm, err := einowrap.NewChatModel(context.Background(), cfg, einowrap.Options{
		BaseURL:        srv.URL,
		RuntimeKeyFunc: keyFn,
	})
	if err != nil {
		t.Fatalf("NewChatModel: %v", err)
	}

	// 1st call: func has no opinion -> YAML key.
	giveKey = false
	if _, err := cm.Generate(context.Background(), []*schema.Message{schema.UserMessage("u")}); err != nil {
		t.Fatalf("Generate #1: %v", err)
	}
	// 2nd call (same instance): func now returns a key -> override wins.
	giveKey = true
	if _, err := cm.Generate(context.Background(), []*schema.Message{schema.UserMessage("u")}); err != nil {
		t.Fatalf("Generate #2: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("captured %d requests, want 2: %v", len(seen), seen)
	}
	if seen[0] != "Bearer yaml-key" {
		t.Errorf("call #1 Authorization = %q, want Bearer yaml-key (ok=false)", seen[0])
	}
	if seen[1] != "Bearer resolver-key" {
		t.Errorf("call #2 Authorization = %q, want Bearer resolver-key (override wins)", seen[1])
	}
}

type capturedEgress struct {
	header http.Header
	url    string
	body   string
}

func newCredentialCaptureServer(t *testing.T, captures *[]capturedEgress, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*captures = append(*captures, capturedEgress{header: r.Header.Clone(), url: r.URL.String(), body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"unavailable"}}`))
	}))
}

func assertCredentialEgress(t *testing.T, got capturedEgress, provider, key string) {
	t.Helper()
	want := map[string]string{}
	switch provider {
	case "openai", "deepseek", "qwen":
		if key != "" {
			want["Authorization"] = "Bearer " + key
		}
	case "claude":
		want["x-api-key"] = key
	case "gemini":
		want["x-goog-api-key"] = key
	}
	for _, header := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
		if got.header.Get(header) != want[header] {
			t.Errorf("%s %s = %q, want %q", provider, header, got.header.Get(header), want[header])
		}
	}
	for _, secret := range []string{key, "yaml-secret"} {
		if secret != "" && (strings.Contains(got.url, secret) || strings.Contains(got.body, secret)) {
			t.Errorf("%s leaked credential in URL/body: url=%q body=%q", provider, got.url, got.body)
		}
	}
}

func TestChatModel_RuntimeCredentialEgressMatrix(t *testing.T) {
	for _, provider := range []string{"openai", "deepseek", "qwen", "claude", "gemini", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			var captures []capturedEgress
			var mu sync.Mutex
			server := newCredentialCaptureServer(t, &captures, &mu)
			defer server.Close()
			key := "runtime-" + provider + "-secret"
			chatModel, err := einowrap.NewChatModel(context.Background(), config.AgentAIConfig{
				Provider: provider, APIKey: "yaml-secret", Model: "credential-test-model", MaxTokens: 16,
			}, einowrap.Options{BaseURL: server.URL, HTTPClient: server.Client(), RuntimeKeyFunc: func(context.Context) (string, bool) {
				return key, true
			}})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
			mu.Lock()
			defer mu.Unlock()
			if len(captures) != 1 {
				t.Fatalf("captured %d requests, want 1", len(captures))
			}
			assertCredentialEgress(t, captures[0], provider, key)
		})
	}
}

func TestChatModel_RuntimeCredentialRotationClearAndProviderHotSwitch(t *testing.T) {
	var captures []capturedEgress
	var mu sync.Mutex
	server := newCredentialCaptureServer(t, &captures, &mu)
	defer server.Close()

	provider := "openai"
	key := "rotation-one-secret"
	keyOK := true
	runtime := einowrap.RuntimeAI{Provider: func(context.Context) (string, bool) { return provider, true }}
	holder := einowrap.NewChatModelHolder(config.AgentAIConfig{
		Provider: "openai", APIKey: "yaml-secret", Model: "credential-test-model", MaxTokens: 16,
	}, einowrap.Options{BaseURL: server.URL, HTTPClient: server.Client(), RuntimeKeyFunc: func(context.Context) (string, bool) {
		return key, keyOK
	}}, runtime)

	call := func() {
		model, err := holder.Get(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = model.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	}
	call()
	key = "rotation-two-secret"
	call()
	key = ""
	call()
	for _, next := range []string{"claude", "gemini", "ollama"} {
		provider = next
		key = "switch-" + next + "-secret"
		call()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captures) != 6 {
		t.Fatalf("captured %d requests, want 6", len(captures))
	}
	assertCredentialEgress(t, captures[0], "openai", "rotation-one-secret")
	assertCredentialEgress(t, captures[1], "openai", "rotation-two-secret")
	assertCredentialEgress(t, captures[2], "openai", "")
	assertCredentialEgress(t, captures[3], "claude", "switch-claude-secret")
	assertCredentialEgress(t, captures[4], "gemini", "switch-gemini-secret")
	assertCredentialEgress(t, captures[5], "ollama", "switch-ollama-secret")
}

func TestChatModel_NativeRuntimeNoOpinionUsesYAMLAndClearRestoresFloor(t *testing.T) {
	for _, provider := range []string{"claude", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			var captures []capturedEgress
			var mu sync.Mutex
			server := newCredentialCaptureServer(t, &captures, &mu)
			defer server.Close()
			key := ""
			keyOK := false
			chatModel, err := einowrap.NewChatModel(context.Background(), config.AgentAIConfig{
				Provider: provider, APIKey: "yaml-secret", Model: "credential-test-model", MaxTokens: 16,
			}, einowrap.Options{BaseURL: server.URL, HTTPClient: server.Client(), RuntimeKeyFunc: func(context.Context) (string, bool) {
				return key, keyOK
			}})
			if err != nil {
				t.Fatal(err)
			}
			call := func() {
				_, _ = chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
			}
			call()
			key, keyOK = "runtime-secret", true
			call()
			key, keyOK = "", false
			call()

			mu.Lock()
			defer mu.Unlock()
			if len(captures) != 3 {
				t.Fatalf("captured %d requests, want 3", len(captures))
			}
			assertCredentialEgress(t, captures[0], provider, "yaml-secret")
			assertCredentialEgress(t, captures[1], provider, "runtime-secret")
			assertCredentialEgress(t, captures[2], provider, "yaml-secret")
		})
	}
}
