// Package eino contains the Eino-backed chat model wrapper used by all
// AI agents in versus-incident. It is the ONLY package in the codebase
// that imports concrete model SDKs — every concrete AI agent (detect,
// analyze, ...) goes through these helpers so a future framework swap
// only touches this package.
//
// There is no `framework` knob in the config (Eino is the framework).
// The MODEL backend, however, IS selectable: `agent.ai.provider` chooses
// among the providers registered in provider.go (openai by default). The
// provider registry is the single chokepoint for concrete model imports.
package eino

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"

	"github.com/VersusControl/versus-incident/pkg/config"
)

type Options struct {
	HTTPClient *http.Client
	BaseURL    string
	Timeout    time.Duration

	// RuntimeKeyFunc is an OPTIONAL per-request provider credential override.
	// Provider builders apply it using their native authentication scheme.
	// ok=false leaves the SDK's configured key untouched; ok=true with an
	// empty key explicitly clears credentials for that request.
	RuntimeKeyFunc func(ctx context.Context) (key string, ok bool)
}

type credentialPolicy uint8

const (
	credentialBearer credentialPolicy = iota
	credentialClaude
	credentialGemini
	credentialNone
)

var credentialHeaders = []string{"Authorization", "x-api-key", "x-goog-api-key"}

type runtimeKeyRoundTripper struct {
	base   http.RoundTripper
	keyFn  func(ctx context.Context) (key string, ok bool)
	policy credentialPolicy
}

func (t runtimeKeyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.keyFn == nil && t.policy != credentialNone {
		return base.RoundTrip(req)
	}
	key, ok := "", false
	if t.keyFn != nil {
		key, ok = t.keyFn(req.Context())
	}
	if !ok && t.policy != credentialNone {
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	for _, header := range credentialHeaders {
		clone.Header.Del(header)
	}
	if key != "" {
		switch t.policy {
		case credentialBearer:
			clone.Header.Set("Authorization", "Bearer "+key)
		case credentialClaude:
			clone.Header.Set("x-api-key", key)
		case credentialGemini:
			clone.Header.Set("x-goog-api-key", key)
		}
	}
	return base.RoundTrip(clone)
}

func (o Options) runtimeKeyFunc() func(context.Context) (string, bool) {
	return o.RuntimeKeyFunc
}

func withRuntimeKeyRoundTripper(c *http.Client, timeout time.Duration, keyFn func(ctx context.Context) (key string, ok bool), policy credentialPolicy) *http.Client {
	if c == nil {
		c = &http.Client{Timeout: timeout}
	}
	wrapped := *c
	wrapped.Transport = runtimeKeyRoundTripper{base: c.Transport, keyFn: keyFn, policy: policy}
	checkRedirect := c.CheckRedirect
	wrapped.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if policy != credentialNone && len(via) > 0 && !sameOrigin(request.URL, via[len(via)-1].URL) {
			return http.ErrUseLastResponse
		}
		if checkRedirect != nil {
			return checkRedirect(request, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return &wrapped
}

func sameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

// NewChatModel builds an Eino ChatModel configured for JSON-mode
// structured output. cfg must already be the *resolved* per-task
// config (see AgentAIConfig.Resolve) — this helper does not look at
// the per-task sub-blocks.
//
// The concrete model backend is selected by cfg.Provider via the provider
// registry (provider.go); an empty provider defaults to openai and an
// unsupported one fails fast. Model must be set; APIKey may be empty (the
// provider client errors at call time, not construction time, which is fine
// for tests).
func NewChatModel(ctx context.Context, cfg config.AgentAIConfig, opts Options) (model.BaseChatModel, error) {
	if cfg.Model == "" {
		return nil, emptyModelConfigError(false)
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

	provider, baseURL, err := configuredChatEndpoint(cfg, opts)
	if err != nil {
		return nil, err
	}
	return newProviderChatModel(ctx, provider, chatModelRequest{
		apiKey:      cfg.APIKey,
		model:       cfg.Model,
		baseURL:     baseURL,
		httpClient:  opts.HTTPClient,
		runtimeKey:  opts.runtimeKeyFunc(),
		timeout:     timeout,
		maxTokens:   maxCompletionTokens,
		temperature: resolveTemperature(cfg.Temperature, 0.2),
		// Force JSON-mode so ParseFinding can decode the reply with the
		// same tolerance it had under the raw HTTP client.
		jsonMode: true,
	})
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
		return nil, emptyModelConfigError(false)
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

	provider, baseURL, err := configuredChatEndpoint(cfg, opts)
	if err != nil {
		return nil, err
	}
	return newProviderChatModel(ctx, provider, chatModelRequest{
		apiKey:      cfg.APIKey,
		model:       cfg.Model,
		baseURL:     baseURL,
		httpClient:  opts.HTTPClient,
		runtimeKey:  opts.runtimeKeyFunc(),
		timeout:     timeout,
		maxTokens:   maxCompletionTokens,
		temperature: resolveTemperature(cfg.Temperature, 0.2),
		jsonMode:    false,
	})
}

func configuredChatEndpoint(cfg config.AgentAIConfig, opts Options) (string, string, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL != "" {
		if err := ValidateBaseURL(baseURL); err != nil {
			return "", "", err
		}
		return DefaultProvider, baseURL, nil
	}
	return cfg.Provider, opts.BaseURL, nil
}

// ValidateBaseURL accepts only absolute HTTP(S) endpoints without userinfo,
// query parameters, or fragments. The same policy is used at config load and
// runtime settings write boundaries; the value is never included in errors.
func ValidateBaseURL(baseURL string) error {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return &ConfigError{message: "eino: base_url must be an absolute HTTP(S) URL without credentials, query, or fragment"}
	}
	return nil
}

// A NEGATIVE value is the explicit "omit temperature" sentinel: it returns nil
// so the provider applies its own default. This is an OPERATOR override that
// works for any provider/model. For OpenAI beta-limited / reasoning models
// (the gpt-5 family, o-series) the omission is also applied AUTOMATICALLY in
// buildOpenAIChatModel (see isFixedSamplingModel), so operators no longer have
// to set -1 per reasoning deployment; the sentinel remains as a manual override
// for any model the family list does not yet cover. A zero value inherits the
// supplied default; any other value is sent verbatim.
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
