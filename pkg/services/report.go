package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/common"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"

	m "github.com/VersusControl/versus-incident/pkg/models"

	"golang.org/x/time/rate"
)

// ---------------------------------------------------------------------------
// Seams (mirror SetAnalyzeAgent / SetStorage)
// ---------------------------------------------------------------------------

// reportRenderer holds the process-wide report image renderer. The OSS
// build installs the default pure-Go PNG card renderer at boot; an
// enterprise build may install a branded / high-fidelity renderer behind
// this SAME seam. Nil-safe: the API returns 500 when unset.
var reportRenderer core.ReportRenderer

// SetReportRenderer installs the renderer used by the report endpoints.
func SetReportRenderer(r core.ReportRenderer) { reportRenderer = r }

// ReportRenderer returns the installed report renderer, or nil.
func ReportRenderer() core.ReportRenderer { return reportRenderer }

var (
	reportRedactorMu    sync.RWMutex
	reportRedactor      core.Scrubber
	defaultScrubber     core.Scrubber
	defaultScrubberOnce sync.Once
)

// SetReportRedactor installs the DLP scrubber every report text field is run
// through before it reaches the card or a channel. Boot installs the
// operator-configured redactor; when none is installed a safe default (the
// built-in agent redaction rules) is used instead, so a report is NEVER
// rendered from unredacted text.
func SetReportRedactor(s core.Scrubber) {
	reportRedactorMu.Lock()
	reportRedactor = s
	reportRedactorMu.Unlock()
}

// reportScrubber returns the installed scrubber, falling back to the
// built-in default so redaction always runs.
func reportScrubber() core.Scrubber {
	reportRedactorMu.RLock()
	s := reportRedactor
	reportRedactorMu.RUnlock()
	if s != nil {
		return s
	}
	defaultScrubberOnce.Do(func() {
		defaultScrubber, _ = agent.NewRedactor(false, nil)
	})
	return defaultScrubber
}

// ---------------------------------------------------------------------------
// Errors + outcome
// ---------------------------------------------------------------------------

var (
	// ErrReportDisabled is returned when the runtime report setting
	// enable is false.
	ErrReportDisabled = errors.New("report: feature disabled")
	// ErrReportRateLimited is returned when the per-window render/send
	// token bucket is exhausted.
	ErrReportRateLimited = errors.New("report: rate limited")
	// ErrReportNoRenderer is returned when no renderer is installed.
	ErrReportNoRenderer = errors.New("report: no renderer configured")
	// ErrReportNoStorage is returned when persistence is not configured.
	ErrReportNoStorage = errors.New("report: storage not configured")
	// ErrReportNoChannel is returned when no enabled channel resolves for
	// delivery.
	ErrReportNoChannel = errors.New("report: no enabled channel resolved")
)

// ReportSendOptions selects the window and (optionally) the channel for an
// aggregate report send. Channel is the explicit target; empty falls through
// the resolution precedence (runtime default_channel → error).
type ReportSendOptions struct {
	Window      string
	Channel     string
	RequestedBy string
}

// ReportOutcome aggregates per-channel delivery results — never
// short-circuiting, exactly like AlertResult. Sent = image delivered;
// Fallback = text summary + note delivered (image-incapable channel);
// Failed = channel returned an error (the PNG is still downloadable).
type ReportOutcome struct {
	Window   string            `json:"window"`
	Sent     []string          `json:"sent"`
	Fallback []string          `json:"fallback"`
	Failed   map[string]string `json:"failed"`
	Bytes    int               `json:"bytes"`
}

// ---------------------------------------------------------------------------
// Per-window rate limiting (DoS guard — rendering + the full-window scan are
// CPU/alloc-bound)
// ---------------------------------------------------------------------------

var (
	reportLimiters   = map[string]*rate.Limiter{}
	reportLimitersMu sync.Mutex
)

// reportAllow consumes one token from the bucket keyed by key (the report
// path keys it "report:<window>"). perMinute<=0 disables the cap
// (unbounded). Burst equals perMinute so a small flurry is allowed, then the
// steady rate is enforced.
func reportAllow(key string, perMinute int) bool {
	if perMinute <= 0 {
		return true
	}
	reportLimitersMu.Lock()
	defer reportLimitersMu.Unlock()
	lim, ok := reportLimiters[key]
	if !ok {
		lim = rate.NewLimiter(rate.Limit(float64(perMinute)/60.0), perMinute)
		reportLimiters[key] = lim
	}
	return lim.Allow()
}

