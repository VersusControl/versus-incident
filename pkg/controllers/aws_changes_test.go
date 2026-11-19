package controllers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
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
	if res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/groups/burst-nope", ""); res.StatusCode != fiber.StatusNotFound {
		t.Fatalf("unknown group = %d", res.StatusCode)
	}
	denied := changesApp(t, map[string]bool{string(core.PermissionInfrastructureView): false})
	if res, _ := awsTestRequest(t, denied, http.MethodGet, "/api/admin/aws/changes", ""); res.StatusCode != fiber.StatusForbidden {
		t.Fatalf("without view = %d", res.StatusCode)
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
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/compare?from="+url.QueryEscape(now.Add(-30*time.Hour).Format(time.RFC3339)), "")
	if res.StatusCode != fiber.StatusForbidden {
		t.Fatalf("compare beyond 24h = %d %s", res.StatusCode, body)
	}
	if res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/changes/compare", ""); res.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("compare without from = %d", res.StatusCode)
	}
}
