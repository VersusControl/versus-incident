package tools

import (
	"sync"
	"testing"
)

func TestResolveStateByRequirement(t *testing.T) {
	SetEntitlementResolver(nil)
	t.Cleanup(func() { SetEntitlementResolver(nil) })
	tests := []struct {
		name        string
		requirement Requirement
		snapshot    Snapshot
		enabled     bool
		want        State
	}{
		{"none available", Requirement{Kind: RequirementNone}, Snapshot{}, true, StateAvailable},
		{"none operator disabled", Requirement{Kind: RequirementNone}, Snapshot{}, false, StateDisabledByOperator},
		{"datasource missing masks operator", Requirement{Kind: RequirementDataSource, SignalKind: "logs"}, Snapshot{}, false, StateNeedsDataSource},
		{"datasource unhealthy masks operator", Requirement{Kind: RequirementDataSource, SignalKind: "logs"}, Snapshot{DataSources: map[string]DependencyStatus{"logs": {Configured: true, Healthy: false, Name: "logs", Health: "token=secret host=db"}}}, false, StateUnhealthy},
		{"datasource available", Requirement{Kind: RequirementDataSource, SignalKind: "logs"}, Snapshot{DataSources: map[string]DependencyStatus{"logs": {Configured: true, Healthy: true}}}, true, StateAvailable},
		{"integration missing", Requirement{Kind: RequirementIntegration, Integration: "github"}, Snapshot{}, true, StateNeedsIntegration},
		{"integration available", Requirement{Kind: RequirementIntegration, Integration: "github"}, Snapshot{Integrations: map[string]DependencyStatus{"github": {Configured: true, Healthy: true}}}, true, StateAvailable},
		{"capability missing", Requirement{Kind: RequirementCapability, Capabilities: []string{"ai_embedder", "runbook_index"}}, Snapshot{Capabilities: map[string]DependencyStatus{"ai_embedder": {Configured: true, Healthy: true}}}, true, StateNeedsCapability},
		{"compound capability available", Requirement{Kind: RequirementCapability, Capabilities: []string{"ai_embedder", "runbook_index"}}, Snapshot{Capabilities: map[string]DependencyStatus{"ai_embedder": {Configured: true, Healthy: true}, "runbook_index": {Configured: true, Healthy: true}}}, true, StateAvailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Resolve(test.requirement, test.snapshot, test.enabled)
			if got.State != test.want {
				t.Fatalf("state = %q, want %q (%+v)", got.State, test.want, got)
			}
			if got.State != StateAvailable && got.State != StateDisabledByOperator && got.Action == "" {
				t.Fatalf("state %q has no action", got.State)
			}
			if got.Health == "token=secret host=db" || containsSecret(got.Reason) {
				t.Fatalf("unsafe detail escaped: %+v", got)
			}
		})
	}
}

func TestResolveEntitlementPrecedesMissingDataSource(t *testing.T) {
	SetEntitlementResolver(func(requirement Requirement, _ DependencyStatus) EntitlementDecision {
		if requirement.Kind == RequirementDataSource && requirement.SignalKind == "metrics" {
			return EntitlementDecision{Required: true, Reason: "Metric tools need a metric data source, which is a Versus Enterprise feature.", Action: "https://versuscontrol.com/enterprise"}
		}
		return EntitlementDecision{}
	})
	t.Cleanup(func() { SetEntitlementResolver(nil) })
	got := Resolve(Requirement{Kind: RequirementDataSource, SignalKind: "metrics"}, Snapshot{}, false)
	if got.State != StateNeedsLicense || got.Action != "https://versuscontrol.com/enterprise" {
		t.Fatalf("Resolve() = %+v", got)
	}
}

func TestEntitlementResolverConcurrentInstallAndResolve(t *testing.T) {
	t.Cleanup(func() { SetEntitlementResolver(nil) })
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(2)
		go func() {
			defer wait.Done()
			SetEntitlementResolver(func(Requirement, DependencyStatus) EntitlementDecision {
				return EntitlementDecision{Required: true, Reason: "License required", Action: "https://versuscontrol.com/enterprise"}
			})
			SetEntitlementResolver(nil)
		}()
		go func() {
			defer wait.Done()
			_ = Resolve(Requirement{Kind: RequirementDataSource, SignalKind: "metrics"}, Snapshot{}, true)
		}()
	}
	wait.Wait()
}

