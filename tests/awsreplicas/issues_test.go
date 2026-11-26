package awsreplicas

// The issue tests run the AWS issue evaluator, its alerts and the issue API
// in real server processes on one Postgres database. Their server binaries
// carry the test rules of tests/awsreplicas/issuerule, linked in by a build
// overlay, so the fake estate (the bad-deploy collector world, where the
// CloudWatch alarm checkout-5xx is in ALARM) raises a critical issue. Every
// alert goes to a Lark webhook served from this test process.
//
// They skip unless these are set, besides TEST_POSTGRES_DSN and
// AWS_REPLICAS_DIR:
//
//	AWS_ISSUES_SERVER_BIN      the OSS server, harness_awsendpoint build with
//	                           the issuerule import added by an overlay
//	AWS_ISSUES_ENTERPRISE_BIN  the Enterprise server built the same way and
//	                           with a test licence verifier (licence test only)
//	AWS_ISSUES_LICENSE_KEY     a test licence the Enterprise build accepts

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/tests/awsreplicas/issuerule"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

const (
	// issuesAccount is the fake estate's production account.
	issuesAccount = "111122223333"
	// evaluationGuard bounds one wait for an evaluation event. A replica
	// evaluates after its own collector writes, at most once a minute, so a
	// few minutes is a hang guard, not a pass condition.
	evaluationGuard = 4 * time.Minute
	// holdGuard releases a held evaluation the test forgot.
	holdGuard      = 5 * time.Minute
	unknownIssueID = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

// ---- the rule gate ----

// issueGate answers the test rules' calls: it counts each replica's
// evaluations and can hold one inside the evaluation.
type issueGate struct {
	server *httptest.Server

	mu       sync.Mutex
	calls    map[string]int
	findings map[string]int
	answer   issuerule.Answer
	holdNext map[string]bool // replica -> hold its next call
	holdAny  bool            // hold the next call that has a finding
	held     chan string
	release  chan struct{}
	changed  chan struct{}
}

func newIssueGate() *issueGate {
	g := &issueGate{calls: map[string]int{}, findings: map[string]int{}, holdNext: map[string]bool{},
		held: make(chan string, 4), changed: make(chan struct{})}
	g.server = httptest.NewServer(g)
	return g
}

func (g *issueGate) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var call issuerule.Call
	if err := json.NewDecoder(io.LimitReader(request.Body, 1<<16)).Decode(&call); err != nil {
		http.Error(writer, "bad call", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.calls[call.Replica]++
	g.findings[call.Replica] = call.Findings
	var wait chan struct{}
	if g.holdNext[call.Replica] || (g.holdAny && call.Findings > 0) {
		delete(g.holdNext, call.Replica)
		g.holdAny = false
		wait = make(chan struct{})
		g.release = wait
	}
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
	if wait != nil {
		g.held <- call.Replica
		select {
		case <-wait:
		case <-request.Context().Done():
		case <-time.After(holdGuard):
		}
	}
	g.mu.Lock()
	answer := g.answer
	g.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(answer)
}

func (g *issueGate) count(replica string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[replica]
}

func (g *issueGate) setAnswer(answer issuerule.Answer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer = answer
}

func (g *issueGate) hold(replica string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holdNext[replica] = true
}

func (g *issueGate) holdFirstFinding() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holdAny = true
}

// releaseHeld lets the held evaluation go on.
func (g *issueGate) releaseHeld() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release != nil {
		close(g.release)
		g.release = nil
	}
}

// waitHeld waits for a held call and names its replica.
func (g *issueGate) waitHeld(t *testing.T, what string) string {
	t.Helper()
	select {
	case replica := <-g.held:
		return replica
	case <-time.After(evaluationGuard):
		t.Fatalf("no evaluation was held within %s: %s", evaluationGuard, what)
		return ""
	}
}

