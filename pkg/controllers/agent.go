package controllers

import (
	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"

	"github.com/gofiber/fiber/v2"
)

// AgentController exposes admin endpoints for inspecting and curating the
// pattern catalog. All endpoints require the gateway secret configured under
// `agent.gateway_secret` (or env AGENT_GATEWAY_SECRET), sent in the
// `X-Gateway-Secret` header. When no secret is configured, every request is
// rejected — this is by design: an empty secret must not silently grant access.
type AgentController struct {
	catalog *agent.Catalog
}

// NewAgentController wires the catalog into a controller. Pass `cat=nil` if
// the agent is disabled — in that case every endpoint will return 503.
func NewAgentController(cat *agent.Catalog) *AgentController {
	return &AgentController{catalog: cat}
}

// Register attaches the agent admin endpoints to the given fiber group.
//
// Routes (under /api/agent):
//
//	GET    /patterns         list all patterns (sorted by Count desc)
//	GET    /patterns/:id     get one pattern
//	POST   /patterns/:id     update severity / label / tags
//	DELETE /patterns/:id     remove a pattern
//	POST   /flush            force-flush the catalog to disk
//	GET    /status           lightweight status (catalog size, dirty flag)
func (a *AgentController) Register(router fiber.Router) {
	g := router.Group("/agent", a.authMiddleware)
	g.Get("/status", a.getStatus)
	g.Get("/patterns", a.listPatterns)
	g.Get("/patterns/:id", a.getPattern)
	g.Post("/patterns/:id", a.updatePattern)
	g.Delete("/patterns/:id", a.deletePattern)
	g.Post("/flush", a.flush)
}

// authMiddleware enforces a shared gateway secret. Clients send the
// configured value verbatim in the `X-Gateway-Secret` header — there is no
// Bearer prefix or other framing.
func (a *AgentController) authMiddleware(c *fiber.Ctx) error {
	cfg := config.GetConfig()
	expected := cfg.Agent.GatewaySecret
	got := c.Get("X-Gateway-Secret")
	if got == "" || got != expected {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	return c.Next()
}

func (a *AgentController) getStatus(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"patterns": a.catalog.Len(),
		"dirty":    a.catalog.Dirty(),
	})
}

func (a *AgentController) listPatterns(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"patterns": a.catalog.All()})
}

func (a *AgentController) getPattern(c *fiber.Ctx) error {
	id := c.Params("id")
	p := a.catalog.Get(id)
	if p == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	return c.JSON(p)
}

type updatePatternRequest struct {
	Severity string   `json:"severity"`
	Label    string   `json:"label"`
	Tags     []string `json:"tags"`
}

func (a *AgentController) updatePattern(c *fiber.Ctx) error {
	id := c.Params("id")
	var req updatePatternRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	if !a.catalog.Label(id, req.Severity, req.Label, req.Tags) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	return c.JSON(a.catalog.Get(id))
}

func (a *AgentController) deletePattern(c *fiber.Ctx) error {
	id := c.Params("id")
	if !a.catalog.Delete(id) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (a *AgentController) flush(c *fiber.Ctx) error {
	if err := a.catalog.Persist(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"ok": true, "patterns": a.catalog.Len()})
}