// ---------------------------------------------------------------------------
// Model assembler (T1) — reads an IncidentRecord (+ latest analysis) and
// scrubs EVERY text field. No AckURL / token / config value is ever placed
// on the model.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Window resolution + aggregate assembler — query the incidents in a
// window and scrub EVERY attacker-influenced text field (service names,
// titles). No AckURL / token / config value is ever placed on the model.
// ---------------------------------------------------------------------------

// severityBands is the fixed, ordered set of severity buckets the aggregate
// reports on. Order is triage priority (worst first) so the renderer draws a
// stable, sorted breakdown.
var severityBands = []string{"critical", "high", "medium", "low", "unknown"}

// severityBand maps a free-form severity/verdict label to one of the fixed
// bands. The unknown band catches webhook incidents with no severity so a
// row is never dropped.
func severityBand(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "fatal", "emergency":
		return "critical"
	case "high", "error", "err":
		return "high"
	case "medium", "warning", "warn":
		return "medium"
	case "low", "info", "notice":
		return "low"
	default:
		return "unknown"
	}
}

// WindowBounds resolves a window label to a [start, end) UTC range and the
// trend unit ("hour" for today/24h, "day" for 7d). An unknown/absent window
// defaults to today. now is the reference time (UTC); callers pass
// time.Now().UTC() in production and a fixed time in tests.
func WindowBounds(window string, now time.Time) (start, end time.Time, unit string) {
	now = now.UTC()
	switch normalizeReportWindow(window) {
	case reportWindow24h:
		return now.Add(-24 * time.Hour), now, "hour"
	case reportWindow7d:
		return now.Add(-7 * 24 * time.Hour), now, "day"
	default: // today
		startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return startOfDay, now, "hour"
	}
}

// BuildAggregateReportModel assembles the channel-agnostic, already-redacted
// aggregate ReportModel for a window. recs are the incidents whose CreatedAt
// falls in [start, end) (newest first). scrubber MUST be non-nil in
// production (callers pass reportScrubber()); it is the DLP boundary for
// every attacker-influenced label. window/start/end/unit come from
// WindowBounds.
func BuildAggregateReportModel(recs []*storage.IncidentRecord, window string, start, end time.Time, unit string, scrubber core.Scrubber, includeCharts bool) core.ReportModel {
	scrub := func(s string) string {
		if scrubber == nil {
			return s
		}
		return scrubber.Scrub(s)
	}

	model := core.ReportModel{
		Window:        normalizeReportWindow(window),
		WindowStart:   start.UTC(),
		WindowEnd:     end.UTC(),
		GeneratedAt:   end.UTC(),
		ByOrigin:      map[string]int{storage.OriginAIDetect: 0, storage.OriginWebhook: 0},
		TrendUnit:     unit,
		IncludeCharts: includeCharts,
		Footer:        "Versus Incident",
	}

	sevCounts := map[string]int{}
	sevOriginCounts := map[string]struct{ ai, webhook int }{}
	type svcTally struct {
		count, ai, webhook int
	}
	svc := map[string]*svcTally{}
	var notable []core.NotableIncident

	for _, rec := range recs {
		if rec == nil {
			continue
		}
		model.Total++
		origin := rec.EffectiveOrigin()
		model.ByOrigin[origin]++
		if rec.Resolved {
			model.Resolved++
		} else {
			model.Open++
		}

		band := severityBand(reportSeverity(rec))
		sevCounts[band]++
		so := sevOriginCounts[band]
		if origin == storage.OriginAIDetect {
			so.ai++
		} else {
			so.webhook++
		}
		sevOriginCounts[band] = so
		if band == "critical" || band == "high" {
			model.CriticalHigh++
		}

		// Service tally (redacted label).
		svcLabel := scrub(reportServiceName(rec))
		if svcLabel == "" {
			svcLabel = "unknown"
		}
		t := svc[svcLabel]
		if t == nil {
			t = &svcTally{}
			svc[svcLabel] = t
		}
		t.count++
		if origin == storage.OriginAIDetect {
			t.ai++
		} else {
			t.webhook++
		}

		// Notable: recent critical/high (recs are newest-first, so the
		// first 5 collected are the most recent).
		if (band == "critical" || band == "high") && len(notable) < 5 {
			title := scrub(rec.Title)
			if title == "" {
				title = scrub(reportTitle(rec))
			}
			notable = append(notable, core.NotableIncident{
				ShortID:   shortID(rec.ID),
				Title:     truncateRunes(title, 80),
				Service:   truncateRunes(svcLabel, 40),
				Severity:  band,
				CreatedAt: rec.CreatedAt.UTC(),
			})
		}
	}

	// Severity breakdown in the fixed triage order (bounded ≤5 bars).
	for _, band := range severityBands {
		so := sevOriginCounts[band]
		model.BySeverity = append(model.BySeverity, core.Bucket{
			Label:    band,
			Count:    sevCounts[band],
			AIDetect: so.ai,
			Webhook:  so.webhook,
		})
	}

	// Trend buckets (bounded: ≤24 hourly / ≤7 daily).
	model.Trend = buildTrend(recs, start.UTC(), end.UTC(), unit)

	// Top services by count, deterministic (count desc, then label asc);
	// bounded to the top 5.
	labels := make([]string, 0, len(svc))
	for l := range svc {
		labels = append(labels, l)
	}
	sort.Slice(labels, func(i, j int) bool {
		if svc[labels[i]].count != svc[labels[j]].count {
			return svc[labels[i]].count > svc[labels[j]].count
		}
		return labels[i] < labels[j]
	})
	if len(labels) > 5 {
		labels = labels[:5]
	}
	for _, l := range labels {
		model.TopServices = append(model.TopServices, core.Bucket{
			Label:    truncateRunes(l, 40),
			Count:    svc[l].count,
			AIDetect: svc[l].ai,
			Webhook:  svc[l].webhook,
		})
	}

	model.Notable = notable
	return model
}

