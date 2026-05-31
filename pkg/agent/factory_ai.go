package agent

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/analyze"
	analyzetools "github.com/VersusControl/versus-incident/pkg/agent/ai/analyze/tools"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// AIBundle bundles every AI-side dependency. All fields are nil-safe:
// when AI is disabled the worker accepts a zero bundle and falls back
// to "dry detect" (classify, log, do not emit).
//
// Router exposes the typed task dispatcher to non-worker consumers
// (admin endpoints, future analyze controller). The worker keeps using
// Detect + Cache + Rate directly so its per-outcome logging
// (dry / cache / quota / ai_error / emitted) stays explicit.
type AIBundle struct {
	Router      *router.Router
	Detect      core.AIAgent // kind=AITaskDetect
	Analyze     core.AIAgent // kind=AITaskAnalyze, built when AI.Enable is true
	Cache       *ai.ResultCache
	Rate        *ai.RateLimiter
	AnalyzeRate *ai.RateLimiter // separate hourly cap for analyze
}

// BuildAIs constructs every AI dependency (router, detect agent,
// optional analyze agent with its tool catalog, per-task cache, per-
// task rate limiter) from the agent config.
//
// Returns a zero AIBundle when cfg.AI.Enable is false so callers can
// pass the result straight to NewWorker without nil checks.
//
// httpClient may be nil — a default *http.Client is used by the chat
// model. store may be nil — caches degrade to in-memory only; the
// analyze agent's tool registry will also be smaller.
func BuildAIs(cfg config.AgentConfig, catalog *Catalog, store storage.Provider, httpClient *http.Client) AIBundle {
	if !cfg.AI.Enable {
		return AIBundle{}
	}

	// Detect-task wiring -----------------------------------------------------
	detectCfg := cfg.AI.Resolve(cfg.AI.Detect)
	detectAgent, err := detect.New(context.Background(), detectCfg, detect.Options{
		HTTPClient: httpClient,
	})
	if err != nil {
		log.Printf("agent: detect agent disabled: %v", err)
		return AIBundle{}
	}

	detectCache := ai.NewResultCache(parseDurationOr(detectCfg.CacheTTL, time.Hour), store)
	detectRate := ai.NewRateLimiter(detectCfg.MaxCallsPerHour)

	// Analyze-task wiring ----------------------------------------------------
	// Built whenever AI is enabled. Analyze is a tool-using path that
	// costs more per call than detect, so it gets its own rate limiter,
	// but it shares the AI.Enable master switch — no separate opt-in.
	var analyzeAgent core.AIAgent
	var analyzeRate *ai.RateLimiter
	{
		analyzeBaseCfg := cfg.AI.Resolve(config.AgentAITaskConfig{Model: cfg.AI.Analyze.Model})

		// Independent source set + redactor for the read-only
		// get_related_logs tool. Built separately from the worker's
		// sources so pulling logs during an analysis never advances the
		// worker's polling cursors. A nil reader simply omits the tool.
		readerSources, srcErrs := BuildSources(cfg)
		for _, e := range srcErrs {
			log.Printf("agent: analyze reader source warning: %v", e)
		}
		reader := newSignalReaderAdapter(readerSources)
		redactor, redactErrs := NewRedactor(cfg.Redaction.Enable && cfg.Redaction.RedactIPs, cfg.Redaction.ExtraPatterns)
		for _, e := range redactErrs {
			log.Printf("agent: analyze reader redactor warning: %v", e)
		}
		serviceMatcher, svcErrs := NewServiceMatcher(cfg.ServicePatterns)
		for _, e := range svcErrs {
			log.Printf("agent: analyze reader service_patterns warning: %v", e)
		}

		// Optional service-dependency graph for the describe_dependencies
		// tool. Built from the operator-authored upstream edges in
		// tools.yaml (tools.describe_dependencies.services); a nil/empty
		// graph omits the tool.
		graph := buildDependencyGraph(cfg.Tools.DescribeDependencies.Services)

		// Optional git-backed change feed for the recent_changes tool. It
		// mirror-clones each configured remote git repository into a local
		// cache and reads its commit history, configured via tools.yaml
		// (tools.recent_changes.git.repos). An empty repos list leaves the
		// feed nil so the tool is omitted; the `git` binary must be on PATH
		// when configured.
		changes := analyzetools.NewGitChangeFeed(buildGitRepos(cfg.Tools.RecentChanges.Git))

		tools := analyzetools.Default(store, newCatalogAdapter(catalog), reader, redactor, serviceMatcher, graph, changes)
		a, aErr := analyze.New(context.Background(), analyzeBaseCfg, tools, analyze.Options{
			HTTPClient:    httpClient,
			ToolTimeout:   parseDurationOr(cfg.Tools.ToolTimeout, 20*time.Second),
			ParallelTools: cfg.Tools.ParallelTools,
		})
		if aErr != nil {
			log.Printf("agent: analyze agent disabled: %v", aErr)
		} else {
			analyzeAgent = a
			analyzeRate = ai.NewRateLimiter(analyzeBaseCfg.MaxCallsPerHour)
			log.Printf("agent: analyze agent enabled model=%s tools=%d",
				analyzeBaseCfg.Model, len(tools))
		}
	}

	// Router wiring ----------------------------------------------------------
	// router.New drops nil-agent entries so callers asking for a kind
	// that wasn't configured get a clean router.ErrNoAgent.
	entries := map[core.AITaskKind]router.Entry{
		core.AITaskDetect: {Agent: detectAgent, Cache: detectCache, Rate: detectRate},
	}
	if analyzeAgent != nil {
		// Analyze cache is empty by design (CacheKey returns ""); the
		// router skips lookups when the task's CacheKey is empty.
		entries[core.AITaskAnalyze] = router.Entry{Agent: analyzeAgent, Cache: nil, Rate: analyzeRate}
	}
	r := router.New(entries)

	return AIBundle{
		Router:      r,
		Detect:      detectAgent,
		Analyze:     analyzeAgent,
		Cache:       detectCache,
		Rate:        detectRate,
		AnalyzeRate: analyzeRate,
	}
}
