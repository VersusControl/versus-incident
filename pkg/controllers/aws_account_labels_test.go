package controllers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func labelsApp(t *testing.T, permissions map[string]bool, store storage.AWSAccountLabelStore) *fiber.App {
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
	controller := NewAWSAccountLabelsController(store, tenancy.DefaultOrgScope())
	controller.now = func() time.Time { return time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC) }
	controller.Register(app.Group("/api"))
	return app
}

func refusedFor(t *testing.T, res *http.Response, body []byte, permission core.Permission) {
	t.Helper()
	var refusal map[string]string
	if res.StatusCode != fiber.StatusForbidden || json.Unmarshal(body, &refusal) != nil ||
		refusal["code"] != "permission_required" || refusal["permission"] != string(permission) {
		t.Fatalf("refusal = %d %s", res.StatusCode, body)
	}
}

// Reads need the infrastructure view; writes need the infrastructure
// settings permission too. Each refusal is the 403 permission_required.
func TestAccountLabelRoutesNeedTheirPermissions(t *testing.T) {
	store, _ := storage.AWSAccountLabelsOf(storage.NewMemory())
	viewOnly := map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureSettingsWrite): false}
	for name, permissions := range map[string]map[string]bool{
		"no view":     {string(core.PermissionInfrastructureView): false},
		"finops only": {string(core.PermissionFinOpsView): true},
	} {
		res, body := awsTestRequest(t, labelsApp(t, permissions, store), http.MethodGet, "/api/admin/aws/accounts/labels", "")
		t.Run(name, func(t *testing.T) { refusedFor(t, res, body, core.PermissionInfrastructureView) })
	}
	app := labelsApp(t, viewOnly, store)
	res, body := awsTestRequest(t, app, http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"Datadog"}`)
	refusedFor(t, res, body, core.PermissionInfrastructureSettingsWrite)
	res, body = awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/labels/555500006666", "")
	refusedFor(t, res, body, core.PermissionInfrastructureSettingsWrite)
	if res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/accounts/labels", ""); res.StatusCode != fiber.StatusOK {
		t.Fatalf("view read = %d %s", res.StatusCode, body)
	}
	if labels, _ := store.AWSAccountLabels(t.Context(), tenancy.DefaultOrgScope()); len(labels) != 0 {
		t.Fatalf("a refused write was stored: %+v", labels)
	}
}

// The routes answer the stored labels in a stable shape: put, list, then
// delete, and refuse a malformed account or label.
func TestAccountLabelRoutesShape(t *testing.T) {
	store, _ := storage.AWSAccountLabelsOf(storage.NewMemory())
	app := labelsApp(t, map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureSettingsWrite): true}, store)

	res, body := awsTestRequest(t, app, http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":" Datadog "}`)
	var put map[string]map[string]any
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &put) != nil {
		t.Fatalf("put = %d %s", res.StatusCode, body)
	}
	want := map[string]any{"account_id": "555500006666", "label": "known", "name": "Datadog", "created_by": "alice@example.com",
		"created_at": "2026-11-22T10:00:00Z"}
	for key, value := range want {
		if put["label"][key] != value {
			t.Fatalf("put %s = %v, want %v (%s)", key, put["label"][key], value, body)
		}
	}

	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/accounts/labels", "")
	var list struct {
		Enabled bool             `json:"enabled"`
		Labels  []map[string]any `json:"labels"`
	}
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &list) != nil || !list.Enabled || len(list.Labels) != 1 ||
		list.Labels[0]["name"] != "Datadog" || list.Labels[0]["account_id"] != "555500006666" {
		t.Fatalf("list = %d %s", res.StatusCode, body)
	}

	for path, payload := range map[string]string{
		"/api/admin/aws/accounts/labels/5555":         `{"name":"x"}`,
		"/api/admin/aws/accounts/labels/555500006666": `{"label":"connected"}`,
	} {
		if res, body := awsTestRequest(t, app, http.MethodPut, path, payload); res.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("bad put %s = %d %s", path, res.StatusCode, body)
		}
	}

	if res, body := awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/labels/555500006666", ""); res.StatusCode != fiber.StatusNoContent {
		t.Fatalf("delete = %d %s", res.StatusCode, body)
	}
	if res, body := awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/labels/555500006666", ""); res.StatusCode != fiber.StatusNotFound {
		t.Fatalf("second delete = %d %s", res.StatusCode, body)
	}

	empty := labelsApp(t, map[string]bool{string(core.PermissionInfrastructureView): true}, nil)
	if res, body := awsTestRequest(t, empty, http.MethodGet, "/api/admin/aws/accounts/labels", ""); res.StatusCode != fiber.StatusOK ||
		string(body) != `{"enabled":false,"labels":[]}` {
		t.Fatalf("no store = %d %s", res.StatusCode, body)
	}
}

// A label inherited from the default org reads as inherited; deleting it
// from the licensed org is a 409 that says so, not a 404, and it stays.
func TestAccountLabelInheritedFromDefaultOrg(t *testing.T) {
	provider := storage.NewMemory()
	store, _ := storage.AWSAccountLabelsOf(provider)
	if _, err := store.PutAWSAccountLabel(t.Context(), tenancy.DefaultOrgScope(), storage.AWSAccountLabel{AccountID: "555500006666", Name: "Datadog"}); err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite), true)
		return ctx.Next()
	})
	NewAWSAccountLabelsController(store, tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)).Register(app.Group("/api"))
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/accounts/labels", "")
	var list struct {
		Labels []map[string]any `json:"labels"`
	}
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &list) != nil || len(list.Labels) != 1 || list.Labels[0]["inherited"] != true {
		t.Fatalf("list = %d %s", res.StatusCode, body)
	}
	res, body = awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/labels/555500006666", "")
	var refusal map[string]string
	if res.StatusCode != fiber.StatusConflict || json.Unmarshal(body, &refusal) != nil || refusal["code"] != "label_inherited" {
		t.Fatalf("delete inherited = %d %s", res.StatusCode, body)
	}
	if labels, _ := store.AWSAccountLabels(t.Context(), tenancy.DefaultOrgScope()); len(labels) != 1 {
		t.Fatalf("the inherited label was removed: %+v", labels)
	}
	// Labelling the account in the licensed org overrides it.
	if res, body := awsTestRequest(t, app, http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"Datadog EU"}`); res.StatusCode != fiber.StatusOK ||
		!strings.Contains(string(body), `"name":"Datadog EU"`) || strings.Contains(string(body), "inherited") {
		t.Fatalf("override = %d %s", res.StatusCode, body)
	}
}
