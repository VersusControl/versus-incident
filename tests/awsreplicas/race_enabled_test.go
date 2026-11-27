//go:build race

package awsreplicas

import "time"

// scaledTimeout stretches a hang guard for race instrumentation. Waits stay
// condition-based; only the point at which they give up moves.
func scaledTimeout(d time.Duration) time.Duration { return 5 * d }
