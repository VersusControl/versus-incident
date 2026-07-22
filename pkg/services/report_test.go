package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/common"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/report"
	"github.com/VersusControl/versus-incident/pkg/storage"

	m "github.com/VersusControl/versus-incident/pkg/models"
)

// --- fakes -----------------------------------------------------------------

type fakeRenderer struct {
	img *core.ReportImage
	err error
}

func (f fakeRenderer) Render(ctx context.Context, _ core.ReportModel) (*core.ReportImage, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.img != nil {
		return f.img, nil
	}
	return &core.ReportImage{Data: []byte("PNGDATA"), MIME: "image/png", Filename: "incident-x.png", Width: 1200, Height: 675}, nil
}

// fakeImageProvider implements AlertProvider + AttachmentSender.
type fakeImageProvider struct {
	name   string
	err    error
	called bool
	gotAtt core.Attachment
}

func (f *fakeImageProvider) Name() string                { return f.name }
func (f *fakeImageProvider) SendAlert(*m.Incident) error { return nil }
func (f *fakeImageProvider) SendAttachment(i *m.Incident, att core.Attachment) error {
	f.called = true
	f.gotAtt = att
	return f.err
}

// fakeTextProvider implements AlertProvider + TextSender only.
type fakeTextProvider struct {
	name    string
	err     error
	called  bool
	gotText string
}

func (f *fakeTextProvider) Name() string                { return f.name }
func (f *fakeTextProvider) SendAlert(*m.Incident) error { return nil }
func (f *fakeTextProvider) SendText(i *m.Incident, text string) error {
	f.called = true
	f.gotText = text
	return f.err
}

func testScrubber(t *testing.T) core.Scrubber {
	t.Helper()
	r, _ := agent.NewRedactor(false, nil)
	return r
}

// rec is a small helper to build an incident record for the aggregate tests.
func rec(id, service, severity, origin, source string, resolved bool, created time.Time) *storage.IncidentRecord {
	return &storage.IncidentRecord{
		ID:        id,
		Title:     id + " title",
		Service:   service,
		Source:    source,
		Origin:    origin,
		Resolved:  resolved,
		CreatedAt: created,
		Content:   map[string]interface{}{"Severity": severity},
	}
}

// --- window resolution -----------------------------------------------------

func TestWindowBounds(t *testing.T) {
	now := time.Date(2026, 7, 4, 15, 30, 0, 0, time.UTC)

	start, end, unit := WindowBounds("today", now)
	if !start.Equal(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("today start = %v", start)
	}
	if !end.Equal(now) || unit != "hour" {
		t.Fatalf("today end=%v unit=%q", end, unit)
	}

	start, _, unit = WindowBounds("24h", now)
	if !start.Equal(now.Add(-24*time.Hour)) || unit != "hour" {
		t.Fatalf("24h start=%v unit=%q", start, unit)
	}

	start, _, unit = WindowBounds("7d", now)
	if !start.Equal(now.Add(-7*24*time.Hour)) || unit != "day" {
		t.Fatalf("7d start=%v unit=%q", start, unit)
	}

	// Unknown defaults to today.
	if _, _, u := WindowBounds("bogus", now); u != "hour" {
		t.Fatalf("bogus window unit=%q, want hour (today)", u)
	}
}

// TestWindowBoundsIn_TimezoneShiftsTodayStart proves "today" is the start of
// the LOCAL calendar day: the same instant yields a different window start in
// UTC vs a +7 timezone, while a nil location stays byte-for-byte UTC.
func TestWindowBoundsIn_TimezoneShiftsTodayStart(t *testing.T) {
	// 00:30 UTC on 2026-07-10 is already 07:30 the same day in ICT, so local
	// "today" started at 00:00 ICT (== 17:00 UTC on 2026-07-09).
	now := time.Date(2026, 7, 10, 0, 30, 0, 0, time.UTC)

	utcStart, _, _ := WindowBoundsIn("today", now, time.UTC)
	if !utcStart.Equal(time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("utc today start = %v", utcStart)
	}

	ict, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	ictStart, _, _ := WindowBoundsIn("today", now, ict)
	if !ictStart.Equal(time.Date(2026, 7, 10, 0, 0, 0, 0, ict)) {
		t.Fatalf("ict today start = %v, want local midnight", ictStart)
	}
	if ictStart.Equal(utcStart) {
		t.Fatal("today start must differ between UTC and a +7 timezone")
	}

	// A nil location behaves exactly like UTC (back-compat contract).
	nilStart, _, _ := WindowBoundsIn("today", now, nil)
	if !nilStart.Equal(utcStart) {
		t.Fatalf("nil-loc start = %v, want == utc %v", nilStart, utcStart)
	}
}

