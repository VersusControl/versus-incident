package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	"github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

const (
	org     = "default"
	account = "111122223333"
	// testRoleARN and testExternalID stand in for a connected wizard account;
	// neither may reach the model.
	testRoleARN    = "arn:aws:iam::111122223333:role/versus-reader-secret-path"
	testExternalID = "vrs-ext-modelmustnotseethis"
)

var fixedNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func viewerCtx() context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true,
		Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
}

type harness struct {
	service   *settings.Service
	store     *settings.Store
	snapshots *snapshot.Store
	fake      *fakeaws.Server
	baseCalls *atomic.Int64
}

func newHarness(t *testing.T, scenario string, accounts ...config.AWSAccountConfig) harness {
	t.Helper()
	fake := fakeaws.New(t, scenario)
	provider := storage.NewMemory()
	store := settings.NewStore(provider, tenancy.DefaultOrgScope())
	snapshots := snapshot.New(provider, tenancy.DefaultOrgScope())
	calls := &atomic.Int64{}
	service := settings.NewService(store,
		settings.WithConfig(func() config.AWSConnectorConfig {
			return config.AWSConnectorConfig{Enable: true, HealthCheckInterval: "5m", Accounts: accounts,
				Budgets: config.AWSBudgetsConfig{MaxRequestsPerSecond: 2}, Collect: config.AWSCollectConfig{Enable: true}}
		}),
		settings.WithHTTPClient(fake.HTTPClient()),
		settings.WithBaseProvider(func(string) (sdkaws.CredentialsProvider, error) {
			calls.Add(1)
			return credentials.NewStaticCredentialsProvider("AKIDTOOLTEST", "secret", "session-"+account), nil
		}),
		settings.WithLimiter(limit.NewManager(2)),
		settings.WithSnapshots(snapshots),
		settings.WithClock(func() time.Time { return fixedNow }))
	return harness{service: service, store: store, snapshots: snapshots, fake: fake, baseCalls: calls}
}

// connect stores one wizard account with health: two denied services, one
// region not enabled.
func (h harness) connect(t *testing.T, accountID, name string) {
	t.Helper()
	h.connectWith(t, accountID, name, false)
}

// connectWith stores an account; a healthy one has no denied service and
// every selected region enabled.
func (h harness) connectWith(t *testing.T, accountID, name string, healthy bool) {
	t.Helper()
	record := settings.AccountRecord{AccountID: accountID, DisplayName: name, RoleARN: testRoleARN, ExternalID: testExternalID,
		Regions: []string{"us-east-1", "ap-southeast-1"}, Source: settings.SourceWizard, CreatedAt: fixedNow}
	if err := h.store.SaveAccount(org, record); err != nil {
		t.Fatal(err)
	}
	health := settings.HealthRecord{AccountID: accountID, Status: settings.StatusConnected, AttemptedAt: fixedNow, AttemptOK: true, CheckedAt: fixedNow.Add(-time.Minute),
		Regions: []settings.RegionStatus{{Region: "us-east-1", Enabled: true}, {Region: "ap-southeast-1", Enabled: false}},
		ProbeResults: []probe.Result{
			{Service: "ec2", Status: probe.StatusReadable, Items: 3},
			{Service: "elasticache", Status: probe.StatusDenied, ErrorCode: probe.CodeDenied},
			{Service: "s3", Status: probe.StatusDenied, ErrorCode: probe.CodeDenied},
			{Service: "aws-health", Status: probe.StatusNotOnPlan, ErrorCode: probe.CodeNotOnPlan},
		}}
	if healthy {
		record.Regions = []string{"us-east-1"}
		if err := h.store.SaveAccount(org, record); err != nil {
			t.Fatal(err)
		}
		health.Regions = health.Regions[:1]
		health.ProbeResults = nil
		for _, entry := range catalog.Services() {
			health.ProbeResults = append(health.ProbeResults, probe.Result{Service: entry.ID, Status: probe.StatusReadable, Items: 1})
		}
	}
	if err := h.store.SaveHealth(org, health); err != nil {
		t.Fatal(err)
	}
}

