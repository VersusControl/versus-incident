package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	awslimit "github.com/VersusControl/versus-incident/pkg/aws/limit"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"

	"github.com/gofiber/fiber/v2"
)

const awsTestOrigin = "http://localhost:3000"

func awsTestBase(string) (aws.CredentialsProvider, error) {
	return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "secret", "")), nil
}

func awsTestService(t *testing.T, scenario string, enabled bool, configured ...config.AWSAccountConfig) *awssettings.Service {
	t.Helper()
	fake := fakeaws.New(t, scenario)
	store := awssettings.NewStore(storage.NewMemory(), tenancy.DefaultOrgScope())
	return awssettings.NewService(store,
		awssettings.WithConfig(func() config.AWSConnectorConfig {
			return config.AWSConnectorConfig{
				Enable:              enabled,
				HealthCheckInterval: "5m",
				Accounts:            configured,
				Budgets:             config.AWSBudgetsConfig{MaxRequestsPerSecond: 100},
			}
		}),
		awssettings.WithHTTPClient(fake.HTTPClient()),
		awssettings.WithBaseProvider(awsTestBase),
		awssettings.WithLimiter(awslimit.NewManager(100)),
		awssettings.WithClock(func() time.Time { return time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC) }),
	)
}

func awsTestApp(t *testing.T, service *awssettings.Service, permissions map[string]bool) (*fiber.App, *[]middleware.AdminAuditEvent) {
	t.Helper()
	events := make([]middleware.AdminAuditEvent, 0)
	middleware.SetAdminAuditHook(func(_ *fiber.Ctx, event middleware.AdminAuditEvent) { events = append(events, event) })
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		for permission, allowed := range permissions {
			middleware.SetRequestPermission(ctx, permission, allowed)
		}
		return ctx.Next()
	})
	NewAWSSettingsController(service).Register(app.Group("/api"))
	return app, &events
}

func awsTestRequest(t *testing.T, app *fiber.App, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	return response, buffer.Bytes()
}

func TestAWSSettingsRequiresGatewayAuthorization(t *testing.T) {
	loadGatewayConfig(t, "aws-settings-auth-secret")
	service := awsTestService(t, "new-account-connect", true)
	app := fiber.New()
	app.Use(middleware.OrgInjector())
	NewAWSSettingsController(service).Register(app.Group("/api"))
	response, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings", "")
	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
}

