package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"
	einodeepseek "github.com/cloudwego/eino-ext/components/model/deepseek"
	einogemini "github.com/cloudwego/eino-ext/components/model/gemini"
	einoollama "github.com/cloudwego/eino-ext/components/model/ollama"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	einoqwen "github.com/cloudwego/eino-ext/components/model/qwen"
	aclopenai "github.com/cloudwego/eino-ext/libs/acl/openai"
	"github.com/cloudwego/eino/components/model"
	"google.golang.org/genai"
)

// DefaultProvider is the model backend used when AgentAIConfig.Provider is
// empty. It is the OSS default and preserves the historical OpenAI behaviour
// byte-for-byte (JSON-mode, MaxCompletionTokens, AuthKeyFunc transport, the
// test-only Options.BaseURL seam).
const DefaultProvider = "openai"

// chatModelRequest is the normalized, provider-agnostic input the registry
// passes to each builder. The HTTPClient is already wrapped with the
// AuthKeyFunc transport (see NewChatModel/NewToolCallingChatModel), so every
// provider that authenticates with a Bearer Authorization header honours the
// runtime key override for free.
type chatModelRequest struct {
	apiKey      string
	model       string
	baseURL     string // test-only Options.BaseURL; "" uses the provider default
	httpClient  *http.Client
	timeout     time.Duration
	maxTokens   int
	temperature *float32
	// jsonMode forces structured JSON output. detect (NewChatModel) sets it;
	// the tool-calling analyze path (NewToolCallingChatModel) does not, because
	// JSON-mode and tool_calls are mutually exclusive on most providers.
	jsonMode bool
}

// chatModelBuilder constructs the provider-specific eino model. Every builder
// returns the tool-calling variant; NewChatModel narrows it to BaseChatModel.
type chatModelBuilder func(ctx context.Context, req chatModelRequest) (model.ToolCallingChatModel, error)

// chatModelBuilders is the provider registry. It is the ONLY place concrete
// model SDKs are referenced; adding a provider is a single map entry plus its
// builder. Keys are lower-case provider identifiers matched against
// AgentAIConfig.Provider (empty normalises to DefaultProvider).
var chatModelBuilders = map[string]chatModelBuilder{
	"openai":   buildOpenAIChatModel,
	"deepseek": buildDeepSeekChatModel,
	"qwen":     buildQwenChatModel,
	"ollama":   buildOllamaChatModel,
	"claude":   buildClaudeChatModel,
	"gemini":   buildGeminiChatModel,
}

// resolveProvider normalises the configured provider: empty/whitespace becomes
// the default, and the value is lower-cased so config casing does not matter.
func resolveProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return DefaultProvider
	}
	return p
}

