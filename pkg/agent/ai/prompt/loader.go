// Package prompt is a content-free helper for assembling multi-file
// system prompts. Each AIAgent (detect, analyze, …) embeds its own
// prompts/*.md fragments and calls Assemble / MustAssemble with the
// canonical ordering. The loader never reaches into another package's
// prompts — operators tune one agent without touching another.
package prompt

import (
	"embed"
	"fmt"
	"strings"
)

// Assemble reads the named files from fs in order and concatenates
// them with a blank-line separator. Missing files return an error
// (no silent skip).
func Assemble(fs embed.FS, order []string) (string, error) {
	var b strings.Builder
	for i, name := range order {
		data, err := fs.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("prompt: read %q: %w", name, err)
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.Write(data)
	}
	return b.String(), nil
}

// MustAssemble is Assemble that panics on error. Intended for
// package-init wiring where a missing fragment is a programmer error
// (go:embed guarantees the files exist at build time, so a failure
// means the order slice drifted from the actual files on disk).
func MustAssemble(fs embed.FS, order []string) string {
	s, err := Assemble(fs, order)
	if err != nil {
		panic(err)
	}
	return s
}
