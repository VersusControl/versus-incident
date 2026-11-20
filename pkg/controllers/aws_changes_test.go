package controllers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

func changesApp(t *testing.T, permissions map[string]bool) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		for p, allowed := range permissions {
			middleware.SetRequestPermission(ctx, p, allowed)
		}
		return ctx.Next()
	})
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope())
	NewAWSChangesController(snapshot.NewChangeFeed(store)).Register(app.Group("/api"))
	return app
}

// OSS serves 1 h and 24 h; a longer window is a quiet Enterprise lock, not
// an error, and the page needs the infrastructure view.
func TestChangesRouteLocksLongWindows(t *testing.T) {
	app := changesApp(t, map[string]bool{string(core.PermissionInfrastructureView): true})
	for _, window := range []string{"7d", "168h", "25h"} {
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes?window="+window, "")
		if res.StatusCode != fiber.StatusForbidden || string(body) != `{"error":"window_locked","max_window":"24h","tier":"enterprise"}` {
			t.Fatalf("window %s = %d %s", window, res.StatusCode, body)
		}
	}
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes?window=1h", "")
	var answer snapshot.ChangesAnswer
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &answer) != nil || answer.Complete || len(answer.Buckets) != 12 {
		t.Fatalf("1h window = %d %s", res.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"bucket_seconds":300`)) {
		t.Fatalf("1h window lacks bucket_seconds of five minutes: %s", body)
	}
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes?window=24h", "")
	var day snapshot.ChangesAnswer
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &day) != nil || len(day.Buckets) != 24 || !bytes.Contains(body, []byte(`"bucket_seconds":3600`)) {
		t.Fatalf("24h window = %d %s", res.StatusCode, body)
	}
	if answer.Limits != (snapshot.ChangesLimitsView{MaxWindow: "24h", MaxCompareAge: "24h"}) {
		t.Fatalf("default limits = %+v, want 24h for both", answer.Limits)
	}
	if res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/groups/burst-nope", ""); res.StatusCode != fiber.StatusNotFound {
		t.Fatalf("unknown group = %d", res.StatusCode)
	}
	denied := changesApp(t, map[string]bool{string(core.PermissionInfrastructureView): false})
	if res, _ := awsTestRequest(t, denied, http.MethodGet, "/api/admin/aws/changes", ""); res.StatusCode != fiber.StatusForbidden {
		t.Fatalf("without view = %d", res.StatusCode)
	}
}

// The FinOps view alone does not open the Changes page: every Changes route
// needs the infrastructure view.
func TestChangesRoutesRefuseFinOpsOnlyCaller(t *testing.T) {
	app := changesApp(t, map[string]bool{string(core.PermissionFinOpsView): true, string(core.PermissionInfrastructureView): false})
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/admin/aws/changes", ""},
		{http.MethodGet, "/api/admin/aws/changes/groups/burst-nope", ""},
		{http.MethodGet, "/api/admin/aws/changes/compare", ""},
		{http.MethodGet, "/api/admin/aws/changes/preferences", ""},
		{http.MethodPut, "/api/admin/aws/changes/preferences", `{"services":[]}`},
	} {
		res, body := awsTestRequest(t, app, route.method, route.path, route.body)
		if res.StatusCode != fiber.StatusForbidden || string(body) != `{"code":"permission_required","error":"infrastructure:view permission is required","permission":"infrastructure:view"}` {
			t.Fatalf("%s %s with only finops:view = %d %s", route.method, route.path, res.StatusCode, body)
		}
	}
}

// An exact span: buckets are five minutes up to an hour and an hour
// beyond; a span over 24 h or starting more than 24 h ago is locked, and a
// start after the end is a bad request.
func TestChangesExactSpanBounds(t *testing.T) {
	app := changesApp(t, map[string]bool{string(core.PermissionInfrastructureView): true})
	now := time.Now().UTC()
	at := func(ago time.Duration) string { return url.QueryEscape(now.Add(-ago).Format(time.RFC3339)) }
	for _, tc := range []struct {
		query   string
		status  int
		buckets int
	}{
		{"since=" + at(30*time.Minute) + "&until=" + at(0), fiber.StatusOK, 6},
		{"since=" + at(3*time.Hour) + "&until=" + at(time.Hour) + "&window=1h", fiber.StatusOK, 2},
		{"since=" + at(25*time.Hour), fiber.StatusForbidden, 0},
		{"since=" + at(23*time.Hour) + "&until=" + at(25*time.Hour), fiber.StatusBadRequest, 0},
	} {
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes?"+tc.query, "")
		if res.StatusCode != tc.status {
			t.Fatalf("%s = %d %s", tc.query, res.StatusCode, body)
		}
		if tc.status == fiber.StatusOK {
			var answer snapshot.ChangesAnswer
			if err := json.Unmarshal(body, &answer); err != nil || len(answer.Buckets) != tc.buckets {
				t.Fatalf("%s buckets = %d, want %d", tc.query, len(answer.Buckets), tc.buckets)
			}
		}
	}
}

// Compare inside 24 h answers; a start further back is locked.
func TestCompareRouteBounds(t *testing.T) {
	app := changesApp(t, map[string]bool{string(core.PermissionInfrastructureView): true})
	now := time.Now().UTC()
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/compare?from="+url.QueryEscape(now.Add(-2*time.Hour).Format(time.RFC3339)), "")
	var answer snapshot.CompareAnswer
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &answer) != nil || answer.Status != snapshot.CompareUnavailable {
		t.Fatalf("compare = %d %s", res.StatusCode, body)
	}
	if answer.Limits != (snapshot.ChangesLimitsView{MaxWindow: "24h", MaxCompareAge: "24h"}) {
		t.Fatalf("default compare limits = %+v, want 24h for both", answer.Limits)
	}
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/compare?from="+url.QueryEscape(now.Add(-30*time.Hour).Format(time.RFC3339)), "")
	if res.StatusCode != fiber.StatusForbidden {
		t.Fatalf("compare beyond 24h = %d %s", res.StatusCode, body)
	}
	if res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/compare", ""); res.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("compare without from = %d", res.StatusCode)
	}
}

// TestChangesRouteLimitsSeamUnlocksLongerWindows: a build that passes
// longer limits through the feed serves a 7d window and a compare 30 h back;
// past those limits it still answers window_locked, naming its own window.
func TestChangesRouteLimitsSeamUnlocksLongerWindows(t *testing.T) {
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	week := 7 * 24 * time.Hour
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope())
	feed := snapshot.NewChangeFeed(store, snapshot.WithChangesLimits(func() snapshot.ChangesLimits {
		return snapshot.ChangesLimits{MaxWindow: week, MaxCompareAge: week}
	}))
	NewAWSChangesController(feed).Register(app.Group("/api"))
	for _, window := range []string{"7d", "168h", "2d"} {
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes?window="+window, "")
		var answer snapshot.ChangesAnswer
		if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &answer) != nil {
			t.Fatalf("window %s = %d %s", window, res.StatusCode, body)
		}
		if span := answer.Window.Until.Sub(answer.Window.Since); window == "7d" && span != week {
			t.Fatalf("7d window spans %s", span)
		}
		if !bytes.Contains(body, []byte(`"bucket_seconds":21600`)) {
			t.Fatalf("window %s answer lacks bucket_seconds of six hours: %s", window, body)
		}
		if wantBuckets := map[string]int{"7d": 28, "168h": 28, "2d": 8}[window]; len(answer.Buckets) != wantBuckets || answer.BucketSeconds != 21600 {
			t.Fatalf("window %s = %d buckets of %d s, want %d of 21600 s", window, len(answer.Buckets), answer.BucketSeconds, wantBuckets)
		}
		if answer.Limits != (snapshot.ChangesLimitsView{MaxWindow: "168h", MaxCompareAge: "168h"}) {
			t.Fatalf("7-day seam limits = %+v, want 168h for both", answer.Limits)
		}
	}
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes?window=8d", "")
	if res.StatusCode != fiber.StatusForbidden || string(body) != `{"error":"window_locked","max_window":"7d","tier":"enterprise"}` {
		t.Fatalf("8d window = %d %s", res.StatusCode, body)
	}
	now := time.Now().UTC()
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/compare?from="+url.QueryEscape(now.Add(-30*time.Hour).Format(time.RFC3339)), "")
	var compared snapshot.CompareAnswer
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &compared) != nil {
		t.Fatalf("compare 30h back with a 7-day limit = %d %s", res.StatusCode, body)
	}
	if compared.Limits != (snapshot.ChangesLimitsView{MaxWindow: "168h", MaxCompareAge: "168h"}) {
		t.Fatalf("7-day seam compare limits = %+v, want 168h for both", compared.Limits)
	}
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/compare?from="+url.QueryEscape(now.Add(-8*24*time.Hour).Format(time.RFC3339)), "")
	if res.StatusCode != fiber.StatusForbidden {
		t.Fatalf("compare 8 days back with a 7-day limit = %d %s", res.StatusCode, body)
	}
}

// preferencesApp serves the Changes routes to one signed-in actor with
// preferences kept in the memory store.
func preferencesApp(t *testing.T, provider storage.Provider, actor string, view bool) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), view)
		// A read-only caller: no settings write.
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite), false)
		if actor != "" {
			middleware.SetRequestActor(ctx, actor)
		}
		return ctx.Next()
	})
	store := snapshot.New(provider, tenancy.DefaultOrgScope())
	NewAWSChangesController(snapshot.NewChangeFeed(store)).WithPreferences(snapshot.NewChangePreferenceStore(provider)).Register(app.Group("/api"))
	return app
}

// The changes and compare routes take a comma-separated service list; the
// changes answer carries the service list and the suspect's in_filter mark.
func TestChangesRouteServiceFilter(t *testing.T) {
	app := changesApp(t, map[string]bool{string(core.PermissionInfrastructureView): true})
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes?service="+url.QueryEscape("rds,iam"), "")
	var view map[string]json.RawMessage
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &view) != nil || string(view["services"]) != "[]" {
		t.Fatalf("filtered changes = %d %s", res.StatusCode, body)
	}
	var suspect map[string]any
	if json.Unmarshal(view["suspect"], &suspect) != nil || suspect["in_filter"] != true {
		t.Fatalf("suspect = %s", view["suspect"])
	}
	for _, path := range []string{"/api/admin/aws/changes?service=" + url.QueryEscape("rds,a b"),
		"/api/admin/aws/changes/compare?service=" + url.QueryEscape("<x>") + "&from=" + url.QueryEscape(time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))} {
		if res, body := awsTestRequest(t, app, http.MethodGet, path, ""); res.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, res.StatusCode, body)
		}
	}
	from := url.QueryEscape(time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339))
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/compare?service=rds,iam&from="+from, "")
	var answer snapshot.CompareAnswer
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &answer) != nil {
		t.Fatalf("filtered compare = %d %s", res.StatusCode, body)
	}
}

// Each signed-in user reads and saves their own service filter, read-only
// users included; the default is RDS and IAM and an empty list is kept.
func TestChangesPreferencesRoutes(t *testing.T) {
	provider := storage.NewMemory()
	alice := preferencesApp(t, provider, "alice@example.com", true)
	const path = "/api/admin/aws/changes/preferences"
	if res, body := awsTestRequest(t, alice, http.MethodGet, path, ""); res.StatusCode != fiber.StatusOK || string(body) != `{"services":["rds","iam"]}` {
		t.Fatalf("default = %d %s", res.StatusCode, body)
	}
	if res, body := awsTestRequest(t, alice, http.MethodPut, path, `{"services":["EC2","made-up"]}`); res.StatusCode != fiber.StatusOK || string(body) != `{"services":["ec2","made-up"]}` {
		t.Fatalf("save = %d %s", res.StatusCode, body)
	}
	if res, body := awsTestRequest(t, alice, http.MethodGet, path, ""); res.StatusCode != fiber.StatusOK || string(body) != `{"services":["ec2","made-up"]}` {
		t.Fatalf("read back = %d %s", res.StatusCode, body)
	}
	bob := preferencesApp(t, provider, "bob@example.com", true)
	if res, body := awsTestRequest(t, bob, http.MethodGet, path, ""); string(body) != `{"services":["rds","iam"]}` {
		t.Fatalf("another user = %d %s", res.StatusCode, body)
	}
	if res, body := awsTestRequest(t, bob, http.MethodPut, path, `{"services":[]}`); res.StatusCode != fiber.StatusOK || string(body) != `{"services":[]}` {
		t.Fatalf("save all = %d %s", res.StatusCode, body)
	}
	if _, body := awsTestRequest(t, bob, http.MethodGet, path, ""); string(body) != `{"services":[]}` {
		t.Fatalf("empty list read back = %s", body)
	}
	for _, bad := range []string{`{}`, `not json`, `{"services":["a b"]}`, `{"services":"rds"}`} {
		if res, body := awsTestRequest(t, alice, http.MethodPut, path, bad); res.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("PUT %s = %d %s", bad, res.StatusCode, body)
		}
	}
	anonymous := preferencesApp(t, provider, "", true)
	if res, _ := awsTestRequest(t, anonymous, http.MethodGet, path, ""); res.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("no user = %d", res.StatusCode)
	}
	denied := preferencesApp(t, provider, "carol@example.com", false)
	if res, _ := awsTestRequest(t, denied, http.MethodPut, path, `{"services":["rds"]}`); res.StatusCode != fiber.StatusForbidden {
		t.Fatalf("without view = %d", res.StatusCode)
	}
}

// An unexpected read failure on the Changes list or a FinOps read answers
// the same generic 500 body, and the server logs the wrapped storage error
// once with the route. An expected error (not found) logs nothing.
func TestInternalReadErrorLogsCauseOnce(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	cause := fmt.Errorf("aws changes: read feed: %w", errors.New("database is locked (5) (SQLITE_BUSY)"))
	changes := &AWSChangesController{}
	app := fiber.New()
	app.Get("/api/admin/aws/changes", func(ctx *fiber.Ctx) error { return changes.changesError(ctx, cause) })
	app.Get("/api/admin/aws/finops/accounts", func(ctx *fiber.Ctx) error { return writeFinOps(ctx, nil, cause) })
	app.Get("/api/admin/aws/finops/missing", func(ctx *fiber.Ctx) error { return writeFinOps(ctx, nil, finops.ErrNotFound) })

	for path, body := range map[string]string{
		"/api/admin/aws/changes":         `{"error":"changes could not be read"}`,
		"/api/admin/aws/finops/accounts": `{"error":"internal"}`,
	} {
		logs.Reset()
		response, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusInternalServerError || string(got) != body {
			t.Fatalf("%s answered %d %s", path, response.StatusCode, got)
		}
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if len(lines) != 1 || !strings.Contains(lines[0], path) || !strings.Contains(lines[0], "SQLITE_BUSY") || !strings.Contains(lines[0], "ERROR") {
			t.Fatalf("%s logged %q", path, logs.String())
		}
	}
	logs.Reset()
	if response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/aws/finops/missing", nil)); err != nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("not found answered %v, %v", response, err)
	}
	if logs.Len() != 0 {
		t.Fatalf("an expected error logged %q", logs.String())
	}
}
