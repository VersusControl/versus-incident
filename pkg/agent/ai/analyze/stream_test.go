package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// recorder is a test AnalyzeObserver that keeps every event it is handed.
type recorder struct {
	mu     sync.Mutex
	events []core.AnalyzeEvent
}

func (r *recorder) OnAnalyzeEvent(ev core.AnalyzeEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) all() []core.AnalyzeEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]core.AnalyzeEvent, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recorder) ofKind(kind string) []core.AnalyzeEvent {
	out := []core.AnalyzeEvent{}
	for _, ev := range r.all() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

const streamFindingJSON = `{"title":"t","summary":"s","next_steps":["x"]}`

func echoToolCallMessage() *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID:       "c-1",
		Type:     "function",
		Function: schema.FunctionCall{Name: "echo_tool", Arguments: `{"q":"disk"}`},
	}})
}

// runWatched drives one analyze run with an observer attached, which is what
// switches the agent onto the streaming path.
func runWatched(t *testing.T, fake *fakeChat, tools []core.AnalyzeTool) (*core.AICallResult, *recorder, error) {
	t.Helper()
	a := newAgentWithFake(t, fake, tools, 3)
	rec := &recorder{}
	ctx := core.WithAnalyzeObserver(context.Background(), rec)
	res, err := a.Run(ctx, core.AnalyzeTask{Snapshot: core.AnalyzeIncidentSnapshot{IncidentID: "i-1"}})
	return res, rec, err
}

// TestStream_ObserverSelectsStreamingPath asserts the observer is the ONLY
// switch: without one the agent calls Generate, with one it calls Stream.
func TestStream_ObserverSelectsStreamingPath(t *testing.T) {
	plain := &fakeChat{turns: []*schema.Message{schema.AssistantMessage(streamFindingJSON, nil)}, chunks: 3}
	a := newAgentWithFake(t, plain, nil, 3)
	if _, err := a.Run(context.Background(), core.AnalyzeTask{Snapshot: core.AnalyzeIncidentSnapshot{IncidentID: "i-1"}}); err != nil {
		t.Fatalf("unwatched Run: %v", err)
	}
	if n := atomic.LoadInt32(&plain.streamCalls); n != 0 {
		t.Fatalf("unwatched run made %d Stream calls, want 0 (Generate path)", n)
	}

	watched := &fakeChat{turns: []*schema.Message{schema.AssistantMessage(streamFindingJSON, nil)}, chunks: 3}
	if _, _, err := runWatched(t, watched, nil); err != nil {
		t.Fatalf("watched Run: %v", err)
	}
	if n := atomic.LoadInt32(&watched.streamCalls); n == 0 {
		t.Fatalf("watched run made no Stream calls; streaming never engaged")
	}
}

// TestStream_NoObserverEmitsNothingAndMatchesGenerate is the guard that the
// streaming work cannot change the synchronous endpoint: the unwatched run
// takes the Generate path and produces the same result the watched run does.
func TestStream_NoObserverEmitsNothingAndMatchesGenerate(t *testing.T) {
	newScript := func() []*schema.Message {
		return []*schema.Message{echoToolCallMessage(), schema.AssistantMessage(streamFindingJSON, nil)}
	}

	plainTool := &stubTool{name: "echo_tool"}
	plain := newAgentWithFake(t, &fakeChat{turns: newScript()}, []core.AnalyzeTool{plainTool}, 3)
	plainRes, err := plain.Run(context.Background(), core.AnalyzeTask{Snapshot: core.AnalyzeIncidentSnapshot{IncidentID: "i-1"}})
	if err != nil {
		t.Fatalf("unwatched Run: %v", err)
	}

	watchedTool := &stubTool{name: "echo_tool"}
	watchedRes, rec, err := runWatched(t, &fakeChat{turns: newScript(), chunks: 4}, []core.AnalyzeTool{watchedTool})
	if err != nil {
		t.Fatalf("watched Run: %v", err)
	}

	if plainRes.RawResponse != watchedRes.RawResponse {
		t.Fatalf("RawResponse drifted:\n plain   = %q\n watched = %q", plainRes.RawResponse, watchedRes.RawResponse)
	}
	if plainRes.UserPrompt != watchedRes.UserPrompt {
		t.Fatalf("UserPrompt drifted between paths")
	}
	if plainRes.Model != watchedRes.Model {
		t.Fatalf("Model drifted: %q vs %q", plainRes.Model, watchedRes.Model)
	}
	if !sameJSON(t, plainRes.Finding, watchedRes.Finding) {
		t.Fatalf("Finding drifted:\n plain   = %+v\n watched = %+v", plainRes.Finding, watchedRes.Finding)
	}
	// The audit trail must not regress when someone happens to be watching.
	if !sameTraces(plainRes.ToolCalls, watchedRes.ToolCalls) {
		t.Fatalf("ToolCallTrace regressed:\n plain   = %+v\n watched = %+v", plainRes.ToolCalls, watchedRes.ToolCalls)
	}
	if got := len(rec.all()); got == 0 {
		t.Fatalf("watched run emitted no events; the comparison above proves nothing")
	}
}

