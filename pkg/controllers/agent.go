package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/analyze"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
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
	// grace-control + the two flush routes are state-changing admin actions:
	// grace-control ends/restarts a service's new-service grace
	// window; the flushes force-persist the in-memory shadow/detect log to
	// disk. They are gated + audited to parity with the rest of the mutating
	// agent surface.
	auditActionServiceGraceControlled = "agent.service.grace_controlled"
	auditActionShadowFlushed          = "agent.shadow.flushed"
	auditActionDetectFlushed          = "agent.detect.flushed"
)

// AgentController exposes admin endpoints for inspecting and curating the
// pattern catalog. All endpoints require the gateway secret configured under
// `agent.gateway_secret` (or env AGENT_GATEWAY_SECRET), sent in the
// `X-Gateway-Secret` header. When no secret is configured, every request is
// rejected — this is by design: an empty secret must not silently grant access.
type AgentController struct {
	catalog         *agent.Catalog
	miner           *agent.Miner
	cursors         *agent.CursorStore
	sources         []core.SignalSource
	shadow          *agent.ShadowLog
	detect          *agent.DetectLog
	overrides       *agent.ServiceOverrideStore
	runbooksEnabled bool

	// catalogCfg + pollInterval let listPatterns compute per-pattern readiness
	// (core.Readiness) without reaching into package globals. Wired at boot via
	// SetCatalogConfig from AgentConfig.Catalog + AgentConfig.PollInterval. Left
	// unset (zero value), readiness degrades to Needed=0/RatePerMin=0 — the row
	// still carries the field, just as "Learning" with no ETA (safe).
	catalogCfg   config.AgentCatalogConfig
	pollInterval time.Duration
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

// SetCursorStore wires the worker's poll-cursor store into the controller so
// that clearing the pattern catalog also rewinds every source cursor. This
// makes the SAME running worker re-read its lookback window and relearn
// immediately after a clear — the in-place equivalent of a fresh process
// start. It MUST be the exact CursorStore the worker mines through (shared
// pointer), or the rewind won't reach the worker's learn loop. Optional: when
// left unset (e.g. the agent runs no worker in this process), clearPatterns
// leaves cursors untouched. Returns the receiver for fluent wiring.
func (a *AgentController) SetCursorStore(cs *agent.CursorStore) *AgentController {
	a.cursors = cs
	return a
}

// SetSources wires the worker's live signal sources into the controller so that
// clearing the pattern catalog can also rewind the read position of any source
// that keeps its OWN (a core.SourceRewinder — e.g. the file source's byte
// offset). Rewinding the poll cursor (SetCursorStore) only re-reads
// cursor-driven backends (Elasticsearch, Loki, …); a file source ignores the
// poll cursor, so without this it stays pinned at EOF after a clear and the
// SAME running worker never re-emits its backlog — the "clear stops learning
// until the container is recreated" halt. MUST be the exact source slice the
// worker polls (shared pointers). Optional and nil-safe: sources without their
// own position simply never implement SourceRewinder. Returns the receiver for
// fluent wiring.
func (a *AgentController) SetSources(sources []core.SignalSource) *AgentController {
	a.sources = sources
	return a
}

// SetCatalogConfig wires the catalog threshold + worker poll interval into the
// controller so listPatterns can attach a computed core.Readiness to each
// pattern row WITHOUT reaching into package globals. `cat.AutoPromoteAfter`
// supplies the readiness `Needed` gate (≤0 → indeterminate/manual-only) and
// `poll` converts each pattern's per-tick sighting EWMA into a per-minute
// arrival rate for the ETA. Optional and safe to omit: left unset the readiness
// degrades to Needed=0/RatePerMin=0 (renders as "Learning", no ETA), so a
// process that runs no worker still serves well-formed rows. Returns the
// receiver for fluent wiring.
func (a *AgentController) SetCatalogConfig(cat config.AgentCatalogConfig, poll time.Duration) *AgentController {
	a.catalogCfg = cat
	a.pollInterval = poll
	return a
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
	all := a.catalog.All()
	rows := make([]patternRow, 0, len(all))
	for _, p := range all {
		// Strip the redacted sample ring from list rows: the list is
		// potentially thousands of patterns and the ring belongs only on the
		// detail read (getPattern). a.catalog.All() already returns copies, so
		// nil-ing this field never touches the stored pattern.
		p.Samples = nil
		rows = append(rows, patternRow{
			Pattern:   p,
			Readiness: agent.LogReadiness(p, a.catalogCfg.AutoPromoteAfter, a.pollInterval),
		})
	}
	return c.JSON(fiber.Map{"patterns": rows})
}

// patternRow is the GET /api/agent/patterns response DTO. It embeds the on-disk
// *agent.Pattern (so every existing field marshals byte-identically) and
// attaches a computed core.Readiness. Readiness is computed at the read
// boundary, never persisted — the Pattern on-disk schema stays untouched. The
// embedded pointer promotes the Pattern JSON fields to the top level, so
// clients see the same object plus an additive `readiness` object.
type patternRow struct {
	*agent.Pattern
	Readiness core.Readiness `json:"readiness"`
}

func (a *AgentController) getPattern(c *fiber.Ctx) error {
	id := c.Params("id")
	p := a.catalog.Get(id)
	if p == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	return c.JSON(p)
}

// updatePatternRequest is the POST /patterns/:id body. Verdict is a pointer so
// the controller can distinguish three intents the UI sends:
//   - key absent ({"tags":[...]})   → Verdict nil   → leave the verdict alone
//   - {"verdict":""}                → Verdict &""   → CLEAR the verdict
//   - {"verdict":"known"}           → Verdict &"..." → SET the verdict
//
// A plain string would collapse the first two into "" and make "Clear verdict"
// a silent no-op; the pointer preserves the distinction end to end.
type updatePatternRequest struct {
	Verdict *string  `json:"verdict"`
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
// re-discovered as new rather than resumed against pre-reset templates, AND
// rewinds every poll cursor (when a CursorStore is wired) so the SAME running
// worker re-reads its lookback window and relearns immediately — the in-place
// equivalent of a fresh process start. For a source that keeps its OWN read
// position independent of the poll cursor (a core.SourceRewinder — e.g. the
// file source's byte offset), it ALSO rewinds that position, because such a
// source ignores the poll cursor and would otherwise stay pinned past the
// already-consumed data and never re-emit. Without BOTH rewinds the worker
// appears to halt after a clear until the container is recreated (only recreate
// reconstructs the source from scratch). Admin-gated by authMiddleware like
// every other destructive agent mutation; returns a count of the patterns
// cleared.
func (a *AgentController) clearPatterns(c *fiber.Ctx) error {
	// Rewind the poll cursors FIRST so a cursor-store failure aborts before we
	// wipe the catalog, leaving the system consistent. If this succeeds but the
	// catalog reset below fails, the worker simply re-reads the window and
	// relearns the still-present patterns (idempotent), which is harmless.
	if a.cursors != nil {
		if err := a.cursors.Reset(c.Context()); err != nil {
			middleware.RecordAdminAudit(c, auditActionPatternsCleared, "all patterns", middleware.AdminAuditDenied)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
	}
	// Reconcile the second cursor of truth: rewind the internal read position of
	// any source that keeps its own (the file source's byte offset), so it
	// re-reads its window in place. A rewind failure aborts before the catalog
	// wipe for the same consistency reason as the cursor reset above.
	if err := a.rewindOwnPositionSources(c.Context()); err != nil {
		middleware.RecordAdminAudit(c, auditActionPatternsCleared, "all patterns", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
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

// rewindOwnPositionSources rewinds the internal read position of every wired
// source that keeps its own (implements core.SourceRewinder). It is the second
// half of the clear rewind: the CursorStore reset handles cursor-driven
// backends, this handles sources like the file source that ignore the poll
// cursor and track a byte offset instead. Returns the first rewind error so the
// caller can abort the clear before wiping the catalog, keeping the system
// consistent. A nil/empty source slice (e.g. no worker in this process) is a
// no-op, as are sources that do not implement SourceRewinder.
func (a *AgentController) rewindOwnPositionSources(ctx context.Context) error {
	for _, src := range a.sources {
		r, ok := src.(core.SourceRewinder)
		if !ok {
			continue
		}
		if err := r.Rewind(ctx); err != nil {
			return fmt.Errorf("rewind source %s: %w", src.Name(), err)
		}
	}
	return nil
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
		middleware.RecordAdminAudit(c, auditActionShadowFlushed, "shadow log", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "shadow log not enabled"})
	}
	if err := a.shadow.Persist(); err != nil {
		middleware.RecordAdminAudit(c, auditActionShadowFlushed, "shadow log", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	middleware.RecordAdminAudit(c, auditActionShadowFlushed, fmt.Sprintf("shadow log (events=%d)", a.shadow.Len()), middleware.AdminAuditSuccess)
	return c.JSON(fiber.Map{"ok": true, "events": a.shadow.Len()})
}

// listServices returns every known service with its first-seen timestamp AND
// its new-service grace status. The grace fields are computed by the SAME
// graceStatus helper getServiceDetail uses, so the "in grace" status the
// Services LIST shows and the status the service DETAIL page shows are one
// computation and can never disagree. (Previously the list returned only
// first_seen/manual and the UI hard-coded every row as "tracked", so a service
// still inside its grace window read as "tracked" in the list but "in grace" on
// its detail page.)
func (a *AgentController) listServices(c *fiber.Ctx) error {
	grace := serviceGraceDuration()
	all := a.catalog.AllServices()
	out := make(map[string]fiber.Map, len(all))
	for name, info := range all {
		inGrace, remaining := graceStatus(info.FirstSeen, grace)
		m := fiber.Map{
			"first_seen":              info.FirstSeen,
			"manual":                  info.Manual,
			"in_grace":                inGrace,
			"grace_seconds_remaining": remaining,
		}
		// Preserve the org_id field exactly as ServiceInfo serialized it
		// (omitempty) so no existing consumer of this shape regresses.
		if info.OrgID != "" {
			m["org_id"] = info.OrgID
		}
		out[name] = m
	}
	return c.JSON(fiber.Map{"services": out})
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
// This is the OSS half of the service-detail surface — logs (patterns) and
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

	inGrace, graceRemaining := graceStatus(info.FirstSeen, serviceGraceDuration())

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
//
// The config read is guarded through GetConfigOrNil (never GetConfig, which
// panics): this read-only status helper can run before config.Load in a test
// or an early request, and an unloaded config simply means grace is not
// configured. In that case we degrade to 0 (grace disabled → in_grace=false,
// no remaining) instead of dereferencing a nil config. When config IS loaded
// the behavior is unchanged.
func serviceGraceDuration() time.Duration {
	cfg := config.GetConfigOrNil()
	if cfg == nil {
		return 0
	}
	d, err := time.ParseDuration(cfg.Agent.NewServiceGrace)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// graceStatus reports whether a service first seen at firstSeen is still inside
// its new-service grace window and, when it is, how many whole seconds remain.
// It is the SINGLE grace computation shared by the service LIST (listServices)
// and the service DETAIL (getServiceDetail) endpoints, so the "in grace" status
// they report is derived identically and can never drift. A zero grace duration
// (unset/disabled) is never in grace; a service pushed out of grace (FirstSeen
// zeroed by EndServiceGrace) reads as not in grace.
func graceStatus(firstSeen time.Time, grace time.Duration) (bool, int) {
	if grace <= 0 {
		return false, 0
	}
	if rem := time.Until(firstSeen.Add(grace)); rem > 0 {
		return true, int(rem.Seconds())
	}
	return false, 0
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
		middleware.RecordAdminAudit(c, auditActionServiceGraceControlled, name, middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
	}
	// Non-secret target: the service name + the requested action. Only the
	// two allowed actions are ever reflected verbatim; a rejected client value
	// is bounded + sanitized (see the default branch) so an arbitrary/oversized
	// string can never land in the audit trail unbounded (log-injection guard).
	var target string
	switch req.Action {
	case "end":
		target = fmt.Sprintf("%s (action=end)", name)
		if !a.catalog.EndServiceGrace(name) {
			middleware.RecordAdminAudit(c, auditActionServiceGraceControlled, target, middleware.AdminAuditDenied)
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "service not found"})
		}
	case "restart":
		target = fmt.Sprintf("%s (action=restart)", name)
		if !a.catalog.RestartServiceGrace(name) {
			middleware.RecordAdminAudit(c, auditActionServiceGraceControlled, target, middleware.AdminAuditDenied)
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "service not found"})
		}
	default:
		// Reflect only a bounded, control-char-stripped view of the rejected
		// action — never the raw client string.
		middleware.RecordAdminAudit(c, auditActionServiceGraceControlled,
			fmt.Sprintf("%s (action=invalid:%s)", name, sanitizeAuditToken(req.Action)),
			middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "action must be \"end\" or \"restart\""})
	}
	middleware.RecordAdminAudit(c, auditActionServiceGraceControlled, target, middleware.AdminAuditSuccess)
	return c.JSON(fiber.Map{"ok": true})
}

// maxServiceNameLen bounds a manual service name so a runaway request can never
// grow the catalog blob without limit.
const maxServiceNameLen = 256

// maxAuditActionLen bounds how much of a rejected, client-supplied action
// string is reflected into an audit target, so an arbitrary/oversized value can
// never land in the trail verbatim.
const maxAuditActionLen = 32

// sanitizeAuditToken bounds and cleans a client-supplied string before it is
// reflected into an audit target: control/newline characters are dropped (so a
// value cannot inject fake trail lines) and the result is capped to
// maxAuditActionLen bytes. Used only on rejection paths that want to preserve a
// little diagnostic value without echoing raw client input.
func sanitizeAuditToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxAuditActionLen {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

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
	// On the denial branches below the client `service` is only trimmed, never
	// clamped to maxServiceNameLen (on the unknown-target branch it definitionally
	// failed the catalog existence check), so it is bounded + control-char-stripped
	// via sanitizeAuditToken before it reaches the audit target — an arbitrary
	// client string can never land in the trail verbatim/unbounded (log-injection
	// guard). The success target below (rule.ID+" -> "+service) is safe when a
	// catalog is present: the existence check guarantees service ≤ maxServiceNameLen.
	if a.catalog != nil {
		if _, exists := a.catalog.Service(service); !exists {
			middleware.RecordAdminAudit(c, auditActionOverrideSet, "service "+sanitizeAuditToken(service), middleware.AdminAuditDenied)
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unknown target service; create it first"})
		}
	}
	rule, err := a.overrides.Put(storage.DefaultOrgID, agent.OverrideRule{
		SourceType: req.SourceType,
		Match:      req.Match,
		Service:    service,
	})
	if err != nil {
		middleware.RecordAdminAudit(c, auditActionOverrideSet, "service "+sanitizeAuditToken(service), middleware.AdminAuditDenied)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	middleware.RecordAdminAudit(c, auditActionOverrideSet, rule.ID+" -> "+service, middleware.AdminAuditSuccess)
	// Retroactive re-point: make the reassignment take effect IMMEDIATELY, not
	// only when a fresh matching log line re-clusters the pattern. For a log
	// override the UI sends the mined pattern id as the match key, so re-point
	// that existing pattern's Service now — GET /api/agent/patterns reflects it
	// on the very next read. When the match is a message substring (no pattern
	// has that id) RepointService is a no-op and the existing lazy
	// re-observation path (brain_log.Learn → Catalog.Upsert) still applies the
	// override on the next matching tick. Metric/trace overrides target the
	// enterprise metric/trace baselines, not the log-pattern catalog, so they
	// keep their lazy behaviour here; the audit row above already records the
	// override itself, so this side effect does not re-audit.
	if a.catalog != nil && rule.SourceType == agent.OverrideSourceLog {
		a.catalog.RepointService(rule.Match, service)
	}
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
		middleware.RecordAdminAudit(c, auditActionDetectFlushed, "detect log", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "detect log not enabled"})
	}
	if err := a.detect.Persist(); err != nil {
		middleware.RecordAdminAudit(c, auditActionDetectFlushed, "detect log", middleware.AdminAuditDenied)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	middleware.RecordAdminAudit(c, auditActionDetectFlushed, fmt.Sprintf("detect log (events=%d)", a.detect.Len()), middleware.AdminAuditSuccess)
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
