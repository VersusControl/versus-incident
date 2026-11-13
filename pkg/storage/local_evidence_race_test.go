//go:build race

package storage

import "time"

// scaledTimeout stretches a test wait budget for race instrumentation. Waits
// stay condition-based; only the point at which they give up moves.
func scaledTimeout(d time.Duration) time.Duration { return 5 * d }
