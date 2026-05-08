package services

import "github.com/VersusControl/versus-incident/pkg/storage"

// store holds the process-wide storage provider used to persist
// incidents. Set once at startup by main.SetStorage. Nil-safe: callers
// MUST guard against a nil store so unit tests that don't wire one in
// still work.
var store storage.Provider

// SetStorage installs the storage provider used to persist incidents and
// record acks. Called once from main after config load.
func SetStorage(p storage.Provider) { store = p }

// Storage returns the currently installed storage provider, or nil when
// the process is running without persistence (older tests).
func Storage() storage.Provider { return store }
