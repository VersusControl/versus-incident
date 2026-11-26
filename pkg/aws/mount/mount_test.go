package mount

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// awsRoutes is every route the AWS surface serves: the connector settings,
// the Changes page, the coverage page, the collection cost card, the issues and FinOps. GET routes also answer HEAD.
var awsRoutes = []string{
	"GET /api/admin/aws/settings",
	"PUT /api/admin/aws/settings/options",
	"PUT /api/admin/aws/settings/privacy",
	"POST /api/admin/aws/connect/drafts",
	"GET /api/admin/aws/connect/drafts/:id",
	"GET /api/admin/aws/connect/drafts/:id/external-id",
	"POST /api/admin/aws/connect/drafts/:id/verify",
	"POST /api/admin/aws/connect/drafts/:id/regions",
	"POST /api/admin/aws/accounts",
	"POST /api/admin/aws/accounts/:account_id/recheck",
	"DELETE /api/admin/aws/accounts/:account_id",
	"PATCH /api/admin/aws/accounts/:account_id",
	"GET /api/admin/aws/changes/",
	"GET /api/admin/aws/changes/groups/:id",
	"GET /api/admin/aws/changes/compare",
	"GET /api/admin/aws/changes/preferences",
	"PUT /api/admin/aws/changes/preferences",
	"GET /api/admin/aws/coverage/",
	"GET /api/admin/aws/accounts/labels/",
	"PUT /api/admin/aws/accounts/labels/:id",
	"DELETE /api/admin/aws/accounts/labels/:id",
	"GET /api/admin/aws/groups/corrections/",
	"GET /api/admin/aws/settings/cost/",
	"POST /api/admin/aws/groups/corrections/",
	"DELETE /api/admin/aws/groups/corrections/",
	"GET /api/admin/aws/issues/",
	"GET /api/admin/aws/issues/report",
	"POST /api/admin/aws/issues/reject",
	"GET /api/admin/aws/issues/:id",
	"POST /api/admin/aws/issues/:id/acknowledge",
	"POST /api/admin/aws/issues/:id/reject",
	"POST /api/admin/aws/issues/:id/reopen",
	"GET /api/admin/aws/finops/summary",
	"GET /api/admin/aws/finops/bridge",
	"GET /api/admin/aws/finops/trend",
	"GET /api/admin/aws/finops/breakdown",
	"GET /api/admin/aws/finops/increases",
	"GET /api/admin/aws/finops/increases/:id",
	"GET /api/admin/aws/finops/savings",
	"GET /api/admin/aws/finops/accounts",
	"GET /api/admin/aws/finops/budgets",
	"GET /api/admin/aws/finops/reports",
	"GET /api/admin/aws/finops/report",
	"GET /api/admin/aws/finops/export.csv",
	"POST /api/admin/aws/finops/increases/:id/status",
	"POST /api/admin/aws/finops/savings/:id/dismiss",
	"PUT /api/admin/aws/finops/budgets",
	"POST /api/admin/aws/finops/reports",
	"POST /api/admin/aws/finops/refresh",
	"POST /api/admin/aws/finops/drilldown",
}

func mountForTest(t *testing.T, app *fiber.App, deps Deps) *Mounted {
	t.Helper()
	if deps.Store == nil {
		deps.Store = storage.NewMemory()
	}
	if deps.Config == nil {
		deps.Config = &c.Config{}
	}
	deps.Router = app.Group("/api")
	mounted, err := Mount(deps)
	if err != nil {
		t.Fatal(err)
	}
	return mounted
}

func routeSet(app *fiber.App) []string {
	var out []string
	for _, route := range app.GetRoutes(true) {
		out = append(out, route.Method+" "+route.Path)
	}
	sort.Strings(out)
	return out
}

