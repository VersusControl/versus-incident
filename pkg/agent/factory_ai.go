package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/analyze"
	chatagent "github.com/VersusControl/versus-incident/pkg/agent/ai/chat"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
	"github.com/VersusControl/versus-incident/pkg/agent/ledger"
	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	elasticsearchtools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/elasticsearch"
	graylogtools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/graylog"
	k8stools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/k8s"
	lokitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/loki"
	signoztools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/signoz"
	splunktools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/splunk"
	versustools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/versus"
	"github.com/VersusControl/versus-incident/pkg/baseline"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	elasticsearchapp "github.com/VersusControl/versus-incident/pkg/elasticsearch"
	graylogapp "github.com/VersusControl/versus-incident/pkg/graylog"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	lokiapp "github.com/VersusControl/versus-incident/pkg/loki"
	"github.com/VersusControl/versus-incident/pkg/runbook"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	signozapp "github.com/VersusControl/versus-incident/pkg/signoz"
	splunkapp "github.com/VersusControl/versus-incident/pkg/splunk"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/google/uuid"
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
	ActionService func(scope tenancy.OrgScope) *act.Service
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
	return buildAIs(cfg, catalog, store, tenancy.DefaultOrgScope(), httpClient, nil, nil)
}

// BuildAIsWithKubernetes reuses the connector service already registered for HTTP.
func BuildAIsWithKubernetes(cfg config.AgentConfig, catalog *Catalog, store storage.Provider, httpClient *http.Client, kubernetesService *kubernetes.Service) AIBundle {
	return buildAIs(cfg, catalog, store, tenancy.DefaultOrgScope(), httpClient, nil, kubernetesService)
}

// BuildAIsForScope constructs every AI dependency with an ordered organization
// read scope. Writes remain owned by the supplied storage provider; the scope
// applies only to read-only analyze tools. BuildAIs supplies the default-only
// scope used by single-tenant OSS deployments.
func BuildAIsForScope(cfg config.AgentConfig, catalog *Catalog, store storage.Provider, scope tenancy.OrgScope, httpClient *http.Client) AIBundle {
	return buildAIs(cfg, catalog, store, scope.Normalized(), httpClient, nil, nil)
}

// BuildAIsForScopeWithChatLocation constructs scoped AI dependencies and uses
// locationProvider to resolve chat date phrases. A nil provider preserves the
// OSS behavior of loading report settings from store.
func BuildAIsForScopeWithChatLocation(cfg config.AgentConfig, catalog *Catalog, store storage.Provider, scope tenancy.OrgScope, httpClient *http.Client, locationProvider func() *time.Location) AIBundle {
	return buildAIs(cfg, catalog, store, scope.Normalized(), httpClient, locationProvider, nil)
}

// BuildAIsForScopeWithChatLocationAndKubernetes reuses the connector service
// already registered for HTTP while preserving scoped reads and chat time.
func BuildAIsForScopeWithChatLocationAndKubernetes(cfg config.AgentConfig, catalog *Catalog, store storage.Provider, scope tenancy.OrgScope, httpClient *http.Client, locationProvider func() *time.Location, kubernetesService *kubernetes.Service) AIBundle {
	return buildAIs(cfg, catalog, store, scope.Normalized(), httpClient, locationProvider, kubernetesService)
}

