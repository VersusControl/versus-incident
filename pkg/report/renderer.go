// Package report is the OSS default implementation of core.ReportRenderer:
// a pure-Go, in-binary PNG "incidents analytics dashboard" renderer. It
// composes a fixed 1200x900 image with the Go standard library (image /
// image/draw / image/png) plus golang.org/x/image for font rasterization,
// using the compiled-in Go font (golang.org/x/image/font/gofont) — a []byte
// baked into the binary, NOT an external file.
//
// This keeps the single-binary + air-gapped promise intact: no headless
// browser, no external process, no external font file, and no network. The
// only thing the report path ever sends over the wire is the channel upload
// itself (handled elsewhere). A branded / high-fidelity renderer is an
// enterprise concern installed behind the SAME core.ReportRenderer seam
// (services.SetReportRenderer); the API, channel delivery, and UI never
// change when the renderer is swapped.
package report

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Card dimensions. Fixed and bounded on purpose (a DoS/abuse guard as well
// as a layout simplification): a caller can never ask for an arbitrarily
// large canvas, so render cost is O(1) in the request. The dashboard is
// taller than the old single-incident card to fit the headline strip, the
// trend + severity charts, and the notable lists.
const (
	CardWidth  = 1200
	CardHeight = 900
	padding    = 48
)

// reportOriginAIDetect / reportOriginWebhook mirror the storage Origin
// constants (the ByOrigin map keys) as string literals, so the renderer
// stays dependent on core alone (it never imports storage).
const (
	reportOriginAIDetect = "ai_detect"
	reportOriginWebhook  = "webhook"
)

// Palette — a clean dark card that reads well pasted into Slack/Telegram/
// email. Deliberately understated and professional (guardrail: the OSS card
// must be first-class, not a teaser).
var (
	colBG        = color.RGBA{0x0f, 0x17, 0x2a, 0xff} // slate-900
	colPanel     = color.RGBA{0x1e, 0x29, 0x3b, 0xff} // slate-800 (chips/panels)
	colDivider   = color.RGBA{0x33, 0x41, 0x55, 0xff} // slate-700
	colText      = color.RGBA{0xe2, 0xe8, 0xf0, 0xff} // slate-200
	colTextMuted = color.RGBA{0x94, 0xa3, 0xb8, 0xff} // slate-400
	colTextFaint = color.RGBA{0x64, 0x74, 0x8b, 0xff} // slate-500
	colAccent    = color.RGBA{0x38, 0xbd, 0xf8, 0xff} // sky-400 (ai-detect series)
)

// severityColor maps a severity label (any case) to its accent color. The
// default (unknown / empty) is a neutral slate so a webhook incident with no
// severity still renders cleanly.
func severityColor(sev string) color.RGBA {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical":
		return color.RGBA{0xef, 0x44, 0x44, 0xff} // red-500
	case "high":
		return color.RGBA{0xf9, 0x73, 0x16, 0xff} // orange-500
	case "medium", "warning", "warn":
		return color.RGBA{0xf5, 0x9e, 0x0b, 0xff} // amber-500
	case "low", "info":
		return color.RGBA{0x38, 0xbd, 0xf8, 0xff} // sky-400
	default:
		return color.RGBA{0x64, 0x74, 0x8b, 0xff} // slate-500
	}
}

// Renderer is the default pure-Go PNG card renderer. It parses the embedded
// Go font ONCE at construction; per-render font faces are created inside
// Render so concurrent renders never share a face's internal buffers (data-
// race safe under -race).
type Renderer struct {
	regular *opentype.Font
	bold    *opentype.Font
}

// NewRenderer parses the compiled-in Go font and returns a ready renderer.
// It returns an error only if the embedded font bytes fail to parse, which
// would be a build-time defect, never a runtime/config condition.
func NewRenderer() (*Renderer, error) {
	reg, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("report: parse regular font: %w", err)
	}
	bold, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("report: parse bold font: %w", err)
	}
	return &Renderer{regular: reg, bold: bold}, nil
}

// faces bundles the per-render font faces at the sizes the layout uses.
type faces struct {
	title  font.Face // dashboard title
	stat   font.Face // big headline stat numbers
	h2     font.Face // section headings
	body   font.Face // body text / list rows
	small  font.Face // meta / labels
	tiny   font.Face // axis ticks / notable meta
	footer font.Face // footer
}

