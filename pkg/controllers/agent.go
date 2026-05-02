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
	shadow  *agent.ShadowLog
}

// NewAgentController wires the catalog and shadow log into a controller.
// Pass `cat=nil` if the agent is disabled — in that case every endpoint will
// return 503. `sl` may be nil to disable the shadow endpoints.
func NewAgentController(cat *agent.Catalog, sl *agent.ShadowLog) *AgentController {
	return &AgentController{catalog: cat, shadow: sl}
}

// Register attaches the agent admin endpoints to the given fiber group.
//
// Routes (under /api/agent):
//
//	GET    /patterns         list all patterns (sorted by Count desc)
//	GET    /patterns/:id     get one pattern
//	POST   /patterns/:id     update verdict / tags
//	DELETE /patterns/:id     remove a pattern
//	POST   /flush            force-flush the catalog to disk
//	GET    /status           lightweight status (catalog size, dirty flag)
//	GET    /shadow           list shadow-mode "would have alerted" events
//	GET    /shadow/stats     aggregate counts for the shadow log
//	DELETE /shadow           clear the shadow log
//	POST   /shadow/flush     force-flush the shadow log to disk
func (a *AgentController) Register(router fiber.Router) {
	g := router.Group("/agent", a.authMiddleware)
	g.Get("/status", a.getStatus)
	g.Get("/patterns", a.listPatterns)
	g.Get("/patterns/:id", a.getPattern)
	g.Post("/patterns/:id", a.updatePattern)
	g.Delete("/patterns/:id", a.deletePattern)
	g.Post("/flush", a.flush)
	g.Get("/shadow", a.listShadow)
	g.Get("/shadow/stats", a.shadowStats)
	g.Delete("/shadow", a.clearShadow)
	g.Post("/shadow/flush", a.flushShadow)
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
	status := fiber.Map{
		"patterns": a.catalog.Len(),
		"dirty":    a.catalog.Dirty(),
	}
	if a.shadow != nil {
		status["shadow_events"] = a.shadow.Len()
		status["shadow_dirty"] = a.shadow.Dirty()
	}
	return c.JSON(status)
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
	Verdict string   `json:"verdict"`
	Tags    []string `json:"tags"`
}

func (a *AgentController) updatePattern(c *fiber.Ctx) error {
	id := c.Params("id")
	var req updatePatternRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	if !a.catalog.Label(id, req.Verdict, req.Tags) {
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

// listShadow returns every shadow-mode event sorted most-recent first.
func (a *AgentController) listShadow(c *fiber.Ctx) error {
	if a.shadow == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "shadow log not enabled"})
	}
	return c.JSON(fiber.Map{"events": a.shadow.All()})
}

// shadowStats returns aggregate counts for the shadow log (events,
// total_signals, verdicts, occurrences).
func (a *AgentController) shadowStats(c *fiber.Ctx) error {
	if a.shadow == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "shadow log not enabled"})
	}
	return c.JSON(a.shadow.Stats())
}

// clearShadow drops every event and persists the empty log.
func (a *AgentController) clearShadow(c *fiber.Ctx) error {
	if a.shadow == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "shadow log not enabled"})
	}
	n := a.shadow.Clear()
	if err := a.shadow.Persist(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"ok": true, "cleared": n})
}

// flushShadow force-writes the shadow log to disk.
func (a *AgentController) flushShadow(c *fiber.Ctx) error {
	if a.shadow == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "shadow log not enabled"})
	}
	if err := a.shadow.Persist(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"ok": true, "events": a.shadow.Len()})
}
