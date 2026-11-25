package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const correctionsPath = "/api/admin/aws/groups/corrections"

type listedCorrections struct {
	Enabled     bool             `json:"enabled"`
	Corrections []map[string]any `json:"corrections"`
}

func listCorrections(t *testing.T, app *fiber.App, query string) listedCorrections {
	t.Helper()
	res, body := awsTestRequest(t, app, http.MethodGet, correctionsPath+query, "")
	var list listedCorrections
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("list = %d %s", res.StatusCode, body)
	}
	return list
}

// A caller without the infrastructure view is refused a delete with the
// 403 permission_required, and the correction stays.
func TestGroupCorrectionDeleteNeedsTheView(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	if _, err := store.PutAWSGroupCorrection(t.Context(), tenancy.DefaultOrgScope(), storage.AWSGroupCorrection{AccountID: groupsAccount,
		Kind: storage.AWSGroupRename, GroupID: "stack:shop", Name: "Shop"}); err != nil {
		t.Fatal(err)
	}
	for name, permissions := range map[string]map[string]bool{
		"no view":        {string(core.PermissionInfrastructureView): false, string(core.PermissionInfrastructureSettingsWrite): true},
		"no permissions": {},
		"finops only":    {string(core.PermissionFinOpsView): true},
	} {
		t.Run(name, func(t *testing.T) {
			app := groupsApp(t, permissions, store, groupsInventory, tenancy.DefaultOrgScope())
			res, body := awsTestRequest(t, app, http.MethodDelete, deletePath(map[string]string{"account_id": groupsAccount, "kind": "rename",
				"group_id": "stack:shop"}), "")
			refusedFor(t, res, body, core.PermissionInfrastructureView)
		})
	}
	if stored, _ := store.AWSGroupCorrections(t.Context(), tenancy.DefaultOrgScope(), ""); len(stored) != 1 {
		t.Fatalf("a refused delete removed the correction: %+v", stored)
	}
}

// Against the real store, the account bound is a 409 correction_limit for
// a new correction; replacing one at the bound and writing for another
// account still succeed.
func TestGroupCorrectionLimitOnTheStore(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	for index := range storage.MaxAWSGroupCorrectionsPerAccount {
		if _, err := store.PutAWSGroupCorrection(t.Context(), tenancy.DefaultOrgScope(), storage.AWSGroupCorrection{AccountID: groupsAccount,
			Kind: storage.AWSGroupRename, GroupID: fmt.Sprintf("prefix:g%d-", index), Name: "n"}); err != nil {
			t.Fatal(err)
		}
	}
	app := groupsApp(t, groupsWriter, store, nil, tenancy.DefaultOrgScope())
	post := func(payload string) (int, string) {
		res, body := awsTestRequest(t, app, http.MethodPost, correctionsPath, payload)
		return res.StatusCode, string(body)
	}
	if status, body := post(`{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:shop","name":"x"}`); status != fiber.StatusConflict ||
		!strings.Contains(body, `"correction_limit"`) {
		t.Fatalf("over the bound = %d %s", status, body)
	}
	if status, body := post(`{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"prefix:g0-","name":"again"}`); status != fiber.StatusOK {
		t.Fatalf("replace at the bound = %d %s", status, body)
	}
	if status, body := post(`{"account_id":"123400000004","kind":"rename","group_id":"stack:shop","name":"x"}`); status != fiber.StatusOK {
		t.Fatalf("another account = %d %s", status, body)
	}
	if stored, _ := store.AWSGroupCorrections(t.Context(), tenancy.DefaultOrgScope(), groupsAccount); len(stored) != storage.MaxAWSGroupCorrectionsPerAccount {
		t.Fatalf("account holds %d", len(stored))
	}
}

