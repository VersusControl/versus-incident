// Package awsreplicas runs two real versus-incident server processes on one
// Postgres database and drives the AWS settings and FinOps routes over HTTP,
// the way a Helm deployment with two replicas runs them.
//
// The server binary is a harness_fakeaws build, so every AWS call goes to a
// tests/fakeaws fixture served from this test process. Each replica reaches it
// through its own front, which counts the Cost Explorer requests that replica
// sent and can hold them, so a refresh can be kept in flight on one replica
// while the other is exercised. Nothing reaches AWS.
//
// The test is skipped unless these are set:
//
//	TEST_POSTGRES_DSN         an isolated, empty database (harness.sh pgtest)
//	AWS_REPLICAS_SERVER_BIN   go build -tags harness_fakeaws -o <bin> ./cmd
//	AWS_REPLICAS_DIR          a writable folder for configs and server logs
package awsreplicas

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	gatewaySecret = "aws-replicas-gateway-secret-0001"
	prodAccount   = "111111111111"
	dataAccount   = "222222222222"
	payerAccount  = "999999999999"
	statusBlob    = "aws/default/finops/status"
	// replicaLockKey is the cost refresh's Postgres advisory lock
	// (pkg/aws/cost refresh.go); holding it here stands in for a refresh in
	// flight on another replica.
	replicaLockKey int64 = 0x5653434F53545231
	// holdLimit stays under the cost adapter's 30 s request timeout, so a
	// held request is answered before the product gives up on it.
	holdLimit = 25 * time.Second
)

// ---- fake AWS fronts ----

var (
	credentialScope = regexp.MustCompile(`Credential=[^/]+/\d{8}/[^/]+/([^/]+)/aws4_request`)
	twelveDigits    = regexp.MustCompile(`\d{12}`)
)

// world is the fixture both replicas share: one fakeaws server, the scenario
// every request is answered from, and the hold on Cost Explorer requests.
type world struct {
	fake *fakeaws.Server

	mu       sync.Mutex
	scenario string
	held     chan struct{} // nil: Cost Explorer requests pass
}

func (w *world) setScenario(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.scenario = name
}

func (w *world) hold() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held == nil {
		w.held = make(chan struct{})
	}
}

func (w *world) release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held != nil {
		close(w.held)
		w.held = nil
	}
}

func (w *world) current() (string, chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.scenario, w.held
}

// front is one replica's view of the fixture.
type front struct {
	world  *world
	server *httptest.Server
	ce     atomic.Int64 // every Cost Explorer request
	paid   atomic.Int64 // the paid ones (all but the free tag listing)

	mu        sync.Mutex
	byAccount map[string]int64
}

func newFront(w *world) *front {
	f := &front{world: w, byAccount: map[string]int64{}}
	f.server = httptest.NewServer(f)
	return f
}

func (f *front) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	scenario, held := f.world.current()
	request.Header.Set(fakeaws.ScenarioHeader, scenario)
	service := ""
	if match := credentialScope.FindStringSubmatch(request.Header.Get("Authorization")); match != nil {
		service = match[1]
	}
	if service == "ce" {
		f.ce.Add(1)
		if target := request.Header.Get("X-Amz-Target"); !strings.HasSuffix(target, ".ListCostAllocationTags") {
			f.paid.Add(1)
		}
		caller := twelveDigits.FindString(request.Header.Get("X-Amz-Security-Token"))
		f.mu.Lock()
		f.byAccount[caller]++
		f.mu.Unlock()
		if held != nil {
			select {
			case <-held:
			case <-time.After(holdLimit):
			case <-request.Context().Done():
			}
		}
	}
	f.world.fake.ServeHTTP(writer, request)
}

func (f *front) account(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byAccount[id]
}

// ---- server replicas ----

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

// connectors is the AWS connector block a replica boots with.
type connectors struct {
	costData   bool
	costSource string
	paidCap    int
}

