// Package sqlitedriver registers the optional provider-private SQLite engine.
// Executable and test hosts import it; Kubernetes/auth libraries remain free of
// driver implementation dependencies.
package sqlitedriver

import _ "modernc.org/sqlite"