// waitCalls waits until every named replica has reached its count of
// evaluations, and calls each(replica) after every evaluation event.
func (g *issueGate) waitCalls(t *testing.T, what string, want map[string]int, each func()) {
	t.Helper()
	deadline := time.After(evaluationGuard)
	for {
		g.mu.Lock()
		done := true
		for replica, n := range want {
			if g.calls[replica] < n {
				done = false
			}
		}
		changed := g.changed
		got := fmt.Sprint(g.calls)
		g.mu.Unlock()
		if done {
			return
		}
		select {
		case <-changed:
			if each != nil {
				each()
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %s: evaluations %s, want %v", evaluationGuard, what, got, want)
		}
	}
}

// ---- the Lark webhook ----

type larkReceiver struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newLarkReceiver() *larkReceiver {
	l := &larkReceiver{}
	l.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		l.mu.Lock()
		l.bodies = append(l.bodies, string(body))
		l.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"code":0,"msg":"success"}`)
	}))
	return l
}

func (l *larkReceiver) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.bodies)
}

func (l *larkReceiver) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.bodies...)
}

// ---- issue replicas ----

// issueProcess is how one replica process is started.
type issueProcess struct {
	bin, dsn string
	gate     *issueGate
	lark     *larkReceiver
	// licence, when set, starts a licensed Enterprise binary.
	licence    string
	enterprise bool
}

func issueConfigs(name string, port int, lark string) (string, string) {
	config := fmt.Sprintf(`name: aws-issues-%s
host: 127.0.0.1
port: %d
gateway_secret: ${GATEWAY_SECRET}
alert:
  lark:
    enable: true
    webhook_url: %s
    template_path: "config/lark_message.tmpl"
queue:
  enable: false
storage:
  type: postgres
  postgres:
    dsn: ${POSTGRES_DSN}
agent:
  enable: false
`, name, port, lark)
	connectors := `connectors:
  aws:
    enable: true
    accounts:
      - account_id: "` + issuesAccount + `"
        display_name: Production
        regions: [us-east-1]
        auth:
          mode: assume_role
          role_arn: arn:aws:iam::` + issuesAccount + `:role/versus-reader
          external_id: ${AWS_ISSUES_EXTERNAL_ID}
    tags:
      value_allowlist: [team]
    budgets:
      max_requests_per_second: 100
    optional_data:
      cost_explorer: false
    collect:
      enable: true
      intervals:
        inventory: 15m
        health: 30s
        changes: 15m
`
	return config, connectors
}

// startIssues starts the replica's server with the issue configuration and
// waits for it to answer /healthz.
func (r *replica) startIssues(p issueProcess) {
	t := r.t
	t.Helper()
	config, connectors := issueConfigs(r.name, r.port, p.lark.server.URL)
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
	fmt.Fprintf(logFile, "\n==== %s start %s enterprise=%t licensed=%t\n", r.name, time.Now().UTC().Format(time.RFC3339), p.enterprise, p.licence != "")
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
		"VERSUS_HARNESS_AWS_ENDPOINT_URL=" + r.front.server.URL,
		"AWS_ACCESS_KEY_ID=ASIAVERSUSBASE00001",
		"AWS_SECRET_ACCESS_KEY=versus-replica-base-secret",
		"AWS_REGION=us-east-1",
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_CONFIG_FILE=" + filepath.Join(r.dir, "no-aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(r.dir, "no-aws-credentials"),
		issuerule.GateEnv + "=" + p.gate.server.URL,
		issuerule.ReplicaEnv + "=" + r.name,
		"LICENSE_KEY=" + p.licence,
		"LICENSE_CACHE_PATH=",
		"AWS_ISSUES_EXTERNAL_ID=vrs-ext-issuesreplicas01",
		"VERSUS_ENTERPRISE_COOKIE_INSECURE=1",
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

// kill ends the replica at once, the way a lost node ends: no shutdown.
func (r *replica) kill() {
	if r.cmd == nil {
		return
	}
	_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
	<-r.done
	_ = os.Remove(r.processRecord())
	_ = r.log.Close()
	r.cmd = nil
}

// callWith sends one request with extra headers.
func (r *replica) callWith(method, path, body string, headers map[string]string) answer {
	r.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, r.url()+path, reader)
	if err != nil {
		r.t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if method != http.MethodGet {
		request.Header.Set("Origin", r.url())
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 90 * time.Second}
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		r.t.Fatalf("%s %s %s: %v", r.name, method, path, err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return answer{status: response.StatusCode, header: response.Header, body: data, took: time.Since(started)}
}

// ---- issue rows ----

type issueRow struct {
	org, id, rule, severity, state string
	version                        int64
	payload                        string
}

func (d *database) issueRows(rule string) []issueRow {
	d.t.Helper()
	rows, err := d.db.Query(`SELECT org_id, id, rule, severity, state, version, coalesce(payload::text, '') FROM aws_issues WHERE rule = $1 ORDER BY org_id, id`, rule)
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

// events counts one issue's events of a kind, in every organization.
func (d *database) events(issue, kind string) int {
	return d.count(`SELECT count(*) FROM aws_issue_events WHERE issue_id = $1 AND kind = $2`, issue, kind)
}

func (d *database) eventLog(issue string) []string {
	d.t.Helper()
	rows, err := d.db.Query(`SELECT org_id, seq, kind, actor, detail FROM aws_issue_events WHERE issue_id = $1 ORDER BY at, seq`, issue)
	if err != nil {
		d.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var org, kind, actor, detail string
		var seq int64
		if err := rows.Scan(&org, &seq, &kind, &actor, &detail); err != nil {
			d.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s#%d %s by %s: %s", org, seq, kind, actor, detail))
	}
	if err := rows.Err(); err != nil {
		d.t.Fatal(err)
	}
	return out
}

// evaluationLockHolders is how many sessions hold the issue evaluator's
// replica lock for org (pkg/aws/issues evaluator lockKey).
func (d *database) evaluationLockHolders(org string) int {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("aws-issues\x00" + org))
	key := hash.Sum64() & 0x7fffffffffffffff
	return d.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
		AND classid = $1::bigint::oid AND objid = $2::bigint::oid AND objsubid = 1`, int64(key>>32), int64(key&0xffffffff))
}

