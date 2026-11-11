package storage

import "errors"

// DeleteBlob removes the blob stored under name using whichever exact-key
// delete the backend offers: the BlobDeleter capability first, then the
// Lifecycle blob domain. It returns nil when the blob was removed,
// ErrNotFound when it did not exist, and ErrUnsupported when the backend
// cannot remove this key at all. ErrUnsupported never means the blob is
// gone; a caller that needs it gone must fall back to its own handling,
// for example overwriting the payload with an empty value its readers
// treat as absent.
func DeleteBlob(p Provider, name string) error {
	if deleter, ok := p.(BlobDeleter); ok {
		err := deleter.DeleteBlob(name)
		if !errors.Is(err, ErrUnsupported) {
			return err
		}
	}
	if lifecycle, ok := p.(Lifecycle); ok {
		err := lifecycle.DeleteByID(DomainBlobs, name)
		if errors.Is(err, ErrUnknownDomain) {
			return ErrUnsupported
		}
		return err
	}
	return ErrUnsupported
}
