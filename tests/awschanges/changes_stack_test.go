// Package awschanges runs the real versus-incident server binary against the
// tests/fakeaws change scenarios and checks the Changes API over HTTP, the
// way an operator's browser or the agent reads it.
//
// The binary is a harness_awsendpoint build, so every AWS call goes to a
// fakeaws fixture served from this test process through a front that picks
// the scenario, counts each replica's LookupEvents requests and can slow
// them. Nothing reaches AWS. The chat checks point the server at a scripted
// OpenAI-compatible model in this process, which calls the named tool and
// keeps the tool result the server sent it, so the redaction of what the
// model sees is checked on the real agent path.
//
// The tests skip unless these are set:
//
//	AWS_CHANGES_SERVER_BIN  go build -tags harness_awsendpoint -o <bin> ./cmd
//	AWS_CHANGES_DIR         a writable folder for configs, data and server logs
//	AWS_CHANGES_STORAGE     file or postgres (postgres needs TEST_POSTGRES_DSN,
//	                        an isolated database, as harness.sh pgtest gives)
package awschanges

import (
	"bytes"
	"database/sql"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/tests/fakeaws"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	gatewaySecret = "aws-changes-gateway-secret-0001"
	prodAccount   = "111122223333"
	stageAccount  = fakeaws.ChangesDeniedAccount
	modelName     = "changes-model"
)

var (
	credentialScope = regexp.MustCompile(`Credential=[^/]+/\d{8}/([^/]+)/[^/]+/aws4_request`)
	twelveDigits    = regexp.MustCompile(`\d{12}`)
)

// ---- fake AWS ----

// world is the fixture every replica shares: one fakeaws server and the
// scenario every request is answered from.
type world struct {
	fake *fakeaws.Server

	mu       sync.Mutex
	scenario string
}

func (w *world) set(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.scenario = name
}

func (w *world) current() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.scenario
}

// front is one replica's view of the fixture.
type front struct {
	world  *world
	server *httptest.Server
	delay  atomic.Int64 // nanoseconds added to each LookupEvents answer

	mu      sync.Mutex
	lookups map[string]int64 // account|signing region
	times   []time.Time      // when each LookupEvents arrived
}

func newFront(w *world) *front {
	f := &front{world: w, lookups: map[string]int64{}}
	f.server = httptest.NewServer(f)
	return f
}

func (f *front) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	request.Header.Set(fakeaws.ScenarioHeader, f.world.current())
	if strings.HasSuffix(request.Header.Get("X-Amz-Target"), ".LookupEvents") {
		region := ""
		if match := credentialScope.FindStringSubmatch(request.Header.Get("Authorization")); match != nil {
			region = match[1]
		}
		account := twelveDigits.FindString(request.Header.Get("X-Amz-Security-Token"))
		f.mu.Lock()
		f.lookups[account+"|"+region]++
		f.times = append(f.times, time.Now())
		f.mu.Unlock()
		if delay := time.Duration(f.delay.Load()); delay > 0 {
			time.Sleep(delay)
		}
	}
	f.world.fake.ServeHTTP(writer, request)
}

func (f *front) lookupTotal() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for _, n := range f.lookups {
		total += n
	}
	return total
}

func (f *front) lookupTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.times...)
}

// ---- scripted model ----

// model answers OpenAI chat completions. When the question names a tool it
// calls that tool once, keeps the tool result the server sends back, and
// answers with a fixed sentence.
type model struct {
	server *httptest.Server

	mu      sync.Mutex
	offered map[string]bool
	results map[string][]string // tool name -> tool results received
}

func newModel(t *testing.T) *model {
	m := &model{offered: map[string]bool{}, results: map[string][]string{}}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	return m
}

