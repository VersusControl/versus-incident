package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/analyze"
	chatagent "github.com/VersusControl/versus-incident/pkg/agent/ai/chat"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	versustools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/versus"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/runbook"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// AIBundle bundles every AI-side dependency. All fields are nil-safe:
// when AI is disabled the worker accepts a zero bundle and emits a
// deterministic templated alert instead of enriching via AI.
//
// Router exposes the typed task dispatcher to non-worker consumers
// (admin endpoints, future analyze controller). The worker keeps using
// Detect + Cache + Rate directly so its per-outcome logging
// (emitted / cached / emitted_basic*) stays explicit.
type AIBundle struct {
	Router      *router.Router
	Detect      core.AIAgent // kind=AITaskDetect
	Analyze     core.AIAgent // kind=AITaskAnalyze, built when AI.Enable is true
	Chat        core.ChatTurnAgent
	Cache       *ai.ResultCache
	Rate        *ai.RateLimiter
	AnalyzeRate *ai.RateLimiter // separate hourly cap for analyze
	ChatRate    *ai.RateLimiter
	// ChatService returns an org-scoped durable service. Nil when chat is unavailable.
	ChatService func(scope tenancy.OrgScope) *chatagent.Service
	// Runbooks is the runbook corpus manager shared by the find_runbook
	// read path and the admin runbooks UI (upload/list/delete). Nil when
	// storage is unavailable. Present even without embeddings so operators
	// can manage the corpus before configuring an embedding model.
	Runbooks            *runbook.Manager
	ToolSettings        *aitools.Manager
	ToolSnapshot        func(tenancy.OrgScope) aitools.Snapshot
	ObserveSourceHealth func(string, error, time.Time)
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
	return buildAIs(cfg, catalog, store, tenancy.DefaultOrgScope(), httpClient, nil)
}

// BuildAIsForScope constructs every AI dependency with an ordered organization
// read scope. Writes remain owned by the supplied storage provider; the scope
// applies only to read-only analyze tools. BuildAIs supplies the default-only
// scope used by single-tenant OSS deployments.
func BuildAIsForScope(cfg config.AgentConfig, catalog *Catalog, store storage.Provider, scope tenancy.OrgScope, httpClient *http.Client) AIBundle {
	return buildAIs(cfg, catalog, store, scope.Normalized(), httpClient, nil)
}

// BuildAIsForScopeWithChatLocation constructs scoped AI dependencies and uses
// locationProvider to resolve chat date phrases. A nil provider preserves the
// OSS behavior of loading report settings from store.
func BuildAIsForScopeWithChatLocation(cfg config.AgentConfig, catalog *Catalog, store storage.Provider, scope tenancy.OrgScope, httpClient *http.Client, locationProvider func() *time.Location) AIBundle {
	return buildAIs(cfg, catalog, store, scope.Normalized(), httpClient, locationProvider)
}

