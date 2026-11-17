package controllers

// aws_settings_stack_test.go — the integration gate for the AWS Settings
// slice. It boots the controller router with the PRODUCTION middleware
// chain (auditAWSSettings -> adminGatewayGuard -> requirePermission ->
// requireInfrastructureView -> requireSettingsWrite) and injects
// tests/fakeaws only through the product's existing HTTPClient seam. Handlers
// are never mocked: every assertion reads the real HTTP answer.
//
// Identity is supplied the same way the licensed build supplies it: an auth
// adapter over the OSS extension seam sets the request org and the two
// infrastructure permissions. No permission is granted to a viewer.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	awslimit "github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"

	"github.com/gofiber/fiber/v2"
)

const (
	awsStackSecret = "aws-settings-stack-gateway-secret"
	awsStackOrigin = "http://localhost:3000"
	awsStackBudget = 100
)

// awsStackIdentities is the test identity provider behind the OSS seam. It
// stands in for the licensed SSO + RBAC adapter, never for a handler.
var awsStackIdentities = map[string]struct {
	org  string
	role string
}{
	"org-a-owner":  {org: "org-a", role: "owner"},
	"org-b-owner":  {org: "org-b", role: "owner"},
	"org-a-viewer": {org: "org-a", role: "viewer"},
}

func awsStackIdentityMiddleware(ctx *fiber.Ctx) error {
	token := ctx.Get("X-Org-Token")
	if token == "" {
		return ctx.Next()
	}
	identity, ok := awsStackIdentities[token]
	if !ok {
		return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	middleware.MarkAuthorized(ctx)
	middleware.SetRequestActor(ctx, identity.org+"/"+identity.role)
	middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
	middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite), identity.role == "owner")
	return ctx.Next()
}

func awsStackBase(string) (aws.CredentialsProvider, error) {
	return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
		"ASIAVERSUSBASE00001", "versus-stack-base-secret", "")), nil
}

// assumeErrorTransport selects one assume-role failure per request. It only
// changes the fixture's answer; the product's assume path is untouched.
type assumeErrorTransport struct {
	next http.RoundTripper
	code string
}

func (transport assumeErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set(fakeaws.AssumeErrorHeader, transport.code)
	return transport.next.RoundTrip(clone)
}

type awsStackOptions struct {
	enabled     bool
	accounts    []config.AWSAccountConfig
	scope       tenancy.OrgScope
	assumeError string
	identities  bool
	start       bool
}

type awsStack struct {
	t       *testing.T
	app     *fiber.App
	service *awssettings.Service
	fake    *fakeaws.Server

	cfgMu sync.Mutex
	cfg   config.AWSConnectorConfig
}

func newAWSStack(t *testing.T, scenario string, options awsStackOptions) *awsStack {
	t.Helper()
	middleware.SetGatewayAuthEnabled(true)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(true) })
	pinGatewaySecret(t, awsStackSecret)

	stack := &awsStack{t: t, fake: fakeaws.New(t, scenario)}
	stack.cfg = config.AWSConnectorConfig{
		Enable:              options.enabled,
		HealthCheckInterval: "5m",
		Accounts:            append([]config.AWSAccountConfig(nil), options.accounts...),
		Budgets:             config.AWSBudgetsConfig{MaxRequestsPerSecond: awsStackBudget},
	}
	scope := options.scope
	if scope.Write == "" {
		scope = tenancy.DefaultOrgScope()
	}

	client := stack.fake.HTTPClient()
	if options.assumeError != "" {
		client = &http.Client{Transport: assumeErrorTransport{next: client.Transport, code: options.assumeError}}
	}
	stack.service = awssettings.NewService(
		awssettings.NewStore(storage.NewMemory(), scope),
		awssettings.WithConfig(stack.readConfig),
		awssettings.WithHTTPClient(client),
		awssettings.WithBaseProvider(awsStackBase),
		awssettings.WithLimiter(awslimit.NewManager(awsStackBudget)),
	)

	stack.app = fiber.New(fiber.Config{Immutable: true})
	stack.app.Use(middleware.OrgInjector())
	if options.identities {
		middleware.SetOrgResolver(func(ctx *fiber.Ctx) string {
			if identity, ok := awsStackIdentities[ctx.Get("X-Org-Token")]; ok {
				return identity.org
			}
			return ""
		})
		t.Cleanup(func() { middleware.SetOrgResolver(nil) })
		stack.app.Use(awsStackIdentityMiddleware)
	}
	api := stack.app.Group("/api")
	RegisterGatewaySessionRoutes(api)
	NewAWSSettingsController(stack.service).Register(api)

	if options.start {
		ctx, cancel := context.WithCancel(context.Background())
		stack.service.Start(ctx)
		t.Cleanup(func() {
			cancel()
			select {
			case <-stack.service.Stopped():
			case <-time.After(10 * time.Second):
				t.Errorf("aws settings service did not stop")
			}
		})
	}
	return stack
}