func (m *model) serve(writer http.ResponseWriter, request *http.Request) {
	if !strings.HasSuffix(request.URL.Path, "/chat/completions") {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"object":"list","data":[{"id":"` + modelName + `","object":"model"}]}`))
		return
	}
	body, _ := io.ReadAll(request.Body)
	var input struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &input)
	question, last := "", -1
	for index, message := range input.Messages {
		if message.Role == "user" {
			question, last = contentText(message.Content), index
		}
	}
	m.mu.Lock()
	for _, tool := range input.Tools {
		m.offered[tool.Function.Name] = true
	}
	m.mu.Unlock()
	tool := ""
	for _, name := range []string{"get_aws_changes", "recent_changes"} {
		if strings.Contains(question, name) {
			tool = name
		}
	}
	var results []string
	if last >= 0 {
		for _, message := range input.Messages[last+1:] {
			if message.Role == "tool" {
				results = append(results, contentText(message.Content))
			}
		}
	}
	if tool != "" && len(results) == 0 {
		args := `{"window":"24h","limit":50}`
		if tool == "recent_changes" {
			args = `{"time_range_minutes":1440}`
		}
		m.write(writer, input.Stream, "", tool, args)
		return
	}
	if tool != "" {
		m.mu.Lock()
		m.results[tool] = append(m.results[tool], results...)
		m.mu.Unlock()
	}
	m.write(writer, input.Stream, fmt.Sprintf("Read %d tool results.", len(results)), "", "")
}

func contentText(content any) string {
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

func (m *model) write(writer http.ResponseWriter, stream bool, content, tool, args string) {
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
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "chatcmpl-changes", "object": "chat.completion", "created": 1, "model": modelName,
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]any, reason any) {
		encoded, _ := json.Marshal(map[string]any{"id": "chatcmpl-changes", "object": "chat.completion.chunk", "created": 1, "model": modelName,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
		fmt.Fprintf(writer, "data: %s\n\n", encoded)
	}
	chunk(message, nil)
	chunk(map[string]any{}, finish)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

func (m *model) toolResults(name string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.results[name]...)
}

func (m *model) wasOffered(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.offered[name]
}

// ---- server replicas ----

type options struct {
	changesInterval   string
	inventoryInterval string
	model             *model
	// accounts is the connectors.aws.accounts YAML list (config
	// accounts, indented for the accounts key); empty means none.
	accounts string
}

type replica struct {
	t     *testing.T
	name  string
	dir   string
	port  int
	front *front
	cmd   *exec.Cmd
	log   *os.File
	done  chan struct{}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func newReplica(t *testing.T, root, name string, f *front) *replica {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The server reads config/*.yaml and the templates relative to its
	// working directory; start from the repository's shipped config folder.
	source, err := filepath.Abs(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config", entry.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &replica{t: t, name: name, dir: dir, port: freePort(t), front: f}
}

func (r *replica) url() string { return "http://127.0.0.1:" + strconv.Itoa(r.port) }

func (r *replica) start(bin, dsn string, o options) {
	t := r.t
	t.Helper()
	storageBlock := "  type: file\n"
	if dsn != "" {
		storageBlock = "  type: postgres\n  postgres:\n    dsn: ${POSTGRES_DSN}\n"
	}
	agentBlock := "agent:\n  enable: false\n"
	if o.model != nil {
		agentBlock = "agent:\n  enable: true\n  ai:\n    enable: true\n    provider: openai\n    base_url: ${CHANGES_MODEL_URL}\n    model: " + modelName +
			"\n    api_key: ${CHANGES_MODEL_KEY}\n    max_tokens: 512\n"
	}
	config := fmt.Sprintf("name: aws-changes-%s\nhost: 127.0.0.1\nport: %d\ngateway_secret: ${GATEWAY_SECRET}\nqueue:\n  enable: false\nstorage:\n%s%s",
		r.name, r.port, storageBlock, agentBlock)
	if err := os.WriteFile(filepath.Join(r.dir, "config", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	changes := o.changesInterval
	if changes == "" {
		changes = "5m"
	}
	inventory := o.inventoryInterval
	if inventory == "" {
		inventory = "15m"
	}
	accounts := " []\n"
	if o.accounts != "" {
		accounts = "\n" + o.accounts
	}
	connectors := fmt.Sprintf(`connectors:
  aws:
    enable: true
    accounts:%s    tags:
      value_allowlist: [team]
    budgets:
      max_requests_per_second: 100
    optional_data:
      cost_explorer: false
    collect:
      enable: true
      intervals:
        inventory: %s
        health: 2m
        changes: %s
`, accounts, inventory, changes)
	if err := os.WriteFile(filepath.Join(r.dir, "config", "connectors.yaml"), []byte(connectors), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(r.dir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(logFile, "\n==== %s start %s storage=%s\n", r.name, time.Now().UTC().Format(time.RFC3339), map[bool]string{true: "postgres", false: "file"}[dsn != ""])
	cmd := exec.Command(bin)
	cmd.Dir = r.dir
	// Nothing from the caller's shell, so no real AWS credential or profile
	// can be picked up; the harness_awsendpoint build sends every AWS
	// request to the front.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.dir,
		"TZ=UTC",
		"GATEWAY_SECRET=" + gatewaySecret,
		"POSTGRES_DSN=" + dsn,
		"VERSUS_HARNESS_AWS_ENDPOINT_URL=" + r.front.server.URL,
		"AWS_ACCESS_KEY_ID=ASIAVERSUSBASE00001",
		"AWS_SECRET_ACCESS_KEY=versus-changes-base-secret",
		"AWS_REGION=us-east-1",
		"VERSUS_AWS_EXTERNAL_ID=vrs-ext-awschanges01",
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_CONFIG_FILE=" + filepath.Join(r.dir, "no-aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(r.dir, "no-aws-credentials"),
	}
	if o.model != nil {
		cmd.Env = append(cmd.Env, "CHANGES_MODEL_URL="+o.model.server.URL+"/v1", "CHANGES_MODEL_KEY=changes-model-placeholder")
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("%s: start: %v", r.name, err)
	}
	r.cmd, r.log, r.done = cmd, logFile, make(chan struct{})
	go func(done chan struct{}) { _ = cmd.Wait(); close(done) }(r.done)
	deadline := time.Now().Add(90 * time.Second)
	for {
		response, err := http.Get(r.url() + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-r.done:
			t.Fatalf("%s exited during boot; see %s", r.name, filepath.Join(r.dir, "server.log"))
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not become healthy; see %s", r.name, filepath.Join(r.dir, "server.log"))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (r *replica) stop() {
	if r.cmd == nil {
		return
	}
	_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
		<-r.done
	}
	_ = r.log.Close()
	r.cmd = nil
}

type answer struct {
	status int
	body   []byte
}

func (a answer) json() map[string]any {
	var out map[string]any
	_ = json.Unmarshal(a.body, &out)
	return out
}

func (r *replica) call(method, path, body string) answer {
	r.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, r.url()+path, reader)
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("X-Gateway-Secret", gatewaySecret)
	if method != http.MethodGet {
		request.Header.Set("Origin", r.url())
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 120 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		r.t.Fatalf("%s %s %s: %v", r.name, method, path, err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return answer{status: response.StatusCode, body: data}
}

// connect runs the setup wizard for one account on this replica.
func (r *replica) connect(account string, regions ...string) {
	t := r.t
	t.Helper()
	created := r.call(http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`)
	if created.status != http.StatusCreated {
		t.Fatalf("%s: draft = %d %s", r.name, created.status, created.body)
	}
	draft, _ := created.json()["draft_id"].(string)
	verify := r.call(http.MethodPost, "/api/admin/aws/connect/drafts/"+draft+"/verify",
		`{"role_arn":"arn:aws:iam::`+account+`:role/versus-reader"}`)
	if verify.status != http.StatusAccepted {
		t.Fatalf("%s: verify %s = %d %s", r.name, account, verify.status, verify.body)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		view := r.call(http.MethodGet, "/api/admin/aws/connect/drafts/"+draft, "")
		if status, _ := view.json()["status"].(string); status == "verified" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: draft for %s did not verify: %s", r.name, account, view.body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	encoded, _ := json.Marshal(regions)
	commit := r.call(http.MethodPost, "/api/admin/aws/accounts",
		`{"draft_id":"`+draft+`","display_name":"qa-`+account+`","regions":`+string(encoded)+`}`)
	if commit.status != http.StatusCreated {
		t.Fatalf("%s: commit %s = %d %s", r.name, account, commit.status, commit.body)
	}
}

