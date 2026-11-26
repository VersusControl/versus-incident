package controllers

import (
	"context"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"

	"github.com/gofiber/fiber/v2"
)

const communityRequestActor = "gateway"

// adminGatewayGuard is the ONE gateway-secret guard shared by every admin
// controller. Extracting it out of eight per-controller copies removes the
// order-dependent double-registration Fiber creates when two controllers mount
// the same path prefix with their own USE middleware, and gives the security
// seam a single definition. An enterprise auth handler may have already
// authorized this request with an alternative credential (SSO); honour that so
// one enterprise credential unlocks both the data plane and the admin surface.
// Community OSS never sets it, so the gateway check is unchanged. Comparison
// is constant-time to deny header-length / prefix-match timing oracles.
func adminGatewayGuard(c *fiber.Ctx) error {
	if middleware.RequestAuthorized(c) {
		return c.Next()
	}
	if !middleware.GatewayAuthEnabled() {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	cfg := config.GetConfig()
	expected := cfg.GatewaySecret
	got := c.Get("X-Gateway-Secret")
	if expected != "" && secureEqual(got, expected) {
		grantCommunityPermissions(c)
		return c.Next()
	}
	if !validGatewaySession(c.Cookies(gatewaySessionCookieName), expected, time.Now()) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if !requestIsSafe(c.Method()) && !requestHasSameOrigin(c) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "invalid request origin"})
	}
	grantCommunityPermissions(c)
	return c.Next()
}

func grantCommunityPermissions(c *fiber.Ctx) {
	if middleware.RequestActor(c) == "" {
		middleware.SetRequestActor(c, communityRequestActor)
	}
	for _, permission := range []core.Permission{core.PermissionInfrastructureView, core.PermissionInfrastructureSettingsWrite, core.PermissionInfrastructureIssuesWrite, core.PermissionServiceHealthSettingsWrite, core.PermissionAgentApprove} {
		name := string(permission)
		if _, explicit := middleware.RequestPermission(c, name); !explicit {
			middleware.SetRequestPermission(c, name, true)
		}
	}
}

func callerAuthorization(c *fiber.Ctx) core.CallerAuthorization {
	allowed, explicit := middleware.RequestPermission(c, string(core.PermissionInfrastructureView))
	permissions := make(map[core.Permission]bool)
	if explicit {
		permissions[core.PermissionInfrastructureView] = allowed
	}
	allowed, explicit = middleware.RequestPermission(c, string(core.PermissionAgentApprove))
	if explicit {
		permissions[core.PermissionAgentApprove] = allowed
	}
	allowed, explicit = middleware.RequestPermission(c, string(core.PermissionFinOpsView))
	if explicit {
		permissions[core.PermissionFinOpsView] = allowed
	}
	accounts := core.CallerAccountScope(c.UserContext())
	if ids, explicit := middleware.RequestAccountScope(c); explicit {
		accounts = &core.AccountScope{IDs: ids}
	}
	toolGroups := core.CallerToolGroups(c.UserContext())
	if groups, explicit := middleware.RequestToolGroups(c); explicit {
		toolGroups = groups
	}
	return core.CallerAuthorization{Authenticated: true, Actor: requestActor(c), Permissions: permissions, Clusters: core.CallerClusterScope(c.UserContext()), Accounts: accounts, ToolGroups: toolGroups}
}

func callerContext(c *fiber.Ctx, parent context.Context) context.Context {
	return core.WithCallerAuthorization(parent, callerAuthorization(c))
}

// refusePermission is the 403 every permission guard answers. error keeps the
// readable text existing clients show; code and permission give a client a
// stable denial state to render instead of a load error.
func refusePermission(c *fiber.Ctx, permission core.Permission) error {
	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"error":      string(permission) + " permission is required",
		"code":       "permission_required",
		"permission": string(permission),
	})
}
