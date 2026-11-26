package controllers

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// AWSAccountLabelsController serves the labels a user gives AWS accounts
// the deployment is not connected to. Reads need the infrastructure view;
// writes need the infrastructure settings permission. It reads and writes
// the label store only, never AWS.
type AWSAccountLabelsController struct {
	store storage.AWSAccountLabelStore
	scope tenancy.OrgScope
	now   func() time.Time
}

// NewAWSAccountLabelsController builds the controller. A nil store answers
// reads with enabled=false and refuses writes as not ready.
func NewAWSAccountLabelsController(store storage.AWSAccountLabelStore, scope tenancy.OrgScope) *AWSAccountLabelsController {
	return &AWSAccountLabelsController{store: store, scope: scope, now: time.Now}
}

// Register mounts the label routes behind the admin chain: the gateway
// guard and the infrastructure view, plus the settings permission on
// writes.
func (c *AWSAccountLabelsController) Register(router fiber.Router) {
	write := core.PermissionInfrastructureSettingsWrite
	g := router.Group("/admin/aws/accounts/labels", auditAWSRefusal(write), adminGatewayGuard, requireChangesView)
	g.Get("/", c.list)
	g.Put("/:id", auditAWSWrite("aws.account.label.saved", write, auditParam("id")), requireAWSSettingsWrite, c.put)
	g.Delete("/:id", auditAWSWrite("aws.account.label.deleted", write, auditParam("id")), requireAWSSettingsWrite, c.delete)
}

// requireAWSSettingsWrite admits only a caller with the infrastructure
// settings permission.
func requireAWSSettingsWrite(ctx *fiber.Ctx) error {
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite))
	if !explicit || !allowed {
		return refusePermission(ctx, core.PermissionInfrastructureSettingsWrite)
	}
	return ctx.Next()
}

type awsAccountLabelsResponse struct {
	Enabled bool                      `json:"enabled"`
	Labels  []storage.AWSAccountLabel `json:"labels"`
}

type awsAccountLabelRequest struct {
	Label string `json:"label"`
	Name  string `json:"name"`
}

func labelStoreNotReady(ctx *fiber.Ctx) error {
	return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "aws account label storage not ready"})
}

func (c *AWSAccountLabelsController) list(ctx *fiber.Ctx) error {
	if c.store == nil {
		return ctx.JSON(awsAccountLabelsResponse{Enabled: false, Labels: []storage.AWSAccountLabel{}})
	}
	labels, err := c.store.AWSAccountLabels(ctx.UserContext(), c.scope)
	if err != nil {
		if errors.Is(err, storage.ErrUnsupported) {
			return labelStoreNotReady(ctx)
		}
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "aws account labels could not be read"})
	}
	return ctx.JSON(awsAccountLabelsResponse{Enabled: true, Labels: labels})
}

func (c *AWSAccountLabelsController) put(ctx *fiber.Ctx) error {
	if c.store == nil {
		return labelStoreNotReady(ctx)
	}
	id := strings.Clone(ctx.Params("id"))
	if !coverageAccountPattern.MatchString(id) {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "account must be a 12-digit AWS account ID"})
	}
	var request awsAccountLabelRequest
	if err := ctx.BodyParser(&request); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "body must be a JSON object with a name"})
	}
	label, err := c.store.PutAWSAccountLabel(ctx.UserContext(), c.scope, storage.AWSAccountLabel{
		AccountID: id, Label: strings.TrimSpace(request.Label), Name: request.Name,
		CreatedBy: strings.Clone(middleware.RequestActor(ctx)), CreatedAt: c.now(),
	})
	switch {
	case errors.Is(err, storage.ErrInvalidAWSAccountLabel):
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "label must be known and the name at most 128 printable characters"})
	case errors.Is(err, storage.ErrUnsupported):
		return labelStoreNotReady(ctx)
	case err != nil:
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "aws account label could not be saved"})
	}
	return ctx.JSON(fiber.Map{"label": label})
}

func (c *AWSAccountLabelsController) delete(ctx *fiber.Ctx) error {
	if c.store == nil {
		return labelStoreNotReady(ctx)
	}
	id := strings.Clone(ctx.Params("id"))
	if !coverageAccountPattern.MatchString(id) {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "account must be a 12-digit AWS account ID"})
	}
	deleted, err := c.store.DeleteAWSAccountLabel(ctx.UserContext(), c.scope, id)
	switch {
	case errors.Is(err, storage.ErrAWSAccountLabelInherited):
		return ctx.Status(fiber.StatusConflict).JSON(fiber.Map{"code": "label_inherited",
			"error": "this label was set before the upgrade and is inherited; set a new label for the account to replace it"})
	case errors.Is(err, storage.ErrUnsupported):
		return labelStoreNotReady(ctx)
	case err != nil:
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "aws account label could not be deleted"})
	case !deleted:
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no label for this account"})
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}
