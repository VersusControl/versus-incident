package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func costSignal(impact int64) core.Signal {
	return core.Signal{Source: "aws-cost:prod", Timestamp: time.Now(), Severity: "warning",
		Message: "Network traffic (NAT) in account 111111111111 went up about $1,950 a month, starting 2026-10-25.",
		Fields: map[string]interface{}{core.FieldService: "network.nat", core.FieldSignal: "cost_increase",
			"increase_id": "abc123", "account": "111111111111", "started_on": "2026-10-25", "monthly_impact_micros": impact}}
}

func TestCostIncreaseAlertsOnceThroughTheSharedEpisode(t *testing.T) {
	loadAgentTestConfig(t)
	previous := store
	SetStorage(storage.NewMemory())
	defer SetStorage(previous)
	var sent []string
	SetEmitInterceptor(func(content map[string]interface{}, _ string) EmitDecision {
		sent = append(sent, content["AlertKind"].(string))
		return EmitDecision{Action: EmitProceed}
	})
	defer SetEmitInterceptor(nil)
	if err := CreateIncidentFromCostIncrease(context.Background(), "", costSignal(1_950_000_000)); err != nil {
		t.Fatal(err)
	}
	if err := CreateIncidentFromCostIncrease(context.Background(), "", costSignal(1_950_000_000)); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != "cost_increase" {
		t.Fatalf("a repeated send of one alert pages once: %v", sent)
	}
	bad := costSignal(1)
	bad.Fields[core.FieldSignal] = "other"
	if err := CreateIncidentFromCostIncrease(context.Background(), "", bad); err == nil {
		t.Fatal("only cost increase signals are accepted")
	}
}

func TestCostIncreaseContentCarriesFindingsAndLink(t *testing.T) {
	c := costIncreaseContent(costSignal(5), "https://versus.example")
	if c["AlertKind"] != "cost_increase" || !strings.Contains(c["FinOpsURL"].(string), "/aws/finops/increases/abc123") {
		t.Fatal(c)
	}
	if _, ok := costIncreaseContent(costSignal(5), "")["Links"]; ok {
		t.Fatal("no link without a public origin")
	}
}

func budgetSig(status string) core.Signal {
	return core.Signal{Source: "aws-cost:prod", Timestamp: time.Now(), Message: "Budget AWS total is at risk for 2026-11.",
		Fields: map[string]interface{}{core.FieldSignal: "budget_status", core.FieldService: "budget", "budget_id": "b1",
			"budget_name": "AWS total", "status": status, "month": "2026-11"}}
}

func TestBudgetAlertsOnceThroughTheSharedEpisode(t *testing.T) {
	loadAgentTestConfig(t)
	previous := store
	SetStorage(storage.NewMemory())
	defer SetStorage(previous)
	var sent []string
	SetEmitInterceptor(func(content map[string]interface{}, _ string) EmitDecision {
		sent = append(sent, content["AlertName"].(string))
		return EmitDecision{Action: EmitProceed}
	})
	defer SetEmitInterceptor(nil)
	for range 2 {
		if err := CreateIncidentFromFinOpsSignal(context.Background(), "", budgetSig("at_risk")); err != nil {
			t.Fatal(err)
		}
	}
	if err := CreateIncidentFromFinOpsSignal(context.Background(), "", budgetSig("over_budget")); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0] != "Budget at risk: AWS total" || sent[1] != "Over budget: AWS total" {
		t.Fatalf("budget alerts %v", sent)
	}
	if err := CreateIncidentFromFinOpsSignal(context.Background(), "", budgetSig("on_track")); err == nil {
		t.Fatal("on track never alerts")
	}
}
