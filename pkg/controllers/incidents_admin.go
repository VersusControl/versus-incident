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
//	GET  /api/admin/incidents/counts          cheap per-origin × per-status tally
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
	// /counts is the cheap, rows-free per-origin × per-status tally the Now
	// page and header badge read; like /search it MUST precede /:id so the
	// literal path is not captured as an incident id.
	g.Get("/counts", i.counts)
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
		return c.JSON(fiber.Map{"incidents": []any{}, "counts": originCounts(nil), "total": 0})
	}
	origin := c.Query("origin")
	// Preferred path: the backend implements the pager capability, so we
	// render a page from ONE bounded query plus ONE cheap count query and
	// never load the whole table. Postgres (unbounded history) and the
	// file/memory backends (already capped) all implement it.
	if pager, ok := store.(storage.IncidentPager); ok {
		counts, err := pager.CountIncidentsByStatus()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		size := parsePageSize(c.Query("page_size"))
		offset, page := pageOffset(c.Query("page"), c.Query("offset"), size)
		recs, err := pager.ListIncidentsPage(origin, offset, size)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(pagedIncidentResponse(recs, counts, origin, offset, size, page))
	}
	// Fallback for backends without the pager capability (the redis stub):
	// the legacy full-window read + in-memory pagination. Correct but
	// unbounded — only reached by a backend that has no bounded read.
	recs, err := store.ListIncidents(0)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(incidentListResponse(recs, origin, c.Query("page"), c.Query("page_size"), parseLimit(c.Query("limit"))))
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
	query := c.Query("q")
	origin := c.Query("origin")
	// Preferred path: the backend implements the search pager, so a broad
	// query against a large history returns the first page from one bounded
	// query plus one count query — never the whole match set. Postgres
	// implements it; it is the only unbounded Searcher.
	if sp, ok := store.(storage.IncidentSearchPager); ok {
		counts, err := sp.CountIncidentsMatchingByStatus(query)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		size := parsePageSize(c.Query("page_size"))
		offset, page := pageOffset(c.Query("page"), c.Query("offset"), size)
		recs, err := sp.SearchIncidentsPage(query, origin, offset, size)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(pagedIncidentResponse(recs, counts, origin, offset, size, page))
	}
	// Fallback: a Searcher without the pager capability. Bound the read to
	// one page so it can never load the whole match set; counts are computed
	// over that bounded page.
	recs, err := searcher.SearchIncidents(query, storage.DefaultIncidentPageSize)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(incidentListResponse(recs, origin, c.Query("page"), c.Query("page_size"), parseLimit(c.Query("limit"))))
}

