package middleware

import (
	"log"
	"strings"

	"github.com/gofiber/fiber/v2"
)

func Logger() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Process request first so we can branch on the response status.
		err := c.Next()

		status := c.Response().StatusCode()
		path := c.Path()
		method := c.Method()

		// Always silence the health probe.
		if path == "/healthz" {
			return err
		}

		// Silence noisy polling reads from the admin UI: every successful
		// GET to /api/agent/* or /api/admin/* (the dashboard polls these
		// every few seconds). Mutations (POST/DELETE) and any non-2xx
		// response still get logged.
		// Also silence successful static asset / UI page requests.
		if status < 400 && method == fiber.MethodGet &&
			(strings.HasPrefix(path, "/api/agent/") ||
				strings.HasPrefix(path, "/api/admin/") ||
				!strings.HasPrefix(path, "/api/")) {
			return err
		}

		clientIP := c.IP()
		userAgent := c.Get("User-Agent")

		if status >= 400 {
			log.Printf(
				"%s %s %d %v %s %s",
				method,
				path,
				status,
				clientIP,
				userAgent,
				c.Response().Body(),
			)
		} else {
			log.Printf(
				"%s %s %d %v %s",
				method,
				path,
				status,
				clientIP,
				userAgent,
			)
		}

		if err != nil {
			log.Printf("Error: %v", err)
		}

		return err
	}
}
