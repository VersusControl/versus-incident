//go:build linux

package storage_test

import "golang.org/x/sys/unix"

// exchangePaths swaps two paths in one step.
func exchangePaths(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
