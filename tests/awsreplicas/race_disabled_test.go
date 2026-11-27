//go:build !race

package awsreplicas

import "time"

// scaledTimeout is the hang guard as written when the race detector is off.
func scaledTimeout(d time.Duration) time.Duration { return d }