func (r *Renderer) newFaces() (*faces, func(), error) {
	// DPI 72 ⇒ 1pt == 1px, so Size is effectively pixels.
	mk := func(f *opentype.Font, size float64) (font.Face, error) {
		return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	}
	fc := &faces{}
	var made []font.Face
	build := func(f *opentype.Font, size float64) font.Face {
		face, err := mk(f, size)
		if err != nil {
			// Fall back to the body size on the off chance a size is
			// rejected; never nil so drawing cannot panic.
			face, _ = mk(f, 16)
		}
		made = append(made, face)
		return face
	}
	fc.title = build(r.bold, 30)
	fc.stat = build(r.bold, 34)
	fc.h2 = build(r.bold, 15)
	fc.body = build(r.regular, 17)
	fc.small = build(r.regular, 14)
	fc.tiny = build(r.regular, 11)
	fc.footer = build(r.regular, 13)
	closer := func() {
		for _, f := range made {
			_ = f.Close()
		}
	}
	return fc, closer, nil
}

// Render composes the dashboard card and encodes it to PNG. It never
// performs I/O beyond writing to an in-memory buffer, and is deterministic
// for a fixed model (no time.Now / map iteration / randomness in the draw
// path), so the "community binary is byte-for-byte the OSS card" guarantee
// holds.
func (r *Renderer) Render(ctx context.Context, m core.ReportModel) (*core.ReportImage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fc, closeFaces, err := r.newFaces()
	if err != nil {
		return nil, err
	}
	defer closeFaces()

	img := image.NewRGBA(image.Rect(0, 0, CardWidth, CardHeight))
	draw.Draw(img, img.Bounds(), image.NewUniform(colBG), image.Point{}, draw.Src)

	accent := worstSeverityColor(m.BySeverity)
	contentRight := CardWidth - padding

	// Top accent bar — the at-a-glance accent (worst severity present).
	fillRect(img, 0, 0, CardWidth, 10, accent)

	// ---- Header ------------------------------------------------------
	y := padding + 30
	drawString(img, fc.title, colText, padding, y, "Incident report")
	y += 30
	sub := fmt.Sprintf("%s → %s UTC", fmtTime(m.WindowStart), fmtTime(m.WindowEnd))
	drawString(img, fc.body, colTextMuted, padding, y, sub)
	y += 26
	divider(img, padding, y, contentRight)
	y += 24

	// ---- Headline stat tiles ----------------------------------------
	tileTop := y
	tileH := 92
	gap := 16
	usable := contentRight - padding
	tileW := (usable - 3*gap) / 4
	ai := m.ByOrigin[reportOriginAIDetect]
	wh := m.ByOrigin[reportOriginWebhook]
	tiles := []struct{ label, value string }{
		{"TOTAL", strconv.Itoa(m.Total)},
		{"AI-DETECT / WEBHOOK", fmt.Sprintf("%d / %d", ai, wh)},
		{"OPEN / RESOLVED", fmt.Sprintf("%d / %d", m.Open, m.Resolved)},
		{"CRITICAL / HIGH", strconv.Itoa(m.CriticalHigh)},
	}
	for i, t := range tiles {
		tx := padding + i*(tileW+gap)
		drawStatTile(img, fc, tx, tileTop, tileW, tileH, t.label, t.value)
	}
	y = tileTop + tileH + 28

	// ---- Charts (trend + severity), honoring include_chart ----------
	if m.IncludeCharts {
		midGap := 32
		colW := (usable - midGap) / 2
		chartTop := y
		chartH := 210
		drawTrendPanel(img, fc, padding, chartTop, colW, chartH, m)
		drawSeverityPanel(img, fc, padding+colW+midGap, chartTop, colW, chartH, m)
		y = chartTop + chartH + 28
	} else {
		drawString(img, fc.small, colTextFaint, padding, y+16, "Charts disabled in report settings.")
		y += 40
	}

	divider(img, padding, y, contentRight)
	y += 26

	// ---- Notable lists: top services + recent high-severity ---------
	listTop := y
	midGap := 32
	colW := (usable - midGap) / 2
	drawTopServices(img, fc, padding, listTop, colW, m)
	drawNotable(img, fc, padding+colW+midGap, listTop, colW, m)

	// Empty-window hint (still a valid card, never an error).
	if m.Total == 0 {
		drawString(img, fc.body, colTextMuted, padding, listTop+30, "No incidents in "+windowTitle(m.Window)+".")
	}

	// ---- Footer -----------------------------------------------------
	footerY := CardHeight - 26
	footer := m.Footer
	if footer == "" {
		footer = "Versus Incident"
	}
	left := "generated " + fmtTime(m.GeneratedAt) + " UTC"
	drawString(img, fc.footer, colTextFaint, padding, footerY, left)
	fw := textWidth(fc.footer, footer)
	drawString(img, fc.footer, colTextMuted, contentRight-fw, footerY, footer)

	// Encode.
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("report: encode png: %w", err)
	}
	filename := "incidents-" + m.Window + "-" + m.WindowEnd.UTC().Format("20060102") + ".png"
	return &core.ReportImage{
		Data:     buf.Bytes(),
		MIME:     "image/png",
		Filename: sanitizeFilename(filename),
		Width:    CardWidth,
		Height:   CardHeight,
	}, nil
}

