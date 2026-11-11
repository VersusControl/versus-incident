package controllers

// aws_collectors_stack_test.go — the integration gate for the AWS collectors
// slice. It wires the settings service, the committed snapshot store, the
// collector and its scheduler exactly the way cmd/main.go
// registerAWSSettingsController does, mounts the controller router with the
// production guard chain, and injects tests/fakeaws only through the
// product's existing HTTPClient seam. The first sync starts the product way:
// committing an account over HTTP fires WithAccountAdded -> Scheduler.Trigger.
//
// The collectors add no HTTP read route, so the suite asserts through the
// Settings `sync` field over HTTP and through the snapshot store's own read
// surface (Resources, Health, Changes, Sync) — the methods later slices wrap.
// Fixture evidence only: nothing here reaches AWS.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	awscollect "github.com/VersusControl/versus-incident/pkg/aws/collect"
	awslimit "github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"

	"github.com/gofiber/fiber/v2"
)

const (
	awsCollectAccount = "111122223333"
	awsCollectRole    = "arn:aws:iam::111122223333:role/versus-reader"
	awsCollectRegion  = "us-east-1"
	// awsCollectFastBudget keeps the functional groups quick; the budget group
	// runs at the production default instead.
	awsCollectFastBudget = 100
	// awsCollectDefaultBudget is connectors.aws.budgets.max_requests_per_second
	// when unset (pkg/config applies 2).
	awsCollectDefaultBudget = 2
	// One region: six regional health + aws-health, cloudtrail, fourteen
	// regional inventory + four global inventory units.
	awsCollectUnitsOneRegion = 26
)

// awsCollectDenied is what access-denied-burst denies outright.
var awsCollectDenied = []string{"lambda", "sqs", "sns", "eventbridge", "step-functions", "kinesis", awssnapshot.TagService}

// ---- request ledger (harness side, wraps the fixture transport) ----------

type awsRequestRecord struct {
	at      time.Time
	account string
	region  string
	service string
}

// awsRequestRecorder timestamps every request that reaches the fixture with
// the caller account (session token) and the SigV4 signing region/service.
// It sits below the SDK middleware, so a request is recorded after it has
// waited on the product limiter — what AWS would see.
type awsRequestRecorder struct {
	next    http.RoundTripper
	mu      sync.Mutex
	records []awsRequestRecord
}

func (recorder *awsRequestRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	service, region := awsSigningScope(request.Header.Get("Authorization"))
	account := awsFirstTwelveDigits(request.Header.Get("X-Amz-Security-Token"))
	if account == "" {
		account = "host"
	}
	recorder.mu.Lock()
	recorder.records = append(recorder.records, awsRequestRecord{at: time.Now(), account: account, region: region, service: service})
	recorder.mu.Unlock()
	return recorder.next.RoundTrip(request)
}

func (recorder *awsRequestRecorder) snapshot() []awsRequestRecord {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]awsRequestRecord(nil), recorder.records...)
}

func awsSigningScope(authorization string) (string, string) {
	index := strings.Index(authorization, "Credential=")
	if index < 0 {
		return "", ""
	}
	credential := authorization[index+len("Credential="):]
	if comma := strings.Index(credential, ","); comma >= 0 {
		credential = credential[:comma]
	}
	parts := strings.Split(strings.TrimSpace(credential), "/")
	if len(parts) < 5 {
		return "", ""
	}
	return parts[3], parts[2]
}

func awsFirstTwelveDigits(value string) string {
	run := 0
	for index := 0; index <= len(value); index++ {
		if index < len(value) && value[index] >= '0' && value[index] <= '9' {
			run++
			continue
		}
		if run == 12 {
			return value[index-12 : index]
		}
		run = 0
	}
	return ""
}

// ---- stack ---------------------------------------------------------------

type awsCollectOptions struct {
	budget  int
	dataDir string // file store when set, memory otherwise
}

type awsCollectStack struct {
	t         *testing.T
	app       *fiber.App
	service   *awssettings.Service
	snapshots *awssnapshot.Store
	scheduler *awscollect.Scheduler
	fake      *fakeaws.Server
	recorder  *awsRequestRecorder
	provider  storage.Provider

	cfgMu sync.Mutex
	cfg   config.AWSConnectorConfig
}

func (stack *awsCollectStack) readConfig() config.AWSConnectorConfig {
	stack.cfgMu.Lock()
	defer stack.cfgMu.Unlock()
	return stack.cfg
}

