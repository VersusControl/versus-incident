package controllers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

type toolsetLegacyCASFailureProvider struct {
	storage.Provider
}

func (provider *toolsetLegacyCASFailureProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	if strings.HasSuffix(name, "/settings.json") {
		return false, errors.New("injected legacy cleanup failure")
	}
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

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

func TestAgentToolsetsListReturnsExactlySevenChildFreeCardsInServerOrder(t *testing.T) {
	app, _ := toolAdminApp(t, aitools.Snapshot{})
	response, err := app.Test(httptest.NewRequest("GET", "/api/admin/agent/toolsets?agent=chat", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var rows []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	want := []string{"kubernetes", "source-control", "logs", "metrics", "traces", "find_runbook", "describe_dependencies"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for index, id := range want {
		if rows[index]["id"] != id {
			t.Errorf("row %d id = %v, want %s", index, rows[index]["id"], id)
		}
		if _, exposed := rows[index]["tool_names"]; exposed {
			t.Errorf("row %s exposed hidden child names", id)
		}
	}
	if rows[0]["child_count"] != float64(9) || rows[0]["icon_key"] != "kubernetes" || rows[0]["ui_path"] != "/agent/kubernetes" {
		t.Fatalf("Kubernetes card = %#v", rows[0])
	}
}

func TestAgentToolsetsFailClosedPermissionNewerPolicyAndCatalogCount(t *testing.T) {
	provider := storage.NewMemory()
	manager := aitools.NewManager(provider)
	if _, err := manager.SetEnabled(tenancy.DefaultOrgScope(), aitools.AgentChat, "get_incident", false); err != nil {
		t.Fatal(err)
	}
	if err := provider.WriteBlob("agent-tools/default/toolsets-v2.json", []byte(`{"version":3,"disabled_toolsets":{},"future":true}`)); err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error { middleware.MarkAuthorized(ctx); return ctx.Next() })
	NewAgentToolsAdminController(manager, func(tenancy.OrgScope) aitools.Snapshot { return aitools.Snapshot{} }).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest("GET", "/api/admin/agent/toolsets?agent=chat", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("status=%v err=%v", response.StatusCode, err)
	}
	var rows []ToolsetAvailability
	if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	versusCount := 0
	for _, metadata := range aitools.Catalog() {
		if metadata.Group == aitools.GroupVersus {
			versusCount++
		}
	}
	for _, row := range rows {
		switch row.ID {
		case "kubernetes":
			if row.State != aitools.StateNeedsPermission {
				t.Fatalf("Kubernetes state=%s reason=%q", row.State, row.Reason)
			}
		case "versus-core":
			if row.ChildCount != versusCount {
				t.Fatalf("versus child count=%d want=%d", row.ChildCount, versusCount)
			}
		}
	}
}

func TestAgentToolsetPutIsAtomicAndAuditedOnce(t *testing.T) {
	snapshot := aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{"logs": {Configured: true, Healthy: true}}}
	app, events := toolAdminApp(t, snapshot)
	request := httptest.NewRequest("PUT", "/api/admin/agent/toolsets/chat/logs", bytes.NewBufferString(`{"enabled":false}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("disable status=%v err=%v", response.StatusCode, err)
	}
	if len(*events) != 1 || (*events)[0].Action != middleware.AuditActionAgentToolsetChanged || (*events)[0].Target != "agent=chat toolset=logs" || (*events)[0].Result != middleware.AdminAuditSuccess {
		t.Fatalf("toolset audit = %+v", *events)
	}
}

func TestAgentToolsetPutResumesPendingTransitionAndAuditsCompletion(t *testing.T) {
	provider := storage.NewMemory()
	legacy := `{"disabled":{"chat":{"get_cluster_overview":true},"analyze":{}},"toolset_transitions":{"chat/kubernetes":{"agent":"chat","toolset":"kubernetes","enabled":true}}}`
	grouped := `{"version":2,"disabled_toolsets":{"chat":{"kubernetes":true},"analyze":{}}}`
	if err := provider.WriteBlob("agent-tools/default/settings.json", []byte(legacy)); err != nil {
		t.Fatal(err)
	}
	if err := provider.WriteBlob("agent-tools/default/toolsets-v2.json", []byte(grouped)); err != nil {
		t.Fatal(err)
	}
	events := make([]middleware.AdminAuditEvent, 0, 1)
	middleware.SetAdminAuditHook(func(_ *fiber.Ctx, event middleware.AdminAuditEvent) { events = append(events, event) })
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error { middleware.MarkAuthorized(ctx); return ctx.Next() })
	app.Use(middleware.OrgInjector())
	snapshot := aitools.Snapshot{Integrations: map[string]aitools.DependencyStatus{"kubernetes": {Configured: true, Healthy: true}}}
	NewAgentToolsAdminController(aitools.NewManager(provider), func(tenancy.OrgScope) aitools.Snapshot { return snapshot }).Register(app.Group("/api"))
	request := httptest.NewRequest("PUT", "/api/admin/agent/toolsets/chat/kubernetes", bytes.NewBufferString(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("resume status=%v err=%v", response.StatusCode, err)
	}
	if len(events) != 1 || events[0].Action != middleware.AuditActionAgentToolsetChanged || events[0].Target != "agent=chat toolset=kubernetes" || events[0].Result != middleware.AdminAuditSuccess {
		t.Fatalf("resume audit=%+v", events)
	}
}

func TestAgentToolsetDisableFailsClosedAndAuditsWhenLegacyDenyWriteFails(t *testing.T) {
	inner := storage.NewMemory()
	provider := &toolsetLegacyCASFailureProvider{Provider: inner}
	if err := inner.WriteBlob("agent-tools/default/toolsets-v2.json", []byte(`{"version":2,"disabled_toolsets":{"chat":{},"analyze":{}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := inner.WriteBlob("agent-tools/default/settings.json", []byte(`{"disabled":{"chat":{"get_cluster_overview":true},"analyze":{}}}`)); err != nil {
		t.Fatal(err)
	}
	events := make([]middleware.AdminAuditEvent, 0, 2)
	middleware.SetAdminAuditHook(func(_ *fiber.Ctx, event middleware.AdminAuditEvent) { events = append(events, event) })
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error { middleware.MarkAuthorized(ctx); return ctx.Next() })
	NewAgentToolsAdminController(aitools.NewManager(provider), func(tenancy.OrgScope) aitools.Snapshot { return aitools.Snapshot{} }).Register(app.Group("/api"))
	for attempt := 1; attempt <= 2; attempt++ {
		request := httptest.NewRequest("PUT", "/api/admin/agent/toolsets/chat/kubernetes", bytes.NewBufferString(`{"enabled":false}`))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request, -1)
		if err != nil || response.StatusCode != fiber.StatusServiceUnavailable {
			t.Fatalf("attempt %d status=%v err=%v", attempt, response.StatusCode, err)
		}
	}
	grouped, _ := inner.ReadBlob("agent-tools/default/toolsets-v2.json")
	if strings.Contains(string(grouped), `"kubernetes":true`) {
		t.Fatalf("grouped deny committed without legacy protection: %s", grouped)
	}
	if len(events) != 2 {
		t.Fatalf("audit=%+v", events)
	}
	for index, event := range events {
		if event.Action != middleware.AuditActionAgentToolsetChanged || event.Result != middleware.AdminAuditDenied {
			t.Fatalf("audit %d=%+v", index+1, event)
		}
	}
}

func TestAgentToolsetNewerPolicyReturnsAuditedConflict(t *testing.T) {
	provider := storage.NewMemory()
	if err := provider.WriteBlob("agent-tools/default/toolsets-v2.json", []byte(`{"version":3,"disabled_toolsets":{}}`)); err != nil {
		t.Fatal(err)
	}
	events := make([]middleware.AdminAuditEvent, 0, 1)
	middleware.SetAdminAuditHook(func(_ *fiber.Ctx, event middleware.AdminAuditEvent) { events = append(events, event) })
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error { middleware.MarkAuthorized(ctx); return ctx.Next() })
	NewAgentToolsAdminController(aitools.NewManager(provider), func(tenancy.OrgScope) aitools.Snapshot {
		return aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{"logs": {Configured: true, Healthy: true}}}
	}).Register(app.Group("/api"))
	request := httptest.NewRequest("PUT", "/api/admin/agent/toolsets/chat/logs", bytes.NewBufferString(`{"enabled":false}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, -1)
	if err != nil || response.StatusCode != fiber.StatusConflict {
		t.Fatalf("status=%v err=%v", response.StatusCode, err)
	}
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || body["error"] != "a newer toolset policy version exists; retry on an upgraded instance" {
		t.Fatalf("body=%#v err=%v", body, err)
	}
	if len(events) != 1 || events[0].Action != middleware.AuditActionAgentToolsetChanged || events[0].Target != "agent=chat toolset=logs" || events[0].Result != middleware.AdminAuditDenied {
		t.Fatalf("audit=%+v", events)
	}
}

func TestAgentToolsetCommonPutGetIsAgentIndependent(t *testing.T) {
	snapshot := aitools.Snapshot{Capabilities: map[string]aitools.DependencyStatus{"dependency_graph": {Configured: true, Healthy: true}}}
	app, _ := toolAdminApp(t, snapshot)
	put := func(agent string, enabled bool) {
		request := httptest.NewRequest("PUT", "/api/admin/agent/toolsets/"+agent+"/describe_dependencies", bytes.NewBufferString(fmt.Sprintf(`{"enabled":%t}`, enabled)))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request, -1)
		if err != nil || response.StatusCode != fiber.StatusOK {
			t.Fatalf("PUT %s enabled=%v status=%v err=%v", agent, enabled, response.StatusCode, err)
		}
	}
	enabled := func(agent string) bool {
		response, err := app.Test(httptest.NewRequest("GET", "/api/admin/agent/toolsets?agent="+agent, nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		var rows []ToolsetAvailability
		if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == "describe_dependencies" {
				return row.Enabled
			}
		}
		t.Fatal("describe_dependencies missing")
		return false
	}
	put("chat", false)
	if enabled("chat") || !enabled("analyze") {
		t.Fatal("Common policy did not remain agent independent")
	}
	put("chat", true)
	if !enabled("chat") {
		t.Fatal("Common policy did not re-enable")
	}
}

func TestAgentToolsetUnsatisfiedEnableAndGroupedChildPutReturnConflict(t *testing.T) {
	app, events := toolAdminApp(t, aitools.Snapshot{})
	request := httptest.NewRequest("PUT", "/api/admin/agent/toolsets/chat/kubernetes", bytes.NewBufferString(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, -1)
	if err != nil || response.StatusCode != fiber.StatusConflict {
		t.Fatalf("enable status=%v err=%v", response.StatusCode, err)
	}
	request = httptest.NewRequest("PUT", "/api/admin/agent/tools/chat/get_cluster_overview", bytes.NewBufferString(`{"enabled":false}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = app.Test(request, -1)
	if err != nil || response.StatusCode != fiber.StatusConflict {
		t.Fatalf("child status=%v err=%v", response.StatusCode, err)
	}
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || !strings.Contains(body["error"], "kubernetes") {
		t.Fatalf("child conflict = %#v, %v", body, err)
	}
	if len(*events) != 2 || (*events)[0].Action != middleware.AuditActionAgentToolsetChanged || (*events)[1].Action != middleware.AuditActionAgentToolChanged {
		t.Fatalf("denial audits = %+v", *events)
	}
}