func buildAIs(cfg config.AgentConfig, catalog *Catalog, store storage.Provider, scope tenancy.OrgScope, httpClient *http.Client, locationProvider func() *time.Location, kubernetesService *kubernetes.Service) AIBundle {
	toolSettings := aitools.NewManager(store)
	configuredToolSnapshot := configuredToolAvailabilitySnapshot(cfg, store)
	elasticsearchSources, elasticsearchErrs := buildElasticsearchToolSources(cfg.Sources)
	for _, err := range elasticsearchErrs {
		log.Printf("agent: Elasticsearch tool source warning: %v", err)
	}
	configuredToolSnapshot.DataSources["elasticsearch"] = elasticsearchConstructionStatus(configuredToolSnapshot.DataSources["elasticsearch"], elasticsearchSources, elasticsearchErrs)
	var kubernetesErr error
	if kubernetesService == nil {
		kubernetesService, kubernetesErr = NewKubernetesService(cfg.Tools.Kubernetes, scope)
	}
	if kubernetesErr != nil {
		status := configuredToolSnapshot.Integrations["kubernetes"]
		status.Constructed = false
		status.Healthy = false
		status.Health = "configuration"
		configuredToolSnapshot.Integrations["kubernetes"] = status
	} else if kubernetesService != nil {
		kubernetesService.SetChangeStorage(store)
		status := configuredToolSnapshot.Integrations["kubernetes"]
		status.Configured = true
		status.Constructed = true
		status.Healthy = true
		configuredToolSnapshot.Integrations["kubernetes"] = status
	}
	var actionService *act.Service
	var actionServiceFactory func(tenancy.OrgScope) *act.Service
	actionStatus := aitools.DependencyStatus{Configured: cfg.Tools.Kubernetes.Actions.Enable, Health: "configuration"}
	if cfg.Tools.Kubernetes.Actions.Enable {
		adapters, actionErr := buildKubernetesActionAdapters(cfg.Tools.Kubernetes)
		if actionErr == nil && store != nil && len(adapters) > 0 {
			actionService, actionErr = act.NewService(store, scope.Normalized().Write, ledger.NewBlobWriter(store, scope.Normalized().Write), nil, adapters...)
		} else if actionErr == nil {
			actionErr = errors.New("durable action storage is unavailable")
		}
		if actionErr != nil {
			log.Printf("agent: Kubernetes action actor unavailable: %v", actionErr)
		} else {
			actionStatus.Constructed = true
			actionStatus.Healthy = true
			actionStatus.Health = ""
			bootScope := scope.Normalized()
			actionServiceFactory = func(requestScope tenancy.OrgScope) *act.Service {
				if requestScope.Normalized().Write != bootScope.Write {
					return nil
				}
				return actionService
			}
		}
	}
	if configuredToolSnapshot.Capabilities == nil {
		configuredToolSnapshot.Capabilities = make(map[string]aitools.DependencyStatus)
	}
	configuredToolSnapshot.Capabilities["kubernetes_actions"] = actionStatus
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
		return AIBundle{ToolSettings: toolSettings, ToolSnapshot: toolSnapshot, ActionService: actionServiceFactory}
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
		HTTPClient:     httpClient,
		RuntimeKeyFunc: authKeyFn,
		Runtime:        aiRT,
	})
	if err != nil {
		logAIConstructionFailure("detect", detectCfg, err)
		return AIBundle{ToolSettings: toolSettings, ToolSnapshot: toolSnapshot, ActionService: actionServiceFactory}
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
	var chatRuntimeTools []core.Tool
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
		for _, source := range elasticsearchSources {
			source.Service.SetScrubber(redactor)
		}
		signozSources, signozErrs := buildSigNozToolSources(cfg.Sources, redactor)
		for _, e := range signozErrs {
			log.Printf("agent: SigNoz tool source warning: %v", e)
		}
		lokiSources, lokiErrs := buildLokiToolSources(cfg.Sources, redactor)
		for _, e := range lokiErrs {
			log.Printf("agent: Loki tool source warning: %v", e)
		}
		graylogSources, graylogErrs := buildGraylogToolSources(cfg.Sources, redactor)
		for _, e := range graylogErrs {
			log.Printf("agent: Graylog tool source warning: %v", e)
		}
		splunkSources, splunkErrs := buildSplunkToolSources(cfg.Sources, redactor)
		for _, e := range splunkErrs {
			log.Printf("agent: Splunk tool source warning: %v", e)
		}
		extensionTools, extensionErrs := contributedTools(scope, cfg.Sources, redactor)
		for _, e := range extensionErrs {
			log.Printf("agent: runtime tool contributor warning: %v", e)
		}
		baselineExtensions, baselineErrs := contributedBaselineProviders(scope, cfg.Sources)
		for _, e := range baselineErrs {
			log.Printf("agent: baseline provider contributor warning: %v", e)
		}
		if kubernetesService != nil {
			kubernetesService.SetScrubber(redactor)
		}
		serviceMatcher, svcErrs := NewServiceMatcher(cfg.ServicePatterns)
		for _, e := range svcErrs {
			log.Printf("agent: analyze reader service_patterns warning: %v", e)
		}

		// Optional service-dependency graph for the describe_dependencies
		// tool. Built from the operator-authored upstream edges in
		// tools.yaml (tools.describe_dependencies.services); a nil/empty
		// graph omits the tool.
		graph := BuildDependencyGraph(cfg.Tools.DescribeDependencies.Services)

		// Optional git-backed change feed for the recent_changes tool. It
		// mirror-clones each configured remote git repository into a local
		// cache and reads its commit history, configured via tools.yaml
		// (tools.recent_changes.git.repos). An empty repos list leaves the
		// feed nil so the tool is omitted; the `git` binary must be on PATH
		// when configured.
		gitChanges := commontools.NewGitChangeFeed(buildGitRepos(cfg.Tools.RecentChanges.Git))
		configuredGit := configuredToolSnapshot.Integrations["github"]
		configuredGit.Constructed = gitChanges != nil
		configuredToolSnapshot.Integrations["github"] = configuredGit
		var kubernetesChanges commontools.ChangeFeed
		if kubernetesService != nil && store != nil {
			kubernetesChanges = newKubernetesChangeFeed(kubernetesService)
		}
		changes := mergeChangeFeeds(gitChanges, kubernetesChanges, newActionChangeFeed(actionService))

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
		runbookMgr = buildRunbookManager(cfg, store, scope, httpClient, authKeyFn, aiRT)
		var embedder core.Embedder
		var runbookSearcher commontools.RunbookSearcher
		if runbookMgr != nil && runbookMgr.HasEmbedder() {
			embedder = runbookMgr.Embedder()
			runbookSearcher = newRunbookSearcherAdapter(runbookMgr.Index())
		}

		runtimeTools = buildAnalyzeTools(store, scope, newCatalogAdapterWithThreshold(catalog, cfg.Catalog.AutoPromoteAfter), reader, redactor, serviceMatcher, graph, changes, embedder, runbookSearcher, detectionHealth)
		if baselineTool := buildBaselineTool(newLogBaselineProvider(catalog, scope, cfg.Catalog.AutoPromoteAfter, store), baselineExtensions, scope); baselineTool != nil {
			runtimeTools = append(runtimeTools, baselineTool)
		}
		runtimeTools = append(runtimeTools, elasticsearchtools.New(elasticsearchSources)...)
		runtimeTools = append(runtimeTools, k8stools.New(kubernetesService)...)
		var extensionLogTools, otherExtensions []core.Tool
		for _, candidate := range extensionTools {
			if candidate.Name() == "discover_log_fields" || candidate.Name() == "read_log_records" {
				extensionLogTools = append(extensionLogTools, candidate)
			} else {
				otherExtensions = append(otherExtensions, candidate)
			}
		}
		logTools := combineLogTools(signoztools.New(signozSources), lokitools.New(lokiSources), graylogtools.New(graylogSources), splunktools.New(splunkSources), extensionLogTools)
		runtimeTools = append(runtimeTools, logTools...)
		runtimeTools = append(runtimeTools, otherExtensions...)
		toolSnapshot = func(requestScope tenancy.OrgScope) aitools.Snapshot {
			snapshot := buildToolAvailabilitySnapshot(configuredToolSnapshot, reader, graph, changes, embedder, runbookSearcher, detectionHealth.DetectionHealth(requestScope))
			return aitools.BindRuntimeCapabilities(snapshot, runtimeTools)
		}
		for index, candidate := range runtimeTools {
			if capabilityTool, ok := candidate.(versustools.ListCapabilities); ok {
				capabilityTool.Capabilities = sourceReadCapabilities(capabilityTool.Capabilities, toolSnapshot(scope), runtimeTools)
				runtimeTools[index] = capabilityTool
				break
			}
		}
		initialView, loadErr := toolSettings.LoadToolsets(scope)
		if loadErr != nil {
			log.Printf("agent: tool settings unavailable: %v", loadErr)
			return AIBundle{ToolSettings: toolSettings, ToolSnapshot: toolSnapshot, ObserveSourceHealth: detectionHealth.Observe, ActionService: actionServiceFactory}
		}
		initialSnapshot := toolSnapshot(scope)
		chatRuntimeTools = chatRuntimeToolCatalog(runtimeTools, actionService)
		analyzeTools, err = initialView.Filter(aitools.AgentAnalyze, runtimeTools, initialSnapshot)
		if err != nil {
			log.Printf("agent: analyze tool settings unavailable: %v", err)
			return AIBundle{ToolSettings: toolSettings, ToolSnapshot: toolSnapshot, ObserveSourceHealth: detectionHealth.Observe, ActionService: actionServiceFactory}
		}
		chatTools, err = initialView.Filter(aitools.AgentChat, chatRuntimeTools, initialSnapshot)
		if err != nil {
			log.Printf("agent: chat tool settings unavailable: %v", err)
			return AIBundle{ToolSettings: toolSettings, ToolSnapshot: toolSnapshot, ObserveSourceHealth: detectionHealth.Observe, ActionService: actionServiceFactory}
		}
		analyzeGeneration := newToolGeneration(toolSettings, scope, toolSnapshot)
		analyzeRuntime := aiRT
		analyzeRuntime.Revision = analyzeGeneration.Revision
		a, aErr := analyze.New(context.Background(), analyzeBaseCfg, runtimeTools, analyze.Options{
			HTTPClient:     httpClient,
			RuntimeKeyFunc: authKeyFn,
			Runtime:        analyzeRuntime,
			ToolProvider: func() ([]core.Tool, error) {
				return analyzeGeneration.Filter(aitools.AgentAnalyze, runtimeTools)
			},
			ToolTimeout:   parseDurationOr(cfg.Tools.ToolTimeout, 20*time.Second),
			ParallelTools: cfg.Tools.ParallelTools,
		})
		if aErr != nil {
			logAIConstructionFailure("analyze", analyzeBaseCfg, aErr)
		} else {
			analyzeAgent = &bootScopedAIAgent{delegate: a, scope: scope.Normalized(), ledger: ledger.NewBlobWriter(store, scope.Normalized().Write)}
			analyzeRate = ai.NewRateLimiter(analyzeBaseCfg.MaxCallsPerHour)
			safeModel := einowrap.SafeProviderError(analyzeBaseCfg.Provider, analyzeBaseCfg.Model, nil).Model
			log.Printf("agent: analyze agent enabled model=%s tools=%d",
				safeModel, len(analyzeTools))
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
	if built, chatErr := chatagent.New(context.Background(), chatCfg, chatRuntimeTools, chatagent.Options{
		HTTPClient: httpClient, RuntimeKeyFunc: authKeyFn, Runtime: chatRuntime,
		ToolProvider: func() ([]core.Tool, error) {
			return chatGeneration.Filter(aitools.AgentChat, chatRuntimeTools)
		},
		SeedProvider: func() ([]core.Tool, error) {
			return loadCurrentTools(toolSettings, scope, toolSnapshot, aitools.AgentChat, chatRuntimeTools)
		},
		ToolTimeout: parseDurationOr(cfg.Tools.ToolTimeout, chatagent.DefaultToolTimeout),
	}); chatErr != nil {
		logAIConstructionFailure("chat", chatCfg, chatErr)
	} else {
		concreteChat = built
		chatAgent = built
		chatRate = ai.NewDistributedRateLimiter(chatCfg.MaxCallsPerHour, store, scope.Normalized().Write, time.Now)
		safeModel := einowrap.SafeProviderError(chatCfg.Provider, chatCfg.Model, nil).Model
		log.Printf("agent: chat agent enabled model=%s tools=%d", safeModel, len(chatTools))
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
			return chatagent.NewServiceWithLocationProviderAndContextDecorator(
				chatagent.NewSessionStore(store, serviceScope, time.Now), r, concreteChat, time.Now,
				locationProvider,
				func(ctx context.Context) context.Context {
					return DecorateAIContext(ctx, serviceScope)
				},
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
		ActionService:       actionServiceFactory,
		Runbooks:            runbookMgr,
		ToolSettings:        toolSettings,
		ToolSnapshot:        toolSnapshot,
		ObserveSourceHealth: detectionHealth.Observe,
	}
}

func buildBaselineTool(baseProvider core.BaselineProvider, extensions []baseline.Surface, scope tenancy.OrgScope) core.Tool {
	if baseProvider == nil && len(extensions) == 0 {
		return nil
	}
	provider := baseline.NewManager(baseline.Surface{Family: "logs", SourceType: "catalog", Provider: baseProvider}, extensions...)
	return commontools.DescribeBaseline{Provider: provider, OrgID: scope.Normalized().Write}
}

func logAIConstructionFailure(task string, cfg config.AgentAIConfig, cause error) {
	var configErr *einowrap.ConfigError
	if errors.As(cause, &configErr) {
		log.Printf("agent: %s agent disabled: configuration error: %v", task, configErr)
		return
	}
	safe := einowrap.SafeProviderError(cfg.Provider, cfg.Model, cause)
	log.Printf("agent: %s agent disabled: provider=%q model=%q class=%q", task, safe.Provider, safe.Model, safe.Class)
}

type bootScopedAIAgent struct {
	delegate core.AIAgent
	scope    tenancy.OrgScope
	ledger   ledger.Writer
}

func (agent *bootScopedAIAgent) Name() string { return agent.delegate.Name() }

func (agent *bootScopedAIAgent) Kind() core.AITaskKind { return agent.delegate.Kind() }

func (agent *bootScopedAIAgent) Run(ctx context.Context, task core.AITask) (*core.AICallResult, error) {
	requestScope, ok := AIContextScope(ctx)
	if !ok || requestScope.Write != agent.scope.Write {
		return nil, fmt.Errorf("analyze: requested scope is unavailable")
	}
	if agent.ledger == nil {
		return nil, ledger.ErrLedgerUnavailable
	}
	runID := uuid.NewString()
	trigger := ledger.Trigger{RunID: runID, Kind: ledger.TriggerIncident, Principal: agent.scope.Write, Org: agent.scope.Write, Surface: "api", PolicyVersion: "oss-default", At: time.Now().UTC()}
	if analyzeTask, ok := task.(core.AnalyzeTask); ok {
		trigger.Subject = ledger.Subject{Kind: "incident", ID: analyzeTask.Snapshot.IncidentID}
		if analyzeTask.Snapshot.RequestedBy != "" {
			trigger.Principal = analyzeTask.Snapshot.RequestedBy
		}
	}
	if err := agent.ledger.Begin(ctx, trigger); err != nil {
		return nil, ledger.ErrLedgerUnavailable
	}
	ctx = ledger.WithRun(ctx, agent.ledger, runID)
	closed := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if !closed {
				_ = agent.ledger.Close(context.WithoutCancel(ctx), runID, ledger.RunOutcome{State: "failed", Code: "panic"})
			}
			panic(recovered)
		}
	}()
	result, runErr := agent.delegate.Run(ctx, task)
	closed = true
	closeErr := agent.ledger.Close(context.WithoutCancel(ctx), runID, ledger.RunOutcome{State: ledgerOutcomeState(runErr), Code: ledgerOutcomeCode(runErr)})
	if closeErr != nil {
		return result, errors.Join(runErr, ledger.ErrLedgerUnavailable)
	}
	return result, runErr
}