// buildTrend buckets incidents into the trend series over [start, end): one
// bucket per hour (unit "hour") or per day (unit "day"), bounded to ≤24 / ≤7
// buckets. Each bucket carries the per-origin split for a stacked bar. recs
// outside the window are ignored (defence-in-depth — the query should have
// already scoped them).
func buildTrend(recs []*storage.IncidentRecord, start, end time.Time, unit string) []core.Bucket {
	var step time.Duration
	var maxBuckets int
	var labelFmt string
	if unit == "day" {
		step = 24 * time.Hour
		maxBuckets = 7
		labelFmt = "01-02"
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	} else {
		step = time.Hour
		maxBuckets = 24
		labelFmt = "15:04"
		start = start.Truncate(time.Hour)
	}

	// Build the slot boundaries first so empty windows still render an axis.
	buckets := []core.Bucket{}
	for slot := start; slot.Before(end) && len(buckets) < maxBuckets; slot = slot.Add(step) {
		buckets = append(buckets, core.Bucket{Label: slot.Format(labelFmt)})
	}
	if len(buckets) == 0 {
		// Degenerate window (end <= start): a single slot so the renderer
		// always has an axis to draw.
		buckets = append(buckets, core.Bucket{Label: start.Format(labelFmt)})
	}

	for _, rec := range recs {
		if rec == nil {
			continue
		}
		created := rec.CreatedAt.UTC()
		idx := int(created.Sub(start) / step)
		if idx < 0 || idx >= len(buckets) {
			continue
		}
		buckets[idx].Count++
		if rec.EffectiveOrigin() == storage.OriginAIDetect {
			buckets[idx].AIDetect++
		} else {
			buckets[idx].Webhook++
		}
	}
	return buckets
}

// reportSeverity extracts a best-effort severity for one record from its
// content (Severity, then Verdict). Raw — the caller bands it.
func reportSeverity(rec *storage.IncidentRecord) string {
	s := contentString(rec.Content, "Severity", "severity")
	if s == "" {
		s = contentString(rec.Content, "Verdict", "verdict")
	}
	return s
}

// reportServiceName resolves the service label for one record: the durable
// Service field, else a best-effort pull from content.
func reportServiceName(rec *storage.IncidentRecord) string {
	if rec.Service != "" {
		return rec.Service
	}
	return contentString(rec.Content, "ServiceName", "Service", "service")
}

// reportTitle resolves a display title for one record when the durable Title
// is empty.
func reportTitle(rec *storage.IncidentRecord) string {
	return contentString(rec.Content, "AlertName", "Summary", "title", "alertname")
}

// ---------------------------------------------------------------------------
// Public entry points
// ---------------------------------------------------------------------------

// RenderIncidentsReport renders the aggregate report PNG for a window
// (preview / download). It is read-only over stored state.
func RenderIncidentsReport(ctx context.Context, window string) (*core.ReportImage, error) {
	cfg := config.GetConfig()
	return renderReport(ctx, cfg, store, reportRenderer, reportScrubber(), window)
}

