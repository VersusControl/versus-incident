package storage

// SetSessionLockQueried installs fn between the session lock query and the
// read of its answer, so an external test can cancel exactly there. The
// returned func restores the previous hook.
func SetSessionLockQueried(fn func()) (restore func()) {
	previous := sessionLockQueried
	sessionLockQueried = fn
	return func() { sessionLockQueried = previous }
}
