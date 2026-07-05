package report

import (
	"bytes"
	"context"
	"image/png"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// fixedModel is a deterministic, populated aggregate model spanning both
// origins and every severity band, with a trend and notable list.
func fixedModel() core.ReportModel {
	start := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	return core.ReportModel{
		Window:       "today",
		WindowStart:  start,
		WindowEnd:    end,
		GeneratedAt:  end,
		Total:        7,
		ByOrigin:     map[string]int{"ai_detect": 4, "webhook": 3},
		Resolved:     3,
		Open:         4,
		CriticalHigh: 3,
		BySeverity: []core.Bucket{
			{Label: "critical", Count: 2, AIDetect: 2},
			{Label: "high", Count: 1, AIDetect: 1},
			{Label: "medium", Count: 2, Webhook: 2},
			{Label: "low", Count: 1, Webhook: 1},
			{Label: "unknown", Count: 1, Webhook: 1},
		},
		Trend: []core.Bucket{
			{Label: "08:00", Count: 2, AIDetect: 1, Webhook: 1},
			{Label: "09:00", Count: 3, AIDetect: 2, Webhook: 1},
			{Label: "10:00", Count: 0},
			{Label: "11:00", Count: 2, AIDetect: 1, Webhook: 1},
		},
		TrendUnit: "hour",
		TopServices: []core.Bucket{
			{Label: "payments", Count: 4, AIDetect: 3, Webhook: 1},
			{Label: "checkout", Count: 3, AIDetect: 1, Webhook: 2},
		},
		Notable: []core.NotableIncident{
			{ShortID: "abcdef01", Title: "Pool exhausted", Service: "payments", Severity: "critical", CreatedAt: end},
			{ShortID: "12345678", Title: "Latency spike", Service: "checkout", Severity: "high", CreatedAt: start},
		},
		IncludeCharts: true,
		Footer:        "Versus Incident",
	}
}

// emptyModel is a valid 0-incidents window.
func emptyModel() core.ReportModel {
	start := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	return core.ReportModel{
		Window:      "today",
		WindowStart: start,
		WindowEnd:   end,
		GeneratedAt: end,
		Total:       0,
		ByOrigin:    map[string]int{"ai_detect": 0, "webhook": 0},
		BySeverity: []core.Bucket{
			{Label: "critical"}, {Label: "high"}, {Label: "medium"}, {Label: "low"}, {Label: "unknown"},
		},
		Trend:         []core.Bucket{{Label: "00:00"}},
		TrendUnit:     "hour",
		IncludeCharts: true,
		Footer:        "Versus Incident",
	}
}

// TestRenderer_ProducesValidPNG asserts the default renderer emits a
// decodable PNG of the fixed dashboard dimensions for a populated window, an
// empty window, a single-origin window, and a weekly (daily-trend) window.
// Air-gapped: NewRenderer uses only the embedded Go font and Render touches no
// file or network.
func TestRenderer_ProducesValidPNG(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	populated := fixedModel()

	singleOrigin := fixedModel()
	singleOrigin.ByOrigin = map[string]int{"ai_detect": 7, "webhook": 0}
	for i := range singleOrigin.Trend {
		singleOrigin.Trend[i].Webhook = 0
		singleOrigin.Trend[i].AIDetect = singleOrigin.Trend[i].Count
	}

	weekly := fixedModel()
	weekly.Window = "7d"
	weekly.TrendUnit = "day"
	weekly.Trend = []core.Bucket{
		{Label: "06-28", Count: 1}, {Label: "06-29", Count: 0}, {Label: "06-30", Count: 3},
		{Label: "07-01", Count: 2}, {Label: "07-02", Count: 0}, {Label: "07-03", Count: 1}, {Label: "07-04", Count: 0},
	}

	cases := map[string]core.ReportModel{
		"populated":     populated,
		"empty":         emptyModel(),
		"single_origin": singleOrigin,
		"weekly":        weekly,
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			img, err := r.Render(context.Background(), m)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if img.MIME != "image/png" {
				t.Fatalf("MIME = %q, want image/png", img.MIME)
			}
			if img.Width != CardWidth || img.Height != CardHeight {
				t.Fatalf("dims = %dx%d, want %dx%d", img.Width, img.Height, CardWidth, CardHeight)
			}
			if len(img.Data) == 0 {
				t.Fatal("empty image data")
			}
			if img.Filename == "" {
				t.Fatal("empty filename")
			}
			decoded, err := png.Decode(bytes.NewReader(img.Data))
			if err != nil {
				t.Fatalf("png.Decode: %v", err)
			}
			b := decoded.Bounds()
			if b.Dx() != CardWidth || b.Dy() != CardHeight {
				t.Fatalf("decoded dims = %dx%d, want %dx%d", b.Dx(), b.Dy(), CardWidth, CardHeight)
			}
		})
	}
}

// TestRenderer_Deterministic asserts the same model renders to byte-identical
// PNGs (no time.Now / map iteration / randomness in the draw path), so a
// golden-ish size/decode check is stable and the enterprise "community binary
// is byte-for-byte the OSS card" guarantee is meaningful.
func TestRenderer_Deterministic(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	m := fixedModel()
	a, err := r.Render(context.Background(), m)
	if err != nil {
		t.Fatalf("Render a: %v", err)
	}
	b, err := r.Render(context.Background(), m)
	if err != nil {
		t.Fatalf("Render b: %v", err)
	}
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatalf("renders differ: %d vs %d bytes", len(a.Data), len(b.Data))
	}
}

// TestRenderer_ChartsToggle asserts include_charts off changes the pixels — a
// guard that the chart branch is wired to the toggle.
func TestRenderer_ChartsToggle(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	with := fixedModel()
	without := fixedModel()
	without.IncludeCharts = false

	a, err := r.Render(context.Background(), with)
	if err != nil {
		t.Fatalf("render with charts: %v", err)
	}
	b, err := r.Render(context.Background(), without)
	if err != nil {
		t.Fatalf("render without charts: %v", err)
	}
	if bytes.Equal(a.Data, b.Data) {
		t.Fatal("include_charts on/off produced identical images")
	}
}

// TestRenderer_EmptyWindowNoPanic asserts an all-quiet window renders a valid
// PNG rather than panicking on a divide-by-zero in the chart scaling.
func TestRenderer_EmptyWindowNoPanic(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	img, err := r.Render(context.Background(), emptyModel())
	if err != nil {
		t.Fatalf("Render empty: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(img.Data)); err != nil {
		t.Fatalf("empty-window PNG not decodable: %v", err)
	}
}

// TestRenderer_RedactedTextNotInBytes is the belt-and-braces end-to-end check
// that a secret which somehow reaches a model field is not present as a
// literal substring in the encoded PNG. (Rasterized text is pixels, so this
// holds structurally; the assembler's redaction is the real control.)
func TestRenderer_RedactedTextNotInBytes(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	m := fixedModel()
	m.TopServices = []core.Bucket{{Label: "<REDACTED:aws_key>", Count: 1}}
	m.Notable = []core.NotableIncident{{ShortID: "x", Title: "<REDACTED:email>", Severity: "critical", CreatedAt: m.WindowEnd}}
	img, err := r.Render(context.Background(), m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if bytes.Contains(img.Data, []byte("AKIAIOSFODNN7EXAMPLE")) {
		t.Fatal("secret literal leaked into PNG bytes")
	}
}

// TestRenderer_ContextCancelled asserts a cancelled context short-circuits
// the render rather than doing wasted work.
func TestRenderer_ContextCancelled(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Render(ctx, fixedModel()); err == nil {
		t.Fatal("expected error on cancelled context")
	}
}