func ledgerOutcomeState(err error) string {
	if err != nil {
		return "failed"
	}
	return "done"
}

func ledgerOutcomeCode(err error) string {
	if err != nil {
		return "run_failed"
	}
	return ""
}

func loadCurrentTools(manager *aitools.Manager, scope tenancy.OrgScope, snapshot func(tenancy.OrgScope) aitools.Snapshot, agent aitools.AgentKind, runtime []core.Tool) ([]core.Tool, error) {
	return manager.Filter(scope, agent, runtime, snapshot(scope))
}

type toolGeneration struct {
	mu       sync.Mutex
	manager  *aitools.Manager
	scope    tenancy.OrgScope
	snapshot func(tenancy.OrgScope) aitools.Snapshot
	view     aitools.ToolsetSettingsView
	current  aitools.Snapshot
	err      error
}

func newToolGeneration(manager *aitools.Manager, scope tenancy.OrgScope, snapshot func(tenancy.OrgScope) aitools.Snapshot) *toolGeneration {
	return &toolGeneration{manager: manager, scope: scope, snapshot: snapshot}
}

func (generation *toolGeneration) Revision(context.Context) (string, bool) {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	generation.view, generation.err = generation.manager.LoadToolsets(generation.scope)
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
	configuredSignals := make(map[signalsources.Kind]int)
	for _, source := range cfg.Sources {
		if source.Enable {
			configuredSignals[signalsources.KindOf(source.Type)]++
		}
	}
	hasGit := len(cfg.Tools.RecentChanges.Git.Repos) > 0
	hasGraph := len(cfg.Tools.DescribeDependencies.Services) > 0
	hasEmbedder := strings.TrimSpace(cfg.Tools.FindRunbook.EmbeddingModel) != ""
	configured := func(ok bool, name string) aitools.DependencyStatus {
		return aitools.DependencyStatus{Configured: ok, Healthy: ok, Name: name}
	}
	metrics := configured(configuredSignals[signalsources.KindMetrics] > 0, "Metric data source")
	metrics.Count = configuredSignals[signalsources.KindMetrics]
	traces := configured(configuredSignals[signalsources.KindTraces] > 0, "Trace data source")
	traces.Count = configuredSignals[signalsources.KindTraces]
	logs := configured(configuredSignals[signalsources.KindLogs] > 0, "Log data source")
	logs.Count = configuredSignals[signalsources.KindLogs]
	return aitools.Snapshot{
		DataSources: map[string]aitools.DependencyStatus{
			"logs":          logs,
			"elasticsearch": configured(hasEnabledElasticsearch(cfg.Sources), "Elasticsearch log source"),
			"metrics":       metrics,
			"traces":        traces,
		},
		Integrations: map[string]aitools.DependencyStatus{"github": configured(hasGit, "GitHub"), "kubernetes": configured(strings.TrimSpace(cfg.Tools.Kubernetes.Endpoint) != "" || strings.TrimSpace(cfg.Tools.Kubernetes.Auth.Mode) != "", "Kubernetes cluster")},
		Capabilities: map[string]aitools.DependencyStatus{
			"ai_embedder": configured(hasEmbedder, "AI embedder"), "runbook_index": configured(hasEmbedder && store != nil, "Runbook index"), "dependency_graph": configured(hasGraph, "Dependency graph"),
			"change_feed": configured(hasGit || (store != nil && (strings.TrimSpace(cfg.Tools.Kubernetes.Endpoint) != "" || strings.TrimSpace(cfg.Tools.Kubernetes.Auth.Mode) != "")), "Change feed"),
		},
	}
}