func newAWSCollectStack(t *testing.T, scenario string, options awsCollectOptions) *awsCollectStack {
	t.Helper()
	middleware.SetGatewayAuthEnabled(true)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(true) })
	pinGatewaySecret(t, awsStackSecret)

	stack := &awsCollectStack{t: t, fake: fakeaws.New(t, scenario)}
	stack.recorder = &awsRequestRecorder{next: stack.fake.HTTPClient().Transport}
	client := &http.Client{Transport: stack.recorder}
	stack.cfg = config.AWSConnectorConfig{
		Enable:              true,
		HealthCheckInterval: "5m",
		Budgets:             config.AWSBudgetsConfig{MaxRequestsPerSecond: options.budget},
		Collect:             config.AWSCollectConfig{Enable: true},
	}
	if options.dataDir != "" {
		provider, err := storage.NewFile(storage.FileOptions{DataDir: options.dataDir})
		if err != nil {
			t.Fatalf("file store: %v", err)
		}
		stack.provider = provider
	} else {
		stack.provider = storage.NewMemory()
	}

	// Same construction as cmd/main.go registerAWSSettingsController; only
	// the HTTP client, the host identity and the config source differ.
	limiter := awslimit.NewManager(options.budget)
	current := stack.readConfig
	scope := tenancy.DefaultOrgScope()
	snapshots := awssnapshot.New(stack.provider, scope, awssnapshot.WithOptions(func() awssnapshot.Options {
		connector := current()
		return awssnapshot.Options{
			MaxGenerations:  connector.Collect.MaxGenerations(),
			MaxAge:          connector.Collect.MaxAge(),
			ChangeRetention: connector.Collect.ChangeRetention(),
			AppSources:      connector.Applications.EffectiveSources(),
		}
	}))
	var service *awssettings.Service
	collector := awscollect.New(snapshots, limiter,
		awscollect.WithLogger(log.Printf),
		awscollect.WithHTTPClient(client),
		awscollect.WithOptions(func() awscollect.Options {
			return awscollect.Options{
				TagAllowlist: service.TagValueAllowlist(),
				AppTagKeys:   current().Applications.EffectiveTagKeys(),
			}
		}),
	)
	scheduler := awscollect.NewScheduler(collector,
		func(ctx context.Context) []awscollect.Target {
			targets := service.CollectTargets(ctx)
			out := make([]awscollect.Target, 0, len(targets))
			for _, target := range targets {
				out = append(out, awscollect.Target{AccountID: target.AccountID, Regions: target.Regions, Credentials: target.Credentials})
			}
			return out
		},
		func() awscollect.Intervals {
			collect := current().Collect
			return awscollect.Intervals{Inventory: collect.InventoryInterval(), Health: collect.HealthInterval(), Changes: collect.ChangesInterval()}
		},
		func() bool {
			connector := current()
			return connector.Enable && connector.Collect.Enable
		},
		awscollect.WithAccounts(func(ctx context.Context) ([]string, error) { return service.AccountIDs(ctx) }),
	)
	service = awssettings.NewService(
		awssettings.NewStore(stack.provider, scope),
		awssettings.WithConfig(current),
		awssettings.WithHTTPClient(client),
		awssettings.WithBaseProvider(awsStackBase),
		awssettings.WithLimiter(limiter),
		awssettings.WithSnapshots(snapshots),
		awssettings.WithAccountAdded(scheduler.Trigger),
	)
	stack.service, stack.snapshots, stack.scheduler = service, snapshots, scheduler
	if collector.Limiter() != service.Limiter() {
		t.Fatal("collectors and probes do not share one limiter")
	}

	stack.app = fiber.New(fiber.Config{Immutable: true})
	stack.app.Use(middleware.OrgInjector())
	api := stack.app.Group("/api")
	RegisterGatewaySessionRoutes(api)
	NewAWSSettingsController(service).Register(api)

	ctx, cancel := context.WithCancel(context.Background())
	service.Start(ctx)
	scheduler.Start(ctx)
	t.Cleanup(func() {
		cancel()
		for name, done := range map[string]<-chan struct{}{"settings": service.Stopped(), "scheduler": scheduler.Stopped()} {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Errorf("%s loop did not stop", name)
			}
		}
		_ = stack.provider.Close()
	})
	return stack
}

func (stack *awsCollectStack) client() *awsClient {
	client := &awsClient{t: stack.t, app: stack.app}
	client.gatewaySession(awsStackSecret)
	return client
}

// ---- helpers -------------------------------------------------------------

// connectCollectAccount runs the wizard over HTTP and returns the time just
// before the commit that triggers the first sync.
func connectCollectAccount(t *testing.T, client *awsClient, name string) time.Time {
	t.Helper()
	start := startStackDraft(t, client)
	verifyStackDraft(t, client, start.DraftID, awsCollectRole)
	waitStackDraft(t, client, start.DraftID, 180*time.Second, false)
	committed := time.Now()
	commitStackAccount(t, client, start.DraftID, name, []string{awsCollectRegion})
	return committed
}

func accountSync(t *testing.T, client *awsClient, accountID string) (awssettings.AccountSyncView, bool) {
	t.Helper()
	view, _ := readSettings(t, client)
	for _, account := range view.Accounts {
		if account.AccountID == accountID {
			return account.Sync, true
		}
	}
	return awssettings.AccountSyncView{}, false
}

type awsSyncObservation struct {
	At        string `json:"at"`
	State     string `json:"state"`
	FirstSync bool   `json:"first_sync"`
	UnitsDone int    `json:"units_done"`
	Units     int    `json:"units_total"`
	SyncedAt  bool   `json:"synced_at_set"`
}