func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	return string(ab) == string(bb)
}

// sameTraces compares audit traces on every field except DurationMs, which is
// wall-clock and never reproducible between two runs.
func sameTraces(a, b []core.ToolCallTrace) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Args != b[i].Args ||
			a[i].Output != b[i].Output || a[i].Error != b[i].Error {
			return false
		}
	}
	return true
}

// TestStream_ToolEvents asserts a watched run reports each tool call twice —
// once on dispatch, once on completion — correlated by a shared Seq, and that
// the event carries no more than the persisted trace does.
func TestStream_ToolEvents(t *testing.T) {
	stub := &stubTool{name: "echo_tool", display: "Checking disk"}
	fake := &fakeChat{
		turns:  []*schema.Message{echoToolCallMessage(), schema.AssistantMessage(streamFindingJSON, nil)},
		chunks: 3,
	}
	res, rec, err := runWatched(t, fake, []core.AnalyzeTool{stub})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	started := rec.ofKind(core.AnalyzeEventToolStarted)
	finished := rec.ofKind(core.AnalyzeEventToolFinished)
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("tool events: %d started / %d finished, want 1/1", len(started), len(finished))
	}
	if started[0].Tool != "echo_tool" || finished[0].Tool != "echo_tool" {
		t.Fatalf("tool name missing: started=%q finished=%q", started[0].Tool, finished[0].Tool)
	}
	if started[0].ToolDisplay != "Checking disk" || finished[0].ToolDisplay != "Checking disk" {
		t.Fatalf("tool display missing: started=%q finished=%q", started[0].ToolDisplay, finished[0].ToolDisplay)
	}
	if started[0].Seq != finished[0].Seq {
		t.Fatalf("seq mismatch: started=%d finished=%d — the UI cannot pair them", started[0].Seq, finished[0].Seq)
	}
	if started[0].Args != `{"q":"disk"}` {
		t.Fatalf("tool args = %q, want the dispatched arguments", started[0].Args)
	}
	if finished[0].Output == "" {
		t.Fatalf("tool_finished carried no output")
	}

	if len(res.ToolCalls) != 1 {
		t.Fatalf("persisted trace len = %d, want 1", len(res.ToolCalls))
	}
	if finished[0].Output != res.ToolCalls[0].Output {
		t.Fatalf("event output diverged from the persisted trace:\n event  = %q\n record = %q",
			finished[0].Output, res.ToolCalls[0].Output)
	}
	if started[0].Args != res.ToolCalls[0].Args {
		t.Fatalf("event args diverged from the persisted trace")
	}
	if finished[0].DurationMs != res.ToolCalls[0].DurationMs {
		t.Fatalf("event duration %d does not match the recorded %d",
			finished[0].DurationMs, res.ToolCalls[0].DurationMs)
	}
}

