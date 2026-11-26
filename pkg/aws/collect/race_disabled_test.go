//go:build !race

package collect

import "time"

// scaledTimeout returns a test wait budget unchanged without race
// instrumentation.
func scaledTimeout(d time.Duration) time.Duration { return d }
