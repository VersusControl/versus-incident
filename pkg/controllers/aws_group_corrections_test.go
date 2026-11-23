package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const groupsAccount = "123400000002"

func groupsApp(t *testing.T, permissions map[string]bool, store storage.AWSGroupCorrectionStore, inventory AWSGroupInventory, scope tenancy.OrgScope) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestActor(ctx, "alice@example.com")
		for permission, allowed := range permissions {
			middleware.SetRequestPermission(ctx, permission, allowed)
		}
		return ctx.Next()
	})
	controller := NewAWSGroupCorrectionsController(store, scope, inventory)
	controller.now = func() time.Time { return time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC) }
	controller.Register(app.Group("/api"))
	return app
}

var groupsWriter = map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureSettingsWrite): true}

// groupsInventory is one account whose functions make the stack:shop group.
func groupsInventory(context.Context) ([]model.Resource, []string, bool, error) {
	var resources []model.Resource
	for _, name := range []string{"a", "b"} {
		resources = append(resources, model.Resource{ARN: "arn:aws:lambda:eu-south-2:" + groupsAccount + ":function:" + name, Service: "lambda",
			Type: "function", Account: groupsAccount, Name: name, Attributes: map[string]string{model.AttrStackName: "shop"}})
	}
	return resources, []string{groupsAccount}, true, nil
}

func deletePath(values map[string]string) string {
	query := url.Values{}
	for key, value := range values {
		query.Set(key, value)
	}
	return "/api/admin/aws/groups/corrections?" + query.Encode()
}

// Reads need the infrastructure view; writes need the infrastructure
// settings permission too. Each refusal is the 403 permission_required,
// and a refused write stores nothing.
func TestGroupCorrectionRoutesNeedTheirPermissions(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	for name, permissions := range map[string]map[string]bool{
		"no view":     {string(core.PermissionInfrastructureView): false},
		"finops only": {string(core.PermissionFinOpsView): true},
	} {
		app := groupsApp(t, permissions, store, groupsInventory, tenancy.DefaultOrgScope())
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/groups/corrections", "")
		t.Run(name, func(t *testing.T) { refusedFor(t, res, body, core.PermissionInfrastructureView) })
		res, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections", `{}`)
		t.Run(name+" write", func(t *testing.T) { refusedFor(t, res, body, core.PermissionInfrastructureView) })
	}
	viewOnly := map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureSettingsWrite): false}
	app := groupsApp(t, viewOnly, store, groupsInventory, tenancy.DefaultOrgScope())
	res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections",
		`{"account_id":"`+groupsAccount+`","kind":"rename","group_id":"stack:shop","name":"Shop"}`)
	refusedFor(t, res, body, core.PermissionInfrastructureSettingsWrite)
	res, body = awsTestRequest(t, app, http.MethodDelete, deletePath(map[string]string{"account_id": groupsAccount, "kind": "rename", "group_id": "stack:shop"}), "")
	refusedFor(t, res, body, core.PermissionInfrastructureSettingsWrite)
	if res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/groups/corrections", ""); res.StatusCode != fiber.StatusOK {
		t.Fatalf("view read = %d %s", res.StatusCode, body)
	}
	if stored, _ := store.AWSGroupCorrections(t.Context(), tenancy.DefaultOrgScope(), ""); len(stored) != 0 {
		t.Fatalf("a refused write was stored: %+v", stored)
	}
}

