package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// scriptedAgent is a stand-in analyze agent. It replays a fixed list of events
// to whatever observer is on the context, then returns a fixed outcome.
type scriptedAgent struct {
	events []core.AnalyzeEvent
	result *core.AICallResult
	err    error
}

func (s *scriptedAgent) Name() string          { return "analyze" }
func (s *scriptedAgent) Kind() core.AITaskKind { return core.AITaskAnalyze }
func (s *scriptedAgent) Run(ctx context.Context, _ core.AITask) (*core.AICallResult, error) {
	for _, ev := range s.events {
		core.EmitAnalyzeEvent(ctx, ev)
	}
	return s.result, s.err
}

// failSaveStore is a Provider whose analysis writes always fail, so a test can
// separate "the run failed" from "the record was lost".
type failSaveStore struct {
	storage.Provider
	err error
}

func (f failSaveStore) SaveAnalysis(*storage.AnalysisRecord) error { return f.err }

// sseFrame is one parsed `event:`/`data:` pair off the wire.
type sseFrame struct {
	event string
	data  core.AnalyzeEvent
	raw   string
}

func newStreamApp(t *testing.T, store storage.Provider, ag core.AIAgent) *fiber.App {
	t.Helper()
	t.Cleanup(func() {
		services.SetStorage(nil)
		services.SetAnalyzeAgent(nil)
	})
	services.SetStorage(store)
	services.SetAnalyzeAgent(ag)

	ctrl := NewIncidentAdminController()
	app := fiber.New()
	app.Post("/incidents/:id/analyze/stream", ctrl.analyzeStream)
	return app
}

func seedStreamIncident(t *testing.T, store storage.Provider) *storage.IncidentRecord {
	t.Helper()
	rec := &storage.IncidentRecord{
		ID:        "inc-1",
		Title:     "disk full",
		Source:    "loki",
		CreatedAt: time.Now().UTC(),
		Content:   map[string]any{"severity": "high"},
	}
	if err := store.SaveIncident(rec); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}
	return rec
}

// callStream POSTs to the stream endpoint and parses the whole SSE response.
func callStream(t *testing.T, app *fiber.App) (*sseResponse, string) {
	t.Helper()
	res, body := callStreamRaw(t, app)
	res.frames = parseSSE(t, body)
	return res, body
}

// callStreamRaw is callStream without the SSE parsing, for the refusal paths
// that answer with plain JSON.
func callStreamRaw(t *testing.T, app *fiber.App) (*sseResponse, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/incidents/inc-1/analyze/stream", strings.NewReader(`{"requested_by":"alice"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 10_000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return &sseResponse{
		contentType:   resp.Header.Get("Content-Type"),
		accelBuffer:   resp.Header.Get("X-Accel-Buffering"),
		cacheControl:  resp.Header.Get("Cache-Control"),
		status:        resp.StatusCode,
		rawBodyString: string(body),
	}, string(body)
}

type sseResponse struct {
	contentType   string
	accelBuffer   string
	cacheControl  string
	status        int
	frames        []sseFrame
	rawBodyString string
}

func (r *sseResponse) terminal(t *testing.T) sseFrame {
	t.Helper()
	var found []sseFrame
	for _, f := range r.frames {
		if f.event == core.AnalyzeEventRunFinished || f.event == core.AnalyzeEventRunFailed {
			found = append(found, f)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d terminal events, want exactly 1:\n%s", len(found), r.rawBodyString)
	}
	return found[0]
}

// parseSSE splits a body into frames the way the browser's parser does:
// records separated by a blank line, each with an `event:` and a `data:` line.
func parseSSE(t *testing.T, body string) []sseFrame {
	t.Helper()
	out := []sseFrame{}
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f sseFrame
		f.raw = block
		var sawData bool
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				sawData = true
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f.data); err != nil {
					t.Fatalf("frame data is not JSON (%v):\n%s", err, block)
				}
			}
		}
		if f.event == "" || !sawData {
			t.Fatalf("malformed SSE frame, browser parser would drop it:\n%q", block)
		}
		out = append(out, f)
	}
	return out
}

func okResult() *core.AICallResult {
	return &core.AICallResult{
		RawResponse: `{"title":"t"}`,
		DurationMs:  42,
		Model:       "fake",
		ToolCalls:   []core.ToolCallTrace{{Name: "logs", Args: `{"q":"a"}`, Output: `{"ok":true}`, DurationMs: 3}},
	}
}

