package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// fakeReader is a scripted SignalReader for tests.
type fakeReader struct {
	names   []string
	byName  map[string][]core.Signal
	gotConn []struct {
		source string
		since  time.Time
	}
}

func (f *fakeReader) Sources() []string { return f.names }

func (f *fakeReader) Pull(_ context.Context, source string, since time.Time) ([]core.Signal, error) {
	f.gotConn = append(f.gotConn, struct {
		source string
		since  time.Time
	}{source, since})
	return f.byName[source], nil
}

// fakeRedactor replaces a literal secret marker so the test can assert
// scrubbing happened without importing pkg/agent (which would form an
// import cycle with this package).
type fakeRedactor struct{}

func (fakeRedactor) Scrub(s string) string {
	return strings.ReplaceAll(s, "password=hunter2", "password=[redacted]")
}

func mustArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

func TestRelatedLogs_WindowAndLimitClamp(t *testing.T) {
	now := time.Now().UTC()
	sigs := make([]core.Signal, 0, 300)
	for i := 0; i < 300; i++ {
		sigs = append(sigs, core.Signal{
			Source:    "file:app",
			Timestamp: now.Add(-time.Duration(i) * time.Second),
			Message:   "line",
		})
	}
	r := &fakeReader{
		names:  []string{"file:app"},
		byName: map[string][]core.Signal{"file:app": sigs},
	}
	tool := RelatedLogs{Reader: r, Redactor: fakeRedactor{}}

	// window over cap → clamp to 1440; limit over cap → clamp to 200.
	res, err := tool.Invoke(context.Background(), mustArgs(t, relatedLogsArgs{
		WindowMinutes: 999999,
		Limit:         9999,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := res.Data["window_minutes"]; got != relatedLogsMaxWindow {
		t.Errorf("window_minutes = %v, want %d", got, relatedLogsMaxWindow)
	}
	logs := res.Data["logs"].([]relatedLogLine)
	if len(logs) != relatedLogsMaxLimit {
		t.Errorf("len(logs) = %d, want %d (limit cap)", len(logs), relatedLogsMaxLimit)
	}
	if !res.Found {
		t.Error("Found = false, want true")
	}
	// since must be window before now.
	if len(r.gotConn) != 1 {
		t.Fatalf("Pull called %d times, want 1", len(r.gotConn))
	}
	wantSince := now.Add(-time.Duration(relatedLogsMaxWindow) * time.Minute)
	if diff := r.gotConn[0].since.Sub(wantSince); diff > time.Minute || diff < -time.Minute {
		t.Errorf("since = %v, want ~%v", r.gotConn[0].since, wantSince)
	}
}

func TestRelatedLogs_NewestFirst(t *testing.T) {
	now := time.Now().UTC()
	r := &fakeReader{
		names: []string{"file:app"},
		byName: map[string][]core.Signal{"file:app": {
			{Source: "file:app", Timestamp: now.Add(-3 * time.Minute), Message: "old"},
			{Source: "file:app", Timestamp: now.Add(-1 * time.Minute), Message: "new"},
			{Source: "file:app", Timestamp: now.Add(-2 * time.Minute), Message: "mid"},
		}},
	}
	tool := RelatedLogs{Reader: r}
	res, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	logs := res.Data["logs"].([]relatedLogLine)
	if len(logs) != 3 {
		t.Fatalf("len(logs) = %d, want 3", len(logs))
	}
	if logs[0].Message != "new" || logs[2].Message != "old" {
		t.Errorf("order = %q..%q, want new..old", logs[0].Message, logs[2].Message)
	}
}

func TestRelatedLogs_RedactionApplied(t *testing.T) {
	now := time.Now().UTC()
	r := &fakeReader{
		names: []string{"file:app"},
		byName: map[string][]core.Signal{"file:app": {
			{Source: "file:app", Timestamp: now, Message: "login password=hunter2 ok"},
		}},
	}
	tool := RelatedLogs{Reader: r, Redactor: fakeRedactor{}}
	res, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	logs := res.Data["logs"].([]relatedLogLine)
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	if strings.Contains(logs[0].Message, "hunter2") {
		t.Errorf("secret leaked: %q", logs[0].Message)
	}
	if !strings.Contains(logs[0].Message, "[redacted]") {
		t.Errorf("redaction not applied: %q", logs[0].Message)
	}
}

func TestRelatedLogs_OutsideWindowDropped(t *testing.T) {
	now := time.Now().UTC()
	r := &fakeReader{
		names: []string{"file:app"},
		byName: map[string][]core.Signal{"file:app": {
			{Source: "file:app", Timestamp: now.Add(-2 * time.Minute), Message: "inside"},
			{Source: "file:app", Timestamp: now.Add(-90 * time.Minute), Message: "outside"},
		}},
	}
	tool := RelatedLogs{Reader: r}
	res, err := tool.Invoke(context.Background(), mustArgs(t, relatedLogsArgs{WindowMinutes: 10}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	logs := res.Data["logs"].([]relatedLogLine)
	if len(logs) != 1 || logs[0].Message != "inside" {
		t.Errorf("logs = %+v, want only 'inside'", logs)
	}
}

func TestRelatedLogs_ServiceFilter(t *testing.T) {
	now := time.Now().UTC()
	r := &fakeReader{
		names: []string{"file:app"},
		byName: map[string][]core.Signal{"file:app": {
			{Source: "file:app", Timestamp: now, Message: "a", Fields: map[string]interface{}{"service": "payments"}},
			{Source: "file:app", Timestamp: now, Message: "b", Fields: map[string]interface{}{"service": "billing"}},
		}},
	}
	tool := RelatedLogs{Reader: r}
	res, err := tool.Invoke(context.Background(), mustArgs(t, relatedLogsArgs{Service: "payments"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	logs := res.Data["logs"].([]relatedLogLine)
	if len(logs) != 1 || logs[0].Message != "a" {
		t.Errorf("logs = %+v, want only payments line", logs)
	}
}

// fakeExtractor mirrors agent.ServiceMatcher: it pulls the value after
// "service=" out of the message.
type fakeExtractor struct{}

func (fakeExtractor) Extract(message string) string {
	const key = "service="
	i := strings.Index(message, key)
	if i < 0 {
		return ""
	}
	rest := message[i+len(key):]
	if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
		rest = rest[:sp]
	}
	return rest
}

func TestRelatedLogs_ServiceFilterUsesExtractor(t *testing.T) {
	now := time.Now().UTC()
	r := &fakeReader{
		names: []string{"file:app"},
		byName: map[string][]core.Signal{"file:app": {
			// No structured fields — only the extractor can resolve the service.
			{Source: "file:app", Timestamp: now, Message: "level=error service=payments boom"},
			{Source: "file:app", Timestamp: now, Message: "level=error service=billing ok"},
		}},
	}
	tool := RelatedLogs{Reader: r, Services: fakeExtractor{}}
	res, err := tool.Invoke(context.Background(), mustArgs(t, relatedLogsArgs{Service: "payments"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	logs := res.Data["logs"].([]relatedLogLine)
	if len(logs) != 1 || !strings.Contains(logs[0].Message, "service=payments") {
		t.Errorf("logs = %+v, want only the payments line", logs)
	}
}

func TestRelatedLogs_Miss(t *testing.T) {
	r := &fakeReader{names: []string{"file:app"}, byName: map[string][]core.Signal{"file:app": {}}}
	tool := RelatedLogs{Reader: r}
	res, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Found {
		t.Error("Found = true, want false on empty result")
	}
	if res.Data["count"] != 0 {
		t.Errorf("count = %v, want 0", res.Data["count"])
	}
}

func TestRelatedLogs_AllSourcesTolerateBadSource(t *testing.T) {
	now := time.Now().UTC()
	// One source returns data; an unknown name in the list errors but
	// must not sink the whole call when querying all sources.
	r := &fakeReader{
		names: []string{"file:good", "file:missing"},
		byName: map[string][]core.Signal{
			"file:good": {{Source: "file:good", Timestamp: now, Message: "ok"}},
			// "file:missing" intentionally absent → Pull returns nil, nil here.
		},
	}
	tool := RelatedLogs{Reader: r}
	res, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	logs := res.Data["logs"].([]relatedLogLine)
	if len(logs) != 1 || logs[0].Message != "ok" {
		t.Errorf("logs = %+v, want single 'ok' line", logs)
	}
}

func TestRelatedLogs_NoReader(t *testing.T) {
	tool := RelatedLogs{}
	if _, err := tool.Invoke(context.Background(), nil); err == nil {
		t.Error("expected error when reader is nil")
	}
}