func TestAWSSettingsViewerIsReadOnly(t *testing.T) {
	service := awsTestService(t, "new-account-connect", true)
	app, events := awsTestApp(t, service, map[string]bool{string(core.PermissionInfrastructureView): true})

	response, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("view status = %d body=%s", response.StatusCode, body)
	}
	var view awssettings.SettingsView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Capabilities.CanWrite {
		t.Fatal("viewer was offered write capability")
	}
	if view.Capabilities.Organizations {
		t.Fatal("OSS reported the Organizations capability")
	}

	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/admin/aws/connect/drafts/" + strings.Repeat("a", 32), ""},
		{http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`},
		{http.MethodPut, "/api/admin/aws/settings/privacy", `{"tag_value_allowlist":["app"]}`},
		{http.MethodDelete, "/api/admin/aws/accounts/111122223333", ""},
	} {
		response, body := awsTestRequest(t, app, request.method, request.path, request.body)
		if response.StatusCode != fiber.StatusForbidden {
			t.Fatalf("%s %s status = %d body=%s", request.method, request.path, response.StatusCode, body)
		}
	}
	for _, event := range *events {
		if event.Action == "aws.settings.changed" {
			t.Fatalf("refused viewer write was audited as a change: %+v", event)
		}
	}
}

// inventorylessProvider hides a provider's typed inventory store, standing
// in for a deployment without the inventory tables.
type inventorylessProvider struct{ storage.Provider }

// awsNoStorageService builds a settings service whose snapshot store cannot
// reach its inventory tables, over a shared provider and settings store.
func awsNoStorageService(t *testing.T, provider storage.Provider) (*awssettings.Service, *awssettings.Store) {
	t.Helper()
	store := awssettings.NewStore(provider, tenancy.DefaultOrgScope())
	snapshots := awssnapshot.New(inventorylessProvider{provider}, tenancy.DefaultOrgScope())
	return awssettings.NewService(store, awssettings.WithSnapshots(snapshots)), store
}

func awsWriteApp(t *testing.T, service *awssettings.Service) *fiber.App {
	t.Helper()
	app, _ := awsTestApp(t, service, map[string]bool{
		string(core.PermissionInfrastructureView):          true,
		string(core.PermissionInfrastructureSettingsWrite): true,
	})
	return app
}

// TestAWSSettingsWriteAnswersNoStorage proves a write that must purge
// inventory answers 503 storage_unavailable when the inventory tables are
// missing, not an internal failure.
func TestAWSSettingsWriteAnswersNoStorage(t *testing.T) {
	provider := storage.NewMemory()
	service, store := awsNoStorageService(t, provider)
	if err := store.SaveAccount("default", awssettings.AccountRecord{AccountID: "111122223333", DisplayName: "Prod",
		Regions: []string{"us-east-1", "us-west-2"}, Source: awssettings.SourceWizard, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	app := awsWriteApp(t, service)
	response, body := awsTestRequest(t, app, http.MethodPatch, "/api/admin/aws/accounts/111122223333", `{"regions":["us-east-1"]}`)
	if response.StatusCode != fiber.StatusServiceUnavailable || !bytes.Contains(body, []byte(`"error":"storage_unavailable"`)) {
		t.Fatalf("regions write = %d %s, want 503 storage_unavailable", response.StatusCode, body)
	}
}

// TestAWSSettingsDeleteAnswersNoStorageAndKeepsRecord proves the delete path
// answers the same closed code and leaves the record retryable.
func TestAWSSettingsDeleteAnswersNoStorageAndKeepsRecord(t *testing.T) {
	provider := storage.NewMemory()
	service, store := awsNoStorageService(t, provider)
	if err := store.SaveAccount("default", awssettings.AccountRecord{AccountID: "111122223333", DisplayName: "Prod",
		Regions: []string{"us-east-1"}, Source: awssettings.SourceWizard, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	app := awsWriteApp(t, service)
	response, body := awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/111122223333", "")
	if response.StatusCode != fiber.StatusServiceUnavailable || !bytes.Contains(body, []byte(`"error":"storage_unavailable"`)) {
		t.Fatalf("delete = %d %s, want 503 storage_unavailable", response.StatusCode, body)
	}
	if accounts, err := store.ListAccounts("default"); err != nil || len(accounts) != 1 {
		t.Fatalf("settings record did not survive the failed delete: %+v %v", accounts, err)
	}
}

func TestAWSSettingsDisabledAnswersEnabledFalse(t *testing.T) {
	service := awsTestService(t, "new-account-connect", false)
	app, _ := awsTestApp(t, service, map[string]bool{
		string(core.PermissionInfrastructureView):          true,
		string(core.PermissionInfrastructureSettingsWrite): true,
	})
	response, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings", "")
	if response.StatusCode != fiber.StatusOK || !bytes.Contains(body, []byte(`"enabled":false`)) {
		t.Fatalf("disabled settings = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`)
	if response.StatusCode != fiber.StatusConflict || !bytes.Contains(body, []byte(`"disabled"`)) {
		t.Fatalf("disabled draft = %d %s", response.StatusCode, body)
	}
}

func TestAWSSettingsVerifyIsRateLimited(t *testing.T) {
	service := awsTestService(t, "new-account-connect", true)
	app, events := awsTestApp(t, service, map[string]bool{
		string(core.PermissionInfrastructureView):          true,
		string(core.PermissionInfrastructureSettingsWrite): true,
	})
	response, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	var start awssettings.DraftStartView
	if err := json.Unmarshal(body, &start); err != nil {
		t.Fatal(err)
	}
	verify := `{"role_arn":"arn:aws:iam::111122223333:role/versus-reader"}`
	for attempt := 0; attempt < 6; attempt++ {
		response, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts/"+start.DraftID+"/verify", verify)
		if response.StatusCode != fiber.StatusAccepted {
			t.Fatalf("verify %d status = %d body=%s", attempt, response.StatusCode, body)
		}
	}
	response, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts/"+start.DraftID+"/verify", verify)
	if response.StatusCode != fiber.StatusTooManyRequests || response.Header.Get(fiber.HeaderRetryAfter) == "" {
		t.Fatalf("rate limit = %d retry-after=%q body=%s", response.StatusCode, response.Header.Get(fiber.HeaderRetryAfter), body)
	}
	if !bytes.Contains(body, []byte(`"rate_limited"`)) {
		t.Fatalf("rate limit body = %s", body)
	}
	recorded := *events
	if len(recorded) == 0 {
		t.Fatal("rate-limited verify produced no audit event")
	}
	last := recorded[len(recorded)-1]
	if last.Action != "aws.settings.rate_limited" || last.Result != middleware.AdminAuditDenied {
		t.Fatalf("7th verify audit = %+v, want aws.settings.rate_limited/%s", last, middleware.AdminAuditDenied)
	}
	for _, event := range recorded {
		if event.Action == "rbac.denied" {
			t.Fatalf("throttled verify was audited as an RBAC denial: %+v", event)
		}
	}
}

