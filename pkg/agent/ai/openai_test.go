package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

func withHandler(t *testing.T, handler http.HandlerFunc) (*OpenAI, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Use BaseURL to point straight at the test server — no transport
	// rewriting needed. This also exercises the BaseURL config path.
	o := NewOpenAIWithRetry(
		config.AgentAIConfig{Model: "test-model", APIKey: "k", BaseURL: srv.URL},
		&http.Client{Timeout: 2 * time.Second},
		3,
		1*time.Millisecond,
		2*time.Millisecond,
	)
	return o, srv
}

func goodResponse() string {
	// Minimum JSON for ParseFinding — title/severity present, JSON
	// well-formed (the body is a JSON object containing the fields
	// ParseFinding looks for).
	return `{"choices":[{"message":{"content":"{\"verdict\":\"unknown\",\"title\":\"t\",\"severity\":\"warn\",\"category\":\"infra\",\"confidence\":0.5,\"summary\":\"s\",\"suggested_actions\":[]}"}}]}`
}

// truncatedResponse returns a chat envelope whose content is a JSON
// object missing its closing brace — simulating Gemini-style
// truncation when the model hits max_tokens mid-output. The envelope
// itself is valid; ParseFinding will fail on the content.
func truncatedResponse() string {
	return `{"choices":[{"finish_reason":"length","message":{"content":"{\"title\":\"truncated mid output, no closing brace\",\"summary\":\"this response was cut o"}}]}`
}

func goodResponseWithFinishStop() string {
	return `{"choices":[{"finish_reason":"stop","message":{"content":"{\"verdict\":\"unknown\",\"title\":\"t\",\"severity\":\"warn\",\"category\":\"infra\",\"confidence\":0.5,\"summary\":\"s\",\"suggested_actions\":[]}"}}]}`
}

func contentFilterResponse() string {
	return `{"choices":[{"finish_reason":"content_filter","message":{"content":""}}]}`
}

func sampleResult() core.AgentResult {
	return core.AgentResult{
		Verdict:   core.VerdictUnknown,
		PatternID: "p1",
		Template:  "t",
		SampleSignals: []core.Signal{
			{Message: "boom"},
		},
		Frequency: 1,
	}
}

// TestOpenAI_TruncationAutoRetry verifies that a finish_reason="length"
// triggers exactly one auto-retry with doubled max_tokens. The first
// call returns a truncated JSON object; the retry returns the full
// object. Analyze should succeed using the retry's content.
func TestOpenAI_TruncationAutoRetry(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// First call: model says it ran out of tokens.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(truncatedResponse()))
			return
		}
		// Retry: full response.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(goodResponseWithFinishStop()))
	})

	res, err := o.Analyze(context.Background(), sampleResult())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.Finding == nil {
		t.Fatal("nil result/finding after auto-retry")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 calls (1 truncated + 1 retry), got %d", got)
	}
}

// TestOpenAI_TruncationRetryFailsWithClearError covers the case where
// the retry STILL returns truncated content. The error should
// explicitly mention max_tokens so operators know the fix.
func TestOpenAI_TruncationRetryFailsWithClearError(t *testing.T) {
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(truncatedResponse()))
	})

	_, err := o.Analyze(context.Background(), sampleResult())
	if err == nil {
		t.Fatal("expected error when both attempts truncate")
	}
	if !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("error should mention truncation+max_tokens; got: %v", err)
	}
}

// TestOpenAI_ContentFilterErrorIsClear verifies that finish_reason=
// content_filter does NOT retry (more tokens won't unblock a safety
// filter) and surfaces a recognizable error.
func TestOpenAI_ContentFilterErrorIsClear(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(contentFilterResponse()))
	})

	_, err := o.Analyze(context.Background(), sampleResult())
	if err == nil {
		t.Fatal("expected error on content_filter")
	}
	if !strings.Contains(err.Error(), "safety filter") {
		t.Fatalf("error should mention safety filter; got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("content_filter must not auto-retry; calls=%d want 1", got)
	}
}