func buildToolAvailabilitySnapshot(configured aitools.Snapshot, reader commontools.SignalReader, graph *commontools.DependencyGraph, changes commontools.ChangeFeed, embedder core.Embedder, runbooks commontools.RunbookSearcher, health versustools.DetectionHealthSnapshot) aitools.Snapshot {
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
			"logs": dataSource("logs", configured.DataSources["logs"], reader != nil), "elasticsearch": configured.DataSources["elasticsearch"], "metrics": configured.DataSources["metrics"], "traces": configured.DataSources["traces"],
		},
		Integrations: map[string]aitools.DependencyStatus{
			"github": resolved(configured.Integrations["github"], configured.Integrations["github"].Constructed && changes != nil), "kubernetes": resolved(configured.Integrations["kubernetes"], configured.Integrations["kubernetes"].Configured && configured.Integrations["kubernetes"].Healthy),
		},
		Capabilities: map[string]aitools.DependencyStatus{
			"ai_embedder": resolved(configured.Capabilities["ai_embedder"], embedder != nil), "runbook_index": resolved(configured.Capabilities["runbook_index"], runbooks != nil), "dependency_graph": resolved(configured.Capabilities["dependency_graph"], graph != nil && graph.Len() > 0),
			"change_feed": resolved(configured.Capabilities["change_feed"], changes != nil),
		},
	}
}

