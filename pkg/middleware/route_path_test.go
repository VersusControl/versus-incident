package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func routePathFor(t *testing.T, config fiber.Config, method, target string) (path string, routed bool) {
	t.Helper()
	app := fiber.New(config)
	app.Use(func(c *fiber.Ctx) error {
		path = RoutePath(c)
		return c.Next()
	})
	app.Add(method, "/api/admin/kubernetes/agent-settings", func(c *fiber.Ctx) error {
		routed = true
		return c.SendStatus(fiber.StatusNoContent)
	})
	response, err := app.Test(httptest.NewRequest(method, target, nil))
	if err != nil {
		t.Fatalf("request %s: %v", target, err)
	}
	_ = response.Body.Close()
	return path, routed
}

func TestRoutePathMatchesTheRouteFiberDispatches(t *testing.T) {
	for _, target := range []string{
		"/api/admin/kubernetes/agent-settings",
		"/api/admin/kubernetes/agent-settings/",
		"/api/admin/kubernetes/agent-settings//",
		"/api/admin/kubernetes/agent-settings///",
		"/API/Admin/Kubernetes/Agent-Settings//",
	} {
		path, routed := routePathFor(t, fiber.Config{}, fiber.MethodGet, target)
		if !routed {
			t.Fatalf("%s: router did not dispatch to the route", target)
		}
		if path != "/api/admin/kubernetes/agent-settings" {
			t.Fatalf("%s: RoutePath = %q, want the routed path", target, path)
		}
	}
}

func TestRoutePathKeepsInteriorDuplicateSlashesTheRouterDoesNotMatch(t *testing.T) {
	path, routed := routePathFor(t, fiber.Config{}, fiber.MethodGet, "/api//admin/kubernetes/agent-settings")
	if routed {
		t.Fatal("router dispatched a path with an interior duplicate slash")
	}
	if path == "/api/admin/kubernetes/agent-settings" {
		t.Fatalf("RoutePath = %q for a path the router did not match", path)
	}
}

func TestRoutePathCaseSensitiveAppKeepsCase(t *testing.T) {
	path, routed := routePathFor(t, fiber.Config{CaseSensitive: true}, fiber.MethodGet, "/API/Admin/Kubernetes/Agent-Settings//")
	if routed {
		t.Fatal("case-sensitive router dispatched a mixed-case path")
	}
	if path != "/API/Admin/Kubernetes/Agent-Settings" {
		t.Fatalf("RoutePath = %q, want case preserved and trailing slashes dropped", path)
	}
}

func TestRoutePathStrictRoutingKeepsTrailingSlashes(t *testing.T) {
	path, routed := routePathFor(t, fiber.Config{StrictRouting: true}, fiber.MethodGet, "/api/admin/kubernetes/agent-settings//")
	if routed {
		t.Fatal("strict router dispatched a path with trailing slashes")
	}
	if path != "/api/admin/kubernetes/agent-settings//" {
		t.Fatalf("RoutePath = %q, want the trailing slashes kept", path)
	}
}

func TestRoutePathRootStaysSlash(t *testing.T) {
	if path, _ := routePathFor(t, fiber.Config{}, fiber.MethodGet, "/"); path != "/" {
		t.Fatalf("RoutePath(/) = %q", path)
	}
}