func TestSafeActionRejectsAuthorityConfusablesAndControls(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"/settings?tab=agent", "/settings?tab=agent"},
		{"/admin#agent-ai-settings", "/admin#agent-ai-settings"},
		{"https://versuscontrol.com/enterprise", "https://versuscontrol.com/enterprise"},
		{"//evil.com", ""},
		{"/\\\\evil.com", ""},
		{"/%5cevil.com", ""},
		{"/%2fevil.com", ""},
		{"/%0aevil.com", ""},
		{"/%00evil.com", ""},
		{"/\tevil.com", ""},
		{"javascript:alert(1)", ""},
		{"https://user@evil.com/path", ""},
	}
	for _, test := range tests {
		if got := safeAction(test.value); got != test.want {
			t.Errorf("safeAction(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestResolutionExactCopyAndActions(t *testing.T) {
	tests := []struct {
		name        string
		requirement Requirement
		snapshot    Snapshot
		wantState   State
		wantReason  string
		wantAction  string
		wantLabel   string
	}{
		{"missing logs", Requirement{Kind: RequirementDataSource, SignalKind: "logs"}, Snapshot{}, StateNeedsDataSource, "No log data source is connected, so this tool cannot read log data.", "/settings?tab=agent", "Add a data source"},
		{"unhealthy logs", Requirement{Kind: RequirementDataSource, SignalKind: "logs"}, Snapshot{DataSources: map[string]DependencyStatus{"logs": {Configured: true, Name: "Log data source", Health: "connection"}}}, StateUnhealthy, "Log data source is unhealthy (connection), so this tool cannot read log data.", "/settings?tab=agent", "Configure data source"},
		{"missing GitHub", Requirement{Kind: RequirementIntegration, Integration: "github"}, Snapshot{}, StateNeedsIntegration, "GitHub is not connected, so this tool cannot read recent changes.", "/settings?tab=agent", "Connect GitHub"},
		{"unhealthy GitHub", Requirement{Kind: RequirementIntegration, Integration: "github"}, Snapshot{Integrations: map[string]DependencyStatus{"github": {Configured: true, Name: "GitHub", Health: "authentication"}}}, StateUnhealthy, "GitHub is unhealthy (authentication), so this tool cannot read recent changes.", "/settings?tab=agent", "Check GitHub"},
		{"missing graph", Requirement{Kind: RequirementCapability, Capabilities: []string{"dependency_graph"}}, Snapshot{}, StateNeedsCapability, "Dependency graph is not configured, so this tool cannot inspect service dependencies.", "/settings?tab=agent", "Configure capability"},
		{"unhealthy graph", Requirement{Kind: RequirementCapability, Capabilities: []string{"dependency_graph"}}, Snapshot{Capabilities: map[string]DependencyStatus{"dependency_graph": {Configured: true, Health: "configuration"}}}, StateUnhealthy, "Dependency graph is unhealthy (configuration), so this tool cannot use the required capability.", "/settings?tab=agent", "Configure capability"},
		{"one missing AI capability", Requirement{Kind: RequirementCapability, Capabilities: []string{"ai_embedder", "runbook_index"}}, Snapshot{Capabilities: map[string]DependencyStatus{"ai_embedder": {Configured: true, Healthy: true}}}, StateNeedsCapability, "Runbook index is not configured, so this tool cannot search runbooks.", "/admin#agent-ai-settings", "AI settings"},
		{"compound unhealthy AI capability", Requirement{Kind: RequirementCapability, Capabilities: []string{"ai_embedder", "runbook_index"}}, Snapshot{Capabilities: map[string]DependencyStatus{"ai_embedder": {Configured: true, Health: "configuration"}, "runbook_index": {Configured: true, Health: "configuration"}}}, StateUnhealthy, "AI embedder and Runbook index are unhealthy (configuration), so this tool cannot use the required capability.", "/admin#agent-ai-settings", "AI settings"},
		{"available", Requirement{Kind: RequirementNone}, Snapshot{}, StateAvailable, "Available to the agent.", "", ""},
		{"disabled", Requirement{Kind: RequirementNone}, Snapshot{}, StateDisabledByOperator, "Not offered to the agent.", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enabled := test.wantState != StateDisabledByOperator
			got := Resolve(test.requirement, test.snapshot, enabled)
			if got.State != test.wantState || got.Reason != test.wantReason || got.Action != test.wantAction || got.ActionLabel != test.wantLabel {
				t.Fatalf("resolution = %+v", got)
			}
		})
	}
}

func containsSecret(value string) bool {
	return value == "token=secret host=db"
}
