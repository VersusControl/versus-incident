package storage

import (
	"context"
	"time"
)

// BlobOperationTimeout bounds one blob read or write on a store that answers
// over the network. A database that stops answering fails the operation
// instead of holding its caller, and whatever was waiting on that caller.
const BlobOperationTimeout = 30 * time.Second

// blobOperationContext bounds ctx by BlobOperationTimeout, keeping an earlier
// deadline the caller already set.
func blobOperationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, BlobOperationTimeout)
}

// ContextBlobStore is a blob store whose reads and writes stop when their
// context ends.
type ContextBlobStore interface {
	ReadBlobContext(ctx context.Context, name string) ([]byte, error)
	WriteBlobContext(ctx context.Context, name string, data []byte) error
}

// ContextBlobCAS is a compare-and-swap that stops when its context ends.
type ContextBlobCAS interface {
	CompareAndSwapBlobContext(ctx context.Context, name string, expected, replacement []byte) (bool, error)
}

// ReadBlobContext reads a blob, stopping when ctx ends on a store that
// supports it. A store without that support answers locally, so it is asked
// only while ctx is still live.
func ReadBlobContext(ctx context.Context, provider Provider, name string) ([]byte, error) {
	if store, ok := provider.(ContextBlobStore); ok {
		return store.ReadBlobContext(ctx, name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return provider.ReadBlob(name)
}

// WriteBlobContext writes a blob, stopping when ctx ends on a store that
// supports it.
func WriteBlobContext(ctx context.Context, provider Provider, name string, data []byte) error {
	if store, ok := provider.(ContextBlobStore); ok {
		return store.WriteBlobContext(ctx, name, data)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return provider.WriteBlob(name, data)
}

// CompareAndSwapBlobContext swaps a blob, stopping when ctx ends on a store
// that supports it.
func CompareAndSwapBlobContext(ctx context.Context, cas BlobCAS, name string, expected, replacement []byte) (bool, error) {
	if store, ok := cas.(ContextBlobCAS); ok {
		return store.CompareAndSwapBlobContext(ctx, name, expected, replacement)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return cas.CompareAndSwapBlob(name, expected, replacement)
}