// TestStream_SlowToolReportsDuration asserts tool_finished carries real
// elapsed time rather than a placeholder zero.
func TestStream_SlowToolReportsDuration(t *testing.T) {
	slow := &sleepTool{name: "echo_tool", nap: 15 * time.Millisecond}
	fake := &fakeChat{
		turns:  []*schema.Message{echoToolCallMessage(), schema.AssistantMessage(streamFindingJSON, nil)},
		chunks: 2,
	}
	_, rec, err := runWatched(t, fake, []core.AnalyzeTool{slow})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	finished := rec.ofKind(core.AnalyzeEventToolFinished)
	if len(finished) != 1 {
		t.Fatalf("got %d tool_finished, want 1", len(finished))
	}
	if finished[0].DurationMs <= 0 {
		t.Fatalf("tool_finished DurationMs = %d, want the measured elapsed time", finished[0].DurationMs)
	}
}

// TestStream_ToolTurnStamping asserts tool events are stamped with the model
// turn that dispatched them. The ReAct graph alternates model -> tools ->
// model, so the tools of turn 1 are all dispatched before turn 2 begins.
func TestStream_ToolTurnStamping(t *testing.T) {
	stub := &stubTool{name: "echo_tool"}
	fake := &fakeChat{
		turns:  []*schema.Message{echoToolCallMessage(), schema.AssistantMessage(streamFindingJSON, nil)},
		chunks: 3,
	}
	_, rec, err := runWatched(t, fake, []core.AnalyzeTool{stub})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	started := rec.ofKind(core.AnalyzeEventToolStarted)
	if len(started) != 1 {
		t.Fatalf("got %d tool_started, want 1", len(started))
	}
	if started[0].Turn != 1 {
		t.Fatalf("tool_started Turn = %d, want 1 (the turn that requested it)", started[0].Turn)
	}
	deltas := rec.ofKind(core.AnalyzeEventModelDelta)
	if len(deltas) == 0 {
		t.Fatalf("no deltas emitted")
	}
	for _, d := range deltas {
		if d.Turn != 2 {
			t.Fatalf("model_delta Turn = %d, want 2 (the answering turn)", d.Turn)
		}
	}
}

// TestStream_ToolErrorEvent drives the collector's tool callbacks directly.
// A framework-level tool failure (one the einoTool adapter does not convert
// into a structured result) must surface as tool_error carrying the message.
func TestStream_ToolErrorEvent(t *testing.T) {
	rec := &recorder{}
	c := &traceCollector{
		obsCtx:       core.WithAnalyzeObserver(context.Background(), rec),
		toolDisplays: map[string]string{"echo_tool": "Checking disk"},
	}
	h := c.toolHandler()

	info := &callbacks.RunInfo{Name: "echo_tool"}
	ctx := h.OnStart(context.Background(), info, &tool.CallbackInput{ArgumentsInJSON: `{"q":"a"}`})
	h.OnError(ctx, info, errors.New("dispatch exploded"))

	errs := rec.ofKind(core.AnalyzeEventToolError)
	if len(errs) != 1 {
		t.Fatalf("got %d tool_error events, want 1", len(errs))
	}
	if errs[0].Error != "dispatch exploded" {
		t.Fatalf("tool_error message = %q, want the failure text", errs[0].Error)
	}
	if errs[0].Tool != "echo_tool" {
		t.Fatalf("tool_error tool = %q, want echo_tool", errs[0].Tool)
	}
	if errs[0].ToolDisplay != "Checking disk" {
		t.Fatalf("tool_error display = %q, want Checking disk", errs[0].ToolDisplay)
	}
	started := rec.ofKind(core.AnalyzeEventToolStarted)
	if len(started) != 1 || started[0].Seq != errs[0].Seq {
		t.Fatalf("tool_error is not correlated with its start: %+v vs %+v", started, errs)
	}
	traces := c.ordered()
	if len(traces) != 1 || traces[0].Name != "echo_tool" || traces[0].Error != "dispatch exploded" {
		t.Fatalf("persisted trace lost the error: %+v", traces)
	}
}