// ---- answer helpers ----

func path(value any, keys ...string) any {
	for _, key := range keys {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

func list(value any, keys ...string) []any {
	out, _ := path(value, keys...).([]any)
	return out
}

func text(value any, keys ...string) string {
	out, _ := path(value, keys...).(string)
	return out
}

func number(value any, keys ...string) int64 {
	out, _ := path(value, keys...).(float64)
	return int64(out)
}

func truth(value any, keys ...string) bool {
	out, _ := path(value, keys...).(bool)
	return out
}

func (r *replica) changes(query string) map[string]any {
	r.t.Helper()
	got := r.call(http.MethodGet, "/api/admin/aws/changes"+query, "")
	if got.status != http.StatusOK {
		r.t.Fatalf("%s changes%s = %d %.400s", r.name, query, got.status, got.body)
	}
	return got.json()
}

// pairsPolled reports whether every account in scope has its first read
// done and a poll time.
func pairsPolled(view map[string]any) bool {
	return !truth(view, "sync", "first_read_pending") && text(view, "sync", "last_poll_at") != ""
}

// realGaps are the gaps that are not the arriving tail.
func realGaps(view map[string]any) []any {
	var out []any
	for _, gap := range list(view, "gaps") {
		if text(gap, "reason") != "arriving" {
			out = append(out, gap)
		}
	}
	return out
}

// invariantErrors checks the rules the page relies on in one answer: a
// complete answer has no real gap, no incomplete account and no lower-bound
// total; an answer with a gap is never complete and its total is a lower
// bound.
func invariantErrors(view map[string]any) []string {
	var out []string
	complete := truth(view, "complete")
	gaps := realGaps(view)
	if complete && len(gaps) > 0 {
		out = append(out, fmt.Sprintf("complete=true with %d gaps (first %v)", len(gaps), gaps[0]))
	}
	if complete && truth(view, "summary", "total_is_lower_bound") {
		out = append(out, "complete=true with total_is_lower_bound")
	}
	for _, account := range list(view, "accounts") {
		if complete && !truth(account, "complete") {
			out = append(out, fmt.Sprintf("complete=true but account %s incomplete (%v)", text(account, "account"), path(account, "reason")))
		}
	}
	if len(gaps) > 0 && !truth(view, "summary", "total_is_lower_bound") {
		out = append(out, fmt.Sprintf("%d gaps but total_is_lower_bound=false", len(gaps)))
	}
	if len(gaps) > 0 && number(view, "summary", "total") == 0 && complete {
		out = append(out, "\"no changes\" claimed over a gap")
	}
	return out
}

// sampler reads /changes every interval and records every invariant
// violation with its time.
type sampler struct {
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	samples int
	errors  []string
}

func (r *replica) sample(every time.Duration) *sampler {
	s := &sampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		client := &http.Client{Timeout: 30 * time.Second}
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
			request, _ := http.NewRequest(http.MethodGet, r.url()+"/api/admin/aws/changes?window=24h&limit=1", nil)
			request.Header.Set("X-Gateway-Secret", gatewaySecret)
			response, err := client.Do(request)
			now := time.Now().UTC().Format(time.RFC3339)
			if err != nil {
				s.add(now + " request failed: " + err.Error())
				continue
			}
			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				s.add(fmt.Sprintf("%s status %d %.200s", now, response.StatusCode, data))
				continue
			}
			var view map[string]any
			_ = json.Unmarshal(data, &view)
			s.mu.Lock()
			s.samples++
			s.mu.Unlock()
			for _, problem := range invariantErrors(view) {
				s.add(now + " " + problem)
			}
		}
	}()
	return s
}

func (s *sampler) add(problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, problem)
}

func (s *sampler) finish() (int, []string) {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples, append([]string(nil), s.errors...)
}

// ---- results ----

type results struct {
	mu   sync.Mutex
	path string
	data map[string]any
}

func (r *results) put(key string, value any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[key] = value
	encoded, _ := json.MarshalIndent(r.data, "", "  ")
	_ = os.WriteFile(r.path, encoded, 0o600)
}

// ---- environment ----

type env struct {
	bin, root, storage, dsn string
	out                     *results
	admin                   *sql.DB
}

func setup(t *testing.T) *env {
	t.Helper()
	bin, root, backend := os.Getenv("AWS_CHANGES_SERVER_BIN"), os.Getenv("AWS_CHANGES_DIR"), os.Getenv("AWS_CHANGES_STORAGE")
	if bin == "" || root == "" {
		t.Skip("needs AWS_CHANGES_SERVER_BIN and AWS_CHANGES_DIR")
	}
	if backend == "" {
		backend = "file"
	}
	e := &env{bin: bin, root: filepath.Join(root, backend), storage: backend}
	if err := os.MkdirAll(e.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if backend == "postgres" {
		e.dsn = os.Getenv("TEST_POSTGRES_DSN")
		if e.dsn == "" {
			t.Skip("AWS_CHANGES_STORAGE=postgres needs TEST_POSTGRES_DSN")
		}
		admin, err := sql.Open("pgx", e.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close() })
		e.admin = admin
	}
	e.out = &results{path: filepath.Join(e.root, "changes-verdict-"+t.Name()+".json"), data: map[string]any{"storage": backend}}
	return e
}