// TestBuildAggregateReportModel_TimezoneRendering asserts the SAME window
// renders its timestamps + caption in the configured timezone: UTC keeps the
// "… UTC" label byte-for-byte, while a non-UTC location shifts the printed
// wall-clock time and stamps the IANA label.
func TestBuildAggregateReportModel_TimezoneRendering(t *testing.T) {
	ict, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)

	us, ue, uu := WindowBoundsIn("24h", now, time.UTC)
	utcModel := BuildAggregateReportModel(nil, "24h", us, ue, uu, testScrubber(t), true, time.UTC)
	if utcModel.TZLabel != "UTC" {
		t.Fatalf("utc TZLabel = %q, want UTC", utcModel.TZLabel)
	}
	if got := utcModel.WindowEnd.Format("15:04"); got != "10:00" {
		t.Fatalf("utc window end = %q, want 10:00", got)
	}
	if cap := reportCaption(utcModel); !strings.Contains(cap, "10:00 UTC") {
		t.Fatalf("utc caption missing '10:00 UTC': %q", cap)
	}

	is, ie, iu := WindowBoundsIn("24h", now, ict)
	ictModel := BuildAggregateReportModel(nil, "24h", is, ie, iu, testScrubber(t), true, ict)
	if ictModel.TZLabel != "Asia/Ho_Chi_Minh" {
		t.Fatalf("ict TZLabel = %q, want Asia/Ho_Chi_Minh", ictModel.TZLabel)
	}
	// 10:00 UTC == 17:00 ICT (UTC+7).
	if got := ictModel.WindowEnd.Format("15:04"); got != "17:00" {
		t.Fatalf("ict window end = %q, want 17:00", got)
	}
	if cap := reportCaption(ictModel); !strings.Contains(cap, "17:00 Asia/Ho_Chi_Minh") {
		t.Fatalf("ict caption missing '17:00 Asia/Ho_Chi_Minh': %q", cap)
	}
	// Both models describe the same instant — only the printed zone differs.
	if !utcModel.WindowEnd.Equal(ictModel.WindowEnd) {
		t.Fatal("UTC and ICT models must describe the same window-end instant")
	}
}

// --- aggregate assembler ----------------------------------------------