func TestAWSSettingsRejectsCrossOriginWrites(t *testing.T) {
	middleware.SetGatewayAuthEnabled(true)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(true) })
	const secret = "aws-settings-origin-secret"
	loadGatewayConfig(t, secret)
	previous := config.GetConfig().GatewaySecret
	previousHost := config.GetConfig().PublicHost
	config.GetConfig().GatewaySecret = secret
	config.GetConfig().PublicHost = awsTestOrigin
	t.Cleanup(func() {
		config.GetConfig().GatewaySecret = previous
		config.GetConfig().PublicHost = previousHost
	})

	service := awsTestService(t, "new-account-connect", true)
	app := fiber.New()
	app.Use(middleware.OrgInjector())
	api := app.Group("/api")
	RegisterGatewaySessionRoutes(api)
	NewAWSSettingsController(service).Register(api)

	exchange, err := http.NewRequest(http.MethodPost, awsTestOrigin+"/api/auth/gateway-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	exchange.Header.Set("X-Gateway-Secret", secret)
	exchangeResponse, err := app.Test(exchange, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer exchangeResponse.Body.Close()
	cookies := exchangeResponse.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("session cookies = %d", len(cookies))
	}

	request, err := http.NewRequest(http.MethodPost, awsTestOrigin+"/api/admin/aws/connect/drafts", strings.NewReader(`{"method":"cloudformation"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://evil.example")
	request.AddCookie(cookies[0])
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != fiber.StatusForbidden {
		t.Fatalf("cross-origin status = %d", response.StatusCode)
	}

	sameOrigin, err := http.NewRequest(http.MethodPost, awsTestOrigin+"/api/admin/aws/connect/drafts", strings.NewReader(`{"method":"cloudformation"}`))
	if err != nil {
		t.Fatal(err)
	}
	sameOrigin.Header.Set("Content-Type", "application/json")
	sameOrigin.Header.Set("Origin", awsTestOrigin)
	sameOrigin.AddCookie(cookies[0])
	response, err = app.Test(sameOrigin, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode == fiber.StatusForbidden {
		t.Fatal("same-origin write was refused")
	}
}

func TestAWSSettingsEndpointContract(t *testing.T) {
	service := awsTestService(t, "new-account-connect", true)
	app, events := awsTestApp(t, service, map[string]bool{
		string(core.PermissionInfrastructureView):          true,
		string(core.PermissionInfrastructureSettingsWrite): true,
	})

	response, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	var start awssettings.DraftStartView
	if err := json.Unmarshal(body, &start); err != nil {
		t.Fatal(err)
	}
	response, revealBody := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/connect/drafts/"+start.DraftID+"/external-id", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("reveal = %d %s", response.StatusCode, revealBody)
	}
	if cache := response.Header.Get(fiber.HeaderCacheControl); cache != "no-store" {
		t.Fatalf("reveal cache-control = %q, want no-store", cache)
	}
	var revealed struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(revealBody, &revealed); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(revealed.ExternalID, awssettings.ExternalIDPrefix) {
		t.Fatalf("revealed external id = %q", revealed.ExternalID)
	}

	response, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts/"+start.DraftID+"/verify",
		`{"role_arn":"arn:aws:iam::444455556666:role/versus-reader"}`)
	if response.StatusCode != fiber.StatusAccepted {
		t.Fatalf("verify = %d %s", response.StatusCode, body)
	}
	var verified awssettings.DraftView
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/connect/drafts/"+start.DraftID, "")
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("draft = %d %s", response.StatusCode, body)
		}
		if err := json.Unmarshal(body, &verified); err != nil {
			t.Fatal(err)
		}
		if verified.Status == awssettings.DraftVerified {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("verify did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if bytes.Contains(body, []byte(revealed.ExternalID)) {
		t.Fatal("draft body carried the full external id")
	}
	if !bytes.Contains(body, []byte(`"assume"`)) || !bytes.Contains(body, []byte(`"counts"`)) {
		t.Fatalf("draft DTO missing fields: %s", body)
	}

	response, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts/"+start.DraftID+"/regions", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("regions = %d %s", response.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"resources_found"`)) {
		t.Fatalf("regions DTO = %s", body)
	}

	response, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/accounts",
		`{"draft_id":"`+start.DraftID+`","display_name":"Staging","regions":["us-east-1","eu-west-1"]}`)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("commit = %d %s", response.StatusCode, body)
	}
	if bytes.Contains(body, []byte(revealed.ExternalID)) {
		t.Fatal("commit body carried the full external id")
	}

	response, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("settings = %d %s", response.StatusCode, body)
	}
	var view awssettings.SettingsView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Accounts) != 1 || view.Accounts[0].AccountID != "444455556666" {
		t.Fatalf("settings accounts = %+v", view.Accounts)
	}
	if bytes.Contains(body, []byte(revealed.ExternalID)) {
		t.Fatal("settings body carried the full external id")
	}
	for _, field := range []string{`"services"`, `"regions"`, `"api_use"`, `"optional_data"`, `"privacy"`} {
		if !bytes.Contains(body, []byte(field)) {
			t.Fatalf("settings DTO missing %s: %s", field, body)
		}
	}

	response, body = awsTestRequest(t, app, http.MethodPatch, "/api/admin/aws/accounts/444455556666", `{"display_name":"Staging EU"}`)
	if response.StatusCode != fiber.StatusOK || !bytes.Contains(body, []byte("Staging EU")) {
		t.Fatalf("patch = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/accounts/444455556666/recheck", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("recheck = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodPut, "/api/admin/aws/settings/options", `{"cost_explorer":true,"max_requests_per_second":2}`)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("options = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodPut, "/api/admin/aws/settings/privacy", `{"tag_value_allowlist":["app","team"]}`)
	if response.StatusCode != fiber.StatusOK || !bytes.Contains(body, []byte("team")) {
		t.Fatalf("privacy = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/444455556666", "")
	if response.StatusCode != fiber.StatusOK || string(body) != `{}` {
		t.Fatalf("delete = %d %s", response.StatusCode, body)
	}

	if len(*events) == 0 {
		t.Fatal("writes were not audited")
	}
	revealedAudit := false
	for _, event := range *events {
		if event.Action == "aws.external-id.revealed" {
			revealedAudit = true
		}
		if strings.Contains(event.Action+event.Target+event.Result, revealed.ExternalID) {
			t.Fatal("audit event carried the external id")
		}
	}
	if !revealedAudit {
		t.Fatal("external-id reveal was not audited")
	}
}

func TestAWSSettingsClosedErrors(t *testing.T) {
	service := awsTestService(t, "new-account-connect", true, config.AWSAccountConfig{AccountID: "111122223333", DisplayName: "Configured"})
	app, _ := awsTestApp(t, service, map[string]bool{
		string(core.PermissionInfrastructureView):          true,
		string(core.PermissionInfrastructureSettingsWrite): true,
	})

	response, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/connect/drafts/"+strings.Repeat("a", 32), "")
	if response.StatusCode != fiber.StatusNotFound || !bytes.Contains(body, []byte(`"not_found"`)) {
		t.Fatalf("unknown draft = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation","extra":true}`)
	if response.StatusCode != fiber.StatusBadRequest || !bytes.Contains(body, []byte(`"invalid_arguments"`)) {
		t.Fatalf("strict decode = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/connect/drafts", `{not json`)
	if response.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("malformed body = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/111122223333", "")
	if response.StatusCode != fiber.StatusConflict || !bytes.Contains(body, []byte(`"config_managed"`)) {
		t.Fatalf("config account delete = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/not-an-account", "")
	if response.StatusCode != fiber.StatusBadRequest || !bytes.Contains(body, []byte(`"invalid_arguments"`)) {
		t.Fatalf("invalid account = %d %s", response.StatusCode, body)
	}
}

func decodeAWSJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return decoded
}

func awsObject(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want an object", name, value)
	}
	return object
}

func awsNumber(t *testing.T, value any, name string) {
	t.Helper()
	if _, ok := value.(float64); !ok {
		t.Fatalf("%s is %T, want a number", name, value)
	}
}

func awsArray(t *testing.T, value any, name string) {
	t.Helper()
	if _, ok := value.([]any); !ok {
		t.Fatalf("%s is %T, want an array", name, value)
	}
}

func awsString(t *testing.T, value any, name string) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("%s is %T, want a string", name, value)
	}
	return text
}

// TestAWSSettingsDTOContract pins the exact keys and JSON types the settings
// screen reads. Counts are numbers, the denied and not-on-plan lists are
// arrays, and every budget or allowlist answer names its source.
func TestAWSSettingsDTOContract(t *testing.T) {
	service := awsTestService(t, "new-account-connect", true, config.AWSAccountConfig{
		AccountID:   "111122223333",
		DisplayName: "Production",
		Regions:     []string{"us-east-1"},
		Auth:        config.AWSAuthConfig{RoleARN: "arn:aws:iam::111122223333:role/versus-reader"},
	})
	app, _ := awsTestApp(t, service, map[string]bool{
		string(core.PermissionInfrastructureView):          true,
		string(core.PermissionInfrastructureSettingsWrite): true,
	})
	response, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("settings = %d %s", response.StatusCode, body)
	}

	view := decodeAWSJSON(t, body)
	budget := awsObject(t, view["budget"], "budget")
	awsNumber(t, budget["used_rps"], "budget.used_rps")
	awsNumber(t, budget["budget_rps"], "budget.budget_rps")
	if source := awsString(t, budget["source"], "budget.source"); source != "config" && source != "wizard" && source != "default" {
		t.Fatalf("budget.source = %q", source)
	}

	accounts, ok := view["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts = %T %v", view["accounts"], view["accounts"])
	}
	account := awsObject(t, accounts[0], "accounts[0]")
	services := awsObject(t, account["services"], "accounts[0].services")
	for _, key := range []string{"total", "readable", "not_used", "not_checked"} {
		awsNumber(t, services[key], "accounts[0].services."+key)
	}
	for _, key := range []string{"denied", "denied_services", "not_on_plan", "not_on_plan_services"} {
		awsArray(t, services[key], "accounts[0].services."+key)
	}
	apiUse := awsObject(t, account["api_use"], "accounts[0].api_use")
	awsNumber(t, apiUse["used_rps"], "accounts[0].api_use.used_rps")
	awsNumber(t, apiUse["budget_rps"], "accounts[0].api_use.budget_rps")

	optional := awsObject(t, view["optional_data"], "optional_data")
	for _, key := range []string{"cost_explorer", "config", "security_hub", "trusted_advisor"} {
		entry := awsObject(t, optional[key], "optional_data."+key)
		awsNumber(t, entry["on"], "optional_data."+key+".on")
		awsString(t, entry["source"], "optional_data."+key+".source")
	}

	privacy := awsObject(t, view["privacy"], "privacy")
	awsArray(t, privacy["tag_value_allowlist"], "privacy.tag_value_allowlist")
	awsString(t, privacy["tag_value_allowlist_source"], "privacy.tag_value_allowlist_source")
}

func TestAWSSettingsOptionsAcceptsNestedBody(t *testing.T) {
	service := awsTestService(t, "new-account-connect", true, config.AWSAccountConfig{
		AccountID:   "111122223333",
		DisplayName: "Production",
		Regions:     []string{"us-east-1"},
	})
	app, _ := awsTestApp(t, service, map[string]bool{
		string(core.PermissionInfrastructureView):          true,
		string(core.PermissionInfrastructureSettingsWrite): true,
	})

	nested := `{"optional_data":{"cost_explorer":true,"config":false},"budget":{"max_requests_per_second":5}}`
	response, body := awsTestRequest(t, app, http.MethodPut, "/api/admin/aws/settings/options", nested)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("nested options = %d %s", response.StatusCode, body)
	}
	response, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings", "")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("settings = %d %s", response.StatusCode, body)
	}
	view := decodeAWSJSON(t, body)
	optional := awsObject(t, view["optional_data"], "optional_data")
	costExplorer := awsObject(t, optional["cost_explorer"], "optional_data.cost_explorer")
	if on, _ := costExplorer["on"].(float64); on != 1 {
		t.Fatalf("cost explorer on = %v", costExplorer["on"])
	}
	if source := awsString(t, costExplorer["source"], "optional_data.cost_explorer.source"); source != "wizard" {
		t.Fatalf("cost explorer source = %q", source)
	}
	budget := awsObject(t, view["budget"], "budget")
	if rate, _ := budget["budget_rps"].(float64); rate != 5 {
		t.Fatalf("budget rps = %v", budget["budget_rps"])
	}
	if source := awsString(t, budget["source"], "budget.source"); source != "wizard" {
		t.Fatalf("budget source = %q", source)
	}

	for _, unknown := range []string{
		`{"optional_data":{"cost_explorer":true,"bogus":true}}`,
		`{"bogus":true}`,
	} {
		response, body = awsTestRequest(t, app, http.MethodPut, "/api/admin/aws/settings/options", unknown)
		if response.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("unknown key %s = %d %s", unknown, response.StatusCode, body)
		}
	}
}

// deferringRemover stands in for the cost adapter: it marks payers removed
// and purges them unless purges are deferred, as when a cost refresh holds
// the cost lock past the bound.
type deferringRemover struct{ deferred atomic.Bool }

func (r *deferringRemover) MarkPayerRemoved(context.Context, string, uint64) error { return nil }

func (r *deferringRemover) PurgePayer(context.Context, string, time.Duration) (bool, error) {
	return !r.deferred.Load(), nil
}

// TestAWSSettingsDeleteAnswersThePurgeOutcome: removing an account answers
// 200. The body is empty when its cost data was purged at once, and carries
// purge "pending" when a running cost refresh left the purge to itself.
func TestAWSSettingsDeleteAnswersThePurgeOutcome(t *testing.T) {
	store := awssettings.NewStore(storage.NewMemory(), tenancy.DefaultOrgScope())
	remover := &deferringRemover{}
	service := awssettings.NewService(store,
		awssettings.WithConfig(func() config.AWSConnectorConfig { return config.AWSConnectorConfig{Enable: true} }),
		awssettings.WithPayerRemover(remover))
	for _, id := range []string{"111122223333", "444455556666"} {
		if err := store.SaveAccount("default", awssettings.AccountRecord{AccountID: id, Source: awssettings.SourceWizard, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	app := awsWriteApp(t, service)
	response, body := awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/111122223333", "")
	if response.StatusCode != fiber.StatusOK || string(body) != `{}` {
		t.Fatalf("delete with the purge done = %d %s, want 200 {}", response.StatusCode, body)
	}
	remover.deferred.Store(true)
	response, body = awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/444455556666", "")
	if response.StatusCode != fiber.StatusOK || string(body) != `{"purge":"pending"}` {
		t.Fatalf("delete with the purge deferred = %d %s, want 200 {\"purge\":\"pending\"}", response.StatusCode, body)
	}
	if accounts, err := store.ListAccounts("default"); err != nil || len(accounts) != 0 {
		t.Fatalf("accounts after both deletes = %+v %v", accounts, err)
	}
}

// contendedRevisionProvider makes every compare-and-swap of the account
// settings revision lose while contend is set, standing in for writers on
// other replicas that keep raising it.
type contendedRevisionProvider struct {
	storage.Provider
	key     string
	contend atomic.Bool
}

func (p *contendedRevisionProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	if p.contend.Load() && name == p.key {
		return false, nil
	}
	return p.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

// TestAWSSettingsBusyWriteAnswersRefreshInProgress: a settings write that
// cannot complete because other writers keep moving the account revision
// answers the same code as a busy cost refresh: 409 refresh_in_progress
// with Retry-After 5.
func TestAWSSettingsBusyWriteAnswersRefreshInProgress(t *testing.T) {
	provider := &contendedRevisionProvider{Provider: storage.NewMemory(), key: awssettings.AccountsRevisionKey("default")}
	store := awssettings.NewStore(provider, tenancy.DefaultOrgScope())
	service := awssettings.NewService(store,
		awssettings.WithConfig(func() config.AWSConnectorConfig { return config.AWSConnectorConfig{Enable: true} }))
	if err := store.SaveAccount("default", awssettings.AccountRecord{AccountID: "111122223333", DisplayName: "Prod", Source: awssettings.SourceWizard, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	provider.contend.Store(true)
	app := awsWriteApp(t, service)
	response, body := awsTestRequest(t, app, http.MethodPatch, "/api/admin/aws/accounts/111122223333", `{"display_name":"Production"}`)
	if response.StatusCode != fiber.StatusConflict || string(body) != `{"error":"refresh_in_progress"}` || response.Header.Get(fiber.HeaderRetryAfter) != "5" {
		t.Fatalf("busy settings write = %d %s Retry-After %q, want 409 refresh_in_progress with Retry-After 5",
			response.StatusCode, body, response.Header.Get(fiber.HeaderRetryAfter))
	}
}
