// Package ui exposes the compiled Vite/React dashboard as an embedded
// filesystem so the Go binary ships with its UI baked in.
//
// The contents of `ui/dist/` are produced by `npm run build` (or
// `scripts/build_ui.sh`). A `.gitkeep` placeholder lives in `ui/dist/`
// so this file compiles even before the first UI build; in that case
// the only entry will be `.gitkeep` and the static handler will 404
// for everything else.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist returns the built UI assets rooted at the `dist/` directory so
// callers can mount it directly (no leading `dist/` prefix in URLs).
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// fs.Sub on an embedded directory cannot fail at runtime; this
		// only triggers if the embed directive itself is broken.
		panic("ui: failed to scope embedded dist: " + err.Error())
	}
	return sub
}