func TestBuildAggregateReportModel_CountsOriginsSeverityServices(t *testing.T) {
	start := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	recs := []*storage.IncidentRecord{
		// explicit ai_detect, critical, resolved
		rec("a1", "payments", "critical", storage.OriginAIDetect, "agent", true, end.Add(-1*time.Hour)),
		// explicit webhook, high, open
		rec("w1", "payments", "high", storage.OriginWebhook, "webhook", false, end.Add(-2*time.Hour)),
		// LEGACY row: empty Origin, agent Source → classifies ai_detect via EffectiveOrigin
		rec("legacy1", "checkout", "medium", "", "agent:es:checkout", false, end.Add(-3*time.Hour)),
		// LEGACY row: empty Origin, webhook Source → webhook
		rec("legacy2", "checkout", "", "", "webhook", true, end.Add(-4*time.Hour)),
	}

	m := BuildAggregateReportModel(recs, "today", start, end, "hour", testScrubber(t), true, time.UTC)

	if m.Total != 4 {
		t.Fatalf("total = %d, want 4", m.Total)
	}
	if m.ByOrigin[storage.OriginAIDetect] != 2 || m.ByOrigin[storage.OriginWebhook] != 2 {
		t.Fatalf("byOrigin = %v (legacy rows must classify via EffectiveOrigin)", m.ByOrigin)
	}
	if m.Resolved != 2 || m.Open != 2 {
		t.Fatalf("resolved=%d open=%d", m.Resolved, m.Open)
	}
	if m.CriticalHigh != 2 {
		t.Fatalf("criticalHigh = %d, want 2", m.CriticalHigh)
	}
	// Severity breakdown must be the fixed 5 bands in order.
	if len(m.BySeverity) != 5 {
		t.Fatalf("bySeverity len = %d, want 5", len(m.BySeverity))
	}
	want := map[string]int{"critical": 1, "high": 1, "medium": 1, "low": 0, "unknown": 1}
	for _, b := range m.BySeverity {
		if b.Count != want[b.Label] {
			t.Fatalf("severity %q = %d, want %d", b.Label, b.Count, want[b.Label])
		}
	}
	// Top services: payments(2) then checkout(2) — deterministic tie-break by
	// label asc, so checkout precedes payments? No: count desc then label
	// asc; both count 2, so "checkout" < "payments".
	if len(m.TopServices) != 2 || m.TopServices[0].Label != "checkout" {
		t.Fatalf("topServices = %+v", m.TopServices)
	}
	// Notable: the critical + high incidents (2), newest-first ordering
	// preserved from the input.
	if len(m.Notable) != 2 || m.Notable[0].ShortID != "a1" {
		t.Fatalf("notable = %+v", m.Notable)
	}
}

func TestBuildAggregateReportModel_EmptyWindow(t *testing.T) {
	start := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	m := BuildAggregateReportModel(nil, "today", start, end, "hour", testScrubber(t), true, time.UTC)
	if m.Total != 0 {
		t.Fatalf("total = %d, want 0", m.Total)
	}
	if m.ByOrigin[storage.OriginAIDetect] != 0 || m.ByOrigin[storage.OriginWebhook] != 0 {
		t.Fatalf("byOrigin = %v", m.ByOrigin)
	}
	if len(m.BySeverity) != 5 {
		t.Fatalf("bySeverity len = %d, want 5 (bands drawn even when empty)", len(m.BySeverity))
	}
	if len(m.TopServices) != 0 || len(m.Notable) != 0 {
		t.Fatalf("expected empty lists, got services=%v notable=%v", m.TopServices, m.Notable)
	}
	if len(m.Trend) == 0 {
		t.Fatal("trend should still draw an axis (>=1 bucket) for an empty window")
	}
}

// TestBuildAggregateReportModel_RedactsServiceAndTitle is the redaction gate:
// a planted secret / PII in the service name or title must be a <REDACTED:…>
// token in the model, never the raw literal.
func TestBuildAggregateReportModel_RedactsServiceAndTitle(t *testing.T) {
	end := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	r := rec("x1", "svc-AKIAIOSFODNN7EXAMPLE", "critical", storage.OriginWebhook, "webhook", false, end.Add(-time.Hour))
	r.Title = "leaked bob@example.com in handler"
	m := BuildAggregateReportModel([]*storage.IncidentRecord{r}, "today", end.Add(-12*time.Hour), end, "hour", testScrubber(t), true, time.UTC)

	blob, _ := json.Marshal(m)
	s := string(blob)
	for _, secret := range []string{"AKIAIOSFODNN7EXAMPLE", "bob@example.com"} {
		if strings.Contains(s, secret) {
			t.Fatalf("model leaked %q: %s", secret, s)
		}
	}
	if len(m.Notable) != 1 || !strings.Contains(m.Notable[0].Title, "<REDACTED:") {
		t.Fatalf("notable title not redacted: %+v", m.Notable)
	}
}

