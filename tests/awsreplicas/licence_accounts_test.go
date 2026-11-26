package awsreplicas

// The licence account test connects two AWS accounts through the Settings
// wizard on the OSS server, then runs the licensed Enterprise server, the OSS
// server again (the licence lapsed) and the licensed server once more, one
// after the other, on one Postgres database. Accounts the wizard stored under
// the default organization must keep working under the licence, an edit or a
// removal under the licence must never write the default organization's rows,
// and the default organization's state must come back unchanged when the
// licence lapses.
//
// Every AWS call goes to the tests/fakeaws collector world (bad-deploy)
// through a front per server that keeps a call log; billing calls are
// answered from a single-account cost data set, so each account is its own
// payer with its own cost. The front can pin the external ID each role
// accepts, so a server signs in only with the stored role and external ID,
// and can refuse one account's sign-in. The server binaries carry the issue
// rules of tests/awsreplicas/issuerule (a build overlay), so each account's
// firing alarm opens a critical issue; the rules report every evaluation to a
// gate in this test. The agent tools are read through the chat API with a
// scripted OpenAI-compatible model in this process.
//
// It skips unless these are set, besides TEST_POSTGRES_DSN and
// AWS_REPLICAS_DIR:
//
//	AWS_ISSUES_SERVER_BIN      the OSS server (harness_awsendpoint, issuerule)
//	AWS_ISSUES_ENTERPRISE_BIN  the Enterprise server built the same way, with
//	                           a test licence verifier
//	AWS_ISSUES_LICENSE_KEY     a test licence that build accepts
//	AWS_ISSUES_LICENSE_ORG     the organization the licence names

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/tests/awsreplicas/issuerule"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

const (
	wizardAccountA = "111111111111"
	wizardAccountB = "222222222222"
	wizardNameA    = "Checkout production"
	wizardNameB    = "Data platform"
	wizardRenamedA = "Checkout production EU"
	licenceModel   = "licence-model"
	// licenceGuard bounds one wait for collection, an evaluation or a
	// refresh. Health checks run every 30 s, the cost scheduler and the
	// issue evaluator at most once a minute, so this is a hang guard.
	licenceGuard = 5 * time.Minute
)

// billingServices are the SigV4 service names the cost data set answers.
var billingServices = map[string]bool{"ce": true, "budgets": true, "cost-optimization-hub": true, "compute-optimizer": true}

// ---- fake AWS front with a call log ----

type awsCall struct {
	Seq        int    `json:"seq"`
	Service    string `json:"service"`
	Operation  string `json:"operation"`
	Account    string `json:"account"`
	RoleARN    string `json:"role_arn,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Status     int    `json:"status"`
}

type wizardFront struct {
	fake   *fakeaws.Server
	server *httptest.Server

	mu     sync.Mutex
	calls  []awsCall
	pinned map[string]string // role ARN -> the one external ID it accepts
	denied map[string]bool   // account -> sign-in refused
}

func newWizardFront(fake *fakeaws.Server) *wizardFront {
	f := &wizardFront{fake: fake, pinned: map[string]string{}, denied: map[string]bool{}}
	f.server = httptest.NewServer(f)
	return f
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func operationOf(request *http.Request, body []byte) string {
	if target := request.Header.Get("X-Amz-Target"); target != "" {
		return target[strings.LastIndex(target, ".")+1:]
	}
	if form, err := url.ParseQuery(string(body)); err == nil && form.Get("Action") != "" {
		return form.Get("Action")
	}
	if index := strings.LastIndex(request.URL.Path, "/operation/"); index >= 0 {
		return request.URL.Path[index+len("/operation/"):]
	}
	return request.Method + " " + request.URL.Path
}

func (f *wizardFront) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(request.Body, 8<<20))
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	call := awsCall{Operation: operationOf(request, body), Account: twelveDigits.FindString(request.Header.Get("X-Amz-Security-Token"))}
	if match := credentialScope.FindStringSubmatch(request.Header.Get("Authorization")); match != nil {
		call.Service = match[1]
	}
	scenario := fakeaws.ScenarioBadDeploy
	if billingServices[call.Service] {
		scenario = fakeaws.ScenarioCostCredits
	}
	request.Header.Set(fakeaws.ScenarioHeader, scenario)
	if call.Operation == "AssumeRole" {
		form, _ := url.ParseQuery(string(body))
		call.RoleARN, call.ExternalID = form.Get("RoleArn"), form.Get("ExternalId")
		call.Account = twelveDigits.FindString(call.RoleARN)
		f.mu.Lock()
		pinned, isPinned := f.pinned[call.RoleARN]
		denied := f.denied[call.Account]
		f.mu.Unlock()
		if denied || isPinned && pinned != call.ExternalID {
			call.Status = http.StatusForbidden
			f.record(call)
			writer.Header().Set("Content-Type", "text/xml")
			writer.WriteHeader(http.StatusForbidden)
			fmt.Fprint(writer, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type>`+
				`<Code>AccessDenied</Code><Message>User is not authorized to perform: sts:AssumeRole</Message></Error>`+
				`<RequestId>licence-front</RequestId></ErrorResponse>`)
			return
		}
	}
	recorder := &statusWriter{ResponseWriter: writer, status: http.StatusOK}
	f.fake.ServeHTTP(recorder, request)
	call.Status = recorder.status
	f.record(call)
}

func (f *wizardFront) record(call awsCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call.Seq = len(f.calls)
	f.calls = append(f.calls, call)
}

// mark is where the call log ends now.
func (f *wizardFront) mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// since lists the calls after mark that match.
func (f *wizardFront) since(mark int, match func(awsCall) bool) []awsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []awsCall
	for _, call := range f.calls[min(mark, len(f.calls)):] {
		if match(call) {
			out = append(out, call)
		}
	}
	return out
}

func (f *wizardFront) pin(roleARN, externalID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if externalID == "" {
		delete(f.pinned, roleARN)
		return
	}
	f.pinned[roleARN] = externalID
}

func (f *wizardFront) deny(account string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied[account] = on
}

