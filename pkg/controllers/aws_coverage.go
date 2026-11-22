package controllers

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"

	"github.com/gofiber/fiber/v2"
)

// AWSCoverageScope returns the configured accounts and their Regions, read
// from settings at request time. enabled is false when the AWS connector is
// off.
type AWSCoverageScope func(ctx context.Context) (expected []snapshot.ScopeAccount, enabled bool, err error)

// AWSCoverageController serves the read-only coverage page: what the stored
// reads could not cover and why. It reads stores only, never AWS.
type AWSCoverageController struct {
	store *snapshot.Store
	scope AWSCoverageScope
}

// NewAWSCoverageController builds the controller. A nil store answers with
// enabled=false; a nil scope reads the stored units alone.
func NewAWSCoverageController(store *snapshot.Store, scope AWSCoverageScope) *AWSCoverageController {
	return &AWSCoverageController{store: store, scope: scope}
}

// Register mounts the coverage route behind the same admin chain as the
// Changes page: the gateway guard and the infrastructure view.
func (c *AWSCoverageController) Register(router fiber.Router) {
	g := router.Group("/admin/aws/coverage", adminGatewayGuard, requireChangesView)
	g.Get("/", c.coverage)
}

var (
	coverageAccountPattern = regexp.MustCompile(`^[0-9]{12}$`)
	coverageRegionPattern  = regexp.MustCompile(`^[a-z]{2}(-gov|-iso[a-z]*)?-[a-z]+-[0-9]$`)
)

// awsCoverageResponse is the coverage answer: the summary card, the three
// lists of gaps by outcome and the answers that are not gaps.
type awsCoverageResponse struct {
	Enabled  bool                     `json:"enabled"`
	Summary  snapshot.CoverageSummary `json:"summary"`
	NotRead  []snapshot.CoverageRow   `json:"not_read"`
	Capped   []snapshot.CoverageRow   `json:"capped"`
	OffInAWS []snapshot.CoverageRow   `json:"off_in_aws"`
	NotGaps  snapshot.CoverageNotGaps `json:"not_gaps"`
}

func (c *AWSCoverageController) coverage(ctx *fiber.Ctx) error {
	if c.store == nil {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	scope := snapshot.CoverageScope{
		Account: strings.Clone(ctx.Query("account")),
		Region:  strings.Clone(ctx.Query("region")),
	}
	if scope.Account != "" && !coverageAccountPattern.MatchString(scope.Account) {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "account must be a 12-digit AWS account ID"})
	}
	if scope.Region != "" && !coverageRegionPattern.MatchString(scope.Region) {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "region must be an AWS Region name"})
	}
	if c.scope != nil {
		expected, enabled, err := c.scope(ctx.UserContext())
		if err != nil {
			return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "aws settings could not be read"})
		}
		if !enabled {
			return ctx.JSON(fiber.Map{"enabled": false})
		}
		scope.Expected, scope.ExpectedKnown = expected, true
	}
	view, err := c.store.Coverage(ctx.UserContext(), scope)
	if err != nil {
		if errors.Is(err, snapshot.ErrNoStorage) {
			return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "aws inventory storage not ready"})
		}
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "aws coverage could not be read"})
	}
	return ctx.JSON(awsCoverageResponse{
		Enabled: true, Summary: view.Summary, NotRead: view.NotRead, Capped: view.Capped, OffInAWS: view.OffInAWS, NotGaps: view.NotGaps,
	})
}
