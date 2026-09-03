package controllers

import (
	"errors"
	"fmt"

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
	DocsURL     string              `json:"docs_url,omitempty"`
	UIPath      string              `json:"ui_path,omitempty"`
	State       aitools.State       `json:"state"`
	Reason      string              `json:"reason"`
	Action      string              `json:"action"`
	ActionLabel string              `json:"action_label"`
	Enabled     bool                `json:"enabled"`
	Requirement aitools.Requirement `json:"requirement"`
	Health      string              `json:"health,omitempty"`
}

// ToolsetAvailability is the child-hiding product card contract.
type ToolsetAvailability struct {
	ID          string                 `json:"id"`
	Section     aitools.CatalogSection `json:"section"`
	DisplayName string                 `json:"display_name"`
	Description string                 `json:"description"`
	IconKey     string                 `json:"icon_key"`
	DocsURL     string                 `json:"docs_url,omitempty"`
	UIPath      string                 `json:"ui_path,omitempty"`
	Visibility  string                 `json:"visibility"`
	State       aitools.State          `json:"state"`
	Reason      string                 `json:"reason"`
	Action      string                 `json:"action"`
	ActionLabel string                 `json:"action_label"`
	Enabled     bool                   `json:"enabled"`
	ChildCount  int                    `json:"child_count"`
	Requirement aitools.Requirement    `json:"requirement"`
	Health      string                 `json:"health,omitempty"`
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
	toolsets := router.Group("/admin/agent/toolsets", adminGatewayGuard)
	toolsets.Get("", controller.listToolsets)
	toolsets.Put("/:agent/:toolset", controller.putToolset)
}

func (controller *AgentToolsAdminController) listToolsets(ctx *fiber.Ctx) error {
	agent := aitools.AgentKind(ctx.Query("agent"))
	if agent != aitools.AgentChat && agent != aitools.AgentAnalyze {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "agent must be chat or analyze"})
	}
	if controller.manager == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "toolset settings unavailable"})
	}
	scope := tenancy.NewOrgScope(middleware.OrgFromContext(ctx))
	view, err := controller.manager.LoadToolsets(scope)
	if err != nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "toolset settings unavailable"})
	}
	snapshot := controller.snapshot(scope)
	metadata := aitools.Toolsets()
	result := make([]ToolsetAvailability, 0, len(metadata))
	for _, toolset := range metadata {
		enabled, err := view.Enabled(agent, toolset.ID)
		if err != nil {
			return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "toolset settings unavailable"})
		}
		resolution := aitools.Resolve(toolset.Requirement, snapshot, enabled)
		if view.NewerPolicy() {
			resolution = aitools.Resolution{State: aitools.StateDisabledByOperator, Reason: "Toolset policy is from a newer version; tools are disabled for compatibility."}
		}
		if toolset.Permission != "" {
			if allowed, explicit := middleware.RequestPermission(ctx, string(toolset.Permission)); !explicit || !allowed {
				resolution = aitools.Resolution{State: aitools.StateNeedsPermission, Reason: "infrastructure:view permission is required"}
			}
		}
		result = append(result, ToolsetAvailability{
			ID: toolset.ID, Section: toolset.Section, DisplayName: toolset.DisplayName, Description: toolset.Description,
			IconKey: toolset.IconKey, DocsURL: toolset.DocsURL, UIPath: toolset.UIPath, Visibility: toolset.Visibility,
			State: resolution.State, Reason: resolution.Reason, Action: resolution.Action, ActionLabel: resolution.ActionLabel,
			Enabled: enabled, ChildCount: len(toolset.ToolNames), Requirement: toolset.Requirement, Health: resolution.Health,
		})
	}
	legacy, err := controller.manager.Load(scope)
	if err != nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "tool settings unavailable"})
	}
	legacyDisabled := false
	for _, child := range aitools.Catalog() {
		if child.Group != aitools.GroupVersus {
			continue
		}
		enabled, enabledErr := legacy.Enabled(agent, child.Name)
		if enabledErr != nil {
			return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "tool settings unavailable"})
		}
		if !enabled {
			legacyDisabled = true
			break
		}
	}
	if legacyDisabled {
		result = append(result, ToolsetAvailability{ID: "versus-core", Section: aitools.SectionCommon, DisplayName: "Versus core", Description: "Restore disabled Versus self-knowledge tools.", IconKey: "common", DocsURL: "https://docs.versusincident.com/#/agent/tools/tools?id=versus-tools", Visibility: aitools.VisibilityNonDefault, State: aitools.StateDisabledByOperator, Reason: "Legacy Versus tools are disabled.", Enabled: false, ChildCount: versusToolCount(), Requirement: aitools.Requirement{Kind: aitools.RequirementNone}})
	}
	return ctx.JSON(result)
}

