package controllers

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	finopstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/finops"
	"github.com/VersusControl/versus-incident/pkg/aws/cost"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fixtures/finopsfix"
	"github.com/VersusControl/versus-incident/tests/fixtures/storagefix"

	"github.com/gofiber/fiber/v2"
)

type storySource struct {
	status    finops.SourceStatus
	lines     []finops.CostLine
	resources []finops.CostLine
	savings   finops.SavingsInputs
}

func (s *storySource) Status(context.Context) (finops.SourceStatus, error) { return s.status, nil }
func (s *storySource) Lines(_ context.Context, r finops.DateRange) ([]finops.CostLine, error) {
	return within(s.lines, r), nil
}
func (s *storySource) ResourceLines(_ context.Context, r finops.DateRange) ([]finops.CostLine, error) {
	return within(s.resources, r), nil
}
func (s *storySource) SavingsInputs(context.Context) (finops.SavingsInputs, error) {
	return s.savings, nil
}

func within(ls []finops.CostLine, r finops.DateRange) []finops.CostLine {
	var out []finops.CostLine
	for _, l := range ls {
		if r.Contains(l.Day) {
			out = append(out, l)
		}
	}
	return out
}

// loadStory expands the reference cost story shared with the FinOps core
// tests into a cost source and config.
func loadStory(t *testing.T) (*storySource, finops.Config) {
	t.Helper()
	raw, err := json.Marshal(finopsfix.NATStory())
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		SeriesRange finops.DateRange `json:"series_range"`
		Series      []struct {
			Line  finops.CostLine `json:"line"`
			Steps []struct {
				From     finops.Date   `json:"from"`
				Cost     finops.Micros `json:"cost_micros"`
				Quantity int64         `json:"quantity_micros"`
			} `json:"steps"`
		} `json:"series"`
		Lines         []finops.CostLine    `json:"lines"`
		ResourceRange finops.DateRange     `json:"resource_range"`
		Resources     []finops.CostLine    `json:"resources"`
		Status        finops.SourceStatus  `json:"status"`
		Savings       finops.SavingsInputs `json:"savings"`
		Config        struct {
			Budgets       []finops.Budget   `json:"budgets"`
			AccountOwners map[string]string `json:"account_owners"`
			Now           time.Time         `json:"now"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	src := &storySource{status: d.Status, savings: d.Savings}
	d.SeriesRange.Each(func(day finops.Date) {
		for _, s := range d.Series {
			found := false
			var cost finops.Micros
			var qty int64
			for _, st := range s.Steps {
				if !day.Before(st.From) {
					found, cost, qty = true, st.Cost, st.Quantity
				}
			}
			if found {
				l := s.Line
				l.Day, l.NetAmortizedMicros, l.QuantityMicros = day, cost, qty
				src.lines = append(src.lines, l)
			}
		}
	})
	src.lines = append(src.lines, d.Lines...)
	d.ResourceRange.Each(func(day finops.Date) {
		for _, r := range d.Resources {
			r.Day = day
			src.resources = append(src.resources, r)
		}
	})
	now := d.Config.Now
	return src, finops.Config{Budgets: d.Config.Budgets, AccountOwners: d.Config.AccountOwners, ProviderName: "AWS",
		UIPath: "/aws/finops", Now: func() time.Time { return now }}
}

// fakeFinOpsBackend serves the story; the paid operations only count.
type fakeFinOpsBackend struct {
	mu        sync.Mutex
	src       finops.CostSource
	store     finops.Store
	cfg       finops.Config
	refreshes int
	drills    int
	drillErr  error
}

func (b *fakeFinOpsBackend) Service() *finops.Service {
	return finops.NewService(b.src, b.store, b.cfg)
}
func (b *fakeFinOpsBackend) Estimate(context.Context, cost.Kind) (finops.Plan, error) {
	return finops.Plan{Estimate: 2, Remaining: 13}, nil
}
func (b *fakeFinOpsBackend) RefreshNow(context.Context) (cost.RefreshResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshes++
	if b.refreshes > 1 {
		return cost.RefreshResult{}, cost.ErrManualUsed
	}
	return cost.RefreshResult{Committed: []string{"111111111111|manual"}}, nil
}
func (b *fakeFinOpsBackend) DrillDown(context.Context, string, string) error {
	b.drills++
	if b.drillErr != nil {
		return b.drillErr
	}
	return &cost.Error{Code: cost.CodeDenied}
}

// countingStore observes what a page render asks the decision store for, so
// a test can tell a read of the stored increases apart from a fresh
// detection (which only the sync path runs, and which persists).
type countingStore struct {
	finops.Store
	mu    sync.Mutex
	lists int
	puts  int
}

func (c *countingStore) ListIncreases(ctx context.Context) ([]finops.IncreaseRecord, error) {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.Store.ListIncreases(ctx)
}

func (c *countingStore) PutIncrease(ctx context.Context, r finops.IncreaseRecord) error {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return c.Store.PutIncrease(ctx, r)
}

func (c *countingStore) counts() (lists, puts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists, c.puts
}

func (c *countingStore) reset() {
	c.mu.Lock()
	c.lists, c.puts = 0, 0
	c.mu.Unlock()
}

// finopsBackend is the story after one sync run, the way a deployment's
// refresh cadence leaves it: detection has been persisted, so the pages read
// stored increases instead of detecting them.
func finopsBackend(t *testing.T) *fakeFinOpsBackend {
	t.Helper()
	src, cfg := loadStory(t)
	st := &countingStore{Store: cost.NewStore(storagefix.Provider(t), tenancy.DefaultOrgScope())}
	b := &fakeFinOpsBackend{src: src, store: st, cfg: cfg}
	if _, err := b.Service().SyncIncreases(context.Background()); err != nil {
		t.Fatalf("sync increases: %v", err)
	}
	st.reset()
	return b
}

func finopsApp(t *testing.T, backend FinOpsBackend, permissions map[string]bool, withSettings bool) (*fiber.App, *[]middleware.AdminAuditEvent) {
	t.Helper()
	var mu sync.Mutex
	events := make([]middleware.AdminAuditEvent, 0)
	middleware.SetAdminAuditHook(func(_ *fiber.Ctx, e middleware.AdminAuditEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		for p, allowed := range permissions {
			middleware.SetRequestPermission(ctx, p, allowed)
		}
		return ctx.Next()
	})
	if withSettings {
		NewAWSSettingsController(awsTestService(t, "new-account-connect", true)).Register(app.Group("/api"))
	}
	NewAWSFinOpsController(backend).Register(app.Group("/api"))
	return app, &events
}

var (
	finopsViewer = map[string]bool{string(core.PermissionInfrastructureView): true}
	finopsAdmin  = map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureSettingsWrite): true}
)

func TestFinOpsRoutesNeedInfrastructureView(t *testing.T) {
	app, _ := finopsApp(t, finopsBackend(t), map[string]bool{string(core.PermissionInfrastructureView): false}, false)
	res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/summary", "")
	if res.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status %d", res.StatusCode)
	}
	loadGatewayConfig(t, "finops-gateway-secret")
	bare := fiber.New()
	NewAWSFinOpsController(finopsBackend(t)).Register(bare.Group("/api"))
	if res, _ := awsTestRequest(t, bare, http.MethodGet, "/api/admin/aws/finops/summary", ""); res.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("gateway guard: %d", res.StatusCode)
	}
}

// TestFinOpsSummaryOverflowAnswersClosedCode proves an unrepresentable total
// is a 422 with a stable code, never a 500 and never a wrapped number.
func TestFinOpsSummaryOverflowAnswersClosedCode(t *testing.T) {
	backend := finopsBackend(t)
	src := backend.src.(*storySource)
	day := finops.MustDate("2026-10-05")
	src.lines = append(src.lines,
		finops.CostLine{Day: day, Account: "111111111111", Service: "compute.ec2", UsageType: "max-a", RecordType: finops.RecordUsage, NetAmortizedMicros: math.MaxInt64},
		finops.CostLine{Day: day, Account: "111111111111", Service: "compute.ec2", UsageType: "max-b", RecordType: finops.RecordUsage, NetAmortizedMicros: math.MaxInt64},
	)
	app, _ := finopsApp(t, backend, finopsViewer, false)
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/summary?period=2026-10", "")
	if res.StatusCode != fiber.StatusUnprocessableEntity {
		t.Fatalf("overflow summary = %d %s, want 422", res.StatusCode, body)
	}
	if string(body) != `{"error":"amount_overflow"}` {
		t.Fatalf("overflow summary body = %s", body)
	}
}

// TestFinOpsStorageOverflowIsMappedWithoutLeaking proves the storage store's
// overflow sentinel takes the same closed answer and its text is not
// rendered.
func TestFinOpsStorageOverflowIsMappedWithoutLeaking(t *testing.T) {
	app := fiber.New()
	app.Get("/boom", func(ctx *fiber.Ctx) error {
		return writeFinOps(ctx, nil, fmt.Errorf("stored sum: %w", storage.ErrAmountOverflow))
	})
	res, body := awsTestRequest(t, app, http.MethodGet, "/boom", "")
	if res.StatusCode != fiber.StatusUnprocessableEntity || string(body) != `{"error":"amount_overflow"}` {
		t.Fatalf("storage overflow = %d %s, want 422 amount_overflow", res.StatusCode, body)
	}
}

func TestFinOpsReadRoutesServeTheViewContract(t *testing.T) {
	backend := finopsBackend(t)
	app, events := finopsApp(t, backend, finopsViewer, false)
	for _, path := range []string{"summary", "bridge", "trend", "breakdown?group_by=account", "increases?status=all", "savings",
		"accounts", "budgets", "reports", "report?period=2026-10"} {
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/"+path+sep(path)+"period=2026-10", "")
		if res.StatusCode != fiber.StatusOK {
			t.Fatalf("%s: %d %s", path, res.StatusCode, body)
		}
		var v map[string]any
		if err := json.Unmarshal(body, &v); err != nil || v["enabled"] != true {
			t.Fatalf("%s: %s", path, body)
		}
	}
	var list finops.IncreasesView
	_, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/increases?status=all&period=2026-10", "")
	if err := json.Unmarshal(body, &list); err != nil || len(list.Items) == 0 {
		t.Fatalf("increases %s", body)
	}
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/increases/"+list.Items[0].ID+"?period=2026-10", "")
	if res.StatusCode != fiber.StatusOK || !strings.Contains(string(body), `"cause_tree"`) {
		t.Fatalf("increase detail %d %s", res.StatusCode, body)
	}
	if res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/increases/nope", ""); res.StatusCode != fiber.StatusNotFound {
		t.Fatalf("unknown increase %d", res.StatusCode)
	}
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/summary?period=not-a-month", "")
	if res.StatusCode != fiber.StatusBadRequest || !strings.Contains(string(body), `"field":"period"`) || strings.Contains(string(body), "not-a-month") {
		t.Fatalf("bad query %d %s", res.StatusCode, body)
	}
	viewed := 0
	for _, e := range *events {
		if e.Action == "aws.finops.report.viewed" {
			viewed++
		}
	}
	if viewed != 1 {
		t.Fatalf("the report read is audited once: %+v", *events)
	}
}

func sep(path string) string {
	if strings.Contains(path, "?") {
		return "&"
	}
	return "?"
}

func TestFinOpsDisabledAnswersEnabledFalse(t *testing.T) {
	app, _ := finopsApp(t, nil, finopsViewer, false)
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/summary", "")
	if res.StatusCode != fiber.StatusOK || !strings.Contains(string(body), `"enabled":false`) || !strings.Contains(string(body), cost.ReasonCostDataOff) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	backend := finopsBackend(t)
	backend.src.(*storySource).status = finops.SourceStatus{Enabled: false, DisabledReason: cost.ReasonCostSourceNotLinked}
	app, _ = finopsApp(t, backend, finopsViewer, false)
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/accounts", "")
	if res.StatusCode != fiber.StatusOK || !strings.Contains(string(body), cost.ReasonCostSourceNotLinked) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

// TestFinOpsRenderReadsStoredIncreases proves the page contract after the
// move to stored detection: a render reads the decision store, changes
// nothing, and a second render answers the same synced records. Detection
// itself runs only on the sync path, so a render can never persist one.
func TestFinOpsRenderReadsStoredIncreases(t *testing.T) {
	backend := finopsBackend(t)
	app, _ := finopsApp(t, backend, finopsViewer, false)
	st := backend.store.(*countingStore)
	stored, err := st.Store.ListIncreases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 {
		t.Fatal("the synced story must have stored increases")
	}
	want := map[string]finops.Micros{}
	for _, r := range stored {
		want[r.ID] = r.MonthlyImpact
	}
	render := func() finops.IncreasesView {
		t.Helper()
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/increases?status=all&period=2026-10", "")
		if res.StatusCode != fiber.StatusOK {
			t.Fatalf("increases: %d %s", res.StatusCode, body)
		}
		var v finops.IncreasesView
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := render()
	if len(first.Items) != len(want) {
		t.Fatalf("render listed %d increases, the store holds %d", len(first.Items), len(want))
	}
	for _, it := range first.Items {
		monthly, ok := want[it.ID]
		if !ok || monthly != it.Monthly {
			t.Fatalf("increase %s: rendered %d, stored %d (known %v)", it.ID, it.Monthly, monthly, ok)
		}
	}
	lists, puts := st.counts()
	if lists == 0 || puts != 0 {
		t.Fatalf("the render read the store %d times and wrote %d, want a read and no write", lists, puts)
	}
	second := render()
	lists, puts = st.counts()
	if puts != 0 {
		t.Fatalf("a second render persisted %d increases, so detection ran on the page path", puts)
	}
	if lists < 2 {
		t.Fatalf("the second render did not read the store (%d reads)", lists)
	}
	for i := range first.Items {
		if second.Items[i].ID != first.Items[i].ID || second.Items[i].Monthly != first.Items[i].Monthly {
			t.Fatalf("render %d changed: %+v then %+v", i, first.Items[i], second.Items[i])
		}
	}
}

func TestFinOpsWritesNeedSettingsWriteAndAreAuditedOnce(t *testing.T) {
	backend := finopsBackend(t)
	app, events := finopsApp(t, backend, finopsViewer, true)
	res, _ := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true}`)
	if res.StatusCode != fiber.StatusForbidden || backend.refreshes != 0 {
		t.Fatalf("viewer refresh %d", res.StatusCode)
	}
	if len(*events) != 1 || (*events)[0].Action != "rbac.denied" {
		t.Fatalf("refusal audited once: %+v", *events)
	}
	app, events = finopsApp(t, backend, finopsAdmin, true)
	var list finops.IncreasesView
	_, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/increases?period=2026-10", "")
	_ = json.Unmarshal(body, &list)
	id := list.Items[0].ID
	res, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/increases/"+id+"/status", `{"status":"expected","until":"2026-12-01"}`)
	if res.StatusCode != fiber.StatusBadRequest || !strings.Contains(string(body), "reason_required") {
		t.Fatalf("reason required: %d %s", res.StatusCode, body)
	}
	res, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/increases/"+id+"/status", `{"status":"expected","reason":"migration","until":"2026-12-01"}`)
	if res.StatusCode != fiber.StatusOK || !strings.Contains(string(body), `"status":"expected"`) {
		t.Fatalf("mark expected: %d %s", res.StatusCode, body)
	}
	res, _ = awsTestRequest(t, app, http.MethodPut, "/api/admin/aws/finops/budgets", `{"budgets":[{"name":"Checkout","scope":"team","scope_value":"checkout","monthly_micros":9000000000}]}`)
	if res.StatusCode != fiber.StatusOK {
		t.Fatalf("budgets %d", res.StatusCode)
	}
	res, _ = awsTestRequest(t, app, http.MethodPut, "/api/admin/aws/finops/budgets", `{"budgets":[{"name":"x","scope":"org","monthly_micros":1}]}`)
	if res.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("per-org budget %d", res.StatusCode)
	}
	if res, _ := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/savings/nope/dismiss", `{"reason":"done"}`); res.StatusCode != fiber.StatusNotFound {
		t.Fatalf("dismiss unknown %d", res.StatusCode)
	}
	res, first := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/reports", `{"month":"2026-10"}`)
	if res.StatusCode != fiber.StatusCreated || !strings.Contains(string(first), `"frozen":{"at"`) || !strings.Contains(string(first), `"not_final":false`) {
		t.Fatalf("freeze %d %s", res.StatusCode, first)
	}
	res, again := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/reports", `{"month":"2026-10"}`)
	if res.StatusCode != fiber.StatusOK || string(again) != string(first) {
		t.Fatalf("freezing a month twice returns the first frozen report: %d", res.StatusCode)
	}
	if _, got := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/report?period=2026-10", ""); string(got) != string(first) {
		t.Fatal("the report route serves the frozen model")
	}
	// Before the 5th the October bill is not final: no freeze, and the
	// report says so.
	early := finopsBackend(t)
	early.cfg.Now = func() time.Time { return time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC) }
	earlyApp, _ := finopsApp(t, early, finopsAdmin, false)
	res, body = awsTestRequest(t, earlyApp, http.MethodPost, "/api/admin/aws/finops/reports", `{"month":"2026-10"}`)
	if res.StatusCode != fiber.StatusConflict || !strings.Contains(string(body), "bill_not_closed") {
		t.Fatalf("bill not closed: %d %s", res.StatusCode, body)
	}
	_, body = awsTestRequest(t, earlyApp, http.MethodGet, "/api/admin/aws/finops/report?period=2026-10", "")
	if !strings.Contains(string(body), `"not_final":true`) || !strings.Contains(string(body), `"frozen":null`) {
		t.Fatalf("open bill report %s", body)
	}
	actions := map[string]int{}
	for _, e := range *events {
		actions[e.Action]++
		if e.Action == "aws.settings.changed" {
			t.Fatalf("FinOps writes are not recorded as settings changes: %+v", *events)
		}
	}
	if actions["aws.finops.increase.status_changed"] != 2 || actions["aws.finops.budgets.changed"] != 2 || actions["aws.finops.report.frozen"] != 1 || actions["aws.finops.report.already_frozen"] != 1 {
		t.Fatalf("each write audited once: %v", actions)
	}
}

