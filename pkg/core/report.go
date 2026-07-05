package core

import (
	"context"
	"time"
)

// Scrubber is the minimal DLP contract the report path depends on: turn a
// possibly-sensitive string into a safe one. It is satisfied by
// agent.Redactor.Scrub, so the report assembler can scrub every field
// WITHOUT the services/report layers importing the heavier agent package.
// Every text value drawn onto a shared report image is run through a
// Scrubber first — defence-in-depth, because webhook incident Content is
// attacker-influenced input.
type Scrubber interface {
	Scrub(s string) string
}

// Bucket is one labelled tally in an aggregate breakdown — a severity band,
// a trend time-slice, or a top service. Count is the total; AIDetect and
// Webhook carry the per-origin split so the trend timeline can draw a
// stacked bar (AIDetect + Webhook == Count). Labels are already redacted for
// the top-services list (a service name is attacker-influenced webhook
// content); the severity/trend labels are renderer-controlled constants.
type Bucket struct {
	Label    string
	Count    int
	AIDetect int
	Webhook  int
}

// NotableIncident is one row in the "recent high-severity" notable list.
// Every string field is ALREADY redacted by the assembler — the renderer
// draws it verbatim and never reaches back into raw content.
type NotableIncident struct {
	ShortID   string
	Title     string
	Service   string
	Severity  string
	CreatedAt time.Time
}

// ReportModel is the channel-agnostic, ALREADY-REDACTED aggregate summary of
// every incident in a time window. It is assembled by querying the incident
// history over the window (both ai_detect and webhook) and handed to a
// ReportRenderer. Every string field has already passed through a Scrubber,
// so a renderer draws it verbatim and must never reach back into raw incident
// content. No AckURL, token, or config value is ever placed here.
type ReportModel struct {
	// Window identity. All timestamps are UTC for determinism.
	Window      string    // "today" | "24h" | "7d"
	WindowStart time.Time // inclusive
	WindowEnd   time.Time // exclusive (== GeneratedAt for today/24h)
	GeneratedAt time.Time

	// Headline stats.
	Total        int            // incidents in the window
	ByOrigin     map[string]int // ai_detect / webhook counts (primary category axis)
	Resolved     int
	Open         int
	CriticalHigh int // count of critical + high severity incidents

	// Chart series (hand-drawn, no chart lib).
	BySeverity []Bucket // ordered critical..unknown (category breakdown)
	Trend      []Bucket // hourly (today/24h) or daily (7d) counts
	TrendUnit  string   // "hour" | "day" — labels the trend axis

	// Notable lists (bounded, redacted labels).
	TopServices []Bucket          // top-N services by count
	Notable     []NotableIncident // recent high-severity incidents

	// IncludeCharts mirrors the runtime include_chart toggle: when false the
	// renderer omits the trend + severity charts (headline + lists remain).
	IncludeCharts bool

	// Footer mark. The OSS card always carries "Versus Incident"; the
	// enterprise white-label renderer overrides it behind the same seam.
	Footer string
}

// ReportImage is a rendered report: the encoded image bytes plus enough
// metadata for a channel to upload it or an HTTP handler to serve it.
type ReportImage struct {
	Data     []byte // encoded image bytes (PNG)
	MIME     string // "image/png"
	Filename string // e.g. "incidents-today-20260704.png"
	Width    int
	Height   int
}

// ReportRenderer turns a ReportModel into a ReportImage. The OSS build
// wires the default pure-Go PNG card renderer at boot via
// services.SetReportRenderer; an enterprise build may install a branded /
// high-fidelity renderer behind the SAME seam without the API, channel
// delivery, or UI changing. Mirrors the SetAnalyzeAgent seam.
type ReportRenderer interface {
	Render(ctx context.Context, m ReportModel) (*ReportImage, error)
}