// schemaDSN gives each server group its own Postgres schema, so every group
// starts from an empty store.
func (e *env) schemaDSN(t *testing.T, name string) string {
	t.Helper()
	if e.dsn == "" {
		return ""
	}
	schema := "awschanges_" + regexp.MustCompile(`[^a-z0-9_]`).ReplaceAllString(strings.ToLower(name), "_")
	if _, err := e.admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// boot starts one server on a fresh store for scenario and connects the
// accounts, then waits until every pair has been polled once.
func (e *env) boot(t *testing.T, name, scenario string, o options, accounts map[string][]string) (*replica, *world) {
	t.Helper()
	fake, err := fakeaws.NewServer(scenario)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	w := &world{fake: fake, scenario: scenario}
	r := newReplica(t, e.root, name, newFront(w))
	t.Cleanup(func() { r.stop(); r.front.server.Close() })
	r.start(e.bin, e.schemaDSN(t, name), o)
	for account, regions := range accounts {
		r.connect(account, regions...)
	}
	return r, w
}

// waitPolled waits until the change list says every pair was read once.
func (r *replica) waitPolled(timeout time.Duration) map[string]any {
	r.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		view := r.changes("?window=24h&limit=200")
		if pairsPolled(view) {
			return view
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s: the first change read did not finish in %s: sync=%v accounts=%v", r.name, timeout, path(view, "sync"), path(view, "accounts"))
		}
		time.Sleep(time.Second)
	}
}

func gapReasons(view map[string]any) map[string]int {
	out := map[string]int{}
	for _, gap := range list(view, "gaps") {
		out[text(gap, "reason")]++
	}
	return out
}

func itemsOf(view map[string]any, kind string) []any {
	var out []any
	for _, item := range list(view, "items") {
		if kind == "" || text(item, "kind") == kind {
			out = append(out, item)
		}
	}
	return out
}

// walkPages follows next_cursor from first and returns every item id in
// order, failing on a repeated id.
func (r *replica) walkPages(t *testing.T, base string, first map[string]any) []string {
	ids, _ := r.walkCounted(t, base, first)
	return ids
}

// walkCounted is walkPages that also sums each item's count (1 for a single
// change).
func (r *replica) walkCounted(t *testing.T, base string, first map[string]any) ([]string, int64) {
	t.Helper()
	var ids []string
	var counted int64
	seen := map[string]bool{}
	page := first
	for pages := 0; ; pages++ {
		for _, item := range list(page, "items") {
			id := text(item, "id")
			if seen[id] {
				t.Errorf("item %s repeated on page %d", id, pages)
			}
			seen[id] = true
			ids = append(ids, id)
			if n := number(item, "count"); n > 0 {
				counted += n
			} else {
				counted++
			}
		}
		cursor := text(page, "next_cursor")
		if cursor == "" {
			return ids, counted
		}
		if pages > 500 {
			t.Fatal("paging did not end")
		}
		separator := "?"
		if strings.Contains(base, "?") {
			separator = "&"
		}
		got := r.call(http.MethodGet, base+separator+"cursor="+url.QueryEscape(cursor), "")
		if got.status != http.StatusOK {
			t.Fatalf("page %d = %d %.300s", pages+1, got.status, got.body)
		}
		page = got.json()
	}
}

// ---- tests ----

