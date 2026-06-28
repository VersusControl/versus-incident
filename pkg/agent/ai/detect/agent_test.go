package detect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
	"unsafe"

	einoollama "github.com/cloudwego/eino-ext/components/model/ollama"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// newTestAgent spins up an httptest-backed detect Agent that returns a
// fixed AIFinding. The model field is read via reflection by the
// tool-free guard, so the agent must be constructed through the real
// constructor (not faked).
func newTestAgent(t *testing.T) (*Agent, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		content, _ := json.Marshal(map[string]any{
			"title":      "x",
			"summary":    "y",
			"severity":   "low",
			"confidence": 0.5,
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "test",
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": string(content)},
			}},
		})
	}))

	a, err := New(context.Background(), config.AgentAIConfig{
		APIKey:      "test-key",
		Model:       "gpt-4o-mini",
		Temperature: 0.2,
		MaxTokens:   256,
	}, Options{BaseURL: srv.URL})
	if err != nil {
		srv.Close()
		t.Fatalf("New: %v", err)
	}
	return a, srv
}

// TestAgent_KindAndName asserts the agent advertises itself as the
// detect task and answers Name() with a stable id. The router relies
// on Kind() for dispatch.
func TestAgent_KindAndName(t *testing.T) {
	a, srv := newTestAgent(t)
	defer srv.Close()

	if got := a.Kind(); got != core.AITaskDetect {
		t.Fatalf("Kind = %q, want %q", got, core.AITaskDetect)
	}
	if got := a.Name(); got != "detect" {
		t.Fatalf("Name = %q, want %q", got, "detect")
	}
}

// TestAgent_RejectsNonDetectTask guards against a router mis-route. If
// somebody registers the detect agent under the analyze kind, Run must
// refuse rather than try to interpret an unknown task shape.
func TestAgent_RejectsNonDetectTask(t *testing.T) {
	a, srv := newTestAgent(t)
	defer srv.Close()

	if _, err := a.Run(context.Background(), core.AnalyzeTask{Snapshot: core.AnalyzeIncidentSnapshot{IncidentID: "i-1"}}); err == nil {
		t.Fatalf("expected error for AnalyzeTask, got nil")
	}
}

// TestAgent_ToolFree asserts the constructed Eino chat model has NO
// tools bound. Detect must stay tool-free — wiring tools here would
// turn every noisy log line into a multi-step LLM workflow. The
// underlying eino-ext/openai Client carries a `tools []tool` field;
// we reach in with reflection (read-only, via unsafe pointer) and
// assert it is empty.
//
// This test fails the build the moment somebody calls BindTools /
// WithTools / NewChatModel-with-tools on the detect path.
func TestAgent_ToolFree(t *testing.T) {
	a, srv := newTestAgent(t)
	defer srv.Close()

	chat := a.ChatModel()
	if chat == nil {
		t.Fatalf("ChatModel() returned nil")
	}

	// Must NOT be a tool-bound wrapper. Detect is a plain ChatModel.
	cm, ok := chat.(*einoopenai.ChatModel)
	if !ok {
		t.Fatalf("ChatModel = %T, want *einoopenai.ChatModel", chat)
	}

	// Dig into the unexported `cli` field of the Eino wrapper to reach
	// the openai-acl Client.
	cliField := reflect.ValueOf(cm).Elem().FieldByName("cli")
	if !cliField.IsValid() {
		t.Fatalf("missing cli field on *einoopenai.ChatModel — Eino internals changed; update guard")
	}
	cli := reflect.NewAt(cliField.Type(), unsafe.Pointer(cliField.UnsafeAddr())).Elem()

	// cli is *openai.Client. Walk into its `tools` slice.
	cliElem := cli.Elem()
	tools := cliElem.FieldByName("tools")
	if !tools.IsValid() {
		t.Fatalf("missing tools field on openai.Client — Eino internals changed; update guard")
	}
	toolsVal := reflect.NewAt(tools.Type(), unsafe.Pointer(tools.UnsafeAddr())).Elem()
	if toolsVal.Kind() != reflect.Slice {
		t.Fatalf("tools field kind = %v, want slice", toolsVal.Kind())
	}
	if n := toolsVal.Len(); n != 0 {
		t.Fatalf("detect agent has %d bound tools; detect must stay tool-free", n)
	}
}

// TestAgent_RuntimeProviderOverride proves the detect agent routes model
// construction through the runtime-provider holder seam: a runtime provider
// override selects a different concrete backend at build time, and an unknown
// runtime provider fails closed to the configured provider (no error, no
// crash). Offline — construction never dials out.
func TestAgent_RuntimeProviderOverride(t *testing.T) {
	cfg := config.AgentAIConfig{APIKey: "k", Model: "gpt-4o-mini", MaxTokens: 64}

	// Runtime override to ollama ⇒ the eagerly-built model is the ollama
	// backend, proving detect consults the holder's runtime-provider seam.
	a, err := New(context.Background(), cfg, Options{
		Runtime: einowrap.RuntimeAI{
			Provider: func(context.Context) (string, bool) { return "ollama", true },
		},
	})
	if err != nil {
		t.Fatalf("New(ollama override): %v", err)
	}
	if _, ok := a.ChatModel().(*einoollama.ChatModel); !ok {
		t.Fatalf("ChatModel = %T, want *einoollama.ChatModel", a.ChatModel())
	}

	// Unknown runtime provider ⇒ fail closed to the configured provider
	// (openai), no error.
	b, err := New(context.Background(), cfg, Options{
		Runtime: einowrap.RuntimeAI{
			Provider: func(context.Context) (string, bool) { return "nope", true },
		},
	})
	if err != nil {
		t.Fatalf("New(unknown override) must fail closed, got: %v", err)
	}
	if _, ok := b.ChatModel().(*einoopenai.ChatModel); !ok {
		t.Fatalf("fail-closed ChatModel = %T, want *einoopenai.ChatModel", b.ChatModel())
	}
}

// TestAgent_FieldIsBaseChatModel guarantees the Agent's model holder is
// parameterised on model.BaseChatModel, not the tool-calling extension. The
// generic type parameter makes it a compile-time impossibility for the
// detect path to ever hold (or rebuild into) a tool-calling model. If the
// holder is ever widened to model.ToolCallingChatModel this guard fails.
func TestAgent_FieldIsBaseChatModel(t *testing.T) {
	chatField, ok := reflect.TypeOf(Agent{}).FieldByName("chat")
	if !ok {
		t.Fatalf("Agent has no chat field")
	}
	want := reflect.TypeOf((*einowrap.Holder[model.BaseChatModel])(nil))
	if chatField.Type != want {
		t.Fatalf("Agent.chat type = %v, want %v", chatField.Type, want)
	}
}
