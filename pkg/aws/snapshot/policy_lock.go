package snapshot

import (
	"context"
	"hash/fnv"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// LockPolicyReads takes one account's policy-read lock across replicas, so
// two replicas never read the same metered policies in one window. A
// backend that serves one process has no such lock and always grants it;
// the stored attempt times then keep a second reader from repeating a read.
func (store *Store) LockPolicyReads(ctx context.Context, account string) (unlock func(), acquired bool, err error) {
	locker, ok := store.provider.(storage.SessionLocker)
	if !ok {
		return func() {}, true, nil
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("aws-policy-reads\x00" + store.scope.Write + "\x00" + account))
	unlock, acquired, err = locker.TryLockSession(ctx, int64(hash.Sum64()&0x7fffffffffffffff))
	if err != nil || !acquired {
		return nil, false, err
	}
	return unlock, true, nil
}