func hasEnabledElasticsearch(sources []config.AgentSourceConfig) bool {
	for _, source := range sources {
		if source.Enable && source.Type == "elasticsearch" {
			return true
		}
	}
	return false
}

func buildElasticsearchToolSources(sources []config.AgentSourceConfig) ([]elasticsearchtools.Source, []error) {
	result := make([]elasticsearchtools.Source, 0)
	errs := make([]error, 0)
	seen := make(map[string]struct{})
	for _, source := range sources {
		if !source.Enable || source.Type != "elasticsearch" {
			continue
		}
		name := strings.TrimSpace(source.Name)
		if name == "" {
			errs = append(errs, fmt.Errorf("agent: Elasticsearch tool source name is required"))
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			errs = append(errs, fmt.Errorf("agent: Elasticsearch tool source %q is duplicated", boundAvailabilityText(name, 80)))
			continue
		}
		seen[name] = struct{}{}
		service, err := elasticsearchapp.NewService(source.Elasticsearch)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent: Elasticsearch tool source %q has invalid configuration", boundAvailabilityText(name, 80)))
			continue
		}
		result = append(result, elasticsearchtools.Source{Name: name, Service: service})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	slices.SortFunc(result, func(left, right elasticsearchtools.Source) int { return strings.Compare(left.Name, right.Name) })
	return result, errs
}

func buildSigNozToolSources(sources []config.AgentSourceConfig, scrubber core.Scrubber) ([]signoztools.Source, []error) {
	result := make([]signoztools.Source, 0)
	errs := make([]error, 0)
	seen := make(map[string]struct{})
	for _, source := range sources {
		if !source.Enable || source.Type != "signoz" {
			continue
		}
		name := strings.TrimSpace(source.Name)
		if name == "" {
			errs = append(errs, fmt.Errorf("SigNoz tool source name is required"))
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			errs = append(errs, fmt.Errorf("SigNoz tool source %q is duplicated", boundAvailabilityText(name, 80)))
			continue
		}
		seen[name] = struct{}{}
		service, err := signozapp.NewService(signozapp.Config{Address: source.Signoz.Address, APIKey: source.Signoz.APIKey, InsecureSkipVerify: source.Signoz.InsecureSkipVerify, AllowLoopback: source.Signoz.AllowLoopback, AllowPrivate: source.Signoz.AllowPrivateNetworks, ScopeFilter: source.Signoz.Query}, signozapp.ToolPolicy())
		if err != nil {
			errs = append(errs, fmt.Errorf("SigNoz tool source %q has invalid configuration", boundAvailabilityText(name, 80)))
			continue
		}
		service.SetScrubber(scrubber)
		result = append(result, signoztools.Source{Name: name, Kind: signozapp.SignalLogs, Service: service})
	}
	slices.SortFunc(result, func(left, right signoztools.Source) int { return strings.Compare(left.Name, right.Name) })
	return result, errs
}

func buildLokiToolSources(sources []config.AgentSourceConfig, scrubber core.Scrubber) ([]lokitools.Source, []error) {
	result := make([]lokitools.Source, 0)
	var errs []error
	seen := map[string]bool{}
	for _, source := range sources {
		if !source.Enable || source.Type != "loki" {
			continue
		}
		name := strings.TrimSpace(source.Name)
		if name == "" || seen[name] {
			errs = append(errs, fmt.Errorf("agent: Loki tool source name is missing or duplicated"))
			continue
		}
		seen[name] = true
		service, err := lokiapp.NewService(source.Loki, scrubber)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent: Loki tool source %q: selector-only scope and safe endpoint are required", boundAvailabilityText(name, 80)))
			continue
		}
		result = append(result, lokitools.Source{Name: name, Service: service})
	}
	slices.SortFunc(result, func(left, right lokitools.Source) int { return strings.Compare(left.Name, right.Name) })
	return result, errs
}

const graylogMaxCredentialBytes = 64 * 1024

