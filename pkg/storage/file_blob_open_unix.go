//go:build unix

package storage

import "io/fs"

// blobLinkMode reports a file mode a blob file may not have: a symbolic link.
func blobLinkMode(mode fs.FileMode) bool { return mode&fs.ModeSymlink != 0 }