func (stack *awsStack) readConfig() config.AWSConnectorConfig {
	stack.cfgMu.Lock()
	defer stack.cfgMu.Unlock()
	return stack.cfg
}

func (stack *awsStack) setAccounts(accounts []config.AWSAccountConfig) {
	stack.cfgMu.Lock()
	defer stack.cfgMu.Unlock()
	stack.cfg.Accounts = append([]config.AWSAccountConfig(nil), accounts...)
}

type awsClient struct {
	t      *testing.T
	app    *fiber.App
	cookie *http.Cookie
	token  string
	seen   []awsExchange
}

type awsExchange struct {
	method  string
	path    string
	status  int
	headers http.Header
	body    []byte
}

func (client *awsClient) do(method, path, body string) (*http.Response, []byte) {
	client.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Host = "localhost:3000"
	if body != "" {
		request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Origin", awsStackOrigin)
	}
	if client.cookie != nil {
		request.AddCookie(client.cookie)
	}
	if client.token != "" {
		request.Header.Set("X-Org-Token", client.token)
	}
	response, err := client.app.Test(request, -1)
	if err != nil {
		client.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		client.t.Fatalf("%s %s read: %v", method, path, err)
	}
	client.seen = append(client.seen, awsExchange{method: method, path: path, status: response.StatusCode, headers: response.Header.Clone(), body: buffer.Bytes()})
	return response, buffer.Bytes()
}

