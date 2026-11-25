package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/google/uuid"
)

// inventoryLeaseTTL is how long an inventory lease outlives its last
// renewal. A process that dies without releasing its lease (a crash, an OOM
// kill) blocks every other replica until it expires, and nothing lists the
// cluster meanwhile. It is kept well below the stale limit for kinds read
// every 30 seconds or every minute (3 minutes, see syncKindStale), so a
// takeover after a crash lists the cluster again before its view can turn
// stale: the newest observation is at most one minute-cadence old at the
// crash, then the lease runs out, the next replica claims within its
// one-second poll and lists. inventoryTakeoverBound adds this up, and a test
// keeps it inside the stale limit.
const inventoryLeaseTTL = time.Minute

// inventoryLeaseRenewAfter is how much of its lease an owner uses before it
// renews: a quarter, so a renewal held up for half the lease by a slow or
// failing write (a database hiccup, a GC pause) still lands with a quarter
// of the lease, three retry steps, to spare.
func inventoryLeaseRenewAfter(ttl time.Duration) time.Duration { return ttl / 4 }

// inventoryLeaseRetryStep is how often an owner checks its lease while a
// collection cycle runs, and retries a failed renewal.
func inventoryLeaseRetryStep(ttl time.Duration) time.Duration { return ttl / 12 }

// inventoryTakeoverBound is the oldest the newest observation of a kind read
// every minute can be when a replica that took over a crashed owner's lease
// publishes again: one cadence before the crash, the whole lease, one poll,
// and a generous collection cycle.
const inventoryTakeoverBound = time.Minute + inventoryLeaseTTL + time.Second + 30*time.Second

// inventoryLeaseTTL is the lease length this service claims and renews.
func (service *Service) inventoryLeaseTTL() time.Duration {
	if service != nil && service.leaseTTL > 0 {
		return service.leaseTTL
	}
	return inventoryLeaseTTL
}

var errInventoryLeaseLost = errors.New("kubernetes: inventory lease lost")

type inventoryLeaseRecord struct {
	Owner     string
	Expires   time.Time
	DataOwner string
	Slot      int
	Revision  string
	// FirstReadyAt is when this cluster first published a generation its own
	// health report called ready. It lives with the cluster's inventory metadata,
	// not with one generation, so it is written once and read back by every later
	// claim, publish, restart, and re-index. Nothing in this codebase assigns it
	// back to zero.
	FirstReadyAt time.Time
}

// firstReadyAt is the cluster's recorded first ready moment. The metadata record
// is the authority; a generation written before the record carried it still
// supplies its own copy, which is the same moment.
func firstReadyAt(record inventoryLeaseRecord, generation inventoryGeneration) time.Time {
	if !record.FirstReadyAt.IsZero() {
		return record.FirstReadyAt
	}
	return generation.FirstReadyAt
}

// withFirstReady carries the cluster's recorded first ready moment from the
// metadata onto a generation read back from the store.
func withFirstReady(generation inventoryGeneration, record inventoryLeaseRecord) inventoryGeneration {
	generation.FirstReadyAt = firstReadyAt(record, generation)
	return generation
}

// publicationKey addresses only the first-publication timestamp. Inventory and
// leases remain credential-scoped; renewing credentials cannot grant data access.
func publicationKey(scope Scope) string {
	return "kubernetes/first-publication/" + logIdentity(scope.OrgID+"\x00"+scope.ClusterID)
}

func readFirstPublication(provider storage.Provider, scope Scope) (time.Time, error) {
	return readFirstPublicationContext(context.Background(), provider, scope)
}

// readFirstPublicationContext is readFirstPublication that stops when ctx
// ends.
func readFirstPublicationContext(ctx context.Context, provider storage.Provider, scope Scope) (time.Time, error) {
	raw, err := storage.ReadBlobContext(ctx, provider, publicationKey(scope))
	if err != nil || len(raw) == 0 {
		return time.Time{}, err
	}
	var at time.Time
	if err := json.Unmarshal(raw, &at); err != nil {
		return time.Time{}, err
	}
	return at.UTC(), nil
}

// rememberFirstPublication only records an already committed ready generation.
// A single CAS wins across credential renewals and workers; it never overwrites
// the original timestamp with a later publication.
func rememberFirstPublication(provider storage.Provider, scope Scope, at time.Time) (time.Time, error) {
	return rememberFirstPublicationContext(context.Background(), provider, scope, at)
}

// rememberFirstPublicationContext is rememberFirstPublication that stops when
// ctx ends.
func rememberFirstPublicationContext(ctx context.Context, provider storage.Provider, scope Scope, at time.Time) (time.Time, error) {
	if at.IsZero() || provider == nil {
		return at, nil
	}
	if recorded, err := readFirstPublicationContext(ctx, provider, scope); err != nil || !recorded.IsZero() {
		return recorded, err
	}
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return time.Time{}, errors.New("kubernetes: publication storage requires compare and swap")
	}
	raw, err := json.Marshal(at.UTC())
	if err != nil {
		return time.Time{}, err
	}
	won, err := storage.CompareAndSwapBlobContext(ctx, cas, publicationKey(scope), nil, raw)
	if err != nil {
		return time.Time{}, err
	}
	if won {
		return at.UTC(), nil
	}
	return readFirstPublicationContext(ctx, provider, scope)
}

