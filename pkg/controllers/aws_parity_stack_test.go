package controllers

// aws_parity_stack_test.go — HP surface parity for the AWS tools (AX §5).
// One server, two interfaces: the web chat (browser gateway session on the
// routes ui/src/lib/api.ts calls) and the versus-devops CLI binary (API v1,
// gateway secret) against the same app on a loopback listener. The chat
// bundle is the production one (agent.BuildAIsForScope) with the AWS and
// FinOps tools bound the way cmd/main.go binds them; only the model is a
// scripted OpenAI-compatible fake, so both runs are deterministic.
//
// Compared after normalising IDs, timestamps and run-scoped fields: the
// events the server recorded for each session, the ledger entries of each
// run, the final answer, and the model requests. fakeaws counters prove the
// agent turns sent no AWS request and spent no paid Cost Explorer request.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

// EnvParityCLI names the built versus-devops binary the launcher provides.
const EnvParityCLI = "VERSUS_DEVOPS_BIN"

type scriptedCall struct {
	name string
	args func(previous []string) string
}

// scriptedModel answers OpenAI chat completions: for each question it calls
// the planned tools one at a time, then answers from the tool results.
type scriptedModel struct {
	mu       sync.Mutex
	plans    map[string][]scriptedCall
	phase    string
	requests map[string][]any
	server   *httptest.Server
}

// increaseIDPattern reads the first increase id from the cost_increases
// model view.
var increaseIDPattern = regexp.MustCompile(`"increases":\[\{[^{}]*?"id":"([0-9a-f]{16})"`)

func newScriptedModel(t *testing.T, plans map[string][]scriptedCall) *scriptedModel {
	model := &scriptedModel{plans: plans, requests: map[string][]any{}}
	model.server = httptest.NewServer(http.HandlerFunc(model.serve))
	t.Cleanup(model.server.Close)
	return model
}

func (model *scriptedModel) setPhase(phase string) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.phase = phase
}

func (model *scriptedModel) serve(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	var input struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			Name       string `json:"name"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &input)
	var decoded any
	_ = json.Unmarshal(body, &decoded)
	model.mu.Lock()
	model.requests[model.phase] = append(model.requests[model.phase], decoded)
	model.mu.Unlock()

	question, last := "", -1
	for index, message := range input.Messages {
		if message.Role == "user" {
			text, _ := message.Content.(string)
			question, last = text, index
		}
	}
	var results []string
	for _, message := range input.Messages[last+1:] {
		if message.Role == "tool" {
			text, _ := message.Content.(string)
			results = append(results, text)
		}
	}
	var plan []scriptedCall
	for key, calls := range model.plans {
		if strings.Contains(question, key) {
			plan = calls
		}
	}
	if len(results) < len(plan) {
		call := plan[len(results)]
		model.write(writer, input.Stream, "", &[2]string{call.name, call.args(results)}, len(results))
		return
	}
	model.write(writer, input.Stream, scriptedAnswer(results), nil, 0)
}

// scriptedAnswer is derived only from the tool results: equal results give
// equal answers; it quotes the first sentence-like field it finds.
func scriptedAnswer(results []string) string {
	digest := sha256.Sum256([]byte(strings.Join(results, "\n")))
	quote := ""
	for _, key := range []string{`"sentence":"`, `"suspect_summary":"`, `"answer":"`, `"status":"`} {
		for _, result := range results {
			if index := strings.Index(result, key); quote == "" && index >= 0 {
				rest := result[index+len(key):]
				if end := strings.Index(rest, `"`); end >= 0 {
					quote = rest[:end]
				}
			}
		}
	}
	return fmt.Sprintf("From the tools: %s (evidence %s, %d tool results).", quote, hex.EncodeToString(digest[:6]), len(results))
}