// supportedProviders returns the sorted list of registered chat providers,
// used to build a helpful fail-fast error message.
func supportedProviders() []string {
	names := make([]string, 0, len(chatModelBuilders))
	for k := range chatModelBuilders {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// SupportedProviders is the exported, sorted list of registered chat
// providers. It lets a consumer (e.g. the enterprise runtime AI-settings API)
// validate an operator-supplied provider on WRITE against the SAME registry
// the model builders use, so an unknown value is rejected at the boundary
// rather than relying solely on the Holder's runtime fail-closed.
func SupportedProviders() []string {
	return supportedProviders()
}

// IsSupportedProvider reports whether name (case-insensitive, with the empty
// string normalised to DefaultProvider) is a registered chat provider. It is
// the boundary validator the enterprise control API gates writes on.
func IsSupportedProvider(name string) bool {
	return isChatProvider(resolveProvider(name))
}

// newProviderChatModel resolves the provider and dispatches to its builder.
// An unknown/unsupported provider fails fast here with a clear error — there
// is NO silent fallback to openai.
func newProviderChatModel(ctx context.Context, provider string, req chatModelRequest) (model.ToolCallingChatModel, error) {
	name := resolveProvider(provider)
	build, ok := chatModelBuilders[name]
	if !ok {
		return nil, fmt.Errorf("eino: unsupported ai provider %q (supported: %s)", name, strings.Join(supportedProviders(), ", "))
	}
	return build(ctx, req)
}

// openAIFixedSamplingPrefixes lists the OpenAI model-id families whose
// chat-completions API fixes the sampling parameters server-side — temperature,
// top_p and n are pinned at 1, presence_penalty and frequency_penalty at 0 —
// and REJECTS any explicit non-default value with an HTTP 400 ("this model has
// beta-limitations, temperature, top_p and n are fixed at 1, while
// presence_penalty and frequency_penalty are fixed at 0"). It covers the
// o-series reasoning models (o1/o3/o4, including their -mini / -preview /
// dated variants) and the gpt-5 family (the default config model is
// gpt-5.4-mini). Extend this slice when OpenAI ships another family with the
// same beta-limitation; nothing else needs to change.
var openAIFixedSamplingPrefixes = []string{"o1", "o3", "o4", "gpt-5"}

// isFixedSamplingModel reports whether the given OpenAI model id belongs to a
// beta-limited / reasoning family that fixes the sampling parameters and
// rejects an explicit temperature (see openAIFixedSamplingPrefixes). When it
// returns true the OpenAI chat builder MUST omit temperature (and must not send
// a non-default top_p / n / presence_penalty / frequency_penalty) so the
// provider applies its own fixed default instead of erroring. Matching is
// case-insensitive and anchored to the model-id prefix with a separator/end
// boundary, so suffixed variants (o1-mini, o4-mini-2025-04-16, gpt-5.4-mini)
// are recognised while unrelated ids that merely share leading characters
// (e.g. a hypothetical "gpt-50") are not. It is OpenAI-specific by design —
// other providers do not share this limitation — so it lives beside the OpenAI
// builder, the single chokepoint where req.model is known.
func isFixedSamplingModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, p := range openAIFixedSamplingPrefixes {
		if m == p {
			return true
		}
		if strings.HasPrefix(m, p) {
			switch m[len(p)] {
			case '-', '.':
				return true
			}
		}
	}
	return false
}

// buildOpenAIChatModel is the default path. It preserves the historical OpenAI
// wiring — JSON-mode response format, MaxCompletionTokens (the reasoning-safe
// field), the configured temperature, the test-only BaseURL, and the
// AuthKeyFunc-wrapped HTTPClient — with one model-aware refinement: for
// beta-limited / reasoning families (see isFixedSamplingModel) temperature is
// omitted so the provider applies its fixed default instead of rejecting the
// request. The SDK never sets top_p / n / presence_penalty / frequency_penalty
// here (they stay nil/zero and are not serialised), so those already satisfy
// the beta-limitation for these models.
func buildOpenAIChatModel(ctx context.Context, req chatModelRequest) (model.ToolCallingChatModel, error) {
	maxCompletionTokens := req.maxTokens
	temperature := req.temperature
	if isFixedSamplingModel(req.model) {
		// Beta-limited / reasoning model: send no explicit temperature so the
		// provider uses its fixed default (the negative sentinel already does
		// this for everyone else; this makes the omission automatic so an
		// operator no longer has to set temperature: -1 per reasoning model).
		temperature = nil
	}
	conf := &einoopenai.ChatModelConfig{
		APIKey:              req.apiKey,
		Timeout:             req.timeout,
		HTTPClient:          req.httpClient,
		BaseURL:             req.baseURL,
		Model:               req.model,
		MaxCompletionTokens: &maxCompletionTokens,
		Temperature:         temperature,
	}
	if req.jsonMode {
		conf.ResponseFormat = &einoopenai.ChatCompletionResponseFormat{
			Type: einoopenai.ChatCompletionResponseFormatTypeJSONObject,
		}
	}
	return einoopenai.NewChatModel(ctx, conf)
}

// buildDeepSeekChatModel wires the OpenAI-compatible DeepSeek backend. It
// accepts a Bearer key, so the AuthKeyFunc transport on req.httpClient works
// unchanged.
func buildDeepSeekChatModel(ctx context.Context, req chatModelRequest) (model.ToolCallingChatModel, error) {
	conf := &einodeepseek.ChatModelConfig{
		APIKey:     req.apiKey,
		Timeout:    req.timeout,
		HTTPClient: req.httpClient,
		BaseURL:    req.baseURL,
		Model:      req.model,
		MaxTokens:  req.maxTokens,
	}
	if req.temperature != nil {
		conf.Temperature = *req.temperature
	}
	if req.jsonMode {
		conf.ResponseFormatType = einodeepseek.ResponseFormatTypeJSONObject
	}
	return einodeepseek.NewChatModel(ctx, conf)
}

// buildQwenChatModel wires Alibaba Qwen (DashScope OpenAI-compatible mode). The
// SDK requires a non-empty BaseURL, so we default to the public compatible
// endpoint when none is supplied. It accepts a Bearer key.
func buildQwenChatModel(ctx context.Context, req chatModelRequest) (model.ToolCallingChatModel, error) {
	baseURL := req.baseURL
	if baseURL == "" {
		baseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	maxTokens := req.maxTokens
	conf := &einoqwen.ChatModelConfig{
		APIKey:      req.apiKey,
		Timeout:     req.timeout,
		HTTPClient:  req.httpClient,
		BaseURL:     baseURL,
		Model:       req.model,
		MaxTokens:   &maxTokens,
		Temperature: req.temperature,
	}
	if req.jsonMode {
		conf.ResponseFormat = &aclopenai.ChatCompletionResponseFormat{
			Type: aclopenai.ChatCompletionResponseFormatTypeJSONObject,
		}
	}
	return einoqwen.NewChatModel(ctx, conf)
}

// buildOllamaChatModel wires a local Ollama server. Ollama is keyless, so the
// AuthKeyFunc transport is a harmless no-op. BaseURL defaults to the local
// daemon when unset. JSON-mode is requested via the native `format` field.
func buildOllamaChatModel(ctx context.Context, req chatModelRequest) (model.ToolCallingChatModel, error) {
	baseURL := req.baseURL
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	conf := &einoollama.ChatModelConfig{
		BaseURL:    baseURL,
		Timeout:    req.timeout,
		HTTPClient: req.httpClient,
		Model:      req.model,
	}
	if req.jsonMode {
		conf.Format = json.RawMessage(`"json"`)
	}
	return einoollama.NewChatModel(ctx, conf)
}

// buildClaudeChatModel wires Anthropic Claude (direct API). Claude authenticates
// with the x-api-key header rather than a Bearer token, so the AuthKeyFunc
// override does not apply; the configured APIKey is used. MaxTokens is required
// (>0) by the SDK — the caller always supplies a non-zero default. Anthropic has
// no response_format knob, so jsonMode is advisory only (detect's ParseFinding
// is tolerant of fenced/plain JSON).
func buildClaudeChatModel(ctx context.Context, req chatModelRequest) (model.ToolCallingChatModel, error) {
	conf := &einoclaude.Config{
		APIKey:      req.apiKey,
		Model:       req.model,
		MaxTokens:   req.maxTokens,
		Temperature: req.temperature,
		HTTPClient:  req.httpClient,
	}
	if req.baseURL != "" {
		base := req.baseURL
		conf.BaseURL = &base
	}
	return einoclaude.NewChatModel(ctx, conf)
}

// buildGeminiChatModel wires Google Gemini (the Generative Language API) via the
// genai client. Gemini authenticates with the api key through the x-goog-api-key
// header set by the genai client — NOT a Bearer token — so, exactly like
// Claude's x-api-key path, the AuthKeyFunc Bearer override on req.httpClient does
// not apply; the configured req.apiKey is passed straight to the client.
// req.baseURL is honoured as the genai HTTPOptions.BaseURL (the test-only
// httptest seam). Gemini exposes structured JSON output only via a full
// ResponseJSONSchema, not a bare response_mime_type toggle, so jsonMode is
// advisory here (like Claude); detect's ParseFinding tolerates fenced/plain JSON.
func buildGeminiChatModel(ctx context.Context, req chatModelRequest) (model.ToolCallingChatModel, error) {
	client, err := newGeminiClient(ctx, req.apiKey, req.baseURL, req.httpClient, req.timeout)
	if err != nil {
		return nil, err
	}
	conf := &einogemini.Config{
		Client:      client,
		Model:       req.model,
		Temperature: req.temperature,
	}
	if req.maxTokens > 0 {
		maxTokens := req.maxTokens
		conf.MaxTokens = &maxTokens
	}
	return einogemini.NewChatModel(ctx, conf)
}

// newGeminiClient builds the google.golang.org/genai client shared by the Gemini
// chat and embedding builders. It pins the Gemini API backend (never Vertex) and
// passes the api key directly: Gemini sends it via the x-goog-api-key header, so
// the Bearer AuthKeyFunc override that req.httpClient may carry is a harmless
// no-op for this provider. baseURL is the test-only endpoint override ("" uses
// the public Gemini endpoint); httpClient and timeout are threaded through so the
// per-request deadline holds even when the SDK supplies its own default client.
func newGeminiClient(ctx context.Context, apiKey, baseURL string, httpClient *http.Client, timeout time.Duration) (*genai.Client, error) {
	cc := &genai.ClientConfig{
		APIKey:     apiKey,
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: httpClient,
	}
	if baseURL != "" {
		cc.HTTPOptions.BaseURL = baseURL
	}
	if timeout > 0 {
		t := timeout
		cc.HTTPOptions.Timeout = &t
	}
	return genai.NewClient(ctx, cc)
}
