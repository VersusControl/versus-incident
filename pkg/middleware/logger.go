package middleware

import (
	"log"

	"github.com/gofiber/fiber/v2"
)

func Logger() fiber.Handler {
	return func(c *fiber.Ctx) error {

		if c.Path() == "/healthz" {
			return c.Next()
		}

		// Process request
		err := c.Next()

		status := c.Response().StatusCode()
		clientIP := c.IP()
		userAgent := c.Get("User-Agent")

		if status >= 400 {
			log.Printf(
				"%s %s %d %v %s %s",
				c.Method(),
				c.Path(),
				status,
				clientIP,
				userAgent,
				c.Response().Body(),
			)
		} else {
			log.Printf(
				"%s %s %d %v %s",
				c.Method(),
				c.Path(),
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
