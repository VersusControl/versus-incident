package controllers

import (
	"errors"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/teams"

	"github.com/gofiber/fiber/v2"
)

// TeamsAdminController exposes CRUD for members and teams plus an
// incident-assignment endpoint. Same X-Gateway-Secret guard as the
// other /api/admin/* controllers.
type TeamsAdminController struct {
	store *teams.Store
}

// NewTeamsAdminController returns a controller backed by the given
// teams store. Pass nil to disable the endpoints entirely (every
// request returns 503).
func NewTeamsAdminController(s *teams.Store) *TeamsAdminController {
	return &TeamsAdminController{store: s}
}

// Register mounts the admin routes:
//
//	GET    /api/admin/members              list
//	POST   /api/admin/members              create
//	GET    /api/admin/members/:id          get one
//	PATCH  /api/admin/members/:id          partial update
//	DELETE /api/admin/members/:id          delete (+ scrub team refs)
//
//	GET    /api/admin/teams                list
//	POST   /api/admin/teams                create
//	GET    /api/admin/teams/:id            get one
//	PATCH  /api/admin/teams/:id            partial update
//	DELETE /api/admin/teams/:id            delete
//
//	POST   /api/admin/incidents/:id/assign assign team + members
func (c *TeamsAdminController) Register(router fiber.Router) {
	m := router.Group("/admin/members", c.authMiddleware, c.requireStore)
	m.Get("/", c.listMembers)
	m.Post("/", c.createMember)
	m.Get("/:id", c.getMember)
	m.Patch("/:id", c.updateMember)
	m.Delete("/:id", c.deleteMember)

	t := router.Group("/admin/teams", c.authMiddleware, c.requireStore)
	t.Get("/", c.listTeams)
	t.Post("/", c.createTeam)
	t.Get("/:id", c.getTeam)
	t.Patch("/:id", c.updateTeam)
	t.Delete("/:id", c.deleteTeam)

	// Mounted as a sibling of /admin/incidents so the incidents admin
	// controller can stay focused on read-only history.
	router.Post("/admin/incidents/:id/assign", c.authMiddleware, c.requireStore, c.assignIncident)
}

func (c *TeamsAdminController) authMiddleware(ctx *fiber.Ctx) error {
	if middleware.RequestAuthorized(ctx) {
		return ctx.Next()
	}
	cfg := config.GetConfig()
	expected := cfg.GatewaySecret
	got := ctx.Get("X-Gateway-Secret")
	if expected == "" || !secureEqual(got, expected) {
		return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	return ctx.Next()
}

func (c *TeamsAdminController) requireStore(ctx *fiber.Ctx) error {
	if c.store == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "teams store not configured"})
	}
	return ctx.Next()
}

// --- members ---------------------------------------------------------------

// memberPayload mirrors teams.Member but uses pointer fields so PATCH
// callers can distinguish "omitted" from "cleared". A non-nil Meta
// pointer replaces the entire MemberMeta struct; a nil Meta leaves it
// alone.
type memberPayload struct {
	Name  string            `json:"name"`
	Alias string            `json:"alias"`
	Meta  *teams.MemberMeta `json:"meta"`
}

func (c *TeamsAdminController) listMembers(ctx *fiber.Ctx) error {
	return ctx.JSON(fiber.Map{"members": c.store.ListMembers()})
}

func (c *TeamsAdminController) createMember(ctx *fiber.Ctx) error {
	var p memberPayload
	if err := ctx.BodyParser(&p); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	in := teams.Member{Name: p.Name, Alias: p.Alias}
	if p.Meta != nil {
		in.Meta = *p.Meta
	}
	m, err := c.store.CreateMember(in)
	if err != nil {
		return mapStoreErr(ctx, err)
	}
	return ctx.Status(fiber.StatusCreated).JSON(m)
}

func (c *TeamsAdminController) getMember(ctx *fiber.Ctx) error {
	m, err := c.store.GetMember(ctx.Params("id"))
	if err != nil {
		return mapStoreErr(ctx, err)
	}
	return ctx.JSON(m)
}

func (c *TeamsAdminController) updateMember(ctx *fiber.Ctx) error {
	var p memberPayload
	if err := ctx.BodyParser(&p); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	patch := teams.Member{Name: p.Name, Alias: p.Alias}
	replaceMeta := p.Meta != nil
	if replaceMeta {
		patch.Meta = *p.Meta
	}
	m, err := c.store.UpdateMember(ctx.Params("id"), patch, replaceMeta)
	if err != nil {
		return mapStoreErr(ctx, err)
	}
	return ctx.JSON(m)
}

