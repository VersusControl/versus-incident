package controllers

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/gofiber/fiber/v2"
)

func auditKubernetesSettings(ctx *fiber.Ctx) error {
	path := strings.TrimSuffix(ctx.Path(), "/")
	put := ctx.Method() == fiber.MethodPut && path == "/api/admin/kubernetes/agent-settings"
	testLink := ctx.Method() == fiber.MethodPost && path == "/api/admin/kubernetes/agent-settings/test-link"
	if !put && !testLink {
		return ctx.Next()
	}
	err := ctx.Next()
	status := ctx.Response().StatusCode()
	if fiberErr, ok := err.(*fiber.Error); ok {
		status = fiberErr.Code
	} else if err != nil {
		status = fiber.StatusInternalServerError
	}
	action, target, result := "kubernetes.agent-settings.changed", "infrastructure:settings:write", middleware.AdminAuditDenied
	if status == fiber.StatusUnauthorized || status == fiber.StatusForbidden {
		action = "rbac.denied"
	} else if !put {
		return err
	} else if status < fiber.StatusBadRequest {
		result = middleware.AdminAuditSuccess
	}
	middleware.RecordAdminAudit(ctx, action, target, result)
	return err
}

func (controller *KubernetesAdminController) requireSettingsWrite(ctx *fiber.Ctx) error {
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite))
	if !explicit || !allowed {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "infrastructure:settings:write permission is required"})
	}
	return ctx.Next()
}

func (controller *KubernetesAdminController) breakLogs(ctx *fiber.Ctx) error {
	for _, key := range []string{"cluster", "namespace", "root_uid", "since", "until", "mode", "level", "q", "cursor", "limit", "container"} {
		if len(ctx.Context().QueryArgs().PeekMulti(key)) > 1 {
			return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
		}
	}
	from, err := parseKubernetesTime(ctx.Query("since"))
	if err != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	to, err := parseKubernetesTime(ctx.Query("until"))
	if err != nil {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	limit := 0
	if value := ctx.Query("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 500 {
			return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
		}
	}
	pods := []string{}
	for _, key := range []string{"pods[]", "pods"} {
		for _, value := range ctx.Context().QueryArgs().PeekMulti(key) {
			pods = append(pods, strings.Clone(string(value)))
		}
	}
	query := breaklogs.Query{Namespace: strings.Clone(ctx.Query("namespace")), RootUID: strings.Clone(ctx.Query("root_uid")), From: from, To: to, Mode: strings.Clone(ctx.Query("mode")), Level: strings.Clone(ctx.Query("level")), Search: strings.Clone(ctx.Query("q")), Container: strings.Clone(ctx.Query("container")), Cursor: strings.Clone(ctx.Query("cursor")), Limit: limit, Pods: pods}
	value, err := requestKubernetesService(ctx).BreakLogs(ctx.UserContext(), query)
	if err == nil {
		value.Patterns = breakLogPatterns(value.PatternInput)
		value.PatternInput = nil
	}
	return writeKubernetes(ctx, value, err)
}

func breakLogPatterns(lines []kubernetes.BreakLogLine) []kubernetes.BreakLogPattern {
	miner := agent.NewMiner(0.4, 4, 100)
	patterns := map[string]*kubernetes.BreakLogPattern{}
	for _, line := range lines {
		id, template, _ := miner.Cluster(line.Text)
		key := id + "\x00" + line.Level
		if patterns[key] == nil {
			patterns[key] = &kubernetes.BreakLogPattern{Level: line.Level}
		}
		patterns[key].Template = template
		patterns[key].Count++
	}
	result := []kubernetes.BreakLogPattern{}
	for _, pattern := range patterns {
		result = append(result, *pattern)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Count != result[right].Count {
			return result[left].Count > result[right].Count
		}
		return result[left].Template < result[right].Template
	})
	if len(result) > 20 {
		result = result[:20]
	}
	return result
}

func (controller *KubernetesAdminController) agentSettings(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).AgentSettings(ctx.UserContext())
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite))
	value.CanWrite = allowed && explicit
	return writeKubernetes(ctx, value, err)
}

func decodeKubernetesSettings(ctx *fiber.Ctx, value any) error {
	if len(ctx.Body()) > 32<<10 {
		return kubernetes.ErrInvalidArguments
	}
	decoder := json.NewDecoder(bytes.NewReader(ctx.Body()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return kubernetes.ErrInvalidArguments
	}
	if decoder.Decode(new(any)) != io.EOF {
		return kubernetes.ErrInvalidArguments
	}
	return nil
}

func (controller *KubernetesAdminController) saveAgentSettings(ctx *fiber.Ctx) error {
	var settings breaklogs.Settings
	if err := decodeKubernetesSettings(ctx, &settings); err != nil {
		return writeKubernetes(ctx, nil, err)
	}
	if err := requestKubernetesService(ctx).SaveAgentSettings(ctx.UserContext(), settings); err != nil {
		return writeKubernetes(ctx, nil, err)
	}
	return controller.agentSettings(ctx)
}

func (controller *KubernetesAdminController) testLogLink(ctx *fiber.Ctx) error {
	var request kubernetes.TestLinkRequest
	if err := decodeKubernetesSettings(ctx, &request); err != nil {
		return writeKubernetes(ctx, nil, err)
	}
	value, err := requestKubernetesService(ctx).TestLogLink(ctx.UserContext(), request)
	if err != nil {
		switch err.Error() {
		case "unauthorized":
			return ctx.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "unauthorized"})
		case "not_found":
			return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_found"})
		case "timeout":
			return ctx.Status(fiber.StatusGatewayTimeout).JSON(fiber.Map{"error": "timeout"})
		case "invalid_mapping":
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_mapping"})
		}
	}
	return writeKubernetes(ctx, value, err)
}
