//go:build darwin

package storage_test

import "golang.org/x/sys/unix"

// exchangePaths swaps two paths in one step.
func exchangePaths(a, b string) error { return unix.RenamexNp(a, b, unix.RENAME_SWAP) }