func (c connectors) yaml() string {
	return fmt.Sprintf(`connectors:
  aws:
    enable: true
    accounts: []
    tags:
      value_allowlist: [team]
    budgets:
      max_requests_per_second: 100
    optional_data:
      cost_explorer: %t
    collect:
      enable: false
    finops:
      team_tag_key: team
      max_paid_requests_per_day: %d
      refresh_time: "00:00"
      cost_source_account: %q
      report:
        timezone: UTC
`, c.costData, c.paidCap, c.costSource)
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
	// The server reads config/*.yaml and the alert templates relative to its
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

// logSize is where this replica's server log ends now.
func (r *replica) logSize() int64 {
	info, err := os.Stat(filepath.Join(r.dir, "server.log"))
	if err != nil {
		return 0
	}
	return info.Size()
}

// logCount counts text in this replica's server log after offset from.
func (r *replica) logCount(from int64, text string) int {
	data, err := os.ReadFile(filepath.Join(r.dir, "server.log"))
	if err != nil || int64(len(data)) < from {
		return 0
	}
	return strings.Count(string(data[from:]), text)
}

// summaryTotal is the FinOps summary total this replica answers for a period.
func (r *replica) summaryTotal(period string) float64 {
	r.t.Helper()
	got := r.call(http.MethodGet, "/api/admin/aws/finops/summary?period="+period, "")
	total, ok := got.json()["total_micros"].(float64)
	if got.status != http.StatusOK || !ok {
		r.t.Fatalf("%s summary %s = %d %.300s", r.name, period, got.status, got.body)
	}
	return total
}

func (r *replica) start(bin, dsn string, c connectors) {
	t := r.t
	t.Helper()
	config := fmt.Sprintf(`name: aws-replica-%s
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
  enable: false
`, r.name, r.port)
	if err := os.WriteFile(filepath.Join(r.dir, "config", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "config", "connectors.yaml"), []byte(c.yaml()), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(r.dir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(logFile, "\n==== %s start %s cost_data=%t cost_source=%q cap=%d\n", r.name, time.Now().UTC().Format(time.RFC3339), c.costData, c.costSource, c.paidCap)
	cmd := exec.Command(bin)
	cmd.Dir = r.dir
	// A minimal environment: nothing from the caller's shell, so no real AWS
	// credential or profile can be picked up. The base credentials are fake;
	// the harness_fakeaws build sends every AWS request to the front.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.dir,
		"TZ=UTC",
		"GATEWAY_SECRET=" + gatewaySecret,
		"POSTGRES_DSN=" + dsn,
		"VERSUS_HARNESS_FAKEAWS_URL=" + r.front.server.URL,
		"AWS_ACCESS_KEY_ID=ASIAVERSUSBASE00001",
		"AWS_SECRET_ACCESS_KEY=versus-replica-base-secret",
		"AWS_REGION=us-east-1",
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_CONFIG_FILE=" + filepath.Join(r.dir, "no-aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(r.dir, "no-aws-credentials"),
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
	header http.Header
	body   []byte
	took   time.Duration
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

// connect runs the setup wizard for one account on this replica.
func (r *replica) connect(account string) {
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
	commit := r.call(http.MethodPost, "/api/admin/aws/accounts",
		`{"draft_id":"`+draft+`","display_name":"qa-`+account+`","regions":["us-east-1"]}`)
	if commit.status != http.StatusCreated {
		t.Fatalf("%s: commit %s = %d %s", r.name, account, commit.status, commit.body)
	}
}

// ---- database checks ----

type database struct {
	t  *testing.T
	db *sql.DB
}

func (d *database) count(query string, args ...any) int {
	d.t.Helper()
	var n int
	if err := d.db.QueryRow(query, args...).Scan(&n); err != nil {
		d.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (d *database) lines(payer string) int {
	return d.count(`SELECT count(*) FROM finops_cost_lines WHERE payer = $1`, payer)
}

func (d *database) decisions(payer string) int {
	return d.count(`SELECT count(*) FROM finops_saving_decisions WHERE payer = $1`, payer)
}

// decisionRows lists the stored saving decisions with their subject.
func (d *database) decisionRows() []map[string]any {
	d.t.Helper()
	rows, err := d.db.Query(`SELECT saving_id, payer, account, kind, orphaned_at IS NOT NULL FROM finops_saving_decisions ORDER BY saving_id`)
	if err != nil {
		d.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, payer, account, kind string
		var orphaned bool
		if err := rows.Scan(&id, &payer, &account, &kind, &orphaned); err != nil {
			d.t.Fatal(err)
		}
		out = append(out, map[string]any{"saving_id": id, "payer": payer, "account": account, "kind": kind, "orphaned": orphaned})
	}
	if err := rows.Err(); err != nil {
		d.t.Fatal(err)
	}
	return out
}

// rollupKey is one daily cost rollup row.
type rollupKey struct {
	org, payer, day, account, region, service, serviceName, usageGroup, recordType, team string
	net                                                                                  int64
}

// rollupRow picks the latest usage row of the daily rollup in a month that
// has ended, so a whole-month summary reads it.
func (d *database) rollupRow() rollupKey {
	d.t.Helper()
	var k rollupKey
	err := d.db.QueryRow(`SELECT org_id, payer, to_char(day, 'YYYY-MM-DD'), account, region, service, service_name, usage_group, record_type, team,
		net_amortized_micros FROM finops_cost_daily WHERE record_type = 'usage' AND day < date_trunc('month', current_date)::date
		ORDER BY day DESC, net_amortized_micros DESC LIMIT 1`).
		Scan(&k.org, &k.payer, &k.day, &k.account, &k.region, &k.service, &k.serviceName, &k.usageGroup, &k.recordType, &k.team, &k.net)
	if err != nil {
		d.t.Fatalf("no daily rollup row in an ended month: %v", err)
	}
	return k
}

const rollupKeyWhere = `org_id = $2 AND payer = $3 AND account = $4 AND region = $5 AND service = $6 AND service_name = $7
	AND usage_group = $8 AND record_type = $9 AND team = $10`

// driftRollup adds micros to the row's daily rollup and to its month's, as a
// writer that bypassed the rollup upkeep would leave them.
func (d *database) driftRollup(k rollupKey, micros int64) {
	d.t.Helper()
	args := []any{micros, k.org, k.payer, k.account, k.region, k.service, k.serviceName, k.usageGroup, k.recordType, k.team, k.day}
	for _, statement := range []string{
		`UPDATE finops_cost_daily SET net_amortized_micros = net_amortized_micros + $1 WHERE ` + rollupKeyWhere + ` AND day = $11::date`,
		`UPDATE finops_cost_monthly SET net_amortized_micros = net_amortized_micros + $1 WHERE ` + rollupKeyWhere +
			` AND month = date_trunc('month', $11::date)::date`,
	} {
		result, err := d.db.Exec(statement, args...)
		if err != nil {
			d.t.Fatalf("%s: %v", statement, err)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			d.t.Fatalf("%s changed %d rows, want 1", statement, n)
		}
	}
}

// rollupNet reads the row's daily rollup amount.
func (d *database) rollupNet(k rollupKey) int64 {
	d.t.Helper()
	var net int64
	if err := d.db.QueryRow(`SELECT net_amortized_micros FROM finops_cost_daily WHERE org_id = $1 AND payer = $2 AND account = $3
		AND region = $4 AND service = $5 AND service_name = $6 AND usage_group = $7 AND record_type = $8 AND team = $9 AND day = $10::date`,
		k.org, k.payer, k.account, k.region, k.service, k.serviceName, k.usageGroup, k.recordType, k.team, k.day).Scan(&net); err != nil {
		d.t.Fatalf("daily rollup row: %v", err)
	}
	return net
}

func (d *database) blob(name string) map[string]any {
	d.t.Helper()
	var data []byte
	err := d.db.QueryRow(`SELECT data FROM vs_blobs WHERE name = $1`, name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		d.t.Fatalf("blob %s: %v", name, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		d.t.Fatalf("blob %s: %v", name, err)
	}
	return out
}

func (d *database) writeBlob(name string, doc map[string]any) {
	d.t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.db.Exec(`UPDATE vs_blobs SET data = $2, updated_at = now() WHERE name = $1`, name, data); err != nil {
		d.t.Fatal(err)
	}
}

func today() string { return time.Now().UTC().Format(time.DateOnly) }

func (d *database) meter() int {
	doc := d.blob("aws/default/finops/meter/" + today())
	n, _ := doc["requests"].(float64)
	return int(n)
}

func (d *database) scheduledToday() bool {
	scheduled, _ := d.blob(statusBlob)["scheduled_on"].(string)
	return scheduled == today()
}

// lockHolders is how many sessions hold the cost refresh's advisory lock.
func (d *database) lockHolders() int {
	key := uint64(replicaLockKey)
	return d.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
		AND classid = $1::bigint::oid AND objid = $2::bigint::oid AND objsubid = 1`,
		int64(key>>32), int64(key&0xffffffff))
}

// forceScheduledRefresh clears the day the scheduled refresh last ran, so the
// next tick runs it again. The stored status is otherwise kept as it is.
func (d *database) forceScheduledRefresh() {
	doc := d.blob(statusBlob)
	if doc == nil {
		return
	}
	delete(doc, "scheduled_on")
	d.writeBlob(statusBlob, doc)
}

// shiftBlobs moves every timestamp in the JSON blobs under prefix forward by
// d and reports how many it moved.
func (d *database) shiftBlobs(prefix string, by time.Duration) int {
	d.t.Helper()
	rows, err := d.db.Query(`SELECT name FROM vs_blobs WHERE name LIKE $1`, prefix+"%")
	if err != nil {
		d.t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			d.t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		d.t.Fatal(err)
	}
	_ = rows.Close()
	moved := 0
	for _, name := range names {
		doc := d.blob(name)
		if doc == nil {
			continue
		}
		if n := shiftTimes(doc, by); n > 0 {
			moved += n
			d.writeBlob(name, doc)
		}
	}
	return moved
}

// shiftRows moves the stored FinOps row times forward by d.
func (d *database) shiftRows(by time.Duration) {
	d.t.Helper()
	for _, statement := range []string{
		`UPDATE finops_cost_lines SET refreshed_at = refreshed_at + $1::interval`,
		`UPDATE finops_saving_decisions SET at = at + $1::interval WHERE at IS NOT NULL`,
	} {
		if _, err := d.db.Exec(statement, fmt.Sprintf("%d seconds", int(by.Seconds()))); err != nil {
			d.t.Fatalf("%s: %v", statement, err)
		}
	}
}

func waitFor(t *testing.T, what string, limit time.Duration, ok func() bool) time.Duration {
	t.Helper()
	started := time.Now()
	for !ok() {
		if time.Since(started) > limit {
			t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return time.Since(started)
}

// ---- results ----

type results struct {
	mu   sync.Mutex
	path string
	data map[string]any
}

func (r *results) put(key string, value any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[key] = value
	encoded, _ := json.MarshalIndent(r.data, "", "  ")
	_ = os.WriteFile(r.path, encoded, 0o600)
}

// ---- the suite ----

func TestAWSTwoReplicasOnOnePostgres(t *testing.T) {
	dsn, bin, root := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("AWS_REPLICAS_SERVER_BIN"), os.Getenv("AWS_REPLICAS_DIR")
	if dsn == "" || bin == "" || root == "" {
		t.Skip("needs TEST_POSTGRES_DSN, AWS_REPLICAS_SERVER_BIN and AWS_REPLICAS_DIR")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	db := &database{t: t, db: pool}
	out := &results{path: filepath.Join(root, "replicas-verdict.json"), data: map[string]any{}}

	fake, err := fakeaws.NewServer(fakeaws.ScenarioCostOrganization)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	w := &world{fake: fake, scenario: fakeaws.ScenarioCostOrganization}
	t.Cleanup(w.release)
	a := newReplica(t, root, "replica-a", newFront(w))
	b := newReplica(t, root, "replica-b", newFront(w))
	t.Cleanup(func() { a.stop(); b.stop(); a.front.server.Close(); b.front.server.Close() })
	single := connectors{costData: true, paidCap: 20}
	off := connectors{costData: false, paidCap: 20}
	restart := func(c connectors) {
		a.stop()
		b.stop()
		a.start(bin, dsn, c)
		b.start(bin, dsn, c)
	}

	ok := t.Run("connect_two_accounts_seen_by_both_replicas", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		a.start(bin, dsn, off)
		b.start(bin, dsn, off)
		a.connect(prodAccount)
		b.connect(dataAccount)
		for _, r := range []*replica{a, b} {
			settings := r.call(http.MethodGet, "/api/admin/aws/settings", "")
			if !bytes.Contains(settings.body, []byte(prodAccount)) || !bytes.Contains(settings.body, []byte(dataAccount)) {
				t.Fatalf("%s settings do not list both accounts: %.400s", r.name, settings.body)
			}
		}
		revision := db.blob("aws/default/accounts-revision")
		out.put("connect", map[string]any{"accounts_revision": revision})
	})
	if !ok {
		return
	}

	t.Run("one_replica_runs_the_scheduled_refresh", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		a.stop()
		b.stop()
		w.hold()
		a.start(bin, dsn, single)
		waitFor(t, "replica A's scheduled refresh to reach Cost Explorer", 60*time.Second, func() bool { return a.front.ce.Load() > 0 })
		b.start(bin, dsn, single)
		time.Sleep(3 * time.Second) // B's first tick runs at boot
		holders := db.lockHolders()
		bDuringHold := b.front.ce.Load()

		refresh := b.call(http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true}`)
		drill := b.call(http.MethodPost, "/api/admin/aws/finops/drilldown", `{"account":"`+prodAccount+`","service":"EC2 - Other"}`)
		remove := b.call(http.MethodDelete, "/api/admin/aws/accounts/"+dataAccount, "")
		bAfterCalls := b.front.ce.Load()
		w.release()
		waited := waitFor(t, "the scheduled refresh to finish", 120*time.Second, db.scheduledToday)

		for _, got := range []struct {
			name string
			a    answer
		}{{"refresh", refresh}, {"drilldown", drill}} {
			code, _ := got.a.json()["error"].(string)
			if got.a.status != http.StatusConflict || code != "refresh_in_progress" || got.a.header.Get("Retry-After") != "5" {
				t.Errorf("replica B %s during A's refresh = %d %s Retry-After=%q, want 409 refresh_in_progress with Retry-After 5",
					got.name, got.a.status, got.a.body, got.a.header.Get("Retry-After"))
			}
		}
		if holders != 1 {
			t.Errorf("advisory lock holders during the refresh = %d, want 1", holders)
		}
		if bDuringHold != 0 || bAfterCalls != 0 {
			t.Errorf("replica B sent Cost Explorer requests while A refreshed: %d at boot, %d after its calls", bDuringHold, bAfterCalls)
		}
		purge, _ := remove.json()["purge"].(string)
		if remove.status != http.StatusOK || (purge != "pending" && len(remove.json()) != 0) {
			t.Errorf("DELETE during the refresh = %d %s, want 200 {} or {\"purge\":\"pending\"}", remove.status, remove.body)
		}
		// The refresh that held the lock purges a payer removed while it ran.
		waitFor(t, "the removed payer's lines to be purged", 90*time.Second, func() bool { return db.lines(dataAccount) == 0 })
		pending := db.blob(statusBlob)["purge_pending"]
		paid := fake.PaidCostRequests()
		meter := db.meter()
		if paid > 20 || meter > 20 || int64(meter) != paid {
			t.Errorf("paid requests: fixture %d, meter %d, want equal and at most 20", paid, meter)
		}
		if db.lines(prodAccount) == 0 {
			t.Errorf("no cost lines stored for %s", prodAccount)
		}
		if pending != nil {
			if m, _ := pending.(map[string]any); len(m) != 0 {
				t.Errorf("purge_pending left after the purge: %v", pending)
			}
		}
		out.put("scheduled_refresh", map[string]any{
			"lock_holders_during_refresh": holders, "replica_a_ce_requests": a.front.ce.Load(), "replica_b_ce_requests": b.front.ce.Load(),
			"refresh_on_b":                      map[string]any{"status": refresh.status, "body": string(refresh.body), "retry_after": refresh.header.Get("Retry-After")},
			"drill_on_b":                        map[string]any{"status": drill.status, "body": string(drill.body), "retry_after": drill.header.Get("Retry-After")},
			"delete_on_b":                       map[string]any{"status": remove.status, "body": string(remove.body), "took_ms": remove.took.Milliseconds()},
			"refresh_finished_after_release_ms": waited.Milliseconds(), "paid_fixture": paid, "meter": meter,
			"lines_prod": db.lines(prodAccount), "lines_removed_payer": db.lines(dataAccount),
		})
	})

	t.Run("second_replica_skips_the_day_and_the_cap_holds", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		// B's next minute tick sees today's refresh done and sends nothing.
		before := b.front.ce.Load()
		time.Sleep(65 * time.Second)
		if after := b.front.ce.Load(); after != before {
			t.Errorf("replica B ran a scheduled refresh after A's: %d -> %d Cost Explorer requests", before, after)
		}
		manualB := b.call(http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true}`)
		manualA := a.call(http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true}`)
		var drills []map[string]any
		services := []string{"EC2 - Other", "Amazon Elastic Compute Cloud - Compute", "Amazon Simple Storage Service"}
		for i := 0; i < 8; i++ {
			r := []*replica{a, b}[i%2]
			got := r.call(http.MethodPost, "/api/admin/aws/finops/drilldown", `{"account":"`+prodAccount+`","service":"`+services[i%len(services)]+`"}`)
			drills = append(drills, map[string]any{"replica": r.name, "status": got.status, "body": fmt.Sprintf("%.160s", got.body)})
		}
		paid, meter := fake.PaidCostRequests(), db.meter()
		if paid > 20 || meter > 20 {
			t.Errorf("paid requests over the daily cap: fixture %d, meter %d", paid, meter)
		}
		if code, _ := manualA.json()["error"].(string); manualB.status == http.StatusOK && (manualA.status != http.StatusTooManyRequests || code != "manual_refresh_used") {
			t.Errorf("second manual refresh on the other replica = %d %s, want 429 manual_refresh_used", manualA.status, manualA.body)
		}
		out.put("cap", map[string]any{
			"manual_on_b": map[string]any{"status": manualB.status, "body": fmt.Sprintf("%.300s", manualB.body)},
			"manual_on_a": map[string]any{"status": manualA.status, "body": fmt.Sprintf("%.300s", manualA.body)},
			"drills":      drills, "paid_fixture": paid, "meter": meter,
			"replica_a_ce_requests": a.front.ce.Load(), "replica_b_ce_requests": b.front.ce.Load(),
		})
	})

	t.Run("each_replica_reconciles_the_rollups_under_the_lock", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		const drift = 7_000_000_000 // $7,000 a writer added to the rollups only
		const rebuilt = "cost rollups reconciled in"
		row := db.rollupRow()
		period := row.day[:7]
		truthA, truthB := a.summaryTotal(period), b.summaryTotal(period)
		if truthA != truthB {
			t.Fatalf("the replicas disagree before the drift: %v vs %v", truthA, truthB)
		}

		// Both replicas boot while another holder has the replica lock: neither
		// may reconcile beside it, and both read totals from the cost lines.
		a.stop()
		b.stop()
		db.driftRollup(row, drift)
		conn, err := pool.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var held bool
		if err := conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, replicaLockKey).Scan(&held); err != nil || !held {
			t.Fatalf("could not hold the replica lock: %v %v", held, err)
		}
		fromA, fromB := a.logSize(), b.logSize()
		a.start(bin, dsn, single)
		b.start(bin, dsn, single)
		time.Sleep(3 * time.Second) // each replica's first tick runs at boot
		heldA, heldB := a.summaryTotal(period), b.summaryTotal(period)
		netWhileHeld := db.rollupNet(row)
		if heldA != truthA || heldB != truthA {
			t.Errorf("before reconciling, totals = A %v, B %v; want %v from the cost lines, not the drifted rollup", heldA, heldB, truthA)
		}
		if netWhileHeld != row.net+drift {
			t.Errorf("the rollup was reconciled beside the lock holder: %d, drifted %d", netWhileHeld, row.net+drift)
		}

		// Released: the next tick of each replica reconciles, one at a time.
		if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, replicaLockKey); err != nil {
			t.Fatal(err)
		}
		released := time.Now()
		repaired := waitFor(t, "a replica to repair the drifted rollup", 90*time.Second, func() bool { return db.rollupNet(row) == row.net })
		time.Sleep(time.Until(released.Add(65 * time.Second))) // both replicas' ticks have run
		rebuiltA, rebuiltB := a.logCount(fromA, rebuilt), b.logCount(fromB, rebuilt)
		if rebuiltA+rebuiltB != 1 {
			t.Errorf("rollup rebuilds after the release: A %d, B %d; want exactly one (the other replica compares and finds nothing)", rebuiltA, rebuiltB)
		}

		// Each replica now reads totals from the rollups: a new drift shows on
		// both, which it could not if either still read the lines.
		db.driftRollup(row, drift)
		readsA, readsB := a.summaryTotal(period), b.summaryTotal(period)
		if readsA != truthA+drift || readsB != truthA+drift {
			t.Errorf("after reconciling, totals = A %v, B %v; want %v from the rollups on both replicas", readsA, readsB, truthA+drift)
		}

		// A restart reconciles again: one replica repairs, both answer the truth.
		fromA, fromB = a.logSize(), b.logSize()
		restart(single)
		repairedOnBoot := waitFor(t, "the restarted replicas to repair the rollup", 90*time.Second, func() bool { return db.rollupNet(row) == row.net })
		time.Sleep(3 * time.Second)
		bootA, bootB := a.logCount(fromA, rebuilt), b.logCount(fromB, rebuilt)
		finalA, finalB := a.summaryTotal(period), b.summaryTotal(period)
		if bootA+bootB != 1 || finalA != truthA || finalB != truthA {
			t.Errorf("after a restart: rebuilds A %d, B %d (want one in total); totals A %v, B %v (want %v)", bootA, bootB, finalA, finalB, truthA)
		}
		out.put("rollup_reconcile", map[string]any{
			"row":    map[string]any{"payer": row.payer, "day": row.day, "account": row.account, "service": row.service, "net": row.net},
			"period": period, "drift_micros": drift, "truth_total": truthA,
			"lock_held":             map[string]any{"total_a": heldA, "total_b": heldB, "rollup_net": netWhileHeld},
			"after_release":         map[string]any{"repaired_after_ms": repaired.Milliseconds(), "rebuilds_a": rebuiltA, "rebuilds_b": rebuiltB},
			"drift_after_reconcile": map[string]any{"total_a": readsA, "total_b": readsB},
			"restart": map[string]any{"repaired_after_ms": repairedOnBoot.Milliseconds(), "rebuilds_a": bootA, "rebuilds_b": bootB,
				"total_a": finalA, "total_b": finalB},
		})
	})

	t.Run("payer_removal_deletes_its_saving_decisions", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		// The NAT spike fixture produces savings for the production account;
		// the day's meter is spent, so the setup refresh gets its own budget.
		w.setScenario(fakeaws.ScenarioCostNatSpike)
		restart(connectors{costData: true, paidCap: 200})
		db.forceScheduledRefresh()
		waitFor(t, "the NAT spike refresh", 150*time.Second, db.scheduledToday)
		savings := a.call(http.MethodGet, "/api/admin/aws/finops/savings?period=2026-10", "")
		var items []map[string]any
		if raw, ok := savings.json()["items"].([]any); ok {
			for _, item := range raw {
				if m, ok := item.(map[string]any); ok {
					items = append(items, m)
				}
			}
		}
		if len(items) == 0 {
			t.Fatalf("no saving to dismiss: %.400s", savings.body)
		}
		// Dismiss every listed saving: those tied to an account and those
		// that are not (a commitment belongs to the deployment's bill).
		var dismissed []string
		withAccount := 0
		for _, item := range items {
			id, _ := item["id"].(string)
			if account, _ := item["account"].(string); account != "" {
				withAccount++
			}
			dismiss := a.call(http.MethodPost, "/api/admin/aws/finops/savings/"+id+"/dismiss", `{"reason":"qa replicas"}`)
			if dismiss.status != http.StatusOK {
				t.Fatalf("dismiss %s = %d %s", id, dismiss.status, dismiss.body)
			}
			dismissed = append(dismissed, id)
		}
		// A decision stored before the subject columns existed has no payer.
		if _, err := pool.Exec(`INSERT INTO finops_saving_decisions (org_id, saving_id, state, reason, actor, at, payer, account)
			VALUES ('default', 'qa-legacy-prod', 'dismissed', 'legacy', 'qa', now(), '', $1)`, prodAccount); err != nil {
			t.Fatal(err)
		}
		rowsBefore := db.decisionRows()
		before := db.decisions(prodAccount)
		remove := b.call(http.MethodDelete, "/api/admin/aws/accounts/"+prodAccount, "")
		legacy := db.count(`SELECT count(*) FROM finops_saving_decisions WHERE saving_id = 'qa-legacy-prod'`)
		rowsAfter := db.decisionRows()
		if remove.status != http.StatusOK || len(remove.json()) != 0 {
			t.Errorf("DELETE with no refresh running = %d %s, want 200 {}", remove.status, remove.body)
		}
		// With one payer connected every dismissed saving is that payer's,
		// the Savings Plan commitment (no account) included.
		if after := db.decisions(prodAccount); before != len(dismissed) || after != 0 || legacy != 0 {
			t.Errorf("decisions of the removed payer: %d before (want %d, one per dismissed saving), %d after, payer-less on its account %d",
				before, len(dismissed), after, legacy)
		}
		if n := db.lines(prodAccount); n != 0 {
			t.Errorf("%d cost lines left for the removed payer", n)
		}
		// The last payer is gone: no decision is left, the Savings Plan
		// commitment one included, since nothing will produce its saving again.
		if len(rowsAfter) != 0 {
			t.Errorf("saving decisions left after the last payer was removed: %v", rowsAfter)
		}
		out.put("payer_removal", map[string]any{"dismissed": dismissed, "with_account": withAccount, "decisions_before": before,
			"decisions_after": db.decisions(prodAccount), "legacy_left": legacy, "rows_before": rowsBefore, "rows_after_last_payer_removed": rowsAfter,
			"delete": map[string]any{"status": remove.status, "body": string(remove.body), "took_ms": remove.took.Milliseconds()}})
	})

	t.Run("pending_purge_clears_on_the_tick_with_cost_data_off", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		// The day's meter is spent; give the setup refresh its own budget.
		restart(connectors{costData: true, paidCap: 200})
		b.connect(prodAccount) // added again at a newer revision
		db.forceScheduledRefresh()
		waitFor(t, "the re-added payer to be collected again", 150*time.Second, func() bool { return db.lines(prodAccount) > 0 })
		restart(off)
		conn, err := pool.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var got bool
		if err := conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, replicaLockKey).Scan(&got); err != nil || !got {
			t.Fatalf("could not hold the refresh lock: %v %v", got, err)
		}
		remove := a.call(http.MethodDelete, "/api/admin/aws/accounts/"+prodAccount, "")
		linesWhileHeld := db.lines(prodAccount)
		pendingWhileHeld := db.blob(statusBlob)["purge_pending"]
		if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, replicaLockKey); err != nil {
			t.Fatal(err)
		}
		if purge, _ := remove.json()["purge"].(string); remove.status != http.StatusOK || purge != "pending" {
			t.Errorf("DELETE with the lock held elsewhere = %d %s, want 200 {\"purge\":\"pending\"}", remove.status, remove.body)
		}
		if linesWhileHeld == 0 {
			t.Errorf("lines were purged while the lock was held; the deferred path was not exercised")
		}
		cleared := waitFor(t, "the next tick to purge the pending payer with cost data off", 150*time.Second, func() bool {
			pending, _ := db.blob(statusBlob)["purge_pending"].(map[string]any)
			return db.lines(prodAccount) == 0 && len(pending) == 0
		})
		out.put("pending_purge_cost_off", map[string]any{
			"delete":           map[string]any{"status": remove.status, "body": string(remove.body), "took_ms": remove.took.Milliseconds()},
			"lines_while_held": linesWhileHeld, "pending_while_held": pendingWhileHeld, "cleared_after_ms": cleared.Milliseconds(),
			"decisions_after": db.decisions(prodAccount),
		})
		if n := db.decisions(prodAccount); n != 0 {
			t.Errorf("%d decisions left for the purged payer", n)
		}
	})

	t.Run("organization_member_removal_keeps_payer_decisions", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		w.setScenario(fakeaws.ScenarioCostOrganization)
		org := connectors{costData: true, costSource: payerAccount, paidCap: 200}
		restart(org)
		a.connect(payerAccount)
		b.connect(dataAccount)
		db.forceScheduledRefresh()
		waitFor(t, "the Organization payer's refresh", 150*time.Second, func() bool { return db.lines(payerAccount) > 0 && db.scheduledToday() })
		// The Organization fixture produces no saving to dismiss, so the payer's
		// decisions are stored the way a dismissal stores them: with the payer
		// and the member account they were listed under.
		for i, account := range []string{payerAccount, prodAccount, dataAccount} {
			if _, err := pool.Exec(`INSERT INTO finops_saving_decisions (org_id, saving_id, state, reason, actor, at, payer, account, kind, title)
				VALUES ('default', $1, 'dismissed', 'qa org', 'qa', now(), $2, $3, 'rightsizing', 'qa')`,
				"qa-org-"+strconv.Itoa(i), payerAccount, account); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(`INSERT INTO finops_saving_decisions (org_id, saving_id, state, reason, actor, at, payer, account)
			VALUES ('default', 'qa-legacy-member', 'dismissed', 'legacy', 'qa', now(), '', $1)`, dataAccount); err != nil {
			t.Fatal(err)
		}
		payerDecisions, payerLines := db.decisions(payerAccount), db.lines(payerAccount)
		removeMember := b.call(http.MethodDelete, "/api/admin/aws/accounts/"+dataAccount, "")
		afterMember := db.decisions(payerAccount)
		legacyAfterMember := db.count(`SELECT count(*) FROM finops_saving_decisions WHERE saving_id = 'qa-legacy-member'`)
		linesAfterMember := db.lines(payerAccount)
		if removeMember.status != http.StatusOK {
			t.Errorf("DELETE member = %d %s", removeMember.status, removeMember.body)
		}
		if payerDecisions != 3 || afterMember != payerDecisions || legacyAfterMember != 1 || linesAfterMember != payerLines {
			t.Errorf("member removal touched the payer: decisions %d -> %d, payer-less on the member %d, lines %d -> %d",
				payerDecisions, afterMember, legacyAfterMember, payerLines, linesAfterMember)
		}
		removePayer := a.call(http.MethodDelete, "/api/admin/aws/accounts/"+payerAccount, "")
		if removePayer.status != http.StatusOK || db.decisions(payerAccount) != 0 || db.lines(payerAccount) != 0 {
			t.Errorf("payer removal = %d %s; decisions left %d, lines left %d", removePayer.status, removePayer.body,
				db.decisions(payerAccount), db.lines(payerAccount))
		}
		out.put("organization", map[string]any{
			"payer_decisions_before": payerDecisions, "payer_decisions_after_member": afterMember, "legacy_member_after_member": legacyAfterMember,
			"payer_lines_before": payerLines, "payer_lines_after_member": linesAfterMember,
			"delete_member":               map[string]any{"status": removeMember.status, "body": string(removeMember.body)},
			"delete_payer":                map[string]any{"status": removePayer.status, "body": string(removePayer.body)},
			"payer_decisions_after_payer": db.decisions(payerAccount), "payer_lines_after_payer": db.lines(payerAccount),
			"legacy_member_after_payer": db.count(`SELECT count(*) FROM finops_saving_decisions WHERE saving_id = 'qa-legacy-member'`),
		})
	})

	t.Run("clock_step_back_keeps_removed_payers_out", func(t *testing.T) {
		a.t, b.t, db.t = t, t, t
		// A clock stepped back by two days sees every stored time two days
		// ahead of now. Move every stored AWS time there (settings, revision,
		// cost status, meter), keep the tombstones as they are, and let a
		// scheduled refresh run with one payer connected.
		removed := db.blob(statusBlob)["removed_payers"]
		shifted := db.shiftBlobs("aws/default/", 48*time.Hour)
		doc := db.blob(statusBlob)
		delete(doc, "scheduled_on")
		db.writeBlob(statusBlob, doc)
		db.shiftRows(48 * time.Hour)
		restart(connectors{costData: true, paidCap: 200})
		removedCalls := map[string]int64{}
		for _, id := range []string{payerAccount, dataAccount} {
			removedCalls[id] = a.front.account(id) + b.front.account(id)
		}
		a.connect(prodAccount)
		defer func() {
			out.put("clock_step_back_state", map[string]any{"status": db.blob(statusBlob), "meter": db.blob("aws/default/finops/meter/" + today()),
				"lines_prod": db.lines(prodAccount), "ce_a": a.front.ce.Load(), "ce_b": b.front.ce.Load()})
		}()
		waitFor(t, "a scheduled refresh after the clock step", 150*time.Second, func() bool { return db.scheduledToday() && db.lines(prodAccount) > 0 })
		after := db.blob(statusBlob)
		for _, id := range []string{payerAccount, dataAccount} {
			if calls := a.front.account(id) + b.front.account(id); calls != removedCalls[id] {
				t.Errorf("removed payer %s was collected again: %d -> %d Cost Explorer requests", id, removedCalls[id], calls)
			}
			if n := db.lines(id); n != 0 {
				t.Errorf("removed payer %s has %d cost lines again", id, n)
			}
			if tomb, _ := after["removed_payers"].(map[string]any); tomb[id] == nil {
				t.Errorf("tombstone for %s dropped: %v", id, after["removed_payers"])
			}
		}
		accounts := b.call(http.MethodGet, "/api/admin/aws/finops/accounts?period=2026-10", "")
		for _, id := range []string{payerAccount, dataAccount} {
			if bytes.Contains(accounts.body, []byte(`"`+id+`"`)) && !bytes.Contains(accounts.body, []byte("cost_only")) {
				t.Logf("accounts view still names %s: %.300s", id, accounts.body)
			}
		}
		out.put("clock_step_back", map[string]any{"removed_payers_before": removed, "removed_payers_after": after["removed_payers"],
			"shifted_fields": shifted, "removed_payer_ce_requests": removedCalls, "decision_rows_end": db.decisionRows()})
	})
}

