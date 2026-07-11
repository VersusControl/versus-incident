package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
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
//	GET  /api/admin/incidents                 list (newest first; ?limit=NN)
//	GET  /api/admin/incidents/search          full-text search (?q=&limit=NN)
//	GET  /api/admin/incidents/intake-settings  read intake settings
//	PUT  /api/admin/incidents/intake-settings  update intake settings
//	GET  /api/admin/incidents/:id             single record
//	POST /api/admin/incidents/:id/resolve     mark resolved (idempotent)
func (i *IncidentAdminController) Register(router fiber.Router) {
	// Capabilities probe — lets the UI enable/disable search depending on
	// whether the active storage backend implements storage.Searcher.
	router.Group("/admin/capabilities", i.authMiddleware).Get("/", i.capabilities)

	g := router.Group("/admin/incidents", i.authMiddleware)
	g.Get("/", i.list)
	// /search MUST be registered before /:id so the literal path is not
	// swallowed by the :id parameter route.
	g.Get("/search", i.search)
	// /intake-settings likewise MUST precede /:id so the literal settings
	// path is not captured as an incident id.
	g.Get("/intake-settings", i.getIntakeSettings)
	g.Put("/intake-settings", i.putIntakeSettings)
	g.Get("/:id", i.get)
	g.Post("/:id/resolve", i.resolve)
	g.Post("/:id/analyze", i.analyze)
	g.Get("/:id/analyses", i.listAnalyses)

	a := router.Group("/admin/analyses", i.authMiddleware)
	a.Get("/", i.listAllAnalyses)
	a.Get("/:analysis_id", i.getAnalysis)
	a.Delete("/:analysis_id", i.deleteAnalysis)
}

// authMiddleware reuses the agent gateway secret. Keeping the same
// header name (X-Gateway-Secret) means the UI only manages one secret.
// Comparison is constant-time (see secureEqual in agent.go) to avoid
// header-length / prefix-match timing oracles.
func (i *IncidentAdminController) authMiddleware(c *fiber.Ctx) error {
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

func (i *IncidentAdminController) list(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.JSON(fiber.Map{"incidents": []any{}, "counts": originCounts(nil)})
	}
	limit := 0
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	// Pull the full window (limit 0) so per-origin counts are computed
	// over every record and the origin filter is applied here, not at the
	// storage layer. The list backends already hold the capped history in
	// memory, so this is the same read the UI has always issued.
	recs, err := store.ListIncidents(0)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(incidentListResponse(recs, c.Query("origin"), c.Query("page"), c.Query("page_size"), limit))
}

// capabilities reports which optional storage features the running
// backend supports, so the UI can hide controls that would 501. Today the
// only flag is full-text search (storage.Searcher), implemented by the
// Postgres backend; memory/file backends report false.
func (i *IncidentAdminController) capabilities(c *fiber.Ctx) error {
	store := services.Storage()
	_, searchable := store.(storage.Searcher)
	cfg := config.GetConfig()
	settings := services.LoadReportSettings(store)
	return c.JSON(fiber.Map{
		"search": searchable,
		// report tells the UI whether to show the incidents-analytics Reports
		// action, the default channel/window, and which enabled channels to
		// offer — so it never guesses. Sourced from the runtime settings store
		// (no YAML block anymore). public_host_set drives whether URL-capable
		// channel fallbacks can carry a link.
		"report": fiber.Map{
			"enable":          settings.Enable,
			"default_channel": settings.DefaultChannel,
			"default_window":  settings.DefaultWindow,
			"include_chart":   settings.IncludeChart,
			"channels":        enabledAlertChannels(cfg),
			"public_host_set": strings.TrimSpace(cfg.PublicHost) != "",
		},
	})
}

// enabledAlertChannels lists the notification channels currently enabled in
// config, in a stable order, for the report channel picker. Returns an empty
// (non-nil) slice so it serializes as [] not null.
func enabledAlertChannels(cfg *config.Config) []string {
	out := []string{}
	if cfg.Alert.Slack.Enable {
		out = append(out, "slack")
	}
	if cfg.Alert.Telegram.Enable {
		out = append(out, "telegram")
	}
	if cfg.Alert.Viber.Enable {
		out = append(out, "viber")
	}
	if cfg.Alert.Email.Enable {
		out = append(out, "email")
	}
	if cfg.Alert.MSTeams.Enable {
		out = append(out, "msteams")
	}
	if cfg.Alert.Lark.Enable {
		out = append(out, "lark")
	}
	return out
}