func buildGraylogToolSources(sources []config.AgentSourceConfig, scrubber core.Scrubber) ([]graylogtools.Source, []error) {
	const maxSources = 128
	const maxGraylogNames = 64
	if len(sources) > maxSources {
		return nil, []error{fmt.Errorf("agent: Graylog tool source configuration exceeds safety limits")}
	}
	result := make([]graylogtools.Source, 0)
	var errs []error
	counts := map[string]int{}
	var credentials []string
	credentialBytes := 0
	optionNodes := 0
	graylogNames := 0
	addCredential := func(value string) bool {
		if len(value) > graylogMaxCredentialBytes-credentialBytes {
			return false
		}
		credentialBytes += len(value)
		credentials = append(credentials, value)
		return true
	}
	for _, source := range sources {
		if source.Enable && source.Type == "graylog" && len(source.Name) > 80 {
			return nil, []error{fmt.Errorf("agent: Graylog tool source configuration exceeds safety limits")}
		}
		for _, value := range []string{
			source.Graylog.APIToken, source.Graylog.Username, source.Graylog.Password,
			source.Loki.BearerToken, source.Loki.Username, source.Loki.Password,
			source.Signoz.APIKey,
			source.Elasticsearch.APIKey, source.Elasticsearch.Username, source.Elasticsearch.Password,
			source.Splunk.Token, source.Splunk.Username, source.Splunk.Password,
		} {
			if !addCredential(value) {
				return nil, []error{fmt.Errorf("agent: Graylog tool source configuration exceeds safety limits")}
			}
		}
		if !collectSourceOptionCredentials(source.Options, &optionNodes, &credentialBytes, addCredential) {
			return nil, []error{fmt.Errorf("agent: Graylog tool source configuration exceeds safety limits")}
		}
		if source.Enable && source.Type == "graylog" {
			graylogNames++
			if graylogNames > maxGraylogNames {
				return nil, []error{fmt.Errorf("agent: Graylog tool source configuration exceeds safety limits")}
			}
			counts[strings.TrimSpace(source.Name)]++
		}
	}
	for _, source := range sources {
		if !source.Enable || source.Type != "graylog" {
			continue
		}
		name := strings.TrimSpace(source.Name)
		if name == "" || counts[name] != 1 {
			errs = append(errs, fmt.Errorf("agent: Graylog tool source name is missing or duplicated"))
			continue
		}
		valid := len(name) <= 80
		for index := 0; valid && index < len(name); index++ {
			char := name[index]
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || (index > 0 && (char == '-' || char == '_' || char == '.'))) {
				valid = false
				break
			}
		}
		if !valid {
			errs = append(errs, fmt.Errorf("agent: Graylog tool source name is invalid"))
			continue
		}
		for _, secret := range credentials {
			if secret != "" && strings.Contains(name, secret) {
				valid = false
				break
			}
			for index := 0; index+8 <= len(name); index++ {
				if strings.Contains(secret, name[index:index+8]) {
					valid = false
					break
				}
			}
			if !valid {
				break
			}
		}
		if scrubber != nil && scrubber.Scrub(name) != name {
			valid = false
		}
		if !valid {
			errs = append(errs, fmt.Errorf("agent: Graylog tool source name is invalid"))
			continue
		}
		service, err := graylogapp.NewService(source.Graylog, scrubber)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent: Graylog tool source has invalid configuration"))
			continue
		}
		result = append(result, graylogtools.Source{Name: name, Service: service})
	}
	slices.SortFunc(result, func(left, right graylogtools.Source) int { return strings.Compare(left.Name, right.Name) })
	return result, errs
}

func buildSplunkToolSources(sources []config.AgentSourceConfig, scrubber core.Scrubber) ([]splunktools.Source, []error) {
	if len(sources) > 128 {
		return nil, []error{fmt.Errorf("agent: Splunk tool source configuration exceeds safety limits")}
	}
	var credentials []string
	credentialBytes := 0
	optionNodes := 0
	counts := map[string]int{}
	for _, source := range sources {
		for _, value := range []string{
			source.Splunk.Token, source.Splunk.Username, source.Splunk.Password,
			source.Graylog.APIToken, source.Graylog.Username, source.Graylog.Password,
			source.Loki.BearerToken, source.Loki.Username, source.Loki.Password,
			source.Signoz.APIKey,
			source.Elasticsearch.APIKey, source.Elasticsearch.Username, source.Elasticsearch.Password,
		} {
			if len(value) > graylogMaxCredentialBytes-credentialBytes {
				return nil, []error{fmt.Errorf("agent: Splunk tool source configuration exceeds safety limits")}
			}
			credentialBytes += len(value)
			credentials = append(credentials, value)
		}
		addCredential := func(value string) bool {
			if len(value) > graylogMaxCredentialBytes-credentialBytes {
				return false
			}
			credentialBytes += len(value)
			credentials = append(credentials, value)
			return true
		}
		if !collectSourceOptionCredentials(source.Options, &optionNodes, &credentialBytes, addCredential) {
			return nil, []error{fmt.Errorf("agent: Splunk tool source configuration exceeds safety limits")}
		}
		if source.Enable && source.Type == "splunk" {
			counts[strings.TrimSpace(source.Name)]++
			if len(source.Name) > 80 || len(counts) > 64 {
				return nil, []error{fmt.Errorf("agent: Splunk tool source configuration exceeds safety limits")}
			}
		}
	}
	result := make([]splunktools.Source, 0)
	var errs []error
	for _, source := range sources {
		if !source.Enable || source.Type != "splunk" {
			continue
		}
		name := strings.TrimSpace(source.Name)
		if name == "" || counts[name] != 1 {
			errs = append(errs, fmt.Errorf("agent: Splunk tool source name is missing or duplicated"))
			continue
		}
		valid := len(name) <= 80
		for index := 0; valid && index < len(name); index++ {
			char := name[index]
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || (index > 0 && (char == '-' || char == '_' || char == '.'))) {
				valid = false
			}
		}
		for _, secret := range credentials {
			if secret != "" && strings.Contains(name, secret) {
				valid = false
			}
			for index := 0; index+8 <= len(name); index++ {
				if strings.Contains(secret, name[index:index+8]) {
					valid = false
					break
				}
			}
		}
		if scrubber != nil && scrubber.Scrub(name) != name {
			valid = false
		}
		if !valid {
			errs = append(errs, fmt.Errorf("agent: Splunk tool source name is invalid"))
			continue
		}
		service, err := splunkapp.NewService(source.Splunk, scrubber)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent: Splunk tool source has invalid configuration"))
			continue
		}
		result = append(result, splunktools.Source{Name: name, Service: service})
	}
	slices.SortFunc(result, func(left, right splunktools.Source) int { return strings.Compare(left.Name, right.Name) })
	return result, errs
}

