package controllers

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/estate"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// AWSEstateController serves the read-only Overview and Topology pages. It
// parses the request and shapes the answer; the estate service reads the
// stores, never AWS.
type AWSEstateController struct {
	service *estate.Service
}

// NewAWSEstateController builds the controller. A nil service answers
// enabled=false.
func NewAWSEstateController(service *estate.Service) *AWSEstateController {
	return &AWSEstateController{service: service}
}

// Register mounts the Overview and Topology routes behind the same admin
// chain as the coverage page: the gateway guard and the infrastructure
// view.
func (c *AWSEstateController) Register(router fiber.Router) {
	router.Group("/admin/aws/overview", adminGatewayGuard, requireChangesView).Get("/", c.overview)
	topology := router.Group("/admin/aws/topology", adminGatewayGuard, requireChangesView)
	topology.Get("/", c.topology)
	topology.Get("/search", c.search)
}

func awsEstateError(ctx *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, estate.ErrInvalidQuery):
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "level, account, group, ref, hops and lens must be values the topology knows",
			"code": "invalid_query"})
	case errors.Is(err, estate.ErrNotFound):
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not in the stored reads", "code": "not_found"})
	case errors.Is(err, snapshot.ErrNoStorage):
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "aws inventory storage not ready", "code": "not_ready"})
	}
	return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "aws estate could not be read", "code": "internal"})
}

func (c *AWSEstateController) overview(ctx *fiber.Ctx) error {
	if c.service == nil {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	scope, err := estateScope(ctx)
	if err != nil {
		return awsEstateError(ctx, err)
	}
	view, err := c.service.Overview(ctx.UserContext(), scope)
	if err != nil {
		return awsEstateError(ctx, err)
	}
	return ctx.JSON(view)
}

func (c *AWSEstateController) topology(ctx *fiber.Ctx) error {
	if c.service == nil {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	scope, err := estateScope(ctx)
	if err != nil {
		return awsEstateError(ctx, err)
	}
	query := estate.TopologyQuery{
		Level: strings.Clone(ctx.Query("level")), Group: strings.Clone(ctx.Query("group")), Ref: strings.Clone(ctx.Query("ref")),
		Accounts: scope.Accounts, Region: scope.Region,
	}
	if len(scope.Accounts) == 1 {
		query.Account = scope.Accounts[0]
	}
	if text := ctx.Query("hops"); text != "" {
		hops, err := strconv.Atoi(text)
		if err != nil || hops < 1 {
			return awsEstateError(ctx, estate.ErrInvalidQuery)
		}
		query.Hops = hops
	}
	if text := ctx.Query("lens"); text != "" {
		for lens := range strings.SplitSeq(text, ",") {
			if lens = strings.TrimSpace(lens); lens != "" {
				query.Lenses = append(query.Lenses, strings.Clone(lens))
			}
		}
		if len(query.Lenses) > 4 {
			return awsEstateError(ctx, estate.ErrInvalidQuery)
		}
	}
	view, err := c.service.Topology(ctx.UserContext(), query)
	if err != nil {
		return awsEstateError(ctx, err)
	}
	return ctx.JSON(view)
}

// estateScope reads the scope bar: account, repeated or comma-separated,
// and region.
func estateScope(ctx *fiber.Ctx) (estate.Scope, error) {
	scope := estate.Scope{Region: strings.Clone(ctx.Query("region"))}
	seen := map[string]bool{}
	for _, value := range ctx.Context().QueryArgs().PeekMulti("account") {
		for account := range strings.SplitSeq(string(value), ",") {
			if account = strings.TrimSpace(account); account != "" && !seen[account] {
				seen[account] = true
				scope.Accounts = append(scope.Accounts, account)
			}
		}
	}
	return scope, scope.Valid()
}

func (c *AWSEstateController) search(ctx *fiber.Ctx) error {
	if c.service == nil {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	scope, err := estateScope(ctx)
	if err != nil {
		return awsEstateError(ctx, err)
	}
	result, err := c.service.Search(ctx.UserContext(), estate.SearchQuery{Text: strings.Clone(ctx.Query("q")), Scope: scope})
	if err != nil {
		return awsEstateError(ctx, err)
	}
	return ctx.JSON(result)
}