// one returns the single row of rows or fails.
func one(t *testing.T, what string, rows []issueRow) issueRow {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%s: %d aws_issues rows, want exactly one: %+v", what, len(rows), rows)
	}
	return rows[0]
}

// issuesSetup is the shared fixture of the issue tests.
type issuesSetup struct {
	t    *testing.T
	db   *database
	gate *issueGate
	lark *larkReceiver
	w    *world
	out  *results
}

func newIssuesSetup(t *testing.T, root, dsn, name string) *issuesSetup {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	fake, err := fakeaws.NewServer(fakeaws.ScenarioBadDeploy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	s := &issuesSetup{t: t, db: &database{t: t, db: pool}, gate: newIssueGate(), lark: newLarkReceiver(),
		w: &world{fake: fake, scenario: fakeaws.ScenarioBadDeploy}, out: &results{path: filepath.Join(root, name), data: map[string]any{}}}
	t.Cleanup(func() {
		s.gate.releaseHeld()
		s.gate.server.Close()
		s.lark.server.Close()
	})
	return s
}

func pathOf(value any, keys ...string) any {
	for _, key := range keys {
		m, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = m[key]
	}
	return value
}

func idsOf(list any) []string {
	var out []string
	items, _ := list.([]any)
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			id, _ := m["id"].(string)
			out = append(out, id)
		}
	}
	return out
}

// ---- the suite ----

