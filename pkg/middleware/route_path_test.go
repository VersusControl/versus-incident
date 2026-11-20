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
	app.Add(method, "/api/admin/aws/external-id", func(c *fiber.Ctx) error {
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
		"/api/admin/aws/external-id",
		"/api/admin/aws/external-id/",
		"/api/admin/aws/external-id//",
		"/api/admin/aws/external-id///",
		"/API/Admin/AWS/External-ID//",
	} {
		path, routed := routePathFor(t, fiber.Config{}, fiber.MethodGet, target)
		if !routed {
			t.Fatalf("%s: router did not dispatch to the route", target)
		}
		if path != "/api/admin/aws/external-id" {
			t.Fatalf("%s: RoutePath = %q, want the routed path", target, path)
		}
	}
}

func TestRoutePathKeepsInteriorDuplicateSlashesTheRouterDoesNotMatch(t *testing.T) {
	path, routed := routePathFor(t, fiber.Config{}, fiber.MethodGet, "/api//admin/aws/external-id")
	if routed {
		t.Fatal("router dispatched a path with an interior duplicate slash")
	}
	if path == "/api/admin/aws/external-id" {
		t.Fatalf("RoutePath = %q for a path the router did not match", path)
	}
}

func TestRoutePathCaseSensitiveAppKeepsCase(t *testing.T) {
	path, routed := routePathFor(t, fiber.Config{CaseSensitive: true}, fiber.MethodGet, "/API/Admin/AWS/External-ID//")
	if routed {
		t.Fatal("case-sensitive router dispatched a mixed-case path")
	}
	if path != "/API/Admin/AWS/External-ID" {
		t.Fatalf("RoutePath = %q, want case preserved and trailing slashes dropped", path)
	}
}

func TestRoutePathStrictRoutingKeepsTrailingSlashes(t *testing.T) {
	path, routed := routePathFor(t, fiber.Config{StrictRouting: true}, fiber.MethodGet, "/api/admin/aws/external-id//")
	if routed {
		t.Fatal("strict router dispatched a path with trailing slashes")
	}
	if path != "/api/admin/aws/external-id//" {
		t.Fatalf("RoutePath = %q, want the trailing slashes kept", path)
	}
}

func TestRoutePathRootStaysSlash(t *testing.T) {
	if path, _ := routePathFor(t, fiber.Config{}, fiber.MethodGet, "/"); path != "/" {
		t.Fatalf("RoutePath(/) = %q", path)
	}
}
