package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

func coverageApp(t *testing.T, permissions map[string]bool, store *snapshot.Store, scope AWSCoverageScope) *fiber.App {
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
	NewAWSCoverageController(store, scope).Register(app.Group("/api"))
	return app
}

func TestCoverageRouteNeedsInfrastructureView(t *testing.T) {
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope())
	for name, permissions := range map[string]map[string]bool{
		"no view":      {string(core.PermissionInfrastructureView): false},
		"finops only":  {string(core.PermissionFinOpsView): true},
		"none granted": {},
	} {
		app := coverageApp(t, permissions, store, nil)
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/coverage", "")
		var refusal map[string]string
		if res.StatusCode != fiber.StatusForbidden || json.Unmarshal(body, &refusal) != nil ||
			refusal["code"] != "permission_required" || refusal["permission"] != string(core.PermissionInfrastructureView) {
			t.Fatalf("%s = %d %s", name, res.StatusCode, body)
		}
	}
}

// The route answers the summary, the three lists and the not-gap counts
// from stored unit states; it never calls AWS.
func TestCoverageRouteAnswersFromStoredStates(t *testing.T) {
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope())
	accounts := []string{"123400000001", "123400000002", "123400000003", "123400000004"}
	for _, id := range accounts {
		if _, err := store.Commit(model.Unit{Account: id, Region: "eu-south-2", Service: "sqs"}, snapshot.Candidate{
			DetailGaps: []model.Gap{{Reason: model.GapDenied, Operation: "GetQueueAttributes", Seen: 7, Total: 9}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Commit(model.Unit{Account: accounts[0], Region: "eu-south-2", Service: "elbv2"}, snapshot.Candidate{
		DetailGaps: []model.Gap{{Reason: model.GapCapped, Operation: "DescribeTargetHealth", Seen: 100, Total: 140}}}); err != nil {
		t.Fatal(err)
	}
	scope := func(context.Context) ([]snapshot.ScopeAccount, bool, error) {
		expected := make([]snapshot.ScopeAccount, 0, len(accounts))
		for _, id := range accounts {
			expected = append(expected, snapshot.ScopeAccount{Account: id, Regions: []string{"eu-south-2"}})
		}
		expected[0].NotEnabled = []string{"ap-east-1"}
		return expected, true, nil
	}
	app := coverageApp(t, map[string]bool{string(core.PermissionInfrastructureView): true}, store, scope)
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/coverage", "")
	var answer struct {
		Enabled  bool                     `json:"enabled"`
		Summary  map[string]int           `json:"summary"`
		NotRead  []snapshot.CoverageRow   `json:"not_read"`
		Capped   []snapshot.CoverageRow   `json:"capped"`
		OffInAWS []snapshot.CoverageRow   `json:"off_in_aws"`
		NotGaps  snapshot.CoverageNotGaps `json:"not_gaps"`
	}
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &answer) != nil || !answer.Enabled {
		t.Fatalf("coverage = %d %s", res.StatusCode, body)
	}
	wantSummary := map[string]int{"accounts_read": 4, "accounts_total": 4, "not_read": 1, "capped": 1, "off_in_aws": 0}
	for key, want := range wantSummary {
		if got, ok := answer.Summary[key]; !ok || got != want {
			t.Fatalf("summary %s = %d (present %t), want %d: %s", key, got, ok, want, body)
		}
	}
	if len(answer.NotRead) != 1 || len(answer.NotRead[0].Accounts) != 4 || answer.NotRead[0].Seen != 28 || answer.NotRead[0].Total != 36 ||
		len(answer.NotRead[0].Fix) != 1 || answer.NotRead[0].Fix[0] != "sqs:GetQueueAttributes" {
		t.Fatalf("not read = %+v", answer.NotRead)
	}
	if len(answer.Capped) != 1 || answer.Capped[0].Seen != 100 || answer.Capped[0].Total != 140 || answer.OffInAWS == nil {
		t.Fatalf("capped = %+v, off in AWS = %#v", answer.Capped, answer.OffInAWS)
	}
	if answer.NotGaps.RegionsNotEnabled != 1 {
		t.Fatalf("not gaps = %+v", answer.NotGaps)
	}

	for _, query := range []string{"?account=12", "?region=Mars"} {
		if res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/coverage"+query, ""); res.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("%s = %d", query, res.StatusCode)
		}
	}
	off := coverageApp(t, map[string]bool{string(core.PermissionInfrastructureView): true}, store,
		func(context.Context) ([]snapshot.ScopeAccount, bool, error) { return nil, false, nil })
	if res, body := awsTestRequest(t, off, http.MethodGet, "/api/admin/aws/coverage", ""); res.StatusCode != fiber.StatusOK || string(body) != `{"enabled":false}` {
		t.Fatalf("connector off = %d %s", res.StatusCode, body)
	}
	broken := coverageApp(t, map[string]bool{string(core.PermissionInfrastructureView): true}, store,
		func(context.Context) ([]snapshot.ScopeAccount, bool, error) {
			return nil, true, errors.New("settings down")
		})
	if res, _ := awsTestRequest(t, broken, http.MethodGet, "/api/admin/aws/coverage", ""); res.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("settings unreadable = %d", res.StatusCode)
	}
}