// preserveFirstPublication migrates an existing, already recorded timestamp even
// when the credential is unavailable. Persistence failure never erases that fact.
func preserveFirstPublication(ctx context.Context, provider storage.Provider, scope Scope, at time.Time) time.Time {
	recorded, err := rememberFirstPublicationContext(ctx, provider, scope, at)
	if err != nil {
		logging.Debugf("kubernetes: first publication metadata persistence deferred")
	}
	if err == nil && !recorded.IsZero() {
		return recorded
	}
	return at.UTC()
}

// recordedFirstReadyAt is the cluster's first ready moment, read from local state
// only. Cluster-scoped publication metadata survives credential renewal. The
// in-memory generation and legacy credential-scoped metadata provide fallback
// for an in-memory deployment and an existing installation, respectively.
//
// No credential is validated on this path and no cluster is read: the marker is
// Versus's own record of a past success, so it has to stay readable exactly when
// the credential that wrote it is dead. A cluster that was never ready, or a
// record that cannot be read right now, yields the zero time — the marker is
// never invented to fill the gap.
func (service *Service) recordedFirstReadyAt() time.Time {
	at, _ := service.recordedFirstReadyAtContext(context.Background())
	return at
}

// recordedFirstReadyAtContext is recordedFirstReadyAt for a request: its store
// reads stop when ctx ends, and when no moment could be found because the store
// failed, that failure is returned with the zero time, so a request is not told
// a cluster never became ready only because the store did not answer.
func (service *Service) recordedFirstReadyAtContext(ctx context.Context) (time.Time, error) {
	if service != nil {
		if at := service.recovery.firstPublication(service.scope.OrgID); !at.IsZero() {
			return at, nil
		}
	}
	if service == nil || service.changes == nil {
		return time.Time{}, nil
	}
	service.changes.mu.Lock()
	provider := service.changes.provider
	service.changes.mu.Unlock()
	var storeErr error
	for _, org := range service.inventoryReadOrgs() {
		scope := service.scope
		scope.OrgID = org
		if provider != nil {
			at, err := readFirstPublicationContext(ctx, provider, scope)
			if err == nil && !at.IsZero() {
				return at, nil
			}
			if err != nil && storeErr == nil {
				storeErr = err
			}
		}
		// Finish this org's layouts before consulting an upgrade fallback org.
		indexScope := service.indexScope()
		indexScope.OrgID = org
		service.changes.mu.Lock()
		cache := service.changes.inventories[indexScope]
		service.changes.mu.Unlock()
		if cache != nil {
			cache.mu.RLock()
			recorded := cache.generation.FirstReadyAt
			cache.mu.RUnlock()
			if !recorded.IsZero() {
				return preserveFirstPublication(ctx, provider, scope, recorded), nil
			}
		}
		if provider == nil {
			continue
		}
		raw, err := storage.ReadBlobContext(ctx, provider, inventoryKey(scope))
		if err != nil && storeErr == nil {
			storeErr = err
		}
		if err != nil || len(raw) == 0 {
			continue
		}
		// Both legacy inventory layouts carry this field on the record.
		var record inventoryLeaseRecord
		if json.Unmarshal(raw, &record) != nil {
			continue
		}
		if recorded := record.FirstReadyAt; !recorded.IsZero() {
			return preserveFirstPublication(ctx, provider, scope, recorded), nil
		}
	}
	return time.Time{}, storeErr
}

// recordedSnapshotObservedAt is when the cluster's stored snapshot was last
// observed, read from local state only, along the same two-step path as
// recordedFirstReadyAt: the published generation in memory first, then the copy
// that survives a restart.
//
// No credential is validated and no cluster is read, so the age survives the very
// credential fault that makes a read fail. A cluster that never published a
// snapshot yields the zero time, and the caller omits the age instead of claiming
// the data is current.
func (service *Service) recordedSnapshotObservedAt(ctx context.Context) time.Time {
	if service == nil || service.changes == nil {
		return time.Time{}
	}
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	provider := service.changes.provider
	service.changes.mu.Unlock()
	if cache != nil {
		cache.mu.RLock()
		generation := cache.generation
		cache.mu.RUnlock()
		if observed := snapshotObservedAt(generation); !observed.IsZero() {
			return observed
		}
	}
	if provider == nil {
		return time.Time{}
	}
	for _, org := range service.inventoryReadOrgs() {
		scope := service.scope
		scope.OrgID = org
		generation, _, err := readInventoryGenerationContext(ctx, provider, scope, "")
		if err != nil {
			continue
		}
		if observed := snapshotObservedAt(generation); !observed.IsZero() {
			return observed
		}
	}
	return time.Time{}
}

// snapshotObservedAt is the oldest observation in a generation: the age of the
// data at its oldest, which is the age a surface can honestly show for it.
func snapshotObservedAt(generation inventoryGeneration) time.Time {
	oldest := time.Time{}
	for _, state := range generation.View.Kinds {
		if !state.ObservedAt.IsZero() && (oldest.IsZero() || state.ObservedAt.Before(oldest)) {
			oldest = state.ObservedAt
		}
	}
	if oldest.IsZero() {
		return time.Time{}
	}
	return oldest.UTC()
}