// A licensed deployment overrides an inherited correction by writing one
// with the same key; deleting its own brings the inherited one back, which
// is again a 409 to delete.
func TestGroupCorrectionInheritedOverrideAndDelete(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	if _, err := store.PutAWSGroupCorrection(t.Context(), tenancy.DefaultOrgScope(), storage.AWSGroupCorrection{AccountID: groupsAccount,
		Kind: storage.AWSGroupRename, GroupID: "stack:shop", Name: "Old shop"}); err != nil {
		t.Fatal(err)
	}
	app := groupsApp(t, groupsWriter, store, groupsInventory, tenancy.NewOrgScope("acme", tenancy.DefaultOrgID))
	key := deletePath(map[string]string{"account_id": groupsAccount, "kind": "rename", "group_id": "stack:shop"})
	if res, body := awsTestRequest(t, app, http.MethodPost, correctionsPath,
		`{"account_id":"`+groupsAccount+`","kind":"rename","group_id":"stack:shop","name":"Shop"}`); res.StatusCode != fiber.StatusOK {
		t.Fatalf("override = %d %s", res.StatusCode, body)
	}
	list := listCorrections(t, app, "")
	if len(list.Corrections) != 1 || list.Corrections[0]["name"] != "Shop" || list.Corrections[0]["inherited"] != nil {
		t.Fatalf("after override = %+v", list.Corrections)
	}
	if res, body := awsTestRequest(t, app, http.MethodDelete, key, ""); res.StatusCode != fiber.StatusNoContent {
		t.Fatalf("delete own = %d %s", res.StatusCode, body)
	}
	list = listCorrections(t, app, "")
	if len(list.Corrections) != 1 || list.Corrections[0]["name"] != "Old shop" || list.Corrections[0]["inherited"] != true {
		t.Fatalf("after delete = %+v", list.Corrections)
	}
	res, body := awsTestRequest(t, app, http.MethodDelete, key, "")
	if res.StatusCode != fiber.StatusConflict || !strings.Contains(string(body), `"correction_inherited"`) {
		t.Fatalf("delete inherited = %d %s", res.StatusCode, body)
	}
}

// The account filter leaves each correction's status as the whole set
// gives it: a stale correction in one account does not shift the status
// shown for another account's correction.
func TestGroupCorrectionStatusUnderTheAccountFilter(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	other := "123400000004"
	inventory := func(ctx context.Context) ([]model.Resource, []string, bool, error) {
		resources, _, _, _ := groupsInventory(ctx)
		resources = append(resources, model.Resource{ARN: "arn:aws:lambda:eu-south-2:" + other + ":function:x", Service: "lambda", Type: "function",
			Account: other, Name: "x", Attributes: map[string]string{model.AttrStackName: "data"}})
		return resources, []string{groupsAccount, other}, true, nil
	}
	app := groupsApp(t, groupsWriter, store, inventory, tenancy.DefaultOrgScope())
	for _, payload := range []string{
		`{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:gone","name":"Gone"}`,
		`{"account_id":"` + other + `","kind":"rename","group_id":"stack:data","name":"Data"}`,
		`{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:shop","name":"Shop"}`,
	} {
		if res, body := awsTestRequest(t, app, http.MethodPost, correctionsPath, payload); res.StatusCode != fiber.StatusOK {
			t.Fatalf("post = %d %s", res.StatusCode, body)
		}
	}
	all := listCorrections(t, app, "")
	statuses := map[string]any{}
	for _, correction := range all.Corrections {
		statuses[correction["group_id"].(string)] = correction["status"]
	}
	if len(all.Corrections) != 3 || statuses["stack:gone"] != "no_longer_matches" || statuses["stack:data"] != "applied" || statuses["stack:shop"] != "applied" {
		t.Fatalf("all = %+v", all.Corrections)
	}
	filtered := listCorrections(t, app, "?account_id="+other)
	if len(filtered.Corrections) != 1 || filtered.Corrections[0]["group_id"] != "stack:data" || filtered.Corrections[0]["status"] != "applied" {
		t.Fatalf("filtered = %+v", filtered.Corrections)
	}
}

// Group IDs the grouping cannot make are refused with a 400 before
// anything is stored.
func TestGroupCorrectionRefusesIDsTheGroupingCannotMake(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	app := groupsApp(t, groupsWriter, store, groupsInventory, tenancy.DefaultOrgScope())
	member := "arn:aws:lambda:eu-south-2:" + groupsAccount + ":function:a"
	for name, payload := range map[string]string{
		"move into a blank user group": `{"account_id":"` + groupsAccount + `","kind":"move","member_arn":"` + member + `","target_group_id":"user: "}`,
		"merge into an unknown rule":   `{"account_id":"` + groupsAccount + `","kind":"merge","group_id":"stack:shop","target_group_id":"team:x"}`,
		"rename an empty tag group":    `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"tag:","name":"x"}`,
		"move with no target":          `{"account_id":"` + groupsAccount + `","kind":"move","member_arn":"` + member + `"}`,
		"rename a padded group":        `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":" stack:shop","name":"x"}`,
	} {
		res, body := awsTestRequest(t, app, http.MethodPost, correctionsPath, payload)
		if res.StatusCode != fiber.StatusBadRequest {
			t.Errorf("%s: %d %s", name, res.StatusCode, body)
		}
	}
	if stored, _ := store.AWSGroupCorrections(t.Context(), tenancy.DefaultOrgScope(), ""); len(stored) != 0 {
		t.Fatalf("stored = %+v", stored)
	}
}
