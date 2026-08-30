package controllers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

func toolAdminApp(t *testing.T, snapshot aitools.Snapshot) (*fiber.App, *[]middleware.AdminAuditEvent) {
	t.Helper()
	events := make([]middleware.AdminAuditEvent, 0)
	middleware.SetAdminAuditHook(func(_ *fiber.Ctx, event middleware.AdminAuditEvent) { events = append(events, event) })
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error { middleware.MarkAuthorized(ctx); return ctx.Next() })
	app.Use(middleware.OrgInjector())
	NewAgentToolsAdminController(aitools.NewManager(storage.NewMemory()), func(tenancy.OrgScope) aitools.Snapshot { return snapshot }).Register(app.Group("/api"))
	return app, &events
}

func TestAgentToolsListIncludesAllGroupsAndUnavailableKubernetes(t *testing.T) {
	app, _ := toolAdminApp(t, aitools.Snapshot{})
	response, err := app.Test(httptest.NewRequest("GET", "/api/admin/agent/tools?agent=chat", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var rows []ToolAvailability
	if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(aitools.Catalog()) {
		t.Fatalf("rows = %d, want %d", len(rows), len(aitools.Catalog()))
	}
	groups := map[aitools.Group]int{}
	for _, row := range rows {
		groups[row.Group]++
		if row.DocsURL == "" {
			t.Errorf("tool %s has no documentation destination", row.Name)
		}
		if row.Group == aitools.GroupK8s && row.State != aitools.StateNeedsIntegration {
			t.Errorf("k8s tool %s state = %s", row.Name, row.State)
		}
	}
	for _, group := range []aitools.Group{aitools.GroupVersus, aitools.GroupCommon, aitools.GroupK8s} {
		if groups[group] == 0 {
			t.Errorf("group %q missing", group)
		}
	}
}

func TestAgentToolsListEmitsMappedDestinationsSeparatelyFromAction(t *testing.T) {
	app, _ := toolAdminApp(t, aitools.Snapshot{})
	response, err := app.Test(httptest.NewRequest("GET", "/api/admin/agent/tools?agent=analyze", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var rows []ToolAvailability
	if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name != "find_runbook" {
			continue
		}
		if row.DocsURL != "https://docs.versusincident.com/#/agent/tools/find-runbook" || row.UIPath != "/agent/runbooks" {
			t.Fatalf("find_runbook destinations = docs %q ui %q", row.DocsURL, row.UIPath)
		}
		if row.Action != "/admin#agent-ai-settings" || row.ActionLabel != "AI settings" {
			t.Fatalf("find_runbook availability action = %q %q", row.Action, row.ActionLabel)
		}
		return
	}
	t.Fatal("find_runbook missing from response")
}

func TestAgentToolsPutSuccessAndUnsatisfiedDenialAuditExactlyOnce(t *testing.T) {
	app, events := toolAdminApp(t, aitools.Snapshot{})
	request := httptest.NewRequest("PUT", "/api/admin/agent/tools/chat/get_incident", bytes.NewBufferString(`{"enabled":false}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("disable status=%v err=%v", response.StatusCode, err)
	}
	if len(*events) != 1 || (*events)[0].Result != middleware.AdminAuditSuccess {
		t.Fatalf("success audit = %+v", *events)
	}
	*events = nil
	request = httptest.NewRequest("PUT", "/api/admin/agent/tools/chat/query_metrics", bytes.NewBufferString(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = app.Test(request, -1)
	if err != nil || response.StatusCode != fiber.StatusConflict {
		t.Fatalf("enable status=%v err=%v", response.StatusCode, err)
	}
	if len(*events) != 1 || (*events)[0].Result != middleware.AdminAuditDenied {
		t.Fatalf("denial audit = %+v", *events)
	}
}

func TestAgentToolsPutRejectsInvalidInputsWithBoundedAudit(t *testing.T) {
	app, events := toolAdminApp(t, aitools.Snapshot{})
	for _, test := range []struct {
		path, body, target string
	}{
		{"/api/admin/agent/tools/detect/get_incident", `{"enabled":false}`, "invalid"},
		{"/api/admin/agent/tools/chat/not-a-tool", `{"enabled":false}`, "invalid"},
		{"/api/admin/agent/tools/chat/" + string(bytes.Repeat([]byte("x"), 200)), `{"enabled":false}`, "invalid"},
		{"/api/admin/agent/tools/chat/bad%0Atool", `{"enabled":false}`, "invalid"},
		{"/api/admin/agent/tools/chat/get_incident", `{"enabled":"%0Acontrol"}`, "agent=chat tool=get_incident"},
	} {
		before := len(*events)
		request := httptest.NewRequest("PUT", test.path, bytes.NewBufferString(test.body))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request, -1)
		if err != nil || response.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("%s status=%v err=%v", test.path, response.StatusCode, err)
		}
		if len(*events) != before+1 {
			t.Fatalf("%s emitted %d audit outcomes, want exactly one", test.path, len(*events)-before)
		}
		last := (*events)[len(*events)-1]
		if last.Target != test.target || last.Result != middleware.AdminAuditDenied {
			t.Fatalf("%s audit = %+v, want denied target %q", test.path, last, test.target)
		}
	}
}
