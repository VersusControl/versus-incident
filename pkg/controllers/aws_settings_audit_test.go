package controllers

import (
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
