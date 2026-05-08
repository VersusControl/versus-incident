package controllers

import (
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/VersusControl/versus-incident/ui"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
)

// MountStaticUI registers the embedded UI at "/" with SPA fallback so
// client-side routes like /dashboard, /incidents/:id, /shadow/:id all
// resolve to index.html when the asset doesn't exist.
//
// Call this AFTER all API/admin routes are registered; otherwise the
// catch-all would shadow them.
func MountStaticUI(app *fiber.App) {
	dist := ui.Dist()

	// First check if the build is present. If only `.gitkeep` is there
	// (e.g. running the binary before `npm run build`), surface a clear
	// hint at "/" instead of an empty 404.
	if !uiBuilt(dist) {
		app.Get("/", func(c *fiber.Ctx) error {
			c.Type("html")
			return c.SendString(uiMissingPage)
		})
		return
	}

	// Serve hashed assets and any direct file hit from the embedded FS.
	// The Next callback skips the middleware for paths that don't map to a
	// real file so the SPA fallback below can handle them.
	app.Use("/", filesystem.New(filesystem.Config{
		Root:   http.FS(dist),
		Browse: false,
		Index:  "index.html",
		Next: func(c *fiber.Ctx) bool {
			path := c.Path()
			// Let the SPA fallback handle client-side routes (anything
			// that isn't a real asset file). Real assets have extensions.
			if path == "/" {
				return false // serve index.html
			}
			_, err := fs.Stat(dist, strings.TrimPrefix(path, "/"))
			return err != nil // skip if file doesn't exist
		},
	}))

	// SPA fallback: anything under "/" that isn't an asset, isn't an API
	// route, and isn't already matched by the FS handler should serve
	// index.html so the React router can take over.
	app.Use(func(c *fiber.Ctx) error {
		path := c.Path()
		if strings.HasPrefix(path, "/api/") ||
			strings.HasPrefix(path, "/healthz") {
			return fiber.ErrNotFound
		}
		f, err := dist.Open("index.html")
		if err != nil {
			return fiber.ErrNotFound
		}
		defer f.Close()
		body, err := io.ReadAll(f)
		if err != nil {
			return fiber.ErrInternalServerError
		}
		c.Status(fiber.StatusOK)
		c.Type("html")
		return c.Send(body)
	})
}

// uiBuilt reports whether the embedded dist directory contains a real
// build (presence of index.html), as opposed to just the .gitkeep
// placeholder shipped before the first `npm run build`.
func uiBuilt(dist fs.FS) bool {
	_, err := fs.Stat(dist, "index.html")
	return err == nil
}

const uiMissingPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Versus Incident</title>
<style>
  body{font-family:ui-sans-serif,system-ui,sans-serif;background:#0f172a;color:#e2e8f0;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center}
  .card{max-width:560px;padding:32px;background:#1e293b;border-radius:12px;box-shadow:0 10px 30px rgba(0,0,0,.4)}
  code{background:#0f172a;padding:2px 6px;border-radius:4px;color:#a5b4fc}
  h1{margin:0 0 12px;font-size:20px}
  p{line-height:1.55;color:#cbd5e1}
  a{color:#a5b4fc}
</style></head>
<body><div class="card">
<h1>Versus Incident — UI not built yet</h1>
<p>The Go binary embeds the dashboard from <code>ui/dist/</code>, but no build was found.</p>
<p>Build it with:</p>
<p><code>./scripts/build_ui.sh</code></p>
<p>or during development:</p>
<p><code>./scripts/watch_ui.sh</code></p>
<p>API still available at <a href="/api/incidents">/api/incidents</a> and <a href="/healthz">/healthz</a>.</p>
</div></body></html>`