// TestChangesAPIOnFakeAWS boots one server per change scenario on the
// selected storage, waits for the first change read, samples the change
// list every two seconds for the whole group and checks the scenario's
// state over HTTP.
func TestChangesAPIOnFakeAWS(t *testing.T) {
	e := setup(t)
	prodOnly := map[string][]string{prodAccount: {"us-east-1"}}

	t.Run("quiet_window_is_complete_and_says_no_changes", func(t *testing.T) {
		r, _ := e.boot(t, "quiet", fakeaws.ScenarioChangesQuiet, options{}, prodOnly)
		s := r.sample(2 * time.Second)
		view := r.waitPolled(120 * time.Second)
		time.Sleep(6 * time.Second)
		view = r.changes("?window=24h")
		samples, problems := s.finish()
		e.out.put("quiet", map[string]any{"complete": view["complete"], "summary": view["summary"], "gaps": view["gaps"], "window": view["window"], "samples": samples, "violations": problems})
		if len(problems) > 0 {
			t.Errorf("invariant violations over %d samples: %v", samples, problems)
		}
		if !truth(view, "complete") || number(view, "summary", "total") != 0 {
			t.Errorf("quiet window: complete=%v total=%d gaps=%v accounts=%v", view["complete"], number(view, "summary", "total"), view["gaps"], view["accounts"])
		}
		if text(view, "window", "settled_until") == "" {
			t.Error("settled_until missing")
		}
		arriving := 0
		for _, bucket := range list(view, "buckets") {
			if text(bucket, "state") == "arriving" {
				arriving++
			}
		}
		if arriving == 0 || len(list(view, "buckets")) != 24 {
			t.Errorf("buckets=%d arriving=%d, want 24 with an arriving tail", len(list(view, "buckets")), arriving)
		}
	})

	t.Run("window_locked_and_span_bounds", func(t *testing.T) {
		r, _ := e.boot(t, "locked", fakeaws.ScenarioChangesQuiet, options{}, prodOnly)
		now := time.Now().UTC()
		checks := []struct {
			name, path string
			status     int
			locked     bool
		}{
			{"window 7d", "/api/admin/aws/changes?window=7d", 403, true},
			{"window 48h", "/api/admin/aws/changes?window=48h", 403, true},
			{"since 25h ago", "/api/admin/aws/changes?since=" + now.Add(-25*time.Hour).Format(time.RFC3339), 403, true},
			{"since 25h ago, until 23h ago", "/api/admin/aws/changes?since=" + now.Add(-25*time.Hour).Format(time.RFC3339) + "&until=" + now.Add(-23*time.Hour).Format(time.RFC3339), 403, true},
			{"until 25h ago", "/api/admin/aws/changes?until=" + now.Add(-25*time.Hour).Format(time.RFC3339), 403, true},
			{"since after until", "/api/admin/aws/changes?since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&until=" + now.Add(-2*time.Hour).Format(time.RFC3339), 400, false},
			{"since 23h ago", "/api/admin/aws/changes?since=" + now.Add(-23*time.Hour).Format(time.RFC3339), 200, false},
			{"compare from 25h ago", "/api/admin/aws/changes/compare?from=" + now.Add(-25*time.Hour).Format(time.RFC3339), 403, true},
			{"window 1h", "/api/admin/aws/changes?window=1h", 200, false},
		}
		observed := map[string]any{}
		for _, check := range checks {
			got := r.call(http.MethodGet, check.path, "")
			observed[check.name] = map[string]any{"status": got.status, "body": string(got.body[:min(len(got.body), 160)])}
			if got.status != check.status {
				t.Errorf("%s: status %d, want %d (%.200s)", check.name, got.status, check.status, got.body)
				continue
			}
			if check.locked {
				body := got.json()
				if body["error"] != "window_locked" || body["tier"] != "enterprise" || body["max_window"] != "24h" {
					t.Errorf("%s: body %s, want window_locked/enterprise/24h", check.name, got.body)
				}
			}
		}
		hour := r.changes("?window=1h")
		if n := len(list(hour, "buckets")); n != 12 {
			t.Errorf("1h window has %d buckets, want 12 five-minute buckets", n)
		}
		e.out.put("window_locked", observed)
	})

	t.Run("throttled_never_claims_complete", func(t *testing.T) {
		r, _ := e.boot(t, "throttled", fakeaws.ScenarioChangesThrottled, options{changesInterval: "30s"}, prodOnly)
		s := r.sample(2 * time.Second)
		deadline := time.Now().Add(150 * time.Second)
		var view map[string]any
		for {
			view = r.changes("?window=24h")
			if gapReasons(view)["throttled"] > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(2 * time.Second)
		}
		// Keep sampling across two more poll cycles.
		time.Sleep(65 * time.Second)
		view = r.changes("?window=24h")
		samples, problems := s.finish()
		e.out.put("throttled", map[string]any{"complete": view["complete"], "gaps": view["gaps"], "summary": view["summary"], "accounts": view["accounts"], "samples": samples, "violations": problems})
		if len(problems) > 0 {
			t.Errorf("invariant violations over %d samples: %v", samples, problems)
		}
		if truth(view, "complete") {
			t.Error("a fully throttled CloudTrail read claims complete")
		}
		if gapReasons(view)["throttled"] == 0 {
			t.Errorf("no throttled gap named: %v", view["gaps"])
		}
		if !truth(view, "summary", "total_is_lower_bound") {
			t.Error("total is not marked as a lower bound over a throttled gap")
		}
	})

	t.Run("cloudtrail_denied_in_one_account", func(t *testing.T) {
		r, _ := e.boot(t, "denied", fakeaws.ScenarioChangesCloudTrailDenied, options{changesInterval: "30s"},
			map[string][]string{prodAccount: {"us-east-1"}, stageAccount: {"eu-west-1"}})
		s := r.sample(2 * time.Second)
		deadline := time.Now().Add(150 * time.Second)
		var view map[string]any
		for {
			view = r.changes("?window=24h")
			if gapReasons(view)["cloudtrail_denied"] > 0 && text(view, "sync", "last_poll_at") != "" || time.Now().After(deadline) {
				break
			}
			time.Sleep(2 * time.Second)
		}
		var accountStates []map[string]any
		for range 3 {
			time.Sleep(30 * time.Second)
			view = r.changes("?window=24h")
			states := map[string]any{}
			for _, account := range list(view, "accounts") {
				states[text(account, "account")] = map[string]any{"complete": path(account, "complete"), "reason": path(account, "reason")}
			}
			accountStates = append(accountStates, states)
		}
		samples, problems := s.finish()
		e.out.put("cloudtrail_denied", map[string]any{"accounts_over_time": accountStates, "gaps": view["gaps"], "complete": view["complete"], "samples": samples, "violations": problems})
		if len(problems) > 0 {
			t.Errorf("invariant violations over %d samples: %v", samples, problems)
		}
		if truth(view, "complete") {
			t.Error("complete with one account's CloudTrail denied")
		}
		denied := 0
		for _, gap := range list(view, "gaps") {
			if text(gap, "reason") != "cloudtrail_denied" {
				continue
			}
			denied++
			if text(gap, "account") != stageAccount {
				t.Errorf("denied gap on account %s, want %s only", text(gap, "account"), stageAccount)
			}
			if text(gap, "missing_permission") != "cloudtrail:LookupEvents" {
				t.Errorf("denied gap names permission %v", path(gap, "missing_permission"))
			}
		}
		if denied == 0 {
			t.Errorf("no cloudtrail_denied gap: %v", view["gaps"])
		}
		// The grid keeps what the readable account read: a bucket with the
		// denied account unread is partial and names it; never gap.
		partial, gapBuckets := 0, 0
		var badPartial []any
		for _, bucket := range list(view, "buckets") {
			switch text(bucket, "state") {
			case "partial":
				partial++
				if accounts := list(bucket, "gap_accounts"); len(accounts) != 1 || accounts[0] != stageAccount {
					badPartial = append(badPartial, bucket)
				}
			case "gap":
				gapBuckets++
			}
		}
		e.out.put("cloudtrail_denied_buckets", map[string]any{"partial": partial, "gap": gapBuckets, "bad_partial": badPartial})
		if partial == 0 || gapBuckets > 0 || len(badPartial) > 0 {
			t.Errorf("buckets: %d partial, %d gap, partial without gap_accounts=[%s]: %v", partial, gapBuckets, stageAccount, badPartial)
		}
		for index, states := range accountStates {
			stage, _ := states[stageAccount].(map[string]any)
			prod, _ := states[prodAccount].(map[string]any)
			if stage == nil || stage["complete"] != false {
				t.Errorf("sample %d: denied account state %v, want incomplete every cycle", index, stage)
			}
			if prod == nil || prod["complete"] != true {
				t.Errorf("sample %d: readable account state %v, want complete every cycle", index, prod)
			}
		}
	})

	t.Run("error_burst_collapses_and_group_pages", func(t *testing.T) {
		r, _ := e.boot(t, "burst", fakeaws.ScenarioChangesErrorBurst, options{}, prodOnly)
		view := r.waitPolled(120 * time.Second)
		bursts := itemsOf(view, "burst")
		e.out.put("error_burst", map[string]any{"items": view["items"], "summary": view["summary"]})
		if len(bursts) != 1 {
			t.Fatalf("bursts = %d, want 1: %v", len(bursts), view["items"])
		}
		burst := bursts[0]
		if number(burst, "count") != fakeaws.ChangesBurstSize || text(burst, "error_code") != "AccessDenied" {
			t.Errorf("burst = %v", burst)
		}
		if got := number(view, "summary", "errors"); got != fakeaws.ChangesBurstSize {
			t.Errorf("summary errors = %d, want %d", got, fakeaws.ChangesBurstSize)
		}
		base := "/api/admin/aws/changes/groups/" + url.PathEscape(text(burst, "id")) + "?limit=5"
		first := r.call(http.MethodGet, base, "")
		if first.status != http.StatusOK {
			t.Fatalf("group = %d %.300s", first.status, first.body)
		}
		ids := r.walkPages(t, base, first.json())
		e.out.put("error_burst_group", map[string]any{"members": len(ids)})
		if len(ids) != fakeaws.ChangesBurstSize {
			t.Errorf("group pages hold %d members, want %d", len(ids), fakeaws.ChangesBurstSize)
		}
	})

	t.Run("busy_account_pages_without_loss", func(t *testing.T) {
		r, _ := e.boot(t, "busy", fakeaws.ScenarioChangesBusy, options{}, prodOnly)
		r.waitPolled(240 * time.Second)
		// An exact span, so the window does not move while the pages are
		// read and every change in it must come back exactly once.
		now := time.Now().UTC().Truncate(time.Second)
		span := "since=" + now.Add(-24*time.Hour+time.Minute).Format(time.RFC3339) + "&until=" + now.Format(time.RFC3339)
		base := "/api/admin/aws/changes?" + span + "&limit=200"
		first := r.changes("?" + span + "&limit=200")
		ids, counted := r.walkCounted(t, base, first)
		e.out.put("busy", map[string]any{"complete": first["complete"], "summary": first["summary"], "ids": len(ids), "counted": counted, "gaps": first["gaps"], "top_actors": path(first, "summary", "top_actors")})
		total := number(first, "summary", "total")
		if total < fakeaws.ChangesBusySize-100 && !truth(first, "summary", "total_is_lower_bound") {
			t.Errorf("busy total %d without a lower bound, gaps %v", total, first["gaps"])
		}
		if counted != total+number(first, "summary", "errors") {
			t.Errorf("paging returned %d changes in %d items, summary says %d changes and %d errors", counted, len(ids), total, number(first, "summary", "errors"))
		}
		if len(list(first, "summary", "top_actors")) == 0 {
			t.Error("no top actors on a busy account")
		}
	})

	t.Run("alarm_with_no_suspect", func(t *testing.T) {
		r, _ := e.boot(t, "alarm", fakeaws.ScenarioChangesAlarmNoSuspect, options{}, prodOnly)
		r.waitPolled(120 * time.Second)
		var view map[string]any
		deadline := time.Now().Add(60 * time.Second)
		for {
			view = r.changes("?window=24h")
			if len(list(view, "alarms")) > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(2 * time.Second)
		}
		e.out.put("alarm_no_suspect", map[string]any{"alarms": view["alarms"], "suspect": view["suspect"]})
		if len(list(view, "alarms")) == 0 {
			t.Fatalf("no alarm on the line: %v", view["alarms"])
		}
		status := text(view, "suspect", "status")
		if status != "none" {
			t.Errorf("suspect status %q, want none (window read and settled): %v", status, view["suspect"])
		}
		if path(view, "suspect", "change") != nil {
			t.Errorf("a suspect was named: %v", path(view, "suspect", "change"))
		}
	})

	t.Run("aws_service_only", func(t *testing.T) {
		r, _ := e.boot(t, "service", fakeaws.ScenarioChangesServiceOnly, options{}, prodOnly)
		view := r.waitPolled(120 * time.Second)
		byClass, _ := path(view, "summary", "by_actor_class").(map[string]any)
		e.out.put("aws_service_only", map[string]any{"by_actor_class": byClass, "people": path(view, "summary", "people")})
		if number(byClass, "person") != 0 || number(byClass, "pipeline") != 0 || number(byClass, "aws_service") != 2 {
			t.Errorf("by_actor_class = %v, want 2 AWS-service changes only", byClass)
		}
		// The resource filter takes a canonical ARN and returns that
		// resource's changes.
		arn, id := "", ""
		for _, item := range list(view, "items") {
			if resources := list(item, "resources"); len(resources) > 0 {
				arn, _ = resources[0].(string)
				id = text(item, "id")
				break
			}
		}
		if arn == "" {
			t.Fatalf("no item names a resource: %v", view["items"])
		}
		// Search takes the raw API name in service:Action form.
		searched := r.changes("?window=24h&q=" + url.QueryEscape("autoscaling:SetDesiredCapacity"))
		e.out.put("search_service_action", map[string]any{"total": number(searched, "summary", "total"), "items": len(list(searched, "items"))})
		if number(searched, "summary", "total") != 1 || len(list(searched, "items")) != 1 || text(list(searched, "items")[0], "event_name") != "autoscaling:SetDesiredCapacity" {
			t.Errorf("q=autoscaling:SetDesiredCapacity: total %d items %v", number(searched, "summary", "total"), list(searched, "items"))
		}
		got := r.call(http.MethodGet, "/api/admin/aws/changes?window=24h&resource="+url.QueryEscape(arn), "")
		e.out.put("resource_filter", map[string]any{"arn": arn, "status": got.status, "body": firstChars(string(got.body), 300)})
		if got.status != http.StatusOK {
			t.Fatalf("resource filter %s = %d %.300s", arn, got.status, got.body)
		}
		found := false
		for _, item := range list(got.json(), "items") {
			if text(item, "id") == id {
				found = true
			}
		}
		if !found {
			t.Errorf("resource filter %s did not return item %s: %.300s", arn, id, got.body)
		}
	})

	t.Run("unknown_actor_generic_template", func(t *testing.T) {
		r, _ := e.boot(t, "unknown", fakeaws.ScenarioChangesUnknownActor, options{}, prodOnly)
		view := r.waitPolled(120 * time.Second)
		var found map[string]any
		for _, item := range list(view, "items") {
			if text(item, "actor_class") == "unknown" {
				found, _ = item.(map[string]any)
			}
		}
		e.out.put("unknown_actor", found)
		if found == nil {
			t.Fatalf("no unknown-actor item: %v", view["items"])
		}
		if text(found, "event_name") != "widgets:"+fakeaws.ChangesUnknownEventName {
			t.Errorf("event_name = %q", text(found, "event_name"))
		}
	})

	t.Run("compare_unavailable_and_partial", func(t *testing.T) {
		r, w := e.boot(t, "compare", fakeaws.ScenarioAccessDeniedBurst, options{inventoryInterval: "30s"}, prodOnly)
		r.waitPolled(120 * time.Second)
		before := r.call(http.MethodGet, "/api/admin/aws/changes/compare?from="+time.Now().UTC().Add(-23*time.Hour).Format(time.RFC3339), "")
		if before.status != http.StatusOK {
			t.Fatalf("compare before history = %d %.300s", before.status, before.body)
		}
		early := before.json()
		if text(early, "status") != "unavailable" || text(early, "history_starts") == "" {
			t.Errorf("compare before the oldest snapshot = status %q history_starts %q", text(early, "status"), text(early, "history_starts"))
		}
		mark := time.Now().UTC().Add(time.Second)
		time.Sleep(3 * time.Second)
		// Denied services become readable: their groups get history only
		// from now on, so a compare from mark leaves them out as partial.
		w.set(fakeaws.ScenarioBadDeploy)
		deadline := time.Now().Add(150 * time.Second)
		var compare map[string]any
		for {
			got := r.call(http.MethodGet, "/api/admin/aws/changes/compare?from="+mark.Format(time.RFC3339), "")
			if got.status != http.StatusOK {
				t.Fatalf("compare = %d %.300s", got.status, got.body)
			}
			compare = got.json()
			if len(list(compare, "partial")) > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(3 * time.Second)
		}
		e.out.put("compare", map[string]any{"unavailable": map[string]any{"status": early["status"], "history_starts": early["history_starts"], "snapshots": early["snapshots"]},
			"after_switch": map[string]any{"status": compare["status"], "complete": compare["complete"], "partial": compare["partial"], "added": len(list(compare, "added")),
				"changed": len(list(compare, "changed")), "removed": len(list(compare, "removed"))}})
		if len(list(compare, "partial")) == 0 {
			t.Errorf("no partial groups after denied services became readable: %v", compare)
		}
		if truth(compare, "complete") && len(list(compare, "partial")) > 0 {
			t.Error("compare complete=true while groups are partial")
		}
	})

	t.Run("agent_tools_redacted_and_recent_changes_without_github", func(t *testing.T) {
		m := newModel(t)
		// bad-deploy holds real-shaped writes in the last 3 hours, among them
		// an SSO session named by an e-mail address and a tag value outside
		// the allowlist: neither may reach the model.
		r, _ := e.boot(t, "agent", fakeaws.ScenarioBadDeploy, options{model: m}, prodOnly)
		r.waitPolled(120 * time.Second)
		apiView := r.call(http.MethodGet, "/api/admin/aws/changes?window=24h&limit=200", "")
		tools := r.call(http.MethodGet, "/api/admin/agent/tools?agent=chat", "")
		toolStates := map[string]any{}
		var catalog []any
		_ = json.Unmarshal(tools.body, &catalog)
		if catalog == nil {
			catalog = list(tools.json(), "tools")
		}
		for _, entry := range catalog {
			name := text(entry, "name")
			if name == "get_aws_changes" || name == "recent_changes" {
				toolStates[name] = map[string]any{"state": path(entry, "state"), "reason": path(entry, "reason")}
			}
		}
		turn := func(question string) {
			created := r.call(http.MethodPost, "/api/admin/chat/sessions", "")
			if created.status != http.StatusOK && created.status != http.StatusCreated {
				t.Fatalf("chat session = %d %.300s", created.status, created.body)
			}
			id := text(created.json(), "id")
			payload, _ := json.Marshal(map[string]string{"message": question})
			got := r.call(http.MethodPost, "/api/admin/chat/sessions/"+id+"/messages", string(payload))
			if got.status != http.StatusOK {
				t.Fatalf("chat turn = %d %.300s", got.status, got.body)
			}
		}
		turn("What changed in AWS today? Use get_aws_changes.")
		turn("Which deploys happened recently? Use recent_changes.")
		awsResults := m.toolResults("get_aws_changes")
		recentResults := m.toolResults("recent_changes")
		e.out.put("agent_tools", map[string]any{"catalog_status": tools.status, "tool_states": toolStates, "offered_get_aws_changes": m.wasOffered("get_aws_changes"),
			"offered_recent_changes": m.wasOffered("recent_changes"), "get_aws_changes_results": len(awsResults), "recent_changes_results": len(recentResults),
			"get_aws_changes_bytes": len(strings.Join(awsResults, "")), "get_aws_changes_sample": firstChars(strings.Join(awsResults, ""), 1500),
			"api_items": len(list(apiView.json(), "items")), "recent_changes_sample": firstChars(strings.Join(recentResults, ""), 1500)})
		secrets := append(fakeaws.CanarySecrets(), fakeaws.WorldRedactedTagValue, fakeaws.WorldPersonalEmail)
		for _, surface := range []struct {
			name string
			body string
		}{{"api", string(apiView.body)}, {"get_aws_changes", strings.Join(awsResults, "\n")}, {"recent_changes", strings.Join(recentResults, "\n")}} {
			for _, secret := range secrets {
				if strings.Contains(surface.body, secret) {
					t.Errorf("planted value %q reached %s", secret, surface.name)
				}
			}
		}
		apiItems := len(list(apiView.json(), "items"))
		if apiItems == 0 {
			t.Fatalf("the scenario stored no changes, so the tool checks prove nothing: %.300s", apiView.body)
		}
		if len(awsResults) == 0 {
			t.Errorf("the model never received a get_aws_changes result (offered=%v, states=%v)", m.wasOffered("get_aws_changes"), toolStates)
		} else if !strings.Contains(awsResults[0], `"found":true`) || !strings.Contains(awsResults[0], `"items":[{`) {
			t.Errorf("get_aws_changes result carries no change list while the API lists %d items: %.400s", apiItems, awsResults[0])
		}
		if len(recentResults) == 0 {
			t.Errorf("the model never received a recent_changes result (offered=%v, states=%v)", m.wasOffered("recent_changes"), toolStates)
		} else if !strings.Contains(strings.ToLower(strings.Join(recentResults, "")), "aws") {
			t.Errorf("recent_changes without GitHub returned no AWS rows: %.400s", recentResults[0])
		}
	})
}