// search runs server-side full-text search over stored incidents using
// the optional storage.Searcher capability. Backends that do not
// implement it (memory, file) return 501 so the UI can fall back to its
// in-page client-side filter.
func (i *IncidentAdminController) search(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.JSON(fiber.Map{"incidents": []any{}})
	}
	searcher, ok := store.(storage.Searcher)
	if !ok {
		return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{
			"error":  "search not supported by the configured storage backend",
			"search": false,
		})
	}
	limit := 0
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	recs, err := searcher.SearchIncidents(c.Query("q"), 0)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(incidentListResponse(recs, c.Query("origin"), c.Query("page"), c.Query("page_size"), limit))
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
		"origin":              r.EffectiveOrigin(),
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

// originCounts tallies incidents per coarse origin over the full result
// set. It is computed BEFORE any origin filter so the UI top-bar can show
// "AI: N · Webhook: M" regardless of which tab is active. Legacy records
// without an explicit Origin are classified from their Source via
// EffectiveOrigin so they are never dropped into an empty bucket.
func originCounts(recs []*storage.IncidentRecord) fiber.Map {
	var ai, webhook int
	for _, r := range recs {
		switch r.EffectiveOrigin() {
		case storage.OriginAIDetect:
			ai++
		default:
			webhook++
		}
	}
	return fiber.Map{
		"ai_detect": ai,
		"webhook":   webhook,
		"total":     len(recs),
	}
}

// filterByOrigin keeps only records whose EffectiveOrigin matches origin.
// An empty or unrecognized origin returns the input unchanged (all
// origins), so existing callers that pass no origin are unaffected. The
// result is a fresh slice — the input is never aliased or mutated.
func filterByOrigin(recs []*storage.IncidentRecord, origin string) []*storage.IncidentRecord {
	if origin != storage.OriginAIDetect && origin != storage.OriginWebhook {
		return recs
	}
	out := make([]*storage.IncidentRecord, 0, len(recs))
	for _, r := range recs {
		if r.EffectiveOrigin() == origin {
			out = append(out, r)
		}
	}
	return out
}

// defaultIncidentPageSize mirrors the UI's PAGE_SIZE (100): the incidents
// list paginates in 100-row windows so a 10k+ webhook history never ships
// to the browser in one response.
const defaultIncidentPageSize = 100

// incidentListResponse is the shared post-processing for the list and
// search endpoints. It computes per-origin counts over the FULL result
// set (so the top-bar shows both feeds regardless of the active tab),
// applies the optional origin filter, then paginates. pageParam /
// pageSizeParam are the raw query strings; when pageParam is empty the
// endpoint returns the full origin-filtered window capped at limit — the
// back-compat shape existing callers depend on.
func incidentListResponse(recs []*storage.IncidentRecord, origin, pageParam, pageSizeParam string, limit int) fiber.Map {
	counts := originCounts(recs)
	recs = filterByOrigin(recs, origin)
	total := len(recs)

	resp := fiber.Map{"counts": counts, "total": total}

	if pageParam != "" {
		page := 1
		if n, err := strconv.Atoi(pageParam); err == nil && n > 1 {
			page = n
		}
		size := defaultIncidentPageSize
		if n, err := strconv.Atoi(pageSizeParam); err == nil && n > 0 {
			size = n
		}
		start := (page - 1) * size
		if start > total {
			start = total
		}
		end := start + size
		if end > total {
			end = total
		}
		recs = recs[start:end]
		resp["page"] = page
		resp["page_size"] = size
	} else if limit > 0 && len(recs) > limit {
		recs = recs[:limit]
	}

	// Strip the (potentially large) Content blob from list responses; the
	// UI fetches the detail endpoint to see it.
	out := make([]fiber.Map, 0, len(recs))
	for _, r := range recs {
		out = append(out, summarize(r))
	}
	resp["incidents"] = out
	return resp
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

// getIntakeSettings returns the current runtime intake settings (or the
// built-in defaults — auto-resolve ON — when none are stored). Same
// X-Gateway-Secret guard as the other admin settings routes (the group's
// authMiddleware).
func (i *IncidentAdminController) getIntakeSettings(c *fiber.Ctx) error {
	return c.JSON(services.LoadIntakeSettings(services.Storage()))
}

// putIntakeSettings persists updated runtime intake settings. The whole
// settings object is replaced (idempotent). 503 when no storage backend is
// configured, mirroring the report settings PUT.
func (i *IncidentAdminController) putIntakeSettings(c *fiber.Ctx) error {
	var s services.IntakeSettings
	if err := c.BodyParser(&s); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid settings body"})
	}
	if err := services.SaveIntakeSettings(services.Storage(), s); err != nil {
		if errors.Is(err, services.ErrIntakeNoStorage) {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	// Return the effective settings after the write.
	return c.JSON(services.LoadIntakeSettings(services.Storage()))
}

// ---------------------------------------------------------------------------
// Analyze
// ---------------------------------------------------------------------------

// analyzeRequest is the optional body for POST /:id/analyze. Empty is
// fine — every field has a sensible default.
type analyzeRequest struct {
	RequestedBy string `json:"requested_by"`
}

// analyze runs the analyze-kind AI agent against one stored incident
// and persists the resulting AnalysisRecord. Returns 503 when either
// storage or the analyze agent is not configured.
func (i *IncidentAdminController) analyze(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
	}
	ag := services.AnalyzeAgent()
	if ag == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "analyze agent not enabled"})
	}

	rec, err := store.GetIncident(c.Params("id"))
	if errors.Is(err, storage.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	var body analyzeRequest
	// Body is optional; tolerate parse errors as "no body".
	_ = c.BodyParser(&body)

	snap := snapshotFromIncident(rec, body.RequestedBy)
	task := core.AnalyzeTask{Snapshot: snap}

	// Hard ceiling so a stuck tool loop cannot pin a request open
	// forever. The agent has its own iteration cap on top of this.
	ctx, cancel := context.WithTimeout(c.UserContext(), 2*time.Minute)
	defer cancel()

	startedAt := time.Now().UTC()
	result, runErr := ag.Run(ctx, task)

	analysis := &storage.AnalysisRecord{
		ID:          uuid.NewString(),
		OrgID:       rec.OrgID,
		IncidentID:  rec.ID,
		RequestedAt: startedAt,
		RequestedBy: body.RequestedBy,
		Status:      "ok",
	}
	if result != nil {
		analysis.DurationMs = result.DurationMs
		analysis.Model = result.Model
		analysis.RawResponse = result.RawResponse
		analysis.Finding = result.Finding
		analysis.ToolCalls = toolCallsFromCore(result.ToolCalls)
	}
	if runErr != nil {
		analysis.Status = "error"
		analysis.Error = runErr.Error()
	}

	if saveErr := store.SaveAnalysis(analysis); saveErr != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("save: %v", saveErr)})
	}

	status := fiber.StatusOK
	if runErr != nil {
		status = fiber.StatusBadGateway
	}
	return c.Status(status).JSON(analysis)
}

