package controllers

// aws_finops_stack_test.go — the integration gate for AWS FinOps (A8). The
// settings service, snapshot store, cost adapter, backend, signal source and
// daily scheduler are wired the way cmd/main.go registerAWSSettingsController
// and registerAWSFinOps wire them; the live connector config is the
// process-wide config those functions read. tests/fakeaws answers every AWS
// call (STS, Cost Explorer, Budgets, Cost Optimization Hub, Compute
// Optimizer) over the product's HTTPClient seam, and new increases go through
// the production alert sink services.CreateIncidentFromCostIncrease.
//
// Differences from cmd/main.go, all test seams: the fixture HTTP client, a
// pinned clock (2026-11-09 10:00 UTC, the day the cost fixtures end) and a
// snapshot store seeded from the scenario file's inventory and change
// history (the collector world does not model the cost story's resources).
// Fixture evidence only: nothing here reaches AWS.

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	awscost "github.com/VersusControl/versus-incident/pkg/aws/cost"
	awslimit "github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
	"github.com/VersusControl/versus-incident/tests/fixtures/awsfix"
	"github.com/VersusControl/versus-incident/tests/fixtures/storagefix"

	"github.com/gofiber/fiber/v2"
)

const finopsStackDeployment = "default"

// finopsStackDay is the day the cost fixtures end; every clock is pinned to it.
var finopsStackDay = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

type stackClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *stackClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *stackClock) Set(now time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = now
}

type finopsStack struct {
	t         *testing.T
	app       *fiber.App
	fake      *fakeaws.Server
	provider  storage.Provider
	settings  *awssettings.Service
	snapshots *awssnapshot.Store
	adapter   *awscost.Adapter
	backend   *awscost.Backend
	scheduler *awscost.Scheduler
	clock     *stackClock
	client    *awsClient

	pumpAgain func() (int, error)
	label     string
	mutate    func(*awscost.Options)

	alertsMu sync.Mutex
	alerts   []map[string]any
}

func costAccountConfig(id, name string) config.AWSAccountConfig {
	return config.AWSAccountConfig{AccountID: id, DisplayName: name, Regions: []string{"us-east-1"},
		Auth: config.AWSAuthConfig{RoleARN: "arn:aws:iam::" + id + ":role/versus-reader", ExternalID: "vrs-ext-finopsstack01"}}
}

// finopsConnector is the connector config every scenario starts from: cost
// data on, team tag values allowlisted, two months of change history.
func finopsConnector(accounts ...config.AWSAccountConfig) config.AWSConnectorConfig {
	return config.AWSConnectorConfig{
		Enable:       true,
		Accounts:     accounts,
		Tags:         config.AWSTagsConfig{ValueAllowlist: []string{"team"}},
		Budgets:      config.AWSBudgetsConfig{MaxRequestsPerSecond: 100},
		OptionalData: config.AWSOptionalDataConfig{CostExplorer: true},
		Collect:      config.AWSCollectConfig{History: config.AWSCollectHistoryConfig{ChangeRetention: "1440h"}},
	}
}

func (stack *finopsStack) setConnector(connector config.AWSConnectorConfig) {
	config.GetConfig().Connectors.AWS = connector
}

func newFinOpsStack(t *testing.T, scenario string, connector config.AWSConnectorConfig) *finopsStack {
	t.Helper()
	return newFinOpsStackWith(t, scenario, connector, nil)
}