// inventoryReadOrgs is the set of org scopes this service may read a stored
// generation from, in precedence order.
func (service *Service) inventoryReadOrgs() []string {
	if len(service.readOrgs) > 0 {
		return service.readOrgs
	}
	return []string{service.scope.OrgID}
}

type inventoryLease struct {
	mu       sync.Mutex
	provider storage.Provider
	cas      storage.BlobCAS
	scope    Scope
	record   inventoryLeaseRecord
	expected []byte
	ttl      time.Duration
}

func (lease *inventoryLease) leaseTTL() time.Duration {
	if lease.ttl > 0 {
		return lease.ttl
	}
	return inventoryLeaseTTL
}

// expiresAt is when the lease runs out unless renewed.
func (lease *inventoryLease) expiresAt() time.Time {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.record.Expires
}

func inventoryDataKey(scope Scope, record inventoryLeaseRecord) (string, error) {
	if !safeSegment(record.DataOwner) || len(record.DataOwner) > 64 || record.Slot < 0 || record.Slot > 1 || !safeSegment(record.Revision) || len(record.Revision) > 64 {
		return "", ErrInvalidArguments
	}
	return inventoryKey(scope) + "/data/" + record.DataOwner + "/" + strconv.Itoa(record.Slot), nil
}

func readInventoryGeneration(provider storage.Provider, scope Scope, revision string) (inventoryGeneration, inventoryLeaseRecord, error) {
	return readInventoryGenerationContext(context.Background(), provider, scope, revision)
}

// readInventoryGenerationContext reads the published generation and stops
// when ctx ends, so a worker waiting on a database that does not answer can
// still be stopped.
func readInventoryGenerationContext(ctx context.Context, provider storage.Provider, scope Scope, revision string) (inventoryGeneration, inventoryLeaseRecord, error) {
	for attempt := 0; attempt < 3; attempt++ {
		generation, record, err := readInventoryGenerationOnce(ctx, provider, scope, revision)
		var payloadErr inventoryPayloadError
		if !errors.As(err, &payloadErr) {
			return generation, record, err
		}
		raw, readErr := storage.ReadBlobContext(ctx, provider, inventoryKey(scope))
		if readErr != nil {
			return generation, record, inventoryStoreReadError{readErr}
		}
		var latest inventoryLeaseRecord
		if json.Unmarshal(raw, &latest) != nil || latest.Revision == record.Revision {
			return generation, record, err
		}
	}
	return inventoryGeneration{}, inventoryLeaseRecord{}, inventoryPayloadError{ErrIncompleteInventory}
}

type inventoryPayloadError struct{ error }

func (err inventoryPayloadError) Unwrap() error { return err.error }

// inventoryStoreReadError is a published generation the store did not hand
// over: it timed out, was unreachable or failed the read. The stored data
// itself was never seen, so nothing is known about the cluster.
type inventoryStoreReadError struct{ error }

func (err inventoryStoreReadError) Unwrap() error { return err.error }

// inventoryReadAnswer names a reader's failed read of the published generation
// for the store when the store failed it, with the same classes a saved
// history read uses, so a database that stalls is never reported as a
// Kubernetes timeout. Any other failure is returned unchanged.
func inventoryReadAnswer(err error) error {
	var store inventoryStoreReadError
	if errors.As(err, &store) {
		return historyReadError(store.error)
	}
	return err
}

func readInventoryGenerationOnce(ctx context.Context, provider storage.Provider, scope Scope, revision string) (inventoryGeneration, inventoryLeaseRecord, error) {
	raw, err := storage.ReadBlobContext(ctx, provider, inventoryKey(scope))
	if err != nil {
		return inventoryGeneration{}, inventoryLeaseRecord{}, inventoryStoreReadError{err}
	}
	if len(raw) == 0 {
		return inventoryGeneration{}, inventoryLeaseRecord{}, nil
	}
	var lease inventoryLeaseRecord
	if err := json.Unmarshal(raw, &lease); err != nil {
		return inventoryGeneration{}, lease, err
	}
	if lease.Revision == "" {
		// Read the previous snapshot format once during upgrade. New writes
		// always use a small lease plus separately stored projected data.
		var legacy inventoryGeneration
		err := json.Unmarshal(raw, &legacy)
		return legacy, lease, err
	}
	if lease.Revision == revision {
		return inventoryGeneration{}, lease, nil
	}
	key, err := inventoryDataKey(scope, lease)
	if err != nil {
		return inventoryGeneration{}, lease, err
	}
	payload, err := storage.ReadBlobContext(ctx, provider, key)
	if err != nil {
		return inventoryGeneration{}, lease, inventoryStoreReadError{err}
	}
	var generation inventoryGeneration
	if err := json.Unmarshal(payload, &generation); err != nil {
		return inventoryGeneration{}, lease, inventoryPayloadError{err}
	}
	if generation.Revision != lease.Revision {
		return inventoryGeneration{}, lease, inventoryPayloadError{ErrIncompleteInventory}
	}
	return generation, lease, nil
}

