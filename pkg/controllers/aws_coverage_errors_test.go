package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

type coverageInventory interface {
	storage.Provider
	storage.AWSInventoryStore
}

// coverageUnitsDown fails every unit read.
type coverageUnitsDown struct{ coverageInventory }

func (coverageUnitsDown) AWSUnits(context.Context, tenancy.OrgScope, storage.AWSUnitQuery) ([]storage.AWSUnitStatus, error) {
	return nil, errors.New("connection reset")
}

// coverageNoInventory is a provider with no AWS inventory tables.
type coverageNoInventory struct{ storage.Provider }

// A storage failure answers an error status with no coverage lists, never
// 200 with empty lists that read as "every source read in full".
func TestCoverageRouteNeverAnswersAStorageFailureAsComplete(t *testing.T) {
	view := map[string]bool{string(core.PermissionInfrastructureView): true}
	expected := func(context.Context) ([]snapshot.ScopeAccount, bool, error) {
		return []snapshot.ScopeAccount{{Account: "123400000001"}}, true, nil
	}
	for name, testCase := range map[string]struct {
		store  *snapshot.Store
		status int
	}{
		"unit read fails": {snapshot.New(coverageUnitsDown{storage.NewMemory().(coverageInventory)}, tenancy.DefaultOrgScope()), fiber.StatusInternalServerError},
		"no inventory":    {snapshot.New(coverageNoInventory{storage.NewMemory()}, tenancy.DefaultOrgScope()), fiber.StatusServiceUnavailable},
	} {
		for _, scope := range []AWSCoverageScope{nil, expected} {
			app := coverageApp(t, view, testCase.store, scope)
			res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/coverage", "")
			var answer map[string]any
			if err := json.Unmarshal(body, &answer); err != nil {
				t.Fatalf("%s: body %s: %v", name, body, err)
			}
			_, hasSummary := answer["summary"]
			_, hasList := answer["not_read"]
			if res.StatusCode != testCase.status || answer["error"] == nil || answer["enabled"] == true || hasSummary || hasList {
				t.Fatalf("%s = %d %s, want %d with an error and no coverage", name, res.StatusCode, body, testCase.status)
			}
		}
	}

	// No store at all is the connector not mounted: enabled false, never a
	// complete coverage.
	app := coverageApp(t, view, nil, expected)
	if res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/coverage", ""); res.StatusCode != fiber.StatusOK || string(body) != `{"enabled":false}` {
		t.Fatalf("no store = %d %s", res.StatusCode, body)
	}
}

// The refusal is checked before any store read: a caller without the view
// gets 403 permission_required even while storage is down.
func TestCoverageRouteRefusesBeforeReadingStorage(t *testing.T) {
	store := snapshot.New(coverageUnitsDown{storage.NewMemory().(coverageInventory)}, tenancy.DefaultOrgScope())
	called := false
	scope := func(context.Context) ([]snapshot.ScopeAccount, bool, error) {
		called = true
		return nil, true, nil
	}
	app := coverageApp(t, map[string]bool{string(core.PermissionInfrastructureView): false}, store, scope)
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/coverage?account=123400000001", "")
	var refusal map[string]string
	if res.StatusCode != fiber.StatusForbidden || json.Unmarshal(body, &refusal) != nil || refusal["code"] != "permission_required" ||
		refusal["permission"] != string(core.PermissionInfrastructureView) || called {
		t.Fatalf("refusal = %d %s (settings read %t)", res.StatusCode, body, called)
	}
}
