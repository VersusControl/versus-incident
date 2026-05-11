package agent

import (
	"net/http"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// AIBundle bundles the three AI-side dependencies the worker needs.
// All three are nil-safe: the worker accepts a zero bundle and falls
// back to "dry detect" (classify but do not emit).
type AIBundle struct {
	SRE   core.AISRE
	Cache *ai.ResultCache
	Rate  *ai.RateLimiter
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

	sre := ai.NewOpenAI(cfg.AI, httpClient)

	ttl := parseDurationOr(cfg.AI.CacheTTL, time.Hour)
	cache := ai.NewResultCache(ttl, store)

	rate := ai.NewRateLimiter(cfg.AI.MaxCallsPerHour)

	return AIBundle{
		SRE:   sre,
		Cache: cache,
		Rate:  rate,
	}
}
