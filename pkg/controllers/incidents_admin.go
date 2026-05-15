package controllers

import (
	"errors"
	"strconv"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// IncidentAdminController exposes read endpoints for the persisted
// incident history. Same X-Gateway-Secret guard as the agent admin
// surface — see AgentController.authMiddleware.
type IncidentAdminController struct{}

// NewIncidentAdminController returns a controller. No state of its own;
// the storage provider is read lazily via services.Storage().
func NewIncidentAdminController() *IncidentAdminController {
	return &IncidentAdminController{}
}

// Register attaches the admin endpoints under /api/admin/incidents.
//
//	GET  /api/admin/incidents              list (newest first; ?limit=NN)
//	GET  /api/admin/incidents/:id          single record
//	POST /api/admin/incidents/:id/resolve  mark resolved (idempotent)
func (i *IncidentAdminController) Register(router fiber.Router) {
	g := router.Group("/admin/incidents", i.authMiddleware)
	g.Get("/", i.list)
	g.Get("/:id", i.get)
	g.Post("/:id/resolve", i.resolve)
}

// authMiddleware reuses the agent gateway secret. Keeping the same
// header name (X-Gateway-Secret) means the UI only manages one secret.
// Comparison is constant-time (see secureEqual in agent.go) to avoid
// header-length / prefix-match timing oracles.
func (i *IncidentAdminController) authMiddleware(c *fiber.Ctx) error {
	cfg := config.GetConfig()
	expected := cfg.GatewaySecret
	got := c.Get("X-Gateway-Secret")
	if expected == "" || !secureEqual(got, expected) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	return c.Next()
}

func (i *IncidentAdminController) list(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.JSON(fiber.Map{"incidents": []any{}})
	}
	limit := 0
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	recs, err := store.ListIncidents(limit)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	// Strip the (potentially large) Content blob from list responses;
	// the UI fetches the detail endpoint to see it.
	out := make([]fiber.Map, 0, len(recs))
	for _, r := range recs {
		out = append(out, summarize(r))
	}
	return c.JSON(fiber.Map{"incidents": out})
}

func (i *IncidentAdminController) get(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
	}
	rec, err := store.GetIncident(c.Params("id"))
	if errors.Is(err, storage.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(rec)
}

// summarize drops the heavy Content map from a record for list views.
func summarize(r *storage.IncidentRecord) fiber.Map {
	return fiber.Map{
		"id":                  r.ID,
		"team_id":             r.TeamID,
		"title":               r.Title,
		"source":              r.Source,
		"service":             r.Service,
		"resolved":            r.Resolved,
		"channels_notified":   r.ChannelsNotified,
		"oncall_triggered":    r.OnCallTriggered,
		"notify_status":       r.NotifyStatus,
		"notify_error":        r.NotifyError,
		"created_at":          r.CreatedAt,
		"acked_at":            r.AckedAt,
		"resolved_at":         r.ResolvedAt,
		"assigned_team_id":    r.AssignedTeamID,
		"assigned_member_ids": r.AssignedMemberIDs,
	}
}

// resolve marks an incident as resolved. Idempotent: re-resolving an
// already-resolved record is a no-op (no error, no timestamp drift).
func (i *IncidentAdminController) resolve(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
	}
	rec, err := store.GetIncident(c.Params("id"))
	if errors.Is(err, storage.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if !rec.Resolved {
		now := time.Now().UTC()
		rec.Resolved = true
		rec.ResolvedAt = &now
		if err := store.SaveIncident(rec); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
	}
	return c.JSON(fiber.Map{
		"id":          rec.ID,
		"resolved":    rec.Resolved,
		"resolved_at": rec.ResolvedAt,
	})
}
