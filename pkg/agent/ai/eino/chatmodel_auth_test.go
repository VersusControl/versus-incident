package eino_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// no AuthKeyFunc the outbound Authorization header is the YAML key, exactly
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

// TestChatModel_AuthOverride_FuncWins proves that when AuthKeyFunc returns
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
		BaseURL:     srv.URL,
		AuthKeyFunc: keyFn,
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
