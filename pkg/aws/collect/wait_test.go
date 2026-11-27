package collect

import (
	"runtime"
	"testing"
	"time"
)

// hangGuard is how long a test waits for an event before it calls the wait a
// hang. It is not a latency budget: a passing test never comes near it, so a
// loaded or race-instrumented run cannot fail a correct product by being slow.
// Race builds stretch it through scaledTimeout.
const hangGuard = 60 * time.Second

// eventually waits until cond reports true. It fails the test with what and a
// dump of every goroutine only when cond stays false for the whole hang guard.
// Prefer waitFor when the product exposes a channel for the event.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	eventuallyWithin(t, hangGuard, what, cond)
}

// eventuallyWithin is eventually with another hang guard, for waits that
// cover real work proportional to a large fixture rather than a timer.
func eventuallyWithin(t testing.TB, guard time.Duration, what string, cond func() bool) {
	t.Helper()
	for wait := pollingWithin(t, guard, what); !cond(); wait.next() {
	}
}

// poll drives a wait loop whose body needs break, return or its own checks:
//
//	for wait := polling(t, "the first sync"); ; wait.next() {
//		if done() {
//			break
//		}
//	}
//
// next pauses briefly and fails the test with the description and a goroutine
// dump once the hang guard has passed.
type poll struct {
	t        testing.TB
	guard    time.Duration
	what     string
	deadline time.Time
	pause    time.Duration
}

func polling(t testing.TB, what string) *poll {
	return pollingWithin(t, hangGuard, what)
}

func pollingWithin(t testing.TB, guard time.Duration, what string) *poll {
	return &poll{t: t, guard: guard, what: what, deadline: time.Now().Add(scaledTimeout(guard)), pause: time.Millisecond}
}

func (wait *poll) next() {
	wait.t.Helper()
	if time.Now().After(wait.deadline) {
		failHung(wait.t, wait.guard, wait.what)
	}
	time.Sleep(wait.pause) // testlint:allow the shared wait helper's own poll pause
	if wait.pause < 10*time.Millisecond {
		wait.pause *= 2
	}
}

// waitFor receives one value from ch, or the zero value once ch is closed. It
// fails the test with what and a goroutine dump only if nothing arrives within
// the hang guard.
func waitFor[T any](t testing.TB, ch <-chan T, what string) T {
	t.Helper()
	timer := time.NewTimer(scaledTimeout(hangGuard))
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
		failHung(t, hangGuard, what)
		var zero T
		return zero
	}
}

// failHung reports a wait that reached the hang guard, with every goroutine's
// stack so the blocked product path is visible in the failure itself.
func failHung(t testing.TB, guard time.Duration, what string) {
	t.Helper()
	t.Fatalf("hang guard (%s) reached: %s; goroutines:\n%s", scaledTimeout(guard), what, goroutineStacks())
}

// goroutineStacks returns the stack of every goroutine.
func goroutineStacks() []byte {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}