// TestAnalyzeStream_SSEFraming asserts the wire format the browser parser
// depends on: text/event-stream, proxy buffering disabled, and one
// `event: <kind>` + `data: <json>` record per event, separated by a blank line.
func TestAnalyzeStream_SSEFraming(t *testing.T) {
	store := storage.NewMemory()
	seedStreamIncident(t, store)
	app := newStreamApp(t, store, &scriptedAgent{
		events: []core.AnalyzeEvent{
			{Seq: 1, Kind: core.AnalyzeEventRunStarted},
			{Seq: 2, Kind: core.AnalyzeEventToolStarted, Tool: "logs", Args: `{"q":"a"}`},
			{Seq: 3, Kind: core.AnalyzeEventToolFinished, Tool: "logs", Output: `{"ok":true}`, DurationMs: 3},
		},
		result: okResult(),
	})

	res, raw := callStream(t, app)
	if res.status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if !strings.HasPrefix(res.contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", res.contentType)
	}
	if res.accelBuffer != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want \"no\" — a proxy would hold the whole stream", res.accelBuffer)
	}
	if res.cacheControl != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", res.cacheControl)
	}
	if !strings.HasSuffix(raw, "\n\n") {
		t.Fatalf("body does not end on a frame separator; the last frame would never parse:\n%q", raw)
	}
	if len(res.frames) != 4 {
		t.Fatalf("got %d frames, want 3 scripted + 1 terminal:\n%s", len(res.frames), raw)
	}
	for i, f := range res.frames {
		if f.event != f.data.Kind {
			t.Fatalf("frame %d: event name %q does not match payload kind %q", i, f.event, f.data.Kind)
		}
	}
	if got := res.frames[1]; got.event != core.AnalyzeEventToolStarted || got.data.Tool != "logs" || got.data.Args != `{"q":"a"}` {
		t.Fatalf("tool frame lost its payload: %+v", got)
	}
	if !strings.Contains(raw, "event: tool_started\ndata: ") {
		t.Fatalf("frame is not in `event: <kind>\\ndata: <json>` form:\n%q", raw)
	}
}

// TestAnalyzeStream_TerminalEventCarriesAnalysisID asserts the run ends with a
// single run_finished naming the record that was persisted, which is how the
// client fetches the full result without guessing.
func TestAnalyzeStream_TerminalEventCarriesAnalysisID(t *testing.T) {
	store := storage.NewMemory()
	seedStreamIncident(t, store)
	app := newStreamApp(t, store, &scriptedAgent{
		events: []core.AnalyzeEvent{{Seq: 1, Kind: core.AnalyzeEventRunStarted}},
		result: okResult(),
	})

	res, raw := callStream(t, app)
	term := res.terminal(t)
	if term.event != core.AnalyzeEventRunFinished {
		t.Fatalf("terminal event = %q, want run_finished:\n%s", term.event, raw)
	}
	if term.data.Error != "" {
		t.Fatalf("successful run reported error %q", term.data.Error)
	}
	if term.data.AnalysisID == "" {
		t.Fatalf("terminal event carries no analysis_id:\n%s", raw)
	}
	if _, err := store.GetAnalysis(term.data.AnalysisID); err != nil {
		t.Fatalf("analysis_id %q is not in storage: %v", term.data.AnalysisID, err)
	}
	if term.data.DurationMs != 42 {
		t.Fatalf("terminal DurationMs = %d, want the persisted 42", term.data.DurationMs)
	}
}

// TestAnalyzeStream_RunFailureIsTerminalFailure asserts a failed model run
// ends the stream with run_failed carrying the run's own error, while the
// partial record is still persisted and named.
func TestAnalyzeStream_RunFailureIsTerminalFailure(t *testing.T) {
	store := storage.NewMemory()
	seedStreamIncident(t, store)
	app := newStreamApp(t, store, &scriptedAgent{
		events: []core.AnalyzeEvent{{Seq: 1, Kind: core.AnalyzeEventRunStarted}},
		result: okResult(),
		err:    errors.New("model refused"),
	})

	res, raw := callStream(t, app)
	term := res.terminal(t)
	if term.event != core.AnalyzeEventRunFailed {
		t.Fatalf("terminal event = %q, want run_failed:\n%s", term.event, raw)
	}
	if term.data.Error != "model refused" {
		t.Fatalf("terminal error = %q, want the run error verbatim", term.data.Error)
	}
	if strings.HasPrefix(term.data.Error, "save:") {
		t.Fatalf("a run failure was reported as a save failure: %q", term.data.Error)
	}
	if term.data.AnalysisID == "" {
		t.Fatalf("a failed run still persists a record; analysis_id is missing")
	}
	rec, err := store.GetAnalysis(term.data.AnalysisID)
	if err != nil {
		t.Fatalf("failed run was not persisted: %v", err)
	}
	if rec.Status != "error" || rec.Error != "model refused" {
		t.Fatalf("persisted record does not record the failure: %+v", rec)
	}
}