// The routes answer in a stable shape: post, list with each correction's
// status, filter by account, then delete.
func TestGroupCorrectionRoutesShape(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	app := groupsApp(t, groupsWriter, store, groupsInventory, tenancy.DefaultOrgScope())
	member := "arn:aws:lambda:eu-south-2:" + groupsAccount + ":function:a"

	res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections",
		`{"account_id":"`+groupsAccount+`","kind":"rename","group_id":"stack:shop","name":" Shop "}`)
	var put map[string]map[string]any
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &put) != nil {
		t.Fatalf("post = %d %s", res.StatusCode, body)
	}
	want := map[string]any{"account_id": groupsAccount, "kind": "rename", "group_id": "stack:shop", "name": "Shop",
		"created_by": "alice@example.com", "created_at": "2026-11-22T10:00:00Z"}
	if len(put["correction"]) != len(want) {
		t.Fatalf("post fields = %s", body)
	}
	for key, value := range want {
		if put["correction"][key] != value {
			t.Fatalf("post %s = %v, want %v (%s)", key, put["correction"][key], value, body)
		}
	}
	for _, payload := range []string{
		`{"account_id":"` + groupsAccount + `","kind":"merge","group_id":"prefix:cart-","target_group_id":"stack:shop"}`,
		`{"account_id":"` + groupsAccount + `","kind":"move","member_arn":"` + member + `","target_group_id":"user:receipts"}`,
		`{"account_id":"123400000004","kind":"rename","group_id":"vpc:vpc-1","name":"Data network"}`,
	} {
		if res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections", payload); res.StatusCode != fiber.StatusOK {
			t.Fatalf("post %s = %d %s", payload, res.StatusCode, body)
		}
	}

	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/groups/corrections?account_id="+groupsAccount, "")
	var list struct {
		Enabled     bool             `json:"enabled"`
		Corrections []map[string]any `json:"corrections"`
	}
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &list) != nil || !list.Enabled || len(list.Corrections) != 3 {
		t.Fatalf("list = %d %s", res.StatusCode, body)
	}
	statuses := map[string]any{}
	for _, correction := range list.Corrections {
		statuses[correction["kind"].(string)] = correction["status"]
	}
	if statuses["rename"] != "applied" || statuses["merge"] != "no_longer_matches" || statuses["move"] != "applied" {
		t.Fatalf("statuses = %v (%s)", statuses, body)
	}
	if res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/groups/corrections?account_id=12", ""); res.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("bad account filter = %d %s", res.StatusCode, body)
	}

	moveKey := map[string]string{"account_id": groupsAccount, "kind": "move", "member_arn": member}
	if res, body := awsTestRequest(t, app, http.MethodDelete, deletePath(moveKey), ""); res.StatusCode != fiber.StatusNoContent {
		t.Fatalf("delete = %d %s", res.StatusCode, body)
	}
	if res, body := awsTestRequest(t, app, http.MethodDelete, deletePath(moveKey), ""); res.StatusCode != fiber.StatusNotFound {
		t.Fatalf("second delete = %d %s", res.StatusCode, body)
	}

	empty := groupsApp(t, map[string]bool{string(core.PermissionInfrastructureView): true}, nil, nil, tenancy.DefaultOrgScope())
	if res, body := awsTestRequest(t, empty, http.MethodGet, "/api/admin/aws/groups/corrections", ""); res.StatusCode != fiber.StatusOK ||
		string(body) != `{"enabled":false,"corrections":[]}` {
		t.Fatalf("no store = %d %s", res.StatusCode, body)
	}
	noInventory := groupsApp(t, groupsWriter, store, nil, tenancy.DefaultOrgScope())
	if res, body := awsTestRequest(t, noInventory, http.MethodGet, "/api/admin/aws/groups/corrections", ""); res.StatusCode != fiber.StatusOK ||
		strings.Contains(string(body), `"status"`) {
		t.Fatalf("no inventory = %d %s", res.StatusCode, body)
	}
	failing := groupsApp(t, groupsWriter, store, func(context.Context) ([]model.Resource, []string, bool, error) {
		return nil, nil, false, errors.New("inventory down")
	}, tenancy.DefaultOrgScope())
	if res, body := awsTestRequest(t, failing, http.MethodGet, "/api/admin/aws/groups/corrections", ""); res.StatusCode != fiber.StatusInternalServerError ||
		strings.Contains(string(body), "inventory down") {
		t.Fatalf("failing inventory = %d %s", res.StatusCode, body)
	}
}