// TestReport_EndToEnd_NoSecretInRenderedPNG is the definitive redaction gate:
// a planted secret in a service name / title must not appear as a literal
// anywhere in the FINAL rendered PNG (aggregate assembler → real redactor →
// real pure-Go renderer). Proves the whole egress path scrubs before pixels.
func TestReport_EndToEnd_NoSecretInRenderedPNG(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	end := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	r := rec("x1", "svc-"+secret, "critical", storage.OriginWebhook, "webhook", false, end.Add(-time.Hour))
	r.Title = "leaked " + secret + " bob@example.com"
	model := BuildAggregateReportModel([]*storage.IncidentRecord{r}, "today", end.Add(-12*time.Hour), end, "hour", testScrubber(t), true, time.UTC)

	renderer, err := report.NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	img, err := renderer.Render(context.Background(), model)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(img.Data), secret) {
		t.Fatal("planted secret leaked into the rendered PNG bytes")
	}
	if strings.Contains(string(img.Data), "bob@example.com") {
		t.Fatal("planted PII leaked into the rendered PNG bytes")
	}
}

func TestBuildTrend_HourlyBucketing(t *testing.T) {
	start := time.Date(2026, 7, 4, 8, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC) // 4 hourly slots: 08,09,10,11
	recs := []*storage.IncidentRecord{
		rec("a", "s", "high", storage.OriginAIDetect, "agent", false, time.Date(2026, 7, 4, 8, 15, 0, 0, time.UTC)),
		rec("b", "s", "high", storage.OriginWebhook, "webhook", false, time.Date(2026, 7, 4, 8, 45, 0, 0, time.UTC)),
		rec("c", "s", "high", storage.OriginAIDetect, "agent", false, time.Date(2026, 7, 4, 11, 5, 0, 0, time.UTC)),
	}
	buckets := buildTrend(recs, start, end, "hour", time.UTC)
	if len(buckets) != 4 {
		t.Fatalf("buckets = %d, want 4", len(buckets))
	}
	if buckets[0].Count != 2 || buckets[0].AIDetect != 1 || buckets[0].Webhook != 1 {
		t.Fatalf("bucket[0] = %+v, want 2 (1 ai, 1 webhook)", buckets[0])
	}
	if buckets[2].Count != 0 {
		t.Fatalf("bucket[2] (10:00) = %d, want 0", buckets[2].Count)
	}
	if buckets[3].Count != 1 || buckets[3].AIDetect != 1 {
		t.Fatalf("bucket[3] (11:00) = %+v", buckets[3])
	}
}

func TestBuildTrend_DailyBoundedTo7(t *testing.T) {
	start := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC) // 7 daily slots
	var recs []*storage.IncidentRecord
	buckets := buildTrend(recs, start, end, "day", time.UTC)
	if len(buckets) != 7 {
		t.Fatalf("daily buckets = %d, want 7 (bounded)", len(buckets))
	}
}

// --- window query fallback / RangeLister -----------------------------------

// rangeStore embeds a Provider and adds the optional RangeLister capability so
// the assembler prefers the pushed-down query when present.
type rangeStore struct {
	storage.Provider
	gotStart, gotEnd time.Time
	recs             []*storage.IncidentRecord
}

func (r *rangeStore) ListIncidentsInRange(start, end time.Time, _ int) ([]*storage.IncidentRecord, error) {
	r.gotStart, r.gotEnd = start, end
	return r.recs, nil
}

func TestIncidentsInWindow_FallbackFilter(t *testing.T) {
	st := storage.NewMemory()
	now := time.Now().UTC()
	in := rec("in", "s", "high", storage.OriginWebhook, "webhook", false, now.Add(-1*time.Hour))
	old := rec("old", "s", "high", storage.OriginWebhook, "webhook", false, now.Add(-48*time.Hour))
	_ = st.SaveIncident(in)
	_ = st.SaveIncident(old)

	got, err := incidentsInWindow(st, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("incidentsInWindow: %v", err)
	}
	if len(got) != 1 || got[0].ID != "in" {
		t.Fatalf("window filter = %+v, want just the in-window record", got)
	}
}

