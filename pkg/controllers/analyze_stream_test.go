package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/agent"
	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// scriptedAgent is a stand-in analyze agent. It replays a fixed list of events
// to whatever observer is on the context, then returns a fixed outcome.
type scriptedAgent struct {
	events []core.AnalyzeEvent
	result *core.AICallResult
	err    error
}

type analyzeRuntimeOrgKey struct{}

type analyzeRuntimeResolver struct{ key string }

func (resolver *analyzeRuntimeResolver) EffectiveKey(ctx context.Context) (string, bool) {
	_, ok := ctx.Value(analyzeRuntimeOrgKey{}).(string)
	return resolver.key, ok
}

func (*analyzeRuntimeResolver) EffectiveEnabled(context.Context) (bool, bool) { return true, true }

func (*analyzeRuntimeResolver) EffectiveProvider(context.Context) (string, bool) {
	return "openai", true
}

func (*analyzeRuntimeResolver) EffectiveKeySet(context.Context) (bool, bool) { return true, true }

func (*analyzeRuntimeResolver) DecorateAIContext(ctx context.Context, scope tenancy.OrgScope) context.Context {
	return context.WithValue(ctx, analyzeRuntimeOrgKey{}, scope.Normalized().Write)
}

type analyzeRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (transport analyzeRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clonedURL := *request.URL
	clonedURL.Scheme = transport.target.Scheme
	clonedURL.Host = transport.target.Host
	clone.URL = &clonedURL
	return transport.base.RoundTrip(clone)
}

func TestAnalyzeHTTPPathsUseRuntimeCredentialAndSanitizeReflectedErrors(t *testing.T) {
	const runtimeKey = "runtime-analyze-secret"
	var capturesMu sync.Mutex
	var authorizations []string
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		capturesMu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		capturesMu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{"message": "reflected " + runtimeKey + "\r\nforged=true"}})
	}))
	defer provider.Close()
	target, err := url.Parse(provider.URL)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })

	resolver := &analyzeRuntimeResolver{key: runtimeKey}
	agent.SetAISettingsResolver(resolver)
	middleware.SetOrgResolver(func(*fiber.Ctx) string { return "org-a" })
	t.Cleanup(func() {
		agent.SetAISettingsResolver(nil)
		middleware.SetOrgResolver(nil)
		services.SetStorage(nil)
		services.SetAnalyzeAgent(nil)
	})

	store := storage.NewMemory()
	record := seedStreamIncident(t, store)
	record.OrgID = "org-a"
	if err := store.SaveIncident(record); err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.LoadCatalog(store)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: analyzeRewriteTransport{target: target, base: http.DefaultTransport}}
	bundle := agent.BuildAIsForScope(config.AgentConfig{AI: config.AgentAIConfig{
		Enable: true, Provider: "openai", APIKey: "", Model: "gpt-4o-mini", MaxTokens: 32,
	}}, catalog, store, tenancy.NewOrgScope("org-a"), client)
	if bundle.Analyze == nil {
		t.Fatal("Analyze agent was not built")
	}
	services.SetStorage(store)
	services.SetAnalyzeAgent(bundle.Analyze)

	controller := NewIncidentAdminController()
	app := fiber.New()
	app.Use(middleware.OrgInjector())
	app.Post("/incidents/:id/analyze", controller.analyze)
	app.Post("/incidents/:id/analyze/stream", controller.analyzeStream)

	syncResponse, err := app.Test(httptest.NewRequest("POST", "/incidents/inc-1/analyze", nil), 10_000)
	if err != nil {
		t.Fatal(err)
	}
	syncBody, _ := io.ReadAll(syncResponse.Body)
	syncResponse.Body.Close()
	streamResponse, streamBody := callStream(t, app)
	if syncResponse.StatusCode != fiber.StatusBadGateway || streamResponse.status != fiber.StatusOK {
		t.Fatalf("statuses sync=%d stream=%d", syncResponse.StatusCode, streamResponse.status)
	}

	analyses, err := store.ListAnalyses(10)
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := json.Marshal(analyses)
	for name, value := range map[string]string{
		"sync response": string(syncBody), "SSE response": streamBody, "logs": logs.String(), "persistence": string(persisted),
	} {
		if strings.Contains(value, runtimeKey) || strings.Contains(value, "forged=true") {
			t.Fatalf("%s leaked reflected provider error: %q", name, value)
		}
	}
	for _, analysis := range analyses {
		if strings.Contains(analysis.RawResponse, runtimeKey) || strings.Contains(analysis.Error, runtimeKey) {
			t.Fatalf("analysis leaked runtime key: %+v", analysis)
		}
		for _, trace := range analysis.ToolCalls {
			encoded, _ := json.Marshal(trace)
			if strings.Contains(string(encoded), runtimeKey) {
				t.Fatalf("tool trace leaked runtime key: %s", encoded)
			}
		}
	}
	capturesMu.Lock()
	defer capturesMu.Unlock()
	if len(authorizations) != 2 {
		t.Fatalf("captured Authorization headers = %q, want two calls", authorizations)
	}
	for _, authorization := range authorizations {
		if authorization != "Bearer "+runtimeKey {
			t.Fatalf("Authorization = %q, want runtime key", authorization)
		}
	}
}