func (model *scriptedModel) write(writer http.ResponseWriter, stream bool, content string, call *[2]string, index int) {
	message := map[string]any{"role": "assistant", "content": content}
	finish := "stop"
	if call != nil {
		message["content"] = ""
		message["tool_calls"] = []any{map[string]any{"index": 0, "id": fmt.Sprintf("call_%d", index+1), "type": "function",
			"function": map[string]any{"name": call[0], "arguments": call[1]}}}
		finish = "tool_calls"
	}
	if !stream {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "chatcmpl-parity", "object": "chat.completion", "created": 1, "model": "gpt-parity",
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]any, reason any) {
		encoded, _ := json.Marshal(map[string]any{"id": "chatcmpl-parity", "object": "chat.completion.chunk", "created": 1, "model": "gpt-parity",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
		fmt.Fprintf(writer, "data: %s\n\n", encoded)
	}
	chunk(message, nil)
	chunk(map[string]any{}, finish)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

// ---- normalisation ----

var (
	volatileKeys = map[string]bool{"id": true, "run_id": true, "session_id": true, "turn_id": true, "at": true, "created_at": true,
		"updated_at": true, "started_at": true, "ended_at": true, "completed_at": true, "timestamp": true, "time": true, "seq": true,
		"duration_ms": true, "elapsed_ms": true, "latency_ms": true, "surface": true, "client_version": true, "request_id": true,
		"lease": true, "epoch": true, "created": true, "mode": true}
	timestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	uuidPattern      = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	sessionPattern   = regexp.MustCompile(`\b[0-9a-f]{32}\b`)
)

func normaliseJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range typed {
			if volatileKeys[key] {
				continue
			}
			out[key] = normaliseJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = normaliseJSON(item)
		}
		return out
	case string:
		text := timestampPattern.ReplaceAllString(typed, "<ts>")
		text = uuidPattern.ReplaceAllString(text, "<uuid>")
		return sessionPattern.ReplaceAllString(text, "<session>")
	default:
		return value
	}
}

func canonical(value any) string {
	encoded, _ := json.Marshal(normaliseJSON(value))
	return string(encoded)
}

// ---- stack ----

type parityServer struct {
	t       *testing.T
	app     *fiber.App
	store   storage.Provider
	model   *scriptedModel
	address string
	web     *awsClient
}

// mountParityChat builds the production chat bundle on store, binds it to the
// app and serves the app on a loopback listener for the CLI.
func mountParityChat(t *testing.T, app *fiber.App, store storage.Provider, connector config.AWSConnectorConfig, plans map[string][]scriptedCall) *parityServer {
	t.Helper()
	model := newScriptedModel(t, plans)
	target, _ := url.Parse(model.server.URL)
	client := &http.Client{Transport: analyzeRewriteTransport{target: target, base: http.DefaultTransport}}
	bundle := agent.BuildAIsForScope(config.AgentConfig{AI: config.AgentAIConfig{
		Enable: true, Provider: "openai", APIKey: "parity-key", Model: "gpt-parity", MaxTokens: 512,
	}}, nil, store, tenancy.DefaultOrgScope(), client, config.ConnectorsConfig{AWS: connector})
	if bundle.ChatService == nil {
		t.Fatal("chat service was not built")
	}
	SetChatServiceFactory(bundle.ChatService)
	t.Cleanup(func() { SetChatServiceFactory(nil) })
	NewChatAdminController(nil).Register(app.Group("/api"))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(listener) }()
	t.Cleanup(func() { _ = app.ShutdownWithTimeout(5 * time.Second) })
	web := &awsClient{t: t, app: app}
	web.gatewaySession(awsStackSecret)
	return &parityServer{t: t, app: app, store: store, model: model, address: listener.Addr().String(), web: web}
}

type surfaceRun struct {
	SessionID string
	Answer    string
	Events    []any
	Received  int
	Ledger    []any
	Requests  []any
	Raw       string
}

// webTurn is what the browser does: create a session and post a message on
// /api/admin/chat (ui/src/lib/api.ts), reading the SSE stream to the end.
func (server *parityServer) webTurn(question string) surfaceRun {
	t := server.t
	server.model.setPhase("web")
	response, body := server.web.do(http.MethodPost, "/api/admin/chat/sessions", "")
	if response.StatusCode != fiber.StatusOK && response.StatusCode != fiber.StatusCreated {
		t.Fatalf("web session = %d %s", response.StatusCode, body)
	}
	var session struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &session)
	payload, _ := json.Marshal(map[string]string{"message": question})
	response, stream := server.web.do(http.MethodPost, "/api/admin/chat/sessions/"+session.ID+"/messages", string(payload))
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("web turn = %d %s", response.StatusCode, stream)
	}
	return surfaceRun{SessionID: session.ID, Received: bytes.Count(stream, []byte("\ndata:")) + bytes.Count(stream[:min(5, len(stream))], []byte("data:")), Raw: string(stream)}
}