// signIns are the sign-ins after mark for one account that answered status.
func (f *wizardFront) signIns(mark int, account string, status int) []awsCall {
	return f.since(mark, func(call awsCall) bool {
		return call.Operation == "AssumeRole" && call.Account == account && call.Status == status
	})
}

// billing counts the billing calls after mark per account and operation.
func (f *wizardFront) billing(mark int) map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, call := range f.since(mark, func(call awsCall) bool { return billingServices[call.Service] }) {
		if out[call.Account] == nil {
			out[call.Account] = map[string]int{}
		}
		out[call.Account][call.Operation]++
	}
	return out
}

// ---- issue rule gate ----

// evaluationGate counts the issue rule's evaluations per server and account.
type evaluationGate struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  map[string]int
}

func newEvaluationGate() *evaluationGate {
	g := &evaluationGate{calls: map[string]int{}}
	g.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var call issuerule.Call
		_ = json.NewDecoder(io.LimitReader(request.Body, 1<<16)).Decode(&call)
		g.mu.Lock()
		g.calls[call.Replica+"/"+call.Account]++
		g.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{}`))
	}))
	return g
}

func (g *evaluationGate) count(server, account string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[server+"/"+account]
}

// ---- scripted model ----

// toolModel answers OpenAI chat completions. A question naming an AWS tool
// gets one call of it; the tool result the server sends back is kept.
type toolModel struct {
	server  *httptest.Server
	mu      sync.Mutex
	results map[string][]string
}

var modelTools = map[string]string{"get_aws_connection": `{}`, "get_aws_changes": `{"window":"24h","limit":50}`}

func newToolModel() *toolModel {
	m := &toolModel{results: map[string][]string{}}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

func (m *toolModel) serve(writer http.ResponseWriter, request *http.Request) {
	if !strings.HasSuffix(request.URL.Path, "/chat/completions") {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"object":"list","data":[{"id":"` + licenceModel + `","object":"model"}]}`))
		return
	}
	body, _ := io.ReadAll(request.Body)
	var input struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &input)
	question, last := "", -1
	for index, message := range input.Messages {
		if message.Role == "user" {
			question, last = messageText(message.Content), index
		}
	}
	tool := ""
	for name := range modelTools {
		if strings.Contains(question, name) {
			tool = name
		}
	}
	var results []string
	if last >= 0 {
		for _, message := range input.Messages[last+1:] {
			if message.Role == "tool" {
				results = append(results, messageText(message.Content))
			}
		}
	}
	if tool != "" && len(results) == 0 {
		writeCompletion(writer, input.Stream, "", tool, modelTools[tool])
		return
	}
	if tool != "" {
		m.mu.Lock()
		m.results[tool] = append(m.results[tool], strings.Join(results, "\n"))
		m.mu.Unlock()
	}
	writeCompletion(writer, input.Stream, fmt.Sprintf("Read %d tool results.", len(results)), "", "")
}

// latest is the last result the server sent for tool.
func (m *toolModel) latest(tool string) (string, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.results[tool]
	if len(list) == 0 {
		return "", 0
	}
	return list[len(list)-1], len(list)
}

func messageText(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		var out strings.Builder
		for _, part := range typed {
			if item, ok := part.(map[string]any); ok {
				text, _ := item["text"].(string)
				out.WriteString(text)
			}
		}
		return out.String()
	}
	return ""
}