// TestOpenAI_BaseURLDefault verifies that omitting BaseURL falls back
// to the OpenAI endpoint. We don't actually call OpenAI; we just
// check the resolved URL on the struct.
func TestOpenAI_BaseURLDefault(t *testing.T) {
	o := NewOpenAI(config.AgentAIConfig{Model: "m", APIKey: "k"}, nil)
	if o.chatURL != defaultOpenAIChatURL {
		t.Fatalf("default base url not honored: got %q want %q", o.chatURL, defaultOpenAIChatURL)
	}
}

// TestOpenAI_BaseURLConfigured verifies that an explicit BaseURL is
// preserved end-to-end. This is the path operators use to swap in
// Gemini's OpenAI-compatible endpoint or a LiteLLM proxy.
func TestOpenAI_BaseURLConfigured(t *testing.T) {
	custom := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	o := NewOpenAI(config.AgentAIConfig{Model: "gemini-2.0-flash", APIKey: "k", BaseURL: custom}, nil)
	if o.chatURL != custom {
		t.Fatalf("configured base url not honored: got %q want %q", o.chatURL, custom)
	}
}

func TestOpenAI_RetriesOn429(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate"}`))
			return
		}
		_, _ = w.Write([]byte(goodResponse()))
	})

	res, err := o.Analyze(context.Background(), sampleResult())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.Finding == nil {
		t.Fatal("nil result/finding")
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}
}

func TestOpenAI_RetriesOn5xx(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(goodResponse()))
	})

	if _, err := o.Analyze(context.Background(), sampleResult()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("calls=%d want 3", calls)
	}
}

func TestOpenAI_NoRetryOn4xx(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	})

	_, err := o.Analyze(context.Background(), sampleResult())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err=%v want it to mention 401", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls=%d want 1 (no retry on 4xx)", calls)
	}
}

func TestOpenAI_RetryExhausted(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	})

	_, err := o.Analyze(context.Background(), sampleResult())
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("calls=%d want 3 (maxAttempts)", calls)
	}
}

func TestOpenAI_NoRetryWhenAttemptsIs1(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	o := NewOpenAIWithRetry(
		config.AgentAIConfig{Model: "test-model", APIKey: "k", BaseURL: srv.URL},
		&http.Client{Timeout: 2 * time.Second},
		1, 0, 0,
	)

	if _, err := o.Analyze(context.Background(), sampleResult()); err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}
}

// TestOpenAI_CancelDuringSleepReturnsFast verifies the ctx-aware
// retry sleep added in v1.4.1 hardening. Before this fix, an
// in-flight backoff blocked SIGTERM for up to MaxBackoff seconds.
// We use a server that always returns 503 and an unusually large
// backoff so the test would obviously hang without the ctx-aware
// sleep.
func TestOpenAI_CancelDuringSleepReturnsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	o := NewOpenAIWithRetry(
		config.AgentAIConfig{Model: "test-model", APIKey: "k", BaseURL: srv.URL},
		&http.Client{Timeout: 2 * time.Second},
		3,
		2*time.Second, // initial backoff — long enough that a non-ctx-aware sleep would hang
		5*time.Second,
	)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel ~50ms in, during the first retry sleep.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := o.Analyze(ctx, sampleResult())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error after cancel")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Analyze took %s — the retry sleep ignored ctx cancel", elapsed)
	}
}

func TestOpenAI_ContextCancelStopsRetry(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	if _, err := o.Analyze(ctx, sampleResult()); err == nil {
		t.Fatal("expected error")
	}
	// One attempt may have raced — we just want to verify we didn't burn through all 3.
	if got := atomic.LoadInt32(&calls); got > 1 {
		t.Fatalf("calls=%d want <=1 (context already cancelled)", got)
	}
}