// cliTurn runs the released CLI binary: `versus-devops ask --output
// stream-json` against the same server.
func (server *parityServer) cliTurn(question string) (surfaceRun, bool) {
	t := server.t
	binary := os.Getenv(EnvParityCLI)
	if binary == "" {
		return surfaceRun{}, false
	}
	server.model.setPhase("cli")
	before := server.sessionIDs()
	command := exec.Command(binary, "ask", "--output", "stream-json", question)
	command.Env = append(os.Environ(), "VERSUS_SERVER=http://"+server.address, "VERSUS_GATEWAY_SECRET="+awsStackSecret, "VERSUS_TOKEN=")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("cli ask: %v; stderr=%s", err, stderr.String())
	}
	received := 0
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			received++
		}
	}
	for id := range server.sessionIDs() {
		if !before[id] {
			return surfaceRun{SessionID: id, Received: received, Raw: stdout.String()}, true
		}
	}
	t.Fatalf("the CLI turn created no session; stdout=%s", stdout.String())
	return surfaceRun{}, false
}

func (server *parityServer) sessionIDs() map[string]bool {
	response, body := server.web.do(http.MethodGet, "/api/v1/agent/sessions", "")
	if response.StatusCode != fiber.StatusOK {
		server.t.Fatalf("sessions = %d %s", response.StatusCode, body)
	}
	var list struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	_ = json.Unmarshal(body, &list)
	out := map[string]bool{}
	for _, session := range list.Sessions {
		out[session.ID] = true
	}
	return out
}

// complete fills the server-side record of a run: replayed events, final
// answer, ledger entries and model requests.
func (server *parityServer) complete(run *surfaceRun, phase string, ledgerRun string) {
	t := server.t
	response, body := server.web.do(http.MethodGet, "/api/v1/agent/sessions/"+run.SessionID+"/events", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("events = %d %s", response.StatusCode, body)
	}
	var replay struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.Unmarshal(body, &replay)
	var answer strings.Builder
	for _, event := range replay.Events {
		run.Events = append(run.Events, event)
		if event["kind"] == "model_delta" {
			text, _ := event["delta"].(string)
			answer.WriteString(text)
		}
	}
	run.Answer = answer.String()
	run.Ledger = server.ledgerEntries(ledgerRun)
	server.model.mu.Lock()
	run.Requests = append([]any(nil), server.model.requests[phase]...)
	server.model.mu.Unlock()
}

// ledgerRuns lists the run ids in the ledger, oldest first.
func (server *parityServer) ledgerRuns() []string {
	blobs, err := server.store.ListBlobs("ledger/")
	if err != nil {
		server.t.Fatalf("ledger: %v", err)
	}
	type run struct {
		id string
		at string
	}
	var runs []run
	for _, blob := range blobs {
		if strings.HasSuffix(blob.Name, "/trigger.json") {
			parts := strings.Split(blob.Name, "/")
			var trigger struct {
				At string `json:"at"`
			}
			_ = json.Unmarshal(blob.Data, &trigger)
			runs = append(runs, run{id: parts[len(parts)-2], at: trigger.At})
		}
	}
	sort.Slice(runs, func(left, right int) bool { return runs[left].at < runs[right].at })
	out := make([]string, 0, len(runs))
	for _, item := range runs {
		out = append(out, item.id)
	}
	return out
}

func (server *parityServer) ledgerEntries(runID string) []any {
	blobs, err := server.store.ListBlobs("ledger/")
	if err != nil {
		server.t.Fatalf("ledger: %v", err)
	}
	var names []string
	data := map[string][]byte{}
	for _, blob := range blobs {
		if strings.Contains(blob.Name, "/runs/"+runID+"/entries/") {
			names = append(names, blob.Name)
			data[blob.Name] = blob.Data
		}
	}
	sort.Strings(names)
	var out []any
	for _, name := range names {
		var entry any
		_ = json.Unmarshal(data[name], &entry)
		out = append(out, entry)
	}
	return out
}

type parityReport struct {
	Question        string         `json:"question"`
	CLI             string         `json:"cli"`
	EventsEqual     bool           `json:"events_equal"`
	LedgerEqual     bool           `json:"ledger_equal"`
	AnswerEqual     bool           `json:"answer_equal"`
	RequestsEqual   bool           `json:"model_requests_equal"`
	Answer          string         `json:"answer"`
	ToolsCalled     []string       `json:"tools_called"`
	LedgerKinds     []string       `json:"ledger_kinds"`
	EventKinds      []string       `json:"event_kinds"`
	ClientReceived  map[string]int `json:"client_received_lines"`
	AWSRequests     int64          `json:"aws_requests_by_agent_turns"`
	PaidCostByTurns int64          `json:"paid_cost_requests_by_agent_turns"`
}

