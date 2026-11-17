package controllers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	awscost "github.com/VersusControl/versus-incident/pkg/aws/cost"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
	"github.com/gofiber/fiber/v2"
)

// TestAWSFinOpsLineBudgetAnswers422 drives the FinOps routes over HTTP with a
// line budget smaller than the stored period. The page views read grouped
// totals and keep answering 200, the bridge and the increase detail without
// the resource names they could not read; a view that has to read the stored
// lines themselves (the resource breakdown, its CSV export) is refused with
// 422 cost_period_too_large and never answers a partial 200 or a 500. It runs
// on the backend TEST_STORAGE_BACKEND selects.
func TestAWSFinOpsLineBudgetAnswers422(t *testing.T) {
	if testing.Short() {
		t.Skip("integration suite needs the fakeaws HTTP stack")
	}
	stack := newFinOpsStackWith(t, fakeaws.ScenarioCostNatSpike, finopsConnector(costAccountConfig("111111111111", "Production")),
		func(opts *awscost.Options) { opts.LineBudget = 1 })
	if !stack.refresh() {
		t.Fatal("the scheduled refresh did not run")
	}
	// The next daily refresh reads resource-level cost for the service with
	// the open increase; those stored rows are what a resource view reads.
	stack.clock.Set(finopsStackDay.Add(24 * time.Hour))
	if !stack.refresh() {
		t.Fatal("the second scheduled refresh did not run")
	}
	_, summary, _ := stack.get("/api/admin/aws/finops/summary?period=2026-10")
	if jsonPath(summary, "sync", "resource_data") != true {
		t.Fatalf("the scenario stored no resource-level lines, so no view reads lines: sync=%v", jsonPath(summary, "sync"))
	}

	totals := []string{
		"/api/admin/aws/finops/summary?period=2026-10",
		"/api/admin/aws/finops/trend?period=2026-10",
		"/api/admin/aws/finops/breakdown?period=2026-10&group_by=service",
		"/api/admin/aws/finops/increases?period=2026-10&status=all",
		"/api/admin/aws/finops/accounts?period=2026-10",
		"/api/admin/aws/finops/savings?period=2026-10",
		"/api/admin/aws/finops/budgets?period=2026-10",
		"/api/admin/aws/finops/report?period=2026-10",
		"/api/admin/aws/finops/bridge?period=2026-10",
	}
	for _, path := range totals {
		if status, body := stack.do(http.MethodGet, path); status != fiber.StatusOK {
			t.Errorf("%s = %d %s, want 200 from grouped totals under a one-line budget", path, status, body)
		}
	}

	// Views that enrich totals with resource-level lines: the bridge against a
	// base period (top resource per driver) and the increase detail (its
	// resource breakdown). Their numbers are totals, so they answer 200 under
	// the budget: without resource names, saying why, never 422.
	status, bridge, _ := stack.get("/api/admin/aws/finops/bridge?period=mtd")
	if status != fiber.StatusOK || jsonPath(bridge, "available") != true {
		t.Errorf("bridge?period=mtd = %d %v, want 200 with a base period", status, bridge)
	} else if note, _ := jsonPath(bridge, "resource_note").(string); note == "" {
		t.Errorf("the bridge names no resource under the budget but does not say why: %v", bridge)
	} else {
		rows, _ := bridge["rows"].([]any)
		cut := 0
		for _, row := range rows {
			if r, _ := row.(map[string]any); r != nil && r["resources"] == "too_large" {
				cut++
			}
		}
		if cut == 0 {
			t.Errorf("no bridge row says its resources were too large: %v", rows)
		}
	}
	status, detail, _ := stack.get("/api/admin/aws/finops/increases/" + jsonString(natIncrease(t, stack), "id") + "?period=2026-10")
	if status != fiber.StatusOK || jsonPath(detail, "resource_data") != "too_large" || jsonString(detail, "resource_note") == "" {
		t.Errorf("increase detail = %d resource_data=%v note=%q, want 200 too_large with a note",
			status, jsonPath(detail, "resource_data"), jsonString(detail, "resource_note"))
	}

	lineReads := []string{
		"/api/admin/aws/finops/breakdown?period=2026-10&group_by=resource",
		"/api/admin/aws/finops/export.csv?period=2026-10&table=breakdown&group_by=resource",
	}
	for _, path := range lineReads {
		status, body := stack.do(http.MethodGet, path)
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &answer)
		t.Logf("%s -> %d %s", path, status, body)
		if status != fiber.StatusUnprocessableEntity || answer.Error != "cost_period_too_large" {
			t.Errorf("%s = %d %s, want 422 cost_period_too_large", path, status, body)
		}
	}
}

func (stack *finopsStack) do(method, path string) (int, []byte) {
	stack.t.Helper()
	response, body := stack.client.do(method, path, "")
	return response.StatusCode, body
}