// waitFirstSync polls the Settings sync field until every planned unit was
// attempted, and returns the distinct progress states it saw on the way.
func waitFirstSync(t *testing.T, client *awsClient, accountID string, deadline time.Duration) (awssettings.AccountSyncView, []awsSyncObservation) {
	t.Helper()
	started := time.Now()
	var seen []awsSyncObservation
	var last awssettings.AccountSyncView
	previousDone := -1
	for {
		view, ok := accountSync(t, client, accountID)
		if !ok {
			t.Fatalf("account %s missing from settings", accountID)
		}
		observation := awsSyncObservation{
			At: time.Since(started).Round(time.Millisecond).String(), State: view.State, FirstSync: view.FirstSync,
			UnitsDone: view.UnitsDone, Units: view.Units, SyncedAt: view.SyncedAt != nil,
		}
		if len(seen) == 0 || seen[len(seen)-1].State != observation.State || seen[len(seen)-1].UnitsDone != observation.UnitsDone ||
			seen[len(seen)-1].Units != observation.Units || seen[len(seen)-1].FirstSync != observation.FirstSync {
			seen = append(seen, observation)
		}
		if view.UnitsDone < previousDone {
			t.Fatalf("units_done went backwards %d -> %d: %+v", previousDone, view.UnitsDone, seen)
		}
		previousDone = view.UnitsDone
		last = view
		if view.Units == awsCollectUnitsOneRegion && view.UnitsDone == view.Units && !view.FirstSync {
			return view, seen
		}
		if time.Since(started) > deadline {
			t.Fatalf("first sync did not finish in %s; last=%+v seen=%+v", deadline, last, seen)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitAttemptedAfter waits until every unit of the account was attempted
// after mark, read through the store's Sync surface.
func waitAttemptedAfter(t *testing.T, store *awssnapshot.Store, accountID string, mark time.Time, deadline time.Duration) []awssnapshot.UnitStatus {
	t.Helper()
	stop := time.Now().Add(deadline)
	for {
		statuses, err := store.Sync(context.Background(), awssnapshot.Query{Account: accountID})
		if err != nil {
			t.Fatalf("store sync: %v", err)
		}
		fresh := 0
		for _, status := range statuses {
			if !status.Sync.AttemptedAt.Before(mark) {
				fresh++
			}
		}
		if len(statuses) == awsCollectUnitsOneRegion && fresh == len(statuses) {
			return statuses
		}
		if time.Now().After(stop) {
			t.Fatalf("only %d/%d units attempted after %s", fresh, len(statuses), mark.Format(time.RFC3339Nano))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func serviceSync(view awssettings.AccountSyncView, service string) (awssettings.ServiceSyncView, bool) {
	for _, row := range view.Services {
		if row.Service == service {
			return row, true
		}
	}
	return awssettings.ServiceSyncView{}, false
}

var (
	awsCollectObservationsMu sync.Mutex
	awsCollectObservations   = map[string]any{}
)

func observeCollect(group string, value any) {
	awsCollectObservationsMu.Lock()
	defer awsCollectObservationsMu.Unlock()
	awsCollectObservations[group] = value
}

// ---- suite ---------------------------------------------------------------

// TestAWSCollectorsStackE2E is the integration verdict for the collectors.
// Each group is one acceptance item; the run writes
// results/aws-collectors-verdict.json when the harness supplies
// HARNESS_RESULTS_DIR.
func TestAWSCollectorsStackE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("integration suite needs the fakeaws HTTP stack")
	}
	groups := []struct {
		name string
		run  func(*testing.T)
	}{
		{"bad_deploy_first_sync", badDeployCollectGroup},
		{"throttled_api_keeps_last_good", throttledCollectGroup},
		{"access_denied_burst_named", accessDeniedCollectGroup},
		{"secret_canary_no_leak", secretCanaryCollectGroup},
		{"budget_two_rps_per_account_region", budgetCollectGroup},
		{"delete_purges_then_fresh_first_sync", deletePurgeCollectGroup},
	}
	results := make([]map[string]string, 0, len(groups))
	for _, group := range groups {
		passed := t.Run(group.name, group.run)
		verdict := "fail"
		if passed {
			verdict = "pass"
		}
		results = append(results, map[string]string{"group": group.name, "verdict": verdict})
	}
	dir := os.Getenv("HARNESS_RESULTS_DIR")
	if dir == "" {
		return
	}
	awsCollectObservationsMu.Lock()
	payload := map[string]any{
		"suite":        "aws-collectors",
		"task":         os.Getenv("HARNESS_TASK"),
		"run_id":       os.Getenv("HARNESS_RUN_ID"),
		"backend":      "tests/fakeaws via the product HTTPClient seam; wiring mirrors cmd/main.go (fixture, not live AWS)",
		"read_surface": "GET /api/admin/aws/settings sync field over HTTP + pkg/aws/snapshot Store read methods in-process (A1 adds no read route)",
		"groups":       results,
		"observations": awsCollectObservations,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	awsCollectObservationsMu.Unlock()
	if err != nil {
		t.Fatalf("verdict encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws-collectors-verdict.json"), encoded, 0o600); err != nil {
		t.Errorf("write verdict: %v", err)
	}
}

// 1. bad-deploy: first sync per service, Settings progress, alarm, task-def
// change, unhealthy targets, rollout, change window without gaps, as_of.
func badDeployCollectGroup(t *testing.T) {
	stack := newAWSCollectStack(t, fakeaws.ScenarioBadDeploy, awsCollectOptions{budget: awsCollectFastBudget})
	client := stack.client()
	committed := connectCollectAccount(t, client, "Production")

	view, seen := waitFirstSync(t, client, awsCollectAccount, 120*time.Second)
	observeCollect("bad_deploy_progress", seen)
	t.Logf("first-sync progress: %+v", seen)
	sawFirstSync := false
	for _, observation := range seen {
		if observation.FirstSync && observation.UnitsDone < awsCollectUnitsOneRegion {
			sawFirstSync = true
		}
		// The plan is known from the start: the total never grows and
		// first_sync stays true until every unit has run.
		if observation.Units != 0 && observation.Units != awsCollectUnitsOneRegion {
			t.Errorf("first-sync units_total = %d mid-sync, want %d from the start: %+v", observation.Units, awsCollectUnitsOneRegion, observation)
		}
		if !observation.FirstSync && observation.UnitsDone < awsCollectUnitsOneRegion {
			t.Errorf("first_sync false before every unit ran: %+v", observation)
		}
	}
	if !sawFirstSync {
		t.Errorf("settings never showed first-sync progress before completion: %+v", seen)
	}
	if view.State != awssettings.SyncStateOK {
		failed := []string{}
		for _, row := range view.Services {
			if row.State != awssettings.SyncStateOK {
				failed = append(failed, row.Service+"="+row.State+fmt.Sprint(row.Gaps))
			}
		}
		t.Errorf("clean bad-deploy sync state = %s; not ok: %v", view.State, failed)
	}
	if view.SyncedAt == nil || view.SyncedAt.Before(committed) || time.Since(*view.SyncedAt) > time.Minute {
		t.Fatalf("synced_at = %v (committed %s)", view.SyncedAt, committed)
	}
	t.Logf("Settings: synced %s ago, %d/%d units, %d services", time.Since(*view.SyncedAt).Round(time.Second), view.UnitsDone, view.Units, len(view.Services))
	if len(view.Services) != awsCollectUnitsOneRegion {
		t.Errorf("settings services = %d, want one per unit (%d)", len(view.Services), awsCollectUnitsOneRegion)
	}
	for _, row := range view.Services {
		if row.UnitsDone != row.Units || row.LastGoodAt == nil {
			t.Errorf("service %s not committed: %+v", row.Service, row)
		}
	}

	ctx := context.Background()
	health, err := stack.snapshots.Health(ctx, awssnapshot.Query{Account: awsCollectAccount})
	if err != nil {
		t.Fatal(err)
	}
	var alarm, targets, rollout *model.Health
	for index := range health.Health {
		item := &health.Health[index]
		switch {
		case item.Source == model.SourceAlarm && strings.Contains(item.Ref, fakeaws.WorldAlarm5xx):
			alarm = item
		case item.Source == model.SourceTargetHealth:
			targets = item
		case item.Source == model.SourceRollout:
			rollout = item
		}
	}
	if alarm == nil || alarm.Status != model.HealthCritical || !strings.Contains(alarm.Resource, fakeaws.WorldLoadBalancerID) {
		t.Errorf("5xx alarm = %+v", alarm)
	}
	if targets == nil || !strings.HasPrefix(targets.Summary, "3 of 4 targets unhealthy") {
		t.Errorf("target health = %+v", targets)
	}
	if rollout == nil || !strings.Contains(rollout.Summary, "checkout:42") {
		t.Errorf("rollout = %+v", rollout)
	}

	// The newest overlap stretch is read but deliberately not claimed (late
	// CloudTrail delivery); the settled lookback before it must be gap-free.
	checkpoint, err := stack.snapshots.Checkpoint(awsCollectAccount, awsCollectRegion)
	if err != nil {
		t.Fatal(err)
	}
	settled := checkpoint.CoveredUntil()
	until := time.Now()
	if lag := until.Sub(settled); lag < awscollect.DefaultChangeOverlap || lag > awscollect.DefaultChangeOverlap+time.Minute {
		t.Errorf("checkpoint covered until %s, %s before now; want about the %s overlap", settled, lag, awscollect.DefaultChangeOverlap)
	}
	window, err := stack.snapshots.Changes(ctx, awssnapshot.Query{Account: awsCollectAccount}, until.Add(-23*time.Hour), settled)
	if err != nil {
		t.Fatal(err)
	}
	if !window.Complete || len(window.Gaps) != 0 {
		t.Errorf("clean-run settled change window has gaps: complete=%v gaps=%+v", window.Complete, window.Gaps)
	}
	recent, err := stack.snapshots.Changes(ctx, awssnapshot.Query{Account: awsCollectAccount}, settled, until)
	if err != nil {
		t.Fatal(err)
	}
	if recent.Complete || len(recent.Gaps) == 0 {
		t.Errorf("unsettled overlap claimed as covered: complete=%v gaps=%+v", recent.Complete, recent.Gaps)
	}
	summaries := map[string]model.Change{}
	for _, change := range append(append([]model.Change{}, window.Changes...), recent.Changes...) {
		summaries[change.Summary] = change
	}
	registered, ok := summaries["New task definition checkout:42 registered"]
	if !ok || registered.Actor.Class != model.ActorPipeline {
		t.Errorf("task definition change = %+v (found %v); all=%v", registered, ok, mapKeys(summaries))
	}
	if _, ok := summaries["Service checkout updated to task definition checkout:42"]; !ok {
		t.Errorf("service update change missing; all=%v", mapKeys(summaries))
	}

	resources, err := stack.snapshots.Resources(ctx, awssnapshot.Query{Account: awsCollectAccount})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) == 0 || len(resources.Gaps) != 0 {
		t.Errorf("resources = %d, gaps = %+v", len(resources.Resources), resources.Gaps)
	}
	// as_of: before the commit nothing existed; at synced_at the alarm did.
	before, _ := stack.snapshots.Health(ctx, awssnapshot.Query{Account: awsCollectAccount, AsOf: committed.Add(-time.Second)})
	at, _ := stack.snapshots.Health(ctx, awssnapshot.Query{Account: awsCollectAccount, AsOf: time.Now()})
	if len(before.Health) != 0 || len(at.Health) != len(health.Health) {
		t.Errorf("as_of health: before commit %d, now %d (latest %d)", len(before.Health), len(at.Health), len(health.Health))
	}
	observeCollect("bad_deploy", map[string]any{
		"services": len(view.Services), "units": view.Units, "state": view.State,
		"resources": len(resources.Resources), "health": len(health.Health), "changes_settled": len(window.Changes), "changes_recent": len(recent.Changes),
		"settled_window_complete": window.Complete, "recent_window_gaps": recent.Gaps,
		"alarm": alarm != nil, "targets": summaryOf(targets), "rollout": summaryOf(rollout),
	})
}

func summaryOf(item *model.Health) string {
	if item == nil {
		return ""
	}
	return item.Summary
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// 2. throttled-api: a clean sync, then a throttled re-sync: every unit keeps
// its count, failures are named throttled with last_good_at, never zero.
func throttledCollectGroup(t *testing.T) {
	stack := newAWSCollectStack(t, "throttled-api", awsCollectOptions{budget: awsCollectFastBudget})
	stack.fake.SetThrottle(0)
	client := stack.client()
	connectCollectAccount(t, client, "Throttled")
	waitFirstSync(t, client, awsCollectAccount, 120*time.Second)

	ctx := context.Background()
	before, err := stack.snapshots.Resources(ctx, awssnapshot.Query{Account: awsCollectAccount})
	if err != nil || len(before.Resources) == 0 {
		t.Fatalf("clean resources = %d, %v", len(before.Resources), err)
	}
	beforeCounts := map[string]int{}
	for _, unit := range before.Units {
		beforeCounts[unit.Unit.Key()] = unit.Count
	}

	// Phase A: every second request is throttled. Each throttle is parsed in
	// its own protocol envelope and retried through the shared limiter, so
	// every unit still commits.
	stack.fake.SetThrottle(2)
	markRetry := time.Now()
	// The scheduler's own wake seam (the one an account add fires).
	stack.scheduler.Trigger(awsCollectAccount)
	retried := waitAttemptedAfter(t, stack.snapshots, awsCollectAccount, markRetry, 180*time.Second)
	retryFailed := map[string]string{}
	for _, status := range retried {
		if status.Sync.State != model.SyncOK {
			retryFailed[status.Unit.Key()] = status.Sync.ErrorCode
		}
	}
	if len(retryFailed) != 0 {
		t.Errorf("alternate-request throttling was not absorbed by the retry: %v", retryFailed)
	}

	// Phase B: every request is throttled, so the retries run out. Every unit
	// must keep its last good data and name the gap `throttled`.
	stack.fake.SetThrottle(1)
	mark := time.Now()
	stack.scheduler.Trigger(awsCollectAccount)
	statuses := waitAttemptedAfter(t, stack.snapshots, awsCollectAccount, mark, 400*time.Second)
	stack.fake.SetThrottle(0)

	failed := 0
	codes := map[string]int{}
	for _, status := range statuses {
		if status.Sync.State != model.SyncFailed {
			t.Errorf("unit %s = %s under full throttling", status.Unit.Key(), status.Sync.State)
			continue
		}
		failed++
		codes[status.Sync.ErrorCode]++
		if status.Sync.ErrorCode != "throttled" {
			t.Errorf("unit %s error_code = %q, want throttled", status.Unit.Key(), status.Sync.ErrorCode)
		}
		if !status.HasData && beforeCounts[status.Unit.Key()] > 0 {
			t.Errorf("throttled unit %s lost its data: %+v", status.Unit.Key(), status)
		}
	}
	t.Logf("full throttling: %d failed units, error codes %v", failed, codes)
	if failed == 0 {
		t.Fatal("throttling did not fail any unit; scenario not exercised")
	}
	after, err := stack.snapshots.Resources(ctx, awssnapshot.Query{Account: awsCollectAccount})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Resources) != len(before.Resources) {
		t.Errorf("resources %d -> %d under throttling", len(before.Resources), len(after.Resources))
	}
	for _, unit := range after.Units {
		if want := beforeCounts[unit.Unit.Key()]; want > 0 && unit.Count != want {
			t.Errorf("unit %s count %d -> %d", unit.Unit.Key(), want, unit.Count)
		}
	}
	reasons := map[string]int{}
	for _, gap := range after.Gaps {
		reasons[string(gap.Reason)]++
		if gap.Reason != model.GapThrottled {
			t.Errorf("gap reason %s, want throttled: %+v", gap.Reason, gap)
		}
		if gap.LastGoodAt.IsZero() {
			t.Errorf("gap without last_good_at: %+v", gap)
		}
	}
	if reasons[string(model.GapThrottled)] == 0 {
		t.Errorf("no throttled gap named: %+v", after.Gaps)
	}

	view, ok := accountSync(t, client, awsCollectAccount)
	if !ok {
		t.Fatal("account missing from settings")
	}
	if view.State != awssettings.SyncStateFailed && view.State != awssettings.SyncStatePartial {
		t.Errorf("settings sync state = %s, want failed or partial", view.State)
	}
	if view.SyncedAt == nil {
		t.Error("settings lost synced_at (last good) under throttling")
	}
	settingsThrottled := 0
	for _, row := range view.Services {
		if row.State != awssettings.SyncStateFailed {
			continue
		}
		settingsThrottled++
		if row.LastGoodAt == nil {
			t.Errorf("failed service %s has no last_good_at", row.Service)
		}
		if len(row.Gaps) != 1 || row.Gaps[0] != string(model.GapThrottled) {
			t.Errorf("failed service %s gaps = %v, want [throttled]", row.Service, row.Gaps)
		}
	}
	if settingsThrottled == 0 {
		t.Error("settings named no throttled service")
	}
	observeCollect("throttled_api", map[string]any{
		"retry_phase_failed": retryFailed, "full_phase_failed_units": failed, "full_phase_error_codes": codes,
		"gap_reasons": reasons, "resources_before": len(before.Resources), "resources_after": len(after.Resources),
		"settings_state": view.State, "settings_throttled_services": settingsThrottled,
	})
}

// 3. access-denied-burst: denied services named, others fine.
func accessDeniedCollectGroup(t *testing.T) {
	stack := newAWSCollectStack(t, fakeaws.ScenarioAccessDeniedBurst, awsCollectOptions{budget: awsCollectFastBudget})
	client := stack.client()
	connectCollectAccount(t, client, "Denied")
	view, _ := waitFirstSync(t, client, awsCollectAccount, 120*time.Second)

	if view.State != awssettings.SyncStatePartial {
		t.Errorf("account sync state = %s, want partial", view.State)
	}
	denied := map[string]bool{}
	for _, service := range awsCollectDenied {
		denied[service] = true
		row, ok := serviceSync(view, service)
		if !ok {
			t.Errorf("denied service %s missing from settings sync", service)
			continue
		}
		if row.State != awssettings.SyncStateFailed || len(row.Gaps) != 1 || row.Gaps[0] != string(model.GapDenied) || row.LastGoodAt != nil {
			t.Errorf("denied service %s = %+v", service, row)
		}
	}
	healthy := []string{}
	for _, row := range view.Services {
		if denied[row.Service] {
			continue
		}
		if row.State != awssettings.SyncStateOK || row.LastGoodAt == nil {
			t.Errorf("service %s = %+v, want ok", row.Service, row)
		}
		healthy = append(healthy, row.Service)
	}
	if elb, _ := serviceSync(view, "elbv2"); !containsStringValue(elb.Gaps, string(model.GapDenied)) {
		t.Errorf("elbv2 detail denial not named: %+v", elb)
	}

	ctx := context.Background()
	lambda, err := stack.snapshots.Resources(ctx, awssnapshot.Query{Account: awsCollectAccount, Service: "lambda"})
	if err != nil {
		t.Fatal(err)
	}
	if len(lambda.Resources) != 0 || len(lambda.Gaps) != 1 || lambda.Gaps[0].Reason != model.GapDenied {
		t.Errorf("lambda read = %d resources, gaps %+v; want a named denied gap", len(lambda.Resources), lambda.Gaps)
	}
	ec2, _ := stack.snapshots.Resources(ctx, awssnapshot.Query{Account: awsCollectAccount, Service: "ec2"})
	if len(ec2.Resources) == 0 {
		t.Error("ec2 lost its resources next to the denied services")
	}
	settingsView, _ := readSettings(t, client)
	probeDenied := []string{}
	if len(settingsView.Accounts) == 1 {
		probeDenied = settingsView.Accounts[0].Services.DeniedServices
	}
	observeCollect("access_denied_burst", map[string]any{
		"sync_state": view.State, "denied_named": awsCollectDenied, "ok_services": healthy, "probe_denied_services": probeDenied,
	})
}

// awsSecretScan collects everything a leak could reach.
type awsSecretScan struct {
	mu    sync.Mutex
	logs  bytes.Buffer
	files int
}

func (scan *awsSecretScan) Write(data []byte) (int, error) {
	scan.mu.Lock()
	defer scan.mu.Unlock()
	return scan.logs.Write(data)
}

func (scan *awsSecretScan) logText() string {
	scan.mu.Lock()
	defer scan.mu.Unlock()
	return scan.logs.String()
}

// 4. secret-canary: no canary in API responses, logs or the store files.
func secretCanaryCollectGroup(t *testing.T) {
	scan := &awsSecretScan{}
	previousWriter := log.Writer()
	log.SetOutput(io.MultiWriter(previousWriter, scan))
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	dataDir := t.TempDir()
	if base := os.Getenv("HARNESS_DATA_DIR"); base != "" {
		dataDir = filepath.Join(base, "secret-canary-store")
	}
	stack := newAWSCollectStack(t, "secret-canary", awsCollectOptions{budget: awsCollectFastBudget, dataDir: dataDir})
	client := stack.client()
	connectCollectAccount(t, client, "Canary")
	waitFirstSync(t, client, awsCollectAccount, 120*time.Second)
	// A fully throttled pass exercises the failure and log paths too.
	stack.fake.SetThrottle(1)
	mark := time.Now()
	stack.scheduler.Trigger(awsCollectAccount)
	waitAttemptedAfter(t, stack.snapshots, awsCollectAccount, mark, 400*time.Second)
	stack.fake.SetThrottle(0)
	readSettings(t, client)

	ctx := context.Background()
	var surface bytes.Buffer
	resources, _ := stack.snapshots.Resources(ctx, awssnapshot.Query{})
	health, _ := stack.snapshots.Health(ctx, awssnapshot.Query{})
	flags, _ := stack.snapshots.SecurityFlags(ctx, awssnapshot.Query{})
	changes, _ := stack.snapshots.Changes(ctx, awssnapshot.Query{}, time.Now().Add(-48*time.Hour), time.Now())
	for _, answer := range []any{resources, health, flags, changes} {
		encoded, _ := json.Marshal(answer)
		surface.Write(encoded)
	}

	var stored bytes.Buffer
	files := 0
	err := filepath.WalkDir(dataDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files++
		stored.Write(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk store: %v", err)
	}
	if files == 0 || !bytes.Contains(stored.Bytes(), []byte(model.RedactedValue)) {
		t.Fatalf("store files=%d; canary tags were not seen, the scan proves nothing", files)
	}

	secrets := append(fakeaws.CanarySecrets(), fakeaws.WorldRedactedTagValue, fakeaws.WorldPersonalEmail)
	logs := scan.logText()
	leaks := []string{}
	for _, secret := range secrets {
		for _, exchange := range client.seen {
			if bytes.Contains(exchange.body, []byte(secret)) {
				leaks = append(leaks, "http "+exchange.method+" "+exchange.path)
			}
			for name, values := range exchange.headers {
				for _, value := range values {
					if strings.Contains(value, secret) {
						leaks = append(leaks, "http header "+name+" "+exchange.path)
					}
				}
			}
		}
		if bytes.Contains(surface.Bytes(), []byte(secret)) {
			leaks = append(leaks, "store read surface")
		}
		if bytes.Contains(stored.Bytes(), []byte(secret)) {
			leaks = append(leaks, "store files")
		}
		if strings.Contains(logs, secret) {
			leaks = append(leaks, "logs")
		}
	}
	if len(leaks) != 0 {
		t.Errorf("canary leaked through: %v", leaks)
	}
	if stack.fake.OperationCount("ec2:DescribeInstances") == 0 {
		t.Error("canary fixture was not exercised")
	}
	observeCollect("secret_canary", map[string]any{
		"http_exchanges": len(client.seen), "store_files": files, "store_bytes": stored.Len(), "log_bytes": len(logs),
		"secrets_checked": len(secrets), "leaks": leaks, "store_dir": dataDir,
	})
}

type awsBudgetPair struct {
	Requests      int     `json:"requests"`
	SpanSeconds   float64 `json:"span_seconds"`
	SustainedRPS  float64 `json:"sustained_rps"`
	Max1s         int     `json:"max_in_any_1s"`
	Max10s        int     `json:"max_in_any_10s"`
	WorstExcess   float64 `json:"worst_excess_over_bucket"`
	BucketAllowed string  `json:"bucket_rule"`
}

// 5. Budget: at the default 2 req/s, every account x region stays inside a
// 2 req/s token bucket (burst limit.Burst) for the whole observation window.
func budgetCollectGroup(t *testing.T) {
	stack := newAWSCollectStack(t, fakeaws.ScenarioLoad5k, awsCollectOptions{budget: awsCollectDefaultBudget})
	client := stack.client()
	connectCollectAccount(t, client, "Load")
	time.Sleep(40 * time.Second)

	records := stack.recorder.snapshot()
	if int64(len(records)) != stack.fake.TotalRequests() {
		t.Errorf("ledger %d requests, fixture counted %d", len(records), stack.fake.TotalRequests())
	}
	pairs := map[string][]time.Time{}
	sts := 0
	for _, record := range records {
		if record.service == "sts" {
			sts++
			continue
		}
		key := record.account + "/" + record.region
		pairs[key] = append(pairs[key], record.at)
	}
	report := map[string]awsBudgetPair{}
	rate := float64(awsCollectDefaultBudget)
	for key, times := range pairs {
		sort.Slice(times, func(left, right int) bool { return times[left].Before(times[right]) })
		entry := awsBudgetPair{Requests: len(times), BucketAllowed: fmt.Sprintf("n <= %.0f*dt + %d", rate, awslimit.Burst)}
		span := times[len(times)-1].Sub(times[0]).Seconds()
		entry.SpanSeconds = span
		if span > 0 {
			entry.SustainedRPS = float64(len(times)-1) / span
		}
		for i := range times {
			for j := i; j < len(times); j++ {
				dt := times[j].Sub(times[i])
				count := j - i + 1
				if dt < time.Second && count > entry.Max1s {
					entry.Max1s = count
				}
				if dt < 10*time.Second && count > entry.Max10s {
					entry.Max10s = count
				}
				// One extra token of slack for clock granularity between the
				// limiter and this ledger.
				if excess := float64(count) - (rate*dt.Seconds() + float64(awslimit.Burst) + 1); excess > entry.WorstExcess {
					entry.WorstExcess = excess
				}
			}
		}
		if entry.WorstExcess > 0 {
			t.Errorf("%s exceeded the 2 req/s bucket by %.1f requests: %+v", key, entry.WorstExcess, entry)
		}
		if span >= 10 && entry.SustainedRPS > rate+float64(awslimit.Burst)/span {
			t.Errorf("%s sustained %.2f req/s over %.0fs", key, entry.SustainedRPS, span)
		}
		report[key] = entry
	}
	collector, ok := report[awsCollectAccount+"/"+awsCollectRegion]
	if !ok || collector.Requests < 40 || collector.SpanSeconds < 20 {
		t.Errorf("load did not sustain long enough to measure: %+v", collector)
	}
	settingsView, _ := readSettings(t, client)
	if len(settingsView.Accounts) == 1 {
		api := settingsView.Accounts[0].APIUse
		if api.UsedRPS <= 0 || api.UsedRPS > float64(api.BudgetRPS) {
			t.Errorf("api_use meter = %+v", api)
		}
		observeCollect("budget_api_use", api)
	}
	t.Logf("budget pairs: %+v (sts %d)", report, sts)
	observeCollect("budget", map[string]any{"pairs": report, "sts_requests": sts, "total": len(records), "budget_rps": awsCollectDefaultBudget})
}

// 6. Account delete purges the inventory at once and for good; re-adding
// shows a fresh first sync with no resurrected history. It runs on the file
// store (the OSS default backend, SQLite tables) and on memory.
func deletePurgeCollectGroup(t *testing.T) {
	t.Run("file_store", func(t *testing.T) {
		dataDir := t.TempDir()
		if base := os.Getenv("HARNESS_DATA_DIR"); base != "" {
			dataDir = filepath.Join(base, "delete-purge-store")
		}
		deletePurgeOn(t, "file", dataDir)
	})
	t.Run("memory_store", func(t *testing.T) { deletePurgeOn(t, "memory", "") })
}

func deletePurgeOn(t *testing.T, backend, dataDir string) {
	stack := newAWSCollectStack(t, fakeaws.ScenarioBadDeploy, awsCollectOptions{budget: awsCollectFastBudget, dataDir: dataDir})
	client := stack.client()
	connectCollectAccount(t, client, "Production")
	first, _ := waitFirstSync(t, client, awsCollectAccount, 120*time.Second)
	ctx := context.Background()
	if resources, _ := stack.snapshots.Resources(ctx, awssnapshot.Query{Account: awsCollectAccount}); len(resources.Resources) == 0 {
		t.Fatal("nothing collected before delete")
	}
	inventoryRows := func() []string {
		inventory, ok := storage.AWSInventory(stack.provider)
		if !ok {
			t.Fatal("provider has no inventory tables")
		}
		scope := tenancy.DefaultOrgScope()
		held := []string{}
		units, err := inventory.AWSUnits(ctx, scope, storage.AWSUnitQuery{Account: awsCollectAccount})
		if err != nil {
			t.Fatalf("units: %v", err)
		}
		for _, unit := range units {
			held = append(held, "unit "+unit.Unit.Region+"/"+unit.Unit.Service)
		}
		resources, _ := inventory.AWSResources(ctx, scope, storage.AWSResourceQuery{Account: awsCollectAccount})
		for _, resource := range resources {
			held = append(held, "resource "+resource.ARN)
		}
		changes, _ := inventory.AWSChanges(ctx, scope, storage.AWSChangeQuery{Account: awsCollectAccount})
		for _, change := range changes {
			held = append(held, "change "+change.EventID)
		}
		coverage, _ := inventory.AWSChangeCoverage(ctx, scope, awsCollectAccount, "")
		for _, pair := range coverage {
			held = append(held, "coverage "+pair.Region)
		}
		return held
	}
	if len(inventoryRows()) == 0 {
		t.Fatal("no inventory rows before delete")
	}

	response, body := client.do(http.MethodDelete, "/api/admin/aws/accounts/"+awsCollectAccount, "")
	if response.StatusCode != fiber.StatusOK || string(body) != `{}` {
		t.Fatalf("delete = %d %s", response.StatusCode, body)
	}
	deleted := time.Now()
	leftovers := map[string]any{}
	assertPurged := func(when string) {
		t.Helper()
		left := inventoryRows()
		settingsAfter, _ := readSettings(t, client)
		leftovers[when] = map[string]int{"inventory_rows": len(left), "settings_accounts": len(settingsAfter.Accounts)}
		if len(left) != 0 {
			t.Errorf("%s: %d inventory rows remain, e.g. %v", when, len(left), left[:min(3, len(left))])
		}
		accounts, _ := stack.snapshots.Accounts()
		if containsStringValue(accounts, awsCollectAccount) {
			t.Errorf("%s: store still lists the account", when)
		}
		statuses, _ := stack.snapshots.Sync(ctx, awssnapshot.Query{Account: awsCollectAccount})
		resources, _ := stack.snapshots.Resources(ctx, awssnapshot.Query{Account: awsCollectAccount})
		changes, _ := stack.snapshots.Changes(ctx, awssnapshot.Query{Account: awsCollectAccount}, time.Now().Add(-time.Hour), time.Now())
		if len(statuses) != 0 || len(resources.Resources) != 0 || len(changes.Changes) != 0 {
			t.Errorf("%s: units=%d resources=%d changes=%d", when, len(statuses), len(resources.Resources), len(changes.Changes))
		}
		if view, _ := readSettings(t, client); len(view.Accounts) != 0 {
			t.Errorf("%s: settings still lists %d accounts", when, len(view.Accounts))
		}
	}
	assertPurged("right after delete")
	// One full scheduler tick: an in-flight or next pass must not resurrect it.
	time.Sleep(11 * time.Second)
	assertPurged("after a scheduler tick")

	readded := connectCollectAccount(t, client, "Production again")
	again, seen := waitFirstSync(t, client, awsCollectAccount, 120*time.Second)
	if len(seen) == 0 || !seen[0].FirstSync {
		t.Errorf("re-add did not start as a first sync: %+v", seen)
	}
	if again.SyncedAt == nil || again.SyncedAt.Before(readded) {
		t.Errorf("re-added synced_at = %v, re-add at %s", again.SyncedAt, readded)
	}
	statuses, _ := stack.snapshots.Sync(ctx, awssnapshot.Query{Account: awsCollectAccount})
	for _, status := range statuses {
		if status.CommittedAt.Before(readded) && !status.CommittedAt.IsZero() {
			t.Errorf("unit %s carries a pre-delete generation committed %s", status.Unit.Key(), status.CommittedAt)
		}
	}
	gap, _ := stack.snapshots.Resources(ctx, awssnapshot.Query{Account: awsCollectAccount, AsOf: deleted.Add(time.Second)})
	if len(gap.Resources) != 0 {
		t.Errorf("as_of between delete and re-add returned %d resources (history resurrected)", len(gap.Resources))
	}
	observeCollect("delete_purge_"+backend, map[string]any{
		"first_synced_at": first.SyncedAt, "readded_synced_at": again.SyncedAt, "readd_progress": seen, "after_delete": leftovers,
	})
}
