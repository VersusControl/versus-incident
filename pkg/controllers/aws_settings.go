package controllers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"

	"github.com/gofiber/fiber/v2"
)

const awsSettingsBodyLimit = 64 << 10

// AWSSettingsController adapts the AWS connector service to the admin API.
type AWSSettingsController struct {
	service *settings.Service
}

// NewAWSSettingsController builds the controller around a connector service.
func NewAWSSettingsController(service *settings.Service) *AWSSettingsController {
	return &AWSSettingsController{service: service}
}

// Register mounts every AWS settings route behind the shared admin chain. The
// audit wrapper runs first so a refused request is recorded too; the gateway
// guard, the permission, and the infrastructure view follow in that order.
func (controller *AWSSettingsController) Register(router fiber.Router) {
	group := router.Group("/admin/aws", auditAWSSettings, adminGatewayGuard, controller.requirePermission)
	group.Use(controller.requireInfrastructureView)
	group.Get("/settings", controller.settings)
	group.Post("/connect/drafts", controller.requireSettingsWrite, controller.createDraft)
	group.Get("/connect/drafts/:id", controller.requireSettingsWrite, controller.draft)
	group.Get("/connect/drafts/:id/external-id", controller.requireSettingsWrite, controller.revealExternalID)
	group.Post("/connect/drafts/:id/verify", controller.requireSettingsWrite, controller.verify)
	group.Post("/connect/drafts/:id/regions", controller.requireSettingsWrite, controller.regions)
	group.Post("/accounts", controller.requireSettingsWrite, controller.commit)
	group.Patch("/accounts/:account_id", controller.requireSettingsWrite, controller.updateAccount)
	group.Delete("/accounts/:account_id", controller.requireSettingsWrite, controller.deleteAccount)
	group.Post("/accounts/:account_id/recheck", controller.requireSettingsWrite, controller.recheck)
	group.Put("/settings/options", controller.requireSettingsWrite, controller.saveOptions)
	group.Put("/settings/privacy", controller.requireSettingsWrite, controller.savePrivacy)
}

// auditAWSSettings records every write and the external-id reveal. Reads are
// not audited; they carry no secret.
func auditAWSSettings(ctx *fiber.Ctx) error {
	path := strings.TrimSuffix(ctx.Path(), "/")
	reveal := ctx.Method() == fiber.MethodGet && strings.HasSuffix(path, "/external-id")
	if ctx.Method() == fiber.MethodGet && !reveal {
		return ctx.Next()
	}
	err := ctx.Next()
	status := ctx.Response().StatusCode()
	if fiberErr, ok := err.(*fiber.Error); ok {
		status = fiberErr.Code
	} else if err != nil {
		status = fiber.StatusInternalServerError
	}
	action, target, result := "aws.settings.changed", "infrastructure:settings:write", middleware.AdminAuditSuccess
	if reveal {
		action = "aws.external-id.revealed"
	}
	if status == fiber.StatusTooManyRequests {
		// A throttled caller is over the verify budget, not unauthorized; keep
		// the audit trail able to tell the two apart.
		action, result = "aws.settings.rate_limited", middleware.AdminAuditDenied
	} else if status == fiber.StatusUnauthorized || status == fiber.StatusForbidden {
		action, result = "rbac.denied", middleware.AdminAuditDenied
	} else if status >= fiber.StatusBadRequest {
		result = middleware.AdminAuditDenied
	}
	middleware.RecordAdminAudit(ctx, action, target, result)
	return err
}

func (controller *AWSSettingsController) requirePermission(ctx *fiber.Ctx) error {
	if !callerCanViewInfrastructure(ctx) {
		return refuseCallerInfrastructureView(ctx)
	}
	return ctx.Next()
}

func (controller *AWSSettingsController) requireInfrastructureView(ctx *fiber.Ctx) error {
	if !callerCanViewInfrastructure(ctx) {
		return refuseCallerInfrastructureView(ctx)
	}
	return ctx.Next()
}

func (controller *AWSSettingsController) requireSettingsWrite(ctx *fiber.Ctx) error {
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite))
	if !explicit || !allowed {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "infrastructure:settings:write permission is required"})
	}
	return ctx.Next()
}

func awsOrg(ctx *fiber.Ctx) string {
	return strings.Clone(middleware.OrgFromContext(ctx))
}

func awsCanWrite(ctx *fiber.Ctx) bool {
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite))
	return explicit && allowed
}

func (controller *AWSSettingsController) settings(ctx *fiber.Ctx) error {
	view, err := controller.service.View(ctx.UserContext(), awsOrg(ctx), awsCanWrite(ctx))
	if err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.JSON(view)
}

type awsDraftRequest struct {
	Method string `json:"method"`
}

