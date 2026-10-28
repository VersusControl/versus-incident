package middleware

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestSecurityHeadersAppliesToAPIAndStaticResponses(t *testing.T) {
	app := fiber.New()
	app.Use(SecurityHeaders())
	app.Get("/api/check", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })
	app.Get("/", func(c *fiber.Ctx) error { return c.Type("html").SendString("<!doctype html>") })

	for _, path := range []string{"/api/check", "/"} {
		request, err := http.NewRequest(http.MethodGet, "http://example.test"+path, nil)
		if err != nil {
			t.Fatalf("build GET %s: %v", path, err)
		}
		response, err := app.Test(request)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		if got := response.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") || !strings.Contains(got, "frame-ancestors 'none'") {
			t.Errorf("GET %s CSP = %q", path, got)
		}
		if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q", path, got)
		}
		if got := response.Header.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("GET %s Referrer-Policy = %q", path, got)
		}
		if got := response.Header.Get("Permissions-Policy"); !strings.Contains(got, "camera=()") {
			t.Errorf("GET %s Permissions-Policy = %q", path, got)
		}
	}
}