// TestAnalyzeStream_SaveFailureIsDistinguishable asserts a lost record reports
// run_failed prefixed with "save:", so the operator can tell "the analysis
// failed" from "the analysis succeeded but we could not keep it".
func TestAnalyzeStream_SaveFailureIsDistinguishable(t *testing.T) {
	store := failSaveStore{Provider: storage.NewMemory(), err: errors.New("disk gone")}
	seedStreamIncident(t, store)
	app := newStreamApp(t, store, &scriptedAgent{
		events: []core.AnalyzeEvent{{Seq: 1, Kind: core.AnalyzeEventRunStarted}},
		result: okResult(),
	})

	res, raw := callStream(t, app)
	term := res.terminal(t)
	if term.event != core.AnalyzeEventRunFailed {
		t.Fatalf("terminal event = %q, want run_failed:\n%s", term.event, raw)
	}
	if term.data.Error != "save: disk gone" {
		t.Fatalf("terminal error = %q, want the save error tagged as such", term.data.Error)
	}
}

// TestAnalyzeStream_SaveFailureOutranksRunFailure asserts that when both fail
// the operator is told the record was lost — the more actionable of the two.
func TestAnalyzeStream_SaveFailureOutranksRunFailure(t *testing.T) {
	store := failSaveStore{Provider: storage.NewMemory(), err: errors.New("disk gone")}
	seedStreamIncident(t, store)
	app := newStreamApp(t, store, &scriptedAgent{
		result: okResult(),
		err:    errors.New("model refused"),
	})

	res, _ := callStream(t, app)
	term := res.terminal(t)
	if term.data.Error != "save: disk gone" {
		t.Fatalf("terminal error = %q, want the save failure to win", term.data.Error)
	}
}

// TestAnalyzeStream_UnavailableWhenUnconfigured asserts the endpoint refuses
// with JSON (not a half-open stream) when there is nothing to run.
func TestAnalyzeStream_UnavailableWhenUnconfigured(t *testing.T) {
	store := storage.NewMemory()
	seedStreamIncident(t, store)
	app := newStreamApp(t, store, nil)

	res, body := callStreamRaw(t, app)
	if res.status != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if strings.HasPrefix(res.contentType, "text/event-stream") {
		t.Fatalf("a refusal was sent as an event stream")
	}
	if !strings.Contains(body, "analyze agent not enabled") {
		t.Fatalf("body = %q, want the reason for the refusal", body)
	}
}

// TestStreamObserver_DropsInsteadOfBlocking asserts a viewer that stops
// reading cannot stall the model run: once the buffer is full, further events
// are dropped and the emitting callback returns.
func TestStreamObserver_DropsInsteadOfBlocking(t *testing.T) {
	ch := make(chan core.AnalyzeEvent, 1)
	obs := streamObserver{ch: ch}

	done := make(chan struct{})
	go func() {
		defer close(done)
		obs.OnAnalyzeEvent(core.AnalyzeEvent{Seq: 1, Kind: core.AnalyzeEventModelDelta})
		obs.OnAnalyzeEvent(core.AnalyzeEvent{Seq: 2, Kind: core.AnalyzeEventModelDelta})
		obs.OnAnalyzeEvent(core.AnalyzeEvent{Seq: 3, Kind: core.AnalyzeEventModelDelta})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("emitting blocked on a full channel; a stalled viewer would freeze the run")
	}

	if len(ch) != 1 {
		t.Fatalf("channel holds %d events, want 1 (capacity)", len(ch))
	}
	got := <-ch
	if got.Seq != 1 {
		t.Fatalf("kept event Seq = %d, want the first one buffered", got.Seq)
	}
}

