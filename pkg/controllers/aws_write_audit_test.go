package controllers

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// auditRows captures the admin audit rows of one test.
type auditRows struct {
	mu   sync.Mutex
	rows []middleware.AdminAuditEvent
}

func captureAudit(t *testing.T) *auditRows {
	t.Helper()
	captured := &auditRows{}
	middleware.SetAdminAuditHook(func(_ *fiber.Ctx, event middleware.AdminAuditEvent) {
		captured.mu.Lock()
		defer captured.mu.Unlock()
		captured.rows = append(captured.rows, event)
	})
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	return captured
}

// take returns the rows written since the last take.
func (captured *auditRows) take() []middleware.AdminAuditEvent {
	captured.mu.Lock()
	defer captured.mu.Unlock()
	rows := captured.rows
	captured.rows = nil
	return rows
}

// awsAuditApp mounts the AWS routes as the mount does, settings first so
// its chain runs in front of every later AWS route.
func awsAuditApp(t *testing.T, provider storage.Provider, permissions map[string]bool) *fiber.App {
	t.Helper()
	actions, err := issues.NewActions(provider, tenancy.DefaultOrgScope(), func() time.Time { return issuesNow })
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := storage.AWSAccountLabelsOf(provider)
	corrections, _ := storage.AWSGroupCorrectionsOf(provider)
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
	api := app.Group("/api")
	NewAWSSettingsController(nil).Register(api)
	NewAWSIssuesController(actions, nil, nil, func() time.Time { return issuesNow }).Register(api)
	NewAWSAccountLabelsController(labels, tenancy.DefaultOrgScope()).Register(api)
	NewAWSGroupCorrectionsController(corrections, tenancy.DefaultOrgScope(), groupsInventory).Register(api)
	store := snapshot.New(provider, tenancy.DefaultOrgScope())
	NewAWSChangesController(snapshot.NewChangeFeed(store)).WithPreferences(snapshot.NewChangePreferenceStore(provider)).Register(api)
	return app
}

func wantAuditRows(t *testing.T, name string, got []middleware.AdminAuditEvent, want ...middleware.AdminAuditEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: audit rows %+v; want %+v", name, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s: audit rows %+v; want %+v", name, got, want)
		}
	}
}

// Each issue write records one row with its own action and the issue (or,
// for a bulk reject, the count and the IDs) as the target; a refused write,
// whether by the issues permission or by the view in front of the route,
// records one rbac.denied row on the issues write permission. None is
// recorded as a settings change.
func TestIssueWritesAreAuditedAsIssueActions(t *testing.T) {
	captured := captureAudit(t)
	provider := storage.NewMemory()
	ids := seedIssues(t, provider, issues.StateOpen, issues.StateOpen, issues.StateRejected, issues.StateOpen)
	writer := awsAuditApp(t, provider, issuesWriter)
	ok := middleware.AdminAuditSuccess
	for _, call := range []struct {
		name, path, body string
		want             middleware.AdminAuditEvent
	}{
		{"acknowledge", "/api/admin/aws/issues/" + ids[0] + "/acknowledge", "", middleware.AdminAuditEvent{Action: "aws.issue.acknowledged", Target: ids[0], Result: ok}},
		{"reject", "/api/admin/aws/issues/" + ids[1] + "/reject", `{"reason":"public on purpose"}`, middleware.AdminAuditEvent{Action: "aws.issue.rejected", Target: ids[1], Result: ok}},
		{"reopen", "/api/admin/aws/issues/" + ids[2] + "/reopen", "", middleware.AdminAuditEvent{Action: "aws.issue.reopened", Target: ids[2], Result: ok}},
		{"bulk reject", "/api/admin/aws/issues/reject", `{"ids":["` + ids[3] + `","` + ids[0] + `","` + ids[3] + `"],"reason":"public on purpose"}`,
			middleware.AdminAuditEvent{Action: "aws.issue.bulk_rejected", Target: "2 issues: " + ids[3] + "," + ids[0], Result: ok}},
		{"reject twice", "/api/admin/aws/issues/" + ids[1] + "/reject", `{"reason":"public on purpose"}`,
			middleware.AdminAuditEvent{Action: "aws.issue.rejected", Target: ids[1], Result: middleware.AdminAuditDenied}},
	} {
		res, body := awsTestRequest(t, writer, http.MethodPost, call.path, call.body)
		if res.StatusCode >= 500 {
			t.Fatalf("%s: %d %s", call.name, res.StatusCode, body)
		}
		wantAuditRows(t, call.name, captured.take(), call.want)
	}

	denied := middleware.AdminAuditEvent{Action: "rbac.denied", Target: string(core.PermissionInfrastructureIssuesWrite), Result: middleware.AdminAuditDenied}
	for name, permissions := range map[string]map[string]bool{
		"no issues write": issuesViewer,
		"no view":         {string(core.PermissionInfrastructureView): false, string(core.PermissionInfrastructureIssuesWrite): true},
	} {
		app := awsAuditApp(t, provider, permissions)
		for _, path := range []string{"/api/admin/aws/issues/" + ids[0] + "/reject", "/api/admin/aws/issues/reject", "/API/Admin/AWS/Issues/" + ids[0] + "/Acknowledge/"} {
			res, _ := awsTestRequest(t, app, http.MethodPost, path, `{"ids":["`+ids[0]+`"],"reason":"public on purpose"}`)
			if res.StatusCode != fiber.StatusForbidden {
				t.Fatalf("%s %s: %d; want 403", name, path, res.StatusCode)
			}
			wantAuditRows(t, name+" "+path, captured.take(), denied)
		}
	}
	// Reads are not audited.
	for _, path := range []string{"/api/admin/aws/issues", "/api/admin/aws/issues/report", "/api/admin/aws/issues/" + ids[0]} {
		awsTestRequest(t, writer, http.MethodGet, path, "")
	}
	wantAuditRows(t, "reads", captured.take())
}