func firstChars(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n]
}

// TestChangesTwoReplicasOnePoller boots one replica, measures its CloudTrail
// requests over a few poll intervals, then boots a second replica on the
// same Postgres database and measures both together. One poller per pair
// means the pair's request count does not grow with the replica count; the
// change list stays complete and the same on both replicas throughout.
func TestChangesTwoReplicasOnePoller(t *testing.T) {
	e := setup(t)
	if e.dsn == "" {
		t.Skip("two replicas share a Postgres database; AWS_CHANGES_STORAGE=postgres not set")
	}
	dsn := e.schemaDSN(t, "replicas")
	fake, err := fakeaws.NewServer(fakeaws.ScenarioBadDeploy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	w := &world{fake: fake, scenario: fakeaws.ScenarioBadDeploy}
	a := newReplica(t, e.root, "replica-a", newFront(w))
	b := newReplica(t, e.root, "replica-b", newFront(w))
	t.Cleanup(func() { a.stop(); b.stop(); a.front.server.Close(); b.front.server.Close() })
	o := options{changesInterval: "30s"}
	a.start(e.bin, dsn, o)
	a.connect(prodAccount, "us-east-1")
	a.waitPolled(120 * time.Second)

	const window = 125 * time.Second
	sa := a.sample(2 * time.Second)
	startA := a.front.lookupTotal()
	time.Sleep(window)
	single := a.front.lookupTotal() - startA

	b.start(e.bin, dsn, o)
	sb := b.sample(2 * time.Second)
	time.Sleep(10 * time.Second) // let the second replica's first pass settle
	fromA, fromB := a.front.lookupTotal(), b.front.lookupTotal()
	time.Sleep(window)
	pairA, pairB := a.front.lookupTotal()-fromA, b.front.lookupTotal()-fromB
	samplesA, problemsA := sa.finish()
	samplesB, problemsB := sb.finish()

	viewA, viewB := a.changes("?window=24h&limit=200"), b.changes("?window=24h&limit=200")
	settingsA := a.call(http.MethodGet, "/api/admin/aws/settings", "")
	e.out.put("two_replicas", map[string]any{"window_seconds": window.Seconds(), "single_replica_lookups": single,
		"two_replicas_lookups": map[string]any{"a": pairA, "b": pairB, "total": pairA + pairB},
		"lookup_times_a":       len(a.front.lookupTimes()), "lookup_times_b": len(b.front.lookupTimes()),
		"samples": map[string]any{"a": samplesA, "b": samplesB}, "violations": map[string]any{"a": problemsA, "b": problemsB},
		"complete": map[string]any{"a": viewA["complete"], "b": viewB["complete"]}, "totals": map[string]any{"a": path(viewA, "summary", "total"), "b": path(viewB, "summary", "total")},
		"settings_status": settingsA.status})
	if len(problemsA)+len(problemsB) > 0 {
		t.Errorf("invariant violations: a=%v b=%v", problemsA, problemsB)
	}
	if number(viewA, "summary", "total") != number(viewB, "summary", "total") || truth(viewA, "complete") != truth(viewB, "complete") {
		t.Errorf("replicas disagree: a total=%d complete=%v, b total=%d complete=%v", number(viewA, "summary", "total"), viewA["complete"],
			number(viewB, "summary", "total"), viewB["complete"])
	}
	if single == 0 {
		t.Fatal("the single replica sent no LookupEvents in the window")
	}
	if both := pairA + pairB; float64(both) > 1.25*float64(single) {
		t.Errorf("two replicas sent %d LookupEvents (a=%d b=%d) in %s, one replica sent %d: the pair is polled by both replicas, not one",
			both, pairA, pairB, window, single)
	}
	body := bytes.ToLower(settingsA.body)
	if bytes.Contains(body, []byte("throttl")) {
		t.Errorf("settings show throttling with two replicas: %.400s", settingsA.body)
	}
}