// drawStatTile draws one headline stat: a panel with a big value and a small
// muted label beneath it.
func drawStatTile(img *image.RGBA, fc *faces, x, y, w, h int, label, value string) {
	fillRect(img, x, y, x+w, y+h, colPanel)
	drawString(img, fc.stat, colText, x+16, y+50, truncateToWidth(fc.stat, value, w-32))
	drawString(img, fc.small, colTextMuted, x+16, y+h-16, truncateToWidth(fc.small, label, w-32))
}

// drawTrendPanel draws the incidents-over-time bar timeline. Each bar is a
// stacked column (webhook base + ai-detect on top). Bars are bounded to the
// model's trend buckets (≤24 hourly / ≤7 daily). An empty window still draws
// the axis (all-zero bars), never a panic.
func drawTrendPanel(img *image.RGBA, fc *faces, x, y, w, h int, m core.ReportModel) {
	unit := "per hour"
	if m.TrendUnit == "day" {
		unit = "per day"
	}
	drawString(img, fc.h2, colTextMuted, x, y+4, "TREND ("+unit+")")
	fillRect(img, x, y+16, x+w, y+h, colPanel)

	plotTop := y + 30
	plotBottom := y + h - 26
	plotLeft := x + 14
	plotRight := x + w - 14
	// Baseline axis.
	fillRect(img, plotLeft, plotBottom, plotRight, plotBottom+1, colDivider)

	buckets := m.Trend
	if len(buckets) == 0 {
		return
	}
	maxCount := 0
	for _, b := range buckets {
		if b.Count > maxCount {
			maxCount = b.Count
		}
	}
	if maxCount < 1 {
		maxCount = 1
	}
	plotH := plotBottom - plotTop
	if plotH < 1 {
		plotH = 1
	}
	slotW := (plotRight - plotLeft) / len(buckets)
	if slotW < 1 {
		slotW = 1
	}
	barW := slotW - 4
	if barW < 1 {
		barW = 1
	}
	// Label at most ~6 ticks so the axis never turns to mush.
	labelEvery := 1
	if len(buckets) > 6 {
		labelEvery = (len(buckets) + 5) / 6
	}
	for i, b := range buckets {
		bx := plotLeft + i*slotW
		total := b.Count
		bh := total * plotH / maxCount
		if total > 0 && bh < 2 {
			bh = 2
		}
		// Stacked: webhook (neutral) at the base, ai-detect (accent) on top.
		whH := 0
		if total > 0 {
			whH = b.Webhook * bh / total
		}
		aiH := bh - whH
		if whH > 0 {
			fillRect(img, bx, plotBottom-whH, bx+barW, plotBottom, colTextFaint)
		}
		if aiH > 0 {
			fillRect(img, bx, plotBottom-whH-aiH, bx+barW, plotBottom-whH, colAccent)
		}
		if i%labelEvery == 0 {
			drawString(img, fc.tiny, colTextFaint, bx, plotBottom+16, b.Label)
		}
	}
}