func (client *awsClient) gatewaySession(secret string) {
	client.t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/gateway-session", nil)
	request.Host = "localhost:3000"
	request.Header.Set("X-Gateway-Secret", secret)
	response, err := client.app.Test(request, -1)
	if err != nil {
		client.t.Fatalf("gateway exchange: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != fiber.StatusNoContent {
		client.t.Fatalf("gateway exchange status = %d", response.StatusCode)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		client.t.Fatalf("gateway exchange cookies = %d", len(cookies))
	}
	client.cookie = cookies[0]
}

func decodeStackJSON(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	if !json.Valid(body) {
		t.Fatalf("body is not JSON: %s", body)
	}
	return json.RawMessage(body)
}

func waitStackDraft(t *testing.T, client *awsClient, draftID string, deadline time.Duration, assertProgressive bool) awssettings.DraftView {
	t.Helper()
	var view awssettings.DraftView
	previous := -1
	partial := false
	stop := time.Now().Add(deadline)
	for {
		response, body := client.do(http.MethodGet, "/api/admin/aws/connect/drafts/"+draftID, "")
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("draft read = %d %s", response.StatusCode, body)
		}
		decodeStackJSON(t, body)
		if err := json.Unmarshal(body, &view); err != nil {
			t.Fatalf("draft decode: %v", err)
		}
		if assertProgressive && previous >= 0 && len(view.Services) < previous {
			t.Fatalf("progressive draft regressed: %d -> %d", previous, len(view.Services))
		}
		previous = len(view.Services)
		if assertProgressive && view.Status == awssettings.DraftVerifying && len(view.Services) > 0 && len(view.Services) < view.Counts.Total {
			partial = true
		}
		if view.Status == awssettings.DraftVerified {
			t.Logf("draft %s verified; progressive partial grid observed=%v", draftID, partial)
			return view
		}
		if time.Now().After(stop) {
			t.Fatalf("draft %s did not verify: %+v", draftID, view)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startStackDraft(t *testing.T, client *awsClient) awssettings.DraftStartView {
	t.Helper()
	response, body := client.do(http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft create = %d %s", response.StatusCode, body)
	}
	var start awssettings.DraftStartView
	if err := json.Unmarshal(body, &start); err != nil {
		t.Fatalf("draft create decode: %v", err)
	}
	if start.DraftID == "" || start.ExternalIDMasked == "" || start.TemplateURL == "" || start.TerraformSnippet == "" {
		t.Fatalf("draft create view = %+v", start)
	}
	return start
}

func verifyStackDraft(t *testing.T, client *awsClient, draftID, roleARN string) {
	t.Helper()
	payload := `{"role_arn":` + strconvQuote(roleARN) + `}`
	response, body := client.do(http.MethodPost, "/api/admin/aws/connect/drafts/"+draftID+"/verify", payload)
	if response.StatusCode != fiber.StatusAccepted {
		t.Fatalf("verify = %d %s", response.StatusCode, body)
	}
	var accepted struct {
		ProbeID string `json:"probe_id"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.ProbeID == "" {
		t.Fatalf("verify answer = %s (%v)", body, err)
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func commitStackAccount(t *testing.T, client *awsClient, draftID, name string, regions []string) awssettings.AccountView {
	t.Helper()
	payload := `{"draft_id":` + strconvQuote(draftID) + `,"display_name":` + strconvQuote(name) + `,"regions":["` + strings.Join(regions, `","`) + `"]}`
	response, body := client.do(http.MethodPost, "/api/admin/aws/accounts", payload)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("commit = %d %s", response.StatusCode, body)
	}
	var account awssettings.AccountView
	if err := json.Unmarshal(body, &account); err != nil {
		t.Fatalf("commit decode: %v", err)
	}
	return account
}

func readSettings(t *testing.T, client *awsClient) (awssettings.SettingsView, []byte) {
	t.Helper()
	response, body := client.do(http.MethodGet, "/api/admin/aws/settings", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("settings = %d %s", response.StatusCode, body)
	}
	var view awssettings.SettingsView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("settings decode: %v", err)
	}
	return view, body
}

func connectAccount(t *testing.T, client *awsClient) awssettings.DraftStartView {
	t.Helper()
	start := startStackDraft(t, client)
	verifyStackDraft(t, client, start.DraftID, "arn:aws:iam::444455556666:role/versus-reader")
	waitStackDraft(t, client, start.DraftID, 30*time.Second, true)
	return start
}

// TestAWSSettingsStackE2E is the integration verdict. Each group is one
// acceptance area; the run writes results/aws-settings-verdict.json when the
// harness supplies HARNESS_RESULTS_DIR.
func TestAWSSettingsStackE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("integration suite needs the fakeaws HTTP stack")
	}
	groups := []struct {
		name string
		run  func(*testing.T)
	}{
		{"connect_end_to_end", connectStackGroup},
		{"assume_denied", assumeDeniedStackGroup},
		{"throttled_api", throttledStackGroup},
		{"secret_canary", secretCanaryStackGroup},
		{"config_wins_merge", configWinsStackGroup},
		{"org_isolation", orgIsolationStackGroup},
		{"viewer_read_only", viewerStackGroup},
		{"verify_rate_limit", rateLimitStackGroup},
		{"connector_disabled", disabledStackGroup},
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
	writeStackVerdict(t, results)
}

func writeStackVerdict(t *testing.T, groups []map[string]string) {
	t.Helper()
	dir := os.Getenv("HARNESS_RESULTS_DIR")
	if dir == "" {
		return
	}
	payload := map[string]any{
		"suite":   "aws-settings",
		"task":    os.Getenv("HARNESS_TASK"),
		"run_id":  os.Getenv("HARNESS_RUN_ID"),
		"backend": "tests/fakeaws via the product HTTPClient seam (fixture, not live AWS)",
		"groups":  groups,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("verdict encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws-settings-verdict.json"), encoded, 0o600); err != nil {
		t.Errorf("write verdict: %v", err)
	}
}

func connectStackGroup(t *testing.T) {
	stack := newAWSStack(t, "new-account-connect", awsStackOptions{enabled: true, start: true})
	client := &awsClient{t: t, app: stack.app}
	client.gatewaySession(awsStackSecret)

	start := startStackDraft(t, client)
	response, revealedBody := client.do(http.MethodGet, "/api/admin/aws/connect/drafts/"+start.DraftID+"/external-id", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("reveal = %d %s", response.StatusCode, revealedBody)
	}
	if cache := response.Header.Get(fiber.HeaderCacheControl); cache != "no-store" {
		t.Fatalf("reveal cache-control = %q", cache)
	}
	var revealed struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(revealedBody, &revealed); err != nil || !strings.HasPrefix(revealed.ExternalID, awssettings.ExternalIDPrefix) {
		t.Fatalf("revealed external id = %s (%v)", revealedBody, err)
	}

	verifyStackDraft(t, client, start.DraftID, "arn:aws:iam::444455556666:role/versus-reader")
	// Progressive read immediately after 202: the grid may still be filling.
	response, progressBody := client.do(http.MethodGet, "/api/admin/aws/connect/drafts/"+start.DraftID, "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("progress draft = %d %s", response.StatusCode, progressBody)
	}
	decodeStackJSON(t, progressBody)
	var progress awssettings.DraftView
	_ = json.Unmarshal(progressBody, &progress)
	if progress.Counts.Total != 24 {
		t.Fatalf("progress counts.total = %d", progress.Counts.Total)
	}
	if progress.Status == awssettings.DraftVerifying && len(progress.Services) >= progress.Counts.Total {
		t.Fatalf("verifying draft already shows a full grid: %+v", progress.Counts)
	}

	view := waitStackDraft(t, client, start.DraftID, 30*time.Second, true)
	if !view.Assume.OK {
		t.Fatalf("assume failed: %+v", view.Assume)
	}
	if len(view.Services) != view.Counts.Total || view.Counts.Total != 24 {
		t.Fatalf("grid = %d rows, counts=%+v", len(view.Services), view.Counts)
	}
	if !containsStringValue(view.Counts.DeniedServices, "elasticache") {
		t.Fatalf("denied services = %v", view.Counts.DeniedServices)
	}
	if !containsStringValue(view.Counts.NotOnPlan, "aws-health") {
		t.Fatalf("not on plan = %v", view.Counts.NotOnPlan)
	}
	if view.Counts.NotChecked != 0 {
		t.Fatalf("not checked = %d %v", view.Counts.NotChecked, view.Counts.NotCheckedServices)
	}
	if !strings.Contains(view.FixPolicy, "elasticache:Describe*") {
		t.Fatalf("fix policy = %q", view.FixPolicy)
	}
	if bytes.Contains(progressBody, []byte(revealed.ExternalID)) || bytes.Contains(mustJSON(t, view), []byte(revealed.ExternalID)) {
		t.Fatal("draft carried the full external id")
	}

	response, regionsBody := client.do(http.MethodPost, "/api/admin/aws/connect/drafts/"+start.DraftID+"/regions", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("regions = %d %s", response.StatusCode, regionsBody)
	}
	var regions []awssettings.RegionStatus
	if err := json.Unmarshal(regionsBody, &regions); err != nil {
		t.Fatalf("regions decode: %v", err)
	}
	if len(regions) != 4 {
		t.Fatalf("regions = %+v", regions)
	}
	found := false
	for _, region := range regions {
		if region.ResourcesFound {
			found = true
		}
	}
	if !found {
		t.Fatalf("no region reported resources: %+v", regions)
	}

	account := commitStackAccount(t, client, start.DraftID, "Staging", []string{"us-east-1", "eu-west-1"})
	if account.AccountID != "444455556666" || account.Source != awssettings.SourceWizard {
		t.Fatalf("committed account = %+v", account)
	}
	if !containsStringValue(account.Services.DeniedServices, "elasticache") {
		t.Fatalf("committed denied = %+v", account.Services)
	}
	if account.Regions.Enabled != 2 || account.APIUse.BudgetRPS != 2*awsStackBudget {
		t.Fatalf("committed regions/api = %+v / %+v", account.Regions, account.APIUse)
	}

	settingsView, settingsBody := readSettings(t, client)
	if len(settingsView.Accounts) != 1 || settingsView.Accounts[0].AccountID != "444455556666" {
		t.Fatalf("settings accounts = %+v", settingsView.Accounts)
	}
	row := settingsView.Accounts[0]
	if row.Services.Total != 24 || row.Services.NotChecked != 0 {
		t.Fatalf("settings service counts = %+v", row.Services)
	}
	if settingsView.Privacy.SelfTest.LastRunAt.IsZero() {
		// Start() runs the self-test at boot; wait for the published record.
		deadline := time.Now().Add(10 * time.Second)
		for settingsView.Privacy.SelfTest.LastRunAt.IsZero() && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			settingsView, settingsBody = readSettings(t, client)
		}
	}
	if settingsView.Privacy.SelfTest.LastRunAt.IsZero() {
		t.Fatal("privacy self-test never published a result")
	}
	if !settingsView.Privacy.SelfTest.Passing || settingsView.Privacy.SelfTest.Leaks24h != 0 {
		t.Fatalf("privacy self-test = %+v", settingsView.Privacy.SelfTest)
	}
	if settingsView.Capabilities.Organizations || !settingsView.Capabilities.CanWrite {
		t.Fatalf("capabilities = %+v", settingsView.Capabilities)
	}
	if settingsView.Budget.Source != "config" || settingsView.Budget.BudgetRPS != awsStackBudget {
		t.Fatalf("budget = %+v", settingsView.Budget)
	}
	if settingsView.Accounts[0].APIUse.UsedRPS <= 0 {
		t.Fatalf("api_use did not reflect real calls: %+v", settingsView.Accounts[0].APIUse)
	}

	// Types the screen reads, asserted on the wire shape.
	raw := decodeAWSJSON(t, settingsBody)
	accounts := raw["accounts"].([]any)
	accountRaw := awsObject(t, accounts[0], "accounts[0]")
	servicesRaw := awsObject(t, accountRaw["services"], "services")
	for _, key := range []string{"total", "readable", "not_used", "not_checked"} {
		awsNumber(t, servicesRaw[key], "services."+key)
	}
	for _, key := range []string{"denied", "denied_services", "not_on_plan", "not_on_plan_services", "not_checked_services"} {
		awsArray(t, servicesRaw[key], "services."+key)
	}
	regionsRaw := awsObject(t, accountRaw["regions"], "regions")
	awsArray(t, regionsRaw["selected"], "regions.selected")
	awsNumber(t, regionsRaw["enabled"], "regions.enabled")
	awsArray(t, regionsRaw["not_enabled"], "regions.not_enabled")
	apiRaw := awsObject(t, accountRaw["api_use"], "api_use")
	awsNumber(t, apiRaw["used_rps"], "api_use.used_rps")
	awsNumber(t, apiRaw["budget_rps"], "api_use.budget_rps")
	budgetRaw := awsObject(t, raw["budget"], "budget")
	awsNumber(t, budgetRaw["used_rps"], "budget.used_rps")
	awsNumber(t, budgetRaw["budget_rps"], "budget.budget_rps")
	awsString(t, budgetRaw["source"], "budget.source")
	privacyRaw := awsObject(t, raw["privacy"], "privacy")
	selfTestRaw := awsObject(t, privacyRaw["self_test"], "privacy.self_test")
	if selfTestRaw["passing"] != true {
		t.Fatalf("privacy.self_test.passing = %v", selfTestRaw["passing"])
	}
	awsNumber(t, selfTestRaw["leaks_24h"], "privacy.self_test.leaks_24h")
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}

func containsStringValue(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func assumeDeniedStackGroup(t *testing.T) {
	codes := []string{
		fakeaws.AssumeTrustDenied,
		fakeaws.AssumeExternalIDMismatch,
		fakeaws.AssumeRoleNotFound,
		fakeaws.AssumeSessionDurationInvalid,
	}
	providerText := []string{
		"not authorized to perform",
		"external id is not authorized",
		"cannot be found",
		"DurationSeconds exceeds",
		"User: arn:aws:iam",
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			stack := newAWSStack(t, "assume-denied", awsStackOptions{enabled: true, assumeError: code})
			client := &awsClient{t: t, app: stack.app}
			client.gatewaySession(awsStackSecret)

			start := startStackDraft(t, client)
			verifyStackDraft(t, client, start.DraftID, "arn:aws:iam::444455556666:role/versus-reader")
			view := waitStackDraft(t, client, start.DraftID, 30*time.Second, true)
			if view.Assume.OK {
				t.Fatalf("assume unexpectedly succeeded for %s", code)
			}
			if view.Assume.ErrorCode != code {
				t.Fatalf("assume.error_code = %q, want %q", view.Assume.ErrorCode, code)
			}
			if view.Counts.NotChecked != view.Counts.Total || len(view.Counts.Denied) != 0 {
				t.Fatalf("assume failure counts = %+v", view.Counts)
			}
			_, body := client.do(http.MethodGet, "/api/admin/aws/connect/drafts/"+start.DraftID, "")
			for _, text := range providerText {
				if bytes.Contains(body, []byte(text)) {
					t.Fatalf("draft body leaked provider text %q: %s", text, body)
				}
			}
		})
	}
}

func throttledStackGroup(t *testing.T) {
	stack := newAWSStack(t, "throttled-api", awsStackOptions{enabled: true})
	client := &awsClient{t: t, app: stack.app}
	client.gatewaySession(awsStackSecret)

	start := startStackDraft(t, client)
	verifyStackDraft(t, client, start.DraftID, "arn:aws:iam::111122223333:role/versus-reader")
	view := waitStackDraft(t, client, start.DraftID, 90*time.Second, false)
	if !view.Assume.OK {
		t.Fatalf("assume failed: %+v", view.Assume)
	}
	if view.Counts.NotChecked == 0 {
		t.Fatalf("throttled run produced no not-checked cells: %+v", view.Counts)
	}
	if len(view.Counts.Denied) != 0 {
		t.Fatalf("throttled run produced a false denial: %v", view.Counts.Denied)
	}
	notUsed := 0
	for _, row := range view.Services {
		switch row.Status {
		case string(probe.StatusNotUsed):
			notUsed++
		case string(probe.StatusNotChecked):
			if row.ErrorCode != probe.CodeThrottled && row.ErrorCode != "backoff" {
				t.Fatalf("not-checked row %s has error_code %q", row.Service, row.ErrorCode)
			}
			if !row.Retryable {
				t.Fatalf("throttled row %s is not retryable", row.Service)
			}
			if row.Items != 0 {
				t.Fatalf("throttled row %s reported %d items", row.Service, row.Items)
			}
		}
	}
	if notUsed != view.Counts.NotUsed {
		t.Fatalf("not_used count %d does not match rows %d", view.Counts.NotUsed, notUsed)
	}

	commitStackAccount(t, client, start.DraftID, "Throttled", []string{"us-east-1"})
	settingsView, _ := readSettings(t, client)
	if len(settingsView.Accounts) != 1 {
		t.Fatalf("accounts = %+v", settingsView.Accounts)
	}
	row := settingsView.Accounts[0]
	if row.APIUse.UsedRPS <= 0 {
		t.Fatalf("api_use did not reflect real calls: %+v", row.APIUse)
	}
	if len(row.Services.DeniedServices) != 0 {
		t.Fatalf("settings reported a denial: %+v", row.Services)
	}
	if row.Services.NotChecked == 0 {
		t.Fatalf("settings lost the not-checked answers: %+v", row.Services)
	}
}

func secretCanaryStackGroup(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(io.MultiWriter(previousWriter, &logs))
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	stack := newAWSStack(t, "secret-canary", awsStackOptions{enabled: true})
	client := &awsClient{t: t, app: stack.app}
	client.gatewaySession(awsStackSecret)

	start := startStackDraft(t, client)
	response, revealBody := client.do(http.MethodGet, "/api/admin/aws/connect/drafts/"+start.DraftID+"/external-id", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("reveal = %d %s", response.StatusCode, revealBody)
	}
	var revealed struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(revealBody, &revealed); err != nil || !strings.HasPrefix(revealed.ExternalID, awssettings.ExternalIDPrefix) {
		t.Fatalf("reveal body = %s (%v)", revealBody, err)
	}
	verifyStackDraft(t, client, start.DraftID, "arn:aws:iam::111122223333:role/versus-reader")
	waitStackDraft(t, client, start.DraftID, 30*time.Second, true)
	if _, body := client.do(http.MethodPost, "/api/admin/aws/connect/drafts/"+start.DraftID+"/regions", ""); len(body) == 0 {
		t.Fatal("regions answer was empty")
	}
	commitStackAccount(t, client, start.DraftID, "Canary", []string{"us-east-1"})
	readSettings(t, client)
	client.do(http.MethodPut, "/api/admin/aws/settings/options", `{"optional_data":{"cost_explorer":true}}`)
	client.do(http.MethodPut, "/api/admin/aws/settings/privacy", `{"tag_value_allowlist":["app","team"]}`)

	if len(client.seen) < 8 {
		t.Fatalf("canary run exercised only %d exchanges", len(client.seen))
	}
	canaries := fakeaws.CanarySecrets()
	for _, exchange := range client.seen {
		for _, canary := range canaries {
			if bytes.Contains(exchange.body, []byte(canary)) {
				t.Fatalf("%s %s leaked canary %q: %s", exchange.method, exchange.path, canary, exchange.body)
			}
			for name, values := range exchange.headers {
				for _, value := range values {
					if strings.Contains(value, canary) {
						t.Fatalf("%s %s header %s leaked canary %q", exchange.method, exchange.path, name, canary)
					}
				}
			}
		}
	}
	for _, canary := range canaries {
		if strings.Contains(logs.String(), canary) {
			t.Fatalf("log output leaked canary %q", canary)
		}
	}
	if strings.Contains(logs.String(), revealed.ExternalID) {
		t.Fatal("log output leaked the full external id")
	}
	if stack.fake.OperationCount("ec2:DescribeInstances") == 0 {
		t.Fatal("canary fixture was not exercised")
	}
}

func configWinsStackGroup(t *testing.T) {
	stack := newAWSStack(t, "new-account-connect", awsStackOptions{enabled: true})
	client := &awsClient{t: t, app: stack.app}
	client.gatewaySession(awsStackSecret)

	start := connectAccount(t, client)
	commitStackAccount(t, client, start.DraftID, "Staging (wizard)", []string{"us-east-1", "eu-west-1"})
	view, _ := readSettings(t, client)
	if len(view.Accounts) != 1 || view.Accounts[0].Source != awssettings.SourceWizard {
		t.Fatalf("wizard row = %+v", view.Accounts)
	}

	stack.setAccounts([]config.AWSAccountConfig{{
		AccountID:   "444455556666",
		DisplayName: "Staging (YAML)",
		Regions:     []string{"us-east-1", "eu-west-1"},
		Auth:        config.AWSAuthConfig{RoleARN: "arn:aws:iam::444455556666:role/versus-reader"},
	}})
	merged, _ := readSettings(t, client)
	if len(merged.Accounts) != 1 {
		t.Fatalf("config+wizard merge produced %d rows: %+v", len(merged.Accounts), merged.Accounts)
	}
	row := merged.Accounts[0]
	if row.Source != awssettings.SourceConfig || row.DisplayName != "Staging (YAML)" {
		t.Fatalf("config did not win the merge: %+v", row)
	}
	if !row.Conflict {
		t.Fatalf("merge did not flag the config/wizard conflict: %+v", row)
	}
	if !containsStringValue(row.Services.DeniedServices, "elasticache") {
		t.Fatalf("merged row lost the health record: %+v", row.Services)
	}
	if response, body := client.do(http.MethodDelete, "/api/admin/aws/accounts/444455556666", ""); response.StatusCode != fiber.StatusConflict || !bytes.Contains(body, []byte("config_managed")) {
		t.Fatalf("config account delete = %d %s", response.StatusCode, body)
	}
}

func orgIsolationStackGroup(t *testing.T) {
	stack := newAWSStack(t, "new-account-connect", awsStackOptions{
		enabled:    true,
		scope:      tenancy.NewOrgScope("org-a", "org-b"),
		identities: true,
	})
	ownerA := &awsClient{t: t, app: stack.app, token: "org-a-owner"}
	ownerB := &awsClient{t: t, app: stack.app, token: "org-b-owner"}

	start := connectAccount(t, ownerA)
	commitStackAccount(t, ownerA, start.DraftID, "Org A account", []string{"us-east-1"})
	if view, _ := readSettings(t, ownerA); len(view.Accounts) != 1 {
		t.Fatalf("org-a accounts = %+v", view.Accounts)
	}

	viewB, _ := readSettings(t, ownerB)
	if len(viewB.Accounts) != 0 {
		t.Fatalf("org-b read %d of org-a's accounts: %+v", len(viewB.Accounts), viewB.Accounts)
	}
	for _, path := range []string{
		"/api/admin/aws/connect/drafts/" + start.DraftID,
		"/api/admin/aws/connect/drafts/" + start.DraftID + "/external-id",
	} {
		response, body := ownerB.do(http.MethodGet, path, "")
		if response.StatusCode != fiber.StatusNotFound {
			t.Fatalf("org-b %s = %d %s", path, response.StatusCode, body)
		}
	}
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/admin/aws/accounts/444455556666/recheck", ""},
		{http.MethodPatch, "/api/admin/aws/accounts/444455556666", `{"display_name":"hijacked"}`},
		{http.MethodDelete, "/api/admin/aws/accounts/444455556666", ""},
	} {
		response, body := ownerB.do(request.method, request.path, request.body)
		if response.StatusCode != fiber.StatusNotFound {
			t.Fatalf("org-b %s %s = %d %s", request.method, request.path, response.StatusCode, body)
		}
	}
	if viewA, _ := readSettings(t, ownerA); len(viewA.Accounts) != 1 || viewA.Accounts[0].DisplayName != "Org A account" {
		t.Fatalf("org-a account changed after org-b writes: %+v", viewA.Accounts)
	}
}

func viewerStackGroup(t *testing.T) {
	stack := newAWSStack(t, "new-account-connect", awsStackOptions{enabled: true, scope: tenancy.NewOrgScope("org-a"), identities: true})
	viewer := &awsClient{t: t, app: stack.app, token: "org-a-viewer"}

	view, body := readSettings(t, viewer)
	if view.Capabilities.CanWrite {
		t.Fatalf("viewer was offered write capability: %s", body)
	}
	draftID := strings.Repeat("a", 32)
	accountID := "444455556666"
	writes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`},
		{http.MethodGet, "/api/admin/aws/connect/drafts/" + draftID, ""},
		{http.MethodGet, "/api/admin/aws/connect/drafts/" + draftID + "/external-id", ""},
		{http.MethodPost, "/api/admin/aws/connect/drafts/" + draftID + "/verify", `{"role_arn":"arn:aws:iam::444455556666:role/versus-reader"}`},
		{http.MethodPost, "/api/admin/aws/connect/drafts/" + draftID + "/regions", ""},
		{http.MethodPost, "/api/admin/aws/accounts", `{"draft_id":"` + draftID + `","display_name":"x","regions":["us-east-1"]}`},
		{http.MethodPatch, "/api/admin/aws/accounts/" + accountID, `{"display_name":"x"}`},
		{http.MethodDelete, "/api/admin/aws/accounts/" + accountID, ""},
		{http.MethodPost, "/api/admin/aws/accounts/" + accountID + "/recheck", ""},
		{http.MethodPut, "/api/admin/aws/settings/options", `{"budget":{"max_requests_per_second":5}}`},
		{http.MethodPut, "/api/admin/aws/settings/privacy", `{"tag_value_allowlist":["app"]}`},
	}
	for _, request := range writes {
		response, answer := viewer.do(request.method, request.path, request.body)
		if response.StatusCode != fiber.StatusForbidden {
			t.Fatalf("viewer %s %s = %d %s", request.method, request.path, response.StatusCode, answer)
		}
	}
}

func rateLimitStackGroup(t *testing.T) {
	stack := newAWSStack(t, "new-account-connect", awsStackOptions{enabled: true})
	client := &awsClient{t: t, app: stack.app}
	client.gatewaySession(awsStackSecret)

	start := startStackDraft(t, client)
	verify := `{"role_arn":"arn:aws:iam::111122223333:role/versus-reader"}`
	for attempt := 1; attempt <= 6; attempt++ {
		response, body := client.do(http.MethodPost, "/api/admin/aws/connect/drafts/"+start.DraftID+"/verify", verify)
		if response.StatusCode != fiber.StatusAccepted {
			t.Fatalf("verify %d = %d %s", attempt, response.StatusCode, body)
		}
	}
	response, body := client.do(http.MethodPost, "/api/admin/aws/connect/drafts/"+start.DraftID+"/verify", verify)
	if response.StatusCode != fiber.StatusTooManyRequests {
		t.Fatalf("7th verify = %d %s", response.StatusCode, body)
	}
	if response.Header.Get(fiber.HeaderRetryAfter) == "" {
		t.Fatal("429 answer has no Retry-After")
	}
	if !bytes.Contains(body, []byte(`"rate_limited"`)) {
		t.Fatalf("429 body = %s", body)
	}
	waitStackDraft(t, client, start.DraftID, 30*time.Second, false)
}

func disabledStackGroup(t *testing.T) {
	stack := newAWSStack(t, "new-account-connect", awsStackOptions{enabled: false})
	client := &awsClient{t: t, app: stack.app}
	client.gatewaySession(awsStackSecret)

	view, body := readSettings(t, client)
	if view.Enabled {
		t.Fatalf("disabled connector answered enabled=true: %s", body)
	}
	raw := decodeAWSJSON(t, body)
	if raw["enabled"] != false {
		t.Fatalf("enabled = %v", raw["enabled"])
	}
	response, answer := client.do(http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`)
	if response.StatusCode != fiber.StatusConflict || !bytes.Contains(answer, []byte(`"disabled"`)) {
		t.Fatalf("disabled draft = %d %s", response.StatusCode, answer)
	}
}
