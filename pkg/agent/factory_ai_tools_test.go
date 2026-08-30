package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	versustools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/versus"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type registrationHealth struct{}

func (registrationHealth) DetectionHealth(tenancy.OrgScope) versustools.DetectionHealthSnapshot {
	return versustools.DetectionHealthSnapshot{Observation: "unknown"}
}

func TestBuildAnalyzeToolsPreservesBaseOrder(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	tools := buildAnalyzeTools(store, tenancy.DefaultOrgScope(), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	want := []string{"get_incident", "get_pattern", "get_service", "get_system_overview", "list_services", "list_capabilities", "get_alert_decision", "search_incidents", "list_patterns", "list_analyses"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	seen := make(map[string]bool, len(tools))
	for index, name := range want {
		if tools[index].Name() != name {
			t.Fatalf("tool[%d] = %q, want %q", index, tools[index].Name(), name)
		}
		if seen[name] {
			t.Fatalf("duplicate tool name %q", name)
		}
		seen[name] = true
	}
}

func TestBuildAnalyzeToolsRegistersAllDiscoveryTools(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	tools := buildAnalyzeTools(store, tenancy.DefaultOrgScope(), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil, nil, registrationHealth{})
	want := []string{"get_incident", "get_pattern", "get_service", "get_system_overview", "list_services", "get_detection_health", "list_capabilities", "get_alert_decision", "search_incidents", "list_patterns", "list_analyses"}

	registered := make(map[string]bool, len(tools))
	for _, tool := range tools {
		registered[tool.Name()] = true
	}
	for _, name := range want {
		if !registered[name] {
			t.Fatalf("discovery tool %q is not registered", name)
		}
	}
}

func TestBuildAnalyzeToolsRuntimeCatalogContract(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	for _, tool := range buildAnalyzeTools(store, tenancy.DefaultOrgScope(), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil, nil, registrationHealth{}) {
		if _, ok := aitools.Lookup(tool.Name()); !ok {
			t.Errorf("runtime tool %q is absent from availability catalog", tool.Name())
		}
	}
}

func TestToolRegistrationFiltersChatAndAnalyzeIndependently(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	scope := tenancy.DefaultOrgScope()
	runtime := buildAnalyzeTools(store, scope, newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	manager := aitools.NewManager(store)
	if _, err := manager.SetEnabled(scope, aitools.AgentChat, "get_incident", false); err != nil {
		t.Fatal(err)
	}
	chat, err := manager.Filter(scope, aitools.AgentChat, runtime, aitools.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	analyze, err := manager.Filter(scope, aitools.AgentAnalyze, runtime, aitools.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	registered := func(tools []core.Tool, name string) bool {
		for _, tool := range tools {
			if tool.Name() == name {
				return true
			}
		}
		return false
	}
	if registered(chat, "get_incident") || !registered(analyze, "get_incident") {
		t.Fatalf("independent filtering failed: chat=%v analyze=%v", registered(chat, "get_incident"), registered(analyze, "get_incident"))
	}
}

func TestBuildAIsExposesToolCatalogWhenAIDisabled(t *testing.T) {
	bundle := BuildAIs(config.AgentConfig{}, nil, storage.NewMemory(), nil)
	if bundle.ToolSettings == nil || bundle.ToolSnapshot == nil {
		t.Fatal("AI-disabled bundle did not expose tool availability")
	}
	resolved := aitools.Resolve(aitools.Requirement{Kind: aitools.RequirementIntegration, Integration: "kubernetes"}, bundle.ToolSnapshot(tenancy.DefaultOrgScope()), true)
	if resolved.State != aitools.StateNeedsIntegration {
		t.Fatalf("kubernetes state = %q", resolved.State)
	}
}

func TestConfiguredToolAvailabilityUsesSourceKinds(t *testing.T) {
	const metricsType = "availability-test-metrics"
	const tracesType = "availability-test-traces"
	signalsources.RegisterKind(metricsType, signalsources.KindMetrics)
	signalsources.RegisterKind(tracesType, signalsources.KindTraces)

	tests := []struct {
		name    string
		sources []config.AgentSourceConfig
		want    map[string]bool
	}{
		{name: "metrics only", sources: []config.AgentSourceConfig{{Enable: true, Type: metricsType}}, want: map[string]bool{"metrics": true}},
		{name: "traces only", sources: []config.AgentSourceConfig{{Enable: true, Type: tracesType}}, want: map[string]bool{"traces": true}},
		{name: "logs only", sources: []config.AgentSourceConfig{{Enable: true, Type: "file"}}, want: map[string]bool{"logs": true}},
		{name: "mixed", sources: []config.AgentSourceConfig{{Enable: true, Type: "file"}, {Enable: true, Type: metricsType}, {Enable: true, Type: tracesType}}, want: map[string]bool{"logs": true, "metrics": true, "traces": true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := configuredToolAvailabilitySnapshot(config.AgentConfig{Sources: test.sources}, nil)
			for _, kind := range []string{"logs", "metrics", "traces"} {
				if got := snapshot.DataSources[kind].Configured; got != test.want[kind] {
					t.Errorf("%s configured = %t, want %t", kind, got, test.want[kind])
				}
				if snapshot.DataSources[kind].Constructed {
					t.Errorf("operator source label constructed %s capability", kind)
				}
			}
		})
	}
}

func TestRuntimeSnapshotDistinguishesConfiguredFailureFromMissing(t *testing.T) {
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs":    {Configured: true, Name: "Log data source"},
		"metrics": {Configured: false, Name: "Metric data source"},
		"traces":  {Configured: false, Name: "Trace data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	snapshot := buildToolAvailabilitySnapshot(configured, nil, nil, nil, nil, nil, nil, nil, versustools.DetectionHealthSnapshot{})
	logs := aitools.Resolve(aitools.Requirement{Kind: aitools.RequirementDataSource, SignalKind: "logs"}, snapshot, true)
	metrics := aitools.Resolve(aitools.Requirement{Kind: aitools.RequirementDataSource, SignalKind: "metrics"}, snapshot, true)
	if logs.State != aitools.StateUnhealthy || logs.Health != "configuration" {
		t.Fatalf("configured failure = %+v", logs)
	}
	if metrics.State != aitools.StateNeedsDataSource {
		t.Fatalf("missing source = %+v", metrics)
	}
}

func TestRuntimeSnapshotUsesConstructedReadersBeforeFirstObservation(t *testing.T) {
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs":    {Configured: true, Name: "Log data source"},
		"metrics": {Configured: true, Name: "Metric data source"},
		"traces":  {Configured: true, Name: "Trace data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	snapshot := buildToolAvailabilitySnapshot(configured, &signalReaderAdapter{}, nil, nil, nil, nil, registrationMetricReader{}, registrationTraceReader{}, versustools.DetectionHealthSnapshot{})
	for _, kind := range []string{"logs", "metrics", "traces"} {
		if got := snapshot.DataSources[kind]; !got.Constructed || !got.Healthy || got.Health != "" {
			t.Errorf("unobserved %s health = %+v", kind, got)
		}
	}
}

func TestSourceKindHealthAggregatesAllUsableSources(t *testing.T) {
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs": {Configured: true, Name: "Log data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	healthy := versustools.SourceHealth{Name: "healthy", Kind: "logs", Configured: true, Observation: "healthy"}
	unobserved := versustools.SourceHealth{Name: "unobserved", Kind: "logs", Configured: true, Observation: "unknown"}
	failedA := versustools.SourceHealth{Name: "alpha", Kind: "logs", Configured: true, Observation: "unhealthy", ErrorClass: "authentication"}
	failedZ := versustools.SourceHealth{Name: "zeta", Kind: "logs", Configured: true, Observation: "unhealthy", ErrorClass: "connection"}

	tests := []struct {
		name        string
		sources     []versustools.SourceHealth
		wantHealthy bool
		wantName    string
		wantClass   string
	}{
		{name: "healthy and failed", sources: []versustools.SourceHealth{failedZ, healthy}, wantHealthy: true},
		{name: "unobserved and failed", sources: []versustools.SourceHealth{failedZ, unobserved}, wantHealthy: true},
		{name: "all failed", sources: []versustools.SourceHealth{failedZ, failedA}, wantName: "alpha", wantClass: "authentication"},
		{name: "one source recovers", sources: []versustools.SourceHealth{failedZ, healthy}, wantHealthy: true},
		{name: "all failed reverse order", sources: []versustools.SourceHealth{failedA, failedZ}, wantName: "alpha", wantClass: "authentication"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := buildToolAvailabilitySnapshot(configured, &signalReaderAdapter{}, nil, nil, nil, nil, nil, nil, versustools.DetectionHealthSnapshot{Sources: test.sources})
			got := snapshot.DataSources["logs"]
			wantName := test.wantName
			if wantName == "" {
				wantName = "Log data source"
			}
			if got.Healthy != test.wantHealthy || got.Name != wantName || got.Health != test.wantClass {
				t.Fatalf("status = %+v", got)
			}
		})
	}
}

func TestConfiguredReadersResolveAndRegisterWithoutWorkerObservations(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.AgentConfig
		kind    string
		tool    string
		reader  commontools.SignalReader
		metrics commontools.MetricReader
		traces  commontools.TraceReader
	}{
		{
			name: "OSS Prometheus reader without metric signal source", kind: "metrics", tool: "query_metrics",
			cfg:     config.AgentConfig{Tools: config.ToolsConfig{QueryMetrics: config.QueryMetricsToolConfig{Prometheus: config.QueryMetricsPrometheusConfig{Address: "http://prometheus"}}}},
			metrics: registrationMetricReader{},
		},
		{
			name: "OSS Tempo reader without trace signal source", kind: "traces", tool: "query_traces",
			cfg:    config.AgentConfig{Tools: config.ToolsConfig{QueryTraces: config.QueryTracesToolConfig{Tempo: config.QueryTracesTempoConfig{Address: "http://tempo"}}}},
			traces: registrationTraceReader{},
		},
		{
			name: "configured log source before first pull", kind: "logs", tool: "get_related_logs",
			cfg:    config.AgentConfig{Sources: []config.AgentSourceConfig{{Name: "logs", Type: "file", Enable: true}}},
			reader: &signalReaderAdapter{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configured := configuredToolAvailabilitySnapshot(test.cfg, nil)
			snapshot := buildToolAvailabilitySnapshot(configured, test.reader, nil, nil, nil, nil, test.metrics, test.traces, versustools.DetectionHealthSnapshot{})
			requirement := aitools.Requirement{Kind: aitools.RequirementDataSource, SignalKind: test.kind}
			if got := aitools.Resolve(requirement, snapshot, true); got.State != aitools.StateAvailable {
				t.Fatalf("resolution = %+v", got)
			}
			filtered, err := aitools.NewManager(storage.NewMemory()).Filter(tenancy.DefaultOrgScope(), aitools.AgentAnalyze, []core.Tool{settingsCompatibleTool{name: test.tool}}, snapshot)
			if err != nil || len(filtered) != 1 || filtered[0].Name() != test.tool {
				t.Fatalf("filtered = %+v, err = %v", filtered, err)
			}
		})
	}
}

type registrationMetricReader struct{}

func (registrationMetricReader) QueryRange(context.Context, string, time.Time, time.Time) ([]commontools.MetricSeries, error) {
	return nil, nil
}

type registrationTraceReader struct{}

func (registrationTraceReader) QueryTraces(context.Context, string, string, time.Time, time.Time, int) ([]commontools.TraceSummary, error) {
	return nil, nil
}

func TestMetricsObservationNeverUnlocksLogs(t *testing.T) {
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs":    {Configured: true, Name: "Log data source"},
		"metrics": {Configured: true, Name: "Metric data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	health := versustools.DetectionHealthSnapshot{Sources: []versustools.SourceHealth{{Kind: "metrics", Configured: true, Observation: "healthy"}}}
	snapshot := buildToolAvailabilitySnapshot(configured, nil, nil, nil, nil, nil, registrationMetricReader{}, nil, health)
	if got := snapshot.DataSources["metrics"]; !got.Healthy {
		t.Fatalf("metrics health = %+v", got)
	}
	if got := snapshot.DataSources["logs"]; got.Healthy || got.Health != "configuration" {
		t.Fatalf("logs health = %+v", got)
	}
}

func TestLiveSourceHealthFiltersAndRestoresLogTools(t *testing.T) {
	scope := tenancy.DefaultOrgScope()
	health := newDetectionHealthAdapter(scope,
		[]config.AgentSourceConfig{{Name: "logs", Type: "file", Enable: true}},
		[]core.SignalSource{panicPullSource{name: "logs"}}, nil,
	)
	configured := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{
		"logs": {Configured: true, Name: "Log data source"},
	}, Integrations: map[string]aitools.DependencyStatus{}, Capabilities: map[string]aitools.DependencyStatus{}}
	runtime := []core.Tool{settingsCompatibleTool{name: "get_related_logs"}}
	manager := aitools.NewManager(storage.NewMemory())
	states := []struct {
		err       error
		wantClass string
		wantTools int
	}{
		{nil, "", 1},
		{errors.New("401 unauthorized"), "authentication", 0},
		{errors.New("connection refused"), "connection", 0},
		{context.DeadlineExceeded, "timeout", 0},
		{nil, "", 1},
	}
	for _, state := range states {
		health.Observe("logs", state.err, time.Now())
		snapshot := buildToolAvailabilitySnapshot(configured, &signalReaderAdapter{}, nil, nil, nil, nil, nil, nil, health.DetectionHealth(scope))
		filtered, err := manager.Filter(scope, aitools.AgentChat, runtime, snapshot)
		if err != nil || len(filtered) != state.wantTools || snapshot.DataSources["logs"].Health != state.wantClass {
			t.Fatalf("error=%v class=%q tools=%d filterErr=%v", state.err, snapshot.DataSources["logs"].Health, len(filtered), err)
		}
	}
}

type settingsCompatibleTool struct{ name string }

func (tool settingsCompatibleTool) Name() string          { return tool.name }
func (settingsCompatibleTool) Description() string        { return "test" }
func (settingsCompatibleTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (tool settingsCompatibleTool) Invoke(context.Context, json.RawMessage) (*core.ToolResult, error) {
	return &core.ToolResult{Tool: tool.name, Found: true}, nil
}

type generationProvider struct {
	storage.Provider
	err error
}

func (provider *generationProvider) ReadBlob(name string) ([]byte, error) {
	if provider.err != nil {
		return nil, provider.err
	}
	return provider.Provider.ReadBlob(name)
}

func (provider *generationProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

func TestToolGenerationIsAtomicAndRecoversWithoutFailingOpen(t *testing.T) {
	provider := &generationProvider{Provider: storage.NewMemory()}
	manager := aitools.NewManager(provider)
	scope := tenancy.DefaultOrgScope()
	snapshotCalls := 0
	generation := newToolGeneration(manager, scope, func(tenancy.OrgScope) aitools.Snapshot {
		snapshotCalls++
		return aitools.Snapshot{}
	})
	runtime := []core.Tool{settingsCompatibleTool{name: "get_incident"}}

	if _, ok := generation.Revision(context.Background()); !ok {
		t.Fatal("initial revision unavailable")
	}
	filtered, err := generation.Filter(aitools.AgentChat, runtime)
	if err != nil || len(filtered) != 1 || snapshotCalls != 1 {
		t.Fatalf("initial generation tools=%d err=%v snapshots=%d", len(filtered), err, snapshotCalls)
	}
	if _, err := manager.SetEnabled(scope, aitools.AgentChat, "get_incident", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := generation.Revision(context.Background()); !ok {
		t.Fatal("disabled revision unavailable")
	}
	filtered, err = generation.Filter(aitools.AgentChat, runtime)
	if err != nil || len(filtered) != 0 {
		t.Fatalf("disabled generation tools=%d err=%v", len(filtered), err)
	}

	provider.err = errors.New("transient read failure")
	if _, ok := generation.Revision(context.Background()); ok {
		t.Fatal("failed revision reported available")
	}
	if _, err := generation.Filter(aitools.AgentChat, runtime); err == nil {
		t.Fatal("failed generation returned a tool graph")
	}
	provider.err = nil
	if _, ok := generation.Revision(context.Background()); !ok {
		t.Fatal("recovered revision unavailable")
	}
	filtered, err = generation.Filter(aitools.AgentChat, runtime)
	if err != nil || len(filtered) != 0 {
		t.Fatalf("recovered generation failed open: tools=%d err=%v", len(filtered), err)
	}
}

func TestSeedLoadsDurableSettingsWithoutHolderRevision(t *testing.T) {
	provider := storage.NewMemory()
	manager := aitools.NewManager(provider)
	scope := tenancy.DefaultOrgScope()
	runtime := []core.Tool{settingsCompatibleTool{name: "get_system_overview"}}
	snapshot := func(tenancy.OrgScope) aitools.Snapshot { return aitools.Snapshot{} }

	if _, err := manager.SetEnabled(scope, aitools.AgentChat, "get_system_overview", false); err != nil {
		t.Fatal(err)
	}
	filtered, err := loadCurrentTools(manager, scope, snapshot, aitools.AgentChat, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("new session seed tools = %d, want 0 before any holder revision", len(filtered))
	}
}

type registrationReliability struct{}

func (registrationReliability) ServiceReliability(context.Context, tenancy.OrgScope, string) (versustools.ServiceReliabilitySnapshot, error) {
	return versustools.ServiceReliabilitySnapshot{}, nil
}

type registrationDecision struct{}

func (registrationDecision) AlertDecision(context.Context, tenancy.OrgScope, string) (versustools.AlertDecisionSnapshot, error) {
	return versustools.AlertDecisionSnapshot{}, nil
}

type registrationStatus struct{}

func (registrationStatus) CapabilityStatuses(tenancy.OrgScope) []versustools.CapabilityStatus {
	return []versustools.CapabilityStatus{
		{Name: "service_reliability", Group: "reliability", Configured: true, Licensed: versustools.CapabilityStatusTrue, Available: versustools.CapabilityStatusTrue, Observation: "reliability ready"},
		{Name: "alert_decisions", Group: "decisions", Configured: false, Licensed: versustools.CapabilityStatusTrue, Available: versustools.CapabilityStatusFalse, SetupAction: "Enable decision storage."},
	}
}

func TestSetChatKnowledgeProvidersFeedsToolsAndCapabilities(t *testing.T) {
	SetChatKnowledgeProviders(ChatKnowledgeProviders{ServiceReliability: registrationReliability{}, AlertDecision: registrationDecision{}, CapabilityStatus: registrationStatus{}})
	t.Cleanup(func() { SetChatKnowledgeProviders(ChatKnowledgeProviders{}) })
	catalog, store := newBuildCatalog(t)
	tools := buildAnalyzeTools(store, tenancy.DefaultOrgScope(), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	var service versustools.GetService
	var decision versustools.GetAlertDecision
	var capability versustools.ListCapabilities
	for _, tool := range tools {
		switch typed := tool.(type) {
		case versustools.GetService:
			service = typed
		case versustools.GetAlertDecision:
			decision = typed
		case versustools.ListCapabilities:
			capability = typed
		}
	}
	if service.Reliability == nil || decision.Provider == nil {
		t.Fatal("registered providers were not threaded into tools")
	}
	if len(capability.Capabilities) != 12 || capability.Capabilities[6].Name != "service_reliability" || !capability.Capabilities[6].Configured || capability.Capabilities[7].Configured || capability.Capabilities[7].Available != versustools.CapabilityStatusFalse {
		t.Fatalf("capabilities = %+v", capability.Capabilities)
	}
}

func TestDataProvidersDoNotImplyCapabilityConfiguration(t *testing.T) {
	providers := ChatKnowledgeProviders{ServiceReliability: registrationReliability{}, AlertDecision: registrationDecision{}}
	capabilities := knowledgeCapabilities(tenancy.DefaultOrgScope(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, providers)
	for _, capability := range capabilities {
		if capability.Name == "service_reliability" || capability.Name == "alert_decisions" {
			if capability.Configured || capability.Available != versustools.CapabilityStatusUnknown {
				t.Fatalf("provider-only capability = %+v", capability)
			}
		}
	}
}

type scopedAnswerabilityStatus struct{ scope tenancy.OrgScope }

func (provider *scopedAnswerabilityStatus) CapabilityStatuses(scope tenancy.OrgScope) []versustools.CapabilityStatus {
	provider.scope = scope
	return []versustools.CapabilityStatus{
		{Name: "incidents", Licensed: versustools.CapabilityStatusFalse, Available: versustools.CapabilityStatusFalse},
		{Name: "service_reliability", Group: "reliability", Configured: true, Licensed: "TRUE", Available: "true", Observation: strings.Repeat("ready", 100)},
		{Name: "alert_decisions", Group: "decisions", Configured: false, Licensed: "invalid", Available: "invalid", SetupAction: "Enable decision storage."},
		{Name: "not_registered", Configured: true, Licensed: "true", Available: "true"},
	}
}

func TestGenericKnowledgeCatalogWithFakeProviders(t *testing.T) {
	statusProvider := &scopedAnswerabilityStatus{}
	SetChatKnowledgeProviders(ChatKnowledgeProviders{
		ServiceReliability: registrationReliability{},
		AlertDecision:      registrationDecision{},
		CapabilityStatus:   statusProvider,
	})
	t.Cleanup(func() { SetChatKnowledgeProviders(ChatKnowledgeProviders{}) })
	catalog, store := newBuildCatalog(t)
	tools := buildAnalyzeTools(store, tenancy.NewOrgScope("licensed", "default"), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil, nil, registrationHealth{})
	byName := make(map[string]any, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	for _, name := range []string{"get_system_overview", "list_services", "get_incident", "get_pattern", "get_service", "get_alert_decision", "list_capabilities"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("answerability tool %q is not registered", name)
		}
	}

	capabilityTool := byName["list_capabilities"].(versustools.ListCapabilities)
	result, err := capabilityTool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	capabilities := result.Data["capabilities"].([]versustools.CapabilityStatus)
	if len(capabilities) != 12 || len(statusProvider.scope.OrgIDs()) != 2 || statusProvider.scope.OrgIDs()[0] != "licensed" {
		t.Fatalf("capability inventory=%+v scope=%v", capabilities, statusProvider.scope.OrgIDs())
	}
	byCapability := make(map[string]versustools.CapabilityStatus, len(capabilities))
	for _, capability := range capabilities {
		byCapability[capability.Name] = capability
	}
	if !byCapability["incidents"].Configured || byCapability["incidents"].Available != versustools.CapabilityStatusTrue {
		t.Fatalf("OSS incident status was overridden: %+v", byCapability["incidents"])
	}
	if got := byCapability["service_reliability"]; got.Licensed != versustools.CapabilityStatusTrue || got.Available != versustools.CapabilityStatusTrue || len([]rune(got.Observation)) != 240 {
		t.Fatalf("reliability status = %+v", got)
	}
	if got := byCapability["alert_decisions"]; got.Licensed != versustools.CapabilityStatusUnknown || got.Available != versustools.CapabilityStatusUnknown || got.SetupAction == "" {
		t.Fatalf("decision status = %+v", got)
	}
	if _, ok := byCapability["not_registered"]; ok {
		t.Fatal("provider added an unregistered capability")
	}

	serviceResult, err := (versustools.GetService{Catalog: newCatalogAdapter(catalog)}).Invoke(context.Background(), json.RawMessage(`{"service":"checkout"}`))
	if err != nil || !serviceResult.IsAvailable() || serviceResult.Data["reliability"].(versustools.ServiceReliabilitySnapshot).SetupAction == "" {
		t.Fatalf("service guidance = %+v, err=%v", serviceResult, err)
	}
	unavailableDecision, err := (versustools.GetAlertDecision{}).Invoke(context.Background(), json.RawMessage(`{"identifier":"missing"}`))
	if err != nil || unavailableDecision.IsAvailable() || unavailableDecision.Found || unavailableDecision.Data["reason_known"] != false || unavailableDecision.Data["setup_action"] == "" {
		t.Fatalf("decision guidance = %+v, err=%v", unavailableDecision, err)
	}
}

func TestCatalogAdapterCarriesEffectiveAutoPromoteThreshold(t *testing.T) {
	catalog, _ := newBuildCatalog(t)
	catalog.Upsert("pattern", "template", "source", 1, 0, "default", "service")
	adapter := newCatalogAdapterWithThreshold(catalog, 42)
	if got := adapter.Get("pattern").AutoPromoteAfter; got != 42 {
		t.Fatalf("AutoPromoteAfter = %d, want 42", got)
	}
}

func TestCatalogAdapterWithThresholdPreservesNilInterface(t *testing.T) {
	adapter := newCatalogAdapterWithThreshold(nil, 42)
	if adapter != nil {
		t.Fatalf("adapter = %T, want nil interface", adapter)
	}
	tools := buildAnalyzeTools(nil, tenancy.DefaultOrgScope(), adapter, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, tool := range tools {
		if tool.Name() == "get_pattern" || tool.Name() == "get_service" || tool.Name() == "get_system_overview" || tool.Name() == "list_services" || tool.Name() == "list_patterns" {
			t.Fatalf("catalog tool %q registered with nil catalog", tool.Name())
		}
		if capability, ok := tool.(versustools.ListCapabilities); ok {
			for _, status := range capability.Capabilities {
				if status.Name == "catalog" && status.Configured {
					t.Fatalf("catalog capability = %+v, want unconfigured", status)
				}
			}
		}
	}
}

func TestBuildAnalyzeToolsThreadsOrgScope(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	scope := tenancy.NewOrgScope("licensed", "default")
	tools := buildAnalyzeTools(store, scope, newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	incident, ok := tools[0].(versustools.GetIncident)
	if !ok {
		t.Fatalf("tool[0] = %T, want GetIncident", tools[0])
	}
	if got := incident.Scope.OrgIDs(); len(got) != 2 || got[0] != "licensed" || got[1] != "default" {
		t.Fatalf("incident scope = %v, want [licensed default]", got)
	}
}