// newFinOpsStackWith lets one discriminating check change the adapter
// options after FromConfig; the verdict groups never use it.
func newFinOpsStackWith(t *testing.T, scenario string, connector config.AWSConnectorConfig, mutate func(*awscost.Options)) *finopsStack {
	t.Helper()
	middleware.SetGatewayAuthEnabled(true)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(true) })
	pinGatewaySecret(t, awsStackSecret)
	live := config.GetConfig()
	previous := live.Connectors.AWS
	live.Connectors.AWS = connector
	t.Cleanup(func() { config.GetConfig().Connectors.AWS = previous })

	stack := &finopsStack{t: t, clock: &stackClock{now: finopsStackDay}, provider: storagefix.Provider(t), label: scenario, mutate: mutate}
	stack.fake = fakeaws.New(t, scenario)
	stack.fake.SetClock(stack.clock.Now)
	client := stack.fake.HTTPClient()
	current := func() config.AWSConnectorConfig {
		if live := config.GetConfigOrNil(); live != nil {
			return live.Connectors.AWS
		}
		return connector
	}
	scope := tenancy.DefaultOrgScope()

	// registerAWSSettingsController (collectors are not started: the
	// snapshot store is seeded from the scenario instead).
	limiter := awslimit.NewManager(connector.Budgets.MaxRequestsPerSecond)
	stack.snapshots = awssnapshot.New(stack.provider, scope, awssnapshot.WithClock(stack.clock.Now), awssnapshot.WithOptions(func() awssnapshot.Options {
		c := current()
		return awssnapshot.Options{MaxGenerations: c.Collect.MaxGenerations(), MaxAge: c.Collect.MaxAge(),
			ChangeRetention: c.Collect.ChangeRetention(), AppSources: c.Applications.EffectiveSources()}
	}))
	stack.settings = awssettings.NewService(
		awssettings.NewStore(stack.provider, scope),
		awssettings.WithConfig(current),
		awssettings.WithHTTPClient(client),
		awssettings.WithBaseProvider(awsStackBase),
		awssettings.WithLimiter(limiter),
		awssettings.WithSnapshots(stack.snapshots),
	)

	// registerAWSFinOps.
	adapter, err := awscost.New(stack.provider, scope, awscost.Options{Deployment: finopsStackDeployment, Limiter: limiter,
		Enabled: func() bool { return false }, DisabledReason: awscost.ReasonCostDataOff, Now: stack.clock.Now, HTTPClient: client})
	if err != nil {
		t.Fatalf("cost adapter: %v", err)
	}
	stack.adapter = adapter
	prepare := func(ctx context.Context) error {
		conn := current()
		var targets []awscost.Target
		if conn.CostDataEnabled() {
			for _, target := range stack.settings.CollectTargets(ctx) {
				targets = append(targets, awscost.Target{AccountID: target.AccountID, Credentials: target.Credentials})
			}
		}
		opts := awscost.FromConfig(conn, targets, stack.settings.TagValueAllowlist(), finopsStackDeployment)
		opts.Limiter = limiter
		opts.Inventory = stack.snapshots
		opts.HTTPClient = client
		opts.Now = stack.clock.Now
		// Resource-level cost for the services with an open increase.
		opts.ResourceServices = func() []string { return stack.backend.OpenIncreaseServices(context.Background()) }
		if stack.mutate != nil {
			stack.mutate(&opts)
		}
		return adapter.Update(opts)
	}
	stack.backend = awscost.NewBackend(adapter, awscost.NewStore(stack.provider, scope), current)
	stack.app = fiber.New(fiber.Config{Immutable: true})
	stack.app.Use(middleware.OrgInjector())
	api := stack.app.Group("/api")
	RegisterGatewaySessionRoutes(api)
	NewAWSSettingsController(stack.settings).Register(api)
	NewAWSFinOpsController(stack.backend).Register(api)
	signals := awscost.NewSignalSource(finopsStackDeployment, awscost.ServiceFunc(stack.backend.Service), nil)
	sink := func(ctx context.Context, sig core.Signal) error {
		return services.CreateIncidentFromFinOpsSignal(ctx, scope.Write, sig)
	}
	stack.scheduler = awscost.NewScheduler(adapter,
		func() (bool, int, int, *time.Location) {
			conn := current()
			finops := conn.FinOps.Effective()
			hour, minute := finops.RefreshClock()
			return conn.CostDataEnabled(), hour, minute, finops.Location()
		},
		prepare,
		awscost.WithClock(stack.clock.Now),
		awscost.WithLogger(log.Printf),
		awscost.WithAfterRefresh(func(ctx context.Context, _ awscost.RefreshResult) {
			if _, err := awscost.PumpAlerts(ctx, signals, sink); err != nil {
				log.Printf("aws cost: increase alerts not sent; retried after the next refresh")
			}
		}),
	)
	stack.pumpAgain = func() (int, error) { return awscost.PumpAlerts(context.Background(), signals, sink) }

	// The alert path: the production emit pipeline on this stack's store,
	// observed through the public emit interceptor.
	previousStore := services.Storage()
	services.SetStorage(stack.provider)
	services.SetEmitInterceptor(func(content map[string]interface{}, _ string) services.EmitDecision {
		stack.alertsMu.Lock()
		stack.alerts = append(stack.alerts, map[string]any{"kind": content["AlertKind"], "pattern": content["PatternID"], "name": content["AlertName"], "description": content["Description"]})
		stack.alertsMu.Unlock()
		return services.EmitDecision{Action: services.EmitProceed}
	})
	t.Cleanup(func() {
		services.SetEmitInterceptor(nil)
		services.SetStorage(previousStore)
	})

	if data := fakeaws.CostScenarioData(scenario); data != nil {
		stack.seedInventory(data)
	}
	stack.client = &awsClient{t: t, app: stack.app}
	stack.client.gatewaySession(awsStackSecret)
	return stack
}

