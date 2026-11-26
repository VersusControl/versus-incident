package storage

import (
	"runtime"
	"testing"
	"time"
)

// hangGuard is how long a test waits for an event before it calls the wait
// a hang. It is not a latency budget: a passing test never comes near it,
// so a loaded or race-instrumented run cannot fail a correct product by
// being slow. Race builds stretch it through scaledTimeout.
const hangGuard = 60 * time.Second

// eventually waits until cond reports true. It fails the test with what
// and a dump of every goroutine only when cond stays false for the whole
// hang guard.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(scaledTimeout(hangGuard))
	for pause := time.Millisecond; !cond(); pause = min(2*pause, 10*time.Millisecond) {
		if time.Now().After(deadline) {
			failHung(t, what)
		}
		time.Sleep(pause) // testlint:allow the shared wait helper's own poll pause
	}
}

// waitFor receives one value from ch, or the zero value once ch is closed.
// It fails the test with what and a goroutine dump only if nothing arrives
// within the hang guard.
func waitFor[T any](t testing.TB, ch <-chan T, what string) T {
	t.Helper()
	timer := time.NewTimer(scaledTimeout(hangGuard))
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
		failHung(t, what)
		var zero T
		return zero
	}
}

// failHung reports a wait that reached the hang guard, with every
// goroutine's stack so the blocked path is visible in the failure itself.
func failHung(t testing.TB, what string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	t.Fatalf("hang guard (%s) reached: %s; goroutines:\n%s", scaledTimeout(hangGuard), what, buf)
}