func writeCompletion(writer http.ResponseWriter, stream bool, content, tool, args string) {
	message := map[string]any{"role": "assistant", "content": content}
	finish := "stop"
	if tool != "" {
		message["content"] = ""
		message["tool_calls"] = []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": tool, "arguments": args}}}
		finish = "tool_calls"
	}
	if !stream {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "chatcmpl-licence", "object": "chat.completion", "created": 1, "model": licenceModel,
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]any, reason any) {
		encoded, _ := json.Marshal(map[string]any{"id": "chatcmpl-licence", "object": "chat.completion.chunk", "created": 1, "model": licenceModel,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
		fmt.Fprintf(writer, "data: %s\n\n", encoded)
	}
	chunk(message, nil)
	chunk(map[string]any{}, finish)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

// ---- server processes ----

type wizardProcess struct {
	bin, dsn, licence string
	front             *wizardFront
	gate              *evaluationGate
	model             *toolModel
}

// startWizard starts the server with wizard accounts only, collection on,
// cost data on and the chat agent on the scripted model.
func (r *replica) startWizard(p wizardProcess) {
	t := r.t
	t.Helper()
	config := fmt.Sprintf(`name: aws-licence-%s
host: 127.0.0.1
port: %d
gateway_secret: ${GATEWAY_SECRET}
queue:
  enable: false
storage:
  type: postgres
  postgres:
    dsn: ${POSTGRES_DSN}
agent:
  enable: true
  ai:
    enable: true
    provider: openai
    base_url: ${LICENCE_MODEL_URL}
    model: %s
    api_key: ${LICENCE_MODEL_KEY}
    max_tokens: 512
`, r.name, r.port, licenceModel)
	connectors := `connectors:
  aws:
    enable: true
    accounts: []
    tags:
      value_allowlist: [team]
    budgets:
      max_requests_per_second: 100
    optional_data:
      cost_explorer: true
    collect:
      enable: true
      intervals:
        inventory: 15m
        health: 30s
        changes: 5m
    finops:
      team_tag_key: team
      max_paid_requests_per_day: 400
      refresh_time: "00:00"
      report:
        timezone: UTC
`
	if err := os.WriteFile(filepath.Join(r.dir, "config", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "config", "connectors.yaml"), []byte(connectors), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(r.dir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(logFile, "\n==== %s start %s licensed=%t\n", r.name, time.Now().UTC().Format(time.RFC3339), p.licence != "")
	cmd := exec.Command(p.bin)
	cmd.Dir = r.dir
	// Nothing from the caller's shell, so no real AWS credential, profile or
	// licence can be picked up; every AWS request goes to the front.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.dir,
		"TZ=UTC",
		"GATEWAY_SECRET=" + gatewaySecret,
		"POSTGRES_DSN=" + p.dsn,
		"VERSUS_HARNESS_AWS_ENDPOINT_URL=" + p.front.server.URL,
		"AWS_ACCESS_KEY_ID=ASIAVERSUSBASE00001",
		"AWS_SECRET_ACCESS_KEY=versus-licence-base-secret",
		"AWS_REGION=us-east-1",
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_CONFIG_FILE=" + filepath.Join(r.dir, "no-aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(r.dir, "no-aws-credentials"),
		issuerule.GateEnv + "=" + p.gate.server.URL,
		issuerule.ReplicaEnv + "=" + r.name,
		"LICENSE_KEY=" + p.licence,
		"LICENSE_CACHE_PATH=",
		"VERSUS_ENTERPRISE_COOKIE_INSECURE=1",
		"LICENCE_MODEL_URL=" + p.model.server.URL + "/v1",
		"LICENCE_MODEL_KEY=licence-model-placeholder",
	}
	if err := r.launch(cmd, logFile); err != nil {
		t.Fatalf("%s: start: %v", r.name, err)
	}
	r.cmd, r.log, r.done = cmd, logFile, make(chan struct{})
	go func(done chan struct{}) { _ = cmd.Wait(); close(done) }(r.done)
	waitFor(t, r.name+" to answer /healthz", 90*time.Second, func() bool {
		select {
		case <-r.done:
			t.Fatalf("%s exited during boot; see %s", r.name, filepath.Join(r.dir, "server.log"))
		default:
		}
		response, err := http.Get(r.url() + "/healthz")
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
}

// session calls one server as its admin: the gateway secret on OSS, a
// signed-in session cookie on the licensed server.
type session struct {
	r      *replica
	cookie string
}

func (s session) do(method, path, body string) answer {
	if s.cookie == "" {
		return s.r.call(method, path, body)
	}
	return s.r.callWith(method, path, body, map[string]string{"Cookie": s.cookie})
}

// signIn signs in as the licensed server's admin. The one-time password is
// printed at the first licensed boot; later boots reuse it.
func signIn(t *testing.T, r *replica, from int64, password *string) session {
	t.Helper()
	if *password == "" {
		waitFor(t, "the licensed server's one-time admin credentials", 60*time.Second, func() bool {
			data, err := os.ReadFile(filepath.Join(r.dir, "server.log"))
			if err != nil || int64(len(data)) < from {
				return false
			}
			if match := bootstrapPassword.FindSubmatch(data[from:]); match != nil {
				*password = string(match[1])
				return true
			}
			return false
		})
	}
	login := r.callWith(http.MethodPost, "/enterprise/api/auth/local/login", `{"username":"admin","password":"`+*password+`"}`, nil)
	cookie := ""
	for _, line := range login.header.Values("Set-Cookie") {
		if name, _, found := strings.Cut(line, ";"); found && cookie == "" {
			cookie = name
		}
	}
	if login.status != http.StatusOK || cookie == "" {
		t.Fatalf("licensed admin login = %d %.300s", login.status, login.body)
	}
	return session{r: r, cookie: cookie}
}

// connectWizard runs the setup wizard for one account.
func (s session) connectWizard(t *testing.T, account, name string) answer {
	t.Helper()
	created := s.do(http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`)
	if created.status != http.StatusCreated {
		t.Fatalf("%s: draft = %d %.300s", s.r.name, created.status, created.body)
	}
	draft, _ := created.json()["draft_id"].(string)
	verify := s.do(http.MethodPost, "/api/admin/aws/connect/drafts/"+draft+"/verify", `{"role_arn":"`+roleOf(account)+`"}`)
	if verify.status != http.StatusAccepted {
		t.Fatalf("%s: verify %s = %d %.300s", s.r.name, account, verify.status, verify.body)
	}
	var view answer
	waitFor(t, "the draft for "+account+" to verify", 60*time.Second, func() bool {
		view = s.do(http.MethodGet, "/api/admin/aws/connect/drafts/"+draft, "")
		status, _ := view.json()["status"].(string)
		if status == "failed" {
			t.Fatalf("%s: draft for %s failed: %.400s", s.r.name, account, view.body)
		}
		return status == "verified"
	})
	encoded, _ := json.Marshal(map[string]any{"draft_id": draft, "display_name": name, "regions": []string{"us-east-1"}})
	commit := s.do(http.MethodPost, "/api/admin/aws/accounts", string(encoded))
	if commit.status != http.StatusCreated {
		t.Fatalf("%s: commit %s = %d %.300s", s.r.name, account, commit.status, commit.body)
	}
	return commit
}

func roleOf(account string) string { return "arn:aws:iam::" + account + ":role/versus-reader" }

// chatTool asks the agent to use one AWS tool and returns the result the
// server sent the model.
func (s session) chatTool(t *testing.T, m *toolModel, tool string) (string, error) {
	t.Helper()
	_, before := m.latest(tool)
	created := s.do(http.MethodPost, "/api/admin/chat/sessions", "")
	if created.status != http.StatusOK && created.status != http.StatusCreated {
		return "", fmt.Errorf("chat session = %d %.300s", created.status, created.body)
	}
	id, _ := created.json()["id"].(string)
	payload, _ := json.Marshal(map[string]string{"message": "Answer from AWS state. Use " + tool + "."})
	turn := s.do(http.MethodPost, "/api/admin/chat/sessions/"+id+"/messages", string(payload))
	if turn.status != http.StatusOK {
		return "", fmt.Errorf("chat turn = %d %.300s", turn.status, turn.body)
	}
	result, after := m.latest(tool)
	if after == before {
		return "", fmt.Errorf("the model never received a %s result: %.300s", tool, turn.body)
	}
	return result, nil
}

// ---- database reads ----

type storedBlob struct {
	data    []byte
	updated time.Time
	found   bool
}

func (d *database) rawBlob(name string) storedBlob {
	d.t.Helper()
	var out storedBlob
	err := d.db.QueryRow(`SELECT data, updated_at FROM vs_blobs WHERE name = $1`, name).Scan(&out.data, &out.updated)
	if errors.Is(err, sql.ErrNoRows) {
		return storedBlob{}
	}
	if err != nil {
		d.t.Fatalf("blob %s: %v", name, err)
	}
	out.found = true
	return out
}

// accountRows counts one organization's rows of one account in the
// inventory, change and cost tables.
func (d *database) accountRows(org, account string) map[string]int {
	out := map[string]int{}
	for _, table := range []string{"aws_resources", "aws_sync_units", "aws_changes"} {
		out[table] = d.count(`SELECT count(*) FROM `+table+` WHERE org_id = $1 AND account = $2`, org, account)
	}
	out["finops_cost_lines"] = d.count(`SELECT count(*) FROM finops_cost_lines WHERE org_id = $1 AND payer = $2`, org, account)
	return out
}

func (d *database) accountIssues(account string) []issueRow {
	d.t.Helper()
	rows, err := d.db.Query(`SELECT org_id, id, rule, severity, state, version, coalesce(payload::text, '') FROM aws_issues
		WHERE account_id = $1 AND rule = $2 ORDER BY org_id, id`, account, issuerule.FiringRuleID)
	if err != nil {
		d.t.Fatal(err)
	}
	defer rows.Close()
	var out []issueRow
	for rows.Next() {
		var row issueRow
		if err := rows.Scan(&row.org, &row.id, &row.rule, &row.severity, &row.state, &row.version, &row.payload); err != nil {
			d.t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		d.t.Fatal(err)
	}
	return out
}

// noLongerConnected counts the issue events that resolved an account's
// issue as no longer connected, in every organization.
func (d *database) noLongerConnected(account string) int {
	return d.count(`SELECT count(*) FROM aws_issue_events e JOIN aws_issues i ON i.org_id = e.org_id AND i.id = e.issue_id
		WHERE i.account_id = $1 AND e.detail = 'account no longer connected'`, account)
}

// costMonth is the latest month that holds one payer's cost lines.
func (d *database) costMonth(org, payer string) string {
	d.t.Helper()
	var month sql.NullString
	if err := d.db.QueryRow(`SELECT to_char(max(day), 'YYYY-MM') FROM finops_cost_lines WHERE org_id = $1 AND payer = $2`, org, payer).Scan(&month); err != nil {
		d.t.Fatal(err)
	}
	return month.String
}

// clearScheduledOn makes one organization's next cost tick run the
// scheduled refresh again. It reports whether the status record exists.
func (d *database) clearScheduledOn(org string) bool {
	name := "aws/" + org + "/finops/status"
	doc := d.blob(name)
	if doc == nil {
		return false
	}
	delete(doc, "scheduled_on")
	d.writeBlob(name, doc)
	return true
}

func (d *database) scheduledOn(org string) string {
	value, _ := d.blob("aws/" + org + "/finops/status")["scheduled_on"].(string)
	return value
}

// ---- what a user sees ----

// view is what one server shows for each account.
type view struct {
	Names    map[string]string `json:"names"`
	Statuses map[string]string `json:"statuses"`
	Changes  map[string]bool   `json:"changes"`
	Coverage map[string]bool   `json:"coverage"`
	// CoverageSummary is each account's coverage summary, "read of total".
	CoverageSummary map[string]string `json:"coverage_summary"`
	FinOps          map[string]bool   `json:"finops"`
	Issues          map[string]bool   `json:"issues"`
	Compare         map[string]int    `json:"compare_snapshots"`
	Codes           map[string]int    `json:"status_codes"`
	CostMonth       string            `json:"cost_month"`
}

func readView(s session, month string, from time.Time) view {
	v := view{Names: map[string]string{}, Statuses: map[string]string{}, Changes: map[string]bool{}, Coverage: map[string]bool{},
		CoverageSummary: map[string]string{}, FinOps: map[string]bool{}, Issues: map[string]bool{}, Compare: map[string]int{}, Codes: map[string]int{}, CostMonth: month}
	settings := s.do(http.MethodGet, "/api/admin/aws/settings", "")
	v.Codes["settings"] = settings.status
	if list, ok := settings.json()["accounts"].([]any); ok {
		for _, item := range list {
			entry, _ := item.(map[string]any)
			id, _ := entry["account_id"].(string)
			v.Names[id], _ = entry["display_name"].(string)
			v.Statuses[id], _ = entry["status"].(string)
		}
	}
	changes := s.do(http.MethodGet, "/api/admin/aws/changes?window=24h&limit=200", "")
	coverage := s.do(http.MethodGet, "/api/admin/aws/coverage", "")
	finops := s.do(http.MethodGet, "/api/admin/aws/finops/accounts?period="+month, "")
	issues := s.do(http.MethodGet, "/api/admin/aws/issues", "")
	v.Codes["changes"], v.Codes["coverage"], v.Codes["finops"], v.Codes["issues"] = changes.status, coverage.status, finops.status, issues.status
	for _, account := range []string{wizardAccountA, wizardAccountB} {
		v.Changes[account] = bytes.Contains(changes.body, []byte(account))
		// Coverage names accounts only in its gap rows; an account read in
		// full counts in the summary of its own scope.
		scoped := s.do(http.MethodGet, "/api/admin/aws/coverage?account="+account, "")
		read, _ := pathOf(scoped.json(), "summary", "accounts_read").(float64)
		total, _ := pathOf(scoped.json(), "summary", "accounts_total").(float64)
		v.Codes["coverage_"+account] = scoped.status
		v.CoverageSummary[account] = fmt.Sprintf("%v of %v read", read, total)
		v.Coverage[account] = read > 0 || total > 0 || bytes.Contains(coverage.body, []byte(account))
		v.FinOps[account] = bytes.Contains(finops.body, []byte(account))
		v.Issues[account] = bytes.Contains(issues.body, []byte(account))
		compare := s.do(http.MethodGet, "/api/admin/aws/changes/compare?account="+account+"&from="+url.QueryEscape(from.UTC().Format(time.RFC3339)), "")
		v.Codes["compare_"+account] = compare.status
		v.Compare[account] = -1
		if compare.status == http.StatusOK {
			count, _ := pathOf(compare.json(), "snapshots", "count").(float64)
			v.Compare[account] = int(count)
			if bytes.Contains(compare.body, []byte(":"+account+":")) && count == 0 {
				v.Compare[account] = -2 // resources named with no snapshot: a leak
			}
		}
	}
	return v
}

// shows reports whether every surface shows the account.
func (v view) shows(account string) bool {
	_, listed := v.Names[account]
	return listed && v.Changes[account] && v.Coverage[account] && v.FinOps[account] && v.Issues[account] && v.Compare[account] > 0
}

// hides reports whether no surface read at request time shows the account.
// The Issues list is left out: the evaluator resolves a removed account's
// issues on its next evaluation, which the caller waits for.
func (v view) hides(account string) bool {
	_, listed := v.Names[account]
	return !listed && !v.Changes[account] && !v.Coverage[account] && !v.FinOps[account] && v.Compare[account] <= 0 && v.Compare[account] != -2
}

func (v view) okCodes(t *testing.T, what string) {
	t.Helper()
	for name, code := range v.Codes {
		if strings.HasPrefix(name, "compare_") {
			continue
		}
		if code != http.StatusOK {
			t.Errorf("%s: %s answered %d", what, name, code)
		}
	}
}

// ---- the suite ----

// TestAWSWizardAccountsAcrossEnterpriseLicence connects two accounts through
// the wizard on OSS, then runs the licensed Enterprise server on the same
// database: both accounts keep working; an edit and a removal under the
// licence leave the default organization's rows as they were; a reconnect
// starts with clean health; a lapse shows the state from before the licence
// and a new licence brings the licensed state back.
func TestAWSWizardAccountsAcrossEnterpriseLicence(t *testing.T) {
	dsn, bin, root := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("AWS_ISSUES_SERVER_BIN"), os.Getenv("AWS_REPLICAS_DIR")
	enterpriseBin, licence, org := os.Getenv("AWS_ISSUES_ENTERPRISE_BIN"), os.Getenv("AWS_ISSUES_LICENSE_KEY"), os.Getenv("AWS_ISSUES_LICENSE_ORG")
	if dsn == "" || bin == "" || root == "" || enterpriseBin == "" || licence == "" || org == "" {
		t.Skip("needs TEST_POSTGRES_DSN, AWS_ISSUES_SERVER_BIN, AWS_ISSUES_ENTERPRISE_BIN, AWS_ISSUES_LICENSE_KEY, AWS_ISSUES_LICENSE_ORG and AWS_REPLICAS_DIR")
	}
	root = filepath.Join(root, "licence-accounts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "licence_accounts_" + strconv.FormatInt(time.Now().Unix(), 10)
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	scoped := dsn + "&search_path=" + schema
	if !strings.Contains(dsn, "?") {
		scoped = dsn + "?search_path=" + schema
	}
	pool, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	db := &database{t: t, db: pool}
	out := &results{path: filepath.Join(root, "licence-accounts-verdict.json"), data: map[string]any{}}
	fake, err := fakeaws.NewServer(fakeaws.ScenarioBadDeploy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	gate, model := newEvaluationGate(), newToolModel()
	ossFront, entFront := newWizardFront(fake), newWizardFront(fake)
	oss := newReplica(t, root, "oss", nil)
	ent := newReplica(t, root, "enterprise", nil)
	t.Cleanup(func() {
		oss.stop()
		ent.stop()
		gate.server.Close()
		model.server.Close()
		ossFront.server.Close()
		entFront.server.Close()
	})
	ossProcess := wizardProcess{bin: bin, dsn: scoped, front: ossFront, gate: gate, model: model}
	entProcess := wizardProcess{bin: enterpriseBin, dsn: scoped, front: entFront, gate: gate, model: model, licence: licence}
	accounts := []string{wizardAccountA, wizardAccountB}
	defaultKey := func(account string) string { return "aws/default/accounts/" + account }
	licensedKey := func(account string) string { return "aws/" + org + "/accounts/" + account }

	var (
		connectedAt     time.Time
		month           string
		password        string
		entAPI          session
		preLicence      = map[string]storedBlob{}
		externalIDs     = map[string]string{}
		defaultRowsB    map[string]int
		defaultIssuesB  []issueRow
		reconnectExtIDB string
	)
	setT := func(t *testing.T) { oss.t, ent.t, db.t = t, t, t }

	ok := t.Run("oss_then_licence", func(t *testing.T) {
		setT(t)
		oss.startWizard(ossProcess)
		ossAPI := session{r: oss}
		connectedAt = time.Now().UTC()
		ossAPI.connectWizard(t, wizardAccountA, wizardNameA)
		ossAPI.connectWizard(t, wizardAccountB, wizardNameB)
		waitFor(t, "OSS to collect inventory, changes, cost and an open issue for both accounts", licenceGuard, func() bool {
			for _, account := range accounts {
				rows := db.accountRows("default", account)
				open := false
				for _, row := range db.accountIssues(account) {
					open = open || row.state == "open"
				}
				if rows["aws_resources"] == 0 || rows["aws_changes"] == 0 || rows["finops_cost_lines"] == 0 || !open {
					return false
				}
			}
			return true
		})
		month = db.costMonth("default", wizardAccountA)
		ossView := readView(ossAPI, month, connectedAt)
		ossView.okCodes(t, "OSS")
		for _, account := range accounts {
			if !ossView.shows(account) {
				t.Errorf("OSS does not show %s on every surface: %+v", account, ossView)
			}
			blob := db.rawBlob(defaultKey(account))
			preLicence[account] = blob
			var record struct {
				RoleARN    string `json:"role_arn"`
				ExternalID string `json:"external_id"`
			}
			if err := json.Unmarshal(blob.data, &record); err != nil || record.ExternalID == "" || record.RoleARN != roleOf(account) {
				t.Fatalf("default account record for %s: %v %.300s", account, err, blob.data)
			}
			externalIDs[account] = record.ExternalID
		}
		if ossView.Names[wizardAccountA] != wizardNameA || ossView.Names[wizardAccountB] != wizardNameB {
			t.Errorf("OSS names = %v", ossView.Names)
		}
		oss.stop()

		// From here a role signs in only with its stored external ID.
		for _, account := range accounts {
			entFront.pin(roleOf(account), externalIDs[account])
		}
		mark := entFront.mark()
		from := ent.logSize()
		ent.startWizard(entProcess)
		entAPI = signIn(t, ent, from, &password)
		gateway := ent.call(http.MethodGet, "/api/admin/aws/settings", "")
		waitFor(t, "the licensed server to sign in, collect and evaluate both accounts twice", licenceGuard, func() bool {
			for _, account := range accounts {
				if len(entFront.signIns(mark, account, http.StatusOK)) == 0 || db.accountRows(org, account)["aws_sync_units"] == 0 ||
					gate.count(ent.name, account) < 2 {
					return false
				}
			}
			return true
		})
		entView := readView(entAPI, month, connectedAt)
		entView.okCodes(t, "licensed")
		refused := entFront.since(mark, func(call awsCall) bool { return call.Operation == "AssumeRole" && call.Status != http.StatusOK })
		issues := map[string][]issueRow{}
		for _, account := range accounts {
			issues[account] = db.accountIssues(account)
			if !entView.shows(account) {
				t.Errorf("the licensed server does not show %s on every surface: %+v", account, entView)
			}
			if entView.Statuses[account] != "connected" {
				t.Errorf("licensed status of %s = %q, want connected", account, entView.Statuses[account])
			}
			for _, row := range issues[account] {
				if row.state != "open" {
					t.Errorf("issue %s of %s in %s is %s under the licence, want open", row.id, account, row.org, row.state)
				}
			}
			if n := db.noLongerConnected(account); n != 0 {
				t.Errorf("%d issue events resolved %s as no longer connected under the licence", n, account)
			}
		}
		if entView.Names[wizardAccountA] != wizardNameA || entView.Names[wizardAccountB] != wizardNameB {
			t.Errorf("licensed names = %v, want the wizard names", entView.Names)
		}
		if len(refused) != 0 {
			t.Errorf("the licensed server's sign-ins were refused (wrong role or external ID): %+v", refused)
		}
		if gateway.status != http.StatusUnauthorized && gateway.status != http.StatusForbidden {
			t.Errorf("licensed server answered the gateway secret on settings: %d", gateway.status)
		}
		signIns := map[string]any{}
		for _, account := range accounts {
			calls := entFront.signIns(mark, account, http.StatusOK)
			signIns[account] = map[string]any{"count": len(calls), "role": calls[0].RoleARN, "external_id_matches": calls[0].ExternalID == externalIDs[account]}
		}
		out.put("oss_then_licence", map[string]any{"cost_month": month, "oss_view": ossView, "licensed_view": entView,
			"licensed_sign_ins": signIns, "refused_sign_ins": refused, "gateway_status": gateway.status,
			"evaluations": map[string]int{wizardAccountA: gate.count(ent.name, wizardAccountA), wizardAccountB: gate.count(ent.name, wizardAccountB)},
			"issues":      fmt.Sprintf("%+v", issues), "licensed_rows": map[string]any{wizardAccountA: db.accountRows(org, wizardAccountA), wizardAccountB: db.accountRows(org, wizardAccountB)},
			"default_rows": map[string]any{wizardAccountA: db.accountRows("default", wizardAccountA), wizardAccountB: db.accountRows("default", wizardAccountB)}})
	})
	if !ok {
		return
	}

	ok = t.Run("edit_under_licence", func(t *testing.T) {
		setT(t)
		before := db.rawBlob(defaultKey(wizardAccountA))
		encoded, _ := json.Marshal(map[string]any{"display_name": wizardRenamedA})
		patch := entAPI.do(http.MethodPatch, "/api/admin/aws/accounts/"+wizardAccountA, string(encoded))
		after := db.rawBlob(defaultKey(wizardAccountA))
		licensed := db.rawBlob(licensedKey(wizardAccountA))
		got := readView(entAPI, month, connectedAt)
		if patch.status != http.StatusOK {
			t.Fatalf("rename under the licence = %d %.300s", patch.status, patch.body)
		}
		if got.Names[wizardAccountA] != wizardRenamedA {
			t.Errorf("licensed name of A = %q, want %q", got.Names[wizardAccountA], wizardRenamedA)
		}
		if !bytes.Equal(before.data, after.data) || !before.updated.Equal(after.updated) || !bytes.Equal(after.data, preLicence[wizardAccountA].data) {
			t.Errorf("the default record of A changed: before %s (%s), after %s (%s)", before.data, before.updated, after.data, after.updated)
		}
		if !licensed.found || !bytes.Contains(licensed.data, []byte(wizardRenamedA)) {
			t.Errorf("no licensed copy of A with the new name: %s", licensed.data)
		}
		out.put("edit_under_licence", map[string]any{"status": patch.status, "names": got.Names,
			"default_identical": bytes.Equal(before.data, after.data) && before.updated.Equal(after.updated),
			"default_record":    string(after.data), "licensed_record": string(licensed.data)})
	})
	if !ok {
		return
	}

	ok = t.Run("delete_under_licence", func(t *testing.T) {
		setT(t)
		// B fails its sign-in for a while first, so its licensed health
		// record carries failures a reconnect must not inherit.
		entFront.deny(wizardAccountB, true)
		waitFor(t, "B's licensed health record to count a failure", licenceGuard, func() bool {
			failures, _ := db.blob("aws/" + org + "/health/" + wizardAccountB)["consecutive_failures"].(float64)
			return failures >= 1
		})
		failingHealth := db.blob("aws/" + org + "/health/" + wizardAccountB)
		entFront.deny(wizardAccountB, false)

		// Control: before the removal B is planned for paid cost calls.
		estimateBefore := entAPI.do(http.MethodPost, "/api/admin/aws/finops/refresh", `{}`)
		defaultRowsB = db.accountRows("default", wizardAccountB)
		defaultBlobB := db.rawBlob(defaultKey(wizardAccountB))
		defaultHealthB := db.rawBlob("aws/default/health/" + wizardAccountB)
		defaultIssuesB = db.accountIssues(wizardAccountB)
		mark := entFront.mark()
		removal := entAPI.do(http.MethodDelete, "/api/admin/aws/accounts/"+wizardAccountB, "")
		if removal.status != http.StatusOK && removal.status != http.StatusNoContent {
			t.Fatalf("remove B under the licence = %d %.300s", removal.status, removal.body)
		}
		got := readView(entAPI, month, connectedAt)
		got.okCodes(t, "after the removal")
		issuesAtRemoval := got.Issues[wizardAccountB]
		waitFor(t, "the licensed evaluator to resolve B's issue", licenceGuard, func() bool {
			for _, row := range db.accountIssues(wizardAccountB) {
				if row.org == org && row.state == "resolved" {
					return true
				}
			}
			return false
		})
		issuesAfter := entAPI.do(http.MethodGet, "/api/admin/aws/issues", "")
		if issuesAfter.status != http.StatusOK || bytes.Contains(issuesAfter.body, []byte(wizardAccountB)) {
			t.Errorf("the Issues list still shows B once its issue is resolved: %d %.600s", issuesAfter.status, issuesAfter.body)
		}
		connection, connectionErr := entAPI.chatTool(t, model, "get_aws_connection")
		changes, changesErr := entAPI.chatTool(t, model, "get_aws_changes")

		// Paid cost planning: the manual estimate, a manual refresh and a
		// scheduled refresh after the removal.
		estimateAfter := entAPI.do(http.MethodPost, "/api/admin/aws/finops/refresh", `{}`)
		manual := entAPI.do(http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true}`)
		forced := db.clearScheduledOn(org)
		if forced {
			waitFor(t, "the licensed scheduled refresh after the removal", licenceGuard, func() bool { return db.scheduledOn(org) == today() })
		}
		billing := entFront.billing(mark)
		resourcesB := entFront.since(mark, func(call awsCall) bool {
			return call.Account == wizardAccountB && call.Operation == "GetCostAndUsageWithResources"
		})

		if !got.hides(wizardAccountB) {
			t.Errorf("B still shows after its removal: %+v", got)
		}
		if !got.shows(wizardAccountA) {
			t.Errorf("A no longer shows after B's removal: %+v", got)
		}
		for _, tool := range []struct {
			name, result string
			err          error
		}{{"get_aws_connection", connection, connectionErr}, {"get_aws_changes", changes, changesErr}} {
			switch {
			case tool.err != nil:
				t.Errorf("agent tool %s: %v", tool.name, tool.err)
			case strings.Contains(tool.result, wizardAccountB):
				t.Errorf("agent tool %s still names B after its removal: %.600s", tool.name, tool.result)
			case !strings.Contains(tool.result, wizardAccountA):
				t.Errorf("agent tool %s does not name A: %.600s", tool.name, tool.result)
			}
		}
		removed := db.rawBlob(licensedKey(wizardAccountB))
		if string(removed.data) != `{"removed":true}` {
			t.Errorf("licensed record of B after removal = %q, want the removal record", removed.data)
		}
		if after := db.rawBlob(defaultKey(wizardAccountB)); !bytes.Equal(after.data, defaultBlobB.data) || !after.updated.Equal(defaultBlobB.updated) {
			t.Errorf("default record of B changed by the removal: %s", after.data)
		}
		if after := db.rawBlob("aws/default/health/" + wizardAccountB); !bytes.Equal(after.data, defaultHealthB.data) {
			t.Errorf("default health of B changed by the removal")
		}
		if after := db.accountRows("default", wizardAccountB); fmt.Sprint(after) != fmt.Sprint(defaultRowsB) {
			t.Errorf("default rows of B changed by the removal: before %v, after %v", defaultRowsB, after)
		}
		if after := db.accountIssues(wizardAccountB); len(after) < len(defaultIssuesB) {
			t.Errorf("default issue rows of B removed: before %+v, after %+v", defaultIssuesB, after)
		}
		for _, row := range db.accountIssues(wizardAccountB) {
			if row.org == "default" && row.state != "open" {
				t.Errorf("default issue %s of B is %s after the removal, want untouched (open)", row.id, row.state)
			}
		}
		if !strings.Contains(string(estimateBefore.body), wizardAccountB) {
			t.Errorf("control: the estimate before the removal does not plan B: %.400s", estimateBefore.body)
		}
		if strings.Contains(string(estimateAfter.body), wizardAccountB) {
			t.Errorf("the estimate after the removal still plans B: %.600s", estimateAfter.body)
		}
		if len(billing[wizardAccountB]) != 0 || len(resourcesB) != 0 {
			t.Errorf("billing calls for B after its removal: %v", billing[wizardAccountB])
		}
		if len(billing[wizardAccountA]) == 0 {
			t.Errorf("control: no billing call for A after B's removal, so the refreshes proved nothing: manual %d %.300s, scheduled forced=%t",
				manual.status, manual.body, forced)
		}
		out.put("delete_under_licence", map[string]any{"status": removal.status, "body": string(removal.body), "view": got,
			"failing_health_before": failingHealth, "licensed_record": string(removed.data), "default_rows": defaultRowsB,
			"default_issues": fmt.Sprintf("%+v", db.accountIssues(wizardAccountB)), "estimate_before": fmt.Sprintf("%.800s", estimateBefore.body),
			"estimate_after": fmt.Sprintf("%.800s", estimateAfter.body), "manual_refresh": map[string]any{"status": manual.status, "body": fmt.Sprintf("%.400s", manual.body)},
			"scheduled_forced": forced, "issues_list_had_b_at_removal": issuesAtRemoval,
			"issues_list_has_b_after_resolve": bytes.Contains(issuesAfter.body, []byte(wizardAccountB)), "billing_after_removal": billing, "resource_calls_b": len(resourcesB),
			"tool_connection": fmt.Sprintf("%.800s", connection), "tool_changes": fmt.Sprintf("%.800s", changes)})
	})
	if !ok {
		return
	}

	ok = t.Run("reconnect_b", func(t *testing.T) {
		setT(t)
		// A new draft has a new external ID: B's sign-in pin follows it.
		entFront.pin(roleOf(wizardAccountB), "")
		mark := entFront.mark()
		reconnectedAt := time.Now().UTC()
		entAPI.connectWizard(t, wizardAccountB, wizardNameB)
		health := db.blob("aws/" + org + "/health/" + wizardAccountB)
		settings := entAPI.do(http.MethodGet, "/api/admin/aws/settings", "")
		var entry map[string]any
		if list, ok := settings.json()["accounts"].([]any); ok {
			for _, item := range list {
				if m, _ := item.(map[string]any); m["account_id"] == wizardAccountB {
					entry = m
				}
			}
		}
		var record struct {
			ExternalID string `json:"external_id"`
		}
		_ = json.Unmarshal(db.rawBlob(licensedKey(wizardAccountB)).data, &record)
		reconnectExtIDB = record.ExternalID
		entFront.pin(roleOf(wizardAccountB), reconnectExtIDB)
		waitFor(t, "the licensed server to collect B again", licenceGuard, func() bool {
			return db.accountRows(org, wizardAccountB)["aws_sync_units"] > 0 && len(entFront.signIns(mark, wizardAccountB, http.StatusOK)) > 0
		})
		waitFor(t, "the licensed evaluator to reopen B's issue", licenceGuard, func() bool {
			for _, row := range db.accountIssues(wizardAccountB) {
				if row.org == org && row.state != "resolved" {
					return true
				}
			}
			return false
		})
		got := readView(entAPI, month, connectedAt)
		connection, connectionErr := entAPI.chatTool(t, model, "get_aws_connection")
		changes, changesErr := entAPI.chatTool(t, model, "get_aws_changes")

		failures, _ := health["consecutive_failures"].(float64)
		checked, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(health["checked_at"]))
		if entry == nil {
			t.Fatalf("B is not listed after the reconnect: %.400s", settings.body)
		}
		if failures != 0 || health["failure_since"] != nil || health["disconnected_since"] != nil || entry["error_code"] != nil ||
			entry["disconnected_since"] != nil || checked.Before(reconnectedAt.Add(-time.Second)) {
			t.Errorf("B's health after the reconnect is not clean: record %v, view status %v error %v disconnected %v",
				health, entry["status"], entry["error_code"], entry["disconnected_since"])
		}
		if !got.shows(wizardAccountB) || !got.shows(wizardAccountA) {
			t.Errorf("after the reconnect not every surface shows both accounts: %+v", got)
		}
		if connectionErr != nil || changesErr != nil || !strings.Contains(connection, wizardAccountB) || !strings.Contains(changes, wizardAccountB) {
			t.Errorf("agent tools after the reconnect: connection %v %.300s; changes %v %.300s", connectionErr, connection, changesErr, changes)
		}
		out.put("reconnect_b", map[string]any{"health_record": health, "settings_entry": entry, "view": got,
			"new_external_id": reconnectExtIDB != externalIDs[wizardAccountB], "sign_ins": len(entFront.signIns(mark, wizardAccountB, http.StatusOK))})
	})
	if !ok {
		return
	}

	ok = t.Run("lapse_shows_the_state_before_the_licence", func(t *testing.T) {
		setT(t)
		ent.stop()
		for _, account := range accounts {
			ossFront.pin(roleOf(account), externalIDs[account])
		}
		mark := ossFront.mark()
		oss.startWizard(ossProcess)
		ossAPI := session{r: oss}
		waitFor(t, "OSS to sign in to both accounts with their default records", licenceGuard, func() bool {
			return len(ossFront.signIns(mark, wizardAccountA, http.StatusOK)) > 0 && len(ossFront.signIns(mark, wizardAccountB, http.StatusOK)) > 0
		})
		got := readView(ossAPI, month, connectedAt)
		got.okCodes(t, "after the lapse")
		refused := ossFront.since(mark, func(call awsCall) bool { return call.Operation == "AssumeRole" && call.Status != http.StatusOK })
		if got.Names[wizardAccountA] != wizardNameA || got.Names[wizardAccountB] != wizardNameB {
			t.Errorf("after the lapse the names are %v, want A %q and B %q", got.Names, wizardNameA, wizardNameB)
		}
		for _, account := range accounts {
			if !got.shows(account) {
				t.Errorf("after the lapse %s does not show on every surface: %+v", account, got)
			}
			if now := db.rawBlob(defaultKey(account)); !bytes.Equal(now.data, preLicence[account].data) {
				t.Errorf("default record of %s differs from before the licence: %s", account, now.data)
			}
		}
		if len(refused) != 0 {
			t.Errorf("OSS sign-ins refused after the lapse: %+v", refused)
		}
		out.put("lapse", map[string]any{"view": got, "issues_b": fmt.Sprintf("%+v", db.accountIssues(wizardAccountB)), "refused": refused})
		oss.stop()
	})
	if !ok {
		return
	}

	t.Run("licence_again_brings_the_licensed_state_back", func(t *testing.T) {
		setT(t)
		oss.stop()
		entFront.pin(roleOf(wizardAccountA), externalIDs[wizardAccountA])
		entFront.pin(roleOf(wizardAccountB), reconnectExtIDB)
		mark := entFront.mark()
		ent.startWizard(entProcess)
		entAPI = signIn(t, ent, ent.logSize(), &password)
		waitFor(t, "the licensed server to sign in to both accounts again", licenceGuard, func() bool {
			return len(entFront.signIns(mark, wizardAccountA, http.StatusOK)) > 0 && len(entFront.signIns(mark, wizardAccountB, http.StatusOK)) > 0
		})
		got := readView(entAPI, month, connectedAt)
		got.okCodes(t, "licensed again")
		refused := entFront.since(mark, func(call awsCall) bool { return call.Operation == "AssumeRole" && call.Status != http.StatusOK })
		if got.Names[wizardAccountA] != wizardRenamedA || got.Names[wizardAccountB] != wizardNameB {
			t.Errorf("licensed again, the names are %v, want A %q and B %q", got.Names, wizardRenamedA, wizardNameB)
		}
		for _, account := range accounts {
			if !got.shows(account) {
				t.Errorf("licensed again, %s does not show on every surface: %+v", account, got)
			}
		}
		if len(refused) != 0 {
			t.Errorf("licensed sign-ins refused after the new licence: %+v", refused)
		}
		orgs := []string{}
		for _, row := range db.accountIssues(wizardAccountA) {
			orgs = append(orgs, row.org+":"+row.state)
		}
		sort.Strings(orgs)
		out.put("licence_again", map[string]any{"view": got, "refused": refused, "issues_a": orgs})
	})
}
