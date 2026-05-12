package controllers

import (
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/agent/ai"
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
	detect  *agent.DetectLog
	health  *agent.HealthTracker
	breaker *ai.Breaker
}

// NewAgentController wires the catalog, shadow log, and detect log into a
// controller. Pass `cat=nil` if the agent is disabled — in that case every
// endpoint will return 503. `sl` may be nil to disable the shadow endpoints,
// and `dl` may be nil to disable the detect-log endpoints. `health` and
// `breaker` may be nil — `getStatus` omits the corresponding section.
func NewAgentController(cat *agent.Catalog, sl *agent.ShadowLog, dl *agent.DetectLog, health *agent.HealthTracker, breaker *ai.Breaker) *AgentController {
	return &AgentController{catalog: cat, shadow: sl, detect: dl, health: health, breaker: breaker}
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
//	GET    /services         list known services with grace status
//	POST   /services/:name/grace  control grace period (end / restart)
//	GET    /detect           list detect-mode AI calls (newest first)
//	GET    /detect/stats     aggregate counts for the detect log
//	GET    /detect/:id       get one detect-mode AI call (full prompt + response)
//	DELETE /detect           clear the detect log
//	POST   /detect/flush     force-flush the detect log to disk
//	GET    /ai/system-prompt the assembled system prompt sent on every AI call
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
	g.Get("/services", a.listServices)
	g.Post("/services/:name/grace", a.controlServiceGrace)
	g.Get("/detect", a.listDetect)
	g.Get("/detect/stats", a.detectStats)
	g.Get("/detect/:id", a.getDetect)
	g.Delete("/detect", a.clearDetect)
	g.Post("/detect/flush", a.flushDetect)
	g.Get("/ai/system-prompt", a.getSystemPrompt)
}

// authMiddleware enforces a shared gateway secret. Clients send the
// configured value verbatim in the `X-Gateway-Secret` header — there is no
// Bearer prefix or other framing. Comparison is constant-time to deny
// header-length / prefix-match timing oracles.
func (a *AgentController) authMiddleware(c *fiber.Ctx) error {
	cfg := config.GetConfig()
	expected := cfg.GatewaySecret
	got := c.Get("X-Gateway-Secret")
	if expected == "" || !secureEqual(got, expected) {
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
	if a.detect != nil {
		status["detect_events"] = a.detect.Len()
		status["detect_dirty"] = a.detect.Dirty()
	}
	if a.health != nil {
		status["sources"] = sourcesPayload(a.health)
	}
	if a.breaker != nil {
		status["ai"] = a.breaker.Stats()
	}
	return c.JSON(status)
}

// sourcePayload is the JSON view of one SourceHealth row surfaced
// under /api/agent/status. It mirrors the struct but uses RFC3339
// timestamps and snake_case keys (consistent with the rest of the
// admin API).
type sourcePayload struct {
	Name                string `json:"name"`
	OK                  bool   `json:"ok"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error,omitempty"`
	LastErrorAt         string `json:"last_error_at,omitempty"`
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	InCooldownUntil     string `json:"in_cooldown_until,omitempty"`
	TotalPullsOK        int64  `json:"total_pulls_ok"`
	TotalPullsFailed    int64  `json:"total_pulls_failed"`
	TotalSignalsPulled  int64  `json:"total_signals_pulled"`
	TotalSignalsDropped int64  `json:"total_signals_dropped"`
	LastPullDurationMs  int64  `json:"last_pull_duration_ms"`
	LastSignalsPulled   int    `json:"last_signals_pulled"`
}

func sourcesPayload(h *agent.HealthTracker) []sourcePayload {
	snap := h.Snapshot()
	out := make([]sourcePayload, 0, len(snap))
	for _, s := range snap {
		row := sourcePayload{
			Name:                s.Name,
			OK:                  s.ConsecutiveFailures == 0,
			ConsecutiveFailures: s.ConsecutiveFailures,
			LastError:           s.LastError,
			TotalPullsOK:        s.TotalPullsOK,
			TotalPullsFailed:    s.TotalPullsFailed,
			TotalSignalsPulled:  s.TotalSignalsPulled,
			TotalSignalsDropped: s.TotalSignalsDropped,
			LastPullDurationMs:  s.LastPullDurationMs,
			LastSignalsPulled:   s.LastSignalsPulled,
		}
		if !s.LastErrorAt.IsZero() {
			row.LastErrorAt = s.LastErrorAt.Format(time.RFC3339)
		}
		if !s.LastSuccessAt.IsZero() {
			row.LastSuccessAt = s.LastSuccessAt.Format(time.RFC3339)
		}
		if !s.InCooldownUntil.IsZero() {
			row.InCooldownUntil = s.InCooldownUntil.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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

// listServices returns every known service with its first-seen timestamp.
func (a *AgentController) listServices(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"services": a.catalog.AllServices()})
}

type serviceGraceRequest struct {
	Action string `json:"action"` // "end" | "restart"
}

// controlServiceGrace lets an operator end or restart a service's grace period.
func (a *AgentController) controlServiceGrace(c *fiber.Ctx) error {
	name := c.Params("name")
	var req serviceGraceRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	switch req.Action {
	case "end":
		if !a.catalog.EndServiceGrace(name) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "service not found"})
		}
	case "restart":
		if !a.catalog.RestartServiceGrace(name) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "service not found"})
		}
	default:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "action must be \"end\" or \"restart\""})
	}
	return c.JSON(fiber.Map{"ok": true})
}

// listDetect returns every detect-mode AI call (newest first). Each
// entry includes the user prompt sent, the raw model response, and the
// parsed finding so the UI can render an audit trail.
func (a *AgentController) listDetect(c *fiber.Ctx) error {
	if a.detect == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "detect log not enabled"})
	}
	return c.JSON(fiber.Map{"events": a.detect.All()})
}

// detectStats returns aggregate counts for the detect log (per
// outcome, per verdict, per severity).
func (a *AgentController) detectStats(c *fiber.Ctx) error {
	if a.detect == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "detect log not enabled"})
	}
	return c.JSON(a.detect.Stats())
}

// getDetect returns one detect event by ID.
func (a *AgentController) getDetect(c *fiber.Ctx) error {
	if a.detect == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "detect log not enabled"})
	}
	e := a.detect.Get(c.Params("id"))
	if e == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	return c.JSON(e)
}

// clearDetect drops every event and persists the empty log.
func (a *AgentController) clearDetect(c *fiber.Ctx) error {
	if a.detect == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "detect log not enabled"})
	}
	n := a.detect.Clear()
	if err := a.detect.Persist(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"ok": true, "cleared": n})
}

// flushDetect force-writes the detect log to disk.
func (a *AgentController) flushDetect(c *fiber.Ctx) error {
	if a.detect == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "detect log not enabled"})
	}
	if err := a.detect.Persist(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"ok": true, "events": a.detect.Len()})
}

// getSystemPrompt returns the assembled system prompt sent to the model
// on every AI call. Detect events store only the user prompt to keep
// the on-disk log small; this endpoint provides the constant half.
func (a *AgentController) getSystemPrompt(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"system_prompt": ai.SystemPrompt()})
}
