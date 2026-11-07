//go:build race

package storage

import "time"

// Race instrumentation slows the embedded SQLite engine about 40x, and the read
// deadline is a production latency bound rather than a correctness property.
func init() {
	localEvidenceReadTimeout = 30 * time.Second
}