func invoke(t *testing.T, h harness, args string) (map[string]any, []byte) {
	t.Helper()
	tools := New(func() *settings.Service { return h.service }, WithClock(func() time.Time { return fixedNow }))
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	result, err := tools[0].Invoke(viewerCtx(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded, encoded
}

// TestAWSSpecMatchesRegistry: one spec table drives the tools; the registry
// catalog and the aws toolset list exactly the same names.
func TestAWSSpecMatchesRegistry(t *testing.T) {
	var catalog []string
	for _, m := range aitools.Catalog() {
		if m.Group == aitools.GroupAWS {
			catalog = append(catalog, m.Name)
			if m.DisplayName != displayNames[m.Name] || m.UIPath != settingsUI {
				t.Errorf("%s metadata %+v", m.Name, m)
			}
		}
	}
	if !reflect.DeepEqual(catalog, toolNames) {
		t.Fatalf("catalog %v, spec %v", catalog, toolNames)
	}
	found := false
	for _, ts := range aitools.Toolsets() {
		if ts.ID == "aws" {
			found = true
			if !reflect.DeepEqual(ts.ToolNames, toolNames) || ts.Permission != core.PermissionInfrastructureView {
				t.Fatalf("toolset %+v", ts)
			}
		}
	}
	if !found {
		t.Fatal("no aws toolset")
	}
	for _, tl := range New(func() *settings.Service { return nil }) {
		if tl.Description() != descriptions[tl.Name()] || tl.ArgsSchema()["additionalProperties"] != false {
			t.Fatalf("%s schema %+v", tl.Name(), tl.ArgsSchema())
		}
	}
	if New(nil) != nil {
		t.Fatal("nil source built tools")
	}
}

// TestAWSToolsHaveNoActTools: the aws toolset only reads. Writes stay in
// the admin UI and AWS changes are never a tool, so the toolset is safe for
// Analyze and MCP.
func TestAWSToolsHaveNoActTools(t *testing.T) {
	for _, ts := range aitools.Toolsets() {
		if ts.ID != "aws" {
			continue
		}
		for _, name := range ts.ToolNames {
			if !strings.HasPrefix(name, "get_") && !strings.HasPrefix(name, "list_") && !strings.HasPrefix(name, "search_") && !strings.HasPrefix(name, "investigate_") {
				t.Errorf("%s is not a read tool", name)
			}
			for _, verb := range []string{"propose", "act", "connect", "verify", "delete", "recheck", "set_", "put_", "rename"} {
				if strings.Contains(name, verb+"_") || name == verb {
					t.Errorf("%s looks like a write", name)
				}
			}
		}
	}
	for _, m := range aitools.Catalog() {
		if m.Group == aitools.GroupAWS && (m.Requirement.Kind != aitools.RequirementCapability || m.Requirement.Capabilities[0] != "aws_connector") {
			t.Errorf("%s requirement %+v", m.Name, m.Requirement)
		}
	}
}

// TestAWSToolsetDescriptionBudget keeps the model-visible text small.
func TestAWSToolsetDescriptionBudget(t *testing.T) {
	total := 0
	for _, spec := range specs {
		schema, _ := json.Marshal(spec.Properties)
		total += len(spec.Description) + len(schema)
		if len(spec.Description) > 320 {
			t.Errorf("%s description has %d characters", spec.Name, len(spec.Description))
		}
	}
	if total > 3500 {
		t.Fatalf("aws toolset descriptions and schemas total %d bytes", total)
	}
}

// TestToolsNeverCallAWS: the tool reads stored state; no request reaches
// AWS, not even the server identity lookup the settings page makes.
func TestToolsNeverCallAWS(t *testing.T) {
	h := newHarness(t, "new-account-connect")
	h.connect(t, account, "Production")
	_, _ = invoke(t, h, `{}`)
	_, _ = invoke(t, h, `{"account_id":"`+account+`"}`)
	if requests := h.fake.TotalRequests(); requests != 0 {
		t.Fatalf("tool made %d AWS requests", requests)
	}
	if calls := h.baseCalls.Load(); calls != 0 {
		t.Fatalf("tool resolved server credentials %d times", calls)
	}
}

// TestToolResultsCarrySyncAndRef: freshness, refs and links are present and
// gaps are named, never zero.
func TestToolResultsCarrySyncAndRef(t *testing.T) {
	h := newHarness(t, "new-account-connect")
	h.connect(t, account, "Production")
	units := []model.Unit{{Account: account, Region: "us-east-1", Service: "ec2"}, {Account: account, Region: "us-east-1", Service: "lambda"}}
	h.snapshots.RecordPending(units)
	h.snapshots.Commit(units[0], snapshot.Candidate{})
	h.snapshots.RecordFailure(units[1], "throttled")

	data, _ := invoke(t, h, `{}`)
	if data["ref"] != connectionRef || data["links"].(map[string]any)["ui"] != settingsUI {
		t.Fatalf("ref/links = %v %v", data["ref"], data["links"])
	}
	sync := data["sync"].(map[string]any)
	for _, key := range []string{"state", "age_s", "partial", "gaps"} {
		if _, ok := sync[key]; !ok {
			t.Fatalf("sync lacks %s: %v", key, sync)
		}
	}
	if sync["partial"] != true || fmt.Sprint(sync["gaps"]) != "[throttled]" {
		t.Fatalf("sync = %v", sync)
	}
	accounts := data["accounts"].([]any)
	first := accounts[0].(map[string]any)
	if first["ref"] != "aws://"+account || first["links"].(map[string]any)["ui"] != settingsUI || first["name"] != "Production" || first["status"] != settings.StatusConnected {
		t.Fatalf("account = %v", first)
	}
	denied := first["denied"].([]any)
	if len(denied) != 2 || len(denied[0].(map[string]any)["fix_actions"].([]any)) == 0 {
		t.Fatalf("denied = %v", denied)
	}
	if fmt.Sprint(first["regions_not_enabled"]) != "[ap-southeast-1]" || fmt.Sprint(first["not_on_plan"]) != "[aws-health]" {
		t.Fatalf("regions/not on plan = %v / %v", first["regions_not_enabled"], first["not_on_plan"])
	}
	accountSync := first["sync"].(map[string]any)
	if accountSync["age_s"] == nil || accountSync["progress"] != "2/2" || len(accountSync["gaps"].([]any)) != 1 {
		t.Fatalf("account sync = %v", accountSync)
	}

	filtered, _ := invoke(t, h, `{"account_id":"444455556666"}`)
	if filtered["accounts_total"] != float64(0) || filtered["hint"] == nil {
		t.Fatalf("filter = %v", filtered)
	}
}

func TestToolRefusesAndExplains(t *testing.T) {
	h := newHarness(t, "new-account-connect")
	tool := New(func() *settings.Service { return h.service })[0]
	result, err := tool.Invoke(context.Background(), nil)
	if err != nil || result.Found || !strings.Contains(result.Reason, "permission") {
		t.Fatalf("no permission = %+v, %v", result, err)
	}
	if _, err := tool.Invoke(viewerCtx(), json.RawMessage(`{"account_id":"nope"}`)); err == nil {
		t.Fatal("bad account accepted")
	}
	if _, err := tool.Invoke(viewerCtx(), json.RawMessage(`{"other":1}`)); err == nil {
		t.Fatal("unknown argument accepted")
	}
	off := New(func() *settings.Service { return nil })[0]
	if result, _ := off.Invoke(viewerCtx(), nil); result.Found {
		t.Fatal("unset connector reported found")
	}
	disabled := settings.NewService(settings.NewStore(storage.NewMemory(), tenancy.DefaultOrgScope()))
	data, _ := json.Marshal(ConnectionView(settings.SettingsView{}, "", 0, fixedNow))
	if !strings.Contains(string(data), `"enabled":false`) || !strings.Contains(string(data), settingsUI) {
		t.Fatalf("disabled view = %s", data)
	}
	if result, err := New(func() *settings.Service { return disabled })[0].Invoke(viewerCtx(), nil); err != nil || result.Found {
		t.Fatalf("disabled connector = %+v, %v", result, err)
	}
}

// TestEveryAWSToolModelViewUnderBudget fills the largest connection the
// config allows (64 accounts, each with denied services, missing regions and
// gaps) next to a load-5k account sync: the view stays within 6 KiB, valid
// JSON, and names the accounts it left out.
func TestEveryAWSToolModelViewUnderBudget(t *testing.T) {
	for _, size := range []int{fakeaws.DefaultLoadSize, 50000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			if size > fakeaws.DefaultLoadSize && testing.Short() {
				t.Skip("large-account load")
			}
			h := newHarness(t, fakeaws.ScenarioLoad5k)
			h.fake.SetLoadSize(size)
			collector := collect.New(h.snapshots, limit.NewManager(1000), collect.WithHTTPClient(h.fake.HTTPClient()), collect.WithRetries(0))
			if _, err := collector.Sync(context.Background(), collect.Target{AccountID: account, Regions: []string{"us-east-1", "eu-west-1"},
				Credentials: credentials.NewStaticCredentialsProvider("AKIDLOAD", "secret", "session-"+account)}); err != nil {
				t.Fatal(err)
			}
			for index := 0; index < 64; index++ {
				id := fmt.Sprintf("%012d", 100000000000+index)
				if index == 0 {
					id = account
				}
				h.connect(t, id, fmt.Sprintf("Account number %02d with a long display name", index))
			}
			data, encoded := invoke(t, h, `{}`)
			if len(encoded) > MaxModelViewBytes || !json.Valid(encoded) {
				t.Fatalf("view is %d bytes", len(encoded))
			}
			if data["accounts_total"] != float64(64) || data["truncated"] != true || data["omitted_accounts"] == nil || data["next"] == nil {
				t.Fatalf("truncation not named: total=%v truncated=%v omitted=%v", data["accounts_total"], data["truncated"], data["omitted_accounts"])
			}
			one, encoded := invoke(t, h, `{"account_id":"`+account+`"}`)
			if len(encoded) > MaxModelViewBytes || len(one["accounts"].([]any)) != 1 {
				t.Fatalf("single account view = %d bytes, %v", len(encoded), one["accounts_total"])
			}
		})
	}
}

