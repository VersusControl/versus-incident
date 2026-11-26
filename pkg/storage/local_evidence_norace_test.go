//go:build !race

package storage

import "time"

// scaledTimeout returns a test wait budget unchanged without race
// instrumentation.
func scaledTimeout(d time.Duration) time.Duration { return d }

// largestSaveBound is how long the largest accepted evidence save may take
// on deep history: well inside the save lease.
const largestSaveBound = 10 * time.Second

// raceDetectorEnabled reports a race-instrumented test build.
const raceDetectorEnabled = false
