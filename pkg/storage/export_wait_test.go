package storage

import "testing"

// Eventually is the package's shared condition wait, for external tests.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	eventually(t, what, cond)
}

// WaitFor is the package's shared channel wait, for external tests.
func WaitFor[T any](t testing.TB, ch <-chan T, what string) T {
	t.Helper()
	return waitFor(t, ch, what)
}