func (service *Service) claimInventory(ctx context.Context, provider storage.Provider, owner string) (*inventoryLease, bool, error) {
	if provider == nil {
		return &inventoryLease{record: inventoryLeaseRecord{Owner: owner}}, true, nil
	}
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return nil, false, errors.New("kubernetes: atomic inventory storage unavailable")
	}
	raw, err := storage.ReadBlobContext(ctx, provider, inventoryKey(service.scope))
	if err != nil {
		return nil, false, storageStageFailure("lease", err)
	}
	var record inventoryLeaseRecord
	if len(raw) > 0 && json.Unmarshal(raw, &record) != nil {
		return nil, false, storageStageFailure("lease", ErrInvalidArguments)
	}
	if record.Owner != owner && record.Expires.After(time.Now()) {
		return nil, false, nil
	}
	ttl := service.inventoryLeaseTTL()
	if record.Owner == owner && record.Expires.After(time.Now().Add(ttl-inventoryLeaseRenewAfter(ttl))) {
		// The record read above already carries the cluster's first ready moment,
		// so a renewed claim publishes generations that still have it.
		return &inventoryLease{provider: provider, cas: cas, scope: service.scope, record: record, expected: raw, ttl: ttl}, true, nil
	}
	record.Owner, record.Expires = owner, time.Now().Add(ttl)
	replacement, err := json.Marshal(record)
	if err != nil {
		return nil, false, err
	}
	var won bool
	err = service.commitStorage(ctx, "lease", func(ctx context.Context) error {
		var err error
		won, err = storage.CompareAndSwapBlobContext(ctx, cas, inventoryKey(service.scope), raw, replacement)
		return err
	})
	return &inventoryLease{provider: provider, cas: cas, scope: service.scope, record: record, expected: replacement, ttl: ttl}, won, err
}

func (lease *inventoryLease) renew(ctx context.Context, service *Service) error {
	if lease.cas == nil {
		return ctx.Err()
	}

	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.renewHeld(ctx, service)
}

// renewHeld renews the lease under the credential epoch fence; the caller
// holds lease.mu.
func (lease *inventoryLease) renewHeld(ctx context.Context, service *Service) error {
	if !lease.record.Expires.After(time.Now()) {
		return errInventoryLeaseLost
	}
	return service.commitStorage(ctx, "lease", func(ctx context.Context) error { return lease.renewFenced(ctx) })
}

// renewFenced renews the lease; the caller holds lease.mu and is already inside
// the credential epoch fence, as a history save is.
func (lease *inventoryLease) renewFenced(ctx context.Context) error {
	if !lease.record.Expires.After(time.Now()) {
		return errInventoryLeaseLost
	}
	record := lease.record
	record.Expires = time.Now().Add(lease.leaseTTL())
	replacement, err := json.Marshal(record)
	if err != nil {
		return err
	}
	won, err := storage.CompareAndSwapBlobContext(ctx, lease.cas, inventoryKey(lease.scope), lease.expected, replacement)
	if err == nil && !won {
		return errInventoryLeaseLost
	}
	if err == nil {
		lease.record, lease.expected = record, replacement
	}
	return err
}

// commitStorage runs one storage write under the credential epoch fence. Only
// the store's own answer is a storage failure: a credential that cannot be
// resolved or accepted before the write keeps its meaning, so it still reaches
// the credential recovery gate.
func (service *Service) commitStorage(ctx context.Context, stage string, commit func(context.Context) error) error {
	return service.commitCredential(ctx, func(ctx context.Context) error { return storageStageFailure(stage, commit(ctx)) })
}

// commitAdmitted runs one storage write that the credential epoch fence
// admits, without holding the fence while the store works. It is for a write
// whose context already ends with the epoch, such as a history save: the
// fence answers the same credential and lease questions as commitStorage, and
// an epoch that ends during the write cancels it and is reported as the
// commit conflict a held fence would have reported.
func (service *Service) commitAdmitted(ctx context.Context, stage string, commit func() error) error {
	if err := service.commitCredential(ctx, func(ctx context.Context) error { return nil }); err != nil {
		return err
	}
	err := storageStageFailure(stage, commit())
	if epoch := service.identity; epoch != nil && epoch.ctx.Err() != nil {
		epoch.mu.Lock()
		cause := retiredEpochCauseLocked(epoch)
		epoch.mu.Unlock()
		return commitConflictError{cause: cause}
	}
	return err
}

func (lease *inventoryLease) release() error {
	if lease.cas == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	raw, err := lease.provider.ReadBlob(inventoryKey(lease.scope))
	if err != nil {
		return err
	}
	var record inventoryLeaseRecord
	if json.Unmarshal(raw, &record) != nil {
		return ErrInvalidArguments
	}
	if record.Owner != lease.record.Owner {
		return nil
	}
	record.Owner, record.Expires = "", time.Time{}
	replacement, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = lease.cas.CompareAndSwapBlob(inventoryKey(lease.scope), raw, replacement)
	return err
}

