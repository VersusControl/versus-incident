package controllers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
)

// kubernetesSettingsAuditApp mounts the Kubernetes settings audit wrapper in
// front of stand-in write handlers, so the wrapper's own route decision is
// tested without a cluster. The X-Deny header plays a refused permission.
func kubernetesSettingsAuditApp(t *testing.T, caseSensitive bool) (*fiber.App, *[]middleware.AdminAuditEvent) {
	t.Helper()
	events := make([]middleware.AdminAuditEvent, 0)
	middleware.SetAdminAuditHook(func(_ *fiber.Ctx, event middleware.AdminAuditEvent) { events = append(events, event) })
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	app := fiber.New(fiber.Config{Immutable: true, CaseSensitive: caseSensitive})
	group := app.Group("/api/admin/kubernetes", auditKubernetesSettings, func(ctx *fiber.Ctx) error {
		if ctx.Get("X-Deny") != "" {
			return ctx.SendStatus(fiber.StatusForbidden)
		}
		return ctx.Next()
	})
	ok := func(ctx *fiber.Ctx) error { return ctx.SendStatus(fiber.StatusOK) }
	group.Put("/agent-settings", ok)
	group.Post("/agent-settings/test-link", ok)
	return app, &events
}

func kubernetesAuditRequest(t *testing.T, app *fiber.App, method, path string, deny bool) int {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	if deny {
		request.Header.Set("X-Deny", "1")
	}
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

func TestKubernetesSettingsAuditIgnoresPathCaseOnCaseInsensitiveApp(t *testing.T) {
	for _, test := range []struct {
		method, path string
		deny         bool
		status       int
		action       string
	}{
		{http.MethodPut, "/api/admin/kubernetes/agent-settings", false, 200, "kubernetes.agent-settings.changed"},
		{http.MethodPut, "/api/admin/Kubernetes/Agent-Settings", false, 200, "kubernetes.agent-settings.changed"},
		{http.MethodPut, "/API/ADMIN/KUBERNETES/AGENT-SETTINGS/", false, 200, "kubernetes.agent-settings.changed"},
		{http.MethodPut, "/api/admin/kubernetes/agent-settings//", false, 200, "kubernetes.agent-settings.changed"},
		{http.MethodPut, "/api/admin/Kubernetes/Agent-Settings///", false, 200, "kubernetes.agent-settings.changed"},
		{http.MethodPost, "/api/admin/kubernetes/agent-settings/test-link//", true, 403, "rbac.denied"},
		{http.MethodPut, "/api/admin/Kubernetes/Agent-Settings", true, 403, "rbac.denied"},
		{http.MethodPost, "/api/admin/kubernetes/agent-settings/test-link", true, 403, "rbac.denied"},
		{http.MethodPost, "/api/admin/Kubernetes/Agent-Settings/Test-Link", true, 403, "rbac.denied"},
	} {
		app, events := kubernetesSettingsAuditApp(t, false)
		if status := kubernetesAuditRequest(t, app, test.method, test.path, test.deny); status != test.status {
			t.Fatalf("%s %s: status %d want %d", test.method, test.path, status, test.status)
		}
		if len(*events) != 1 || (*events)[0].Action != test.action {
			t.Fatalf("%s %s: audit %+v, want exactly one %s", test.method, test.path, *events, test.action)
		}
	}
	app, events := kubernetesSettingsAuditApp(t, false)
	if status := kubernetesAuditRequest(t, app, http.MethodPost, "/api/admin/Kubernetes/Agent-Settings/Test-Link", false); status != 200 || len(*events) != 0 {
		t.Fatalf("an allowed link test is not a settings write: status %d audit %+v", status, *events)
	}
}

func TestKubernetesSettingsAuditOnCaseSensitiveAppIsUnchanged(t *testing.T) {
	app, events := kubernetesSettingsAuditApp(t, true)
	if status := kubernetesAuditRequest(t, app, http.MethodPut, "/api/admin/kubernetes/agent-settings", false); status != 200 || len(*events) != 1 {
		t.Fatalf("lowercase write: status %d audit %+v", status, *events)
	}
	app, events = kubernetesSettingsAuditApp(t, true)
	if status := kubernetesAuditRequest(t, app, http.MethodPut, "/api/admin/Kubernetes/Agent-Settings", false); status != fiber.StatusNotFound || len(*events) != 0 {
		t.Fatalf("mixed-case path on a case-sensitive app: status %d audit %+v", status, *events)
	}
}

func TestAgentAPIv1RouteKindFollowsTheMatchedRoute(t *testing.T) {
	app := fiber.New(fiber.Config{Immutable: true})
	kind := func(ctx *fiber.Ctx) error {
		if isAgentAPIv1(ctx) {
			return ctx.SendString("v1")
		}
		return ctx.SendString("legacy")
	}
	app.Group("/api/v1/agent").Post("/sessions/:id/turns", kind)
	app.Group("/api/admin/chat").Post("/sessions/:id/messages", kind)
	for path, want := range map[string]string{
		"/api/v1/agent/sessions/AbC/turns":     "v1",
		"/API/V1/Agent/Sessions/AbC/Turns":     "v1",
		"/api/admin/chat/sessions/v1/messages": "legacy",
	} {
		response, err := app.Test(httptest.NewRequest(http.MethodPost, path, nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		_, _ = body.ReadFrom(response.Body)
		response.Body.Close()
		if body.String() != want {
			t.Fatalf("%s: kind %q want %q", path, body.String(), want)
		}
	}
}

func TestPersistedHistoryRequestIgnoresPathCaseOnCaseInsensitiveApp(t *testing.T) {
	for _, caseSensitive := range []bool{false, true} {
		app := fiber.New(fiber.Config{Immutable: true, CaseSensitive: caseSensitive})
		app.Use(func(ctx *fiber.Ctx) error {
			if persistedHistoryRequest(ctx) {
				return ctx.SendString("persisted")
			}
			return ctx.SendString("live")
		})
		for path, want := range map[string]string{
			"/api/admin/kubernetes/timeline": "persisted",
			"/api/admin/Kubernetes/TimeLine": map[bool]string{false: "persisted", true: "live"}[caseSensitive],
			"/api/admin/kubernetes/pods":     "live",
		} {
			response, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil), -1)
			if err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			_, _ = body.ReadFrom(response.Body)
			response.Body.Close()
			if body.String() != want {
				t.Fatalf("case-sensitive=%v %s: %q want %q", caseSensitive, path, body.String(), want)
			}
		}
	}
}

func TestPermissionGuardsAnswerAStableDenialBody(t *testing.T) {
	for _, test := range []struct {
		name       string
		guard      fiber.Handler
		permission core.Permission
	}{
		{"changes view", requireChangesView, core.PermissionInfrastructureView},
		{"finops view", requireFinOpsView, core.PermissionInfrastructureView},
		{"finops write", requireFinOpsWrite, core.PermissionInfrastructureSettingsWrite},
		{"aws settings write", (&AWSSettingsController{}).requireSettingsWrite, core.PermissionInfrastructureSettingsWrite},
		{"kubernetes settings write", (&KubernetesAdminController{}).requireSettingsWrite, core.PermissionInfrastructureSettingsWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := fiber.New()
			app.Get("/guarded", test.guard, func(ctx *fiber.Ctx) error { return ctx.SendStatus(fiber.StatusOK) })
			response, err := app.Test(httptest.NewRequest(http.MethodGet, "/guarded", nil), -1)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body map[string]string
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"error": string(test.permission) + " permission is required", "code": "permission_required", "permission": string(test.permission)}
			if response.StatusCode != fiber.StatusForbidden || len(body) != len(want) {
				t.Fatalf("status %d body %v, want 403 %v", response.StatusCode, body, want)
			}
			for key, value := range want {
				if body[key] != value {
					t.Fatalf("body %v, want %v", body, want)
				}
			}
		})
	}
}