// seedInventory commits the scenario's inventory and change history into the
// snapshot store through its write API (the A1 collectors' commit path).
func (stack *finopsStack) seedInventory(scenario *awsfix.CostScenario) {
	t := stack.t
	raw, err := json.Marshal(scenario)
	if err != nil {
		t.Fatalf("scenario encode: %v", err)
	}
	var data struct {
		Inventory      []model.Resource   `json:"inventory"`
		Changes        []model.Change     `json:"changes"`
		ChangeCoverage []changes.Interval `json:"change_coverage"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("scenario decode: %v", err)
	}
	units := map[model.Unit][]model.Resource{}
	for _, resource := range data.Inventory {
		unit := model.Unit{Account: resource.Account, Region: resource.Region, Service: resource.Service}
		units[unit] = append(units[unit], resource)
	}
	for unit, resources := range units {
		if _, err := stack.snapshots.Commit(unit, awssnapshot.Candidate{Resources: resources}); err != nil {
			t.Fatalf("seed %v: %v", unit, err)
		}
	}
	byRegion := map[[2]string][]model.Change{}
	for _, change := range data.Changes {
		key := [2]string{change.Account, change.Region}
		byRegion[key] = append(byRegion[key], change)
	}
	for key, found := range byRegion {
		if err := stack.snapshots.AppendChanges(key[0], key[1], found, data.ChangeCoverage, ""); err != nil {
			t.Fatalf("seed changes: %v", err)
		}
	}
}

func (stack *finopsStack) refresh() bool {
	stack.t.Helper()
	return stack.scheduler.RunDue(context.Background())
}

func (stack *finopsStack) get(path string) (int, map[string]any, []byte) {
	stack.t.Helper()
	response, body := stack.client.do(http.MethodGet, path, "")
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	saveFinOpsBody(stack.t, stack.label+"_"+stack.clock.Now().Format("0102")+"_"+path, body)
	return response.StatusCode, decoded, body
}

func (stack *finopsStack) post(path, payload string) (int, []byte) {
	stack.t.Helper()
	response, body := stack.client.do(http.MethodPost, path, payload)
	return response.StatusCode, body
}

func (stack *finopsStack) alertKindCount(kind string) int {
	stack.alertsMu.Lock()
	defer stack.alertsMu.Unlock()
	count := 0
	for _, alert := range stack.alerts {
		if alert["kind"] == kind {
			count++
		}
	}
	return count
}

func (stack *finopsStack) alertCount() int {
	stack.alertsMu.Lock()
	defer stack.alertsMu.Unlock()
	return len(stack.alerts)
}

// saveFinOpsBody keeps each answer under the run's results for review.
func saveFinOpsBody(t *testing.T, path string, body []byte) {
	dir := os.Getenv("HARNESS_RESULTS_DIR")
	if dir == "" {
		return
	}
	name := strings.NewReplacer("/api/admin/aws/finops/", "", "/api/admin/aws/", "", "/", "_", "?", "__", "&", "_", "=", "-", ",", "+").Replace(path)
	folder := filepath.Join(dir, "finops", strings.ReplaceAll(t.Name(), "/", "_"))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(folder, name+".json"), body, 0o600)
}

// ---- JSON helpers ----

func jsonPath(value any, keys ...string) any {
	for _, key := range keys {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

func jsonList(value any, keys ...string) []map[string]any {
	items, _ := jsonPath(value, keys...).([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

func jsonInt(value any, keys ...string) int64 {
	number, _ := jsonPath(value, keys...).(float64)
	return int64(number)
}

func jsonString(value any, keys ...string) string {
	text, _ := jsonPath(value, keys...).(string)
	return text
}

var (
	finopsObservationsMu sync.Mutex
	finopsObservations   = map[string]any{}
)

func observeFinOps(group string, value any) {
	finopsObservationsMu.Lock()
	defer finopsObservationsMu.Unlock()
	finopsObservations[group] = value
}

// ---- suite ----

// TestAWSFinOpsStackE2E is the A8 integration verdict; it writes
// results/aws-finops-verdict.json when the harness supplies
// HARNESS_RESULTS_DIR.
func TestAWSFinOpsStackE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("integration suite needs the fakeaws HTTP stack")
	}
	groups := []struct {
		name string
		run  func(*testing.T)
	}{
		{"cost_nat_spike_story", natSpikeFinOpsGroup},
		{"cost_nat_spike_two_nats_tie", twoNATsFinOpsGroup},
		{"budget_alert_once_per_level", budgetAlertFinOpsGroup},
		{"cost_new_untagged_db", newUntaggedDBFinOpsGroup},
		{"cost_denied_account", deniedAccountFinOpsGroup},
		{"cost_throttled", throttledFinOpsGroup},
		{"cost_cap_reached", capReachedFinOpsGroup},
		{"cost_credits_and_one_off_fee", creditsFeeFinOpsGroup},
		{"ce_unavailable_single_source", unavailableFinOpsGroup},
		{"organization_payer", organizationFinOpsGroup},
		{"drilldown_guards", drillGuardFinOpsGroup},
		{"export_csv_injection", exportFinOpsGroup},
		{"increase_alert_once", alertOnceFinOpsGroup},
		{"disabled_answers_200", disabledFinOpsGroup},
	}
	results := make([]map[string]string, 0, len(groups))
	for _, group := range groups {
		verdict := "fail"
		if t.Run(group.name, group.run) {
			verdict = "pass"
		}
		results = append(results, map[string]string{"group": group.name, "verdict": verdict})
	}
	dir := os.Getenv("HARNESS_RESULTS_DIR")
	if dir == "" {
		return
	}
	finopsObservationsMu.Lock()
	encoded, _ := json.MarshalIndent(map[string]any{
		"suite": "aws-finops", "task": os.Getenv("HARNESS_TASK"), "run_id": os.Getenv("HARNESS_RUN_ID"),
		"backend": "tests/fakeaws cost scenarios via the product HTTPClient seam; wiring mirrors cmd/main.go (fixture, not live AWS)",
		"groups":  results, "observations": finopsObservations,
	}, "", "  ")
	finopsObservationsMu.Unlock()
	_ = os.WriteFile(filepath.Join(dir, "aws-finops-verdict.json"), encoded, 0o600)
}

func natSpikeStack(t *testing.T) *finopsStack {
	stack := newFinOpsStack(t, fakeaws.ScenarioCostNatSpike, finopsConnector(costAccountConfig("111111111111", "Production")))
	if !stack.refresh() {
		t.Fatal("the scheduled refresh did not run")
	}
	return stack
}

// bridgeSums checks base + rows + other == total and total - base == change
// when the period has a base; without one the bridge must say why.
func bridgeSums(t *testing.T, bridge map[string]any) {
	t.Helper()
	if bridge["available"] != true {
		if jsonString(bridge, "compare", "reason") == "" {
			t.Errorf("bridge unavailable without a named reason: %v", bridge["compare"])
		}
		return
	}
	base, total, change := jsonInt(bridge, "base_micros"), jsonInt(bridge, "total_micros"), jsonInt(bridge, "change_micros")
	rows := jsonInt(bridge, "other", "amount_micros")
	for _, row := range jsonList(bridge, "rows") {
		rows += jsonInt(row, "amount_micros")
	}
	if base+rows != total || total-base != change {
		t.Errorf("bridge does not sum: base %d + rows/other %d != total %d (change %d)", base, rows, total, change)
	}
}

func natIncrease(t *testing.T, stack *finopsStack) map[string]any {
	t.Helper()
	_, increases, _ := stack.get("/api/admin/aws/finops/increases?period=2026-10&status=all")
	for _, item := range jsonList(increases, "items") {
		if jsonString(item, "service") == "network.nat" {
			return item
		}
	}
	t.Fatalf("no NAT increase: %v", jsonList(increases, "items"))
	return nil
}

func likelyCheckoutRelease(nat map[string]any) bool {
	suspect := jsonPath(nat, "suspect_change")
	return jsonString(suspect, "confidence_word") == "Likely" && strings.Contains(jsonString(suspect, "summary"), "checkout-api") &&
		strings.Contains(jsonString(suspect, "summary")+jsonString(nat, "sentence"), ":42 (was :41)")
}

func natSpikeFinOpsGroup(t *testing.T) {
	stack := natSpikeStack(t)
	status, summary, _ := stack.get("/api/admin/aws/finops/summary?period=2026-10")
	if status != fiber.StatusOK || summary["enabled"] != true {
		t.Fatalf("summary = %d %v", status, summary)
	}
	// October is the first month of data: the October bridge names the
	// missing base instead of inventing one; the month-to-date bridge sums.
	_, october, _ := stack.get("/api/admin/aws/finops/bridge?period=2026-10")
	if october["available"] != false || jsonString(october, "compare", "reason") == "" {
		t.Errorf("October bridge without a base period is not named: available=%v compare=%v", october["available"], october["compare"])
	}
	_, bridge, _ := stack.get("/api/admin/aws/finops/bridge?period=mtd")
	if bridge["available"] != true {
		t.Errorf("month-to-date bridge unavailable: %v", bridge["compare"])
	}
	bridgeSums(t, bridge)
	nat := natIncrease(t, stack)
	if owner := jsonString(nat, "owner", "name"); owner != "checkout" {
		t.Errorf("increase owner = %q, want checkout", owner)
	}
	if !likelyCheckoutRelease(nat) {
		t.Errorf("suspect = %v; resource_data=%v; sentence = %q", jsonPath(nat, "suspect_change"), jsonPath(summary, "sync", "resource_data"), jsonString(nat, "sentence"))
	}
	status, detail, _ := stack.get("/api/admin/aws/finops/increases/" + jsonString(nat, "id") + "?period=2026-10")
	if status != fiber.StatusOK {
		t.Errorf("increase detail = %d", status)
	}
	_, savings, _ := stack.get("/api/admin/aws/finops/savings?period=2026-10")
	var endpoints, commitment bool
	var inTotal int64
	for _, item := range jsonList(savings, "items") {
		kind := jsonString(item, "kind")
		if kind == "commitment" {
			commitment = true
			continue
		}
		inTotal += jsonInt(item, "monthly_micros")
		if kind == "network_path" && strings.Contains(strings.ToLower(jsonString(item, "title")), "endpoint") {
			endpoints = true
		}
	}
	if !endpoints {
		t.Errorf("no VPC endpoint saving: %v", jsonList(savings, "items"))
	}
	if !commitment {
		t.Errorf("no Savings Plan (commitment) item")
	}
	if total := jsonInt(savings, "total", "monthly_micros"); total != inTotal {
		t.Errorf("savings total %d != non-commitment items %d (commitment must stay outside)", total, inTotal)
	}
	paid := stack.fake.PaidCostRequests()
	if used := jsonInt(summary, "meter", "used"); used != paid || paid > 20 {
		t.Errorf("meter used %d, fixture paid requests %d (cap 20)", used, paid)
	}

	// The next daily refresh reads resource-level cost for the service with
	// the open increase, and the suspect stays the checkout-api release.
	firstDayCalls := stack.fake.CostCalls()
	stack.clock.Set(finopsStackDay.Add(24 * time.Hour))
	stack.refresh()
	_, nextDay, _ := stack.get("/api/admin/aws/finops/summary?period=2026-10")
	resourceCalls := stack.fake.CostCalls()["ce:GetCostAndUsageWithResources"]
	if firstDayCalls["ce:GetCostAndUsageWithResources"] != 0 || resourceCalls == 0 || jsonPath(nextDay, "sync", "resource_data") != true {
		t.Errorf("resource-level cost for the open increase: day 1 %d calls, day 2 %d calls, resource_data=%v", firstDayCalls["ce:GetCostAndUsageWithResources"], resourceCalls, jsonPath(nextDay, "sync", "resource_data"))
	}
	if again := natIncrease(t, stack); !likelyCheckoutRelease(again) {
		t.Errorf("day 2 suspect = %v", jsonPath(again, "suspect_change"))
	}
	observeFinOps("cost_nat_spike_story", map[string]any{
		"answer": summary["answer"], "october_bridge": october["compare"], "mtd_bridge_rows": len(jsonList(bridge, "rows")),
		"increase": jsonString(nat, "sentence"), "suspect": jsonPath(nat, "suspect_change"), "owner": jsonPath(nat, "owner"),
		"resource_data": jsonPath(summary, "sync", "resource_data"), "detail_signal": jsonPath(detail, "usage_signal"),
		"savings_total": jsonInt(savings, "total", "monthly_micros"), "paid_requests": paid, "cost_calls": stack.fake.CostCalls(),
		"day2_resource_data": jsonPath(nextDay, "sync", "resource_data"), "day2_meter": jsonPath(nextDay, "meter"),
	})
}

func deniedAccountFinOpsGroup(t *testing.T) {
	stack := newFinOpsStack(t, fakeaws.ScenarioCostDeniedAccount, finopsConnector(
		costAccountConfig("111111111111", "Production"), costAccountConfig("222222222222", "Data"), costAccountConfig(fakeaws.CostDeniedAccount, "Sandbox")))
	stack.refresh()
	_, summary, _ := stack.get("/api/admin/aws/finops/summary?period=mtd")
	sync := jsonPath(summary, "sync")
	missing := jsonList(sync, "missing_accounts")
	if jsonInt(sync, "accounts_readable") != 2 || jsonInt(sync, "accounts_total") != 3 || len(missing) != 1 ||
		jsonString(missing[0], "id") != fakeaws.CostDeniedAccount || !strings.Contains(jsonString(missing[0], "reason"), "denied") || jsonString(missing[0], "name") != "Sandbox" {
		t.Errorf("sync = %v", sync)
	}
	notes, _ := json.Marshal(jsonPath(sync, "notes"))
	if !bytes.Contains(notes, []byte("2 of 3 accounts")) {
		t.Errorf("the gap is not named in the notes: %s", notes)
	}
	if jsonInt(summary, "total_micros") <= 0 {
		t.Errorf("readable accounts show no cost: %v", summary["total_micros"])
	}
	if paid := stack.fake.PaidCostRequests(); paid > 20 {
		t.Errorf("paid requests %d over the cap", paid)
	}
	observeFinOps("cost_denied_account", map[string]any{"answer": summary["answer"], "sync": sync, "paid": stack.fake.PaidCostRequests()})
}

// waitLimiterBackoff lets the shared limiter's throttle pause (real time, at
// most 30 s) pass before the next pinned-clock day.
func waitLimiterBackoff() { time.Sleep(32 * time.Second) }

func throttledFinOpsGroup(t *testing.T) {
	// First read throttled: nothing to show yet, and it says why.
	stack := newFinOpsStack(t, fakeaws.ScenarioCostThrottled, finopsConnector(costAccountConfig("111111111111", "Production")))
	stack.refresh()
	_, first, _ := stack.get("/api/admin/aws/finops/summary?period=mtd")
	if first["enabled"] != false || jsonString(first, "reason") != "throttled" || jsonString(first, "cost_source", "account_id") != "111111111111" {
		t.Errorf("throttled first read: enabled=%v reason=%q source=%v", first["enabled"], jsonString(first, "reason"), first["cost_source"])
	}
	// Last good stays: a clean day, then a throttled day.
	stack.fake.SetCostThrottled(false)
	waitLimiterBackoff()
	stack.clock.Set(finopsStackDay.Add(24 * time.Hour))
	stack.refresh()
	_, good, _ := stack.get("/api/admin/aws/finops/summary?period=mtd")
	if good["enabled"] != true || jsonInt(good, "total_micros") <= 0 {
		t.Fatalf("recovery: enabled=%v reason=%q total=%v", good["enabled"], jsonString(good, "reason"), good["total_micros"])
	}
	stack.fake.SetCostThrottled(true)
	stack.clock.Set(finopsStackDay.Add(48 * time.Hour))
	stack.refresh()
	_, kept, _ := stack.get("/api/admin/aws/finops/summary?period=mtd")
	syncText, _ := json.Marshal(jsonPath(kept, "sync"))
	if kept["enabled"] != true || jsonInt(kept, "total_micros") != jsonInt(good, "total_micros") {
		t.Errorf("throttled day lost the last good data: enabled=%v total %v -> %v", kept["enabled"], good["total_micros"], kept["total_micros"])
	}
	if !bytes.Contains(syncText, []byte("throttl")) {
		t.Errorf("throttled day not labelled: %s", syncText)
	}
	observeFinOps("cost_throttled", map[string]any{"first": map[string]any{"enabled": first["enabled"], "reason": first["reason"]},
		"recovered_total": good["total_micros"], "kept_total": kept["total_micros"], "kept_sync": jsonPath(kept, "sync")})
}

func capReachedFinOpsGroup(t *testing.T) {
	connector := finopsConnector(costAccountConfig("999999999999", "Management"))
	connector.FinOps.CostSourceAccount = "999999999999"
	stack := newFinOpsStack(t, fakeaws.ScenarioCostCapReached, connector)
	stack.refresh()
	paid := stack.fake.PaidCostRequests()
	_, day1, _ := stack.get("/api/admin/aws/finops/summary?period=mtd")
	if paid != 20 {
		t.Errorf("day 1 paid requests at the fixture = %d, want exactly 20", paid)
	}
	if jsonInt(day1, "meter", "used") != 20 || jsonInt(day1, "meter", "cap") != 20 {
		t.Errorf("day 1 meter = %v, want 20 of 20", day1["meter"])
	}
	if day1["enabled"] != false || jsonString(day1, "reason") != "cap_reached" {
		t.Errorf("day 1 that did not fit is not named: enabled=%v reason=%q", day1["enabled"], jsonString(day1, "reason"))
	}
	// The cap holds for the rest of the day: a manual refresh sends nothing.
	stack.post("/api/admin/aws/finops/refresh", `{"confirm":true}`)
	if again := stack.fake.PaidCostRequests(); again != paid {
		t.Errorf("paid requests %d -> %d after the cap", paid, again)
	}
	// Day 2 is priced from day 1's pages and commits within the cap.
	waitLimiterBackoff()
	stack.clock.Set(finopsStackDay.Add(24 * time.Hour))
	stack.refresh()
	day2Paid := stack.fake.PaidCostRequests() - paid
	_, day2, _ := stack.get("/api/admin/aws/finops/summary?period=mtd")
	if day2Paid > 20 || day2["enabled"] != true || jsonInt(day2, "total_micros") <= 0 {
		t.Errorf("day 2: paid %d, enabled=%v reason=%q total=%v", day2Paid, day2["enabled"], jsonString(day2, "reason"), day2["total_micros"])
	}
	if used := jsonInt(day2, "meter", "used"); used != day2Paid {
		t.Errorf("day 2 meter used %d, fixture %d", used, day2Paid)
	}
	observeFinOps("cost_cap_reached", map[string]any{"day1_paid": paid, "day1_reason": day1["reason"], "day1_meter": day1["meter"],
		"day2_paid": day2Paid, "day2_meter": day2["meter"], "day2_notes": jsonPath(day2, "sync", "notes"), "day2_skipped": jsonPath(day2, "sync", "skipped_calls")})
}

func causeTotals(rows []map[string]any) map[string]int64 {
	out := map[string]int64{}
	for _, row := range rows {
		out[jsonString(row, "cause")+"|"+jsonString(row, "record_type")+"|"+jsonString(row, "service")] += jsonInt(row, "amount_micros")
	}
	return out
}

func creditsFeeFinOpsGroup(t *testing.T) {
	stack := newFinOpsStack(t, fakeaws.ScenarioCostCredits, finopsConnector(costAccountConfig("111111111111", "Production")))
	stack.refresh()
	_, bridge, _ := stack.get("/api/admin/aws/finops/bridge?period=mtd")
	bridgeSums(t, bridge)
	_, breakdown, _ := stack.get("/api/admin/aws/finops/breakdown?period=2026-10&group_by=service")
	creditShown := false
	for _, row := range jsonList(breakdown, "rows") {
		if jsonString(row, "key") == "billing.credit" && jsonInt(row, "amount_micros") == -50_000_000 {
			creditShown = true
		}
	}
	if !creditShown {
		t.Errorf("October credit not shown as its own row: %v", jsonList(breakdown, "rows"))
	}
	_, increases, _ := stack.get("/api/admin/aws/finops/increases?period=2026-10&status=all")
	for _, item := range jsonList(increases, "items") {
		if strings.HasPrefix(jsonString(item, "service"), "billing.") {
			t.Errorf("a credit raised an increase: %v", item)
		}
	}
	fee := newFinOpsStack(t, fakeaws.ScenarioCostOneOffFee, organizationConnector())
	fee.refresh()
	_, feeBreakdown, _ := fee.get("/api/admin/aws/finops/breakdown?period=mtd&group_by=service")
	feeShown := false
	for _, row := range jsonList(feeBreakdown, "rows") {
		if jsonString(row, "key") == "billing.fee" && jsonInt(row, "amount_micros") == 1_200_000_000 {
			feeShown = true
		}
	}
	if !feeShown {
		t.Errorf("one-off support fee not shown on its own: %v", jsonList(feeBreakdown, "rows"))
	}
	_, feeIncreases, _ := fee.get("/api/admin/aws/finops/increases?period=mtd&status=all")
	for _, item := range jsonList(feeIncreases, "items") {
		if strings.HasPrefix(jsonString(item, "service"), "billing.") {
			t.Errorf("a one-off fee raised an increase: %v", item)
		}
	}
	if fee.alertCount() != 0 && stack.alertCount() != 0 {
		t.Logf("alerts: credits %d, fee %d", stack.alertCount(), fee.alertCount())
	}
	for _, alert := range fee.alerts {
		if name, _ := alert["name"].(string); strings.Contains(name, "billing.") {
			t.Errorf("the one-off fee alerted: %v", alert)
		}
	}
	observeFinOps("cost_credits_and_one_off_fee", map[string]any{"mtd_bridge_causes": causeTotals(jsonList(bridge, "rows")),
		"credit_rows": jsonList(breakdown, "rows"), "fee_rows": jsonList(feeBreakdown, "rows"), "fee_alerts": fee.alerts})
}

func organizationConnector() config.AWSConnectorConfig {
	connector := finopsConnector(costAccountConfig("999999999999", "Management"),
		costAccountConfig("111111111111", "Production"), costAccountConfig("222222222222", "Data"))
	connector.FinOps.CostSourceAccount = "999999999999"
	return connector
}

func unavailableFinOpsGroup(t *testing.T) {
	connector := finopsConnector(costAccountConfig("111111111111", "Production"))
	connector.FinOps.CostSourceAccount = "111111111111"
	stack := newFinOpsStack(t, fakeaws.ScenarioCostUnavailable, connector)
	stack.refresh()
	status, summary, _ := stack.get("/api/admin/aws/finops/summary?period=2026-10")
	encoded, _ := json.Marshal(summary)
	if status != fiber.StatusOK || !bytes.Contains(encoded, []byte("111111111111")) {
		t.Errorf("unavailable source not named: %d %s", status, encoded)
	}
	if !bytes.Contains(encoded, []byte("unavailable")) {
		t.Errorf("reason unavailable missing: %s", encoded)
	}
	if jsonInt(summary, "total_micros") != 0 && summary["enabled"] == true && len(jsonList(summary, "sync", "missing_accounts")) == 0 {
		t.Errorf("unavailable source shown as data: %s", encoded)
	}
	observeFinOps("ce_unavailable_single_source", map[string]any{"answer": summary["answer"], "sync": jsonPath(summary, "sync"),
		"enabled": summary["enabled"], "reason": summary["reason"]})
}

func organizationFinOpsGroup(t *testing.T) {
	stack := newFinOpsStack(t, fakeaws.ScenarioCostOrganization, organizationConnector())
	stack.refresh()
	_, accounts, _ := stack.get("/api/admin/aws/finops/accounts?period=2026-10")
	kinds := map[string]string{}
	encodedAccounts, _ := json.Marshal(accounts)
	for _, item := range jsonList(accounts, "items") {
		kinds[jsonString(item, "id")+jsonString(item, "account")] = jsonString(item, "kind") + jsonString(item, "account_kind")
	}
	if !bytes.Contains(encodedAccounts, []byte("333333333333")) || !bytes.Contains(encodedAccounts, []byte("cost_only")) {
		t.Errorf("cost-only account not shown: %s", encodedAccounts)
	}
	_, savings, savingsBody := stack.get("/api/admin/aws/finops/savings?period=2026-10")
	if !bytes.Contains(savingsBody, []byte("needs_usage_detail")) {
		t.Errorf("no needs_usage_detail gap: %s", jsonPath(savings, "gaps"))
	}
	_, bridge, _ := stack.get("/api/admin/aws/finops/bridge?period=mtd")
	bridgeSums(t, bridge)
	observeFinOps("organization_payer", map[string]any{"account_kinds": kinds, "savings_gaps": jsonPath(savings, "gaps"),
		"paid": stack.fake.PaidCostRequests()})
}

func drillGuardFinOpsGroup(t *testing.T) {
	stack := natSpikeStack(t)
	before := stack.fake.PaidCostRequests()
	status, body := stack.post("/api/admin/aws/finops/drilldown", `{"account":"111111111111","service":"Invented Service"}`)
	if status != fiber.StatusNotFound {
		t.Errorf("unknown pair = %d %s", status, body)
	}
	// Next morning, before the scheduled refresh, with a cap the refresh
	// needs whole: the click is refused and spends nothing.
	stack.clock.Set(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC))
	connector := config.GetConfig().Connectors.AWS
	connector.FinOps.MaxPaidRequestsPerDay = 6
	stack.setConnector(connector)
	stack.refresh() // not due yet: only updates the options
	status, body = stack.post("/api/admin/aws/finops/drilldown", `{"account":"111111111111","service":"EC2 - Other"}`)
	if status != fiber.StatusTooManyRequests || !bytes.Contains(body, []byte("reserved_for_scheduled_refresh")) {
		t.Errorf("reserved drill = %d %s", status, body)
	}
	if after := stack.fake.PaidCostRequests(); after != before {
		t.Errorf("refused drill-downs sent %d paid requests", after-before)
	}
	observeFinOps("drilldown_guards", map[string]any{"unknown_pair": fiber.StatusNotFound, "reserved": status, "paid_before": before})
}

func exportFinOpsGroup(t *testing.T) {
	connector := finopsConnector(costAccountConfig("111111111111", `=HYPERLINK("http://x","y")`))
	stack := newFinOpsStack(t, fakeaws.ScenarioCostNatSpike, connector)
	stack.refresh()
	for _, table := range []string{"breakdown", "increases", "savings"} {
		response, body := stack.client.do(http.MethodGet, "/api/admin/aws/finops/export.csv?period=2026-10&group_by=account&table="+table, "")
		saveFinOpsBody(t, "export_"+table, body)
		if response.StatusCode != fiber.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/csv") {
			t.Errorf("%s export = %d %s", table, response.StatusCode, response.Header.Get("Content-Type"))
			continue
		}
		rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
		if err != nil || len(rows) < 2 {
			t.Errorf("%s csv: %v (%d rows)", table, err, len(rows))
			continue
		}
		for _, row := range rows[1:] {
			for index, cell := range row {
				if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) && !looksNumeric(cell) {
					t.Errorf("%s column %d is a live formula: %q", table, index, cell)
				}
			}
		}
		if table == "breakdown" && !bytes.Contains(body, []byte(`'=HYPERLINK`)) {
			t.Errorf("injected account name not neutralised: %s", body)
		}
	}
	observeFinOps("export_csv_injection", map[string]any{"tables": 3})
}