func collectSourceOptionCredentials(options map[string]interface{}, totalNodes, totalBytes *int, addCredential func(string) bool) bool {
	// Count the root map as depth zero and every visited container or scalar as one node.
	const maxDepth = 8
	const maxNodes = 1024
	const maxTotalNodes = 4096
	const maxKeyBytes = 256
	visited := 0
	var scan func(reflect.Value, int, bool) bool
	scan = func(value reflect.Value, depth int, credentialNamed bool) bool {
		visited++
		*totalNodes++
		if depth > maxDepth || visited > maxNodes || *totalNodes > maxTotalNodes {
			return false
		}
		for value.IsValid() && value.Kind() == reflect.Interface {
			value = value.Elem()
		}
		if !value.IsValid() {
			return true
		}
		switch value.Kind() {
		case reflect.Map:
			if value.Type().Key().Kind() != reflect.String {
				return false
			}
			iterator := value.MapRange()
			for iterator.Next() {
				key := iterator.Key().String()
				if len(key) > maxKeyBytes || len(key) > graylogMaxCredentialBytes-*totalBytes {
					return false
				}
				*totalBytes += len(key)
				normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(key))
				matched := strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "password") || strings.Contains(normalized, "apikey") || strings.Contains(normalized, "username") || strings.Contains(normalized, "credential") || normalized == "authorization"
				if !scan(iterator.Value(), depth+1, credentialNamed || matched) {
					return false
				}
			}
		case reflect.Slice, reflect.Array:
			for index := 0; index < value.Len(); index++ {
				if !scan(value.Index(index), depth+1, credentialNamed) {
					return false
				}
			}
		case reflect.String:
			if credentialNamed {
				if !addCredential(value.String()) {
					return false
				}
			} else {
				length := value.Len()
				if length > graylogMaxCredentialBytes-*totalBytes {
					return false
				}
				*totalBytes += length
			}
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
		default:
			return false
		}
		return true
	}
	return scan(reflect.ValueOf(options), 0, false)
}

func combineLogTools(groups ...[]core.Tool) []core.Tool {
	var result []core.Tool
	blocked := map[string]bool{}
	for _, group := range groups {
		for _, candidate := range group {
			if blocked[candidate.Name()] {
				continue
			}
			found := false
			for index, existing := range result {
				if existing.Name() != candidate.Name() {
					continue
				}
				found = true
				combined, err := CombineSourceRoutedTools(existing, candidate)
				if err != nil {
					log.Printf("agent: ambiguous log capability omitted: %s", candidate.Name())
					blocked[candidate.Name()] = true
					result = append(result[:index], result[index+1:]...)
				} else {
					result[index] = combined
				}
				break
			}
			if !found {
				result = append(result, candidate)
			}
		}
	}
	return result
}