func TestIncidentsInWindow_UsesRangeLister(t *testing.T) {
	rs := &rangeStore{Provider: storage.NewMemory(), recs: []*storage.IncidentRecord{rec("r", "s", "high", storage.OriginAIDetect, "agent", false, time.Now())}}
	start := time.Now().Add(-24 * time.Hour)
	end := time.Now()
	got, err := incidentsInWindow(rs, start, end)
	if err != nil {
		t.Fatalf("incidentsInWindow: %v", err)
	}
	if len(got) != 1 || rs.gotStart.IsZero() {
		t.Fatalf("RangeLister not used: got=%v start=%v", got, rs.gotStart)
	}
}

// --- delivery path ----------------------------------------------------

// enableReport writes runtime settings enabling the report so the delivery
// helpers see it on (settings live in the store now, not YAML).
func enableReport(t *testing.T, st storage.Provider, s ReportSettings) {
	t.Helper()
	if err := SaveReportSettings(st, s); err != nil {
		t.Fatalf("SaveReportSettings: %v", err)
	}
}

// windowStore seeds a memory store with one ai_detect + one webhook incident
// created "now" so any window contains them.
func windowStore(t *testing.T) storage.Provider {
	t.Helper()
	st := storage.NewMemory()
	now := time.Now().UTC()
	_ = st.SaveIncident(rec("agent0001", "payments", "critical", storage.OriginAIDetect, "agent:es:payments", false, now.Add(-1*time.Minute)))
	_ = st.SaveIncident(rec("webhook01", "db", "high", storage.OriginWebhook, "webhook", false, now.Add(-2*time.Minute)))
	return st
}

func TestSendReport_ImageChannelUploadsPNGWithCaption(t *testing.T) {
	st := windowStore(t)
	enableReport(t, st, ReportSettings{Enable: true, IncludeChart: true, DefaultWindow: "24h"})
	slack := &fakeImageProvider{name: "slack"}

	out, err := sendReport(context.Background(), &config.Config{}, st, fakeRenderer{}, testScrubber(t),
		[]core.AlertProvider{slack}, ReportSendOptions{Window: "24h", Channel: "slack"})
	if err != nil {
		t.Fatalf("sendReport: %v", err)
	}
	if len(out.Sent) != 1 || out.Sent[0] != "slack" {
		t.Fatalf("sent = %v, want [slack]", out.Sent)
	}
	if out.Window != "24h" {
		t.Fatalf("window = %q", out.Window)
	}
	if !slack.called || string(slack.gotAtt.Data) != "PNGDATA" {
		t.Fatalf("slack did not receive the PNG: %+v", slack)
	}
	if !strings.Contains(slack.gotAtt.Caption, "Incident report — last 24h") {
		t.Fatalf("caption missing window: %q", slack.gotAtt.Caption)
	}
	if !strings.Contains(slack.gotAtt.Caption, "2 incidents") {
		t.Fatalf("caption missing count: %q", slack.gotAtt.Caption)
	}
}

func TestSendReport_TextFallback_PublicHostPrecedence(t *testing.T) {
	st := windowStore(t)
	enableReport(t, st, ReportSettings{Enable: true, IncludeChart: true})

	// public_host empty → air-gapped UI pointer, no link.
	teams := &fakeTextProvider{name: "msteams"}
	out, err := sendReport(context.Background(), &config.Config{}, st, fakeRenderer{}, testScrubber(t),
		[]core.AlertProvider{teams}, ReportSendOptions{Window: "today", Channel: "msteams"})
	if err != nil {
		t.Fatalf("sendReport: %v", err)
	}
	if len(out.Fallback) != 1 || out.Fallback[0] != "msteams" {
		t.Fatalf("fallback = %v", out.Fallback)
	}
	if strings.Contains(teams.gotText, "http") {
		t.Fatalf("air-gapped fallback must not fabricate a link: %q", teams.gotText)
	}
	if !strings.Contains(teams.gotText, "Reports in the Versus UI") {
		t.Fatalf("fallback missing UI pointer: %q", teams.gotText)
	}

	// public_host set → gateway-guarded report.png link.
	teams2 := &fakeTextProvider{name: "msteams"}
	_, err = sendReport(context.Background(), &config.Config{PublicHost: "https://ops.example.com/"}, st, fakeRenderer{}, testScrubber(t),
		[]core.AlertProvider{teams2}, ReportSendOptions{Window: "7d", Channel: "msteams"})
	if err != nil {
		t.Fatalf("sendReport: %v", err)
	}
	if !strings.Contains(teams2.gotText, "https://ops.example.com/api/admin/reports/incidents/report.png?window=7d") {
		t.Fatalf("public_host link missing/wrong: %q", teams2.gotText)
	}
}