// SendIncidentsReport renders the aggregate report and delivers it to the
// resolved channel(s): image upload where supported, redacted text summary +
// note where not. Per-channel outcomes are aggregated without
// short-circuiting.
func SendIncidentsReport(ctx context.Context, opts ReportSendOptions) (*ReportOutcome, error) {
	cfg := config.GetConfig()
	providers, err := common.NewAlertProviderFactory(cfg).CreateProviders()
	if err != nil {
		return nil, fmt.Errorf("report: build providers: %w", err)
	}
	return sendReport(ctx, cfg, store, reportRenderer, reportScrubber(), providers, opts)
}

// ---------------------------------------------------------------------------
// Internal orchestration (config + deps injected so it is unit-testable
// without the global config / storage / factory)
// ---------------------------------------------------------------------------

// buildWindowModel reads the runtime settings, applies the guards, queries
// the incidents in the window, and assembles the redacted aggregate model.
// It is the shared front-half of render + send. window has already been
// normalized by the caller boundary; it is normalized again here defensively.
func buildWindowModel(st storage.Provider, scrubber core.Scrubber, settings ReportSettings, window string) (core.ReportModel, error) {
	if !settings.Enable {
		return core.ReportModel{}, ErrReportDisabled
	}
	if st == nil {
		return core.ReportModel{}, ErrReportNoStorage
	}
	window = normalizeReportWindow(window)
	if !reportAllow("report:"+window, settings.RatePerMinute) {
		return core.ReportModel{}, ErrReportRateLimited
	}
	start, end, unit := WindowBounds(window, time.Now().UTC())
	recs, err := incidentsInWindow(st, start, end)
	if err != nil {
		return core.ReportModel{}, err
	}
	return BuildAggregateReportModel(recs, window, start, end, unit, scrubber, settings.IncludeChart), nil
}

func renderReport(ctx context.Context, cfg *config.Config, st storage.Provider, renderer core.ReportRenderer, scrubber core.Scrubber, window string) (*core.ReportImage, error) {
	settings := LoadReportSettings(st)
	if renderer == nil {
		return nil, ErrReportNoRenderer
	}
	model, err := buildWindowModel(st, scrubber, settings, window)
	if err != nil {
		return nil, err
	}
	return renderer.Render(ctx, model)
}

