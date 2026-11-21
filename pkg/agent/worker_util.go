package agent

import (
	"os"
	"time"
)

// ANSI color codes used to make agent detection lines stand out from
// Versus's own error/info logs in the terminal. Disabled automatically when
// stdout is not a TTY (e.g. piped to a file or shipped to a log collector).
var (
	colorGreen = "\033[32m"
	colorReset = "\033[0m"
)

func init() {
	// Only emit color when stdout is a terminal. Best-effort — we don't import
	// a full TTY-detection library; checking for the env hint is enough.
	if os.Getenv("NO_COLOR") != "" {
		colorGreen = ""
		colorReset = ""
		return
	}
	fi, err := os.Stdout.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		colorGreen = ""
		colorReset = ""
	}
}

// -----------------------------------------------------------------------------
// utility
// -----------------------------------------------------------------------------

func parseDurationOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