func TestAnalyzeHTTPPathsSanitizeBuildFailureInSyncSSEAndPersistence(t *testing.T) {
	const secret = "runtime-build-reflection-secret"
	store := storage.NewMemory()
	seedStreamIncident(t, store)
	services.SetStorage(store)
	buildErr := einowrap.SafeProviderError("gemini\r\nforged=true", "unsafe\r\n"+secret, errors.New("build reflected "+secret))
	services.SetAnalyzeAgent(&scriptedAgent{
		result: &core.AICallResult{Model: buildErr.Model},
		err:    buildErr,
	})
	t.Cleanup(func() {
		services.SetStorage(nil)
		services.SetAnalyzeAgent(nil)
	})

	controller := NewIncidentAdminController()
	app := fiber.New()
	app.Post("/incidents/:id/analyze", controller.analyze)
	app.Post("/incidents/:id/analyze/stream", controller.analyzeStream)
	syncResponse, err := app.Test(httptest.NewRequest("POST", "/incidents/inc-1/analyze", nil), 10_000)
	if err != nil {
		t.Fatal(err)
	}
	syncBody, _ := io.ReadAll(syncResponse.Body)
	syncResponse.Body.Close()
	streamResponse, streamBody := callStream(t, app)
	if syncResponse.StatusCode != fiber.StatusBadGateway || streamResponse.status != fiber.StatusOK {
		t.Fatalf("statuses sync=%d stream=%d", syncResponse.StatusCode, streamResponse.status)
	}
	analyses, err := store.ListAnalyses(10)
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := json.Marshal(analyses)
	for name, value := range map[string]string{"sync response": string(syncBody), "SSE response": streamBody, "persistence": string(persisted)} {
		if strings.Contains(value, secret) || strings.Contains(value, "forged=true") {
			t.Fatalf("%s leaked malicious build failure: %q", name, value)
		}
	}
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
// run_failed without exposing backend details to the client.
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
	if term.data.Error != internalServerErrorMessage || strings.Contains(raw, "disk gone") {
		t.Fatalf("terminal error = %q, want generic error without backend detail", term.data.Error)
	}
}