// drawSeverityPanel draws a horizontal bar per severity band (critical..
// unknown), each with a label and count. Bounded to ≤5 rows.
func drawSeverityPanel(img *image.RGBA, fc *faces, x, y, w, h int, m core.ReportModel) {
	drawString(img, fc.h2, colTextMuted, x, y+4, "BY SEVERITY")
	fillRect(img, x, y+16, x+w, y+h, colPanel)

	rows := m.BySeverity
	if len(rows) == 0 {
		return
	}
	maxCount := 0
	for _, b := range rows {
		if b.Count > maxCount {
			maxCount = b.Count
		}
	}
	if maxCount < 1 {
		maxCount = 1
	}
	labelW := 82
	countW := 44
	barLeft := x + 14 + labelW
	barMax := (x + w - 14) - countW - barLeft
	if barMax < 1 {
		barMax = 1
	}
	rowTop := y + 34
	rowGap := (h - 44) / len(rows)
	if rowGap < 16 {
		rowGap = 16
	}
	for i, b := range rows {
		ry := rowTop + i*rowGap
		drawString(img, fc.small, colTextMuted, x+14, ry+4, b.Label)
		bw := b.Count * barMax / maxCount
		if b.Count > 0 && bw < 2 {
			bw = 2
		}
		fillRect(img, barLeft, ry-8, barLeft+bw, ry+6, severityColor(b.Label))
		drawString(img, fc.small, colText, x+w-14-countW+8, ry+4, strconv.Itoa(b.Count))
	}
}

// drawTopServices draws the top-N services list (already-redacted labels).
func drawTopServices(img *image.RGBA, fc *faces, x, y, w int, m core.ReportModel) {
	drawString(img, fc.h2, colTextMuted, x, y, "TOP SERVICES")
	ly := y + 26
	if len(m.TopServices) == 0 {
		drawString(img, fc.small, colTextFaint, x, ly, "—")
		return
	}
	for _, s := range m.TopServices {
		count := strconv.Itoa(s.Count)
		cw := textWidth(fc.small, count)
		drawString(img, fc.body, colText, x, ly, truncateToWidth(fc.body, s.Label, w-cw-16))
		drawString(img, fc.small, colTextMuted, x+w-cw, ly, count)
		ly += 26
	}
}

// drawNotable draws the recent high-severity incident list (redacted titles).
func drawNotable(img *image.RGBA, fc *faces, x, y, w int, m core.ReportModel) {
	drawString(img, fc.h2, colTextMuted, x, y, "RECENT CRITICAL / HIGH")
	ly := y + 26
	if len(m.Notable) == 0 {
		drawString(img, fc.small, colTextFaint, x, ly, "—")
		return
	}
	for _, n := range m.Notable {
		dot := severityColor(n.Severity)
		fillRect(img, x, ly-10, x+8, ly-2, dot)
		title := n.Title
		if title == "" {
			title = "Incident"
		}
		drawString(img, fc.body, colText, x+16, ly, truncateToWidth(fc.body, title, w-16))
		ly += 22
		meta := fmtTime(n.CreatedAt)
		if n.Service != "" {
			meta += "  ·  " + n.Service
		}
		drawString(img, fc.tiny, colTextFaint, x+16, ly, truncateToWidth(fc.tiny, meta, w-16))
		ly += 24
	}
}

// worstSeverityColor returns the accent for the highest-priority severity
// band that has any incidents, defaulting to a neutral slate for an empty
// window.
func worstSeverityColor(rows []core.Bucket) color.RGBA {
	for _, b := range rows {
		if b.Count > 0 {
			return severityColor(b.Label)
		}
	}
	return color.RGBA{0x64, 0x74, 0x8b, 0xff}
}

// windowTitle is the human label for a window used in the header/filename.
func windowTitle(window string) string {
	switch window {
	case "24h":
		return "last 24h"
	case "7d":
		return "last 7 days"
	default:
		return "today"
	}
}

// ---------------------------------------------------------------------------
// small drawing helpers
// ---------------------------------------------------------------------------

func fillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	draw.Draw(img, image.Rect(x0, y0, x1, y1), image.NewUniform(c), image.Point{}, draw.Src)
}

func divider(img *image.RGBA, x0, y, x1 int) {
	fillRect(img, x0, y, x1, y+1, colDivider)
}

func drawString(img *image.RGBA, face font.Face, col color.Color, x, y int, s string) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

func textWidth(face font.Face, s string) int {
	return font.MeasureString(face, s).Ceil()
}

// truncateToWidth shortens s with an ellipsis so it fits maxW.
func truncateToWidth(face font.Face, s string, maxW int) string {
	if textWidth(face, s) <= maxW {
		return s
	}
	r := []rune(s)
	for len(r) > 1 {
		r = r[:len(r)-1]
		if textWidth(face, string(r)+"…") <= maxW {
			return string(r) + "…"
		}
	}
	return "…"
}

func fmtTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04")
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
