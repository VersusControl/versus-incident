package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"
)

// RoutePath returns the request path in the form the router matches it, for
// middleware that decides what to audit or which checks apply before the final
// route is known. It follows the app's routing configuration exactly: a
// case-insensitive app routes /API/Admin/X to the same handler as /api/admin/x,
// and non-strict routing drops every trailing slash, so /x// reaches the /x
// handler. Comparing the raw path instead would let such a request reach a
// handler while skipping the decision. Interior duplicate slashes are kept,
// because the router does not collapse them: /api//admin matches no /api/admin
// route.
func RoutePath(c *fiber.Ctx) string {
	path := c.Path()
	config := c.App().Config()
	if !config.CaseSensitive {
		path = strings.ToLower(path)
	}
	if !config.StrictRouting && len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}
