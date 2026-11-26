package controllers

import (
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/utils"

	"github.com/gofiber/fiber/v2"
)

// AWSChangesController serves the read-only Changes page.
type AWSChangesController struct {
	feed        *snapshot.ChangeFeed
	preferences *snapshot.ChangePreferenceStore
}

// NewAWSChangesController builds the controller. A nil feed (the AWS
// connector is off) answers with enabled=false.
func NewAWSChangesController(feed *snapshot.ChangeFeed) *AWSChangesController {
	return &AWSChangesController{feed: feed}
}

// WithPreferences keeps each signed-in user's Changes page choices in
// store. Without it the preferences route answers the defaults and cannot
// save.
func (c *AWSChangesController) WithPreferences(store *snapshot.ChangePreferenceStore) *AWSChangesController {
	c.preferences = store
	return c
}

// Register mounts the Changes routes behind the same admin chain as the
// other AWS pages: the gateway guard and the infrastructure view.
func (c *AWSChangesController) Register(router fiber.Router) {
	view := core.PermissionInfrastructureView
	g := router.Group("/admin/aws/changes", auditAWSRefusal(view), adminGatewayGuard, requireChangesView)
	g.Get("/", c.changes)
	g.Get("/groups/:id", c.group)
	g.Get("/compare", c.compare)
	// Any caller who may view the page may save their own choices.
	g.Get("/preferences", c.getPreferences)
	g.Put("/preferences", auditAWSWrite("aws.changes.preferences.saved", view, auditFixed("change preferences")), c.putPreferences)
}

// requireChangesView admits only a caller with the infrastructure view: the
// change history names resources and actors across the deployment, which
// the FinOps view alone does not cover. The caller's decision is carried
// into the request context the change feed reads.
func requireChangesView(ctx *fiber.Ctx) error {
	if !callerCanViewInfrastructure(ctx) {
		return refuseCallerInfrastructureView(ctx)
	}
	ctx.SetUserContext(callerContext(ctx, ctx.UserContext()))
	return ctx.Next()
}

// serviceQuery reads the comma-separated service filter.
func serviceQuery(ctx *fiber.Ctx) ([]string, error) {
	services, err := snapshot.ParseServices(strings.Clone(ctx.Query("service")))
	if err != nil {
		return nil, errors.New("service must be a comma-separated list of service names")
	}
	return services, nil
}

func (c *AWSChangesController) getPreferences(ctx *fiber.Ctx) error {
	preferences, err := c.preferences.Get(strings.Clone(middleware.RequestActor(ctx)))
	if err != nil {
		return preferencesError(ctx, err)
	}
	return ctx.JSON(preferences)
}

func (c *AWSChangesController) putPreferences(ctx *fiber.Ctx) error {
	var body struct {
		Services *[]string `json:"services"`
	}
	if err := json.Unmarshal(ctx.Body(), &body); err != nil || body.Services == nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "body must be {\"services\": [...]}"})
	}
	setAWSAuditTarget(ctx, "services="+strings.Join(*body.Services, ","))
	saved, err := c.preferences.Save(strings.Clone(middleware.RequestActor(ctx)), snapshot.ChangePreferences{Services: *body.Services})
	if err != nil {
		return preferencesError(ctx, err)
	}
	return ctx.JSON(saved)
}

func preferencesError(ctx *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, snapshot.ErrNoUser):
		return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "a signed-in user is required"})
	case errors.Is(err, snapshot.ErrBadFeedQuery):
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "services must be at most 64 service names"})
	case errors.Is(err, snapshot.ErrNoStorage):
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "preference storage not ready"})
	}
	return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "preferences could not be read or saved"})
}

func (c *AWSChangesController) compare(ctx *fiber.Ctx) error {
	if c.feed == nil {
		return ctx.JSON(fiber.Map{"enabled": false})
	}
	services, err := serviceQuery(ctx)
	if err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	request := snapshot.CompareRequest{
		Account:  strings.Clone(ctx.Query("account")),
		Region:   strings.Clone(ctx.Query("region")),
		Services: services,
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
		return c.changesError(ctx, err)
	}
	return ctx.JSON(answer)
}

// changesWindowLocked answers a window or compare start beyond the
// edition's limits; max_window names the longest window it serves (24h in
// the open-source build).
func (c *AWSChangesController) changesWindowLocked(ctx *fiber.Ctx) error {
	return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "window_locked", "tier": "enterprise",
		"max_window": snapshot.FormatWindow(c.feed.Limits().MaxWindow)})
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
		parsed, locked, err := snapshot.ParseWindow(ctx.Query("window"), c.feed.Limits())
		if locked {
			return c.changesWindowLocked(ctx)
		}
		if err != nil {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		window = parsed
	}
	services, err := serviceQuery(ctx)
	if err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	request := snapshot.FeedRequest{
		Window:       window,
		Since:        since,
		Account:      strings.Clone(ctx.Query("account")),
		Region:       strings.Clone(ctx.Query("region")),
		ActorClass:   strings.Clone(ctx.Query("actor_class")),
		Services:     services,
		ServiceFacet: true,
		Resource:     strings.Clone(ctx.Query("resource")),
		Text:         strings.Clone(ctx.Query("q")),
		Cursor:       strings.Clone(ctx.Query("cursor")),
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
		return c.changesError(ctx, err)
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
		return c.changesError(ctx, err)
	}
	return ctx.JSON(answer)
}

func (c *AWSChangesController) changesError(ctx *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, snapshot.ErrWindowLocked):
		return c.changesWindowLocked(ctx)
	case errors.Is(err, snapshot.ErrCursorExpired):
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cursor_expired"})
	case errors.Is(err, snapshot.ErrBadFeedQuery):
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid query"})
	case errors.Is(err, snapshot.ErrGroupNotFound):
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "group not found"})
	case errors.Is(err, snapshot.ErrNoStorage):
		return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "change storage not ready"})
	}
	logInternalError("aws changes", ctx, err)
	return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "changes could not be read"})
}

// logInternalError logs once why a request answered 500: the route and the
// wrapped storage error, secrets redacted, never the request or answer
// body. The client still sees only the generic error.
func logInternalError(feature string, ctx *fiber.Ctx, err error) {
	log.Printf("ERROR %s: %s %s failed (storage error: %s); check the server's storage (data folder or database) is reachable and not locked",
		feature, ctx.Method(), ctx.Path(), utils.RedactSecrets(err.Error()))
}
