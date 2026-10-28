package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type reliabilityProvider struct{ scope tenancy.OrgScope }

func (provider *reliabilityProvider) ServiceReliability(_ context.Context, scope tenancy.OrgScope, service string) (ServiceReliabilitySnapshot, error) {
	provider.scope = scope
	return ServiceReliabilitySnapshot{
		Service: service, SLIs: []ServiceIndicator{{Name: "availability"}},
		SLOs:   []ServiceObjective{{Name: "availability", SLI: "availability", Target: 0.999}},
		Source: "test", Observation: "configured", Configured: true,
		Licensed: CapabilityStatusTrue, Available: CapabilityStatusTrue,
	}, nil
}

type oversizedReliabilityProvider struct{}

func (oversizedReliabilityProvider) ServiceReliability(_ context.Context, _ tenancy.OrgScope, _ string) (ServiceReliabilitySnapshot, error) {
	items := make([]ServiceIndicator, reliabilityItemLimit+5)
	for index := range items {
		items[index] = ServiceIndicator{Name: strings.Repeat("secret", 100), Description: strings.Repeat("secret", 100)}
	}
	return ServiceReliabilitySnapshot{Service: "secret", SLIs: items, Licensed: CapabilityStatusTrue, Available: CapabilityStatusTrue}, nil
}

func TestGetServiceBoundsAndRedactsArbitraryProviderOutput(t *testing.T) {
	result, err := (GetService{Reliability: oversizedReliabilityProvider{}, Redactor: secretRedactor{}}).Invoke(context.Background(), json.RawMessage(`{"service":"checkout"}`))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := result.Data["reliability"].(ServiceReliabilitySnapshot)
	if len(snapshot.SLIs) != reliabilityItemLimit || strings.Contains(snapshot.Service+snapshot.SLIs[0].Name+snapshot.SLIs[0].Description, "secret") {
		t.Fatalf("provider output was not bounded and redacted: %+v", snapshot)
	}
}

func TestGetServiceIncludesReliabilityAndPreservesBaseFactsWithoutProvider(t *testing.T) {
	catalog := &fakeCatalog{services: map[string]ServiceInfo{"checkout": {}}, all: []*PatternView{{ID: "p1", Service: "checkout"}}}
	withoutProvider, err := (GetService{Catalog: catalog}).Invoke(context.Background(), json.RawMessage(`{"service":"checkout"}`))
	if err != nil {
		t.Fatal(err)
	}
	reliability := withoutProvider.Data["reliability"].(ServiceReliabilitySnapshot)
	if !withoutProvider.Found || reliability.Available != CapabilityStatusFalse || reliability.SetupAction == "" {
		t.Fatalf("base result = %+v", withoutProvider)
	}

	provider := &reliabilityProvider{}
	result, err := (GetService{Catalog: catalog, Reliability: provider, Scope: tenancy.NewOrgScope("license", "default")}).Invoke(context.Background(), json.RawMessage(`{"service":"checkout"}`))
	if err != nil {
		t.Fatal(err)
	}
	reliability = result.Data["reliability"].(ServiceReliabilitySnapshot)
	if len(reliability.SLIs) != 1 || len(reliability.SLOs) != 1 || provider.scope.OrgIDs()[0] != "license" {
		t.Fatalf("reliability=%+v scope=%v", reliability, provider.scope.OrgIDs())
	}
}

type alertDecisionProvider struct {
	scope tenancy.OrgScope
	found bool
}

func (provider *alertDecisionProvider) AlertDecision(_ context.Context, scope tenancy.OrgScope, identifier string) (AlertDecisionSnapshot, error) {
	provider.scope = scope
	evidence := make([]string, alertDecisionEvidenceLimit+2)
	for index := range evidence {
		evidence[index] = fmt.Sprintf("evidence %d secret", index)
	}
	return AlertDecisionSnapshot{
		Identifier: identifier, Found: provider.found, DecisionType: "spam", Status: "applied",
		Action: "suppress", Outcome: "notification skipped", ReasonKnown: false,
		Evidence: evidence, Provenance: "stored_rule", Fingerprint: identifier,
	}, nil
}

func TestGetAlertDecisionUnavailableScopedBoundedAndRedacted(t *testing.T) {
	unavailable, err := (GetAlertDecision{}).Invoke(context.Background(), json.RawMessage(`{"identifier":"fp-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if unavailable.IsAvailable() || unavailable.Data["reason_known"] != false || unavailable.Data["setup_action"] == "" {
		t.Fatalf("unavailable result = %+v", unavailable)
	}

	provider := &alertDecisionProvider{found: true}
	result, err := (GetAlertDecision{Provider: provider, Scope: tenancy.NewOrgScope("license", "default"), Redactor: secretRedactor{}}).Invoke(context.Background(), json.RawMessage(`{"identifier":"fp-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	decision := result.Data["decision"].(AlertDecisionSnapshot)
	if !result.Found || decision.DecisionType != "spam" || decision.ReasonKnown || len(decision.Evidence) != alertDecisionEvidenceLimit || provider.scope.OrgIDs()[0] != "license" {
		t.Fatalf("decision=%+v scope=%v", decision, provider.scope.OrgIDs())
	}
	for _, evidence := range decision.Evidence {
		if evidence == "" || evidence[len(evidence)-6:] == "secret" {
			t.Fatalf("unredacted evidence = %q", evidence)
		}
	}
}

func TestGetAlertDecisionPreservesProviderNotFound(t *testing.T) {
	result, err := (GetAlertDecision{Provider: &alertDecisionProvider{}}).Invoke(context.Background(), json.RawMessage(`{"identifier":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	decision := result.Data["decision"].(AlertDecisionSnapshot)
	if result.Found || decision.Found || decision.ReasonKnown {
		t.Fatalf("missing decision = %+v", result)
	}
}

func TestListCapabilitiesReturnsDetachedFilteredInventory(t *testing.T) {
	configured := []CapabilityStatus{
		{Name: "sli_slo", Group: "reliability", Configured: false, Licensed: CapabilityStatusUnknown, Available: CapabilityStatusFalse, SetupAction: "configure provider"},
		{Name: "alert_decisions", Group: "decisions", Available: CapabilityStatusTrue},
	}
	result, err := (ListCapabilities{Capabilities: configured}).Invoke(context.Background(), json.RawMessage(`{"group":"RELIABILITY"}`))
	if err != nil {
		t.Fatal(err)
	}
	items := result.Data["capabilities"].([]CapabilityStatus)
	if len(items) != 1 || items[0].Name != "sli_slo" {
		t.Fatalf("filtered capabilities = %+v", items)
	}
	items[0].Name = "changed"
	if configured[0].Name != "sli_slo" {
		t.Fatal("capability inventory aliases caller data")
	}
}