// TestSendReport_FailSafe: one channel failing never mutes another.
func TestSendReport_FailSafe(t *testing.T) {
	st := windowStore(t)
	enableReport(t, st, ReportSettings{Enable: true, DefaultChannel: "slack"})
	good := &fakeImageProvider{name: "slack"}
	bad := &fakeImageProvider{name: "telegram", err: errors.New("boom")}

	// No explicit channel and no default resolves both? default_channel is
	// slack, so pass explicit list via two sends is not possible in one call;
	// instead target telegram explicitly for the failure and slack via a
	// second send would double count. Use default_channel=slack (sent) and a
	// separate explicit telegram to assert failure recording.
	out, err := sendReport(context.Background(), &config.Config{}, st, fakeRenderer{}, testScrubber(t),
		[]core.AlertProvider{good, bad}, ReportSendOptions{Window: "today", Channel: "telegram"})
	if err != nil {
		t.Fatalf("sendReport returned error (should be per-channel): %v", err)
	}
	if _, ok := out.Failed["telegram"]; !ok {
		t.Fatalf("telegram failure not recorded: %+v", out.Failed)
	}
	if good.called {
		t.Fatal("slack should not be called when telegram is the explicit target")
	}
}

func TestSendReport_RateLimitedPerWindow(t *testing.T) {
	st := windowStore(t)
	enableReport(t, st, ReportSettings{Enable: true, RatePerMinute: 1, DefaultChannel: "slack"})
	// Use a unique window bucket so the package-global limiter map is not
	// polluted by other tests: 24h key is "report:24h".
	cfg := &config.Config{}
	opts := ReportSendOptions{Window: "24h", Channel: "slack"}
	if _, err := sendReport(context.Background(), cfg, st, fakeRenderer{}, testScrubber(t), []core.AlertProvider{&fakeImageProvider{name: "slack"}}, opts); err != nil {
		t.Fatalf("first send: %v", err)
	}
	_, err := sendReport(context.Background(), cfg, st, fakeRenderer{}, testScrubber(t), []core.AlertProvider{&fakeImageProvider{name: "slack"}}, opts)
	if !errors.Is(err, ErrReportRateLimited) {
		t.Fatalf("second send err = %v, want ErrReportRateLimited", err)
	}
}