func (c *TeamsAdminController) deleteMember(ctx *fiber.Ctx) error {
	if err := c.store.DeleteMember(ctx.Params("id")); err != nil {
		return mapStoreErr(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// --- teams -----------------------------------------------------------------

// teamPayload uses a pointer slice for MemberIDs so PATCH can tell
// "omitted" from "replace with empty list".
type teamPayload struct {
	Name        string    `json:"name"`
	Alias       string    `json:"alias"`
	Description string    `json:"description"`
	MemberIDs   *[]string `json:"member_ids"`
}

func (c *TeamsAdminController) listTeams(ctx *fiber.Ctx) error {
	return ctx.JSON(fiber.Map{"teams": c.store.ListTeams()})
}

func (c *TeamsAdminController) createTeam(ctx *fiber.Ctx) error {
	var p teamPayload
	if err := ctx.BodyParser(&p); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	in := teams.Team{Name: p.Name, Alias: p.Alias, Description: p.Description}
	if p.MemberIDs != nil {
		in.MemberIDs = *p.MemberIDs
	}
	t, err := c.store.CreateTeam(in)
	if err != nil {
		return mapStoreErr(ctx, err)
	}
	return ctx.Status(fiber.StatusCreated).JSON(t)
}

func (c *TeamsAdminController) getTeam(ctx *fiber.Ctx) error {
	t, err := c.store.GetTeam(ctx.Params("id"))
	if err != nil {
		return mapStoreErr(ctx, err)
	}
	return ctx.JSON(t)
}

func (c *TeamsAdminController) updateTeam(ctx *fiber.Ctx) error {
	var p teamPayload
	if err := ctx.BodyParser(&p); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	patch := teams.Team{Name: p.Name, Alias: p.Alias, Description: p.Description}
	replaceMembers := p.MemberIDs != nil
	if replaceMembers {
		patch.MemberIDs = *p.MemberIDs
	}
	t, err := c.store.UpdateTeam(ctx.Params("id"), patch, replaceMembers)
	if err != nil {
		return mapStoreErr(ctx, err)
	}
	return ctx.JSON(t)
}

func (c *TeamsAdminController) deleteTeam(ctx *fiber.Ctx) error {
	if err := c.store.DeleteTeam(ctx.Params("id")); err != nil {
		return mapStoreErr(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// --- assignment ------------------------------------------------------------

type assignPayload struct {
	// Pointer so the controller can distinguish "field omitted" (leave
	// existing assignment alone) from `"team_id": ""` / `[]` (clear).
	TeamID    *string   `json:"team_id"`
	MemberIDs *[]string `json:"member_ids"`
}

func (c *TeamsAdminController) assignIncident(ctx *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "incident storage not configured"})
	}

	var p assignPayload
	if err := ctx.BodyParser(&p); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}

	// Validate references against the teams store before we mutate the
	// incident record. Empty / nil clears the field.
	if p.TeamID != nil && *p.TeamID != "" && !c.store.TeamExists(*p.TeamID) {
		return ctx.Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"error": "unknown team_id"})
	}
	if p.MemberIDs != nil {
		if err := c.store.ValidateMemberIDs(*p.MemberIDs); err != nil {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
	}

	rec, err := store.GetIncident(ctx.Params("id"))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "incident not found"})
		}
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	if p.TeamID != nil {
		rec.AssignedTeamID = *p.TeamID
	}
	if p.MemberIDs != nil {
		// Store nil rather than [] so the JSON omits the field when empty.
		if len(*p.MemberIDs) == 0 {
			rec.AssignedMemberIDs = nil
		} else {
			rec.AssignedMemberIDs = append([]string(nil), (*p.MemberIDs)...)
		}
	}

	if err := store.SaveIncident(rec); err != nil {
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return ctx.JSON(fiber.Map{
		"id":                  rec.ID,
		"assigned_team_id":    rec.AssignedTeamID,
		"assigned_member_ids": rec.AssignedMemberIDs,
		"updated_at":          time.Now().UTC(),
	})
}

// --- helpers ---------------------------------------------------------------

func mapStoreErr(ctx *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, teams.ErrNotFound):
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	case errors.Is(err, teams.ErrInvalid):
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	default:
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
}