func versusToolCount() int {
	count := 0
	for _, metadata := range aitools.Catalog() {
		if metadata.Group == aitools.GroupVersus {
			count++
		}
	}
	return count
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
			Description: metadata.Description, DocsURL: metadata.DocsURL, UIPath: metadata.UIPath,
			State: resolution.State, Reason: resolution.Reason,
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
	if owner := groupedOwner(name); owner != "" {
		return deny(fiber.StatusConflict, "tool is managed by toolset "+owner)
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
	changed, err := controller.manager.SetEnabled(scope, agent, name, *request.Enabled)
	if errors.Is(err, aitools.ErrInvalidAgent) || errors.Is(err, aitools.ErrUnknownTool) {
		return deny(fiber.StatusBadRequest, "invalid agent or tool")
	}
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
	return ctx.JSON(fiber.Map{"agent": agent, "name": name, "enabled": *request.Enabled, "changed": changed})
}

func (controller *AgentToolsAdminController) putToolset(ctx *fiber.Ctx) error {
	agent := aitools.AgentKind(ctx.Params("agent"))
	id := ctx.Params("toolset")
	target := boundedToolsetAuditTarget(agent, id)
	deny := func(status int, message string) error {
		middleware.RecordAdminAudit(ctx, middleware.AuditActionAgentToolsetChanged, target, middleware.AdminAuditDenied)
		return ctx.Status(status).JSON(fiber.Map{"error": message})
	}
	if controller.manager == nil {
		return deny(fiber.StatusServiceUnavailable, "toolset settings unavailable")
	}
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if err := ctx.BodyParser(&request); err != nil || request.Enabled == nil {
		return deny(fiber.StatusBadRequest, "enabled must be a boolean")
	}
	toolset, ok := findToolset(id)
	if id == "versus-core" {
		toolset, ok = recoveryToolset(), true
	}
	if !ok || (agent != aitools.AgentChat && agent != aitools.AgentAnalyze) {
		return deny(fiber.StatusBadRequest, "invalid agent or toolset")
	}
	scope := tenancy.NewOrgScope(middleware.OrgFromContext(ctx))
	if *request.Enabled {
		resolution := aitools.Resolve(toolset.Requirement, controller.snapshot(scope), true)
		if resolution.State != aitools.StateAvailable {
			return deny(fiber.StatusConflict, resolution.Reason)
		}
	}
	var changed bool
	var err error
	if id == "versus-core" {
		if !*request.Enabled {
			return deny(fiber.StatusConflict, "versus-core can only be restored")
		}
		for _, child := range aitools.Catalog() {
			if child.Group != aitools.GroupVersus {
				continue
			}
			childChanged, childErr := controller.manager.SetEnabled(scope, agent, child.Name, true)
			if childErr != nil {
				err = childErr
				break
			}
			changed = changed || childChanged
		}
	} else {
		changed, err = controller.manager.SetToolsetEnabled(scope, agent, id, *request.Enabled)
	}
	if errors.Is(err, aitools.ErrNewerPolicyVersion) {
		return deny(fiber.StatusConflict, "a newer toolset policy version exists; retry on an upgraded instance")
	}
	if errors.Is(err, storage.ErrUnsupported) {
		return deny(fiber.StatusServiceUnavailable, "durable toolset settings unavailable")
	}
	if errors.Is(err, aitools.ErrConflict) {
		action := "disable"
		if *request.Enabled {
			action = "enable"
		}
		return deny(fiber.StatusConflict, fmt.Sprintf("toolset %s/%s %s conflicted; retry", agent, id, action))
	}
	if err != nil {
		return deny(fiber.StatusServiceUnavailable, "toolset settings unavailable")
	}
	middleware.RecordAdminAudit(ctx, middleware.AuditActionAgentToolsetChanged, target, middleware.AdminAuditSuccess)
	return ctx.JSON(fiber.Map{"agent": agent, "id": id, "enabled": *request.Enabled, "changed": changed})
}

func findToolset(id string) (aitools.ToolsetMetadata, bool) {
	for _, toolset := range aitools.Toolsets() {
		if toolset.ID == id {
			return toolset, true
		}
	}
	return aitools.ToolsetMetadata{}, false
}

func recoveryToolset() aitools.ToolsetMetadata {
	return aitools.ToolsetMetadata{ID: "versus-core", Section: aitools.SectionCommon, DisplayName: "Versus core", IconKey: "common", Requirement: aitools.Requirement{Kind: aitools.RequirementNone}, Visibility: aitools.VisibilityNonDefault}
}

func groupedOwner(name string) string {
	for _, toolset := range aitools.Toolsets() {
		if toolset.Section == aitools.SectionCommon {
			continue
		}
		for _, child := range toolset.ToolNames {
			if child == name {
				return toolset.ID
			}
		}
	}
	return ""
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

func boundedToolsetAuditTarget(agent aitools.AgentKind, id string) string {
	if agent != aitools.AgentChat && agent != aitools.AgentAnalyze {
		return "invalid"
	}
	if _, ok := findToolset(id); !ok {
		if id != "versus-core" {
			return "invalid"
		}
	}
	return "agent=" + string(agent) + " toolset=" + id
}
