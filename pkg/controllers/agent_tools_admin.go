package controllers

import (
	"errors"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

// ToolAvailability is the bounded server-owned card contract. Requirement and
// Health are summaries only; connection details and raw backend errors never
// cross this boundary.
type ToolAvailability struct {
	Group       aitools.Group       `json:"group"`
	Name        string              `json:"name"`
	DisplayName string              `json:"display_name"`
	Description string              `json:"description"`
	State       aitools.State       `json:"state"`
	Reason      string              `json:"reason"`
	Action      string              `json:"action"`
	ActionLabel string              `json:"action_label"`
	Enabled     bool                `json:"enabled"`
	Requirement aitools.Requirement `json:"requirement"`
	Health      string              `json:"health,omitempty"`
}

type AgentToolsAdminController struct {
	manager  *aitools.Manager
	snapshot func(tenancy.OrgScope) aitools.Snapshot
}

func NewAgentToolsAdminController(manager *aitools.Manager, snapshot func(tenancy.OrgScope) aitools.Snapshot) *AgentToolsAdminController {
	if snapshot == nil {
		snapshot = func(tenancy.OrgScope) aitools.Snapshot { return aitools.Snapshot{} }
	}
	return &AgentToolsAdminController{manager: manager, snapshot: snapshot}
}

func (controller *AgentToolsAdminController) Register(router fiber.Router) {
	group := router.Group("/admin/agent/tools", adminGatewayGuard)
	group.Get("", controller.list)
	group.Put("/:agent/:name", controller.put)
}

func (controller *AgentToolsAdminController) list(ctx *fiber.Ctx) error {
	agent := aitools.AgentKind(ctx.Query("agent"))
	if agent != aitools.AgentChat && agent != aitools.AgentAnalyze {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "agent must be chat or analyze"})
	}
	if controller.manager == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "tool settings unavailable"})
	}
	scope := tenancy.NewOrgScope(middleware.OrgFromContext(ctx))
	snapshot := controller.snapshot(scope)
	view, err := controller.manager.Load(scope)
	if err != nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "tool settings unavailable"})
	}
	result := make([]ToolAvailability, 0, len(aitools.Catalog()))
	for _, metadata := range aitools.Catalog() {
		enabled, err := view.Enabled(agent, metadata.Name)
		if err != nil {
			return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "tool settings unavailable"})
		}
		resolution := aitools.Resolve(metadata.Requirement, snapshot, enabled)
		result = append(result, ToolAvailability{
			Group: metadata.Group, Name: metadata.Name, DisplayName: metadata.DisplayName,
			Description: metadata.Description, State: resolution.State, Reason: resolution.Reason,
			Action: resolution.Action, ActionLabel: resolution.ActionLabel, Enabled: enabled, Requirement: metadata.Requirement, Health: resolution.Health,
		})
	}
	return ctx.JSON(result)
}

func (controller *AgentToolsAdminController) put(ctx *fiber.Ctx) error {
	agent := aitools.AgentKind(ctx.Params("agent"))
	name := ctx.Params("name")
	target := boundedToolAuditTarget(agent, name)
	deny := func(status int, message string) error {
		middleware.RecordAdminAudit(ctx, middleware.AuditActionAgentToolChanged, target, middleware.AdminAuditDenied)
		return ctx.Status(status).JSON(fiber.Map{"error": message})
	}
	if controller.manager == nil {
		return deny(fiber.StatusServiceUnavailable, "tool settings unavailable")
	}
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if err := ctx.BodyParser(&request); err != nil || request.Enabled == nil {
		return deny(fiber.StatusBadRequest, "enabled must be a boolean")
	}
	scope := tenancy.NewOrgScope(middleware.OrgFromContext(ctx))
	current, err := controller.manager.Enabled(scope, agent, name)
	if errors.Is(err, aitools.ErrInvalidAgent) || errors.Is(err, aitools.ErrUnknownTool) {
		return deny(fiber.StatusBadRequest, "invalid agent or tool")
	}
	if err != nil {
		return deny(fiber.StatusServiceUnavailable, "tool settings unavailable")
	}
	if *request.Enabled {
		metadata, ok := aitools.Lookup(name)
		if !ok {
			return deny(fiber.StatusBadRequest, "invalid agent or tool")
		}
		resolution := aitools.Resolve(metadata.Requirement, controller.snapshot(scope), true)
		if resolution.State != aitools.StateAvailable {
			return deny(fiber.StatusConflict, resolution.Reason)
		}
	}
	_, err = controller.manager.SetEnabled(scope, agent, name, *request.Enabled)
	if errors.Is(err, storage.ErrUnsupported) {
		return deny(fiber.StatusServiceUnavailable, "durable tool settings unavailable")
	}
	if errors.Is(err, aitools.ErrConflict) {
		return deny(fiber.StatusConflict, "tool settings changed concurrently; retry")
	}
	if err != nil {
		return deny(fiber.StatusServiceUnavailable, "tool settings unavailable")
	}
	middleware.RecordAdminAudit(ctx, middleware.AuditActionAgentToolChanged, target, middleware.AdminAuditSuccess)
	return ctx.JSON(fiber.Map{"agent": agent, "name": name, "enabled": *request.Enabled, "changed": current != *request.Enabled})
}

func boundedToolAuditTarget(agent aitools.AgentKind, name string) string {
	if agent != aitools.AgentChat && agent != aitools.AgentAnalyze {
		return "invalid"
	}
	if _, ok := aitools.Lookup(name); !ok {
		return "invalid"
	}
	return "agent=" + string(agent) + " tool=" + name
}
