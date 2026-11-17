package services

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// costIncreaseSignal is the logical signal a cost increase carries.
const costIncreaseSignal = "cost_increase"

// budgetSignal is the logical signal a budget alert carries.
const budgetSignal = "budget_status"

// CreateIncidentFromFinOpsSignal sends a FinOps signal (a cost increase or
// a budget that moved to At risk or Over budget) through the alert path.
func CreateIncidentFromFinOpsSignal(ctx context.Context, org string, sig core.Signal) error {
	if sig.Fields[core.FieldSignal] == budgetSignal {
		return createIncidentFromBudget(ctx, org, sig)
	}
	return CreateIncidentFromCostIncrease(ctx, org, sig)
}

// createIncidentFromBudget alerts once per budget, status and month: the
// condition key carries all three, so a repeat send coalesces.
func createIncidentFromBudget(ctx context.Context, org string, sig core.Signal) error {
	org = storage.NormalizeOrgID(org)
	if err := ctx.Err(); err != nil {
		return err
	}
	id, _ := sig.Fields["budget_id"].(string)
	status, _ := sig.Fields["status"].(string)
	month, _ := sig.Fields["month"].(string)
	name, _ := sig.Fields["budget_name"].(string)
	if id == "" || (status != "at_risk" && status != "over_budget") {
		return errors.New("invalid budget signal")
	}
	message := strings.Join(strings.Fields(sig.Message), " ")
	if len(message) > 512 {
		message = message[:512]
	}
	word := "Budget at risk: "
	if status == "over_budget" {
		word = "Over budget: "
	}
	content := map[string]interface{}{
		"AlertKind": budgetSignal, "AlertName": word + name, "Severity": "medium",
		"Description": reportScrubber().Scrub(message), "ServiceName": "budget", "Service": "budget",
		"Source": sig.Source, "PatternID": "finops:budget:" + id, "Status": "firing",
	}
	if u, err := url.Parse(config.GetConfig().PublicHost); err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") && u.User == nil {
		link := u.Scheme + "://" + u.Host + "/aws/finops"
		content["FinOpsURL"] = link
		content["Links"] = []map[string]string{{"id": "finops", "label": "Open in FinOps", "url": link}}
	}
	emission := &core.DetectionEmissionResult{}
	return emitDetection(content, storage.DetectionIdentity{
		AgentKind: "finops", Source: sig.Source, Service: "budget/" + id,
		SignalKind: budgetSignal, ConditionKey: id + ":" + status + ":" + month,
	}, "medium", map[string]string{"oncall_enable": "false"}, org, 1, emission, storage.DefaultDetectionNotificationLease, ctx)
}

// CreateIncidentFromCostIncrease sends one FinOps increase through the same
// detection episode and alert fan-out as every other finding. The condition
// key carries the impact, so a repeat send of the same alert coalesces and
// only a doubled impact (decided by the FinOps core) alerts again.
func CreateIncidentFromCostIncrease(ctx context.Context, org string, sig core.Signal) error {
	org = storage.NormalizeOrgID(org)
	if err := ctx.Err(); err != nil {
		return err
	}
	id, _ := sig.Fields["increase_id"].(string)
	if sig.Fields[core.FieldSignal] != costIncreaseSignal || id == "" {
		return errors.New("invalid cost increase signal")
	}
	content := costIncreaseContent(sig, config.GetConfig().PublicHost)
	account, _ := sig.Fields["account"].(string)
	service, _ := sig.Fields[core.FieldService].(string)
	emission := &core.DetectionEmissionResult{}
	return emitDetection(content, storage.DetectionIdentity{
		AgentKind: "finops", Source: sig.Source, Service: account + "/" + service,
		SignalKind: costIncreaseSignal, ConditionKey: id + ":" + fmt.Sprint(sig.Fields["monthly_impact_micros"]),
	}, "medium", map[string]string{"oncall_enable": "false"}, org, 1, emission, storage.DefaultDetectionNotificationLease, ctx)
}

// costIncreaseContent is the alert content: computed findings only, never
// raw cost lines or tag values.
func costIncreaseContent(sig core.Signal, publicHost string) map[string]interface{} {
	id, _ := sig.Fields["increase_id"].(string)
	account, _ := sig.Fields["account"].(string)
	service, _ := sig.Fields[core.FieldService].(string)
	started, _ := sig.Fields["started_on"].(string)
	message := strings.Join(strings.Fields(sig.Message), " ")
	if len(message) > 512 {
		message = message[:512]
	}
	c := map[string]interface{}{
		"AlertKind": costIncreaseSignal, "AlertName": "Cost increase: " + service + " in account " + account,
		"Severity": "medium", "Description": reportScrubber().Scrub(message), "Account": account,
		"ServiceName": service, "Service": service, "StartedOn": started, "Source": sig.Source,
		"PatternID": "finops:" + id, "Status": "firing",
	}
	u, err := url.Parse(publicHost)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return c
	}
	link := u.Scheme + "://" + u.Host + "/aws/finops/increases/" + url.PathEscape(id)
	c["FinOpsURL"] = link
	c["Links"] = []map[string]string{{"id": "finops", "label": "Open in FinOps", "url": link}}
	return c
}