func TestFinOpsRefreshEstimatesFirstAndRunsOnceADay(t *testing.T) {
	backend := finopsBackend(t)
	app, _ := finopsApp(t, backend, finopsAdmin, false)
	res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", "")
	if res.StatusCode != fiber.StatusOK || backend.refreshes != 0 || !strings.Contains(string(body), `"ran":false`) || !strings.Contains(string(body), `"estimate":2`) {
		t.Fatalf("estimate first: %d %s", res.StatusCode, body)
	}
	if res, _ := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true}`); res.StatusCode != fiber.StatusOK || backend.refreshes != 1 {
		t.Fatalf("refresh %d", res.StatusCode)
	}
	res, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true}`)
	if res.StatusCode != fiber.StatusTooManyRequests || !strings.Contains(string(body), "manual_refresh_used") {
		t.Fatalf("once a day: %d %s", res.StatusCode, body)
	}
	res, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/drilldown", `{"account":"111111111111","service":"EC2 - Other"}`)
	if res.StatusCode != fiber.StatusBadGateway || !strings.Contains(string(body), cost.CodeDenied) {
		t.Fatalf("drill-down error code: %d %s", res.StatusCode, body)
	}
	if res, _ := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true,"extra":1}`); res.StatusCode != fiber.StatusBadRequest {
		t.Fatal("unknown body fields are refused")
	}
}

// busyFinOpsBackend answers every paid call as another replica refreshing.
type busyFinOpsBackend struct{ *fakeFinOpsBackend }

func (b busyFinOpsBackend) RefreshNow(context.Context) (cost.RefreshResult, error) {
	return cost.RefreshResult{}, cost.ErrBusy
}

// TestFinOpsBusyAnswersRefreshInProgress: "Refresh now" and a drill-down
// that meet a refresh on another replica answer 409 refresh_in_progress
// with Retry-After 5, the same answer a busy settings write gets.
func TestFinOpsBusyAnswersRefreshInProgress(t *testing.T) {
	backend := finopsBackend(t)
	backend.drillErr = cost.ErrBusy
	app, _ := finopsApp(t, busyFinOpsBackend{backend}, finopsAdmin, false)
	for _, call := range []struct{ path, body string }{
		{"/api/admin/aws/finops/refresh", `{"confirm":true}`},
		{"/api/admin/aws/finops/drilldown", `{"account":"111111111111","service":"EC2 - Other"}`},
	} {
		res, body := awsTestRequest(t, app, http.MethodPost, call.path, call.body)
		if res.StatusCode != fiber.StatusConflict || string(body) != `{"error":"refresh_in_progress"}` || res.Header.Get(fiber.HeaderRetryAfter) != "5" {
			t.Fatalf("%s while busy = %d %s Retry-After %q, want 409 refresh_in_progress with Retry-After 5",
				call.path, res.StatusCode, body, res.Header.Get(fiber.HeaderRetryAfter))
		}
	}
}

func TestFinOpsCSVExportEscapesFormulas(t *testing.T) {
	backend := finopsBackend(t)
	src := backend.src.(*storySource)
	src.status.Accounts[0].Name = "=HYPERLINK(\"http://x\")"
	app, events := finopsApp(t, backend, finopsViewer, false)
	for _, table := range []string{"breakdown", "increases", "savings"} {
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/export.csv?period=2026-10&group_by=account&table="+table, "")
		if res.StatusCode != fiber.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/csv") {
			t.Fatalf("%s: %d", table, res.StatusCode)
		}
		rows, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
		if err != nil || len(rows) < 2 {
			t.Fatalf("%s: %v %s", table, err, body)
		}
		for _, row := range rows[1:] {
			for i, cell := range row {
				if cell != "" && strings.ContainsRune("=+@\t\r", rune(cell[0])) {
					t.Fatalf("%s column %d is a live formula: %q", table, i, cell)
				}
			}
		}
		if table == "breakdown" && !strings.Contains(string(body), `'=HYPERLINK`) {
			t.Fatalf("formula label not neutralised: %s", body)
		}
	}
	if res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/export.csv?table=secrets", ""); res.StatusCode != fiber.StatusBadRequest {
		t.Fatal("unknown table")
	}
	exported := 0
	for _, e := range *events {
		if e.Action == "aws.finops.exported" {
			exported++
		}
	}
	if exported != 4 {
		t.Fatalf("every export is audited: %+v", *events)
	}
	for in, want := range map[string]string{"=1+1": "'=1+1", "+x": "'+x", "-2": "'-2", "@SUM": "'@SUM", "\tx": "'\tx", "plain": "plain", "": ""} {
		if got := csvText(in); got != want {
			t.Errorf("csvText(%q) = %q, want %q", in, got, want)
		}
	}
}