func kindsOf(values []any, key string) []string {
	var out []string
	for _, value := range values {
		if text, _ := jsonPath(value, key).(string); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func saveParity(t *testing.T, name string, value any) {
	dir := os.Getenv("HARNESS_RESULTS_DIR")
	if dir == "" {
		return
	}
	folder := filepath.Join(dir, "parity")
	_ = os.MkdirAll(folder, 0o755)
	encoded, _ := json.MarshalIndent(value, "", "  ")
	_ = os.WriteFile(filepath.Join(folder, name+".json"), encoded, 0o600)
}

// runParity asks one question through both interfaces and compares.
func runParity(t *testing.T, server *parityServer, name, question string, counters func() (int64, int64), wantTools []string) parityReport {
	t.Helper()
	totalBefore, paidBefore := counters()
	web := server.webTurn(question)
	cli, ran := server.cliTurn(question)
	totalAfter, paidAfter := counters()
	runs := server.ledgerRuns()
	report := parityReport{Question: question, AWSRequests: totalAfter - totalBefore, PaidCostByTurns: paidAfter - paidBefore,
		ClientReceived: map[string]int{"web_sse_lines": web.Received}}
	if len(runs) < 1 {
		t.Fatal("no ledger run")
	}
	server.complete(&web, "web", runs[0])
	report.Answer = web.Answer
	report.ToolsCalled = kindsOfTools(web.Ledger)
	report.LedgerKinds = kindsOf(web.Ledger, "kind")
	report.EventKinds = kindsOf(web.Events, "kind")
	saveParity(t, name+"-web", web)
	if report.AWSRequests != 0 || report.PaidCostByTurns != 0 {
		t.Errorf("agent turns caused %d AWS requests and %d paid Cost Explorer requests", report.AWSRequests, report.PaidCostByTurns)
	}
	for _, tool := range wantTools {
		if !containsStringValue(report.ToolsCalled, tool) {
			t.Errorf("tool %s not called; ledger tools %v, ledger kinds %v", tool, report.ToolsCalled, report.LedgerKinds)
		}
	}
	for _, entry := range web.Ledger {
		if jsonString(entry, "state") == "failed" {
			t.Errorf("a tool call failed: %v", entry)
		}
	}
	if !strings.HasPrefix(web.Answer, "From the tools:") {
		t.Errorf("web answer = %q", web.Answer)
	}
	if !ran {
		report.CLI = "blocked: " + EnvParityCLI + " not set (CLI binary not built)"
		t.Errorf("%s", report.CLI)
		return report
	}
	report.CLI = "versus-devops ask --output stream-json"
	report.ClientReceived["cli_stream_json_lines"] = cli.Received
	if len(runs) < 2 {
		t.Fatalf("the CLI turn wrote no ledger run: %v", runs)
	}
	server.complete(&cli, "cli", runs[1])
	saveParity(t, name+"-cli", cli)
	report.EventsEqual = canonical(web.Events) == canonical(cli.Events)
	report.LedgerEqual = canonical(web.Ledger) == canonical(cli.Ledger)
	report.AnswerEqual = web.Answer == cli.Answer
	report.RequestsEqual = canonical(web.Requests) == canonical(cli.Requests)
	if !report.EventsEqual {
		t.Errorf("events differ:\nweb %s\ncli %s", canonical(web.Events), canonical(cli.Events))
	}
	if !report.LedgerEqual {
		t.Errorf("ledger differs:\nweb %s\ncli %s", canonical(web.Ledger), canonical(cli.Ledger))
	}
	if !report.AnswerEqual {
		t.Errorf("answers differ: web %q cli %q", web.Answer, cli.Answer)
	}
	if !report.RequestsEqual {
		t.Errorf("model requests differ:\nweb %s\ncli %s", canonical(web.Requests), canonical(cli.Requests))
	}
	return report
}

func kindsOfTools(entries []any) []string {
	var out []string
	for _, entry := range entries {
		for _, key := range []string{"tool", "tool_name", "name"} {
			if name, _ := jsonPath(entry, key).(string); name != "" {
				out = append(out, name)
			}
			if name, _ := jsonPath(entry, "payload", key).(string); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

var (
	parityObservationsMu sync.Mutex
	parityObservations   = map[string]any{}
)

// TestAWSAgentParityStackE2E is the HP parity verdict for the AWS tools.
func TestAWSAgentParityStackE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("integration suite needs the fakeaws HTTP stack")
	}
	groups := []struct {
		name string
		run  func(*testing.T)
	}{
		{"aws_connection_healthy", awsConnectionParityGroup},
		{"aws_cost_up_in_october", awsCostParityGroup},
	}
	results := make([]map[string]string, 0, len(groups))
	for _, group := range groups {
		verdict := "fail"
		if t.Run(group.name, group.run) {
			verdict = "pass"
		}
		results = append(results, map[string]string{"group": group.name, "verdict": verdict})
	}
	if dir := os.Getenv("HARNESS_RESULTS_DIR"); dir != "" {
		parityObservationsMu.Lock()
		encoded, _ := json.MarshalIndent(map[string]any{"suite": "aws-parity", "run_id": os.Getenv("HARNESS_RUN_ID"),
			"groups": results, "observations": parityObservations}, "", "  ")
		parityObservationsMu.Unlock()
		_ = os.WriteFile(filepath.Join(dir, "aws-parity-verdict.json"), encoded, 0o600)
	}
}

func observeParity(name string, report parityReport) {
	parityObservationsMu.Lock()
	defer parityObservationsMu.Unlock()
	parityObservations[name] = report
}

// "Is my AWS connection healthy?" on fakeaws bad-deploy with the Staging
// account, whose ElastiCache is denied.
func awsConnectionParityGroup(t *testing.T) {
	stack := newAWSStack(t, "bad-deploy", awsStackOptions{enabled: true})
	client := &awsClient{t: t, app: stack.app}
	client.gatewaySession(awsStackSecret)
	start := connectAccount(t, client)
	commitStackAccount(t, client, start.DraftID, "Staging", []string{"us-east-1", "eu-west-1"})
	view, _ := readSettings(t, client)
	if len(view.Accounts) != 1 || !containsStringValue(view.Accounts[0].Services.DeniedServices, "elasticache") {
		t.Fatalf("setup: %+v", view.Accounts)
	}
	agent.ConfigureAWS(func() *awssettings.Service { return stack.service })
	t.Cleanup(func() { agent.ConfigureAWS(nil) })
	server := mountParityChat(t, stack.app, storage.NewMemory(), stack.readConfig(), map[string][]scriptedCall{
		"AWS connection healthy": {{name: "get_aws_connection", args: func([]string) string { return "{}" }}},
	})
	counters := func() (int64, int64) { return stack.fake.TotalRequests(), stack.fake.PaidCostRequests() }
	report := runParity(t, server, "aws-connection", "Is my AWS connection healthy?", counters, []string{"get_aws_connection"})
	if !strings.Contains(report.Answer+canonical(report), "elasticache") && !strings.Contains(strings.ToLower(report.Answer), "denied") {
		t.Logf("answer does not quote the denied service: %q", report.Answer)
	}
	observeParity("aws_connection_healthy", report)
}

// "Why did AWS cost go up in October?" on fakeaws cost-nat-spike.
func awsCostParityGroup(t *testing.T) {
	stack := natSpikeStack(t)
	agent.ConfigureFinOps(stack.backend.Reader)
	t.Cleanup(func() { agent.ConfigureFinOps(nil) })
	connector := config.GetConfig().Connectors.AWS
	server := mountParityChat(t, stack.app, storage.NewMemory(), connector, map[string][]scriptedCall{
		"cost go up in October": {
			{name: "cost_increases", args: func([]string) string { return `{"period":"2026-10"}` }},
			{name: "cost_increase_detail", args: func(previous []string) string {
				match := increaseIDPattern.FindStringSubmatch(previous[0])
				id := ""
				if match != nil {
					id = match[1]
				}
				return `{"id":"` + id + `","period":"2026-10"}`
			}},
		},
	})
	counters := func() (int64, int64) { return stack.fake.TotalRequests(), stack.fake.PaidCostRequests() }
	report := runParity(t, server, "aws-cost-october", "Why did AWS cost go up in October?", counters, []string{"cost_increases", "cost_increase_detail"})
	observeParity("aws_cost_up_in_october", report)
}