// TestStream_FailingToolStaysStructured pins the pre-existing contract that a
// tool returning an error is reported to the model as a structured result
// rather than aborting the graph. The live stream mirrors that: the operator
// sees the failure in the completion event's payload.
func TestStream_FailingToolStaysStructured(t *testing.T) {
	boom := &errTool{name: "echo_tool"}
	fake := &fakeChat{
		turns:  []*schema.Message{echoToolCallMessage(), schema.AssistantMessage(streamFindingJSON, nil)},
		chunks: 2,
	}
	res, rec, err := runWatched(t, fake, []core.AnalyzeTool{boom})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	finished := rec.ofKind(core.AnalyzeEventToolFinished)
	if len(finished) != 1 {
		t.Fatalf("got %d tool_finished, want 1", len(finished))
	}
	if !strings.Contains(finished[0].Output, errBoom.Error()) {
		t.Fatalf("tool failure not visible in the stream: %q", finished[0].Output)
	}
	if len(res.ToolCalls) != 1 || finished[0].Output != res.ToolCalls[0].Output {
		t.Fatalf("stream and record disagree about the failure: %+v vs %+v", finished[0], res.ToolCalls)
	}
}

// TestStream_ModelDeltasReassemble asserts the chunks the viewer sees are
// exactly the final answer, in order, and that the run still reassembles that
// answer for the parser and the persisted record.
func TestStream_ModelDeltasReassemble(t *testing.T) {
	fake := &fakeChat{turns: []*schema.Message{schema.AssistantMessage(streamFindingJSON, nil)}, chunks: 4}
	res, rec, err := runWatched(t, fake, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	deltas := rec.ofKind(core.AnalyzeEventModelDelta)
	if len(deltas) < 2 {
		t.Fatalf("got %d deltas, want the answer split across several chunks", len(deltas))
	}
	var sb strings.Builder
	for _, d := range deltas {
		if d.Output == "" {
			t.Fatalf("an empty chunk was emitted as a delta")
		}
		sb.WriteString(d.Output)
	}
	if sb.String() != streamFindingJSON {
		t.Fatalf("deltas reassemble to %q, want %q", sb.String(), streamFindingJSON)
	}
	if res.RawResponse != streamFindingJSON {
		t.Fatalf("RawResponse = %q, want %q", res.RawResponse, streamFindingJSON)
	}
	if res.Finding == nil || res.Finding.Title != "t" {
		t.Fatalf("finding not parsed from the reassembled answer: %+v", res.Finding)
	}
}

// TestStream_EmptyChunkEmitsNoDelta asserts a content-free chunk (providers
// send them to open a stream) produces no delta, so the transcript never gains
// an empty block.
func TestStream_EmptyChunkEmitsNoDelta(t *testing.T) {
	fake := &fakeChat{
		turns:     []*schema.Message{schema.AssistantMessage(streamFindingJSON, nil)},
		chunks:    3,
		leadEmpty: true,
	}
	res, rec, err := runWatched(t, fake, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	deltas := rec.ofKind(core.AnalyzeEventModelDelta)
	if len(deltas) != 3 {
		t.Fatalf("got %d deltas, want 3 (the empty lead chunk must not produce one)", len(deltas))
	}
	if res.RawResponse != streamFindingJSON {
		t.Fatalf("RawResponse = %q, want %q", res.RawResponse, streamFindingJSON)
	}
}

// TestStream_MidStreamErrorFailsRun asserts a stream that breaks part-way
// fails the run with the underlying cause rather than persisting a truncated
// answer as if it were complete.
func TestStream_MidStreamErrorFailsRun(t *testing.T) {
	fake := &fakeChat{
		turns:       []*schema.Message{schema.AssistantMessage(streamFindingJSON, nil)},
		chunks:      4,
		streamErr:   errors.New("connection reset"),
		streamErrAt: 2,
	}
	res, rec, err := runWatched(t, fake, nil)
	if err == nil {
		t.Fatalf("Run succeeded despite a broken stream; result = %+v", res)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("error = %v, want the underlying stream failure", err)
	}
	if res == nil || res.RawResponse != "" {
		t.Fatalf("a truncated answer was persisted as an answer: %+v", res)
	}
	if got := len(rec.ofKind(core.AnalyzeEventModelDelta)); got != 2 {
		t.Fatalf("got %d deltas before the break, want 2", got)
	}
}

// TestStream_ModelFinishedPerTurn asserts every model turn reports completion.
// The UI clears its streaming cursor on model_finished, so a turn that ends
// without one leaves the viewer spinning forever. Under Stream() the framework
// dispatches the stream-output timing, not OnEnd — this is the regression
// guard for that wiring.
func TestStream_ModelFinishedPerTurn(t *testing.T) {
	stub := &stubTool{name: "echo_tool"}
	fake := &fakeChat{
		turns:  []*schema.Message{echoToolCallMessage(), schema.AssistantMessage(streamFindingJSON, nil)},
		chunks: 3,
	}
	_, rec, err := runWatched(t, fake, []core.AnalyzeTool{stub})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	starts := rec.ofKind(core.AnalyzeEventModelStarted)
	ends := rec.ofKind(core.AnalyzeEventModelFinished)
	if len(starts) != 2 {
		t.Fatalf("got %d model_started, want 2", len(starts))
	}
	if len(ends) != len(starts) {
		t.Fatalf("got %d model_finished for %d model_started; a turn never closed", len(ends), len(starts))
	}
	byTurn := map[int]core.AnalyzeEvent{}
	for _, e := range ends {
		byTurn[e.Turn] = e
	}
	if _, ok := byTurn[1]; !ok {
		t.Fatalf("turn 1 never reported finished: %+v", ends)
	}
	final, ok := byTurn[2]
	if !ok {
		t.Fatalf("the answering turn never reported finished: %+v", ends)
	}
	if final.Output != streamFindingJSON {
		t.Fatalf("final model_finished output = %q, want the whole answer", final.Output)
	}
	// A turn's deltas must all precede its own finish, or the viewer clears
	// the cursor and then receives more text.
	for _, d := range rec.ofKind(core.AnalyzeEventModelDelta) {
		if d.Turn == final.Turn && d.Seq > final.Seq {
			t.Fatalf("delta seq %d arrived after model_finished seq %d", d.Seq, final.Seq)
		}
	}
}

// TestStream_TerminalEventPrecededByModelFinish asserts the run does not
// return while a model turn is still emitting: everything the viewer needs has
// been delivered before the caller sends its terminal event.
func TestStream_TerminalEventPrecededByModelFinish(t *testing.T) {
	for i := 0; i < 50; i++ {
		fake := &fakeChat{turns: []*schema.Message{schema.AssistantMessage(streamFindingJSON, nil)}, chunks: 6}
		_, rec, err := runWatched(t, fake, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(rec.ofKind(core.AnalyzeEventModelFinished)) != 1 {
			t.Fatalf("iteration %d: the run returned before the turn finished: %+v", i, rec.all())
		}
	}
}

// TestStream_EventsAreCapped asserts nothing reaches the event stream that the
// persisted record would not already hold: an oversized model chunk goes
// through the same cap as tool output.
func TestStream_EventsAreCapped(t *testing.T) {
	huge := strings.Repeat("z", maxToolOutputBytes*2)
	fake := &fakeChat{turns: []*schema.Message{schema.AssistantMessage(huge, nil)}, chunks: 1}
	_, rec, err := runWatched(t, fake, nil)
	// The oversized answer is not valid finding JSON, so the run fails at the
	// parser — the events are what this test is about.
	if err == nil {
		t.Fatalf("expected the non-JSON answer to fail parsing")
	}
	if got := len(rec.ofKind(core.AnalyzeEventModelDelta)); got == 0 {
		t.Fatalf("no delta was emitted; the cap assertion below is vacuous")
	}
	limit := maxToolOutputBytes + len(`..."truncated"`)
	for _, ev := range rec.all() {
		if len(ev.Output) > limit {
			t.Fatalf("%s event carried %d bytes, above the %d cap", ev.Kind, len(ev.Output), limit)
		}
	}
}