// TestToolOutputCarriesNoSecrets: after a secret-canary sync, with a stored
// wizard account (role ARN, external ID) and an open draft, the tool output
// holds none of them, no Versus principal and no canary.
func TestToolOutputCarriesNoSecrets(t *testing.T) {
	h := newHarness(t, "secret-canary")
	h.connect(t, account, "Production")
	if err := h.store.SaveDraft(org, settings.Draft{ID: "0123456789abcdef0123456789abcdef", Method: "role_arn", RoleARN: testRoleARN,
		ExternalID: testExternalID, PrincipalARN: "arn:aws:iam::999988887777:role/versus-server", CreatedAt: fixedNow, ExpiresAt: fixedNow.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	collector := collect.New(h.snapshots, limit.NewManager(1000), collect.WithHTTPClient(h.fake.HTTPClient()), collect.WithRetries(0))
	if _, err := collector.Sync(context.Background(), collect.Target{AccountID: account, Regions: []string{"us-east-1"},
		Credentials: credentials.NewStaticCredentialsProvider("AKIDCANARY", "secret", "session-"+account)}); err != nil {
		t.Fatal(err)
	}
	_, encoded := invoke(t, h, `{}`)
	forbidden := append([]string{testRoleARN, testExternalID, "vrs-ext-", "versus-server", "role/", "principal", "external_id", "draft", "self_test"}, fakeaws.CanarySecrets()...)
	for _, secret := range forbidden {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("tool output carries %q: %s", secret, encoded)
		}
	}
}

// TestCutShortViewNeverDropsAProblemAccount connects 64 accounts, the ten
// with problems sorting last by ID. The cut-short view lists every problem
// account before any healthy one, names every omitted account, and paging
// with next.offset reaches all 64.
func TestCutShortViewNeverDropsAProblemAccount(t *testing.T) {
	h := newHarness(t, "new-account-connect")
	problem := map[string]bool{}
	for index := 0; index < 64; index++ {
		id := fmt.Sprintf("%012d", 100000000000+index)
		unhealthy := index >= 54
		problem[id] = unhealthy
		h.connectWith(t, id, fmt.Sprintf("Account number %02d with a long display name", index), !unhealthy)
	}
	seen := map[string]bool{}
	args := `{}`
	for page := 0; page < 64; page++ {
		data, encoded := invoke(t, h, args)
		if len(encoded) > MaxModelViewBytes {
			t.Fatalf("page %d is %d bytes", page, len(encoded))
		}
		shown := data["accounts"].([]any)
		healthySeen := false
		for _, item := range shown {
			id := item.(map[string]any)["account_id"].(string)
			if problem[id] && healthySeen {
				t.Fatalf("problem account %s listed after a healthy one", id)
			}
			healthySeen = healthySeen || !problem[id]
			seen[id] = true
		}
		if healthySeen {
			for id, bad := range problem {
				if bad && !seen[id] {
					t.Fatalf("problem account %s was cut while healthy ones were shown", id)
				}
			}
		}
		next, more := data["next"].(map[string]any)
		if !more {
			break
		}
		omitted := data["omitted_account_ids"].([]any)
		if len(omitted) != int(data["omitted_accounts"].(float64)) {
			t.Fatalf("omitted ids %d, omitted count %v", len(omitted), data["omitted_accounts"])
		}
		args = fmt.Sprintf(`{"offset":%d}`, int(next["offset"].(float64)))
	}
	if len(seen) != 64 {
		t.Fatalf("paging reached %d of 64 accounts", len(seen))
	}
}

// TestUnknownAccountIsNotFound: asking for an account that is not connected
// answers found=false with a hint, not an empty success.
func TestUnknownAccountIsNotFound(t *testing.T) {
	h := newHarness(t, "new-account-connect")
	h.connect(t, account, "Production")
	tool := New(func() *settings.Service { return h.service }, WithClock(func() time.Time { return fixedNow }))[0]
	result, err := tool.Invoke(viewerCtx(), json.RawMessage(`{"account_id":"444455556666"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result.Data)
	if result.Found || !strings.Contains(string(encoded), "not connected; see /aws/settings") {
		t.Fatalf("unknown account = found %v, %s", result.Found, encoded)
	}
	result, _ = tool.Invoke(viewerCtx(), json.RawMessage(`{"account_id":"`+account+`"}`))
	if !result.Found {
		t.Fatal("connected account not found")
	}
	if _, err := tool.Invoke(viewerCtx(), json.RawMessage(`{"offset":-1}`)); err == nil {
		t.Fatal("negative offset accepted")
	}
}