// shiftTimes moves every RFC 3339 timestamp in the document forward by d and
// reports how many it moved.
func shiftTimes(value any, d time.Duration) int {
	n := 0
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if text, ok := item.(string); ok {
				if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
					v[key] = at.Add(d).Format(time.RFC3339Nano)
					n++
					continue
				}
			}
			n += shiftTimes(item, d)
		}
	case []any:
		for i, item := range v {
			if text, ok := item.(string); ok {
				if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
					v[i] = at.Add(d).Format(time.RFC3339Nano)
					n++
					continue
				}
			}
			n += shiftTimes(item, d)
		}
	}
	return n
}

// TestAWSTwoReplicasHoldTheDailyPaidCap boots both replicas at once against an
// Organization too large for one day's budget (60 linked accounts in pages of
// 50 groups) with the default cap of 20 paid requests a day, then spends from
// both: the boot ticks race for the scheduled refresh, a manual refresh and
// drill-downs follow on each replica, and both keep ticking. It runs in its own
// schema, so its day's meter starts at zero.
func TestAWSTwoReplicasHoldTheDailyPaidCap(t *testing.T) {
	dsn, bin, root := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("AWS_REPLICAS_SERVER_BIN"), os.Getenv("AWS_REPLICAS_DIR")
	if dsn == "" || bin == "" || root == "" {
		t.Skip("needs TEST_POSTGRES_DSN, AWS_REPLICAS_SERVER_BIN and AWS_REPLICAS_DIR")
	}
	root = filepath.Join(root, "paid-cap")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "qa_paid_cap_" + strconv.FormatInt(time.Now().Unix(), 10)
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
	defer pool.Close()
	db := &database{t: t, db: pool}
	out := &results{path: filepath.Join(root, "paid-cap-verdict.json"), data: map[string]any{}}

	fake, err := fakeaws.NewServer(fakeaws.ScenarioCostCapReached)
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	w := &world{fake: fake, scenario: fakeaws.ScenarioCostCapReached}
	a := newReplica(t, root, "replica-a", newFront(w))
	b := newReplica(t, root, "replica-b", newFront(w))
	defer func() { a.stop(); b.stop(); a.front.server.Close(); b.front.server.Close() }()

	a.start(bin, scoped, connectors{costData: false, paidCap: 20})
	a.connect(payerAccount)
	a.stop()
	org := connectors{costData: true, costSource: payerAccount, paidCap: 20}
	// Both boot ticks race for the day's scheduled refresh.
	var wg sync.WaitGroup
	for _, r := range []*replica{a, b} {
		wg.Add(1)
		go func(r *replica) { defer wg.Done(); r.start(bin, scoped, org) }(r)
	}
	wg.Wait()
	waitFor(t, "the day's first refresh to spend", 120*time.Second, func() bool { return db.meter() > 0 })
	waitFor(t, "the refresh to settle", 120*time.Second, func() bool { return db.lockHolders() == 0 })
	afterBoot := map[string]int64{"replica_a_paid": a.front.paid.Load(), "replica_b_paid": b.front.paid.Load(), "paid": fake.PaidCostRequests(), "meter": int64(db.meter())}

	var calls []map[string]any
	for i, r := range []*replica{a, b, a, b} {
		got := r.call(http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true}`)
		calls = append(calls, map[string]any{"replica": r.name, "call": "refresh", "status": got.status, "body": fmt.Sprintf("%.200s", got.body)})
		account := strconv.FormatInt(100000000001+int64(i), 10)
		got = r.call(http.MethodPost, "/api/admin/aws/finops/drilldown", `{"account":"`+account+`","service":"Amazon Elastic Compute Cloud - Compute"}`)
		calls = append(calls, map[string]any{"replica": r.name, "call": "drilldown " + account, "status": got.status, "body": fmt.Sprintf("%.200s", got.body)})
	}
	// Two more ticks on each replica.
	time.Sleep(125 * time.Second)
	paid, meter := fake.PaidCostRequests(), db.meter()
	if paid > 20 || meter > 20 {
		t.Errorf("paid requests over the daily cap across two replicas: fixture %d, meter %d", paid, meter)
	}
	if int64(meter) != paid {
		t.Errorf("meter %d does not match the %d paid requests the fixture answered", meter, paid)
	}
	if afterBoot["replica_a_paid"] > 0 && afterBoot["replica_b_paid"] > 0 {
		t.Errorf("both replicas sent Cost Explorer requests during the boot race: %v", afterBoot)
	}
	status := db.blob(statusBlob)
	out.put("paid_cap", map[string]any{"after_boot": afterBoot, "calls": calls, "paid_fixture": paid, "meter": meter,
		"replica_a_paid": a.front.paid.Load(), "replica_b_paid": b.front.paid.Load(),
		"scheduled_on": status["scheduled_on"], "manual_on": status["manual_on"], "skipped_calls": status["skipped_calls"]})
}