// The label, correction and Changes-preference writes under /admin/aws
// each record their own action, never a settings change, and a refusal
// names the permission the write needs.
func TestAWSPageWritesHaveTheirOwnAuditActions(t *testing.T) {
	captured := captureAudit(t)
	provider := storage.NewMemory()
	ok := middleware.AdminAuditSuccess
	settingsWriter := map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureSettingsWrite): true}
	writer := awsAuditApp(t, provider, settingsWriter)
	correction := "account=" + groupsAccount + " kind=rename group=stack:shop"
	for _, call := range []struct {
		name, method, path, body string
		want                     middleware.AdminAuditEvent
	}{
		{"label saved", http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"Datadog"}`,
			middleware.AdminAuditEvent{Action: "aws.account.label.saved", Target: "555500006666", Result: ok}},
		{"label deleted", http.MethodDelete, "/api/admin/aws/accounts/labels/555500006666", "",
			middleware.AdminAuditEvent{Action: "aws.account.label.deleted", Target: "555500006666", Result: ok}},
		{"correction saved", http.MethodPost, "/api/admin/aws/groups/corrections",
			`{"account_id":"` + groupsAccount + `","kind":"rename","group_id":"stack:shop","name":"Shop"}`,
			middleware.AdminAuditEvent{Action: "aws.group.correction.saved", Target: correction, Result: ok}},
		{"correction deleted", http.MethodDelete, deletePath(map[string]string{"account_id": groupsAccount, "kind": "rename", "group_id": "stack:shop"}), "",
			middleware.AdminAuditEvent{Action: "aws.group.correction.deleted", Target: correction, Result: ok}},
		{"preferences saved", http.MethodPut, "/api/admin/aws/changes/preferences", `{"services":["ec2","s3"]}`,
			middleware.AdminAuditEvent{Action: "aws.changes.preferences.saved", Target: "services=ec2,s3", Result: ok}},
	} {
		res, body := awsTestRequest(t, writer, call.method, call.path, call.body)
		if res.StatusCode >= 400 {
			t.Fatalf("%s: %d %s", call.name, res.StatusCode, body)
		}
		wantAuditRows(t, call.name, captured.take(), call.want)
	}

	settingsDenied := middleware.AdminAuditEvent{Action: "rbac.denied", Target: string(core.PermissionInfrastructureSettingsWrite), Result: middleware.AdminAuditDenied}
	viewDenied := middleware.AdminAuditEvent{Action: "rbac.denied", Target: string(core.PermissionInfrastructureView), Result: middleware.AdminAuditDenied}
	viewOnly := awsAuditApp(t, provider, map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureSettingsWrite): false})
	noView := awsAuditApp(t, provider, map[string]bool{string(core.PermissionInfrastructureView): false, string(core.PermissionInfrastructureSettingsWrite): true})
	for _, call := range []struct {
		name   string
		app    *fiber.App
		method string
		path   string
		body   string
		want   middleware.AdminAuditEvent
	}{
		{"label, no settings write", viewOnly, http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"Datadog"}`, settingsDenied},
		{"label, no view", noView, http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"Datadog"}`, settingsDenied},
		{"correction, no settings write", viewOnly, http.MethodPost, "/api/admin/aws/groups/corrections", `{}`, settingsDenied},
		{"preferences, no view", noView, http.MethodPut, "/api/admin/aws/changes/preferences", `{"services":[]}`, viewDenied},
	} {
		res, _ := awsTestRequest(t, call.app, call.method, call.path, call.body)
		if res.StatusCode != fiber.StatusForbidden {
			t.Fatalf("%s: %d; want 403", call.name, res.StatusCode)
		}
		wantAuditRows(t, call.name, captured.take(), call.want)
	}
}
