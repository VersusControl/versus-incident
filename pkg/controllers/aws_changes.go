package controllers

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"

	"github.com/gofiber/fiber/v2"
)

// AWSChangesController serves the read-only Changes page.
type AWSChangesController struct {
	feed *snapshot.ChangeFeed
}

// NewAWSChangesController builds the controller. A nil feed (the AWS
// connector is off) answers with enabled=false.
func NewAWSChangesController(feed *snapshot.ChangeFeed) *AWSChangesController {
	return &AWSChangesController{feed: feed}
}

// Register mounts the Changes routes behind the same admin chain as the
// other AWS pages: the gateway guard and the infrastructure view.
func (c *AWSChangesController) Register(router fiber.Router) {
	g := router.Group("/admin/aws/changes", adminGatewayGuard, requireFinOpsView)
	g.Get("/", c.changes)
	g.Get("/groups/:id", c.group)
	g.Get("/compare", c.compare)
}

func (c *AWSChangesController) compare(ctx *fiber.Ctx) error {
	if c.feed == nil {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	request := snapshot.CompareRequest{
		Account: strings.Clone(ctx.Query("account")),
		Region:  strings.Clone(ctx.Query("region")),
		Service: strings.Clone(ctx.Query("service")),
	}
	from, err := time.Parse(time.RFC3339, ctx.Query("from"))
	if err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "from must be an RFC 3339 time"})
	}
	request.From = from
	if value := ctx.Query("to"); value != "" {
		to, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "to must be an RFC 3339 time"})
		}
		request.To = to
	}
	answer, err := c.feed.Compare(ctx.UserContext(), request)
	if err != nil {
		return changesError(ctx, err)
	}
	return ctx.JSON(answer)
}

// changesWindowLocked is the OSS answer for a window longer than 24 h.
func changesWindowLocked(ctx *fiber.Ctx) error {
	return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "window_locked", "tier": "enterprise", "max_window": "24h"})
}

func parseChangesWindow(value string) (time.Duration, bool, error) {
	switch strings.TrimSpace(value) {
	case "", "24h":
		return snapshot.FeedWindowDay, false, nil
	case "1h":
		return snapshot.FeedWindowHour, false, nil
	}
	parsed, err := time.ParseDuration(value)
	if err == nil && parsed > snapshot.MaxFeedWindow {
		return 0, true, nil
	}
	if strings.HasSuffix(value, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(value, "d")); err == nil && days >= 1 {
			return 0, true, nil
		}
	}
	return 0, false, errors.New("window must be 1h or 24h")
}

func (c *AWSChangesController) changes(ctx *fiber.Ctx) error {
	if c.feed == nil {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	var since time.Time
	window := snapshot.FeedWindowDay
	if value := ctx.Query("since"); value != "" {
		// An exact span: the window parameter is ignored.
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "since must be an RFC 3339 time"})
		}
		since = parsed
	} else {
		parsed, locked, err := parseChangesWindow(ctx.Query("window"))
		if locked {
			return changesWindowLocked(ctx)
		}
		if err != nil {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		window = parsed
	}
	request := snapshot.FeedRequest{
		Window:     window,
		Since:      since,
		Account:    strings.Clone(ctx.Query("account")),
		Region:     strings.Clone(ctx.Query("region")),
		ActorClass: strings.Clone(ctx.Query("actor_class")),
		Service:    strings.Clone(ctx.Query("service")),
		Resource:   strings.Clone(ctx.Query("resource")),
		Text:       strings.Clone(ctx.Query("q")),
		Cursor:     strings.Clone(ctx.Query("cursor")),
	}
	if value := ctx.Query("until"); value != "" {
		until, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "until must be an RFC 3339 time"})
		}
		request.Until = until
	}
	if value := ctx.Query("show_routine"); value != "" {
		show, err := strconv.ParseBool(value)
		if err != nil {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "show_routine must be true or false"})
		}
		request.ShowRoutine = show
	}
	if value := ctx.Query("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > snapshot.MaxFeedLimit {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "limit must be between 1 and 200"})
		}
		request.Limit = limit
	}
	answer, err := c.feed.Answer(ctx.UserContext(), request)
	if err != nil {
		return changesError(ctx, err)
	}
	return ctx.JSON(answer)
}

func (c *AWSChangesController) group(ctx *fiber.Ctx) error {
	if c.feed == nil {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	limit := 0
	if value := ctx.Query("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > snapshot.MaxFeedLimit {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "limit must be between 1 and 200"})
		}
		limit = parsed
	}
	answer, err := c.feed.Group(ctx.UserContext(), strings.Clone(ctx.Params("id")), strings.Clone(ctx.Query("cursor")), limit)
	if err != nil {
		return changesError(ctx, err)
	}
	return ctx.JSON(answer)
}

func changesError(ctx *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, snapshot.ErrWindowLocked):
		return changesWindowLocked(ctx)
	case errors.Is(err, snapshot.ErrCursorExpired):
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cursor_expired"})
	case errors.Is(err, snapshot.ErrBadFeedQuery):
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid query"})
	case errors.Is(err, snapshot.ErrGroupNotFound):
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "group not found"})
	case errors.Is(err, snapshot.ErrNoStorage):
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "change storage not ready"})
	}
	return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "changes could not be read"})
}