func (lease *inventoryLease) publish(ctx context.Context, service *Service, generation *inventoryGeneration) error {
	generation.Revision = uuid.NewString()
	// The first ready moment is stamped on the generation being published, so it
	// holds for an in-memory cluster too; with a store the record below makes it
	// the cluster's durable copy.
	if lease.provider != nil {
		at, err := readFirstPublicationContext(ctx, lease.provider, lease.scope)
		if err != nil {
			return err
		}
		if !at.IsZero() {
			generation.FirstReadyAt = at
		}
	}
	if generation.FirstReadyAt.IsZero() {
		generation.FirstReadyAt = service.recovery.firstPublication(service.scope.OrgID)
	}
	generation.markFirstReady(time.Now())
	if lease.cas == nil {
		return ctx.Err()
	}
	lease.mu.Lock()
	record := lease.record
	if !record.Expires.After(time.Now()) {
		lease.mu.Unlock()
		return errInventoryLeaseLost
	}
	lease.mu.Unlock()
	// The publish is where a generation becomes durable, so it is also where the
	// cluster's first ready moment is carried. The record keeps it across every
	// later publish and ownership change; the generation mirrors it so a report can
	// read it with the data it already has.
	record.FirstReadyAt = firstReadyAt(record, *generation)
	generation.FirstReadyAt = record.FirstReadyAt
	retiredOwner := record.DataOwner
	record.DataOwner, record.Revision = record.Owner, generation.Revision
	record.Slot = 1 - record.Slot
	key, err := inventoryDataKey(lease.scope, record)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			if err := lease.cleanupUnreferenced(record.DataOwner, record.Slot); err != nil {
				logging.Debugf("kubernetes: orphan inventory cleanup deferred")
			}
		}
	}()

	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	service.changes.mu.Unlock()
	generation.EvidenceBatch = nil
	generation.EvidenceFailure = ""
	// No capture is prepared while a save runs or the store's backoff waits:
	// the checkpoint is not read, and the first capture after it covers every
	// change since the last saved one.
	if service.historyCaptureDue(cache) {
		previous := previousCapture(cache)
		refusal := ""
		if evidenceErr := service.prepareEvidence(ctx, generation, previous); evidenceErr != nil {
			generation.EvidenceFailure = errorClass(evidenceErr)
			refusal = "reason=" + captureRefusalReason(evidenceErr) + " class=" + generation.EvidenceFailure
		}
		noteCaptureRefusal(service.scope, cache, refusal)
	}
	payload, err := json.Marshal(generation)
	if err != nil {
		return err
	}
	// Each owner alternates its own two slots. A stale owner cannot modify a
	// successor's data, and the small CAS is the only publication point.
	payloadStarted := time.Now()
	writeErr := service.commitStorage(ctx, "payload", func(ctx context.Context) error { return storage.WriteBlobContext(ctx, lease.provider, key, payload) })
	service.recovery.stage("payload", payloadStarted, writeErr)
	if err := writeErr; err != nil {
		return err
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.record.Expires.After(time.Now()) {
		return errInventoryLeaseLost
	}
	record.Expires = time.Now().Add(lease.leaseTTL())
	replacement, err := json.Marshal(record)
	if err != nil {
		return err
	}
	casStarted := time.Now()
	err = service.commitStorage(ctx, "cas", func(ctx context.Context) error {
		won, err := storage.CompareAndSwapBlobContext(ctx, lease.cas, inventoryKey(lease.scope), lease.expected, replacement)
		if err == nil && !won {
			return errInventoryLeaseLost
		}
		return err
	})
	service.recovery.stage("cas", casStarted, err)
	if err == nil {
		lease.record, lease.expected = record, replacement
		published = true
		if _, markerErr := rememberFirstPublication(lease.provider, lease.scope, generation.FirstReadyAt); markerErr != nil {
			return markerErr
		}
		if retiredOwner != "" && retiredOwner != record.DataOwner {
			for slot := 0; slot < 2; slot++ {
				if err := lease.cleanupUnreferenced(retiredOwner, slot); err != nil {
					logging.Debugf("kubernetes: retired inventory cleanup deferred")
				}
			}
		}
	}

	return err
}

func (lease *inventoryLease) cleanupUnreferenced(owner string, slot int) error {
	if lease.provider == nil || lease.cas == nil {
		return nil
	}
	raw, err := lease.provider.ReadBlob(inventoryKey(lease.scope))
	if err != nil {
		return err
	}
	var current inventoryLeaseRecord
	if json.Unmarshal(raw, &current) != nil {
		return ErrInvalidArguments
	}
	if current.DataOwner == owner && current.Slot == slot {
		return nil
	}
	key, err := inventoryDataKey(lease.scope, inventoryLeaseRecord{DataOwner: owner, Slot: slot, Revision: "cleanup"})
	if err != nil {
		return err
	}
	// Exact-key CAS also makes a delayed stale write self-cleaning after its
	// publication fence fails. A live pointer is never removed.
	payload, err := lease.provider.ReadBlob(key)
	if err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err = lease.cas.CompareAndSwapBlob(key, payload, nil)
	return err
}

func hydrateInventory(view kubeindex.Snapshot) (*kubeindex.Index, error) {
	return kubeindex.Restore(view)
}

