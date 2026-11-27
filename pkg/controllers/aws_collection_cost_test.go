package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/callcost"
	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

func collectionCostApp(t *testing.T, permissions map[string]bool, source AWSCollectionCostSource) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		for p, allowed := range permissions {
			middleware.SetRequestPermission(ctx, p, allowed)
		}
		return ctx.Next()
	})
	NewAWSCollectionCostController(source).Register(app.Group("/api"))
	return app
}

func TestCollectionCostRouteNeedsInfrastructureView(t *testing.T) {
	called := false
	source := func(context.Context) (callcost.Inputs, bool, error) {
		called = true
		return callcost.Inputs{}, true, nil
	}
	for name, permissions := range map[string]map[string]bool{
		"no view":      {string(core.PermissionInfrastructureView): false},
		"finops only":  {string(core.PermissionFinOpsView): true},
		"none granted": {},
	} {
		app := collectionCostApp(t, permissions, source)
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings/cost", "")
		var refusal map[string]string
		if res.StatusCode != fiber.StatusForbidden || json.Unmarshal(body, &refusal) != nil ||
			refusal["permission"] != string(core.PermissionInfrastructureView) {
			t.Fatalf("%s = %d %s", name, res.StatusCode, body)
		}
	}
	if called {
		t.Fatal("a refused caller read the ledger")
	}
}

// The answer has every field of the cost card, from the ledger and the
// stored reads; a multiplier nothing has read is null, never zero.
func TestCollectionCostRouteShape(t *testing.T) {
	provider := storage.NewMemory()
	ledger, _ := storage.AWSCallCountsOf(provider)
	now := time.Date(2026, 11, 23, 12, 30, 0, 0, time.UTC)
	if err := ledger.AddAWSCallCounts(context.Background(), tenancy.DefaultOrgScope(), []storage.AWSCallCount{
		{AccountID: "123400000001", Region: "eu-south-2", Source: "elbv2", Hour: now, Calls: 300, Runs: 15},
		{AccountID: "123400000001", Region: "global", Source: "s3-policy", Hour: now, Calls: 40, Metered: 40, Runs: 1},
	}); err != nil {
		t.Fatal(err)
	}
	snapshots := snapshot.New(provider, tenancy.DefaultOrgScope())
	source := func(context.Context) (callcost.Inputs, bool, error) {
		return callcost.Inputs{Ledger: ledger, Scope: tenancy.DefaultOrgScope(), Snapshots: snapshots, Accounts: []string{"123400000001"},
			Budgets: collect.DefaultCallBudgets(), CallsPerDay: 20000, Now: now}, true, nil
	}
	app := collectionCostApp(t, map[string]bool{string(core.PermissionInfrastructureView): true}, source)
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings/cost", "")
	if res.StatusCode != fiber.StatusOK {
		t.Fatalf("cost = %d %s", res.StatusCode, body)
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"enabled", "calls_24h", "events_24h", "metered_24h", "estimate_month", "sources_capped", "stretched", "by_account_source", "budgets", "optional_off"} {
		if _, ok := answer[key]; !ok {
			t.Errorf("answer has no %s: %s", key, body)
		}
	}
	var estimate map[string]any
	if err := json.Unmarshal(answer["estimate_month"], &estimate); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"direct_usd", "guardduty_usd", "extra_trail_copies_usd", "lake_gb", "siem_gb"} {
		if _, ok := estimate[key]; !ok {
			t.Errorf("estimate has no %s: %s", key, answer["estimate_month"])
		}
	}
	// No trail read: the extra copies are not known. No GuardDuty status
	// source: the amount if it is on, marked not known.
	if estimate["extra_trail_copies_usd"] != nil || estimate["guardduty_usd"] == nil || estimate["guardduty_known"] != false {
		t.Errorf("multipliers = %s", answer["estimate_month"])
	}
	for _, key := range []string{"guardduty_known", "unknown_accounts"} {
		if _, ok := estimate[key]; !ok {
			t.Errorf("estimate has no %s", key)
		}
	}
	if string(answer["ledger_incomplete"]) != "false" {
		t.Errorf("ledger_incomplete = %s", answer["ledger_incomplete"])
	}
	if string(answer["calls_24h"]) != "340" || string(answer["metered_24h"]) != "40" || string(answer["stretched"]) != "[]" {
		t.Errorf("counts = %s / %s / %s", answer["calls_24h"], answer["metered_24h"], answer["stretched"])
	}

	off := collectionCostApp(t, map[string]bool{string(core.PermissionInfrastructureView): true},
		func(context.Context) (callcost.Inputs, bool, error) { return callcost.Inputs{}, false, nil })
	if res, body := awsTestRequest(t, off, http.MethodGet, "/api/admin/aws/settings/cost", ""); res.StatusCode != fiber.StatusOK || string(body) != `{"enabled":false}` {
		t.Fatalf("connector off = %d %s", res.StatusCode, body)
	}
	broken := collectionCostApp(t, map[string]bool{string(core.PermissionInfrastructureView): true},
		func(context.Context) (callcost.Inputs, bool, error) {
			return callcost.Inputs{}, true, errors.New("settings down")
		})
	if res, _ := awsTestRequest(t, broken, http.MethodGet, "/api/admin/aws/settings/cost", ""); res.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("settings unreadable = %d", res.StatusCode)
	}
}
