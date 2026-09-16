//go:build !darwin && !linux

package storage

import (
	"context"
	"fmt"
)

var errSecureModelStateUnsupported = fmt.Errorf("storage: secure model-state file access is unsupported on this platform: %w", ErrUnsupported)

func validateSecureStorageRoot(string) error { return errSecureModelStateUnsupported }

func parseModelStateName(name string) ([]string, string, bool) {
	return nil, "", false
}

func readSecureModelStateBlob(context.Context, string, string, int64) ([]byte, error) {
	return nil, errSecureModelStateUnsupported
}

func writeSecureModelStateBlob(string, string, []byte, bool) error {
	return errSecureModelStateUnsupported
}

func deleteSecureModelStateBlob(string, string) error { return errSecureModelStateUnsupported }

func compareAndSwapSecureModelStateBlob(string, string, []byte, []byte) (bool, error) {
	return false, errSecureModelStateUnsupported
}

func listSecureModelStateBlobs(string, string) ([]Blob, error) {
	return nil, errSecureModelStateUnsupported
}

func listSecureModelStatePage(context.Context, string, string, string, int) (BlobPage, error) {
	return BlobPage{}, errSecureModelStateUnsupported
}