// TestAWSIssuesTwoReplicasOnOnePostgres runs two replicas on one database
// against the bad-deploy estate: one critical issue and one alert through a
// replica killed mid-evaluation, a reject on one replica that lands while the
// other evaluates, and a bulk reject the other replica reads at once.
func TestAWSIssuesTwoReplicasOnOnePostgres(t *testing.T) {
	dsn, bin, root := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("AWS_ISSUES_SERVER_BIN"), os.Getenv("AWS_REPLICAS_DIR")
	if dsn == "" || bin == "" || root == "" {
		t.Skip("needs TEST_POSTGRES_DSN, AWS_ISSUES_SERVER_BIN and AWS_REPLICAS_DIR")
	}
	root = filepath.Join(root, "issues")
	s := newIssuesSetup(t, root, dsn, "issues-verdict.json")
	db, gate, lark := s.db, s.gate, s.lark
	a := newReplica(t, root, "replica-a", newFront(s.w))
	b := newReplica(t, root, "replica-b", newFront(s.w))
	t.Cleanup(func() { a.stop(); b.stop(); a.front.server.Close(); b.front.server.Close() })
	process := issueProcess{bin: bin, dsn: dsn, gate: gate, lark: lark}
	byName := map[string]*replica{a.name: a, b.name: b}
	other := map[string]*replica{a.name: b, b.name: a}
	var issueID string

	ok := t.Run("one_issue_and_one_alert_through_a_kill_mid_evaluation", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		// The first evaluation that finds the alarm is held inside the rule,
		// with the replica lock held and nothing written yet.
		gate.holdFirstFinding()
		a.startIssues(process)
		b.startIssues(process)
		victimName := gate.waitHeld(t, "the first evaluation that finds the alarm")
		victim, survivor := byName[victimName], other[victimName]
		heldRows := len(db.issueRows(issuerule.FiringRuleID))
		heldLock := db.evaluationLockHolders("default")
		heldAlerts := lark.count()
		victim.kill()
		gate.releaseHeld()
		if heldRows != 0 || heldLock != 1 || heldAlerts != 0 {
			t.Errorf("while %s was held mid-evaluation: %d issue rows (want 0), %d lock holders (want 1), %d alerts (want 0)",
				victimName, heldRows, heldLock, heldAlerts)
		}
		// The survivor opens the issue on its own next evaluation.
		waitFor(t, "the survivor to open the issue and alert", evaluationGuard, func() bool {
			return len(db.issueRows(issuerule.FiringRuleID)) == 1 && lark.count() >= 1
		})
		openedBy := survivor.name
		victim.startIssues(process)
		// Each replica evaluates twice more, so the first of the two, with
		// its alert pump, has finished on both.
		want := map[string]int{a.name: gate.count(a.name) + 2, b.name: gate.count(b.name) + 2}
		maxAlerts := 0
		gate.waitCalls(t, "two more evaluations on each replica after the restart", want, func() {
			maxAlerts = max(maxAlerts, lark.count())
		})
		row := one(t, "after the kill and restart", db.issueRows(issuerule.FiringRuleID))
		issueID = row.id
		alerted, opened := db.events(row.id, "alerted"), db.events(row.id, "opened")
		if row.state != "open" || row.severity != "critical" {
			t.Errorf("issue %s is %s/%s, want open/critical", row.id, row.state, row.severity)
		}
		if alerted != 1 || opened != 1 || lark.count() != 1 || maxAlerts > 1 {
			t.Errorf("alerts: %d alerted events, %d opened events, %d Lark messages (most seen %d); want exactly one each. events: %v",
				alerted, opened, lark.count(), maxAlerts, db.eventLog(row.id))
		}
		message := ""
		if messages := lark.messages(); len(messages) > 0 {
			message = messages[0]
		}
		if !strings.Contains(message, "checkout-5xx") {
			t.Errorf("the Lark message does not name the alarm: %.400s", message)
		}
		s.out.put("kill_mid_evaluation", map[string]any{
			"held_replica": victimName, "opened_by": openedBy, "held": map[string]any{"rows": heldRows, "lock_holders": heldLock, "alerts": heldAlerts},
			"issue": row.id, "state": row.state, "version": row.version, "alerted_events": alerted, "opened_events": opened,
			"lark_messages": lark.count(), "max_lark_seen": maxAlerts, "evaluations": map[string]int{a.name: gate.count(a.name), b.name: gate.count(b.name)},
			"events": db.eventLog(row.id), "lark_first": fmt.Sprintf("%.600s", message),
		})
	})
	if !ok || issueID == "" {
		return
	}

	ok = t.Run("reject_on_a_while_b_evaluates_stays_rejected", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		before := one(t, "before the reject", db.issueRows(issuerule.FiringRuleID))
		alertsBefore := lark.count()
		gate.hold(b.name)
		if got := gate.waitHeld(t, "replica B's next evaluation"); got != b.name {
			t.Fatalf("held %s, want %s", got, b.name)
		}
		lockDuringB := db.evaluationLockHolders("default")
		// B has read the issue as open; the reject lands on A now.
		reject := a.call(http.MethodPost, "/api/admin/aws/issues/"+issueID+"/reject",
			`{"reason":"Expected while the checkout rollback runs"}`)
		afterReject := one(t, "after the reject", db.issueRows(issuerule.FiringRuleID))
		// A new note makes B's held evaluation write the issue, over the
		// version it read before the reject. Its retry is held again.
		gate.setAnswer(issuerule.Answer{Note: "race-1"})
		gate.hold(b.name)
		gate.releaseHeld()
		retried := gate.waitHeld(t, "replica B's retry after its write conflicted")
		atRetry := one(t, "at B's retry", db.issueRows(issuerule.FiringRuleID))
		gate.releaseHeld()
		waitFor(t, "B's retried write", evaluationGuard, func() bool {
			return strings.Contains(one(t, "after B's retry", db.issueRows(issuerule.FiringRuleID)).payload, "race-1")
		})
		afterB := one(t, "after B's write", db.issueRows(issuerule.FiringRuleID))

		if reject.status != http.StatusOK || pathOf(reject.json(), "issue", "state") != "rejected" {
			t.Fatalf("reject on A while B evaluated = %d %.400s", reject.status, reject.body)
		}
		if lockDuringB != 1 {
			t.Errorf("issue evaluation lock holders while B was held = %d, want 1", lockDuringB)
		}
		if retried != b.name || atRetry.state != "rejected" || atRetry.version != afterReject.version || strings.Contains(atRetry.payload, "race-1") {
			t.Errorf("B's held write did not conflict and retry: second hold on %s, issue %s v%d (reject wrote v%d), note written=%t",
				retried, atRetry.state, atRetry.version, afterReject.version, strings.Contains(atRetry.payload, "race-1"))
		}
		if afterB.state != "rejected" {
			t.Errorf("after B's evaluation the issue is %s, want rejected", afterB.state)
		}
		// Watch both replicas evaluate twice more: the issue is rejected at
		// every evaluation and nothing alerts.
		var states []string
		sample := func() {
			row := one(t, "sample", db.issueRows(issuerule.FiringRuleID))
			states = append(states, row.state)
			if row.state != "rejected" {
				t.Errorf("sample %d: issue is %s, want rejected", len(states), row.state)
			}
		}
		gate.waitCalls(t, "two more evaluations on each replica after the reject",
			map[string]int{a.name: gate.count(a.name) + 2, b.name: gate.count(b.name) + 2}, sample)
		end := one(t, "after the watch", db.issueRows(issuerule.FiringRuleID))
		detailA, detailB := a.call(http.MethodGet, "/api/admin/aws/issues/"+issueID, ""), b.call(http.MethodGet, "/api/admin/aws/issues/"+issueID, "")
		for _, got := range []answer{detailA, detailB} {
			if got.status != http.StatusOK || pathOf(got.json(), "issue", "state") != "rejected" {
				t.Errorf("issue detail = %d %.300s, want 200 rejected", got.status, got.body)
			}
		}
		alerted := db.events(issueID, "alerted")
		if end.state != "rejected" || lark.count() != alertsBefore || alerted != 1 || db.events(issueID, "rejected") != 1 ||
			db.events(issueID, "regressed")+db.events(issueID, "reopened") != 0 {
			t.Errorf("after the watch: %s, Lark %d (was %d), alerted %d; events %v", end.state, lark.count(), alertsBefore, alerted, db.eventLog(issueID))
		}
		s.out.put("reject_while_other_evaluates", map[string]any{
			"before": map[string]any{"state": before.state, "version": before.version}, "lock_holders_while_b_held": lockDuringB,
			"reject":       map[string]any{"status": reject.status, "took_ms": reject.took.Milliseconds(), "body": fmt.Sprintf("%.300s", reject.body)},
			"after_reject": map[string]any{"state": afterReject.state, "version": afterReject.version},
			"b_retry":      map[string]any{"replica": retried, "state": atRetry.state, "version": atRetry.version},
			"after_b":      map[string]any{"state": afterB.state, "version": afterB.version},
			"samples":      states, "end": map[string]any{"state": end.state, "version": end.version},
			"lark_messages": lark.count(), "alerted_events": alerted, "events": db.eventLog(issueID),
		})
	})
	if !ok {
		return
	}

	t.Run("bulk_reject_on_a_is_read_by_b_at_once", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		gate.setAnswer(issuerule.Answer{Listed: true})
		waitFor(t, "the listed alarms to open as medium issues", evaluationGuard, func() bool {
			rows := db.issueRows(issuerule.ListedRuleID)
			if len(rows) < 2 {
				return false
			}
			for _, row := range rows {
				if row.state != "open" {
					return false
				}
			}
			return true
		})
		var medium []string
		for _, row := range db.issueRows(issuerule.ListedRuleID) {
			medium = append(medium, row.id)
		}
		listBefore := b.call(http.MethodGet, "/api/admin/aws/issues", "")
		ids := append(append([]string{}, medium...), issueID, unknownIssueID)
		encoded, _ := json.Marshal(map[string]any{"ids": ids, "reason": "Inventory alarms are expected here"})
		bulk := a.call(http.MethodPost, "/api/admin/aws/issues/reject", string(encoded))
		// The next request on B, with nothing between.
		listB := b.call(http.MethodGet, "/api/admin/aws/issues", "")
		rejectedB := b.call(http.MethodGet, "/api/admin/aws/issues?state=rejected", "")
		reportB := b.call(http.MethodGet, "/api/admin/aws/issues/report", "")
		listA := a.call(http.MethodGet, "/api/admin/aws/issues", "")
		repeat := a.call(http.MethodPost, "/api/admin/aws/issues/reject", string(encoded))

		perID := map[string]string{}
		if raw, ok := bulk.json()["results"].([]any); ok {
			for _, item := range raw {
				m, _ := item.(map[string]any)
				id, _ := m["id"].(string)
				result, _ := m["result"].(string)
				perID[id] = result
			}
		}
		if bulk.status != http.StatusOK || perID[issueID] != "already_rejected" || perID[unknownIssueID] != "not_found" {
			t.Errorf("bulk reject = %d %.600s", bulk.status, bulk.body)
		}
		for _, id := range medium {
			if perID[id] != "rejected" {
				t.Errorf("bulk reject result for %s = %q, want rejected", id, perID[id])
			}
		}
		if raw, _ := repeat.json()["results"].([]any); repeat.status != http.StatusOK || strings.Count(string(repeat.body), `"already_rejected"`) != len(medium)+1 || len(raw) != len(ids) {
			t.Errorf("repeated bulk reject = %d %.600s, want every known issue already_rejected", repeat.status, repeat.body)
		}
		wantRejected := len(medium) + 1
		summaryA, summaryB := pathOf(listA.json(), "summary"), pathOf(listB.json(), "summary")
		if fmt.Sprint(summaryA) != fmt.Sprint(summaryB) {
			t.Errorf("summaries differ: A %v, B %v", summaryA, summaryB)
		}
		for _, got := range []struct {
			name string
			page answer
		}{{"A", listA}, {"B", listB}} {
			doc := got.page.json()
			if n, _ := doc["rejected_count"].(float64); int(n) != wantRejected {
				t.Errorf("%s rejected_count = %v, want %d", got.name, doc["rejected_count"], wantRejected)
			}
			if n, _ := pathOf(doc, "summary", "open").(float64); n != 0 {
				t.Errorf("%s summary open = %v, want 0 after the bulk reject", got.name, n)
			}
		}
		if listed := idsOf(rejectedB.json()["items"]); len(listed) != wantRejected {
			t.Errorf("B's Rejected tab lists %d issues, want %d: %.400s", len(listed), wantRejected, rejectedB.body)
		}
		report := reportB.json()
		if fmt.Sprint(report["summary"]) != fmt.Sprint(summaryB) {
			t.Errorf("B's report summary %v differs from its list %v", report["summary"], summaryB)
		}
		sections := map[string]int{}
		for _, name := range []string{"act_now", "worth_a_look", "rejected"} {
			list, _ := pathOf(report, "sections", name).([]any)
			sections[name] = len(list)
		}
		if sections["act_now"] != 0 || sections["worth_a_look"] != 0 || sections["rejected"] != wantRejected {
			t.Errorf("B's report sections = %v, want act_now 0, worth_a_look 0, rejected %d", sections, wantRejected)
		}
		dbRejected := db.count(`SELECT count(*) FROM aws_issues WHERE state = 'rejected'`)
		if dbRejected != wantRejected {
			t.Errorf("%d rejected rows in aws_issues, want %d", dbRejected, wantRejected)
		}
		// Watch both replicas evaluate twice more: B's counts never move
		// and nothing alerts.
		var samples []string
		gate.waitCalls(t, "two more evaluations on each replica after the bulk reject",
			map[string]int{a.name: gate.count(a.name) + 2, b.name: gate.count(b.name) + 2}, func() {
				doc := b.call(http.MethodGet, "/api/admin/aws/issues", "").json()
				samples = append(samples, fmt.Sprint(doc["summary"]))
				if n, _ := doc["rejected_count"].(float64); int(n) != wantRejected {
					t.Errorf("sample %d: B rejected_count %v, want %d", len(samples), doc["rejected_count"], wantRejected)
				}
			})
		if lark.count() != 1 {
			t.Errorf("Lark messages after the bulk reject = %d, want still 1", lark.count())
		}
		s.out.put("bulk_reject", map[string]any{
			"medium": medium, "results": perID, "repeat": fmt.Sprintf("%.600s", repeat.body),
			"list_b_before": fmt.Sprint(listBefore.json()["summary"]), "summary_a": summaryA, "summary_b": summaryB,
			"rejected_count_b": listB.json()["rejected_count"], "rejected_tab_b": idsOf(rejectedB.json()["items"]),
			"report_sections_b": sections, "report_summary_b": report["summary"], "db_rejected": dbRejected,
			"samples_b": samples, "lark_messages": lark.count(),
		})
	})
}

