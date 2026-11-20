package controllers

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/cost"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/middleware"
)

// TestFinOpsRoutesTakeCommaSeparatedScope: accounts, regions and teams are
// comma-separated lists; repeated singular keys are accepted too.
func TestFinOpsRoutesTakeCommaSeparatedScope(t *testing.T) {
	app, _ := finopsApp(t, finopsBackend(t), finopsViewer, false)
	keys := func(path string) []string {
		var v finops.BreakdownView
		_, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/breakdown?period=2026-10&"+path, "")
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range v.Rows {
			out = append(out, r.Key)
		}
		sort.Strings(out)
		return out
	}
	both := keys("group_by=account&accounts=111111111111,222222222222")
	if len(both) != 2 || both[0] != "111111111111" || both[1] != "222222222222" {
		t.Fatalf("two accounts: %v", both)
	}
	if one := keys("group_by=account&accounts=222222222222"); len(one) != 1 || one[0] != "222222222222" {
		t.Fatalf("one account: %v", one)
	}
	if regions := keys("group_by=region&regions=us-east-1,global"); len(regions) != 2 {
		t.Fatalf("two regions: %v", regions)
	}
	if teams := keys("group_by=team&teams=checkout,platform"); len(teams) != 2 || teams[0] != "checkout" || teams[1] != "platform" {
		t.Fatalf("two teams: %v", teams)
	}
	var sum finops.SummaryView
	_, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/summary?period=2026-10&accounts=222222222222", "")
	if err := json.Unmarshal(body, &sum); err != nil || len(sum.ScopeOptions.Accounts) != 3 {
		t.Fatalf("scope options keep every account under a filter: %+v", sum.ScopeOptions)
	}
}

// TestFinOpsRoutesAdmitFinOpsViewAndHonourAccountScope: an auth adapter that
// grants only the FinOps view and limits the caller to one account through
// the request locals gets the FinOps page for that account alone; with
// neither view the routes refuse.
func TestFinOpsRoutesAdmitFinOpsViewAndHonourAccountScope(t *testing.T) {
	backend := finopsBackend(t)
	backend.cfg.AccountScope = cost.CallerAccounts
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), false)
		middleware.SetRequestPermission(ctx, string(core.PermissionFinOpsView), ctx.Get("X-Test-Finance") == "yes")
		if ctx.Get("X-Test-Scope") == "prod" {
			middleware.SetRequestAccountScope(ctx, []string{"111111111111"})
		}
		return ctx.Next()
	})
	NewAWSFinOpsController(backend).Register(app.Group("/api"))
	request := func(finance, scope, path string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, "http://versus.test"+path, nil)
		req.Header.Set("X-Test-Finance", finance)
		req.Header.Set("X-Test-Scope", scope)
		res, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body json.RawMessage
		_ = json.NewDecoder(res.Body).Decode(&body)
		return res.StatusCode, body
	}
	if status, _ := request("no", "prod", "/api/admin/aws/finops/summary"); status != fiber.StatusForbidden {
		t.Fatalf("neither view = %d, want 403", status)
	}
	status, body := request("yes", "", "/api/admin/aws/finops/accounts?period=2026-10")
	var all finops.AccountsView
	if err := json.Unmarshal(body, &all); status != fiber.StatusOK || err != nil || len(all.Accounts) != 3 {
		t.Fatalf("FinOps view without a scope = %d %d accounts", status, len(all.Accounts))
	}
	status, body = request("yes", "prod", "/api/admin/aws/finops/accounts?period=2026-10")
	var scoped finops.AccountsView
	if err := json.Unmarshal(body, &scoped); status != fiber.StatusOK || err != nil || len(scoped.Accounts) != 1 || scoped.Accounts[0].ID != "111111111111" {
		t.Fatalf("scoped accounts = %d %+v", status, scoped.Accounts)
	}
}

// TestCallerAuthorizationCopiesFinOpsViewAndAccountScope: the FinOps view and
// the account scope an adapter sets in the request locals reach the caller
// decision the tools and services read; without them nothing is limited.
func TestCallerAuthorizationCopiesFinOpsViewAndAccountScope(t *testing.T) {
	app := fiber.New()
	var limited, plain core.CallerAuthorization
	app.Get("/limited", func(ctx *fiber.Ctx) error {
		middleware.SetRequestPermission(ctx, string(core.PermissionFinOpsView), true)
		middleware.SetRequestAccountScope(ctx, []string{"111111111111"})
		limited = callerAuthorization(ctx)
		return nil
	})
	app.Get("/plain", func(ctx *fiber.Ctx) error {
		plain = callerAuthorization(ctx)
		return nil
	})
	for _, path := range []string{"/limited", "/plain"} {
		req, _ := http.NewRequest(http.MethodGet, "http://versus.test"+path, nil)
		if _, err := app.Test(req, -1); err != nil {
			t.Fatal(err)
		}
	}
	if !limited.Permissions[core.PermissionFinOpsView] || limited.Accounts == nil || len(limited.Accounts.IDs) != 1 || limited.Accounts.IDs[0] != "111111111111" {
		t.Fatalf("limited caller %+v", limited)
	}
	if _, set := plain.Permissions[core.PermissionFinOpsView]; set || plain.Accounts != nil {
		t.Fatalf("a caller without adapter decisions was limited: %+v", plain)
	}
	if middleware.RequestAccountScopeKey != "versus.account_scope" {
		t.Fatalf("the account scope local key changed: %q", middleware.RequestAccountScopeKey)
	}
}