// Malformed corrections are refused with a 400 before anything is stored:
// bad accounts, kinds, group IDs, ARNs, names, and Ungrouped as anything
// but a move target.
func TestGroupCorrectionValidation(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	app := groupsApp(t, groupsWriter, store, groupsInventory, tenancy.DefaultOrgScope())
	member := "arn:aws:lambda:eu-south-2:" + groupsAccount + ":function:a"
	for name, payload := range map[string]string{
		"not json":            `{`,
		"short account":       `{"account_id":"1234","kind":"rename","group_id":"stack:shop","name":"x"}`,
		"unknown kind":        `{"account_id":"` + groupsAccount + `","kind":"split","group_id":"stack:shop"}`,
		"unknown rule":        `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"shop","name":"x"}`,
		"rename ungrouped":    `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"ungrouped","name":"x"}`,
		"merge ungrouped":     `{"account_id":"` + groupsAccount + `","kind":"merge","group_id":"ungrouped","target_group_id":"stack:shop"}`,
		"merge into ungroup":  `{"account_id":"` + groupsAccount + `","kind":"merge","group_id":"stack:shop","target_group_id":"ungrouped"}`,
		"rename no name":      `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:shop"}`,
		"rename control":      `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:shop","name":"a\u0007b"}`,
		"rename long":         `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:shop","name":"` + strings.Repeat("x", 129) + `"}`,
		"group control":       `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:a\nb","name":"x"}`,
		"group too long":      `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:` + strings.Repeat("x", 600) + `","name":"x"}`,
		"move not an arn":     `{"account_id":"` + groupsAccount + `","kind":"move","member_arn":"i-0abc","target_group_id":"stack:shop"}`,
		"move other account":  `{"account_id":"` + groupsAccount + `","kind":"move","member_arn":"arn:aws:lambda:eu-south-2:123400000004:function:a","target_group_id":"stack:shop"}`,
		"move bad target":     `{"account_id":"` + groupsAccount + `","kind":"move","member_arn":"` + member + `","target_group_id":"shop"}`,
		"move with group":     `{"account_id":"` + groupsAccount + `","kind":"move","member_arn":"` + member + `","group_id":"stack:shop","target_group_id":"ungrouped"}`,
		"rename with member":  `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:shop","name":"x","member_arn":"` + member + `"}`,
		"move with name":      `{"account_id":"` + groupsAccount + `","kind":"move","member_arn":"` + member + `","target_group_id":"stack:shop","name":"x"}`,
		"merge with a target": `{"account_id":"` + groupsAccount + `","kind":"merge","group_id":"stack:shop"}`,
	} {
		res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections", payload)
		if res.StatusCode != fiber.StatusBadRequest || !strings.Contains(string(body), `"error"`) {
			t.Errorf("%s: %d %s", name, res.StatusCode, body)
		}
	}
	if res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections",
		`{"account_id":"`+groupsAccount+`","kind":"move","member_arn":"`+member+`","target_group_id":"ungrouped"}`); res.StatusCode != fiber.StatusOK {
		t.Fatalf("move into Ungrouped = %d %s", res.StatusCode, body)
	}
	for name, key := range map[string]map[string]string{
		"no key":      {},
		"bad kind":    {"account_id": groupsAccount, "kind": "split", "group_id": "stack:shop"},
		"move group":  {"account_id": groupsAccount, "kind": "move", "group_id": "stack:shop"},
		"bad account": {"account_id": "x", "kind": "rename", "group_id": "stack:shop"},
	} {
		if res, body := awsTestRequest(t, app, http.MethodDelete, deletePath(key), ""); res.StatusCode != fiber.StatusBadRequest {
			t.Errorf("delete %s = %d %s", name, res.StatusCode, body)
		}
	}
	if stored, _ := store.AWSGroupCorrections(t.Context(), tenancy.DefaultOrgScope(), ""); len(stored) != 1 {
		t.Fatalf("stored = %+v", stored)
	}
}

// A correction inherited from the default org reads as inherited;
// deleting it from the licensed org is a 409 that says so, and it stays.
func TestGroupCorrectionInheritedFromDefaultOrg(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	if _, err := store.PutAWSGroupCorrection(t.Context(), tenancy.DefaultOrgScope(), storage.AWSGroupCorrection{AccountID: groupsAccount,
		Kind: storage.AWSGroupRename, GroupID: "stack:shop", Name: "Shop"}); err != nil {
		t.Fatal(err)
	}
	app := groupsApp(t, groupsWriter, store, groupsInventory, tenancy.NewOrgScope("acme", tenancy.DefaultOrgID))
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/groups/corrections", "")
	var list struct {
		Corrections []map[string]any `json:"corrections"`
	}
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &list) != nil || len(list.Corrections) != 1 ||
		list.Corrections[0]["inherited"] != true || list.Corrections[0]["status"] != "applied" {
		t.Fatalf("list = %d %s", res.StatusCode, body)
	}
	res, body = awsTestRequest(t, app, http.MethodDelete, deletePath(map[string]string{"account_id": groupsAccount, "kind": "rename", "group_id": "stack:shop"}), "")
	var refusal map[string]string
	if res.StatusCode != fiber.StatusConflict || json.Unmarshal(body, &refusal) != nil || refusal["code"] != "correction_inherited" {
		t.Fatalf("delete inherited = %d %s", res.StatusCode, body)
	}
	if stored, _ := store.AWSGroupCorrections(t.Context(), tenancy.DefaultOrgScope(), ""); len(stored) != 1 {
		t.Fatalf("the inherited correction was deleted: %+v", stored)
	}
}

// A store that refuses a new correction over the account's bound answers
// 409 correction_limit.
func TestGroupCorrectionLimitIsAConflict(t *testing.T) {
	app := groupsApp(t, groupsWriter, limitedCorrections{}, nil, tenancy.DefaultOrgScope())
	res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections",
		`{"account_id":"`+groupsAccount+`","kind":"rename","group_id":"stack:shop","name":"x"}`)
	if res.StatusCode != fiber.StatusConflict || !strings.Contains(string(body), "correction_limit") {
		t.Fatalf("over the limit = %d %s", res.StatusCode, body)
	}
}

