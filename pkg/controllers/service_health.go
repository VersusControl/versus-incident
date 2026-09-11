package controllers

import (
	"errors"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"

	"github.com/gofiber/fiber/v2"
)

const auditActionServiceHealthSettingsChanged = "agent.service_health.settings.changed"

// ServiceHealthController serves persisted snapshots and runtime timing settings.
type ServiceHealthController struct{ manager *servicehealth.Manager }

// NewServiceHealthController constructs the always-enabled OSS controller.
func NewServiceHealthController(manager *servicehealth.Manager) *ServiceHealthController {
	return &ServiceHealthController{manager: manager}
}

// Register mounts the authorized Service Health read and settings routes.
func (controller *ServiceHealthController) Register(router fiber.Router) {
	group := router.Group("/agent", adminGatewayGuard)
	group.Get("/service-health", controller.getServiceHealth)
	group.Get("/service-health/settings", controller.getServiceHealthSettings)
	group.Patch("/service-health/settings", controller.patchServiceHealthSettings)
}

func (controller *ServiceHealthController) getServiceHealth(ctx *fiber.Ctx) error {
	if controller.manager == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "service health unavailable"})
	}
	snapshot, err := controller.manager.SnapshotOrEmpty(middleware.OrgFromContext(ctx))
	if err != nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "service health unavailable"})
	}
	return ctx.JSON(snapshot)
}

func (controller *ServiceHealthController) getServiceHealthSettings(ctx *fiber.Ctx) error {
	if controller.manager == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "service health settings unavailable"})
	}
	settings, err := controller.manager.LoadSettings(middleware.OrgFromContext(ctx))
	if err != nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "service health settings unavailable"})
	}
	return ctx.JSON(settings)
}

func (controller *ServiceHealthController) patchServiceHealthSettings(ctx *fiber.Ctx) error {
	deny := func(status int, message string) error {
		middleware.RecordAdminAudit(ctx, auditActionServiceHealthSettingsChanged, "service-health", middleware.AdminAuditDenied)
		return ctx.Status(status).JSON(fiber.Map{"error": message})
	}
	if allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionServiceHealthSettingsWrite)); !explicit || !allowed {
		return deny(fiber.StatusForbidden, "settings write permission required")
	}
	if controller.manager == nil {
		return deny(fiber.StatusServiceUnavailable, "service health settings unavailable")
	}
	var request struct {
		IntervalSeconds *int    `json:"interval_seconds"`
		WindowSeconds   *int    `json:"window_seconds"`
		Revision        *uint64 `json:"revision"`
	}
	if err := ctx.BodyParser(&request); err != nil || request.IntervalSeconds == nil || request.WindowSeconds == nil || request.Revision == nil {
		return deny(fiber.StatusBadRequest, "interval_seconds, window_seconds, and revision are required")
	}
	settings, err := controller.manager.UpdateSettings(middleware.OrgFromContext(ctx), servicehealth.Settings{IntervalSeconds: *request.IntervalSeconds, WindowSeconds: *request.WindowSeconds, Revision: *request.Revision})
	var validation *servicehealth.ValidationError
	switch {
	case errors.As(err, &validation):
		middleware.RecordAdminAudit(ctx, auditActionServiceHealthSettingsChanged, "service-health", middleware.AdminAuditDenied)
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": validation.Error(), "fields": validation.Fields})
	case errors.Is(err, servicehealth.ErrConflict):
		return deny(fiber.StatusConflict, "service health settings changed concurrently; retry")
	case err != nil:
		return deny(fiber.StatusServiceUnavailable, "service health settings unavailable")
	}
	middleware.RecordAdminAudit(ctx, auditActionServiceHealthSettingsChanged, "service-health", middleware.AdminAuditSuccess)
	return ctx.JSON(settings)
}
