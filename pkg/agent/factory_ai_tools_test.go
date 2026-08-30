package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	versustools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/versus"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type registrationHealth struct{}

func (registrationHealth) DetectionHealth() versustools.DetectionHealthSnapshot {
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