func buildAIs(cfg config.AgentConfig, catalog *Catalog, store storage.Provider, scope tenancy.OrgScope, httpClient *http.Client, locationProvider func() *time.Location) AIBundle {
	toolSettings := aitools.NewManager(store)
	configuredToolSnapshot := configuredToolAvailabilitySnapshot(cfg, store)
	toolSnapshot := func(tenancy.OrgScope) aitools.Snapshot { return configuredToolSnapshot }
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
		return AIBundle{ToolSettings: toolSettings, ToolSnapshot: toolSnapshot}
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
	var analyzeTools []core.Tool
	var chatTools []core.Tool
	var runtimeTools []core.Tool
	var runbookMgr *runbook.Manager
	var detectionHealth *detectionHealthAdapter
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
		detectionHealth = newDetectionHealthAdapter(scope, cfg.Sources, readerSources, srcErrs)
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
		changes := commontools.NewGitChangeFeed(buildGitRepos(cfg.Tools.RecentChanges.Git))

		// Optional runbook-RAG seam for the find_runbook tool. When an
		// embedding model is configured (tools.yaml
		// tools.find_runbook.embedding_model), build the embedder, auto-
		// ingest the runbook source dir (incremental — only new/changed
		// runbooks are embedded), load the persisted corpus from storage,
		// and snapshot it into an in-memory vector index. Any failure
		// leaves embedder/searcher nil so buildAnalyzeTools omits the
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
		var runbookSearcher commontools.RunbookSearcher
		if runbookMgr != nil && runbookMgr.HasEmbedder() {
			embedder = runbookMgr.Embedder()
			runbookSearcher = newRunbookSearcherAdapter(runbookMgr.Index())
		}

		// Optional metric/trace readers for the query_metrics / query_traces
		// tools. Each is configured independently in tools.yaml
		// (tools.query_metrics.prometheus / tools.query_traces.tempo) so an
		// on-demand analyze query never touches a detect-path source cursor.
		// A blank endpoint yields a nil reader so buildAnalyzeTools omits
		// the tool — community installs without a metric/trace backend are
		// unaffected.
		metrics := newMetricReaderAdapter(cfg.Tools.QueryMetrics.Prometheus)
		traces := newTraceReaderAdapter(cfg.Tools.QueryTraces.Tempo)

		runtimeTools = buildAnalyzeTools(store, scope, newCatalogAdapterWithThreshold(catalog, cfg.Catalog.AutoPromoteAfter), reader, redactor, serviceMatcher, graph, changes, embedder, runbookSearcher, metrics, traces, detectionHealth)
		toolSnapshot = func(requestScope tenancy.OrgScope) aitools.Snapshot {
			return buildToolAvailabilitySnapshot(configuredToolSnapshot, reader, graph, changes, embedder, runbookSearcher, metrics, traces, detectionHealth.DetectionHealth(requestScope))
		}
		initialView, loadErr := toolSettings.Load(scope)
		if loadErr != nil {
			log.Printf("agent: tool settings unavailable: %v", loadErr)
			return AIBundle{ToolSettings: toolSettings, ToolSnapshot: toolSnapshot, ObserveSourceHealth: detectionHealth.Observe}
		}
		initialSnapshot := toolSnapshot(scope)
		analyzeTools, err = initialView.Filter(aitools.AgentAnalyze, runtimeTools, initialSnapshot)
		if err != nil {
			log.Printf("agent: analyze tool settings unavailable: %v", err)
			return AIBundle{ToolSettings: toolSettings, ToolSnapshot: toolSnapshot, ObserveSourceHealth: detectionHealth.Observe}
		}
		chatTools, err = initialView.Filter(aitools.AgentChat, runtimeTools, initialSnapshot)
		if err != nil {
			log.Printf("agent: chat tool settings unavailable: %v", err)
			return AIBundle{ToolSettings: toolSettings, ToolSnapshot: toolSnapshot, ObserveSourceHealth: detectionHealth.Observe}
		}
		analyzeGeneration := newToolGeneration(toolSettings, scope, toolSnapshot)
		analyzeRuntime := aiRT
		analyzeRuntime.Revision = analyzeGeneration.Revision
		a, aErr := analyze.New(context.Background(), analyzeBaseCfg, runtimeTools, analyze.Options{
			HTTPClient:  httpClient,
			AuthKeyFunc: authKeyFn,
			Runtime:     analyzeRuntime,
			ToolProvider: func() ([]core.Tool, error) {
				return analyzeGeneration.Filter(aitools.AgentAnalyze, runtimeTools)
			},
			ToolTimeout:   parseDurationOr(cfg.Tools.ToolTimeout, 20*time.Second),
			ParallelTools: cfg.Tools.ParallelTools,
		})
		if aErr != nil {
			log.Printf("agent: analyze agent disabled: %v", aErr)
		} else {
			analyzeAgent = a
			analyzeRate = ai.NewRateLimiter(analyzeBaseCfg.MaxCallsPerHour)
			log.Printf("agent: analyze agent enabled model=%s tools=%d",
				analyzeBaseCfg.Model, len(analyzeTools))
		}
	}

	// Chat-task wiring -------------------------------------------------------
	// Chat reuses the read-only tool catalog but owns an independent ADK agent,
	// prompt, result contract, and rate limiter.
	var chatAgent core.ChatTurnAgent
	var concreteChat *chatagent.Agent
	var chatRate *ai.RateLimiter
	chatCfg := cfg.AI.Resolve(cfg.AI.Chat)
	chatGeneration := newToolGeneration(toolSettings, scope, toolSnapshot)
	chatRuntime := aiRT
	chatRuntime.Revision = chatGeneration.Revision
	if built, chatErr := chatagent.New(context.Background(), chatCfg, runtimeTools, chatagent.Options{
		HTTPClient: httpClient, AuthKeyFunc: authKeyFn, Runtime: chatRuntime,
		ToolProvider: func() ([]core.Tool, error) {
			return chatGeneration.Filter(aitools.AgentChat, runtimeTools)
		},
		SeedProvider: func() ([]core.Tool, error) {
			return loadCurrentTools(toolSettings, scope, toolSnapshot, aitools.AgentChat, runtimeTools)
		},
		ToolTimeout: parseDurationOr(cfg.Tools.ToolTimeout, chatagent.DefaultToolTimeout),
	}); chatErr != nil {
		log.Printf("agent: chat agent disabled: %v", chatErr)
	} else {
		concreteChat = built
		chatAgent = built
		chatRate = ai.NewDistributedRateLimiter(chatCfg.MaxCallsPerHour, store, scope.Normalized().Write, time.Now)
		log.Printf("agent: chat agent enabled model=%s tools=%d", chatCfg.Model, len(chatTools))
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
	r := router.NewWithChat(entries, router.ChatEntry{Agent: chatAgent, Rate: chatRate})
	var chatServiceFactory func(tenancy.OrgScope) *chatagent.Service
	if concreteChat != nil && store != nil {
		bootScope := scope.Normalized()
		if locationProvider == nil {
			locationProvider = func() *time.Location { return chatagent.LocationFromReportSettings(store) }
		}
		chatServiceFactory = func(serviceScope tenancy.OrgScope) *chatagent.Service {
			// The read-only tool catalog is boot-scoped. Reject a mismatched
			// request scope until tools are constructed per request as well.
			if serviceScope.Normalized().Write != bootScope.Write {
				return nil
			}
			return chatagent.NewServiceWithLocationProvider(
				chatagent.NewSessionStore(store, serviceScope, time.Now), r, concreteChat, time.Now,
				locationProvider,
			)
		}
	}

	return AIBundle{
		Router:              r,
		Detect:              detectAgent,
		Analyze:             analyzeAgent,
		Chat:                chatAgent,
		Cache:               detectCache,
		Rate:                detectRate,
		AnalyzeRate:         analyzeRate,
		ChatRate:            chatRate,
		ChatService:         chatServiceFactory,
		Runbooks:            runbookMgr,
		ToolSettings:        toolSettings,
		ToolSnapshot:        toolSnapshot,
		ObserveSourceHealth: detectionHealth.Observe,
	}
}

