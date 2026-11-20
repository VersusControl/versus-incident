package controllers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
)

// TestFinOpsWriteRefusalsAreAuditedOnce: a FinOps write refused before the
// FinOps route's own audit step (no infrastructure:view) still leaves one
// audit record, and a call the FinOps route audits itself is not recorded
// twice.
func TestFinOpsWriteRefusalsAreAuditedOnce(t *testing.T) {
	cases := []struct {
		name        string
		permissions map[string]bool
		status      int
		action      string
		result      string
	}{
		{"no infrastructure view", map[string]bool{string(core.PermissionInfrastructureView): false}, fiber.StatusForbidden, "rbac.denied", middleware.AdminAuditDenied},
		{"view without settings write", finopsViewer, fiber.StatusForbidden, "rbac.denied", middleware.AdminAuditDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, events := finopsApp(t, finopsBackend(t), tc.permissions, true)
			response, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", "")
			if response.StatusCode != tc.status {
				t.Fatalf("status = %d %s", response.StatusCode, body)
			}
			if len(*events) != 1 {
				t.Fatalf("audit records = %d: %+v", len(*events), *events)
			}
			if event := (*events)[0]; event.Action != tc.action || event.Result != tc.result {
				t.Fatalf("audit = %+v", event)
			}
		})
	}
	t.Run("refused read is not a write record", func(t *testing.T) {
		app, events := finopsApp(t, finopsBackend(t), map[string]bool{string(core.PermissionInfrastructureView): false}, true)
		if response, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/summary", ""); response.StatusCode != fiber.StatusForbidden {
			t.Fatalf("status = %d", response.StatusCode)
		}
		if len(*events) != 0 {
			t.Fatalf("plain read refusal audited: %+v", *events)
		}
	})
}

// TestFinOpsWriteRefusedAtTheGatewayIsAudited covers the 401 branch: a
// FinOps write without a gateway session is refused by the shared AWS
// admin chain before any FinOps handler runs, and leaves one audit record.
func TestFinOpsWriteRefusedAtTheGatewayIsAudited(t *testing.T) {
	loadGatewayConfig(t, "finops-audit-gateway-secret")
	events := make([]middleware.AdminAuditEvent, 0)
	middleware.SetAdminAuditHook(func(_ *fiber.Ctx, event middleware.AdminAuditEvent) { events = append(events, event) })
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	NewAWSSettingsController(awsTestService(t, "new-account-connect", true)).Register(app.Group("/api"))
	NewAWSFinOpsController(finopsBackend(t)).Register(app.Group("/api"))

	response, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", "")
	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d %s", response.StatusCode, body)
	}
	if len(events) != 1 || events[0].Action != "rbac.denied" || events[0].Result != middleware.AdminAuditDenied {
		t.Fatalf("audit = %+v", events)
	}
}

