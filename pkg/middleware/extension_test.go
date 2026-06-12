package middleware

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// resetSlots clears the process-wide registration slots so each test
// starts from community-mode defaults.
func resetSlots() {
	SetAuthMiddleware(nil)
	SetOrgResolver(nil)
}

func TestOrgInjectorDefaultsToDefaultOrg(t *testing.T) {
	resetSlots()
	app := fiber.New()
	app.Use(OrgInjector())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString(OrgFromContext(c))
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != storage.DefaultOrgID {
		t.Fatalf("org = %q, want %q", string(body), storage.DefaultOrgID)
	}
}

func TestOrgInjectorUsesRegisteredResolver(t *testing.T) {
	resetSlots()
	defer resetSlots()
	SetOrgResolver(func(c *fiber.Ctx) string {
		return c.Get("X-Org")
	})

	app := fiber.New()
	app.Use(OrgInjector())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString(OrgFromContext(c))
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Org", "acme")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "acme" {
		t.Fatalf("org = %q, want acme", string(body))
	}
}

func TestOrgFromContextWithoutInjector(t *testing.T) {
	resetSlots()
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error {
		// No OrgInjector mounted — must still return the default org.
		return c.SendString(OrgFromContext(c))
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	body, _ := io.ReadAll(resp.Body)
	if string(body) != storage.DefaultOrgID {
		t.Fatalf("org = %q, want %q", string(body), storage.DefaultOrgID)
	}
}

func TestAuthMiddlewareDefaultIsPassThrough(t *testing.T) {
	resetSlots()
	app := fiber.New()
	app.Use(AuthMiddleware())
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString("ok") })

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuthMiddlewareRegisteredHandlerRuns(t *testing.T) {
	resetSlots()
	defer resetSlots()
	SetAuthMiddleware(func(c *fiber.Ctx) error {
		if c.Get("X-Token") != "secret" {
			return c.Status(fiber.StatusUnauthorized).SendString("denied")
		}
		return c.Next()
	})

	app := fiber.New()
	app.Use(AuthMiddleware())
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString("ok") })

	// Missing token — rejected.
	resp, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// Valid token — allowed.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Token", "secret")
	resp, _ = app.Test(req)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