func (controller *AWSSettingsController) createDraft(ctx *fiber.Ctx) error {
	var request awsDraftRequest
	if err := decodeAWSBody(ctx, &request); err != nil {
		return writeAWSSettings(ctx, nil, settings.ErrInvalidArguments)
	}
	view, err := controller.service.CreateDraft(ctx.UserContext(), awsOrg(ctx), strings.Clone(request.Method))
	if err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.Status(fiber.StatusCreated).JSON(view)
}

func (controller *AWSSettingsController) draft(ctx *fiber.Ctx) error {
	view, err := controller.service.Draft(awsOrg(ctx), strings.Clone(ctx.Params("id")))
	if err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.JSON(view)
}

func (controller *AWSSettingsController) revealExternalID(ctx *fiber.Ctx) error {
	externalID, err := controller.service.RevealExternalID(awsOrg(ctx), strings.Clone(ctx.Params("id")))
	if err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	ctx.Set(fiber.HeaderCacheControl, "no-store")
	return ctx.JSON(fiber.Map{"external_id": externalID})
}

type awsVerifyRequest struct {
	RoleARN string `json:"role_arn"`
}

func (controller *AWSSettingsController) verify(ctx *fiber.Ctx) error {
	var request awsVerifyRequest
	if err := decodeAWSBody(ctx, &request); err != nil {
		return writeAWSSettings(ctx, nil, settings.ErrInvalidArguments)
	}
	probeID, err := controller.service.Verify(awsOrg(ctx), strings.Clone(ctx.Params("id")), strings.Clone(request.RoleARN))
	if err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.Status(fiber.StatusAccepted).JSON(fiber.Map{"probe_id": probeID})
}

func (controller *AWSSettingsController) regions(ctx *fiber.Ctx) error {
	regions, err := controller.service.DiscoverRegions(ctx.UserContext(), awsOrg(ctx), strings.Clone(ctx.Params("id")))
	if err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.JSON(regions)
}

type awsCommitRequest struct {
	DraftID     string   `json:"draft_id"`
	DisplayName string   `json:"display_name"`
	Regions     []string `json:"regions"`
}

func (controller *AWSSettingsController) commit(ctx *fiber.Ctx) error {
	var request awsCommitRequest
	if err := decodeAWSBody(ctx, &request); err != nil {
		return writeAWSSettings(ctx, nil, settings.ErrInvalidArguments)
	}
	view, err := controller.service.Commit(ctx.UserContext(), awsOrg(ctx), strings.Clone(request.DraftID), strings.Clone(request.DisplayName), cloneStrings(request.Regions))
	if err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.Status(fiber.StatusCreated).JSON(view)
}

type awsAccountPatch struct {
	DisplayName string   `json:"display_name"`
	Regions     []string `json:"regions"`
}

func (controller *AWSSettingsController) updateAccount(ctx *fiber.Ctx) error {
	var request awsAccountPatch
	if err := decodeAWSBody(ctx, &request); err != nil {
		return writeAWSSettings(ctx, nil, settings.ErrInvalidArguments)
	}
	view, err := controller.service.UpdateAccount(awsOrg(ctx), strings.Clone(ctx.Params("account_id")), strings.Clone(request.DisplayName), cloneStrings(request.Regions))
	if err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.JSON(view)
}

