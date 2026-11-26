package controllers

import (
	"context"

	"github.com/VersusControl/versus-incident/pkg/aws/callcost"

	"github.com/gofiber/fiber/v2"
)

// AWSCollectionCostSource builds the inputs of one cost answer at request
// time: the ledger, the snapshot store, the connected accounts and the
// budgets in force. enabled is false when the AWS connector is off.
type AWSCollectionCostSource func(ctx context.Context) (inputs callcost.Inputs, enabled bool, err error)

// AWSCollectionCostController serves the Settings collection cost card:
// the calls Versus made in the last 24 hours, the budgets, what the daily
// target slowed down and the month's estimate. It reads stores only, never
// AWS.
type AWSCollectionCostController struct {
	source AWSCollectionCostSource
}

// NewAWSCollectionCostController builds the controller. A nil source
// answers with enabled=false.
func NewAWSCollectionCostController(source AWSCollectionCostSource) *AWSCollectionCostController {
	return &AWSCollectionCostController{source: source}
}

// Register mounts the route behind the gateway guard and the
// infrastructure view.
func (c *AWSCollectionCostController) Register(router fiber.Router) {
	g := router.Group("/admin/aws/settings/cost", adminGatewayGuard, requireChangesView)
	g.Get("/", c.cost)
}

type awsCollectionCostResponse struct {
	Enabled bool `json:"enabled"`
	callcost.View
}

func (c *AWSCollectionCostController) cost(ctx *fiber.Ctx) error {
	if c.source == nil {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	inputs, enabled, err := c.source(ctx.UserContext())
	if err != nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "aws settings could not be read"})
	}
	if !enabled {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	view, err := callcost.Read(ctx.UserContext(), inputs)
	if err != nil {
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "aws call ledger could not be read"})
	}
	return ctx.JSON(awsCollectionCostResponse{Enabled: true, View: view})
}
