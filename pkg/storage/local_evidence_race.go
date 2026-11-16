//go:build race

package storage

import "time"

// Race instrumentation slows the embedded SQLite engine about 40x, and the read
// deadline is a production latency bound rather than a correctness property.
// The raise lives in a race-only build file, not a test file, so every
// race-instrumented test binary that reads local evidence through this package
// gets it, including the cluster history tests in other packages.
func init() {
	localEvidenceReadTimeout = 30 * time.Second
}
