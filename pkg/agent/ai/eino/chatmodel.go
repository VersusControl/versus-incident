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

	// AuthKeyFunc is an OPTIONAL per-request Authorization override. When
	// non-nil the outbound transport calls it for every request: if it
	// returns ok the request's Authorization header is replaced with
	// "Bearer <key>" AFTER the SDK set its YAML-keyed header (so the
	// override wins); ok=false leaves the YAML-keyed header untouched. When
	// nil the transport is a plain pass-through and the client is used
	// exactly as before — this is the OSS byte-for-byte path. The package
	// stays generic: it knows nothing about who supplies the key (the agent
	// package injects a function backed by its runtime AISettingsResolver,
	// which avoids an import cycle because eino never imports agent).
	AuthKeyFunc func(ctx context.Context) (key string, ok bool)
}

// authRoundTripper injects a runtime Authorization override onto every
// outbound request. It consults keyFn per request, so a hot-swapped key
// takes effect without rebuilding the client. Per the http.RoundTripper
// contract it must not mutate the input request, so it clones the request
// (which deep-copies the header) before writing the override.
type authRoundTripper struct {
	base  http.RoundTripper
	keyFn func(ctx context.Context) (key string, ok bool)
}

func (a authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := a.base
	if base == nil {
		base = http.DefaultTransport
	}
	if a.keyFn == nil {
		return base.RoundTrip(req)
	}
	key, ok := a.keyFn(req.Context())
	if !ok {
		// No opinion: send the request exactly as the SDK built it.
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+key)
	return base.RoundTrip(clone)
}

// withAuthRoundTripper returns an *http.Client whose transport injects the
// runtime Authorization override. When keyFn is nil the input client is
// returned unchanged (nil included), so the no-resolver path is byte-for-
// byte identical to the pre-seam wiring. The input client is never mutated:
// a shallow copy is wrapped so the caller's client keeps its own transport.
func withAuthRoundTripper(c *http.Client, timeout time.Duration, keyFn func(ctx context.Context) (key string, ok bool)) *http.Client {
	if keyFn == nil {
		return c
	}
	if c == nil {
		c = &http.Client{Timeout: timeout}
	}
	wrapped := *c
	wrapped.Transport = authRoundTripper{base: c.Transport, keyFn: keyFn}
	return &wrapped
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

	// MaxTokens maps to the provider's max_completion_tokens. The default
	// is reasoning-safe: for gpt-5.* / o-series models the budget is shared
	// by hidden reasoning tokens and the visible reply, so a low cap can be
	// fully consumed by reasoning and yield empty content.
	maxCompletionTokens := cfg.MaxTokens
	if maxCompletionTokens == 0 {
		maxCompletionTokens = 2048
	}

	conf := &einoopenai.ChatModelConfig{
		APIKey:     cfg.APIKey,
		Timeout:    timeout,
		HTTPClient: withAuthRoundTripper(opts.HTTPClient, timeout, opts.AuthKeyFunc),
		BaseURL:    opts.BaseURL,
		Model:      cfg.Model,
		// OpenAI-compatible reasoning/beta models (gpt-5.*, o-series)
		// reject `max_tokens`; send the supported field instead.
		MaxCompletionTokens: &maxCompletionTokens,
		Temperature:         resolveTemperature(cfg.Temperature, 0.2),
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

	// Analyze is a multi-step ReAct loop (tool calls + final answer), so it
	// needs even more headroom than detect for reasoning models.
	maxCompletionTokens := cfg.MaxTokens
	if maxCompletionTokens == 0 {
		maxCompletionTokens = 4096
	}

	conf := &einoopenai.ChatModelConfig{
		APIKey:              cfg.APIKey,
		Timeout:             timeout,
		HTTPClient:          withAuthRoundTripper(opts.HTTPClient, timeout, opts.AuthKeyFunc),
		BaseURL:             opts.BaseURL,
		Model:               cfg.Model,
		MaxCompletionTokens: &maxCompletionTokens,
		Temperature:         resolveTemperature(cfg.Temperature, 0.2),
	}

	return einoopenai.NewChatModel(ctx, conf)
}

// A NEGATIVE value is the explicit "omit temperature" sentinel:
// it returns nil so the provider applies its own default. This is
// required for beta-limited / reasoning models (e.g. the gpt-5 family,
// o-series) that fix temperature at 1 and reject any explicit value. A
// zero value inherits the supplied default; any other value is sent
// verbatim.
func resolveTemperature(configured, def float64) *float32 {
	if configured < 0 {
		return nil
	}
	t := float32(configured)
	if configured == 0 {
		t = float32(def)
	}
	return &t
}