func (lease *inventoryLease) commitJournal(parent context.Context, service *Service, generation inventoryGeneration) error {
	if generation.EvidenceBatch == nil {
		return nil
	}
	// The lease lock is held only to read and renew the lease, never while
	// the store works: a cycle publishing live data takes the same lock.
	short := func() bool { return lease.cas != nil && time.Until(lease.record.Expires) < lease.leaseTTL()/2 }
	lease.mu.Lock()
	if !lease.record.Expires.After(time.Now()) {
		lease.mu.Unlock()
		return kubechanges.ErrLeaseLost
	}
	// A large capture is saved in several transactions. Start it with at least
	// half a lease ahead, and confirm the lease before each transaction, so
	// every part is fenced by a lease that is still held.
	if short() {
		if err := lease.renewHeld(parent, service); err != nil {
			lease.mu.Unlock()
			return err
		}
	}
	expires, expected, owner := lease.record.Expires, append([]byte(nil), lease.expected...), lease.record.Owner
	lease.mu.Unlock()
	// The save stops when the lease runs out; each renewal between its
	// transactions moves that point, so a long save that keeps its lease is
	// not cut at the expiry it started with.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	expiry := time.AfterFunc(time.Until(expires), cancel)
	defer expiry.Stop()
	batch := *generation.EvidenceBatch
	// The save is fenced by who holds the lease, not by its bytes: this
	// process's own publications and renewals rewrite the lease while the
	// save runs and must never conflict with it.
	batch.GuardKey, batch.GuardExpected, batch.GuardOwner = inventoryKey(lease.scope), expected, owner
	// Called by the store inside the save, between its transactions.
	batch.KeepLease = func(keepCtx context.Context) ([]byte, error) {
		// Each transaction is admitted only while the proof that fenced the
		// capture still stands: a save never lands after it failed.
		if cache := service.inventoryCache(); cache != nil {
			cache.mu.RLock()
			_, refused := service.historySaveAuthorityLocked(cache)
			cache.mu.RUnlock()
			if refused {
				return nil, errHistorySaveRefused
			}
		}
		lease.mu.Lock()
		defer lease.mu.Unlock()
		if !lease.record.Expires.After(time.Now()) {
			return nil, kubechanges.ErrLeaseLost
		}
		if short() {
			if err := lease.renewFenced(keepCtx); err != nil {
				return nil, storageStageFailure("lease", err)
			}
			expiry.Reset(time.Until(lease.record.Expires))
		}
		return append([]byte(nil), lease.expected...), nil
	}
	generation.EvidenceBatch = &batch
	started := time.Now()
	err := service.commitEvidence(ctx, generation)
	service.recovery.stage("journal", started, err)
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	service.changes.mu.Unlock()
	return service.abandonRejectedHistorySave(cache, batch.ID, err)
}

// abandonRejectedHistorySave stops a capture the history store rejected for good
// from blocking collection. Retrying the same capture cannot succeed, so it is
// dropped: the stored rejection note keeps the outage visible, and the next
// capture is prepared against the unchanged checkpoint as a known failure, so
// it records the uncovered interval as a coverage gap. Every other failure is
// returned unchanged and keeps its retry.
func (service *Service) abandonRejectedHistorySave(cache *inventoryCache, batchID string, err error) error {
	key := problemKey(service.scope.OrgID, service.scope.ClusterID, "history_save")
	if err == nil {
		problems().Clear(key, "kubernetes: cluster %q history saves again", service.scope.ClusterID)
	}
	var saveFailure *captureStorageError
	if !errors.As(err, &saveFailure) || !saveFailure.rejected || cache == nil {
		return err
	}
	cache.mu.Lock()
	cache.abandonedSave = batchID
	cache.mu.Unlock()
	reason := captureRefusalReason(err)
	problems().Fail(slog.LevelWarn, key, reason, "kubernetes: cluster %q history store rejected a capture; collection continues and the next capture records the gap reason=%s (storage rejected); restarting does not help, so if it repeats on every capture report it with this line", service.scope.ClusterID, reason)
	return nil
}

// noteCaptureRefusal logs a capture refused before saving when the reason
// differs from the last one logged for the cache's scope, and logs once when
// captures are prepared for saving again. refusal is empty for a capture that
// was prepared. A scope without a cache yet logs every refusal.
//
// A refusal that can never succeed warns through the shared throttle, keyed
// by org and cluster, so a successor service after a credential rotation
// does not warn again; the line that ends it is shown at info. A refusal the
// next capture usually clears (permissions not confirmed yet, an incomplete
// listing, a storage timeout) is debug, since a lasting cause of those is
// warned about by the inventory cycle.
func noteCaptureRefusal(scope Scope, cache *inventoryCache, refusal string) {
	key := problemKey(scope.OrgID, scope.ClusterID, "capture_refusal")
	if refusal == "" && problems().Clear(key, "kubernetes: cluster %q lasting capture refusal cleared", scope.ClusterID) {
		logging.Infof("kubernetes: cluster %q history captures are prepared for saving again", scope.ClusterID)
	}
	if cache != nil {
		cache.refusal.mu.Lock()
		previous := cache.refusal.logged
		cache.refusal.logged = refusal
		cache.refusal.mu.Unlock()
		if refusal == previous {
			return
		}
		if refusal == "" {
			if !captureRefusalLasting(previous) {
				logging.Debugf("kubernetes: history captures are prepared for saving again")
			}
			return
		}
	}
	switch {
	case refusal == "":
	case captureRefusalLasting(refusal):
		problems().Fail(slog.LevelWarn, key, refusal, "kubernetes: cluster %q history capture refused before saving %s (capture cannot be saved); restarting does not help, so if it repeats on every capture report it with this line", scope.ClusterID, refusal)
	default:
		logging.Debugf("kubernetes: history capture refused before saving %s", refusal)
	}
}