func looksNumeric(cell string) bool {
	for _, character := range strings.TrimPrefix(cell, "-") {
		if (character < '0' || character > '9') && character != '.' {
			return false
		}
	}
	return len(cell) > 1
}

func alertOnceFinOpsGroup(t *testing.T) {
	stack := natSpikeStack(t)
	_, increases, _ := stack.get("/api/admin/aws/finops/increases?period=2026-10&status=open")
	open := len(jsonList(increases, "items"))
	first := stack.alertKindCount("cost_increase")
	if open == 0 || first != open {
		t.Errorf("first refresh alerted %d times for %d open increases", first, open)
	}
	// A retried pump and a refresh the next day send nothing new.
	if sent, err := stack.pumpAgain(); err != nil || sent != 0 {
		t.Errorf("second pump sent %d (%v)", sent, err)
	}
	stack.clock.Set(finopsStackDay.Add(24 * time.Hour))
	stack.refresh()
	if again := stack.alertKindCount("cost_increase"); again != first {
		t.Errorf("alerts %d -> %d after the next refresh", first, again)
	}
	incidents, err := stack.provider.ListIncidents(50)
	observeFinOps("increase_alert_once", map[string]any{"open_increases": open, "alerts": stack.alerts, "incidents": len(incidents), "list_error": errString(err)})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func disabledFinOpsGroup(t *testing.T) {
	connector := finopsConnector(costAccountConfig("111111111111", "Production"))
	connector.OptionalData.CostExplorer = false
	stack := newFinOpsStack(t, fakeaws.ScenarioCostNatSpike, connector)
	if stack.refresh() {
		t.Error("a refresh ran with cost data off")
	}
	for _, route := range []string{"summary", "bridge", "increases", "savings", "accounts", "budgets"} {
		status, view, _ := stack.get("/api/admin/aws/finops/" + route + "?period=2026-10")
		if status != fiber.StatusOK || view["enabled"] != false || jsonString(view, "reason") == "" {
			t.Errorf("%s = %d enabled=%v reason=%q", route, status, view["enabled"], jsonString(view, "reason"))
		}
	}
	if paid := stack.fake.PaidCostRequests(); paid != 0 {
		t.Errorf("disabled FinOps sent %d paid requests", paid)
	}
	observeFinOps("disabled_answers_200", map[string]any{"paid": stack.fake.PaidCostRequests()})
}

// Two NAT gateways and no resource-level cost: the two deploys tie, each
// Possible, with the hint to turn on resource-level data.
func twoNATsFinOpsGroup(t *testing.T) {
	stack := newFinOpsStack(t, fakeaws.ScenarioCostNatSpikeTwoNATs, finopsConnector(costAccountConfig("111111111111", "Production")))
	stack.refresh()
	nat := natIncrease(t, stack)
	suspects := jsonList(nat, "suspects")
	refs := []string{}
	for _, suspect := range suspects {
		refs = append(refs, jsonString(suspect, "ref"))
		if jsonString(suspect, "confidence") != "possible" || jsonPath(suspect, "tied") != true {
			t.Errorf("tied suspect = %v", suspect)
		}
	}
	if len(suspects) < 2 || !containsStringValue(refs, "change:evt-checkout-42") || !containsStringValue(refs, "change:evt-inventory-7") {
		t.Errorf("tied suspects = %v", refs)
	}
	if jsonString(nat, "suspect_change", "confidence_word") == "Likely" {
		t.Errorf("a tie named a Likely suspect: %v", nat["suspect_change"])
	}
	encoded, _ := json.Marshal(suspects)
	if !bytes.Contains(bytes.ToLower(encoded), []byte("resource-level")) {
		t.Errorf("no resource-level hint on the tie: %s", encoded)
	}
	_, summary, _ := stack.get("/api/admin/aws/finops/summary?period=2026-10")
	if jsonPath(summary, "sync", "resource_data") != false {
		t.Errorf("refused resource detail not marked off: %v", jsonPath(summary, "sync", "resource_data"))
	}
	observeFinOps("cost_nat_spike_two_nats_tie", map[string]any{"suspects": suspects, "sentence": nat["sentence"],
		"resource_data": jsonPath(summary, "sync", "resource_data"), "cost_calls": stack.fake.CostCalls()})
}

// A deployment budget alerts once when it goes At risk and once more when it
// goes Over budget; repeats in the same month send nothing.
func budgetAlertFinOpsGroup(t *testing.T) {
	connector := finopsConnector(costAccountConfig("111111111111", "Production"))
	connector.FinOps.Budgets = []config.AWSFinOpsBudgetConfig{{Name: "AWS total", Scope: "deployment", MonthlyUSD: 5000}}
	stack := newFinOpsStack(t, fakeaws.ScenarioCostNatSpike, connector)
	stack.refresh()
	_, budgets, _ := stack.get("/api/admin/aws/finops/budgets?period=mtd")
	atRisk := stack.alertKindCount("budget_status")
	if atRisk != 1 {
		t.Errorf("At risk alerts = %d, want 1 (alerts %v)", atRisk, stack.alerts)
	}
	if sent, err := stack.pumpAgain(); err != nil || sent != 0 {
		t.Errorf("repeat pump sent %d (%v)", sent, err)
	}
	waitLimiterBackoff()
	stack.clock.Set(finopsStackDay.Add(24 * time.Hour))
	stack.refresh()
	if again := stack.alertKindCount("budget_status"); again != 1 {
		t.Errorf("At risk re-alerted the next day: %d", again)
	}
	// Lower the budget below month-to-date spend: Over budget, once.
	lowered := config.GetConfig().Connectors.AWS
	lowered.FinOps.Budgets = []config.AWSFinOpsBudgetConfig{{Name: "AWS total", Scope: "deployment", MonthlyUSD: 1000}}
	stack.setConnector(lowered)
	stack.clock.Set(finopsStackDay.Add(48 * time.Hour))
	stack.refresh()
	over := stack.alertKindCount("budget_status")
	if over != 2 {
		t.Errorf("budget alerts after going over = %d, want 2", over)
	}
	if sent, err := stack.pumpAgain(); err != nil || sent != 0 {
		t.Errorf("repeat pump after over budget sent %d (%v)", sent, err)
	}
	names := []string{}
	for _, alert := range stack.alerts {
		if alert["kind"] == "budget_status" {
			name, _ := alert["name"].(string)
			names = append(names, name)
		}
	}
	if len(names) != 2 || !strings.HasPrefix(names[0], "Budget at risk: ") || !strings.HasPrefix(names[1], "Over budget: ") {
		t.Errorf("budget alert names = %v", names)
	}
	observeFinOps("budget_alert_once_per_level", map[string]any{"budgets": jsonList(budgets, "items"), "alerts": names})
}

// A new untagged database: its own new-resource cause in the bridge, an
// increase with no team owner, never checkout.
func newUntaggedDBFinOpsGroup(t *testing.T) {
	stack := newFinOpsStack(t, fakeaws.ScenarioCostNewUntaggedDB, finopsConnector(costAccountConfig("111111111111", "Production")))
	stack.refresh()
	// The first setup does not read back to September, so the new database
	// shows in the month-to-date bridge (Nov 1-7 against Oct 1-7).
	_, bridge, _ := stack.get("/api/admin/aws/finops/bridge?period=mtd")
	if bridge["available"] != true {
		t.Errorf("month-to-date bridge unavailable: %v", bridge["compare"])
	}
	bridgeSums(t, bridge)
	var db map[string]any
	for _, row := range jsonList(bridge, "rows") {
		if jsonString(row, "service") == "database.relational" {
			db = row
		}
	}
	if db == nil || jsonString(db, "cause") != "new_resource" {
		t.Errorf("database row = %v (rows %v)", db, jsonList(bridge, "rows"))
	}
	_, increases, _ := stack.get("/api/admin/aws/finops/increases?period=2026-10&status=all")
	var inc map[string]any
	for _, item := range jsonList(increases, "items") {
		if jsonString(item, "service") == "database.relational" {
			inc = item
		}
	}
	if inc == nil {
		t.Errorf("no increase for the new database: %v", jsonList(increases, "items"))
	} else if owner := jsonString(inc, "owner", "name"); owner == "checkout" || owner == "platform" {
		t.Errorf("untagged database given a team owner: %v", inc["owner"])
	}
	observeFinOps("cost_new_untagged_db", map[string]any{"db_row": db, "increase": inc, "bridge_rows": jsonList(bridge, "rows"), "compare": bridge["compare"]})
}