// TestMountRegistersTheAWSRouteSet: the shared setup serves exactly the
// connector, Changes and FinOps routes the open-source binary served, and a
// build that passes its own seams serves the same set.
func TestMountRegistersTheAWSRouteSet(t *testing.T) {
	var want []string
	for _, route := range awsRoutes {
		want = append(want, route)
		if method, path, _ := strings.Cut(route, " "); method == fiber.MethodGet {
			want = append(want, fiber.MethodHead+" "+path)
		}
	}
	sort.Strings(want)
	oss := fiber.New()
	mountForTest(t, oss, Deps{})
	licensed := fiber.New()
	mountForTest(t, licensed, Deps{Scope: tenancy.NewOrgScope("acme", tenancy.DefaultOrgID),
		ChangesLimits: func() awssnapshot.ChangesLimits { return awssnapshot.ChangesLimits{MaxWindow: 7 * 24 * time.Hour} },
		FinOpsOptions: func() finops.EditionOptions { return finops.EditionOptions{Chargeback: true} }})
	for name, app := range map[string]*fiber.App{"open-source": oss, "licensed": licensed} {
		got := routeSet(app)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s routes differ:\n%s", name, diffRoutes(want, got))
		}
	}
}

// TestMountKeepsEachRouteMiddlewareChain: every route, middleware groups
// included, carries the same handler chain as the admin controllers
// registered on their own, the way the binary mounted them before.
func TestMountKeepsEachRouteMiddlewareChain(t *testing.T) {
	chains := func(app *fiber.App) []string {
		var out []string
		for _, route := range app.GetRoutes(false) {
			out = append(out, route.Method+" "+route.Path+" handlers="+strconv.Itoa(len(route.Handlers)))
		}
		sort.Strings(out)
		return out
	}
	direct := fiber.New()
	controllers.NewAWSSettingsController(nil).Register(direct.Group("/api"))
	controllers.NewAWSChangesController(nil).WithPreferences(nil).Register(direct.Group("/api"))
	controllers.NewAWSCoverageController(nil, nil).Register(direct.Group("/api"))
	controllers.NewAWSAccountLabelsController(nil, tenancy.DefaultOrgScope()).Register(direct.Group("/api"))
	controllers.NewAWSGroupCorrectionsController(nil, tenancy.DefaultOrgScope(), nil).Register(direct.Group("/api"))
	controllers.NewAWSCollectionCostController(nil).Register(direct.Group("/api"))
	controllers.NewAWSIssuesController(nil, nil, nil, nil).Register(direct.Group("/api"))
	controllers.NewAWSFinOpsController(nil).Register(direct.Group("/api"))
	mounted := fiber.New()
	mountForTest(t, mounted, Deps{})
	want, got := chains(direct), chains(mounted)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("route chains differ:\n%s", diffRoutes(want, got))
	}
}