// captureRefusalLasting reports a refusal that retrying the same capture
// cannot fix. refusal has the form "reason=<reason> class=<class>".
func captureRefusalLasting(refusal string) bool {
	reason, _, _ := strings.Cut(strings.TrimPrefix(refusal, "reason="), " ")
	switch reason {
	case "over_save_limit", "unstorable_text", "invalid_capture", "store_unsupported":
		return true
	}
	return false
}

func (service *Service) historySaveAbandoned(cache *inventoryCache, batchID string) bool {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return batchID != "" && cache.abandonedSave == batchID
}

// historySaveRetryCeiling bounds the wait between saves after the store
// failed one.
const historySaveRetryCeiling = 5 * time.Minute

// pendingHistorySave is the history save state of one inventory scope.
//
// At most one save runs at a time, off the collection path, under the
// worker's context. While it runs, cycles publish live inventory but prepare
// no capture, and after the store failed a save they prepare none until the
// backoff has passed. A capture is a diff from base, the generation whose
// capture was last saved, against the checkpoint it reads when it is
// prepared, so a capture prepared after a pause covers every change since:
// nothing is lost while a save runs, and no gap is recorded for it. Only a
// capture the store failed to save is dropped; the next capture records the
// stretch it covered as a gap. The inventory cache's lock guards every field
// but the wait group.
type pendingHistorySave struct {
	// queued is a capture waiting to start: the generation restored at start,
	// or a capture held while its cycle's authority proof is pending.
	queued   *inventoryGeneration
	inFlight string
	base     *inventoryGeneration
	failures int
	next     time.Time
	dropped  int
	// lost is set when a capture was dropped because the store failed it, so
	// the next capture records the stretch as a gap.
	lost bool
	// failing is set from a store failure until the next save succeeds.
	failing bool
	ctx     context.Context
	running *sync.WaitGroup
	// clock tells the backoff time; nil is the wall clock.
	clock func() time.Time
}

func (pending *pendingHistorySave) now() time.Time {
	if pending.clock != nil {
		return pending.clock()
	}
	return time.Now()
}

// captureDue reports whether a cycle publishing now prepares a capture: no
// save runs or waits to start, and no store-failure backoff is pending.
func (pending *pendingHistorySave) captureDue() bool {
	return pending.inFlight == "" && pending.queued == nil && (!pending.failing || !pending.now().Before(pending.next))
}

// known reports whether batch is already saved, being saved or waiting.
func (pending *pendingHistorySave) known(batch string) bool {
	for _, generation := range []*inventoryGeneration{pending.queued, pending.base} {
		if generation != nil && generation.EvidenceBatch != nil && generation.EvidenceBatch.ID == batch {
			return true
		}
	}
	return pending.inFlight == batch
}

// historySaveStoreFailure reports a save that failed in the store: slow,
// unavailable or refusing. Only that sets the backoff, is reported as history
// not saving and records a gap; a lost lease, a moved checkpoint or a
// credential answer drops the capture, and the next one, prepared from the
// fresh checkpoint, covers it.
func historySaveStoreFailure(err error) bool {
	return !errors.Is(err, errInventoryLeaseLost) && !errors.Is(err, kubechanges.ErrLeaseLost) && !errors.Is(err, storage.ErrEvidenceConflict) &&
		!countsAgainstCredential(err) && !errors.Is(err, errHistorySaveRefused)
}

// historySaveAuthorityLocked tells whether a capture may be saved now: held
// while the cycle's authority proof is pending, refused once the proof failed
// or the credential epoch ended, and allowed otherwise. The caller holds the
// inventory cache's lock.
func (service *Service) historySaveAuthorityLocked(cache *inventoryCache) (allowed, refused bool) {
	if service.identity != nil && service.identity.ctx.Err() != nil {
		return false, true
	}
	if cache.authorityFailure != nil || cache.failed && (cache.failedClass == "forbidden" || cache.failedClass == "unauthorized") {
		return false, true
	}
	if !cache.authorityQuestioned.empty() {
		return false, false
	}
	return true, false
}

// errHistorySaveRefused stops a save whose cycle's authority proof failed or
// whose credential epoch ended after the capture was published.
var errHistorySaveRefused = errors.New("kubernetes: history save refused after the authority proof failed")

// startHistorySaver binds a scope's history saves to its worker's context.
// The returned stop refuses every later save, then waits until the one
// running, if any, has ended.
func (service *Service) startHistorySaver(ctx context.Context, cache *inventoryCache) func() {
	running := &sync.WaitGroup{}
	cache.mu.Lock()
	cache.historySave.ctx, cache.historySave.running = ctx, running
	cache.mu.Unlock()
	return func() {
		cache.mu.Lock()
		if cache.historySave.running == running {
			cache.historySave.running = nil
		}
		cache.mu.Unlock()
		running.Wait()
	}
}