// TestAWSSettingsAuditIgnoresPathCase: the app routes paths case-insensitively,
// so a mixed-case write or external-id reveal reaches the same handler as the
// lowercase path and must leave the same single audit record.
func TestAWSSettingsAuditIgnoresPathCase(t *testing.T) {
	admin := map[string]bool{
		string(core.PermissionInfrastructureView):          true,
		string(core.PermissionInfrastructureSettingsWrite): true,
	}
	for _, prefix := range []string{"/api/admin/aws", "/api/Admin/AWS", "/API/ADMIN/AWS"} {
		t.Run(prefix, func(t *testing.T) {
			app, events := awsTestApp(t, awsTestService(t, "new-account-connect", true), admin)
			response, body := awsTestRequest(t, app, http.MethodPost, prefix+"/Connect/Drafts", `{"method":"cloudformation"}`)
			if response.StatusCode != fiber.StatusCreated {
				t.Fatalf("draft = %d %s", response.StatusCode, body)
			}
			if len(*events) != 1 || (*events)[0].Action != "aws.settings.changed" {
				t.Fatalf("draft write audit = %+v", *events)
			}
			var start struct {
				DraftID string `json:"draft_id"`
			}
			if err := json.Unmarshal(body, &start); err != nil || start.DraftID == "" {
				t.Fatalf("draft body %s: %v", body, err)
			}
			*events = (*events)[:0]
			if response, body := awsTestRequest(t, app, http.MethodGet, prefix+"/connect/drafts/"+start.DraftID+"/External-ID", ""); response.StatusCode != fiber.StatusOK {
				t.Fatalf("reveal = %d %s", response.StatusCode, body)
			}
			if len(*events) != 1 || (*events)[0].Action != "aws.external-id.revealed" || (*events)[0].Result != middleware.AdminAuditSuccess {
				t.Fatalf("reveal audit = %+v", *events)
			}
			*events = (*events)[:0]
			awsTestRequest(t, app, http.MethodPut, prefix+"/Settings/Privacy", `{}`)
			if len(*events) != 1 || (*events)[0].Action != "aws.settings.changed" {
				t.Fatalf("privacy write audit = %+v", *events)
			}
		})
	}
	for _, suffix := range []string{"//", "///"} {
		t.Run("reveal with trailing "+suffix, func(t *testing.T) {
			app, events := awsTestApp(t, awsTestService(t, "new-account-connect", true), admin)
			response, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`)
			var start struct {
				DraftID string `json:"draft_id"`
			}
			if response.StatusCode != fiber.StatusCreated || json.Unmarshal(body, &start) != nil || start.DraftID == "" {
				t.Fatalf("draft = %d %s", response.StatusCode, body)
			}
			*events = (*events)[:0]
			if response, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/connect/drafts/"+start.DraftID+"/external-id"+suffix, ""); response.StatusCode != fiber.StatusOK {
				t.Fatalf("reveal = %d %s", response.StatusCode, body)
			}
			if len(*events) != 1 || (*events)[0].Action != "aws.external-id.revealed" || (*events)[0].Result != middleware.AdminAuditSuccess {
				t.Fatalf("reveal audit = %+v", *events)
			}
		})
	}
	t.Run("refused mixed-case reveal", func(t *testing.T) {
		app, events := awsTestApp(t, awsTestService(t, "new-account-connect", true), map[string]bool{string(core.PermissionInfrastructureView): true})
		if response, _ := awsTestRequest(t, app, http.MethodGet, "/api/Admin/AWS/connect/drafts/any/External-Id", ""); response.StatusCode != fiber.StatusForbidden {
			t.Fatalf("status = %d", response.StatusCode)
		}
		if len(*events) != 1 || (*events)[0].Action != "rbac.denied" {
			t.Fatalf("refused reveal audit = %+v", *events)
		}
	})
}

// TestFinOpsMixedCaseWriteIsAuditedOnce: a mixed-case FinOps write takes the
// FinOps branch of the shared AWS audit wrapper, so it gets the FinOps route's
// own record and no extra settings-changed record.
func TestFinOpsMixedCaseWriteIsAuditedOnce(t *testing.T) {
	for _, path := range []string{"/api/admin/aws/finops/refresh", "/api/admin/aws/FinOps/Refresh", "/API/ADMIN/AWS/FINOPS/REFRESH/"} {
		t.Run(path, func(t *testing.T) {
			app, events := finopsApp(t, finopsBackend(t), finopsAdmin, true)
			awsTestRequest(t, app, http.MethodPost, path, `{}`)
			if len(*events) != 1 || auditCount(*events, "aws.settings.changed", "") != 0 {
				t.Fatalf("audit = %+v", *events)
			}
			viewer, viewerEvents := finopsApp(t, finopsBackend(t), finopsViewer, true)
			if response, _ := awsTestRequest(t, viewer, http.MethodPost, path, `{}`); response.StatusCode != fiber.StatusForbidden {
				t.Fatalf("viewer status = %d", response.StatusCode)
			}
			if len(*viewerEvents) != 1 || (*viewerEvents)[0].Action != "rbac.denied" {
				t.Fatalf("viewer audit = %+v", *viewerEvents)
			}
			refused, refusedEvents := finopsApp(t, finopsBackend(t), map[string]bool{string(core.PermissionInfrastructureView): false}, true)
			if response, _ := awsTestRequest(t, refused, http.MethodPost, path, `{}`); response.StatusCode != fiber.StatusForbidden {
				t.Fatalf("no-view status = %d", response.StatusCode)
			}
			if len(*refusedEvents) != 1 || (*refusedEvents)[0].Action != "rbac.denied" {
				t.Fatalf("no-view audit = %+v", *refusedEvents)
			}
		})
	}
}
