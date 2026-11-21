//go:build !unix

package storage

import "io/fs"

// blobLinkMode reports a file mode a blob file may not have: a symbolic link
// or another reparse point, which Windows reports as irregular.
func blobLinkMode(mode fs.FileMode) bool { return mode&(fs.ModeSymlink|fs.ModeIrregular) != 0 }