func (i *IncidentAdminController) listAnalyses(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.JSON(fiber.Map{"analyses": []any{}})
	}
	limit := 0
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	recs, err := store.ListAnalysesByIncident(c.Params("id"), limit)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"analyses": recs})
}

func (i *IncidentAdminController) listAllAnalyses(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.JSON(fiber.Map{"analyses": []any{}})
	}
	limit := 0
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	recs, err := store.ListAnalyses(limit)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"analyses": recs})
}

func (i *IncidentAdminController) getAnalysis(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
	}
	rec, err := store.GetAnalysis(c.Params("analysis_id"))
	if errors.Is(err, storage.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(rec)
}

func (i *IncidentAdminController) deleteAnalysis(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
	}
	if err := store.DeleteAnalysis(c.Params("analysis_id")); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// snapshotFromIncident flattens a stored IncidentRecord into the
// analyze agent's input contract. Severity is best-effort: pulled from
// the alert payload when present.
func snapshotFromIncident(rec *storage.IncidentRecord, requestedBy string) core.AnalyzeIncidentSnapshot {
	severity := ""
	if rec.Content != nil {
		if v, ok := rec.Content["severity"]; ok {
			if s, ok := v.(string); ok {
				severity = s
			}
		}
	}
	return core.AnalyzeIncidentSnapshot{
		IncidentID:  rec.ID,
		Title:       rec.Title,
		Service:     rec.Service,
		Source:      rec.Source,
		Severity:    severity,
		Resolved:    rec.Resolved,
		CreatedAt:   rec.CreatedAt,
		AckedAt:     rec.AckedAt,
		ResolvedAt:  rec.ResolvedAt,
		Content:     rec.Content,
		RequestedBy: requestedBy,
	}
}

func toolCallsFromCore(traces []core.ToolCallTrace) []storage.AnalysisToolCall {
	if len(traces) == 0 {
		return nil
	}
	out := make([]storage.AnalysisToolCall, 0, len(traces))
	for _, t := range traces {
		out = append(out, storage.AnalysisToolCall{
			Name:       t.Name,
			Args:       []byte(t.Args),
			Output:     []byte(t.Output),
			DurationMs: t.DurationMs,
			Error:      t.Error,
		})
	}
	return out
}