func TestAnalyze_SaveFailureIsNotDisclosed(t *testing.T) {
	backendDetail := "write /private/analyses.json: credential=secret"
	store := failSaveStore{Provider: storage.NewMemory(), err: errors.New(backendDetail)}
	seedStreamIncident(t, store)
	t.Cleanup(func() {
		services.SetStorage(nil)
		services.SetAnalyzeAgent(nil)
	})
	services.SetStorage(store)
	services.SetAnalyzeAgent(&scriptedAgent{result: okResult()})

	app := fiber.New()
	app.Post("/incidents/:id/analyze", NewIncidentAdminController().analyze)
	resp, err := app.Test(httptest.NewRequest("POST", "/incidents/inc-1/analyze", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusInternalServerError)
	}
	if strings.Contains(string(body), backendDetail) || !strings.Contains(string(body), internalServerErrorMessage) {
		t.Fatalf("body = %q, want generic error without backend detail", body)
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

	res, raw := callStream(t, app)
	term := res.terminal(t)
	if term.data.Error != internalServerErrorMessage || strings.Contains(raw, "disk gone") {
		t.Fatalf("terminal error = %q, want generic save failure without backend detail", term.data.Error)
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
	org          string
	authorized   bool
	hasObserver  bool
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
	c.org, _ = ctx.Value(analyzeOrgKey{}).(string)
	c.authorized = core.CallerAuthorized(ctx, core.PermissionInfrastructureView)
	c.hasObserver = core.AnalyzeObserverFrom(ctx) != nil
	return c.result, nil
}

type analyzeOrgKey struct{}

type analyzeContextResolver struct {
	scopes []tenancy.OrgScope
}

func (*analyzeContextResolver) EffectiveKey(context.Context) (string, bool) {
	return "", false
}

func (*analyzeContextResolver) EffectiveEnabled(context.Context) (bool, bool) {
	return false, false
}

func (resolver *analyzeContextResolver) DecorateAIContext(ctx context.Context, scope tenancy.OrgScope) context.Context {
	resolver.scopes = append(resolver.scopes, scope.Normalized())
	return context.WithValue(ctx, analyzeOrgKey{}, scope.Write)
}

func TestAnalyzeHandlersDecorateTrustedRequestScope(t *testing.T) {
	store := storage.NewMemory()
	record := seedStreamIncident(t, store)
	record.OrgID = "org-a"
	if err := store.SaveIncident(record); err != nil {
		t.Fatal(err)
	}
	agent.SetAISettingsResolver(&analyzeContextResolver{})
	middleware.SetOrgResolver(func(*fiber.Ctx) string { return "org-a" })
	t.Cleanup(func() {
		agent.SetAISettingsResolver(nil)
		middleware.SetOrgResolver(nil)
		services.SetStorage(nil)
		services.SetAnalyzeAgent(nil)
	})
	services.SetStorage(store)

	controller := NewIncidentAdminController()
	streamProbe := &ctxProbeAgent{result: okResult()}
	services.SetAnalyzeAgent(streamProbe)
	streamApp := fiber.New()
	streamApp.Use(func(ctx *fiber.Ctx) error {
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	streamApp.Use(middleware.OrgInjector())
	streamApp.Post("/incidents/:id/analyze/stream", controller.analyzeStream)
	callStream(t, streamApp)
	if streamProbe.org != "org-a" {
		t.Fatalf("stream analyze org = %q, want org-a", streamProbe.org)
	}
	if !streamProbe.authorized || !streamProbe.hasObserver {
		t.Fatalf("stream context lost authorization or observer: authorized=%v observer=%v", streamProbe.authorized, streamProbe.hasObserver)
	}

	syncProbe := &ctxProbeAgent{result: okResult()}
	services.SetAnalyzeAgent(syncProbe)
	syncApp := fiber.New()
	syncApp.Use(func(ctx *fiber.Ctx) error {
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	syncApp.Use(middleware.OrgInjector())
	syncApp.Post("/incidents/:id/analyze", controller.analyze)
	response, err := syncApp.Test(httptest.NewRequest("POST", "/incidents/inc-1/analyze", nil), 10_000)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if syncProbe.org != "org-a" {
		t.Fatalf("sync analyze org = %q, want org-a", syncProbe.org)
	}
	if !syncProbe.authorized || syncProbe.hasObserver {
		t.Fatalf("sync context authorization/observer = %v/%v, want true/false", syncProbe.authorized, syncProbe.hasObserver)
	}
}

type analyzeScopedStore struct {
	storage.Provider
	scope tenancy.OrgScope
}

func (store analyzeScopedStore) OrgScope() tenancy.OrgScope { return store.scope }

func TestAnalyzeHandlersAdmitTrustedReadScopeAndPinRuntimeToWriteOrg(t *testing.T) {
	base := storage.NewMemory()
	writeRecord := seedStreamIncident(t, base)
	writeRecord.OrgID = "licensed"
	if err := base.SaveIncident(writeRecord); err != nil {
		t.Fatal(err)
	}
	archiveRecord := *writeRecord
	archiveRecord.ID = "inc-archive"
	archiveRecord.OrgID = storage.DefaultOrgID
	if err := base.SaveIncident(&archiveRecord); err != nil {
		t.Fatal(err)
	}
	foreignRecord := *writeRecord
	foreignRecord.ID = "inc-foreign"
	foreignRecord.OrgID = "foreign"
	if err := base.SaveIncident(&foreignRecord); err != nil {
		t.Fatal(err)
	}

	store := analyzeScopedStore{Provider: base, scope: tenancy.NewOrgScope("licensed", storage.DefaultOrgID)}
	resolver := &analyzeContextResolver{}
	agent.SetAISettingsResolver(resolver)
	middleware.SetOrgResolver(func(*fiber.Ctx) string { return "licensed" })
	t.Cleanup(func() {
		agent.SetAISettingsResolver(nil)
		middleware.SetOrgResolver(nil)
		services.SetStorage(nil)
		services.SetAnalyzeAgent(nil)
	})
	services.SetStorage(store)
	controller := NewIncidentAdminController()

	for _, path := range []string{"analyze", "analyze/stream"} {
		for _, incidentID := range []string{"inc-1", "inc-archive"} {
			probe := &ctxProbeAgent{result: okResult()}
			services.SetAnalyzeAgent(probe)
			app := fiber.New()
			app.Use(middleware.OrgInjector())
			app.Post("/incidents/:id/analyze", controller.analyze)
			app.Post("/incidents/:id/analyze/stream", controller.analyzeStream)
			response, err := app.Test(httptest.NewRequest("POST", "/incidents/"+incidentID+"/"+path, nil), 10_000)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != fiber.StatusOK || !probe.ran || probe.org != "licensed" {
				t.Fatalf("%s %s status/run/runtime = %d/%v/%q", path, incidentID, response.StatusCode, probe.ran, probe.org)
			}
		}
	}
	for _, scope := range resolver.scopes {
		if scope.Write != "licensed" || !scope.Contains(storage.DefaultOrgID) {
			t.Fatalf("decorated scope = %#v, want licensed write plus default read", scope)
		}
	}

	probe := &ctxProbeAgent{result: okResult()}
	decorationsBeforeForeign := len(resolver.scopes)
	services.SetAnalyzeAgent(probe)
	app := fiber.New()
	app.Use(middleware.OrgInjector())
	app.Post("/incidents/:id/analyze", controller.analyze)
	response, err := app.Test(httptest.NewRequest("POST", "/incidents/inc-foreign/analyze", nil), 10_000)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != fiber.StatusNotFound || probe.ran {
		t.Fatalf("foreign status/run = %d/%v, want 404/false", response.StatusCode, probe.ran)
	}
	if len(resolver.scopes) != decorationsBeforeForeign {
		t.Fatal("foreign incident reached runtime scope/key decoration")
	}
}

func TestAnalyzeHandlersRejectForeignIncidentBeforeAgentRun(t *testing.T) {
	store := storage.NewMemory()
	seedStreamIncident(t, store)
	middleware.SetOrgResolver(func(*fiber.Ctx) string { return "org-b" })
	t.Cleanup(func() {
		middleware.SetOrgResolver(nil)
		services.SetStorage(nil)
		services.SetAnalyzeAgent(nil)
	})
	services.SetStorage(store)
	probe := &ctxProbeAgent{result: okResult()}
	services.SetAnalyzeAgent(probe)
	controller := NewIncidentAdminController()
	app := fiber.New()
	app.Use(middleware.OrgInjector())
	app.Post("/incidents/:id/analyze", controller.analyze)
	app.Post("/incidents/:id/analyze/stream", controller.analyzeStream)

	for _, path := range []string{"/incidents/inc-1/analyze", "/incidents/inc-1/analyze/stream"} {
		response, err := app.Test(httptest.NewRequest("POST", path, nil), 10_000)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != fiber.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, response.StatusCode)
		}
	}
	if probe.ran {
		t.Fatal("Analyze agent ran for a foreign incident scope")
	}
}