func sendReport(ctx context.Context, cfg *config.Config, st storage.Provider, renderer core.ReportRenderer, scrubber core.Scrubber, providers []core.AlertProvider, opts ReportSendOptions) (*ReportOutcome, error) {
	settings := LoadReportSettings(st)
	if renderer == nil {
		return nil, ErrReportNoRenderer
	}
	window := normalizeReportWindow(opts.Window)
	model, err := buildWindowModel(st, scrubber, settings, window)
	if err != nil {
		return nil, err
	}
	img, err := renderer.Render(ctx, model)
	if err != nil {
		return nil, fmt.Errorf("report: render: %w", err)
	}

	// Resolve target channels to enabled providers.
	byName := map[string]core.AlertProvider{}
	for _, p := range providers {
		byName[p.Name()] = p
	}
	var targets []core.AlertProvider
	for _, name := range resolveReportChannels(opts.Channel, settings.DefaultChannel) {
		if p, ok := byName[name]; ok {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return nil, ErrReportNoChannel
	}

	caption := reportCaption(model)
	fallbackText := caption + "\n\n" + fallbackNote(cfg, window)
	// A minimal incident carrier — carries NO raw content, so the text
	// senders (Teams/Viber/Lark) cannot leak unredacted fields; the
	// already-redacted caption is the only text that travels.
	carrier := &m.Incident{ID: "report-" + window}
	att := core.Attachment{Filename: img.Filename, MIME: img.MIME, Data: img.Data, Caption: caption}

	out := &ReportOutcome{Window: window, Failed: map[string]string{}, Bytes: len(img.Data)}
	for _, p := range targets {
		if as, ok := p.(core.AttachmentSender); ok {
			if err := as.SendAttachment(carrier, att); err != nil {
				out.Failed[p.Name()] = err.Error()
			} else {
				out.Sent = append(out.Sent, p.Name())
			}
			continue
		}
		if ts, ok := p.(core.TextSender); ok {
			if err := ts.SendText(carrier, fallbackText); err != nil {
				out.Failed[p.Name()] = err.Error()
			} else {
				out.Fallback = append(out.Fallback, p.Name())
			}
			continue
		}
		// Neither capability: graceful no-push fallback — the operator can
		// still download the PNG via GET report.png.
		out.Fallback = append(out.Fallback, p.Name())
	}
	return out, nil
}

// incidentsInWindow returns the incidents whose CreatedAt falls in
// [start, end). It prefers the optional storage.RangeLister capability
// (Postgres pushes the CreatedAt bound into SQL) and falls back to
// ListIncidents(0) + an in-service filter for file/memory backends — the
// exact pattern the admin list handler already uses. No new storage
// primitive is required for OSS.
func incidentsInWindow(st storage.Provider, start, end time.Time) ([]*storage.IncidentRecord, error) {
	if rl, ok := st.(storage.RangeLister); ok {
		return rl.ListIncidentsInRange(start, end, 0)
	}
	recs, err := st.ListIncidents(0)
	if err != nil {
		return nil, err
	}
	out := make([]*storage.IncidentRecord, 0, len(recs))
	for _, r := range recs {
		if r == nil {
			continue
		}
		c := r.CreatedAt.UTC()
		if c.Before(start) {
			continue
		}
		if !end.IsZero() && !c.Before(end) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// resolveReportChannels applies the precedence: explicit → runtime
// default_channel → none. The report is window-scoped, so there is no
// per-incident "share back where it fired" fallback.
func resolveReportChannels(explicit, defaultChannel string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	if defaultChannel != "" {
		return []string{defaultChannel}
	}
	return nil
}

// reportCaption is the short, already-redacted text summary attached to an
// image upload and used as the fallback body. It draws only from the
// redacted aggregate ReportModel — never from raw content.
func reportCaption(model core.ReportModel) string {
	var b strings.Builder
	b.WriteString("Incident report — ")
	b.WriteString(windowLabel(model.Window))
	b.WriteString(": ")
	b.WriteString(fmt.Sprintf("%d incident", model.Total))
	if model.Total != 1 {
		b.WriteString("s")
	}
	ai := model.ByOrigin[storage.OriginAIDetect]
	wh := model.ByOrigin[storage.OriginWebhook]
	b.WriteString(fmt.Sprintf(" (%d AI-detect, %d webhook)", ai, wh))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("%d open · %d resolved · %d critical/high", model.Open, model.Resolved, model.CriticalHigh))
	b.WriteString("\nWindow ")
	b.WriteString(model.WindowStart.UTC().Format("2006-01-02 15:04"))
	b.WriteString(" → ")
	b.WriteString(model.WindowEnd.UTC().Format("2006-01-02 15:04"))
	b.WriteString(" UTC")
	if len(model.TopServices) > 0 {
		names := make([]string, 0, len(model.TopServices))
		for _, s := range model.TopServices {
			names = append(names, s.Label)
		}
		b.WriteString("\nTop services: ")
		b.WriteString(truncateRunes(strings.Join(names, ", "), 200))
	}
	return b.String()
}

// windowLabel is the human label for a window used in the caption/filename.
func windowLabel(window string) string {
	switch window {
	case reportWindow24h:
		return "last 24h"
	case reportWindow7d:
		return "last 7d"
	default:
		return "today"
	}
}

// fallbackNote is the trailing note for image-incapable channels. Link
// precedence (design §5): the root public_host (config.yaml top-level) alone
// decides whether a link is offered — public_host set → the gateway-guarded
// report.png link; public_host empty → an air-gapped UI pointer (no link is
// fabricated). There is no report.public_url anymore. The link carries no
// bearer, so it is only useful behind an authenticated proxy.
func fallbackNote(cfg *config.Config, window string) string {
	if strings.TrimSpace(cfg.PublicHost) != "" {
		host := strings.TrimRight(cfg.PublicHost, "/")
		return "View the report image: " + host + "/api/admin/reports/incidents/report.png?window=" + window
	}
	return "(image reports aren't supported on this channel — open Reports in the Versus UI)"
}

func shortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// contentString returns the first non-empty string value found at any of
// the given keys (exact, then case-insensitive).
func contentString(content map[string]interface{}, keys ...string) string {
	if content == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := content[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	lower := map[string]interface{}{}
	for k, v := range content {
		lower[strings.ToLower(k)] = v
	}
	for _, k := range keys {
		if v, ok := lower[strings.ToLower(k)]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
