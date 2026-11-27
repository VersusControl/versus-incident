package controllers

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
)

// AWS write audit. The AWS settings chain is mounted on /admin/aws and so
// runs in front of every AWS page's routes. Each page's write records its
// own action, with what it acted on as the target; a write refused before
// its route runs (the gateway, or the view permission) is recorded by
// whichever wrapper saw the refusal first, as rbac.denied on the permission
// the write needs. Each request gets exactly one record.

// awsAuditRecordedKey marks a request whose audit record is written, so a
// wrapper in front of it does not write a second one.
const awsAuditRecordedKey = "aws_write_audited"

// awsAuditTargetKey lets a handler name what it acted on once it has read
// its body.
const awsAuditTargetKey = "aws_audit_target"

// awsAuditTargetMax bounds one recorded target.
const awsAuditTargetMax = 512

// awsWriteRoutes are the AWS pages under /admin/aws that audit their own
// writes, with the permission those writes need.
var awsWriteRoutes = []struct {
	prefix     string
	permission core.Permission
}{
	{"/admin/aws/issues", core.PermissionInfrastructureIssuesWrite},
	{"/admin/aws/accounts/labels", core.PermissionInfrastructureSettingsWrite},
	{"/admin/aws/groups/corrections", core.PermissionInfrastructureSettingsWrite},
	{"/admin/aws/changes", core.PermissionInfrastructureView},
}

// awsWriteRoutePermission reports whether path belongs to a page that
// audits its own writes, and the permission they need.
func awsWriteRoutePermission(path string) (core.Permission, bool) {
	for _, route := range awsWriteRoutes {
		if strings.Contains(path, route.prefix) {
			return route.permission, true
		}
	}
	return "", false
}

func recordAWSWriteAudit(ctx *fiber.Ctx, action, target, result string) {
	middleware.RecordAdminAudit(ctx, action, target, result)
	ctx.Locals(awsAuditRecordedKey, true)
}

func awsWriteAudited(ctx *fiber.Ctx) bool {
	recorded, _ := ctx.Locals(awsAuditRecordedKey).(bool)
	return recorded
}

// setAWSAuditTarget names what a write acted on.
func setAWSAuditTarget(ctx *fiber.Ctx, target string) {
	ctx.Locals(awsAuditTargetKey, auditTargetValue(target))
}

// awsResponseStatus is the status a handler chain answered with.
func awsResponseStatus(ctx *fiber.Ctx, err error) int {
	status := ctx.Response().StatusCode()
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		status = fiberErr.Code
	} else if err != nil {
		status = fiber.StatusInternalServerError
	}
	return status
}

func awsRefused(status int) bool {
	return status == fiber.StatusUnauthorized || status == fiber.StatusForbidden
}

func safeMethod(method string) bool {
	return method == fiber.MethodGet || method == fiber.MethodHead || method == fiber.MethodOptions
}

// auditAWSRefusal records a write refused before its route recorded it:
// by the gateway, or by the view permission in front of the route.
func auditAWSRefusal(permission core.Permission) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		return auditAWSRefusalOf(ctx, permission)
	}
}

func auditAWSRefusalOf(ctx *fiber.Ctx, permission core.Permission) error {
	if safeMethod(ctx.Method()) {
		return ctx.Next()
	}
	err := ctx.Next()
	if !awsWriteAudited(ctx) && awsRefused(awsResponseStatus(ctx, err)) {
		recordAWSWriteAudit(ctx, "rbac.denied", string(permission), middleware.AdminAuditDenied)
	}
	return err
}

// auditAWSWrite records one audit row for a write route: action and its
// target, the outcome, and a refusal as rbac.denied on permission. The
// target is what the handler named, else what target reads from the
// request.
func auditAWSWrite(action string, permission core.Permission, target func(*fiber.Ctx) string) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		fallback := auditTargetValue(target(ctx))
		err := ctx.Next()
		status := awsResponseStatus(ctx, err)
		act, recorded, result := action, fallback, middleware.AdminAuditSuccess
		if named, ok := ctx.Locals(awsAuditTargetKey).(string); ok && named != "" {
			recorded = named
		}
		switch {
		case awsRefused(status):
			act, recorded, result = "rbac.denied", string(permission), middleware.AdminAuditDenied
		case status >= fiber.StatusBadRequest:
			result = middleware.AdminAuditDenied
		}
		recordAWSWriteAudit(ctx, act, recorded, result)
		return err
	}
}

// auditParam targets the route parameter name.
func auditParam(name string) func(*fiber.Ctx) string {
	return func(ctx *fiber.Ctx) string { return ctx.Params(name) }
}

// auditFixed targets a fixed name.
func auditFixed(target string) func(*fiber.Ctx) string {
	return func(*fiber.Ctx) string { return target }
}

// auditTargetValue copies a request value into an audit target: cut to
// awsAuditTargetMax, with spaces and control characters replaced so a
// value cannot forge another field.
func auditTargetValue(value string) string {
	var out strings.Builder
	for _, r := range value {
		if out.Len() >= awsAuditTargetMax {
			break
		}
		if r > ' ' && r < 0x7f || r == ' ' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}