func diffRoutes(want, got []string) string {
	seen := map[string]int{}
	for _, route := range want {
		seen[route]++
	}
	for _, route := range got {
		seen[route]--
	}
	var out []string
	for route, count := range seen {
		switch {
		case count > 0:
			out = append(out, "missing "+route)
		case count < 0:
			out = append(out, "extra   "+route)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// changesRequest calls the Changes route as a caller who may view the
// infrastructure pages.
func changesRequest(t *testing.T, app *fiber.App, query string) (int, string) {
	t.Helper()
	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/aws/changes?"+query, nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func viewerApp() *fiber.App {
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	return app
}

// TestMountDefaultsLockTheLicensedOptions: without seams the mount is the
// open-source build: the default org, 24 h Changes limits, a 7d window
// refused as window_locked, and every licensed FinOps option off.
func TestMountDefaultsLockTheLicensedOptions(t *testing.T) {
	app := viewerApp()
	mounted := mountForTest(t, app, Deps{})
	if got := mounted.Changes.Limits(); got != awssnapshot.DefaultChangesLimits() {
		t.Fatalf("changes limits = %+v", got)
	}
	if got := mounted.FinOpsOptions(); got != finops.DefaultEditionOptions() || got.RetentionMonths != 13 {
		t.Fatalf("finops options = %+v", got)
	}
	status, body := changesRequest(t, app, "window=7d")
	if status != fiber.StatusForbidden || body != `{"error":"window_locked","max_window":"24h","tier":"enterprise"}` {
		t.Fatalf("7d = %d %s", status, body)
	}
	if status, body := changesRequest(t, app, "since="+time.Now().Add(-30*time.Hour).UTC().Format(time.RFC3339)); status != fiber.StatusForbidden {
		t.Fatalf("since 30h back = %d %s", status, body)
	}
	if mounted.deployment != DefaultDeployment {
		t.Fatalf("deployment = %q", mounted.deployment)
	}
}

// TestMountPassesTheLicenseSeams: the limits a build passes reach the
// Changes route, read on every request, and its FinOps options are the ones
// the mount reports.
func TestMountPassesTheLicenseSeams(t *testing.T) {
	week := 7 * 24 * time.Hour
	limits := awssnapshot.ChangesLimits{MaxWindow: week, MaxCompareAge: week}
	options := finops.EditionOptions{ScheduledPDF: true, BrandedPDF: true, Chargeback: true, FinanceRole: true, RetentionMonths: 36}
	app := viewerApp()
	mounted := mountForTest(t, app, Deps{
		ChangesLimits: func() awssnapshot.ChangesLimits { return limits },
		FinOpsOptions: func() finops.EditionOptions { return options },
	})
	if status, body := changesRequest(t, app, "window=7d"); status != fiber.StatusOK {
		t.Fatalf("7d with a 7-day seam = %d %s", status, body)
	}
	if got := mounted.FinOpsOptions(); got != options {
		t.Fatalf("finops options = %+v", got)
	}
	// A lapsed license narrows the next request without a restart.
	limits = awssnapshot.DefaultChangesLimits()
	if status, body := changesRequest(t, app, "window=7d"); status != fiber.StatusForbidden {
		t.Fatalf("7d after the seam narrowed = %d %s", status, body)
	}
}

// TestMountWiresCostRetention: the refresh deletes by the storage seam and
// reads by the edition retention. Unset, both are the open-source 13; a
// lapsed license narrows what is shown and leaves storage alone.
func TestMountWiresCostRetention(t *testing.T) {
	oss := mountForTest(t, viewerApp(), Deps{})
	opts := oss.CostOptions(c.AWSConnectorConfig{}, nil, nil)
	if oss.CostStorageRetentionMonths() != 13 || opts.RetentionMonths() != 13 || opts.VisibleMonths() != 13 {
		t.Fatalf("open-source retention %d, options %d %d", oss.CostStorageRetentionMonths(), opts.RetentionMonths(), opts.VisibleMonths())
	}
	licensed := true
	mounted := mountForTest(t, viewerApp(), Deps{
		CostStorageRetentionMonths: func() int { return 24 },
		FinOpsOptions: func() finops.EditionOptions {
			if !licensed {
				return finops.DefaultEditionOptions()
			}
			return finops.EditionOptions{RetentionMonths: 24}
		},
	})
	opts = mounted.CostOptions(c.AWSConnectorConfig{}, nil, nil)
	if opts.RetentionMonths() != 24 || opts.VisibleMonths() != 24 {
		t.Fatalf("licensed options %d %d", opts.RetentionMonths(), opts.VisibleMonths())
	}
	licensed = false
	if opts.RetentionMonths() != 24 || opts.VisibleMonths() != 13 {
		t.Fatalf("lapsed options %d %d, want storage 24 and shown 13", opts.RetentionMonths(), opts.VisibleMonths())
	}
}

// TestMountRequiresItsDependencies: a missing store, config or router is an
// error, not a panic at the first request.
func TestMountRequiresItsDependencies(t *testing.T) {
	app := fiber.New()
	for name, deps := range map[string]Deps{
		"store":  {Config: &c.Config{}, Router: app.Group("/api")},
		"config": {Store: storage.NewMemory(), Router: app.Group("/api")},
		"router": {Store: storage.NewMemory(), Config: &c.Config{}},
	} {
		if _, err := Mount(deps); err == nil {
			t.Errorf("mount without %s succeeded", name)
		}
	}
}

// TestWaitForStopHonoursGrace: a part that never stops holds shutdown only
// for the grace period.
func TestWaitForStopHonoursGrace(t *testing.T) {
	never := make(chan struct{})
	started := time.Now()
	if waitForStop(never, 20*time.Millisecond) || time.Since(started) > time.Second {
		t.Fatal("grace period not honoured")
	}
	done := make(chan struct{})
	close(done)
	if !waitForStop(done, time.Second) {
		t.Fatal("a stopped part reported as running")
	}
}
