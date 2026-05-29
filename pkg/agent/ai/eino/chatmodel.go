// Package eino contains the Eino-backed chat model wrapper used by all
// AI agents in versus-incident. It is the ONLY package in the codebase
// that imports Eino's model package — every concrete AI agent (detect,
// analyze, ...) goes through this helper so a future framework swap
// only touches one file.
//
// There is no `framework` knob in the config. Eino is the
// implementation; if operators need a different backend they bring it
// up here.
package eino

import (
	"context"
	"fmt"
	"net/http"
	"time"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// BaseURL is overridable for tests. Production code passes "" to use
// the Eino / go-openai default (https://api.openai.com/v1). The agent
// admin endpoints never expose this; only the chatmodel test sets it
// to point at an httptest server.
type Options struct {
	HTTPClient *http.Client
	BaseURL    string
	Timeout    time.Duration
}

// NewChatModel builds an Eino ChatModel configured for JSON-mode
// structured output. cfg must already be the *resolved* per-task
// config (see AgentAIConfig.Resolve) — this helper does not look at
// the per-task sub-blocks.
//
// Model must be set; APIKey may be empty (the OpenAI ACL client errors
// at call time, not construction time, which is fine for tests).
func NewChatModel(ctx context.Context, cfg config.AgentAIConfig, opts Options) (model.BaseChatModel, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("eino: model is empty")
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	temperature := float32(cfg.Temperature)
	if temperature == 0 {
		temperature = 0.2
	}

	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 512
	}

	conf := &einoopenai.ChatModelConfig{
		APIKey:      cfg.APIKey,
		Timeout:     timeout,
		HTTPClient:  opts.HTTPClient,
		BaseURL:     opts.BaseURL,
		Model:       cfg.Model,
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
		// Force JSON-mode so ParseFinding can decode the reply with the
		// same tolerance it had under the raw HTTP client.
		ResponseFormat: &einoopenai.ChatCompletionResponseFormat{
			Type: einoopenai.ChatCompletionResponseFormatTypeJSONObject,
		},
	}

	return einoopenai.NewChatModel(ctx, conf)
}

// NewToolCallingChatModel mirrors NewChatModel but returns the
// tool-calling variant. The analyze agent needs WithTools to register
// its read-only tool catalog; detect uses the base helper above.
//
// IMPORTANT: this helper deliberately does NOT force JSON-mode. With
// tools bound, the model alternates between tool_calls (JSON, never
// content) and a final assistant message; forcing JSON-mode causes
// providers to reject the tool-call turns.
func NewToolCallingChatModel(ctx context.Context, cfg config.AgentAIConfig, opts Options) (model.ToolCallingChatModel, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("eino: model is empty")
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	temperature := float32(cfg.Temperature)
	if temperature == 0 {
		temperature = 0.2
	}

	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}

	conf := &einoopenai.ChatModelConfig{
		APIKey:      cfg.APIKey,
		Timeout:     timeout,
		HTTPClient:  opts.HTTPClient,
		BaseURL:     opts.BaseURL,
		Model:       cfg.Model,
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
	}

	return einoopenai.NewChatModel(ctx, conf)
}