func TestSendTerminalAnalyzeEventWaitsForBufferSpace(t *testing.T) {
	ch := make(chan core.AnalyzeEvent, 1)
	ch <- core.AnalyzeEvent{Seq: 1, Kind: core.AnalyzeEventModelDelta}
	term := core.AnalyzeEvent{Kind: core.AnalyzeEventRunFailed, Error: "model failed"}
	done := make(chan struct{})
	go func() {
		sendTerminalAnalyzeEvent(context.Background(), ch, term)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("terminal event was dropped while the buffer was full")
	case <-time.After(10 * time.Millisecond):
	}

	<-ch
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal send did not resume after buffer space became available")
	}
	if got := <-ch; got.Kind != core.AnalyzeEventRunFailed || got.Error != "model failed" {
		t.Fatalf("terminal event = %+v, want run_failed with model error", got)
	}
}

// TestRunAndPersistAnalysis_SeparatesErrors asserts the shared runner reports
// run failure and save failure independently, which is what lets the
// synchronous endpoint answer 502 vs 500 and the stream tag its terminal event.
func TestRunAndPersistAnalysis_SeparatesErrors(t *testing.T) {
	t.Cleanup(func() {
		services.SetStorage(nil)
		services.SetAnalyzeAgent(nil)
	})
	rec := &storage.IncidentRecord{ID: "inc-1", Title: "boom", CreatedAt: time.Now().UTC()}

	services.SetStorage(storage.NewMemory())
	services.SetAnalyzeAgent(&scriptedAgent{result: okResult(), err: errors.New("model refused")})
	analysis, runErr, saveErr := runAndPersistAnalysis(context.Background(), rec, "alice")
	if runErr == nil || runErr.Error() != "model refused" {
		t.Fatalf("runErr = %v, want the run failure", runErr)
	}
	if saveErr != nil {
		t.Fatalf("saveErr = %v, want nil — the record was still written", saveErr)
	}
	if analysis == nil || analysis.Status != "error" || analysis.RequestedBy != "alice" {
		t.Fatalf("analysis record wrong: %+v", analysis)
	}
	if len(analysis.ToolCalls) != 1 || analysis.ToolCalls[0].Name != "logs" {
		t.Fatalf("audit trail not carried onto the record: %+v", analysis.ToolCalls)
	}

	services.SetStorage(failSaveStore{Provider: storage.NewMemory(), err: errors.New("disk gone")})
	services.SetAnalyzeAgent(&scriptedAgent{result: okResult()})
	analysis, runErr, saveErr = runAndPersistAnalysis(context.Background(), rec, "")
	if runErr != nil {
		t.Fatalf("runErr = %v, want nil", runErr)
	}
	if saveErr == nil || saveErr.Error() != "disk gone" {
		t.Fatalf("saveErr = %v, want the save failure", saveErr)
	}
	if analysis == nil || analysis.Status != "ok" {
		t.Fatalf("a successful run must still be reported: %+v", analysis)
	}
}

// TestAnalyzeStream_DetachedRunUsesItsOwnDeadline asserts the analysis runs on
// a context detached from the request but still bounded: closing the tab must
// not cancel an expensive run, and a stuck run must still be reclaimed.
func TestAnalyzeStream_DetachedRunUsesItsOwnDeadline(t *testing.T) {
	store := storage.NewMemory()
	seedStreamIncident(t, store)

	probe := &ctxProbeAgent{result: okResult()}
	app := newStreamApp(t, store, probe)
	callStream(t, app)

	if !probe.ran {
		t.Fatalf("agent never ran")
	}
	if probe.errDuringRun != nil {
		t.Fatalf("run context was already cancelled while running: %v", probe.errDuringRun)
	}
	if !probe.hasDeadline {
		t.Fatalf("run context has no deadline; a stuck run would never be reclaimed")
	}
	if probe.remaining <= 0 || probe.remaining > analyzeRunTimeout {
		t.Fatalf("run deadline is %s away, want within (0, %s]", probe.remaining, analyzeRunTimeout)
	}
}

// ctxProbeAgent records what the run context looked like from inside Run.
type ctxProbeAgent struct {
	result *core.AICallResult

	ran          bool
	errDuringRun error
	hasDeadline  bool
	remaining    time.Duration
}

func (c *ctxProbeAgent) Name() string          { return "analyze" }
func (c *ctxProbeAgent) Kind() core.AITaskKind { return core.AITaskAnalyze }
func (c *ctxProbeAgent) Run(ctx context.Context, _ core.AITask) (*core.AICallResult, error) {
	c.ran = true
	c.errDuringRun = ctx.Err()
	dl, ok := ctx.Deadline()
	c.hasDeadline = ok
	if ok {
		c.remaining = time.Until(dl)
	}
	return c.result, nil
}