type limitedCorrections struct {
	storage.AWSGroupCorrectionStore
}

func (limitedCorrections) PutAWSGroupCorrection(context.Context, tenancy.OrgScope, storage.AWSGroupCorrection) (storage.AWSGroupCorrection, error) {
	return storage.AWSGroupCorrection{}, storage.ErrAWSGroupCorrectionLimit
}

// A licensed deployment cannot delete a merge it inherits, but it undoes
// it by posting the neutral merge of the group into itself: the group
// stays whole and the neutral row reads applied.
func TestGroupCorrectionNeutralMergeUndoesAnInheritedMerge(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	other := "arn:aws:lambda:eu-south-2:" + groupsAccount + ":function:"
	inventory := func(context.Context) ([]model.Resource, []string, bool, error) {
		var resources []model.Resource
		for name, stack := range map[string]string{"a": "shop", "b": "shop", "c": "cart", "d": "cart"} {
			resources = append(resources, model.Resource{ARN: other + name, Service: "lambda", Type: "function", Account: groupsAccount,
				Name: name, Attributes: map[string]string{model.AttrStackName: stack}})
		}
		return resources, []string{groupsAccount}, true, nil
	}
	if _, err := store.PutAWSGroupCorrection(t.Context(), tenancy.DefaultOrgScope(), storage.AWSGroupCorrection{AccountID: groupsAccount,
		Kind: storage.AWSGroupMerge, GroupID: "stack:cart", TargetGroupID: "stack:shop"}); err != nil {
		t.Fatal(err)
	}
	app := groupsApp(t, groupsWriter, store, inventory, tenancy.NewOrgScope("acme", tenancy.DefaultOrgID))
	key := deletePath(map[string]string{"account_id": groupsAccount, "kind": "merge", "group_id": "stack:cart"})
	if res, body := awsTestRequest(t, app, http.MethodDelete, key, ""); res.StatusCode != fiber.StatusConflict {
		t.Fatalf("delete inherited merge = %d %s", res.StatusCode, body)
	}
	if res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections",
		`{"account_id":"`+groupsAccount+`","kind":"merge","group_id":"stack:cart","target_group_id":"stack:cart"}`); res.StatusCode != fiber.StatusOK {
		t.Fatalf("neutral merge = %d %s", res.StatusCode, body)
	}
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/groups/corrections", "")
	var list struct {
		Corrections []map[string]any `json:"corrections"`
	}
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &list) != nil || len(list.Corrections) != 1 ||
		list.Corrections[0]["target_group_id"] != "stack:cart" || list.Corrections[0]["inherited"] != nil || list.Corrections[0]["status"] != "applied" {
		t.Fatalf("list = %d %s", res.StatusCode, body)
	}
}

// Names a user sees are refused when they hide or reorder text: C1
// controls, bidirectional controls and zero-width characters, in a rename
// and in the name of a group a move makes; that name obeys the length
// limit too.
func TestGroupCorrectionRefusesHiddenCharactersInNames(t *testing.T) {
	store, _ := storage.AWSGroupCorrectionsOf(storage.NewMemory())
	app := groupsApp(t, groupsWriter, store, groupsInventory, tenancy.DefaultOrgScope())
	member := "arn:aws:lambda:eu-south-2:" + groupsAccount + ":function:a"
	rename := func(name string) string {
		return `{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:shop","name":"` + name + `"}`
	}
	moveTo := func(target string) string {
		return `{"account_id":"` + groupsAccount + `","kind":"move","member_arn":"` + member + `","target_group_id":"` + target + `"}`
	}
	for name, payload := range map[string]string{
		"c1 control":       rename(`a\u0085b`),
		"right-to-left":    rename(`shop\u202epot`),
		"isolate":          rename(`a\u2066b`),
		"zero width":       rename(`sh\u200bop`),
		"byte order mark":  rename(`\ufeffshop`),
		"user group long":  moveTo("user:" + strings.Repeat("x", 129)),
		"user group bidi":  moveTo(`user:a\u202eb`),
		"user group space": moveTo(`user:a\u200db`),
	} {
		if res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections", payload); res.StatusCode != fiber.StatusBadRequest {
			t.Errorf("%s: %d %s", name, res.StatusCode, body)
		}
	}
	if res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/groups/corrections", moveTo("user:"+strings.Repeat("界", 128))); res.StatusCode != fiber.StatusOK {
		t.Fatalf("user group at the limit = %d %s", res.StatusCode, body)
	}
	if stored, _ := store.AWSGroupCorrections(t.Context(), tenancy.DefaultOrgScope(), ""); len(stored) != 1 {
		t.Fatalf("stored = %+v", stored)
	}
}
