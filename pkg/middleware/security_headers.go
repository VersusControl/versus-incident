package middleware

import "github.com/gofiber/fiber/v2"

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; connect-src 'self'; img-src 'self' data:; font-src 'self'; form-action 'self'"

// SecurityHeaders applies browser hardening consistently to API and UI responses.
func SecurityHeaders() fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentSecurityPolicy, contentSecurityPolicy)
		c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
		c.Set(fiber.HeaderReferrerPolicy, "same-origin")
		c.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		return c.Next()
	}
}