var bootstrapPassword = regexp.MustCompile(`Password:\s+(\S+)`)

// TestAWSIssueRejectionHoldsUnderEnterpriseLicence rejects the critical issue
// on the OSS server under the default organization, then starts the licensed
// Enterprise server on the same database: the issue stays rejected through
// its evaluations, no row opens in the licensed organization and nothing
// alerts. It runs in its own schema.
func TestAWSIssueRejectionHoldsUnderEnterpriseLicence(t *testing.T) {
	dsn, bin, root := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("AWS_ISSUES_SERVER_BIN"), os.Getenv("AWS_REPLICAS_DIR")
	enterpriseBin, licence := os.Getenv("AWS_ISSUES_ENTERPRISE_BIN"), os.Getenv("AWS_ISSUES_LICENSE_KEY")
	if dsn == "" || bin == "" || root == "" || enterpriseBin == "" || licence == "" {
		t.Skip("needs TEST_POSTGRES_DSN, AWS_ISSUES_SERVER_BIN, AWS_ISSUES_ENTERPRISE_BIN, AWS_ISSUES_LICENSE_KEY and AWS_REPLICAS_DIR")
	}
	root = filepath.Join(root, "issues-licence")
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "issues_licence_" + strconv.FormatInt(time.Now().Unix(), 10)
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	scoped := dsn + "&search_path=" + schema
	if !strings.Contains(dsn, "?") {
		scoped = dsn + "?search_path=" + schema
	}
	s := newIssuesSetup(t, root, scoped, "licence-verdict.json")
	db, gate, lark := s.db, s.gate, s.lark
	oss := newReplica(t, root, "oss", newFront(s.w))
	ent := newReplica(t, root, "enterprise", newFront(s.w))
	t.Cleanup(func() { oss.stop(); ent.stop(); oss.front.server.Close(); ent.front.server.Close() })

	// OSS: the issue opens and alerts once, then is rejected under default.
	oss.startIssues(issueProcess{bin: bin, dsn: scoped, gate: gate, lark: lark})
	waitFor(t, "the OSS server to open the issue and alert", evaluationGuard, func() bool {
		return len(db.issueRows(issuerule.FiringRuleID)) == 1 && lark.count() >= 1
	})
	opened := one(t, "OSS", db.issueRows(issuerule.FiringRuleID))
	reject := oss.call(http.MethodPost, "/api/admin/aws/issues/"+opened.id+"/reject", `{"reason":"Expected while the checkout rollback runs"}`)
	if reject.status != http.StatusOK {
		t.Fatalf("OSS reject = %d %.300s", reject.status, reject.body)
	}
	gate.waitCalls(t, "two more OSS evaluations", map[string]int{oss.name: gate.count(oss.name) + 2}, nil)
	rejected := one(t, "OSS after the reject", db.issueRows(issuerule.FiringRuleID))
	alertsBefore, alertedBefore := lark.count(), db.events(opened.id, "alerted")
	oss.stop()
	if rejected.org != "default" || rejected.state != "rejected" || alertsBefore != 1 || alertedBefore != 1 {
		t.Fatalf("before the licence: issue %s/%s, %d Lark messages, %d alerted events; want default/rejected, 1, 1",
			rejected.org, rejected.state, alertsBefore, alertedBefore)
	}

	// Enterprise, licensed, on the same database.
	from := ent.logSize()
	ent.startIssues(issueProcess{bin: enterpriseBin, dsn: scoped, gate: gate, lark: lark, licence: licence, enterprise: true})
	gateway := ent.call(http.MethodGet, "/api/admin/aws/issues", "")
	var cookie string
	var password string
	waitFor(t, "the licensed server's one-time admin credentials", 60*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(ent.dir, "server.log"))
		if err != nil || int64(len(data)) < from {
			return false
		}
		if match := bootstrapPassword.FindSubmatch(data[from:]); match != nil {
			password = string(match[1])
			return true
		}
		return false
	})
	login := ent.callWith(http.MethodPost, "/enterprise/api/auth/local/login", `{"username":"admin","password":"`+password+`"}`, nil)
	for _, line := range login.header.Values("Set-Cookie") {
		if name, _, found := strings.Cut(line, ";"); found && cookie == "" {
			cookie = name
		}
	}
	if login.status != http.StatusOK || cookie == "" {
		t.Fatalf("licensed admin login = %d %.300s", login.status, login.body)
	}
	session := map[string]string{"Cookie": cookie}
	// The Enterprise evaluator reads the estate and finds the alarm again,
	// twice, so the first of those evaluations has finished with its pump.
	start := gate.count(ent.name)
	findings := 0
	gate.waitCalls(t, "two Enterprise evaluations", map[string]int{ent.name: start + 2}, nil)
	waitFor(t, "an Enterprise evaluation that finds the alarm", evaluationGuard, func() bool {
		gate.mu.Lock()
		findings = gate.findings[ent.name]
		gate.mu.Unlock()
		return findings > 0
	})
	seen := gate.count(ent.name)
	gate.waitCalls(t, "two more Enterprise evaluations after it found the alarm", map[string]int{ent.name: seen + 2}, nil)
	rows := db.issueRows(issuerule.FiringRuleID)
	listed := ent.callWith(http.MethodGet, "/api/admin/aws/issues?state=rejected", "", session)
	detail := ent.callWith(http.MethodGet, "/api/admin/aws/issues/"+opened.id, "", session)
	org := ""
	if len(rows) == 1 {
		org = rows[0].org
	}
	if gateway.status != http.StatusUnauthorized && gateway.status != http.StatusForbidden {
		t.Errorf("licensed server answered the gateway secret on the issue list: %d %.200s", gateway.status, gateway.body)
	}
	if len(rows) != 1 || rows[0].state != "rejected" {
		t.Errorf("after the licence: aws_issues rows %+v, want one, rejected", rows)
	}
	if lark.count() != alertsBefore || db.events(opened.id, "alerted") != alertedBefore ||
		db.events(opened.id, "opened") != 1 || db.events(opened.id, "regressed")+db.events(opened.id, "reopened") != 0 {
		t.Errorf("after the licence: Lark %d (was %d); events %v", lark.count(), alertsBefore, db.eventLog(opened.id))
	}
	if !strings.Contains(string(listed.body), opened.id) || pathOf(detail.json(), "issue", "state") != "rejected" {
		t.Errorf("licensed API: rejected tab %d %.300s; detail %d %.300s", listed.status, listed.body, detail.status, detail.body)
	}
	s.out.put("licence", map[string]any{
		"oss": map[string]any{"issue": opened.id, "org": rejected.org, "state": rejected.state, "lark": alertsBefore, "alerted": alertedBefore},
		"enterprise": map[string]any{"gateway_status": gateway.status, "login_status": login.status, "evaluations": gate.count(ent.name),
			"findings_last": findings, "rows": fmt.Sprintf("%+v", rows), "org": org, "rejected_tab_status": listed.status,
			"rejected_tab_has_issue": strings.Contains(string(listed.body), opened.id), "detail_state": pathOf(detail.json(), "issue", "state")},
		"lark_messages": lark.count(), "events": db.eventLog(opened.id),
		"rows_by_org": db.count(`SELECT count(DISTINCT org_id) FROM aws_issues`),
	})
}