// historyCaptureDue reports whether a cycle publishing now prepares a capture.
func (service *Service) historyCaptureDue(cache *inventoryCache) bool {
	if cache == nil {
		return true
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.historySave.captureDue()
}

// queueHistorySave starts the save of a published capture. It never waits for
// the store: the capture is already live, and only saved history lags while
// the store is slow.
func (service *Service) queueHistorySave(lease *inventoryLease, cache *inventoryCache, generation inventoryGeneration) {
	if cache == nil || generation.EvidenceBatch == nil {
		return
	}
	cache.mu.Lock()
	if cache.historySave.known(generation.EvidenceBatch.ID) {
		cache.mu.Unlock()
		return
	}
	if cache.historySave.queued != nil {
		cache.historySave.dropped++
	}
	cache.historySave.queued = &generation
	cache.mu.Unlock()
	service.startHistorySave(lease, cache)
}

// startHistorySave starts the waiting capture's save when its cycle's
// authority stands and no other save runs.
func (service *Service) startHistorySave(lease *inventoryLease, cache *inventoryCache) {
	if cache == nil || lease == nil {
		return
	}
	cache.mu.Lock()
	pending := &cache.historySave
	allowed, refused := service.historySaveAuthorityLocked(cache)
	if refused && pending.queued != nil {
		// A capture is never saved once the proof that fenced it failed.
		pending.queued = nil
		pending.dropped++
	}
	if !allowed || pending.queued == nil || pending.inFlight != "" || pending.ctx == nil || pending.running == nil || pending.ctx.Err() != nil {
		cache.mu.Unlock()
		return
	}
	generation := *pending.queued
	pending.queued, pending.inFlight = nil, generation.EvidenceBatch.ID
	ctx, running := pending.ctx, pending.running
	running.Add(1)
	cache.mu.Unlock()
	go func() {
		defer running.Done()
		err := lease.commitJournal(ctx, service, generation)
		service.finishHistorySave(cache, generation, err)
	}()
}

// finishHistorySave records how a save ended. A saved capture becomes the
// base of the next. A store failure drops the capture, sets the backoff and is
// reported as history not saving; the next capture records the gap.
func (service *Service) finishHistorySave(cache *inventoryCache, generation inventoryGeneration, err error) {
	cache.mu.Lock()
	pending := &cache.historySave
	pending.inFlight = ""
	switch {
	case err == nil:
		pending.base = &generation
		pending.failing, pending.failures, pending.next, pending.lost = false, 0, time.Time{}, false
		cache.mu.Unlock()
		return
	case pending.ctx.Err() != nil:
		// The worker is stopping; its successor saves what it publishes.
		cache.mu.Unlock()
		return
	case !historySaveStoreFailure(err):
		// The next capture, prepared from the fresh checkpoint against the
		// same base, covers this one.
		pending.dropped++
		cache.mu.Unlock()
		return
	}
	pending.dropped++
	pending.lost, pending.failing = true, true
	pending.failures++
	pending.next = pending.now().Add(min(time.Second<<min(pending.failures-1, 9), historySaveRetryCeiling))
	dropped := pending.dropped
	cache.mu.Unlock()
	problems().Fail(slog.LevelWarn, problemKey(service.scope.OrgID, service.scope.ClusterID, "history_save"), "storage_unavailable",
		"kubernetes: cluster %q history is not saving (storage unavailable); live views stay current and the next capture is saved after a backoff, %d captures dropped; check the history database is reachable and answering quickly", service.scope.ClusterID, dropped)
}

// previousCapture is the generation a new capture is diffed from: the one
// whose capture was last saved, or the published one before any save. A
// capture the store rejected, or dropped because the store failed it, left
// saved history short, so the new capture records that stretch as a gap.
func previousCapture(cache *inventoryCache) inventoryGeneration {
	if cache == nil {
		return inventoryGeneration{}
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	previous := cache.generation
	if cache.historySave.base != nil {
		previous = *cache.historySave.base
	}
	if previous.EvidenceBatch != nil && previous.EvidenceBatch.ID == cache.abandonedSave {
		previous.EvidenceFailure = "storage_rejected"
	}
	if cache.historySave.lost {
		previous.EvidenceFailure = "storage_unavailable"
	}
	return previous
}

// adopt folds a fresh claim of the same lease into the lease already held, so
// a history save running on it and the next cycle share one lease and its
// renewals. The claim's state is taken only when it is the newer write.
func (lease *inventoryLease) adopt(claimed *inventoryLease) *inventoryLease {
	if lease == nil || claimed == nil || lease == claimed {
		return claimed
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.record.Owner != claimed.record.Owner || lease.scope != claimed.scope {
		return claimed
	}
	if claimed.record.Expires.After(lease.record.Expires) {
		lease.record, lease.expected = claimed.record, claimed.expected
	}
	lease.provider, lease.cas, lease.ttl = claimed.provider, claimed.cas, claimed.ttl
	return lease
}

// inventoryCache is this scope's inventory cache, or nil before the worker
// starts.
func (service *Service) inventoryCache() *inventoryCache {
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	return service.changes.inventories[service.indexScope()]
}