func loadCurrentTools(manager *aitools.Manager, scope tenancy.OrgScope, snapshot func(tenancy.OrgScope) aitools.Snapshot, agent aitools.AgentKind, runtime []core.Tool) ([]core.Tool, error) {
	view, err := manager.Load(scope)
	if err != nil {
		return nil, err
	}
	return view.Filter(agent, runtime, snapshot(scope))
}

type toolGeneration struct {
	mu       sync.Mutex
	manager  *aitools.Manager
	scope    tenancy.OrgScope
	snapshot func(tenancy.OrgScope) aitools.Snapshot
	view     aitools.SettingsView
	current  aitools.Snapshot
	err      error
}

func newToolGeneration(manager *aitools.Manager, scope tenancy.OrgScope, snapshot func(tenancy.OrgScope) aitools.Snapshot) *toolGeneration {
	return &toolGeneration{manager: manager, scope: scope, snapshot: snapshot}
}

func (generation *toolGeneration) Revision(context.Context) (string, bool) {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	generation.view, generation.err = generation.manager.Load(generation.scope)
	if generation.err != nil {
		return "", false
	}
	generation.current = generation.snapshot(generation.scope)
	encoded, err := json.Marshal(generation.current)
	if err != nil {
		generation.err = err
		return "", false
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%s:%x", generation.view.Revision(), sum[:]), true
}

func (generation *toolGeneration) Filter(agent aitools.AgentKind, runtime []core.Tool) ([]core.Tool, error) {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	if generation.err != nil {
		return nil, generation.err
	}
	return generation.view.Filter(agent, runtime, generation.current)
}

func configuredToolAvailabilitySnapshot(cfg config.AgentConfig, store storage.Provider) aitools.Snapshot {
	configuredSignals := make(map[signalsources.Kind]bool)
	for _, source := range cfg.Sources {
		if source.Enable {
			configuredSignals[signalsources.KindOf(source.Type)] = true
		}
	}
	hasGit := len(cfg.Tools.RecentChanges.Git.Repos) > 0
	hasGraph := len(cfg.Tools.DescribeDependencies.Services) > 0
	hasEmbedder := strings.TrimSpace(cfg.Tools.FindRunbook.EmbeddingModel) != ""
	configured := func(ok bool, name string) aitools.DependencyStatus {
		return aitools.DependencyStatus{Configured: ok, Healthy: ok, Name: name}
	}
	metrics := configured(configuredSignals[signalsources.KindMetrics] || strings.TrimSpace(cfg.Tools.QueryMetrics.Prometheus.Address) != "", "Metric data source")
	metrics.Constructed = newMetricReaderAdapter(cfg.Tools.QueryMetrics.Prometheus) != nil
	if metrics.Configured && !metrics.Constructed {
		metrics.Healthy = false
		metrics.Health = "configuration"
	}
	traces := configured(configuredSignals[signalsources.KindTraces] || strings.TrimSpace(cfg.Tools.QueryTraces.Tempo.Address) != "", "Trace data source")
	traces.Constructed = newTraceReaderAdapter(cfg.Tools.QueryTraces.Tempo) != nil
	if traces.Configured && !traces.Constructed {
		traces.Healthy = false
		traces.Health = "configuration"
	}
	return aitools.Snapshot{
		DataSources: map[string]aitools.DependencyStatus{
			"logs":    configured(configuredSignals[signalsources.KindLogs], "Log data source"),
			"metrics": metrics,
			"traces":  traces,
		},
		Integrations: map[string]aitools.DependencyStatus{"github": configured(hasGit, "GitHub"), "kubernetes": configured(false, "Kubernetes cluster")},
		Capabilities: map[string]aitools.DependencyStatus{
			"ai_embedder": configured(hasEmbedder, "AI embedder"), "runbook_index": configured(hasEmbedder && store != nil, "Runbook index"), "dependency_graph": configured(hasGraph, "Dependency graph"),
		},
	}
}

func buildToolAvailabilitySnapshot(configured aitools.Snapshot, reader commontools.SignalReader, graph *commontools.DependencyGraph, changes commontools.ChangeFeed, embedder core.Embedder, runbooks commontools.RunbookSearcher, metrics commontools.MetricReader, traces commontools.TraceReader, health versustools.DetectionHealthSnapshot) aitools.Snapshot {
	resolved := func(status aitools.DependencyStatus, healthy bool) aitools.DependencyStatus {
		status.Healthy = status.Configured && healthy
		if status.Configured && !status.Healthy {
			status.Health = "configuration"
		}
		return status
	}
	dataSource := func(kind string, status aitools.DependencyStatus, constructed bool) aitools.DependencyStatus {
		healthy, observed, name, class := sourceKindHealth(health, kind)
		status.Constructed = constructed
		status.Healthy = status.Configured && constructed && (!observed || healthy)
		if status.Configured && !constructed {
			status.Health = "configuration"
		} else if status.Configured && observed {
			if name != "" {
				status.Name = name
			}
			status.Health = class
		}
		return status
	}
	return aitools.Snapshot{
		DataSources: map[string]aitools.DependencyStatus{
			"logs": dataSource("logs", configured.DataSources["logs"], reader != nil), "metrics": dataSource("metrics", configured.DataSources["metrics"], metrics != nil), "traces": dataSource("traces", configured.DataSources["traces"], traces != nil),
		},
		Integrations: map[string]aitools.DependencyStatus{
			"github": resolved(configured.Integrations["github"], changes != nil), "kubernetes": resolved(configured.Integrations["kubernetes"], false),
		},
		Capabilities: map[string]aitools.DependencyStatus{
			"ai_embedder": resolved(configured.Capabilities["ai_embedder"], embedder != nil), "runbook_index": resolved(configured.Capabilities["runbook_index"], runbooks != nil), "dependency_graph": resolved(configured.Capabilities["dependency_graph"], graph != nil && graph.Len() > 0),
		},
	}
}

func sourceKindHealth(snapshot versustools.DetectionHealthSnapshot, kind string) (bool, bool, string, string) {
	configured := 0
	failed := make([]versustools.SourceHealth, 0)
	for _, source := range snapshot.Sources {
		if source.Kind != kind || !source.Configured {
			continue
		}
		configured++
		if source.Observation == "unhealthy" {
			failed = append(failed, source)
			continue
		}
		if source.Observation == "healthy" || source.Observation == "unknown" || source.Observation == "" {
			return true, source.Observation == "healthy", "", ""
		}
	}
	if configured == 0 || len(failed) != configured {
		return false, false, "", ""
	}
	slices.SortFunc(failed, func(left, right versustools.SourceHealth) int {
		if result := strings.Compare(left.Name, right.Name); result != 0 {
			return result
		}
		return strings.Compare(left.ErrorClass, right.ErrorClass)
	})
	return false, true, boundAvailabilityText(failed[0].Name, 80), boundAvailabilityText(failed[0].ErrorClass, 40)
}

func boundAvailabilityText(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func buildAnalyzeTools(store storage.Provider, scope tenancy.OrgScope, catalog versustools.PatternCatalog, reader commontools.SignalReader, redactor commontools.LineRedactor, services commontools.ServiceExtractor, graph *commontools.DependencyGraph, changes commontools.ChangeFeed, embedder core.Embedder, runbooks commontools.RunbookSearcher, metrics commontools.MetricReader, traces commontools.TraceReader, health versustools.DetectionHealthReader) []core.Tool {
	scope = scope.Normalized()
	providers := chatKnowledgeProviders()
	tools := make([]core.Tool, 0, 18)
	if store != nil {
		tools = append(tools,
			versustools.GetIncident{Store: store, Scope: scope, Redactor: redactor},
		)
	}
	if catalog != nil {
		tools = append(tools,
			versustools.GetPattern{Catalog: catalog, Redactor: redactor},
			versustools.GetService{Catalog: catalog, Redactor: redactor, Reliability: providers.ServiceReliability, Scope: scope},
		)
	}
	if store != nil && catalog != nil {
		if paged, ok := catalog.(versustools.PagedPatternCatalog); ok {
			tools = append(tools,
				versustools.GetSystemOverview{Store: store, Scope: scope, Catalog: paged, Health: health},
				versustools.ListServices{Store: store, Scope: scope, Catalog: paged},
			)
		}
	}
	if health != nil {
		tools = append(tools, versustools.GetDetectionHealth{Reader: health, Scope: scope})
	}
	tools = append(tools,
		versustools.ListCapabilities{Capabilities: knowledgeCapabilities(scope, store, catalog, reader, graph, changes, embedder, runbooks, metrics, traces, health, providers)},
		versustools.GetAlertDecision{Provider: providers.AlertDecision, Scope: scope, Redactor: redactor},
	)
	if store != nil {
		tools = append(tools, versustools.SearchIncidents{Store: store, Scope: scope, Redactor: redactor})
	}
	if catalog != nil {
		if paged, ok := catalog.(versustools.PagedPatternCatalog); ok {
			tools = append(tools, versustools.ListPatterns{Catalog: paged, Redactor: redactor})
		}
	}
	if store != nil {
		tools = append(tools, versustools.ListAnalyses{Store: store, Scope: scope, Redactor: redactor})
	}
	if reader != nil {
		tools = append(tools, commontools.RelatedLogs{Reader: reader, Redactor: redactor, Services: services})
	}
	if graph != nil && graph.Len() > 0 {
		tools = append(tools, commontools.DescribeDependencies{Graph: graph, Store: store, Scope: scope})
	}
	if changes != nil {
		tools = append(tools, commontools.RecentChanges{Feed: changes})
	}
	if embedder != nil && runbooks != nil {
		tools = append(tools, commontools.FindRunbook{Embedder: embedder, Index: runbooks, Redactor: redactor})
	}
	if metrics != nil {
		tools = append(tools, commontools.QueryMetrics{Reader: metrics})
	}
	if traces != nil {
		tools = append(tools, commontools.QueryTraces{Reader: traces, Redactor: redactor})
	}
	return tools
}

func knowledgeCapabilities(scope tenancy.OrgScope, store storage.Provider, catalog versustools.PatternCatalog, reader commontools.SignalReader, graph *commontools.DependencyGraph, changes commontools.ChangeFeed, embedder core.Embedder, runbooks commontools.RunbookSearcher, metrics commontools.MetricReader, traces commontools.TraceReader, health versustools.DetectionHealthReader, providers ChatKnowledgeProviders) []versustools.CapabilityStatus {
	status := func(name string, configured bool, setup string) versustools.CapabilityStatus {
		available := versustools.CapabilityStatusFalse
		reason := "not configured"
		if configured {
			available = versustools.CapabilityStatusTrue
			reason = "configured"
			setup = ""
		}
		return versustools.CapabilityStatus{Name: name, Configured: configured, Licensed: versustools.CapabilityStatusTrue, Available: available, Reason: reason, SetupAction: setup}
	}
	capabilities := []versustools.CapabilityStatus{
		status("incidents", store != nil, "Configure an incident storage provider."),
		status("catalog", catalog != nil, "Configure a pattern catalog."),
		status("source_health", health != nil, "Configure detection source health reporting."),
		status("logs", reader != nil, "Configure at least one log signal source."),
		status("metrics", metrics != nil, "Configure a metrics query source."),
		status("traces", traces != nil, "Configure a trace query source."),
		{Name: "service_reliability", Group: "reliability", Licensed: versustools.CapabilityStatusUnknown, Available: versustools.CapabilityStatusUnknown, Reason: "status not reported", SetupAction: "Enable and configure a service reliability provider."},
		{Name: "alert_decisions", Group: "decisions", Licensed: versustools.CapabilityStatusUnknown, Available: versustools.CapabilityStatusUnknown, Reason: "status not reported", SetupAction: "Enable and configure an alert decision provider."},
		status("kubernetes", false, "Configure Kubernetes discovery for this deployment."),
		status("git_changes", changes != nil, "Configure a Git change feed and repositories."),
		status("runbooks", embedder != nil && runbooks != nil, "Configure runbook storage and an embedding model."),
		status("dependencies", graph != nil && graph.Len() > 0, "Configure the service dependency graph."),
	}
	if providers.CapabilityStatus != nil {
		capabilities = mergeCapabilityStatuses(capabilities, providers.CapabilityStatus.CapabilityStatuses(scope.Normalized()))
	}
	return capabilities
}

func mergeCapabilityStatuses(base, reported []versustools.CapabilityStatus) []versustools.CapabilityStatus {
	indexes := make(map[string]int, len(base))
	for index := range base {
		indexes[base[index].Name] = index
	}
	for _, capability := range reported {
		index, ok := indexes[capability.Name]
		if !ok {
			continue
		}
		switch capability.Name {
		case "incidents", "catalog", "source_health":
			continue
		}
		capability.Name = base[index].Name
		capability.Licensed = normalizeCapabilityState(capability.Licensed)
		capability.Available = normalizeCapabilityState(capability.Available)
		capability.Reason = boundCapabilityText(capability.Reason)
		capability.Observation = boundCapabilityText(capability.Observation)
		capability.SetupAction = boundCapabilityText(capability.SetupAction)
		base[index] = capability
	}
	return base
}

func normalizeCapabilityState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case versustools.CapabilityStatusTrue:
		return versustools.CapabilityStatusTrue
	case versustools.CapabilityStatusFalse:
		return versustools.CapabilityStatusFalse
	default:
		return versustools.CapabilityStatusUnknown
	}
}

func boundCapabilityText(value string) string {
	const maxCapabilityText = 240
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maxCapabilityText {
		value = string(runes[:maxCapabilityText])
	}
	return value
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
