package controllers

import (
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/gofiber/fiber/v2"
	"strings"
)

func (controller *KubernetesAdminController) issueAlerts(ctx *fiber.Ctx) error {
	value, err := requestKubernetesService(ctx).IssueAlertSettings(ctx.UserContext())
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite))
	value.CanWrite = allowed && explicit
	if config.GetConfigOrNil() != nil {
		cfg := config.GetConfigForAlert(ctx.UserContext(), nil)
		value.LinksReady = cfg.PublicHost != ""
		value.ChannelsEnabled = cfg.Alert.Slack.Enable || cfg.Alert.Telegram.Enable || cfg.Alert.Viber.Enable || cfg.Alert.Email.Enable || cfg.Alert.MSTeams.Enable || cfg.Alert.Lark.Enable
	}
	return writeKubernetes(ctx, value, err)
}

func (controller *KubernetesAdminController) saveIssueAlerts(ctx *fiber.Ctx) error {
	var request struct {
		Enabled bool   `json:"enabled"`
		Level   string `json:"level"`
	}
	if err := decodeKubernetesSettings(ctx, &request); err != nil {
		return writeKubernetes(ctx, nil, err)
	}
	err := requestKubernetesService(ctx).SaveIssueAlertSettings(ctx.UserContext(), request.Enabled, request.Level, strings.Clone(middleware.RequestActor(ctx)))
	if err != nil {
		return writeKubernetes(ctx, nil, err)
	}
	return controller.issueAlerts(ctx)
}
