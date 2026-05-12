package agent

import (
	"net/http"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// AIBundle bundles the AI-side dependencies the worker needs.
// All fields are nil-safe: the worker accepts a zero bundle and falls
// back to "dry detect" (classify but do not emit). Breaker may also be
// nil when the operator disables it via failure_threshold=0.
type AIBundle struct {
	SRE     core.AISRE
	Cache   *ai.ResultCache
	Rate    *ai.RateLimiter
	Breaker *ai.Breaker
}

// BuildAI constructs the AI SRE, result cache, and rate limiter for
// detect mode.
//
// Returns a zero AIBundle when cfg.AI.Enable is false so callers can
// pass it straight to NewWorker without nil checks.
//
// httpClient may be nil — a default *http.Client (30s timeout, no
// proxy) is used. store may be nil — the cache then degrades to
// in-memory only.
func BuildAI(cfg config.AgentConfig, store storage.Provider, httpClient *http.Client) AIBundle {
	if !cfg.AI.Enable {
		return AIBundle{}
	}

	retry := cfg.Reliability.AIRetry
	maxAttempts := retry.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	initialBackoff := parseDurationOr(retry.InitialBackoff, 500*time.Millisecond)
	maxBackoff := parseDurationOr(retry.MaxBackoff, 5*time.Second)
	sre := ai.NewOpenAIWithRetry(cfg.AI, httpClient, maxAttempts, initialBackoff, maxBackoff)

	ttl := parseDurationOr(cfg.AI.CacheTTL, time.Hour)
	cache := ai.NewResultCache(ttl, store)

	rate := ai.NewRateLimiter(cfg.AI.MaxCallsPerHour)

	// Breaker: failure_threshold=0 disables it (always closed).
	// Default 5 failures, 2m cooldown. Latency window 100 (last 100
	// successful calls feed p50/p95).
	bcfg := cfg.Reliability.AIBreaker
	threshold := bcfg.FailureThreshold
	if threshold == 0 {
		threshold = 5
	}
	cooldown := parseDurationOr(bcfg.Cooldown, 2*time.Minute)
	breaker := ai.NewBreaker(threshold, cooldown, 100)

	return AIBundle{
		SRE:     sre,
		Cache:   cache,
		Rate:    rate,
		Breaker: breaker,
	}
}