func toolData(t *testing.T, tool core.Tool, args string) map[string]any {
	t.Helper()
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true,
		Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	res, err := tool.Invoke(ctx, json.RawMessage(args))
	if err != nil || res == nil || !res.IsAvailable() {
		t.Fatalf("%s: %+v %v", tool.Name(), res, err)
	}
	return res.Data
}

// TestControllerAndToolSameResult: the route and the tool call the same
// service method, so the numbers match.
func TestControllerAndToolSameResult(t *testing.T) {
	backend := finopsBackend(t)
	app, _ := finopsApp(t, backend, finopsViewer, false)
	tools := map[string]core.Tool{}
	for _, tl := range finopstools.New(func() finops.Reader { return backend.Service() }) {
		tools[tl.Name()] = tl
	}
	route := func(path string, v any) {
		_, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/"+path, "")
		if err := json.Unmarshal(body, v); err != nil {
			t.Fatal(err)
		}
	}
	var sum finops.SummaryView
	route("summary?period=2026-10", &sum)
	bd := toolData(t, tools["cost_breakdown"], `{"period":"2026-10"}`)
	if bd["total_micros"] != sum.Total {
		t.Fatalf("total: tool %v route %d", bd["total_micros"], sum.Total)
	}
	var bridge finops.BridgeView
	route("bridge?period=2026-10", &bridge)
	dr := toolData(t, tools["cost_drivers"], `{"period":"2026-10"}`)
	if dr["change_micros"] != bridge.Change || len(dr["drivers"].([]map[string]any)) != min(len(bridge.Rows), 8) {
		t.Fatalf("drivers differ: %v vs %d", dr["change_micros"], bridge.Change)
	}
	var incs finops.IncreasesView
	route("increases?period=2026-10&status=all", &incs)
	in := toolData(t, tools["cost_increases"], `{"period":"2026-10","status":"all"}`)
	items := in["increases"].([]map[string]any)
	if len(items) != len(incs.Items) || items[0]["id"] != incs.Items[0].ID || items[0]["monthly_impact_micros"] != incs.Items[0].Monthly {
		t.Fatalf("increases differ: %v vs %+v", items, incs.Items)
	}
	var detail finops.IncreaseDetailView
	route("increases/"+incs.Items[0].ID+"?period=2026-10", &detail)
	dt := toolData(t, tools["cost_increase_detail"], `{"period":"2026-10","id":"`+incs.Items[0].ID+`"}`)
	if dt["id"] != detail.Increase.ID || dt["monthly_impact_micros"] != detail.Increase.Monthly ||
		dt["suspect"] != detail.Increase.Suspect.State || len(dt["cause_tree"].([]map[string]any)) != min(len(detail.CauseTree), 5) ||
		dt["cause"] != detail.Increase.Cause {
		t.Fatalf("increase detail differs: %v vs %+v", dt, detail.Increase)
	}
	var budgets finops.BudgetView
	route("budgets?period=2026-10", &budgets)
	bs := toolData(t, tools["budget_status"], `{"period":"2026-10"}`)
	if len(bs["budgets"].([]map[string]any)) != len(budgets.Budgets) {
		t.Fatal("budgets differ")
	}
	var accounts finops.AccountsView
	route("accounts?period=2026-10", &accounts)
	ac := toolData(t, tools["cost_accounts"], `{"period":"2026-10"}`)
	if ac["total_micros"] != accounts.Total {
		t.Fatal("accounts differ")
	}
	var savings finops.SavingsView
	route("savings?period=2026-10", &savings)
	sv := toolData(t, tools["savings_opportunities"], `{"period":"2026-10"}`)
	if len(sv["savings"].([]map[string]any)) != min(len(savings.Items), 10) {
		t.Fatal("savings differ")
	}
}