// counts returns the whole-set per-origin × per-status incident tally in one
// cheap, rows-free response so the Now page and the header badge can show
// authoritative numbers WITHOUT loading a page of rows. Preferred path is the
// bounded pager's single COUNT query; the fallback (a backend with no pager)
// tallies a materialized window. The shape matches the list response's counts
// object (top-level unresolved + by_status), so both surfaces read one type.
func (i *IncidentAdminController) counts(c *fiber.Ctx) error {
	store := services.Storage()
	if store == nil {
		return c.JSON(countsMap(storage.IncidentStatusCounts{}))
	}
	if pager, ok := store.(storage.IncidentPager); ok {
		sc, err := pager.CountIncidentsByStatus()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(countsMap(sc))
	}
	recs, err := store.ListIncidents(0)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(countsMap(storage.StatusCountsOf(recs)))
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

// maxIncidentPageSize caps the caller-requested page_size so one request can
// never ask the backend for an unbounded window and reintroduce the
// load-everything problem the pager exists to fix.
const maxIncidentPageSize = 5000

// parseLimit parses a positive integer limit query param, returning 0 (no
// limit) for a missing, empty, non-numeric, or non-positive value.
func parseLimit(v string) int {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 0
}

// parseNonNegInt parses a non-negative integer query param (offset/cursor),
// returning 0 for anything missing, non-numeric, or negative.
func parseNonNegInt(v string) int {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return 0
}

// parsePageSize resolves the requested page size for the bounded list/search
// read: the storage default (1000) when unset, clamped to
// [1, maxIncidentPageSize] otherwise so a single request stays bounded.
func parsePageSize(v string) int {
	if v == "" {
		return storage.DefaultIncidentPageSize
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return storage.DefaultIncidentPageSize
	}
	if n > maxIncidentPageSize {
		return maxIncidentPageSize
	}
	return n
}

// parseAnalysisPageSize resolves the requested page size for the bounded
// analyses read: the storage default (1000) when unset, clamped to
// [1, maxIncidentPageSize] otherwise so a single request stays bounded — the
// analyses twin of parsePageSize.
func parseAnalysisPageSize(v string) int {
	if v == "" {
		return storage.DefaultAnalysisPageSize
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return storage.DefaultAnalysisPageSize
	}
	if n > maxIncidentPageSize {
		return maxIncidentPageSize
	}
	return n
}

// pageOffset resolves the starting row offset and the 1-based page number for
// a bounded list/search read from the two accepted params. An explicit
// zero-based offset cursor (what the UI's infinite scroll sends via
// next_offset) takes precedence; otherwise a 1-based page number is converted
// to an offset. When neither is set the read starts at the first page. size is
// the resolved page size (always > 0, from parsePageSize).
func pageOffset(pageParam, offsetParam string, size int) (offset, page int) {
	if offsetParam != "" {
		offset = parseNonNegInt(offsetParam)
		page = offset/size + 1
		return offset, page
	}
	page = 1
	if n, err := strconv.Atoi(pageParam); err == nil && n > 1 {
		page = n
	}
	offset = (page - 1) * size
	return offset, page
}

// originCountsMap renders a storage.IncidentCounts into the {ai_detect,
// webhook, total} shape the UI top-bar reads — the same shape originCounts
// produces from a materialized slice, so the paged and fallback paths return
// an identical counts object. The counts are unresolved-only (open work).
func originCountsMap(c storage.IncidentCounts) fiber.Map {
	return fiber.Map{
		"ai_detect": c.AIDetect,
		"webhook":   c.Webhook,
		"total":     c.Total,
	}
}

// perOriginMap renders one status bucket across origins into the {ai_detect,
// webhook, total} shape the UI reads for a single status row.
func perOriginMap(ai, webhook, total int) fiber.Map {
	return fiber.Map{"ai_detect": ai, "webhook": webhook, "total": total}
}

// statusCountsMap renders the whole-set per-origin × per-status tally into the
// nested by_status shape the UI consumes: one {ai_detect, webhook, total}
// object per status bucket (open / acked / resolved / all). It is additive to
// the existing {ai_detect, webhook, total} counts object — consumers that read
// only the top-level unresolved counts are unaffected.
func statusCountsMap(c storage.IncidentStatusCounts) fiber.Map {
	return fiber.Map{
		"open":     perOriginMap(c.AIDetect.Open, c.Webhook.Open, c.Total.Open),
		"acked":    perOriginMap(c.AIDetect.Acked, c.Webhook.Acked, c.Total.Acked),
		"resolved": perOriginMap(c.AIDetect.Resolved, c.Webhook.Resolved, c.Total.Resolved),
		"all":      perOriginMap(c.AIDetect.Total, c.Webhook.Total, c.Total.Total),
	}
}

// unresolvedCounts derives the open-work (unresolved = open + acked) per-origin
// tally from the full per-status breakdown, so the back-compat top-level counts
// object stays identical to what CountIncidents returned while the whole
// response is built from ONE by-status count.
func unresolvedCounts(c storage.IncidentStatusCounts) storage.IncidentCounts {
	return storage.IncidentCounts{
		AIDetect: c.AIDetect.Open + c.AIDetect.Acked,
		Webhook:  c.Webhook.Open + c.Webhook.Acked,
		Total:    c.Total.Open + c.Total.Acked,
	}
}

// countsMap renders the full count object the list / search / counts responses
// carry: the back-compat top-level unresolved (open-work) per-origin tally PLUS
// the additive by_status breakdown, both from ONE per-status count. Existing
// consumers read ai_detect/webhook/total unchanged; the count surfaces read
// by_status for the authoritative per-origin × per-status numbers.
func countsMap(c storage.IncidentStatusCounts) fiber.Map {
	m := originCountsMap(unresolvedCounts(c))
	m["by_status"] = statusCountsMap(c)
	return m
}

// filteredTotal returns the unresolved count that matches the active origin
// filter, derived from the whole-set breakdown so the badge for the active
// tab shows that tab's open count, while the counts object stays the full
// both-feeds breakdown. This is the badge total, NOT the pagination total:
// counts are unresolved-only, so "load more" is driven off page-fullness (a
// full page implies another page) rather than this number.
func filteredTotal(c storage.IncidentCounts, origin string) int {
	switch origin {
	case storage.OriginAIDetect:
		return c.AIDetect
	case storage.OriginWebhook:
		return c.Webhook
	default:
		return c.Total
	}
}

// pagedIncidentResponse builds the list/search response from the CHEAP count
// (the unresolved whole-set breakdown + the filtered open total) and ONE
// bounded page of rows. It preserves the existing shape — counts / total /
// incidents / page / page_size — and adds offset-based continuation: offset is
// where this page started and next_offset is where the caller resumes to load
// more, so the UI shows the open counts, renders the first page, and fetches
// the next chunk only on demand.
//
// Because counts are unresolved-only they cannot bound the list (the list is
// all-incidents so resolved rows stay reachable). "Has more" is therefore
// driven off PAGE-FULLNESS: a full page (len == size) implies at least one
// more page, an underfull page is the last one. This lets the operator page
// past the unresolved count — and past row 1000 — through the entire history.
func pagedIncidentResponse(recs []*storage.IncidentRecord, counts storage.IncidentStatusCounts, origin string, offset, size, page int) fiber.Map {
	total := filteredTotal(unresolvedCounts(counts), origin)
	out := make([]fiber.Map, 0, len(recs))
	for _, r := range recs {
		out = append(out, summarize(r))
	}
	resp := fiber.Map{
		"counts":    countsMap(counts),
		"total":     total,
		"incidents": out,
		"offset":    offset,
		"page":      page,
		"page_size": size,
	}
	// A full page implies the backend may hold more rows; the caller resumes
	// from the row just past this page. An underfull page is the last one.
	if len(recs) == size {
		resp["next_offset"] = offset + len(recs)
	} else {
		resp["next_offset"] = nil
	}
	return resp
}

// pagedAnalysisResponse builds the analyses list response from the CHEAP total
// count and ONE bounded page of rows. It preserves the existing `analyses`
// array and adds total + offset-based continuation (offset / next_offset /
// page / page_size), mirroring pagedIncidentResponse: the UI shows the true
// total, renders the first page, and fetches the next chunk only on demand. A
// full page (len == size) implies at least one more page; an underfull page is
// the last one.
func pagedAnalysisResponse(recs []*storage.AnalysisRecord, total, offset, size, page int) fiber.Map {
	out := recs
	if out == nil {
		out = []*storage.AnalysisRecord{}
	}
	resp := fiber.Map{
		"analyses":  out,
		"total":     total,
		"offset":    offset,
		"page":      page,
		"page_size": size,
	}
	if len(recs) == size {
		resp["next_offset"] = offset + len(recs)
	} else {
		resp["next_offset"] = nil
	}
	return resp
}

// search endpoints. It computes per-origin counts over the FULL result
// set (so the top-bar shows both feeds regardless of the active tab),
// applies the optional origin filter, then paginates. pageParam /
// pageSizeParam are the raw query strings; when pageParam is empty the
// endpoint returns the full origin-filtered window capped at limit — the
// back-compat shape existing callers depend on.
func incidentListResponse(recs []*storage.IncidentRecord, origin, pageParam, pageSizeParam string, limit int) fiber.Map {
	counts := originCounts(recs)
	// by_status is the authoritative per-origin × per-status breakdown; on this
	// fallback path (a backend with no bounded pager) it is tallied over the
	// materialized window so the count surfaces still read the same shape.
	counts["by_status"] = statusCountsMap(storage.StatusCountsOf(recs))
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
		return c.JSON(fiber.Map{"analyses": []any{}, "total": 0})
	}
	// Preferred path: the backend implements the pager capability, so we render
	// a page from ONE bounded query plus ONE cheap count query and never load
	// the whole vs_analyses table. Postgres (unbounded) and the file/memory
	// backends (already capped) all implement it.
	if pager, ok := store.(storage.AnalysisPager); ok {
		total, err := pager.CountAnalyses()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		size := parseAnalysisPageSize(c.Query("page_size"))
		offset, page := pageOffset(c.Query("page"), c.Query("offset"), size)
		recs, err := pager.ListAnalysesPage(offset, size)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(pagedAnalysisResponse(recs, total, offset, size, page))
	}
	// Fallback for backends without the pager capability (the redis stub): a
	// BOUNDED read so it can never load the whole table. Legacy shape (just
	// analyses) — the caller-supplied limit is clamped to the default page.
	limit := parseLimit(c.Query("limit"))
	if limit <= 0 || limit > storage.DefaultAnalysisPageSize {
		limit = storage.DefaultAnalysisPageSize
	}
	recs, err := store.ListAnalyses(limit)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"analyses": recs, "total": len(recs)})
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
