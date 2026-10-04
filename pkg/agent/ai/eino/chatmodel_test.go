package eino_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// TestChatModel_ProducesParseableFinding asserts that a chat model
// built by NewChatModel against a canned chat/completions backend
// returns a JSON payload that detect.ParseFinding round-trips into the
// expected AIFinding. This is the chat-model wiring test: it does NOT
// exercise the prompt content or the DetectAgent — those are covered separately.
func TestChatModel_ProducesParseableFinding(t *testing.T) {
	expected := core.AIFinding{
		Title:      "Database connection refused",
		Summary:    "The api service cannot reach the primary postgres instance.",
		Severity:   "high",
		Category:   "database",
		Confidence: 0.82,
		Suggestions: []string{
			"Check primary postgres reachability",
			"Inspect recent network policy changes",
		},
	}

	// Stub OpenAI chat/completions endpoint. Returns one choice whose
	// message.content is the JSON-encoded AIFinding above, mimicking
	// JSON-mode output.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("missing/incorrect Authorization header: %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if _, ok := req["max_completion_tokens"]; !ok {
			t.Fatalf("request missing max_completion_tokens: %s", string(body))
		}
		if _, ok := req["max_tokens"]; ok {
			t.Fatalf("request must not include deprecated max_tokens: %s", string(body))
		}

		content, err := json.Marshal(expected)
		if err != nil {
			t.Fatalf("marshal finding: %v", err)
		}

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
					"content": string(content),
				},
			}},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"total_tokens":      30,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := config.AgentAIConfig{
		APIKey:      "test-key",
		Model:       "gpt-4o-mini",
		Temperature: 0.2,
		MaxTokens:   256,
	}

	ctx := context.Background()
	cm, err := einowrap.NewChatModel(ctx, cfg, einowrap.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewChatModel: %v", err)
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

	got, err := detect.ParseFinding(msg.Content)
	if err != nil {
		t.Fatalf("ParseFinding: %v\nraw: %s", err, msg.Content)
	}

	if got.Title != expected.Title {
		t.Errorf("Title = %q, want %q", got.Title, expected.Title)
	}
	if got.Summary != expected.Summary {
		t.Errorf("Summary = %q, want %q", got.Summary, expected.Summary)
	}
	if got.Severity != expected.Severity {
		t.Errorf("Severity = %q, want %q", got.Severity, expected.Severity)
	}
	if got.Category != expected.Category {
		t.Errorf("Category = %q, want %q", got.Category, expected.Category)
	}
	if got.Confidence < 0.81 || got.Confidence > 0.83 {
		t.Errorf("Confidence = %v, want ~0.82", got.Confidence)
	}
	if len(got.Suggestions) != 2 {
		t.Fatalf("Suggestions len = %d, want 2", len(got.Suggestions))
	}
}

func TestNewChatModel_EmptyModel(t *testing.T) {
	_, err := einowrap.NewChatModel(context.Background(), config.AgentAIConfig{Model: ""}, einowrap.Options{})
	if err == nil {
		t.Fatalf("expected error for empty model")
	}
}

func TestChatModelConfiguredBaseURL(t *testing.T) {
	for _, toolCalling := range []bool{false, true} {
		name := "detect"
		if toolCalling {
			name = "tool-calling"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/chat/completions" {
					t.Errorf("path = %q", request.URL.Path)
				}
				if request.Header.Get("Authorization") != "Bearer custom-key" {
					t.Error("custom endpoint did not receive bearer authentication")
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{"id":"custom","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer server.Close()
			cfg := config.AgentAIConfig{Provider: "claude", BaseURL: server.URL + "/v1", Model: "custom-model", APIKey: "configured-key"}
			opts := einowrap.Options{BaseURL: "http://unused.invalid", RuntimeKeyFunc: func(context.Context) (string, bool) {
				return "custom-key", true
			}}
			ctx := context.Background()
			if toolCalling {
				holder := einowrap.NewToolCallingChatModelHolder(cfg, opts, einowrap.RuntimeAI{
					Provider: func(context.Context) (string, bool) { return "gemini", true },
					Credential: func(_ context.Context, provider, baseURL string) (string, bool) {
						if provider != "openai" || baseURL != server.URL+"/v1" {
							return "", true
						}
						return "custom-key", true
					},
				})
				chatModel, err := holder.Get(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := chatModel.Generate(ctx, []*schema.Message{schema.UserMessage("test")}); err != nil {
					t.Fatal(err)
				}
			} else {
				chatModel, err := einowrap.NewChatModel(ctx, cfg, opts)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := chatModel.Generate(ctx, []*schema.Message{schema.UserMessage("test")}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestChatModelInvalidBaseURL(t *testing.T) {
	for _, baseURL := range []string{"models.example/v1", "ftp://models.example", "https://user:secret@models.example/v1", "https://models.example/v1?key=secret", "https://models.example/v1#secret"} {
		cfg := config.AgentAIConfig{BaseURL: baseURL, Model: "custom-model"}
		if _, err := einowrap.NewChatModel(context.Background(), cfg, einowrap.Options{}); err == nil {
			t.Error("invalid base_url accepted")
		}
		if _, err := einowrap.NewToolCallingChatModel(context.Background(), cfg, einowrap.Options{}); err == nil {
			t.Error("invalid tool-calling base_url accepted")
		}
		if _, err := einowrap.NewEmbedder(context.Background(), cfg, einowrap.Options{}); err == nil {
			t.Error("invalid embedding base_url accepted")
		}
	}
}