func elasticsearchConstructionStatus(status aitools.DependencyStatus, sources []elasticsearchtools.Source, errs []error) aitools.DependencyStatus {
	status.Constructed = status.Configured && len(errs) == 0 && len(sources) > 0
	status.Healthy = status.Constructed
	if status.Configured && !status.Constructed {
		status.Health = "configuration"
	}
	return status
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

func chatRuntimeToolCatalog(readTools []core.Tool, actionService *act.Service) []core.Tool {
	if actionService == nil {
		return readTools
	}
	chatTools := append([]core.Tool(nil), readTools...)
	return append(chatTools, act.ProposalTool{Service: actionService})
}

func buildAnalyzeTools(store storage.Provider, scope tenancy.OrgScope, catalog versustools.PatternCatalog, reader commontools.SignalReader, redactor commontools.LineRedactor, services commontools.ServiceExtractor, graph *commontools.DependencyGraph, changes commontools.ChangeFeed, embedder core.Embedder, runbooks commontools.RunbookSearcher, health versustools.DetectionHealthReader) []core.Tool {
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
		versustools.ListCapabilities{Capabilities: knowledgeCapabilities(scope, store, catalog, reader, graph, changes, embedder, runbooks, health, providers)},
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
	return tools
}

func knowledgeCapabilities(scope tenancy.OrgScope, store storage.Provider, catalog versustools.PatternCatalog, reader commontools.SignalReader, graph *commontools.DependencyGraph, changes commontools.ChangeFeed, embedder core.Embedder, runbooks commontools.RunbookSearcher, health versustools.DetectionHealthReader, providers ChatKnowledgeProviders) []versustools.CapabilityStatus {
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
		status("metrics", false, "Configure a metrics data source."),
		status("traces", false, "Configure a trace data source."),
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

func sourceReadCapabilities(statuses []versustools.CapabilityStatus, snapshot aitools.Snapshot, runtime []core.Tool) []versustools.CapabilityStatus {
	for index := range statuses {
		name := statuses[index].Name
		readTool := ""
		switch name {
		case "metrics":
			readTool = "read_metric_series"
		case "traces":
			readTool = "read_trace_spans"
		default:
			continue
		}
		present := false
		for _, candidate := range runtime {
			if candidate.Name() == readTool {
				present = true
				break
			}
		}
		resolution := aitools.Resolve(aitools.Requirement{Kind: aitools.RequirementDataSource, SignalKind: name, Capabilities: []string{readTool}}, snapshot, true)
		if resolution.State == aitools.StateNeedsLicense {
			statuses[index].Licensed = versustools.CapabilityStatusFalse
		}
		if !present || !snapshot.Capabilities[readTool].Constructed {
			statuses[index].Configured = false
			statuses[index].Available = versustools.CapabilityStatusFalse
			statuses[index].Reason = "native read capability not constructed"
			continue
		}
		statuses[index].Configured = true
		statuses[index].Available = versustools.CapabilityStatusFalse
		if resolution.State == aitools.StateNeedsLicense || statuses[index].Licensed == versustools.CapabilityStatusFalse {
			statuses[index].Licensed = versustools.CapabilityStatusFalse
		} else if resolution.State == aitools.StateAvailable {
			statuses[index].Licensed = versustools.CapabilityStatusTrue
			statuses[index].Available = versustools.CapabilityStatusTrue
			statuses[index].Reason = "configured"
			statuses[index].SetupAction = ""
		} else {
			statuses[index].Reason = resolution.Reason
		}
	}
	return statuses
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
func buildRunbookManager(cfg config.AgentConfig, store storage.Provider, scope tenancy.OrgScope, httpClient *http.Client, runtimeKey func(context.Context) (string, bool), runtime einowrap.RuntimeAI) *runbook.Manager {
	return buildRunbookManagerFromDir(cfg, store, scope, httpClient, runtimeKey, runtime, filepath.Join(storage.DefaultDataDir, runbook.SourceSubdir))
}

func buildRunbookManagerFromDir(cfg config.AgentConfig, store storage.Provider, scope tenancy.OrgScope, httpClient *http.Client, runtimeKey func(context.Context) (string, bool), runtime einowrap.RuntimeAI, sourceDir string) *runbook.Manager {
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
		base := config.AgentAIConfig{
			Provider: cfg.AI.Provider,
			BaseURL:  cfg.AI.BaseURL,
			Model:    embCfg.EmbeddingModel,
			APIKey:   cfg.AI.APIKey,
		}
		embeddingKey := func(ctx context.Context) (string, bool) {
			hasBaseURL := strings.TrimSpace(cfg.AI.BaseURL) != ""
			if runtime.BaseURL != nil {
				if baseURL, ok := runtime.BaseURL(ctx); ok {
					hasBaseURL = strings.TrimSpace(baseURL) != ""
				}
			}
			if !hasBaseURL && runtime.Provider != nil {
				if provider, ok := runtime.Provider(ctx); ok && !einowrap.IsSupportedEmbedderProvider(provider) {
					return "", false
				}
			}
			if runtimeKey == nil {
				return "", false
			}
			return runtimeKey(ctx)
		}
		holder := einowrap.NewEmbedderHolder(base, einowrap.Options{
			HTTPClient:     httpClient,
			RuntimeKeyFunc: embeddingKey,
		}, runtime)
		bootCtx := DecorateAIContext(context.Background(), scope)
		if _, embErr := holder.Get(bootCtx); embErr != nil {
			var configErr *einowrap.ConfigError
			if errors.As(embErr, &configErr) {
				log.Printf("agent: find_runbook disabled: embedder configuration error: %v", configErr)
			} else {
				safeErr := einowrap.SafeProviderError(effectiveEmbeddingProvider(bootCtx, base.Provider, runtime), base.Model, embErr)
				log.Printf("agent: find_runbook disabled: embedder init failed: %v", safeErr)
			}
		} else {
			embedder = &runtimeEmbedder{holder: holder, scope: scope.Normalized(), provider: base.Provider, model: embCfg.EmbeddingModel, runtime: runtime}
		}
	}

	bootScope := scope.Normalized()
	mgr := runbook.NewManager(rbStore, embedder, bootScope.Write, func(ctx context.Context) context.Context {
		return DecorateAIContext(ctx, bootScope)
	})

	// Auto-ingest the runbook source dir so operators never run a separate
	// CLI. Ingestion is incremental — unchanged runbooks reuse their cached
	// vector, so a reboot with no edits makes no embedding calls. A no-op
	// when no embedder is configured. Non-fatal: we still serve the
	// previously-persisted corpus on failure.
	if n, ingErr := mgr.IngestDir(context.Background(), sourceDir); ingErr != nil {
		log.Printf("agent: find_runbook: runbook ingest failed: %v (serving previously-persisted corpus)", ingErr)
	} else if n > 0 {
		log.Printf("agent: find_runbook: ingested %d runbook(s) from %s", n, sourceDir)
	}

	if embedder != nil {
		safeModel := einowrap.SafeProviderError(cfg.AI.Provider, embCfg.EmbeddingModel, nil).Model
		log.Printf("agent: find_runbook enabled model=%s runbooks=%d", safeModel, rbStore.Len())
	} else {
		log.Printf("agent: runbooks UI enabled (no embedding model; uploads not searchable until configured) runbooks=%d", rbStore.Len())
	}
	return mgr
}

type runtimeEmbedder struct {
	holder   *einowrap.Holder[core.Embedder]
	scope    tenancy.OrgScope
	provider string
	model    string
	runtime  einowrap.RuntimeAI
}

func (embedder *runtimeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	requestScope, ok := AIContextScope(ctx)
	if !ok || requestScope.Write != embedder.scope.Write {
		return nil, fmt.Errorf("AI scope mismatch")
	}
	current, err := embedder.holder.Get(ctx)
	if err != nil {
		return nil, einowrap.SafeProviderError(embedder.effectiveProvider(ctx), embedder.model, err)
	}
	vectors, err := current.Embed(ctx, texts)
	if err != nil {
		return nil, einowrap.SafeProviderError(embedder.effectiveProvider(ctx), embedder.model, err)
	}
	return vectors, nil
}

func (embedder *runtimeEmbedder) effectiveProvider(ctx context.Context) string {
	return effectiveEmbeddingProvider(ctx, embedder.provider, embedder.runtime)
}

func effectiveEmbeddingProvider(ctx context.Context, configured string, runtime einowrap.RuntimeAI) string {
	if runtime.Provider != nil {
		if provider, ok := runtime.Provider(ctx); ok && einowrap.IsSupportedEmbedderProvider(provider) {
			return strings.ToLower(strings.TrimSpace(provider))
		}
	}
	provider := strings.ToLower(strings.TrimSpace(configured))
	if provider == "" {
		return einowrap.DefaultProvider
	}
	return provider
}