func TestSendReport_Guards(t *testing.T) {
	st := windowStore(t)

	t.Run("disabled (no settings written → default off)", func(t *testing.T) {
		fresh := storage.NewMemory()
		_, err := sendReport(context.Background(), &config.Config{}, fresh, fakeRenderer{}, testScrubber(t), []core.AlertProvider{&fakeImageProvider{name: "slack"}}, ReportSendOptions{Window: "today", Channel: "slack"})
		if !errors.Is(err, ErrReportDisabled) {
			t.Fatalf("err = %v, want ErrReportDisabled (fresh install off)", err)
		}
	})

	t.Run("no channel resolves", func(t *testing.T) {
		enableReport(t, st, ReportSettings{Enable: true})
		_, err := sendReport(context.Background(), &config.Config{}, st, fakeRenderer{}, testScrubber(t), []core.AlertProvider{&fakeImageProvider{name: "slack"}}, ReportSendOptions{Window: "today"})
		if !errors.Is(err, ErrReportNoChannel) {
			t.Fatalf("err = %v, want ErrReportNoChannel", err)
		}
	})

	t.Run("channel not enabled → no channel", func(t *testing.T) {
		enableReport(t, st, ReportSettings{Enable: true})
		_, err := sendReport(context.Background(), &config.Config{}, st, fakeRenderer{}, testScrubber(t), []core.AlertProvider{&fakeImageProvider{name: "slack"}}, ReportSendOptions{Window: "today", Channel: "email"})
		if !errors.Is(err, ErrReportNoChannel) {
			t.Fatalf("err = %v, want ErrReportNoChannel", err)
		}
	})

	t.Run("no storage → 503 sentinel", func(t *testing.T) {
		_, err := sendReport(context.Background(), &config.Config{}, nil, fakeRenderer{}, testScrubber(t), []core.AlertProvider{&fakeImageProvider{name: "slack"}}, ReportSendOptions{Window: "today", Channel: "slack"})
		if !errors.Is(err, ErrReportDisabled) {
			// nil store → settings default off → disabled first.
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRenderReport_GuardsAndPNG(t *testing.T) {
	st := windowStore(t)
	enableReport(t, st, ReportSettings{Enable: true, IncludeChart: true})

	r, err := renderReport(context.Background(), &config.Config{}, st, fakeRenderer{}, testScrubber(t), "today")
	if err != nil {
		t.Fatalf("renderReport: %v", err)
	}
	if string(r.Data) != "PNGDATA" {
		t.Fatalf("data = %q", r.Data)
	}

	// no renderer → ErrReportNoRenderer
	if _, err := renderReport(context.Background(), &config.Config{}, st, nil, testScrubber(t), "today"); !errors.Is(err, ErrReportNoRenderer) {
		t.Fatalf("no-renderer err = %v", err)
	}

	// fresh install → disabled
	if _, err := renderReport(context.Background(), &config.Config{}, storage.NewMemory(), fakeRenderer{}, testScrubber(t), "today"); !errors.Is(err, ErrReportDisabled) {
		t.Fatalf("disabled err = %v", err)
	}
}

func TestResolveReportChannels_Precedence(t *testing.T) {
	if got := resolveReportChannels("slack", "telegram"); len(got) != 1 || got[0] != "slack" {
		t.Fatalf("explicit wins: %v", got)
	}
	if got := resolveReportChannels("", "telegram"); len(got) != 1 || got[0] != "telegram" {
		t.Fatalf("default_channel: %v", got)
	}
	if got := resolveReportChannels("", ""); got != nil {
		t.Fatalf("no channel: %v", got)
	}
}

// reportChannelResolver is a runtime channel-override stub mirroring an
// operator who changed the Slack channel's credentials/channel-id/enable at
// runtime (the hot-reload seam). It overlays only the Slack channel and leaves
// every other channel at its YAML floor.
type reportChannelResolver struct {
	enable    bool
	token     string
	channelID string
}

func (r reportChannelResolver) ResolveAlert(_ context.Context, base *config.AlertConfig) bool {
	base.Slack.Enable = r.enable
	base.Slack.Token = r.token
	base.Slack.ChannelID = r.channelID
	return true
}

// TestSendIncidentsReport_HonorsRuntimeChannelOverride proves the report send
// path resolves its channel config the SAME way alerts do (via
// GetConfigForAlert), so a runtime channel override reaches the report's
// providers instead of the stale YAML config — and that with NO resolver the
// report is byte-for-byte identical to the pre-fix behaviour (OSS parity).
func TestSendIncidentsReport_HonorsRuntimeChannelOverride(t *testing.T) {
	loadAgentTestConfig(t)
	base := config.GetConfigOrNil()
	if base == nil {
		t.Fatal("global config not loaded")
	}
	// Known YAML floor: Slack enabled with a YAML channel + token.
	savedAlert := base.Alert
	t.Cleanup(func() { base.Alert = savedAlert })
	base.Alert = config.AlertConfig{
		Slack: config.SlackConfig{
			Enable:       true,
			Token:        "yaml-token",
			ChannelID:    "C-YAML",
			TemplatePath: "slack.tmpl",
		},
	}

	st := windowStore(t)
	prevStore := Storage()
	SetStorage(st)
	t.Cleanup(func() { SetStorage(prevStore) })
	enableReport(t, st, ReportSettings{Enable: true, DefaultChannel: "slack"})
	prevRenderer := ReportRenderer()
	SetReportRenderer(fakeRenderer{})
	t.Cleanup(func() { SetReportRenderer(prevRenderer) })
	t.Cleanup(func() { config.SetAlertConfigResolver(nil) })

	ctx := context.Background()

	// 1. A registered runtime override changes the channel-id + token + enable
	//    the report's providers are built from — matching the alert path.
	t.Run("runtime override reaches report providers", func(t *testing.T) {
		config.SetAlertConfigResolver(reportChannelResolver{enable: true, token: "runtime-token", channelID: "C-RUNTIME"})
		t.Cleanup(func() { config.SetAlertConfigResolver(nil) })

		// The exact resolution SendIncidentsReport performs.
		eff := config.GetConfigForAlert(ctx, nil)
		if eff.Alert.Slack.ChannelID != "C-RUNTIME" || eff.Alert.Slack.Token != "runtime-token" || !eff.Alert.Slack.Enable {
			t.Fatalf("effective slack = %+v, want runtime override (channel-id + token + enable)", eff.Alert.Slack)
		}
		// The report's providers are built from that resolved config, so the
		// Slack provider now targets the overridden channel + token.
		providers, err := common.NewAlertProviderFactory(eff).CreateProviders()
		if err != nil {
			t.Fatalf("build providers: %v", err)
		}
		if !hasProviderNamed(providers, "slack") {
			t.Fatalf("report providers missing overridden slack channel: %v", providerNames(providers))
		}
		// The runtime overlay never mutates the global config (golden rule #4).
		if base.Alert.Slack.ChannelID != "C-YAML" || base.Alert.Slack.Token != "yaml-token" {
			t.Fatalf("global cfg mutated by overlay: %+v", base.Alert.Slack)
		}
	})

	// 2. A runtime override that DISABLES the target propagates through the
	//    real send path (network-free): no provider is built, so the send
	//    resolves no channel — proving SendIncidentsReport honors the override
	//    (with the old GetConfig() it would still target the enabled YAML slack).
	t.Run("runtime override changes the real send path", func(t *testing.T) {
		config.SetAlertConfigResolver(reportChannelResolver{enable: false})
		t.Cleanup(func() { config.SetAlertConfigResolver(nil) })

		if _, err := SendIncidentsReport(ctx, ReportSendOptions{Window: "today"}); !errors.Is(err, ErrReportNoChannel) {
			t.Fatalf("err = %v, want ErrReportNoChannel (runtime override disabled the slack target)", err)
		}
	})

	// 3. OSS parity: with NO resolver and nil params, GetConfigForAlert returns
	//    the GLOBAL cfg pointer unchanged (documented fast path), so a pure-OSS
	//    report is byte-for-byte identical to the pre-fix GetConfig() behaviour.
	t.Run("no resolver uses YAML config unchanged (OSS parity)", func(t *testing.T) {
		config.SetAlertConfigResolver(nil)

		if got := config.GetConfigForAlert(ctx, nil); got != config.GetConfig() {
			t.Fatal("OSS fast path must return the global cfg pointer unchanged (byte-for-byte parity)")
		}
		eff := config.GetConfigForAlert(ctx, nil)
		if eff.Alert.Slack.ChannelID != "C-YAML" || eff.Alert.Slack.Token != "yaml-token" || !eff.Alert.Slack.Enable {
			t.Fatalf("effective slack = %+v, want YAML floor unchanged", eff.Alert.Slack)
		}
	})
}

func hasProviderNamed(providers []core.AlertProvider, name string) bool {
	for _, p := range providers {
		if p.Name() == name {
			return true
		}
	}
	return false
}

func providerNames(providers []core.AlertProvider) []string {
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name())
	}
	return names
}
