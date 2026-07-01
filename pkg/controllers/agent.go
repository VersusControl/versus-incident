package controllers

import (
	"fmt"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/analyze"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// Stable, namespaced audit actions for the state-changing agent admin routes.
// The controller emits these through the generic middleware.RecordAdminAudit
// seam (a community no-op); the enterprise build's audit layer records them
// into its per-org append-only trail. The strings are the shared vocabulary —
// kept in sync with the enterprise audit catalog (pkg/audit) without the OSS
// tree importing it.
const (
	auditActionPatternsCleared  = "agent.patterns.cleared"
	auditActionServicesCleared  = "agent.services.cleared"
	auditActionShadowCleared    = "agent.shadow.cleared"
	auditActionDetectCleared    = "agent.detect.cleared"
	auditActionServiceCreated   = "agent.service.created"
	auditActionServiceRenamed   = "agent.service.renamed"
	auditActionServiceDeleted   = "agent.service.deleted"
	auditActionOverrideSet      = "agent.service_override.set"
	auditActionOverrideDeleted  = "agent.service_override.deleted"
	auditActionPatternRelabeled = "agent.pattern.relabeled"
	auditActionPatternDeleted   = "agent.pattern.deleted"
)

// AgentController exposes admin endpoints for inspecting and curating the
// pattern catalog. All endpoints require the gateway secret configured under
// `agent.gateway_secret` (or env AGENT_GATEWAY_SECRET), sent in the
// `X-Gateway-Secret` header. When no secret is configured, every request is
// rejected — this is by design: an empty secret must not silently grant access.
type AgentController struct {
	catalog         *agent.Catalog
	miner           *agent.Miner
	shadow          *agent.ShadowLog
	detect          *agent.DetectLog
	overrides       *agent.ServiceOverrideStore
	runbooksEnabled bool
}

// NewAgentController wires the catalog, miner, shadow log, and detect log into
// a controller. Pass `cat=nil` if the agent is disabled — in that case every
// endpoint will return 503. `mn` is the shared drain miner; it may be nil (the
// patterns clear then wipes only the persisted patterns, not in-memory miner
// state). `sl` may be nil to disable the shadow endpoints, and `dl` may
// be nil to disable the detect-log endpoints. `ov` is the manual-attribution
// override store backing the service-override endpoints; it may be nil to
// disable them. `runbooksEnabled` tells the status endpoint whether the
// runbooks subsystem is available.
func NewAgentController(cat *agent.Catalog, mn *agent.Miner, sl *agent.ShadowLog, dl *agent.DetectLog, ov *agent.ServiceOverrideStore, runbooksEnabled bool) *AgentController {
	return &AgentController{catalog: cat, miner: mn, shadow: sl, detect: dl, overrides: ov, runbooksEnabled: runbooksEnabled}
}

// Register attaches the agent admin endpoints to the given fiber group.
//
// Routes (under /api/agent):
//
//	GET    /patterns         list all patterns (sorted by Count desc)
//	GET    /patterns/:id     get one pattern
//	POST   /patterns/:id     update verdict / tags
//	DELETE /patterns/:id     remove a pattern
//	DELETE /patterns         wipe ALL learned log patterns (relearn fresh)
//	GET    /status           lightweight status (catalog size, dirty flag)
//	GET    /shadow           list shadow-mode "would have alerted" events
//	GET    /shadow/stats     aggregate counts for the shadow log
//	DELETE /shadow           clear the shadow log
//	POST   /shadow/flush     force-flush the shadow log to disk
//	GET    /services         list known services with grace status
//	GET    /services/:name    aggregate detail for one service (meta + grace +
//	                          patterns + bounded incident summary)
//	POST   /services          create a manual service (selectable override target)
//	PUT    /services/:name    rename a manual service
//	DELETE /services/:name    delete a manual service (blocked when overrides target it)
//	DELETE /services         wipe ALL discovered/manual services (re-discover fresh)
//	POST   /services/:name/grace  control grace period (end / restart)
//	GET    /service-overrides       list manual-attribution override rules
//	POST   /service-overrides       create/replace one override rule
//	DELETE /service-overrides/:id   delete one override rule
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
	g.Delete("/patterns", a.clearPatterns)
	g.Get("/shadow", a.listShadow)
	g.Get("/shadow/stats", a.shadowStats)
	g.Delete("/shadow", a.clearShadow)
	g.Post("/shadow/flush", a.flushShadow)
	g.Get("/services", a.listServices)
	g.Post("/services", a.createService)
	g.Delete("/services", a.clearServices)
	g.Get("/services/:name", a.getServiceDetail)
	g.Put("/services/:name", a.renameService)
	g.Delete("/services/:name", a.deleteService)
	g.Post("/services/:name/grace", a.controlServiceGrace)
	g.Get("/service-overrides", a.listServiceOverrides)
	g.Post("/service-overrides", a.createServiceOverride)
	g.Delete("/service-overrides/:id", a.deleteServiceOverride)
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
	// An enterprise auth handler may have already authenticated this request
	// with an alternative credential (e.g. an SSO session); honour that so a
	// single enterprise credential unlocks both the data plane and the admin
	// surfaces. Community OSS never sets this, so the gateway check is unchanged.
	if middleware.RequestAuthorized(c) {
		return c.Next()
	}
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
		"patterns":           a.catalog.Len(),
		"dirty":              a.catalog.Dirty(),
		"runbooks_available": a.runbooksEnabled,
	}
	if a.shadow != nil {
		status["shadow_events"] = a.shadow.Len()
		status["shadow_dirty"] = a.shadow.Dirty()
	}
	if a.detect != nil {
		status["detect_events"] = a.detect.Len()
		status["detect_dirty"] = a.detect.Dirty()
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
		middleware.RecordAdminAudit(c, auditActionPatternRelabeled, id, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	if !a.catalog.Label(id, req.Verdict, req.Tags) {
		middleware.RecordAdminAudit(c, auditActionPatternRelabeled, id, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	middleware.RecordAdminAudit(c, auditActionPatternRelabeled, id, middleware.AdminAuditSuccess)
	return c.JSON(a.catalog.Get(id))
}

func (a *AgentController) deletePattern(c *fiber.Ctx) error {
	id := c.Params("id")
	if !a.catalog.Delete(id) {
		middleware.RecordAdminAudit(c, auditActionPatternDeleted, id, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	middleware.RecordAdminAudit(c, auditActionPatternDeleted, id, middleware.AdminAuditSuccess)
	return c.SendStatus(fiber.StatusNoContent)
}

// clearPatterns wipes every learned log pattern — the whole pattern catalog —
// and persists the empty pattern set, so the agent relearns log patterns from
// scratch on the next tick. Discovered/manual services are LEFT INTACT. It also
// resets the shared drain miner (when wired) so recurring lines are
// re-discovered as new rather than resumed against pre-reset templates.
// Admin-gated by authMiddleware like every other destructive agent mutation;
// returns a count of the patterns cleared.
func (a *AgentController) clearPatterns(c *fiber.Ctx) error {
	patterns, err := a.catalog.ResetPatterns()
	if err != nil {
		middleware.RecordAdminAudit(c, auditActionPatternsCleared, "all patterns", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if a.miner != nil {
		a.miner.Reset()
	}
	// Reflect the pattern-reset scope AND the cleared count so the trail records
	// exactly how much learned pattern state this action wiped.
	target := fmt.Sprintf("all patterns (patterns=%d)", patterns)
	middleware.RecordAdminAudit(c, auditActionPatternsCleared, target, middleware.AdminAuditSuccess)
	return c.JSON(fiber.Map{"ok": true, "patterns": patterns})
}

// clearServices wipes every discovered/manual service — the whole service
// catalog — and persists the empty service set, so the agent re-discovers
// services from scratch on the next tick. Learned log patterns and the drain
// miner are LEFT INTACT. Admin-gated by authMiddleware like every other
// destructive agent mutation; returns a count of the services cleared.
func (a *AgentController) clearServices(c *fiber.Ctx) error {
	services, err := a.catalog.ResetServices()
	if err != nil {
		middleware.RecordAdminAudit(c, auditActionServicesCleared, "all services", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	// Reflect the service-reset scope AND the cleared count so the trail records
	// exactly how much discovered service state this action wiped.
	target := fmt.Sprintf("all services (services=%d)", services)
	middleware.RecordAdminAudit(c, auditActionServicesCleared, target, middleware.AdminAuditSuccess)
	return c.JSON(fiber.Map{"ok": true, "services": services})
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
		middleware.RecordAdminAudit(c, auditActionShadowCleared, "shadow log", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "shadow log not enabled"})
	}
	n := a.shadow.Clear()
	if err := a.shadow.Persist(); err != nil {
		middleware.RecordAdminAudit(c, auditActionShadowCleared, "shadow log", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	middleware.RecordAdminAudit(c, auditActionShadowCleared, fmt.Sprintf("shadow log (cleared=%d)", n), middleware.AdminAuditSuccess)
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

// serviceIncidentScanLimit bounds the incident-history scan for the
// service-detail aggregation (newest-first). It matches the analyze-mode
// incident collector's default page size so the bounded read stays cheap on
// large histories.
const serviceIncidentScanLimit = 500

// serviceIncidentWindowDays is the rolling window the per-service incident
// summary covers.
const serviceIncidentWindowDays = 30

// serviceRecentIncidentMax caps the recent-incident list in the detail
// response.
const serviceRecentIncidentMax = 10

// getServiceDetail returns an org-aware aggregate view of one service: its
// first-seen + grace status, the patterns attributed to it, and a bounded
// summary of its recent incidents.
//
// This is the OSS half of the X30 service-detail surface — logs (patterns) and
// incidents only. Metrics/traces are an Enterprise capability and ride a
// separate /intel endpoint; they are intentionally absent from this response so
// the OSS shape carries no metric/trace fields. An unknown service (not in the
// catalog) returns 404.
//
// The pattern catalog, service registry, and incident store are single-tenant
// OSS state: every entry is keyed under storage.DefaultOrgID. We resolve org the
// same way the data-plane reads of that OSS-owned state do — to the catalog's
// single-tenant org — so a single-org licensed deployment serves this endpoint
// AND the enterprise /intel endpoint under the same deployment. (Enterprise
// learned baselines key under the deployment org separately; this read is the
// OSS catalog's, which is default.)
func (a *AgentController) getServiceDetail(c *fiber.Ctx) error {
	name := c.Params("name")
	org := storage.DefaultOrgID

	// Service meta + grace. AllServices() is the trusted catalog view; the key
	// is the catalog service name, never a redacted value.
	info, ok := a.catalog.AllServices()[name]
	if !ok || storage.NormalizeOrgID(info.OrgID) != org {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "service not found"})
	}

	inGrace := false
	graceRemaining := 0
	if grace := serviceGraceDuration(); grace > 0 {
		if rem := time.Until(info.FirstSeen.Add(grace)); rem > 0 {
			inGrace = true
			graceRemaining = int(rem.Seconds())
		}
	}

	// Patterns attributed to this service (org-scoped). catalog.All() is already
	// sorted by Count desc, so the most-frequent patterns lead.
	patterns := make([]fiber.Map, 0)
	for _, p := range a.catalog.All() {
		if p.Service != name || storage.NormalizeOrgID(p.OrgID) != org {
			continue
		}
		patterns = append(patterns, fiber.Map{
			"id":        p.ID,
			"template":  p.Template,
			"count":     p.Count,
			"verdict":   p.Verdict,
			"source":    p.Source,
			"last_seen": p.LastSeen,
			"tags":      p.Tags,
		})
	}

	incidents := a.serviceIncidentSummary(name, org)

	return c.JSON(fiber.Map{
		"service":                 name,
		"first_seen":              info.FirstSeen,
		"in_grace":                inGrace,
		"grace_seconds_remaining": graceRemaining,
		"patterns":                patterns,
		"incidents":               incidents,
		"counts": fiber.Map{
			"patterns":  len(patterns),
			"incidents": incidents["count"],
		},
	})
}

// serviceGraceDuration parses the configured new-service grace window, or 0
// when unset/invalid (grace disabled). Mirrors agent.parseDurationOr without
// importing the unexported helper.
func serviceGraceDuration() time.Duration {
	d, err := time.ParseDuration(config.GetConfig().Agent.NewServiceGrace)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// serviceIncidentSummary builds the bounded, org-scoped incident summary for
// one service: count, a severity histogram, and the most-recent incidents
// (newest first). It scans at most serviceIncidentScanLimit records and keeps
// only those inside the serviceIncidentWindowDays window. A nil store or read
// error degrades to an empty summary rather than failing the whole detail call.
func (a *AgentController) serviceIncidentSummary(name, org string) fiber.Map {
	summary := fiber.Map{
		"window_days": serviceIncidentWindowDays,
		"count":       0,
		"severities":  fiber.Map{},
		"recent":      []fiber.Map{},
	}
	store := services.Storage()
	if store == nil {
		return summary
	}
	recs, err := store.ListIncidents(serviceIncidentScanLimit)
	if err != nil {
		return summary
	}

	cutoff := time.Now().UTC().Add(-serviceIncidentWindowDays * 24 * time.Hour)
	severities := fiber.Map{}
	recent := make([]fiber.Map, 0, serviceRecentIncidentMax)
	count := 0
	for _, rec := range recs {
		if rec.Service != name || storage.NormalizeOrgID(rec.OrgID) != org {
			continue
		}
		if rec.CreatedAt.Before(cutoff) {
			continue
		}
		count++
		sev := incidentSeverity(rec)
		if n, ok := severities[sev].(int); ok {
			severities[sev] = n + 1
		} else {
			severities[sev] = 1
		}
		if len(recent) < serviceRecentIncidentMax {
			recent = append(recent, fiber.Map{
				"id":         rec.ID,
				"title":      rec.Title,
				"severity":   sev,
				"created_at": rec.CreatedAt,
			})
		}
	}

	summary["count"] = count
	summary["severities"] = severities
	summary["recent"] = recent
	return summary
}

// incidentSeverity reads the best-effort severity carried in the alert
// payload, falling back to "unknown" (mirrors snapshotFromIncident's lookup).
func incidentSeverity(rec *storage.IncidentRecord) string {
	if rec.Content != nil {
		if v, ok := rec.Content["severity"]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return "unknown"
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

// maxServiceNameLen bounds a manual service name so a runaway request can never
// grow the catalog blob without limit.
const maxServiceNameLen = 256

// createServiceRequest is the POST /services body.
type createServiceRequest struct {
	Name string `json:"name"`
}

// createService records an operator-created (manual) service so it is
// selectable as an override target before any signal is attributed to it. It
// is per-org scoped to the OSS catalog's single-tenant org (like
// getServiceDetail). A duplicate name (auto-discovered or manual) is a 409.
func (a *AgentController) createService(c *fiber.Ctx) error {
	if a.catalog == nil {
		middleware.RecordAdminAudit(c, auditActionServiceCreated, "create service", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "agent disabled"})
	}
	var req createServiceRequest
	if err := c.BodyParser(&req); err != nil {
		middleware.RecordAdminAudit(c, auditActionServiceCreated, "create service", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		middleware.RecordAdminAudit(c, auditActionServiceCreated, "create service", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	if len(name) > maxServiceNameLen {
		middleware.RecordAdminAudit(c, auditActionServiceCreated, "create service", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name too long"})
	}
	if _, exists := a.catalog.Service(name); exists {
		middleware.RecordAdminAudit(c, auditActionServiceCreated, name, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "service already exists"})
	}
	if err := a.catalog.CreateService(name); err != nil {
		middleware.RecordAdminAudit(c, auditActionServiceCreated, name, middleware.AdminAuditDenied)
		if err == agent.ErrServiceExists {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "service already exists"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	middleware.RecordAdminAudit(c, auditActionServiceCreated, name, middleware.AdminAuditSuccess)
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"service": name, "manual": true})
}

// renameServiceRequest is the PUT /services/:name body.
type renameServiceRequest struct {
	Name string `json:"name"`
}

// renameService renames a manual service. Auto-discovered services cannot be
// renamed (400) — their name is derived from live signals. The target name
// must be free (409). Override rules that targeted the old name are repointed
// so none dangle.
func (a *AgentController) renameService(c *fiber.Ctx) error {
	if a.catalog == nil {
		middleware.RecordAdminAudit(c, auditActionServiceRenamed, "rename service", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "agent disabled"})
	}
	oldName := c.Params("name")
	var req renameServiceRequest
	if err := c.BodyParser(&req); err != nil {
		middleware.RecordAdminAudit(c, auditActionServiceRenamed, oldName, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	newName := strings.TrimSpace(req.Name)
	if newName == "" {
		middleware.RecordAdminAudit(c, auditActionServiceRenamed, oldName, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	if len(newName) > maxServiceNameLen {
		middleware.RecordAdminAudit(c, auditActionServiceRenamed, oldName, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name too long"})
	}
	target := oldName + " -> " + newName
	info, exists := a.catalog.Service(oldName)
	if !exists {
		middleware.RecordAdminAudit(c, auditActionServiceRenamed, target, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "service not found"})
	}
	if !info.Manual {
		middleware.RecordAdminAudit(c, auditActionServiceRenamed, target, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "only manually-created services can be renamed"})
	}
	if newName == oldName {
		middleware.RecordAdminAudit(c, auditActionServiceRenamed, target, middleware.AdminAuditSuccess)
		return c.JSON(fiber.Map{"service": newName, "manual": true, "overrides_repointed": 0})
	}
	if _, taken := a.catalog.Service(newName); taken {
		middleware.RecordAdminAudit(c, auditActionServiceRenamed, target, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "target service already exists"})
	}
	if err := a.catalog.RenameService(oldName, newName); err != nil {
		middleware.RecordAdminAudit(c, auditActionServiceRenamed, target, middleware.AdminAuditDenied)
		switch err {
		case agent.ErrServiceNotFound:
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "service not found"})
		case agent.ErrServiceExists:
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "target service already exists"})
		default:
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
	}
	repointed := 0
	if a.overrides != nil {
		n, err := a.overrides.RepointService(storage.DefaultOrgID, oldName, newName)
		if err != nil {
			middleware.RecordAdminAudit(c, auditActionServiceRenamed, target, middleware.AdminAuditDenied)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		repointed = n
	}
	middleware.RecordAdminAudit(c, auditActionServiceRenamed, target, middleware.AdminAuditSuccess)
	return c.JSON(fiber.Map{"service": newName, "manual": true, "overrides_repointed": repointed})
}

// deleteService removes a manual service. Auto-discovered services cannot be
// deleted (400) — they re-appear on the next signal. Deletion is BLOCKED (409)
// while any override rule still targets the service, so a delete never orphans
// a correction: the operator must reassign/remove those overrides first.
func (a *AgentController) deleteService(c *fiber.Ctx) error {
	if a.catalog == nil {
		middleware.RecordAdminAudit(c, auditActionServiceDeleted, "delete service", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "agent disabled"})
	}
	name := c.Params("name")
	info, exists := a.catalog.Service(name)
	if !exists {
		middleware.RecordAdminAudit(c, auditActionServiceDeleted, name, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "service not found"})
	}
	if !info.Manual {
		middleware.RecordAdminAudit(c, auditActionServiceDeleted, name, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "only manually-created services can be deleted"})
	}
	if a.overrides != nil {
		if n := a.overrides.CountForService(storage.DefaultOrgID, name); n > 0 {
			middleware.RecordAdminAudit(c, auditActionServiceDeleted, name, middleware.AdminAuditDenied)
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error":     "service has override rules that target it; remove them first",
				"overrides": n,
			})
		}
	}
	if !a.catalog.DeleteService(name) {
		middleware.RecordAdminAudit(c, auditActionServiceDeleted, name, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "service not found"})
	}
	middleware.RecordAdminAudit(c, auditActionServiceDeleted, name, middleware.AdminAuditSuccess)
	return c.SendStatus(fiber.StatusNoContent)
}

// listServiceOverrides returns every manual-attribution override rule for the
// OSS catalog's single-tenant org.
func (a *AgentController) listServiceOverrides(c *fiber.Ctx) error {
	if a.overrides == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "service overrides not enabled"})
	}
	return c.JSON(fiber.Map{"overrides": a.overrides.List(storage.DefaultOrgID)})
}

// createOverrideRequest is the POST /service-overrides body.
type createOverrideRequest struct {
	SourceType string `json:"source_type"`
	Match      string `json:"match"`
	Service    string `json:"service"`
}

// createServiceOverride creates (or replaces the same-key) override rule. The
// target service must already exist in the catalog (create it inline first) so
// a rule can never point at a typo/orphan. Metric/trace rules are accepted in
// any build but only take effect where the enterprise metric/trace brains run;
// in an OSS build they persist inert (there is no metric/trace signal to
// re-label), matching "inert for metrics/traces".
func (a *AgentController) createServiceOverride(c *fiber.Ctx) error {
	if a.overrides == nil {
		middleware.RecordAdminAudit(c, auditActionOverrideSet, "set override", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "service overrides not enabled"})
	}
	var req createOverrideRequest
	if err := c.BodyParser(&req); err != nil {
		middleware.RecordAdminAudit(c, auditActionOverrideSet, "set override", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	service := strings.TrimSpace(req.Service)
	if a.catalog != nil {
		if _, exists := a.catalog.Service(service); !exists {
			middleware.RecordAdminAudit(c, auditActionOverrideSet, "service "+service, middleware.AdminAuditDenied)
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unknown target service; create it first"})
		}
	}
	rule, err := a.overrides.Put(storage.DefaultOrgID, agent.OverrideRule{
		SourceType: req.SourceType,
		Match:      req.Match,
		Service:    service,
	})
	if err != nil {
		middleware.RecordAdminAudit(c, auditActionOverrideSet, "service "+service, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	middleware.RecordAdminAudit(c, auditActionOverrideSet, rule.ID+" -> "+service, middleware.AdminAuditSuccess)
	return c.Status(fiber.StatusCreated).JSON(rule)
}

// deleteServiceOverride removes one override rule by id.
func (a *AgentController) deleteServiceOverride(c *fiber.Ctx) error {
	if a.overrides == nil {
		middleware.RecordAdminAudit(c, auditActionOverrideDeleted, "delete override", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "service overrides not enabled"})
	}
	id := c.Params("id")
	ok, err := a.overrides.Delete(storage.DefaultOrgID, id)
	if err != nil {
		middleware.RecordAdminAudit(c, auditActionOverrideDeleted, id, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if !ok {
		middleware.RecordAdminAudit(c, auditActionOverrideDeleted, id, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	middleware.RecordAdminAudit(c, auditActionOverrideDeleted, id, middleware.AdminAuditSuccess)
	return c.SendStatus(fiber.StatusNoContent)
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
		middleware.RecordAdminAudit(c, auditActionDetectCleared, "detect log", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "detect log not enabled"})
	}
	n := a.detect.Clear()
	if err := a.detect.Persist(); err != nil {
		middleware.RecordAdminAudit(c, auditActionDetectCleared, "detect log", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	middleware.RecordAdminAudit(c, auditActionDetectCleared, fmt.Sprintf("detect log (cleared=%d)", n), middleware.AdminAuditSuccess)
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

// getSystemPrompt returns the assembled system prompt sent to the
// model. Defaults to the detect prompt; pass ?kind=analyze for the
// analyze agent's prompt. Detect/analyze log events store only the
// user prompt to keep the on-disk log small; this endpoint provides
// the constant half.
func (a *AgentController) getSystemPrompt(c *fiber.Ctx) error {
	kind := c.Query("kind", "detect")
	switch kind {
	case "detect":
		return c.JSON(fiber.Map{
			"kind":          "detect",
			"system_prompt": detect.SystemPrompt(),
			"sources":       detect.PromptOrder(),
		})
	case "analyze":
		return c.JSON(fiber.Map{
			"kind":          "analyze",
			"system_prompt": analyze.SystemPrompt(),
			"sources":       analyze.PromptOrder(),
		})
	default:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "unknown kind; expected 'detect' or 'analyze'",
		})
	}
}
