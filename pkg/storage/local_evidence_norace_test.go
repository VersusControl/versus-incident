//go:build !race

package storage

import "time"

// scaledTimeout returns a test wait budget unchanged without race
// instrumentation.
func scaledTimeout(d time.Duration) time.Duration { return d }

// finalSaveBound is how long one evidence save transaction may take in the
// capture-limit test: well inside the save lease.
const finalSaveBound = 5 * time.Second

// largestSaveBound is how long the largest accepted evidence save may take
// on deep history: well inside the save lease.
const largestSaveBound = 10 * time.Second
