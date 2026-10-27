package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"sync"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/google/uuid"
)

const inventoryLeaseTTL = 3 * time.Minute

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
		log.Printf("kubernetes: first publication metadata persistence deferred")
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
		return nil, false, err
	}
	var record inventoryLeaseRecord
	if len(raw) > 0 && json.Unmarshal(raw, &record) != nil {
		return nil, false, ErrInvalidArguments
	}
	if record.Owner != owner && record.Expires.After(time.Now()) {
		return nil, false, nil
	}
	if record.Owner == owner && record.Expires.After(time.Now().Add(time.Minute)) {
		// The record read above already carries the cluster's first ready moment,
		// so a renewed claim publishes generations that still have it.
		return &inventoryLease{provider: provider, cas: cas, scope: service.scope, record: record, expected: raw}, true, nil
	}
	record.Owner, record.Expires = owner, time.Now().Add(inventoryLeaseTTL)
	replacement, err := json.Marshal(record)
	if err != nil {
		return nil, false, err
	}
	var won bool
	err = service.commitCredential(ctx, func() error {
		var err error
		won, err = cas.CompareAndSwapBlob(inventoryKey(service.scope), raw, replacement)
		return err
	})
	return &inventoryLease{provider: provider, cas: cas, scope: service.scope, record: record, expected: replacement}, won, err
}

func (lease *inventoryLease) renew(ctx context.Context, service *Service) error {
	if lease.cas == nil {
		return ctx.Err()
	}

	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.record.Expires.After(time.Now()) {
		return errors.New("kubernetes: inventory lease expired")
	}
	record := lease.record
	record.Expires = time.Now().Add(inventoryLeaseTTL)
	replacement, err := json.Marshal(record)
	if err != nil {
		return err
	}
	err = service.commitCredential(ctx, func() error {
		won, err := lease.cas.CompareAndSwapBlob(inventoryKey(lease.scope), lease.expected, replacement)
		if err == nil && !won {
			return errors.New("kubernetes: inventory lease lost")
		}
		return err
	})
	if err == nil {
		lease.record, lease.expected = record, replacement
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
		at, err := readFirstPublication(lease.provider, lease.scope)
		if err != nil {
			return err
		}
		if !at.IsZero() {
			generation.FirstReadyAt = at
		}
	}
	generation.markFirstReady(time.Now())
	if lease.cas == nil {
		return ctx.Err()
	}
	lease.mu.Lock()
	record := lease.record
	if !record.Expires.After(time.Now()) {
		lease.mu.Unlock()
		return errors.New("kubernetes: inventory lease expired")
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
				log.Printf("kubernetes: orphan inventory cleanup deferred")
			}
		}
	}()
	payload, err := json.Marshal(generation)
	if err != nil {
		return err
	}
	// Each owner alternates its own two slots. A stale owner cannot modify a
	// successor's data, and the small CAS is the only publication point.
	if err := service.commitCredential(ctx, func() error { return lease.provider.WriteBlob(key, payload) }); err != nil {
		return err
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.record.Expires.After(time.Now()) {
		return errors.New("kubernetes: inventory lease expired")
	}
	record.Expires = time.Now().Add(inventoryLeaseTTL)
	replacement, err := json.Marshal(record)
	if err != nil {
		return err
	}
	err = service.commitCredential(ctx, func() error {
		won, err := lease.cas.CompareAndSwapBlob(inventoryKey(lease.scope), lease.expected, replacement)
		if err == nil && !won {
			return errors.New("kubernetes: inventory lease lost")
		}
		return err
	})
	if err == nil {
		lease.record, lease.expected = record, replacement
		published = true
		if _, markerErr := rememberFirstPublication(lease.provider, lease.scope, generation.FirstReadyAt); markerErr != nil {
			return markerErr
		}
		if retiredOwner != "" && retiredOwner != record.DataOwner {
			for slot := 0; slot < 2; slot++ {
				if err := lease.cleanupUnreferenced(retiredOwner, slot); err != nil {
					log.Printf("kubernetes: retired inventory cleanup deferred")
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
