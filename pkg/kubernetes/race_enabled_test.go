//go:build race

package kubernetes

import "time"

// scaledTimeout stretches a test wait budget for race instrumentation. Waits
// stay condition-based; only the point at which they give up moves.
func scaledTimeout(d time.Duration) time.Duration { return 5 * d }

// The persisted history read deadline is a production latency bound, not a
// correctness property, so race builds get the same headroom the storage
// engine's own read deadline receives.
func init() {
	persistedHistoryReadTimeout = 30 * time.Second
}