func (controller *AWSSettingsController) deleteAccount(ctx *fiber.Ctx) error {
	if err := controller.service.DeleteAccount(awsOrg(ctx), strings.Clone(ctx.Params("account_id"))); err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (controller *AWSSettingsController) recheck(ctx *fiber.Ctx) error {
	view, err := controller.service.Recheck(ctx.UserContext(), awsOrg(ctx), strings.Clone(ctx.Params("account_id")))
	if err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.JSON(view)
}

type awsOptionsData struct {
	CostExplorer   *bool `json:"cost_explorer"`
	Config         *bool `json:"config"`
	SecurityHub    *bool `json:"security_hub"`
	TrustedAdvisor *bool `json:"trusted_advisor"`
}

type awsBudgetOptions struct {
	MaxRequestsPerSecond *int `json:"max_requests_per_second"`
}

// awsOptionsRequest accepts the nested body (optional_data + budget) and the
// older flat spelling. Nested values win when both are present.
type awsOptionsRequest struct {
	CostExplorer         *bool             `json:"cost_explorer"`
	Config               *bool             `json:"config"`
	SecurityHub          *bool             `json:"security_hub"`
	TrustedAdvisor       *bool             `json:"trusted_advisor"`
	MaxRequestsPerSecond *int              `json:"max_requests_per_second"`
	OptionalData         *awsOptionsData   `json:"optional_data"`
	Budget               *awsBudgetOptions `json:"budget"`
}

func (request awsOptionsRequest) options() settings.Options {
	options := settings.Options{
		CostExplorer:         request.CostExplorer,
		Config:               request.Config,
		SecurityHub:          request.SecurityHub,
		TrustedAdvisor:       request.TrustedAdvisor,
		MaxRequestsPerSecond: request.MaxRequestsPerSecond,
	}
	if nested := request.OptionalData; nested != nil {
		if nested.CostExplorer != nil {
			options.CostExplorer = nested.CostExplorer
		}
		if nested.Config != nil {
			options.Config = nested.Config
		}
		if nested.SecurityHub != nil {
			options.SecurityHub = nested.SecurityHub
		}
		if nested.TrustedAdvisor != nil {
			options.TrustedAdvisor = nested.TrustedAdvisor
		}
	}
	if budget := request.Budget; budget != nil && budget.MaxRequestsPerSecond != nil {
		options.MaxRequestsPerSecond = budget.MaxRequestsPerSecond
	}
	return options
}

func (controller *AWSSettingsController) saveOptions(ctx *fiber.Ctx) error {
	var request awsOptionsRequest
	if err := decodeAWSBody(ctx, &request); err != nil {
		return writeAWSSettings(ctx, nil, settings.ErrInvalidArguments)
	}
	options := request.options()
	if err := controller.service.SaveOptions(awsOrg(ctx), options); err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	response := map[string]any{
		"optional_data": awsOptionsData{
			CostExplorer:   options.CostExplorer,
			Config:         options.Config,
			SecurityHub:    options.SecurityHub,
			TrustedAdvisor: options.TrustedAdvisor,
		},
		"budget": awsBudgetOptions{MaxRequestsPerSecond: options.MaxRequestsPerSecond},
	}
	if options.CostExplorer != nil {
		response["cost_explorer"] = *options.CostExplorer
	}
	if options.Config != nil {
		response["config"] = *options.Config
	}
	if options.SecurityHub != nil {
		response["security_hub"] = *options.SecurityHub
	}
	if options.TrustedAdvisor != nil {
		response["trusted_advisor"] = *options.TrustedAdvisor
	}
	if options.MaxRequestsPerSecond != nil {
		response["max_requests_per_second"] = *options.MaxRequestsPerSecond
	}
	return ctx.JSON(response)
}

type awsPrivacyRequest struct {
	TagValueAllowlist []string `json:"tag_value_allowlist"`
}

func (controller *AWSSettingsController) savePrivacy(ctx *fiber.Ctx) error {
	var request awsPrivacyRequest
	if err := decodeAWSBody(ctx, &request); err != nil {
		return writeAWSSettings(ctx, nil, settings.ErrInvalidArguments)
	}
	if err := controller.service.SavePrivacy(awsOrg(ctx), cloneStrings(request.TagValueAllowlist)); err != nil {
		return writeAWSSettings(ctx, nil, err)
	}
	return ctx.JSON(fiber.Map{"tag_value_allowlist": settings.SanitizeAllowlist(request.TagValueAllowlist)})
}

func decodeAWSBody(ctx *fiber.Ctx, target any) error {
	body := ctx.Body()
	if len(body) == 0 || len(body) > awsSettingsBodyLimit {
		return settings.ErrInvalidArguments
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	for index, value := range values {
		cloned[index] = strings.Clone(value)
	}
	return cloned
}

// writeAWSSettings maps a closed service error to its HTTP answer. It never
// renders provider text or a raw storage error.
func writeAWSSettings(ctx *fiber.Ctx, value any, err error) error {
	if err == nil {
		if value == nil {
			return ctx.SendStatus(fiber.StatusNoContent)
		}
		return ctx.JSON(value)
	}
	status, code := fiber.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, settings.ErrNotFound):
		status, code = fiber.StatusNotFound, "not_found"
	case errors.Is(err, settings.ErrInvalidArguments), errors.Is(err, settings.ErrInvalidRecord):
		status, code = fiber.StatusBadRequest, "invalid_arguments"
	case errors.Is(err, settings.ErrOutOfScope):
		status, code = fiber.StatusForbidden, "forbidden"
	case errors.Is(err, settings.ErrConfigManaged):
		status, code = fiber.StatusConflict, "config_managed"
	case errors.Is(err, settings.ErrNotVerified):
		status, code = fiber.StatusConflict, "not_verified"
	case errors.Is(err, settings.ErrDisabled):
		status, code = fiber.StatusConflict, "disabled"
	case errors.Is(err, settings.ErrOrganizationsUnavailable):
		status, code = fiber.StatusForbidden, "organizations_unavailable"
	case errors.Is(err, settings.ErrRateLimited):
		ctx.Set(fiber.HeaderRetryAfter, "60")
		status, code = fiber.StatusTooManyRequests, "rate_limited"
	case errors.Is(err, settings.ErrNoStorage):
		status, code = fiber.StatusServiceUnavailable, "storage_unavailable"
	case errors.Is(err, settings.ErrNoServerIdentity), errors.Is(err, awsauth.ErrNoServerIdentity):
		status, code = fiber.StatusConflict, "no_server_identity"
	default:
		if authCode := awsauth.ErrorCode(err); authCode != "" {
			status, code = fiber.StatusBadGateway, authCode
		}
	}
	return ctx.Status(status).JSON(fiber.Map{"error": code})
}
