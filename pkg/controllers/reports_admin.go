package controllers

import (
	"errors"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

// ReportsAdminController exposes the incidents-analytics report: render an
// aggregate dashboard PNG over a time window and deliver it to a channel,
// preview it, and read/update the runtime report settings. Window-scoped, not
// per-incident. Same X-Gateway-Secret guard as the rest of the admin surface.
type ReportsAdminController struct{}

// NewReportsAdminController returns a controller. No state of its own; storage
// + renderer are read via the services seams.
func NewReportsAdminController() *ReportsAdminController {
	return &ReportsAdminController{}
}

// Register attaches the endpoints under /api/admin/reports.
//
//	POST /api/admin/reports/incidents            render aggregate + send to a channel (?window=)
//	GET  /api/admin/reports/incidents/report.png render + return the PNG (?window=)
//	GET  /api/admin/reports/settings             current runtime report settings
//	PUT  /api/admin/reports/settings             update runtime report settings
func (rc *ReportsAdminController) Register(router fiber.Router) {
	g := router.Group("/admin/reports", rc.authMiddleware)
	// The literal /incidents/report.png MUST be registered before
	// /incidents so the more specific path wins.
	g.Get("/incidents/report.png", rc.reportPNG)
	g.Post("/incidents", rc.send)
	g.Get("/settings", rc.getSettings)
	g.Put("/settings", rc.putSettings)
}

// authMiddleware reuses the agent gateway secret (constant-time compare),
// mirroring the incident admin surface.
func (rc *ReportsAdminController) authMiddleware(c *fiber.Ctx) error {
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

// reportSendRequest is the optional body for POST /incidents. Channel is
// optional: empty falls through the resolution precedence (runtime
// default_channel → error).
type reportSendRequest struct {
	Channel     string `json:"channel"`
	RequestedBy string `json:"requested_by"`
}

// send renders the aggregate report for the requested window and delivers it
// to the resolved channel(s). Returns the per-channel outcome; 502 when at
// least one channel send failed (the PNG is still downloadable).
func (rc *ReportsAdminController) send(c *fiber.Ctx) error {
	window, ok := validReportWindow(c.Query("window"))
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid window (want today|24h|7d)"})
	}
	var body reportSendRequest
	_ = c.BodyParser(&body)

	out, err := services.SendIncidentsReport(c.UserContext(), services.ReportSendOptions{
		Window:      window,
		Channel:     strings.Clone(strings.TrimSpace(body.Channel)),
		RequestedBy: strings.Clone(strings.TrimSpace(body.RequestedBy)),
	})
	if err != nil {
		return reportError(c, err)
	}
	status := fiber.StatusOK
	if len(out.Failed) > 0 {
		// Partial (or total) channel failure — the image is still
		// retrievable, so surface 502 with the full outcome.
		status = fiber.StatusBadGateway
	}
	return c.Status(status).JSON(out)
}

// reportPNG renders the aggregate dashboard for the requested window and
// returns it as image/png for preview / download.
func (rc *ReportsAdminController) reportPNG(c *fiber.Ctx) error {
	window, ok := validReportWindow(c.Query("window"))
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid window (want today|24h|7d)"})
	}
	img, err := services.RenderIncidentsReport(c.UserContext(), window)
	if err != nil {
		return reportError(c, err)
	}
	c.Set(fiber.HeaderContentType, img.MIME)
	c.Set(fiber.HeaderContentDisposition, "inline; filename=\""+img.Filename+"\"")
	return c.Status(fiber.StatusOK).Send(img.Data)
}

// validReportWindow enforces the window contract at the HTTP boundary: an
// absent/empty window is allowed (the service defaults it to today), a
// recognized window passes through (cloned off the pooled buffer), and a
// present-but-unrecognized window is rejected with ok=false so the handler
// returns 400.
func validReportWindow(raw string) (string, bool) {
	w := strings.TrimSpace(raw)
	switch w {
	case "":
		return "", true
	case "today", "24h", "7d":
		return strings.Clone(w), true
	default:
		return "", false
	}
}

// getSettings returns the current runtime report settings (or the built-in
// defaults when none are stored).
func (rc *ReportsAdminController) getSettings(c *fiber.Ctx) error {
	return c.JSON(services.LoadReportSettings(services.Storage()))
}

// putSettings persists updated runtime report settings. The whole settings
// object is replaced (idempotent); values are sanitized in the store.
func (rc *ReportsAdminController) putSettings(c *fiber.Ctx) error {
	var s services.ReportSettings
	if err := c.BodyParser(&s); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid settings body"})
	}
	// Copy the strings off the pooled request buffer before they outlive the
	// request.
	s.DefaultChannel = strings.Clone(strings.TrimSpace(s.DefaultChannel))
	s.DefaultWindow = strings.Clone(strings.TrimSpace(s.DefaultWindow))
	s.SendTime = strings.Clone(strings.TrimSpace(s.SendTime))
	s.Timezone = strings.Clone(strings.TrimSpace(s.Timezone))
	// Validate the scheduler fields at the HTTP boundary. Empty is allowed —
	// the store sanitizes it back to the built-in default (09:00 / UTC) — but
	// a non-empty value must be well-formed: send_time must be "HH:MM" 24h and
	// timezone must be "UTC" or a loadable IANA name.
	if s.SendTime != "" && !services.ValidSendTime(s.SendTime) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid send_time (want HH:MM, 24-hour)"})
	}
	if s.Timezone != "" && !services.ValidTimezone(s.Timezone) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid timezone (want UTC or an IANA name like Asia/Ho_Chi_Minh)"})
	}
	if err := services.SaveReportSettings(services.Storage(), s); err != nil {
		if errors.Is(err, services.ErrReportNoStorage) {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	// Return the effective (sanitized) settings after the write.
	return c.JSON(services.LoadReportSettings(services.Storage()))
}

// reportError maps the service-layer sentinels to HTTP statuses. A bad window
// is rejected upstream by normalizing to today, so the only "bad request"
// here is a missing channel; disabled → 501, rate-limited → 429, no storage →
// 503.
func reportError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, services.ErrReportDisabled):
		return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{"error": "report feature disabled"})
	case errors.Is(err, services.ErrReportRateLimited):
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "rate limited — try again shortly"})
	case errors.Is(err, services.ErrReportNoStorage):
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
	case errors.Is(err, services.ErrReportNoRenderer):
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "report renderer not available"})
	case errors.Is(err, services.ErrReportNoChannel):
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no enabled channel resolved for this report"})
	default:
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
}
