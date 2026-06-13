package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestBuildAIs_ThreadsBaseURLIntoDetect asserts the factory wires
// cfg.AI.BaseURL into the detect agent's chat client: a detect Run lands
// on the configured endpoint, not api.openai.com. detect/analyze.Options
// already honored BaseURL; the factory was the missing link that never set
// it. An httptest server stands in for any OpenAI-compatible backend
// (Ollama / vLLM / LocalAI / gateway).
func TestBuildAIs_ThreadsBaseURLIntoDetect(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		content, _ := json.Marshal(map[string]any{
			"title": "x", "summary": "y", "severity": "low", "confidence": 0.5,
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
	defer srv.Close()

	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	cfg := config.AgentConfig{}
	cfg.AI.Enable = true
	cfg.AI.APIKey = "test-key"
	cfg.AI.Model = "gpt-4o-mini"
	cfg.AI.BaseURL = srv.URL

	bundle := BuildAIs(cfg, cat, storage.NewMemory(), nil)
	if bundle.Detect == nil {
		t.Fatal("detect agent not built")
	}

	if _, err := bundle.Detect.Run(context.Background(), core.DetectTask{
		Result: core.AgentResult{Template: "WARN <*> failed", Frequency: 1},
	}); err != nil {
		t.Fatalf("detect Run: %v", err)
	}

	if atomic.LoadInt32(&hits) == 0 {
		t.Error("detect agent did not call the configured base_url")
	}
}
