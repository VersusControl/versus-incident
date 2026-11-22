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
	raw, err := provider.ReadBlob(publicationKey(scope))
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
	if at.IsZero() || provider == nil {
		return at, nil
	}
	if recorded, err := readFirstPublication(provider, scope); err != nil || !recorded.IsZero() {
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
	won, err := cas.CompareAndSwapBlob(publicationKey(scope), nil, raw)
	if err != nil {
		return time.Time{}, err
	}
	if won {
		return at.UTC(), nil
	}
	return readFirstPublication(provider, scope)
}

// preserveFirstPublication migrates an existing, already recorded timestamp even
// when the credential is unavailable. Persistence failure never erases that fact.
func preserveFirstPublication(provider storage.Provider, scope Scope, at time.Time) time.Time {
	recorded, err := rememberFirstPublication(provider, scope, at)
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
	if service != nil {
		if at := service.recovery.firstPublication(service.scope.OrgID); !at.IsZero() {
			return at
		}
	}
	if service == nil || service.changes == nil {
		return time.Time{}
	}
	service.changes.mu.Lock()
	provider := service.changes.provider
	service.changes.mu.Unlock()
	for _, org := range service.inventoryReadOrgs() {
		scope := service.scope
		scope.OrgID = org
		if provider != nil {
			if at, err := readFirstPublication(provider, scope); err == nil && !at.IsZero() {
				return at
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
				return preserveFirstPublication(provider, scope, recorded)
			}
		}
		if provider == nil {
			continue
		}
		raw, err := provider.ReadBlob(inventoryKey(scope))
		if err != nil || len(raw) == 0 {
			continue
		}
		// Both legacy inventory layouts carry this field on the record.
		var record inventoryLeaseRecord
		if json.Unmarshal(raw, &record) != nil {
			continue
		}
		if recorded := record.FirstReadyAt; !recorded.IsZero() {
			return preserveFirstPublication(provider, scope, recorded)
		}
	}
	return time.Time{}
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
func (service *Service) recordedSnapshotObservedAt() time.Time {
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
		generation, _, err := readInventoryGeneration(provider, scope, "")
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
	for attempt := 0; attempt < 3; attempt++ {
		generation, record, err := readInventoryGenerationOnce(provider, scope, revision)
		var payloadErr inventoryPayloadError
		if !errors.As(err, &payloadErr) {
			return generation, record, err
		}
		raw, readErr := provider.ReadBlob(inventoryKey(scope))
		if readErr != nil {
			return generation, record, readErr
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

func readInventoryGenerationOnce(provider storage.Provider, scope Scope, revision string) (inventoryGeneration, inventoryLeaseRecord, error) {
	raw, err := provider.ReadBlob(inventoryKey(scope))
	if err != nil || len(raw) == 0 {
		return inventoryGeneration{}, inventoryLeaseRecord{}, err
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
	payload, err := provider.ReadBlob(key)
	if err != nil {
		return inventoryGeneration{}, lease, err
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
	raw, err := provider.ReadBlob(inventoryKey(service.scope))
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
	err = service.commitStorage(ctx, "lease", func() error {
		var err error
		won, err = cas.CompareAndSwapBlob(inventoryKey(service.scope), raw, replacement)
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
	return service.commitStorage(ctx, "lease", lease.renewFenced)
}

// renewFenced renews the lease; the caller holds lease.mu and is already inside
// the credential epoch fence, as a history save is.
func (lease *inventoryLease) renewFenced() error {
	if !lease.record.Expires.After(time.Now()) {
		return errInventoryLeaseLost
	}
	record := lease.record
	record.Expires = time.Now().Add(lease.leaseTTL())
	replacement, err := json.Marshal(record)
	if err != nil {
		return err
	}
	won, err := lease.cas.CompareAndSwapBlob(inventoryKey(lease.scope), lease.expected, replacement)
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
func (service *Service) commitStorage(ctx context.Context, stage string, commit func() error) error {
	return service.commitCredential(ctx, func() error { return storageStageFailure(stage, commit()) })
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
		at, err := readFirstPublication(lease.provider, lease.scope)
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
	previous := inventoryGeneration{}
	if cache != nil {
		cache.mu.RLock()
		previous = cache.generation
		// A capture whose save was abandoned left saved history short of it.
		if previous.EvidenceBatch != nil && previous.EvidenceBatch.ID == cache.abandonedSave {
			previous.EvidenceFailure = "storage_rejected"
		}
		cache.mu.RUnlock()
	}
	generation.EvidenceBatch = nil
	generation.EvidenceFailure = ""
	refusal := ""
	if evidenceErr := service.prepareEvidence(ctx, generation, previous); evidenceErr != nil {
		generation.EvidenceFailure = errorClass(evidenceErr)
		refusal = "reason=" + captureRefusalReason(evidenceErr) + " class=" + generation.EvidenceFailure
	}
	noteCaptureRefusal(service.scope, cache, refusal)
	payload, err := json.Marshal(generation)
	if err != nil {
		return err
	}
	// Each owner alternates its own two slots. A stale owner cannot modify a
	// successor's data, and the small CAS is the only publication point.
	payloadStarted := time.Now()
	writeErr := service.commitStorage(ctx, "payload", func() error { return lease.provider.WriteBlob(key, payload) })
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
	err = service.commitStorage(ctx, "cas", func() error {
		won, err := lease.cas.CompareAndSwapBlob(inventoryKey(lease.scope), lease.expected, replacement)
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
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.record.Expires.After(time.Now()) {
		return kubechanges.ErrLeaseLost
	}
	// A large capture is saved in several transactions. Start it with at least
	// half a lease ahead, and confirm the lease before each transaction, so
	// every part is fenced by a lease that is still held.
	short := func() bool { return lease.cas != nil && time.Until(lease.record.Expires) < lease.leaseTTL()/2 }
	if short() {
		if err := lease.renewHeld(parent, service); err != nil {
			return err
		}
	}
	// The save stops when the lease runs out; each renewal between its
	// transactions moves that point, so a long save that keeps its lease is
	// not cut at the expiry it started with.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	expiry := time.AfterFunc(time.Until(lease.record.Expires), cancel)
	defer expiry.Stop()
	batch := *generation.EvidenceBatch
	batch.GuardKey, batch.GuardExpected = inventoryKey(lease.scope), append([]byte(nil), lease.expected...)
	// Called by the store inside the save, which already holds the fence.
	batch.KeepLease = func(context.Context) ([]byte, error) {
		if !lease.record.Expires.After(time.Now()) {
			return nil, kubechanges.ErrLeaseLost
		}
		if short() {
			if err := lease.renewFenced(); err != nil {
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
