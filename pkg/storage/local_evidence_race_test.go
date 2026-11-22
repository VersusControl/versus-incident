//go:build race

package storage

import "time"

// scaledTimeout stretches a test wait budget for race instrumentation. Waits
// stay condition-based; only the point at which they give up moves.
func scaledTimeout(d time.Duration) time.Duration { return 5 * d }

// largestSaveBound is how long the largest accepted evidence save may take
// on deep history. Race instrumentation makes it about one and a half
// times slower (about 15 s), so the bound rises, yet it stays inside the
// 20 s save lease.
const largestSaveBound = 18 * time.Second

// raceDetectorEnabled reports a race-instrumented test build.
const raceDetectorEnabled = true
