package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// ProviderURL resolves the chat/completions endpoint for the named
// AI provider. All three supported providers speak the OpenAI
// chat/completions wire format; only the host changes (Gemini and
// Claude both ship an OpenAI compatibility shim). Returns an error
// for unknown values so a misconfigured deploy fails at startup with
// a clear message rather than silently 404'ing on every call.
//
// Empty input is treated as "openai" for backwards compatibility with
// configs written before the field existed.
func ProviderURL(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "openai":
		return "https://api.openai.com/v1/chat/completions", nil
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", nil
	case "claude":
		return "https://api.anthropic.com/v1/chat/completions", nil
	default:
		return "", fmt.Errorf("ai: unknown provider %q (want openai | gemini | claude)", provider)
	}
}

// normalizeProvider returns the canonical lowercase provider name,
// using "openai" for the empty default. Exposed so the audit log and
// startup banner show what the analyzer is actually talking to.
func normalizeProvider(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return "openai"
	}
	return p
}

// OpenAI is an AISRE backed by an OpenAI-compatible /chat/completions
// endpoint. The same client code drives OpenAI itself, Gemini's
// OpenAI compatibility shim, and Anthropic's OpenAI compatibility
// endpoint — the URL is the only thing that changes. Selected via
// AgentAIConfig.Provider; see ProviderURL.
type OpenAI struct {
	cfg        config.AgentAIConfig
	httpClient *http.Client
	chatURL    string // resolved from cfg.Provider at construction
	name       string // canonical provider name surfaced via Name()

	// SampleFn extracts the sample lines passed to the model from an
	// AgentResult. Defaults to "first up to 3 messages". Exposed so
	// the worker can swap in a redaction-aware extractor without the
	// AI package depending on pkg/agent.
	SampleFn func(core.AgentResult) []string

	// Retry parameters. Zero values mean "no retry beyond the first
	// attempt" — set by NewOpenAIWithRetry.
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// NewOpenAI constructs the analyzer. httpClient may be nil — a sane
// default (30s timeout, no proxy) is used. No retry — backwards
// compatible. Returns an error for an unknown provider.
func NewOpenAI(cfg config.AgentAIConfig, client *http.Client) (*OpenAI, error) {
	return NewOpenAIWithRetry(cfg, client, 1, 0, 0)
}

// NewOpenAIWithRetry constructs the analyzer with explicit retry
// parameters. maxAttempts <= 1 disables retry. Each retry sleeps
// `min(maxBackoff, initialBackoff * 2^(attempt-1))` plus up to 25%
// jitter. Retries fire on HTTP 429, any 5xx, or any network/transport
// error. 4xx other than 429 surface immediately (the request is
// fundamentally broken — retrying won't help). Returns an error if
// cfg.Provider is not one of the supported values.
func NewOpenAIWithRetry(cfg config.AgentAIConfig, client *http.Client, maxAttempts int, initialBackoff, maxBackoff time.Duration) (*OpenAI, error) {
	chatURL, err := ProviderURL(cfg.Provider)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &OpenAI{
		cfg:            cfg,
		httpClient:     client,
		chatURL:        chatURL,
		name:           normalizeProvider(cfg.Provider),
		SampleFn:       defaultSampleFn,
		maxAttempts:    maxAttempts,
		initialBackoff: initialBackoff,
		maxBackoff:     maxBackoff,
	}, nil
}

// Name implements core.AISRE. Returns the canonical provider name
// ("openai" / "gemini" / "claude") so the audit log identifies the
// backend each call actually hit.
func (o *OpenAI) Name() string { return o.name }

// Analyze sends the result to the configured chat endpoint and parses
// the structured AIFinding from the response. The returned
// AICallResult also carries the user prompt sent, the raw model
// response, the wall-clock duration, and the model id — used by the
// detect log to render an audit trail in the UI.
func (o *OpenAI) Analyze(ctx context.Context, r core.AgentResult) (*core.AICallResult, error) {
	if o == nil {
		return nil, fmt.Errorf("ai: nil analyzer")
	}
	if o.cfg.Model == "" {
		return nil, fmt.Errorf("ai: model is empty")
	}

	source := ""
	service := ""
	if len(r.SampleSignals) > 0 {
		source = r.SampleSignals[0].Source
		if v, ok := r.SampleSignals[0].Fields["service"]; ok {
			if s, ok := v.(string); ok {
				service = s
			}
		}
	}

	samples := o.SampleFn(r)
	system, user := BuildPrompt(r, source, service, samples)

	body := chatRequest{
		Model:       o.cfg.Model,
		Temperature: floatOr(o.cfg.Temperature, 0.2),
		MaxTokens:   intOr(o.cfg.MaxTokens, 512),
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat: &responseFormat{Type: "json_object"},
	}

	content, finishReason, durationMs, err := o.callOnce(ctx, body)
	if err != nil {
		return nil, err
	}

	// Auto-retry once when the response is truncated by max_tokens.
	// More verbose models (Gemini-2.5-flash is a notable offender)
	// often blow past a 512-token budget mid-JSON, leaving an
	// unbalanced object that ParseFinding cannot recover. Doubling
	// once usually clears it; capped at truncationRetryMaxTokens to
	// bound cost on pathological prompts.
	if finishReason == finishLength && body.MaxTokens < truncationRetryMaxTokens {
		body.MaxTokens *= 2
		if body.MaxTokens > truncationRetryMaxTokens {
			body.MaxTokens = truncationRetryMaxTokens
		}
		retryContent, retryFinish, retryDur, retryErr := o.callOnce(ctx, body)
		if retryErr != nil {
			// Surface the retry error but keep going — the first
			// (truncated) content may still parse into something
			// usable, and the operator should see both signals.
			return nil, fmt.Errorf("ai: truncated then retry failed: %w", retryErr)
		}
		content, finishReason = retryContent, retryFinish
		durationMs += retryDur
	}

	if finishReason == finishContentFilter {
		return nil, fmt.Errorf("ai: response blocked by safety filter (finish_reason=content_filter)")
	}

	finding, err := ParseFinding(content)
	if err != nil {
		// Surface the first chunk of what the model actually returned
		// so the detect audit log shows operators why parsing failed —
		// otherwise they only see "no JSON object found" with no clue
		// what the model said. Annotate the truncation case explicitly
		// since it has a different fix (raise max_tokens) than other
		// parse failures.
		if finishReason == finishLength {
			return nil, fmt.Errorf("ai: response truncated at max_tokens=%d, raise it (raw: %s)",
				body.MaxTokens, truncate(content, 300))
		}
		return nil, fmt.Errorf("%w (raw: %s)", err, truncate(content, 300))
	}
	if finding.SampleIDs == nil && r.PatternID != "" {
		finding.SampleIDs = []string{r.PatternID}
	}
	return &core.AICallResult{
		Finding:     finding,
		UserPrompt:  user,
		RawResponse: content,
		DurationMs:  durationMs,
		Model:       o.cfg.Model,
	}, nil
}

// callOnce sends one chat request, parses the response envelope, and
// returns the content + finish_reason + total wall-clock duration. It
// is a thin wrapper over do() that decodes the OpenAI-shaped response
// envelope; Analyze uses it twice when a truncation auto-retry is
// needed.
func (o *OpenAI) callOnce(ctx context.Context, body chatRequest) (content string, finishReason string, durationMs int64, err error) {
	start := time.Now()
	raw, doErr := o.do(ctx, body)
	durationMs = time.Since(start).Milliseconds()
	if doErr != nil {
		return "", "", durationMs, doErr
	}
	var resp chatResponse
	if jerr := json.Unmarshal(raw, &resp); jerr != nil {
		return "", "", durationMs, fmt.Errorf("ai: decode response: %w", jerr)
	}
	if len(resp.Choices) == 0 {
		return "", "", durationMs, fmt.Errorf("ai: response had no choices")
	}
	content = strings.TrimSpace(resp.Choices[0].Message.Content)
	finishReason = resp.Choices[0].FinishReason
	if content == "" && finishReason != finishContentFilter {
		// Empty content with a stop/length finish_reason is anomalous;
		// content_filter explains itself.
		return "", finishReason, durationMs, fmt.Errorf("ai: empty content from model")
	}
	return content, finishReason, durationMs, nil
}

func (o *OpenAI) do(ctx context.Context, body chatRequest) ([]byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ai: marshal request: %w", err)
	}

	attempts := o.maxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// Ctx already cancelled — give up immediately. Surface
		// lastErr when we have one (it carries useful HTTP detail like
		// "ai: http 502: ...") and the bare ctx error otherwise.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("ai: http: %w", err)
		}

		data, status, err := o.doOnce(ctx, buf)
		if err == nil {
			return data, nil
		}
		lastErr = err

		// Don't retry on non-retryable HTTP statuses (4xx other than
		// 429). The first attempt either succeeds or we surface the
		// error so the operator can fix config (bad API key, bad
		// model name, etc.).
		if !isRetryable(status, err) || attempt == attempts {
			return nil, lastErr
		}
		// Compute exponential backoff with jitter (up to 25%).
		backoff := o.initialBackoff
		for i := 1; i < attempt; i++ {
			backoff *= 2
			if o.maxBackoff > 0 && backoff > o.maxBackoff {
				backoff = o.maxBackoff
				break
			}
		}
		// rand.Int63n requires a positive argument; guard against
		// extremely small backoffs that would round to zero.
		if quarter := int64(backoff / 4); quarter > 0 {
			backoff += time.Duration(rand.Int63n(quarter))
		}

		// Honor ctx cancellation during the retry sleep so SIGTERM
		// is not blocked by an in-flight backoff (worst case 15s
		// across 3 attempts at default settings before this fix).
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("ai: http: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

// doOnce performs a single HTTP attempt. Returns the response body
// (nil on error), the HTTP status code (0 when no response was
// received), and any error. The status code lets the caller tell
// retryable failures (429, 5xx) from non-retryable (4xx other).
func (o *OpenAI) doOnce(ctx context.Context, payload []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.chatURL, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("ai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("ai: http: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("ai: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("ai: http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	return data, resp.StatusCode, nil
}

// isRetryable reports whether a failed attempt is worth retrying.
// status==0 means the request never got a response (network/transport
// failure) — usually transient. status==429 (rate limit) and 5xx
// (server-side) are also transient. Anything else (400, 401, 403, 404,
// 422…) is a client error that won't fix itself.
func isRetryable(status int, err error) bool {
	if status == 0 {
		return err != nil
	}
	if status == http.StatusTooManyRequests {
		return true
	}
	if status >= 500 && status < 600 {
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// wire types
// -----------------------------------------------------------------------------

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason,omitempty"`
	} `json:"choices"`
}

// Sentinel finish_reason values surfaced by both OpenAI and Gemini's
// OpenAI-compatible shim. Other providers (Claude via LiteLLM, etc.)
// usually map to these via their adapters.
const (
	// finishLength means the model hit max_tokens before completing.
	// For our JSON-output prompt this means the JSON object is
	// truncated → ParseFinding will fail with "no JSON object found".
	// We auto-retry once with doubled max_tokens to recover.
	finishLength = "length"
	// finishContentFilter means the provider's safety filter blocked
	// the response. Retrying with more tokens will not help — surface
	// a clear error so the operator can adjust the prompt or model.
	finishContentFilter = "content_filter"
	// truncationRetryMaxTokens caps how large the auto-retry will
	// allow max_tokens to grow. Beyond this we give up rather than
	// rack up costs.
	truncationRetryMaxTokens = 4096
)

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func defaultSampleFn(r core.AgentResult) []string {
	out := make([]string, 0, 3)
	for _, s := range r.SampleSignals {
		if s.Message == "" {
			continue
		}
		out = append(out, s.Message)
		if len(out) == 3 {
			break
		}
	}
	return out
}

func floatOr(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func intOr(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
