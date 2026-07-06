package agent

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/analyze"
	analyzetools "github.com/VersusControl/versus-incident/pkg/agent/ai/analyze/tools"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/runbook"
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
	// Runbooks is the runbook corpus manager shared by the find_runbook
	// read path and the admin runbooks UI (upload/list/delete). Nil when
	// storage is unavailable. Present even without embeddings so operators
	// can manage the corpus before configuring an embedding model.
	Runbooks *runbook.Manager
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
	// Resolve the detect-task config up front so the construction gate can
	// see whether a model is actually configured.
	detectCfg := cfg.AI.Resolve(cfg.AI.Detect)

	// Construct the bundle when AI is enabled at boot, OR when a runtime
	// AISettingsResolver is registered (so an off-at-boot enterprise binary
	// still has an idle bundle the runtime enable flag can switch on). In
	// the resolver case a model must still be configured — otherwise we
	// would build a nil-key client that only errors at call time. OSS
	// registers no resolver, so this collapses to the original
	// `!cfg.AI.Enable` gate and is byte-for-byte unchanged.
	if !cfg.AI.Enable && (aiSettingsResolver() == nil || detectCfg.Model == "") {
		return AIBundle{}
	}

	// Per-request Authorization override backed by the runtime resolver.
	// Nil in OSS (no resolver) so the chat-model transport stays a plain
	// pass-through.
	authKeyFn := aiSettingsKeyFunc()

	// Runtime overrides (provider / enabled / key state) folded into each
	// agent's model-holder rebuild signature. Zero value in OSS (no
	// resolver), so the holder pins the configured provider and builds once.
	aiRT := aiRuntime()

	// Detect-task wiring -----------------------------------------------------
	detectAgent, err := detect.New(context.Background(), detectCfg, detect.Options{
		HTTPClient:  httpClient,
		AuthKeyFunc: authKeyFn,
		Runtime:     aiRT,
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
	var runbookMgr *runbook.Manager
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

		// Optional runbook-RAG seam for the find_runbook tool. When an
		// embedding model is configured (tools.yaml
		// tools.find_runbook.embedding_model), build the embedder, auto-
		// ingest the runbook source dir (incremental — only new/changed
		// runbooks are embedded), load the persisted corpus from storage,
		// and snapshot it into an in-memory vector index. Any failure
		// leaves embedder/searcher nil so analyzetools.Default omits the
		// tool — community installs without embeddings are unaffected.
		// Runbook-RAG corpus manager. Created whenever storage is available
		// so the admin runbooks UI can upload/list/delete runbooks even
		// before an embedding model is configured. When an embedding model
		// IS configured (tools.yaml tools.find_runbook.embedding_model), the
		// manager also embeds the corpus and exposes a live search index, so
		// the find_runbook tool is wired with the manager's embedder +
		// searcher. Uploads atomically rebuild the index, so newly uploaded
		// runbooks are searchable without a restart.
		runbookMgr = buildRunbookManager(cfg, store, httpClient)
		var embedder core.Embedder
		var runbookSearcher analyzetools.RunbookSearcher
		if runbookMgr != nil && runbookMgr.HasEmbedder() {
			embedder = runbookMgr.Embedder()
			runbookSearcher = newRunbookSearcherAdapter(runbookMgr.Index())
		}

		// Optional metric/trace readers for the query_metrics / query_traces
		// tools. Each is configured independently in tools.yaml
		// (tools.query_metrics.prometheus / tools.query_traces.tempo) so an
		// on-demand analyze query never touches a detect-path source cursor.
		// A blank endpoint yields a nil reader so analyzetools.Default omits
		// the tool — community installs without a metric/trace backend are
		// unaffected.
		metrics := newMetricReaderAdapter(cfg.Tools.QueryMetrics.Prometheus)
		traces := newTraceReaderAdapter(cfg.Tools.QueryTraces.Tempo)

		tools := analyzetools.Default(store, newCatalogAdapter(catalog), reader, redactor, serviceMatcher, graph, changes, embedder, runbookSearcher, metrics, traces)
		a, aErr := analyze.New(context.Background(), analyzeBaseCfg, tools, analyze.Options{
			HTTPClient:    httpClient,
			AuthKeyFunc:   authKeyFn,
			Runtime:       aiRT,
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
		Runbooks:    runbookMgr,
	}
}

// buildRunbookManager builds the runbook corpus manager shared by the
// find_runbook read path and the admin runbooks UI. It returns nil only
// when storage is unavailable (an in-memory-only corpus would not
// survive a restart, so runbook management is disabled).
//
// When an embedding model is configured (tools.find_runbook.embedding_
// model) it builds the embedder and the manager auto-ingests the runbook
// source dir (incremental — only new or edited runbooks are embedded),
// so the find_runbook tool gets a live, searchable corpus. When no
// embedding model is configured the manager still loads the corpus so
// operators can upload/list/delete runbooks; those runbooks become
// searchable once an embedding model is set and the corpus re-ingested.
func buildRunbookManager(cfg config.AgentConfig, store storage.Provider, httpClient *http.Client) *runbook.Manager {
	if store == nil {
		log.Printf("agent: runbooks disabled: no storage backend for runbook corpus")
		return nil
	}

	rbStore, err := runbook.LoadStore(store)
	if err != nil {
		log.Printf("agent: runbooks disabled: load runbook corpus failed: %v", err)
		return nil
	}

	var embedder core.Embedder
	embCfg := cfg.Tools.FindRunbook
	if embCfg.EmbeddingModel != "" {
		e, embErr := einowrap.NewEmbedder(context.Background(), config.AgentAIConfig{
			Provider: cfg.AI.Provider,
			Model:    embCfg.EmbeddingModel,
			APIKey:   cfg.AI.APIKey,
		}, einowrap.Options{
			HTTPClient: httpClient,
		})
		if embErr != nil {
			log.Printf("agent: find_runbook disabled: embedder init failed: %v", embErr)
		} else {
			embedder = e
		}
	}

	mgr := runbook.NewManager(rbStore, embedder)

	// Auto-ingest the runbook source dir so operators never run a separate
	// CLI. Ingestion is incremental — unchanged runbooks reuse their cached
	// vector, so a reboot with no edits makes no embedding calls. A no-op
	// when no embedder is configured. Non-fatal: we still serve the
	// previously-persisted corpus on failure.
	dir := filepath.Join(storage.DefaultDataDir, runbook.SourceSubdir)
	if n, ingErr := mgr.IngestDir(context.Background(), dir, ""); ingErr != nil {
		log.Printf("agent: find_runbook: runbook ingest failed: %v (serving previously-persisted corpus)", ingErr)
	} else if n > 0 {
		log.Printf("agent: find_runbook: ingested %d runbook(s) from %s", n, dir)
	}

	if embedder != nil {
		log.Printf("agent: find_runbook enabled model=%s runbooks=%d", embCfg.EmbeddingModel, rbStore.Len())
	} else {
		log.Printf("agent: runbooks UI enabled (no embedding model; uploads not searchable until configured) runbooks=%d", rbStore.Len())
	}
	return mgr
}
