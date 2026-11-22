package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// The lease constants keep a takeover after a crash inside the stale limit:
// the newest observation of a kind read every 30 seconds or every minute is
// at most inventoryTakeoverBound old when the next owner publishes, and the
// restart grace outlasts a takeover that starts at the restart.
func TestInventoryTakeoverAfterACrashFinishesInsideTheStaleLimit(t *testing.T) {
	staleLimit := time.Duration(-1)
	for kind := range indexedResourceIDs {
		cadence := inventoryCadence(kind)
		if cadence > time.Minute {
			continue
		}
		limit := max(3*time.Minute, 2*cadence)
		if staleLimit < 0 || limit < staleLimit {
			staleLimit = limit
		}
	}
	if inventoryTakeoverBound >= staleLimit || staleLimit-inventoryTakeoverBound < 20*time.Second {
		t.Fatalf("a crash takeover can take %s, too close to the %s stale limit", inventoryTakeoverBound, staleLimit)
	}
	ttl := inventoryLeaseTTL
	renewAfter, step := inventoryLeaseRenewAfter(ttl), inventoryLeaseRetryStep(ttl)
	if renewAfter <= 0 || step <= 0 || ttl-renewAfter-ttl/2 < 2*step {
		t.Fatalf("lease %s renewed after %s, retried every %s: a renewal held up for half the lease would lose it", ttl, renewAfter, step)
	}
	if takeoverAfterRestart := ttl + time.Second + 30*time.Second; takeoverAfterRestart >= staleLimit {
		t.Fatalf("the restart grace (%s) ends before a takeover that starts at the restart (%s)", staleLimit, takeoverAfterRestart)
	}
}

// leaseFixture runs inventory workers that share one store against one
// cluster, with a short lease.
type leaseFixture struct {
	t        *testing.T
	provider storage.Provider
	endpoint string
	ttl      time.Duration
	scope    Scope
}

func newLeaseFixture(t *testing.T, ttl time.Duration) *leaseFixture {
	return &leaseFixture{t: t, provider: storage.NewMemory(), endpoint: crashLoopClusterServer(t).URL, ttl: ttl}
}

// start runs one replica's inventory worker and returns its service.
func (fixture *leaseFixture) start() *Service {
	fixture.t.Helper()
	return fixture.startWith(fixture.provider)
}

// startWith runs one replica's inventory worker on its own view of the
// shared store.
func (fixture *leaseFixture) startWith(provider storage.Provider) *Service {
	t := fixture.t
	t.Helper()
	service := newTestService(t, fixture.endpoint, Scope{OrgID: "default", ClusterID: "cluster"})
	service.leaseTTL = fixture.ttl
	seedInventoryDiscovery(service)
	service.client.indexLimiter = nil
	service.SetChangeStorage(provider)
	fixture.scope = service.scope
	service.startInventory(t.Context())
	t.Cleanup(service.StopCapture)
	return service
}

func (fixture *leaseFixture) record() inventoryLeaseRecord {
	raw, err := fixture.provider.ReadBlob(inventoryKey(fixture.scope))
	if err != nil {
		fixture.t.Fatal(err)
	}
	var record inventoryLeaseRecord
	if len(raw) > 0 && json.Unmarshal(raw, &record) != nil {
		fixture.t.Fatal("unreadable lease record")
	}
	return record
}

// crash leaves the lease as a process killed right after renewing it would:
// held by an owner that never releases it, for a whole lease.
func (fixture *leaseFixture) crash() time.Time {
	t := fixture.t
	t.Helper()
	key := inventoryKey(fixture.scope)
	raw, err := fixture.provider.ReadBlob(key)
	if err != nil {
		t.Fatal(err)
	}
	var record inventoryLeaseRecord
	if json.Unmarshal(raw, &record) != nil {
		t.Fatal("unreadable lease record")
	}
	crashed := time.Now()
	record.Owner, record.Expires = "crashed-process", crashed.Add(fixture.ttl)
	replacement, _ := json.Marshal(record)
	if won, err := fixture.provider.(storage.BlobCAS).CompareAndSwapBlob(key, raw, replacement); err != nil || !won {
		t.Fatalf("could not leave the crashed lease: won=%t %v", won, err)
	}
	return crashed
}

// published waits for the first publication and reports the worker's state
// if none comes.
func (fixture *leaseFixture) published(service *Service) {
	t := fixture.t
	t.Helper()
	deadline := time.Now().Add(scaledTimeout(30 * time.Second))
	for fixture.record().Revision == "" {
		if time.Now().After(deadline) {
			service.changes.mu.Lock()
			cache := service.changes.inventories[service.indexScope()]
			service.changes.mu.Unlock()
			cache.mu.RLock()
			defer cache.mu.RUnlock()
			t.Fatalf("no publication: record %+v, failed=%t class=%s kinds=%d revision=%q", fixture.record(), cache.failed, cache.failedClass, len(cache.generation.View.Kinds), cache.generation.Revision)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func servedRevision(service *Service) string {
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	service.changes.mu.Unlock()
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.generation.Revision
}

// A process killed while it holds the lease leaves it unreleased. Another
// replica, and the killed one once restarted, take it over as soon as it
// runs out and publish again; until that publication the restarted replica
// keeps its restart grace, and afterwards both serve the new generation.
func TestCrashedOwnerLeaseIsTakenOverWhenItRunsOut(t *testing.T) {
	for _, replicas := range []string{"restarted and running replica", "single restarted replica"} {
		t.Run(replicas, func(t *testing.T) {
			fixture := newLeaseFixture(t, 2*time.Second)
			crashedOwner := fixture.start()
			fixture.published(crashedOwner)
			var running *Service
			if replicas == "restarted and running replica" {
				running = fixture.start()
			}
			before := fixture.record().Revision
			crashed := fixture.crash()
			crashedOwner.StopCapture()
			if owner := fixture.record().Owner; owner != "crashed-process" {
				t.Fatalf("stopping the crashed process changed its lease owner to %q", owner)
			}
			restarted := fixture.start()

			// Until the lease runs out nobody else may publish, and the
			// restarted replica keeps its grace.
			for time.Until(crashed.Add(fixture.ttl)) > 200*time.Millisecond {
				if record := fixture.record(); record.Revision != before || record.Owner != "crashed-process" {
					t.Fatalf("the crashed lease was taken while still live: %+v", record)
				}
				if restarted.inventoryFreshFloor().IsZero() {
					t.Fatal("the restarted replica lost its grace before anyone published")
				}
				time.Sleep(20 * time.Millisecond)
			}
			waitUntil(t, 10*time.Second, "takeover publication", func() bool {
				record := fixture.record()
				return record.Revision != before && record.Owner != "crashed-process" && record.Owner != ""
			})
			took := time.Since(crashed)
			if limit := scaledTimeout(fixture.ttl + 5*time.Second); took > limit {
				t.Fatalf("takeover took %s after the crash, want within %s", took, limit)
			}
			published := fixture.record().Revision
			for _, service := range []*Service{running, restarted} {
				if service == nil {
					continue
				}
				waitUntil(t, 10*time.Second, "replica serving the new generation", func() bool {
					return servedRevision(service) != before && servedRevision(service) != ""
				})
				if service == restarted {
					// The worker ends the grace when its pass that served
					// the publication finishes.
					waitUntil(t, 10*time.Second, "restart grace to end after the publication", func() bool {
						return service.inventoryFreshFloor().IsZero()
					})
				}
			}
			if published == before {
				t.Fatal("no new generation was published")
			}
		})
	}
}

// A clean stop releases the lease, and the running replica takes over at
// once, whatever the lease length.
func TestReleasedLeaseIsTakenOverWithinSeconds(t *testing.T) {
	fixture := newLeaseFixture(t, 0)
	owner := fixture.start()
	fixture.published(owner)
	running := fixture.start()
	before := fixture.record()
	stopped := time.Now()
	owner.StopCapture()
	waitUntil(t, 10*time.Second, "takeover after a clean stop", func() bool {
		record := fixture.record()
		return record.Owner != "" && record.Owner != before.Owner && record.Revision != before.Revision
	})
	if took := time.Since(stopped); took > scaledTimeout(3*time.Second) {
		t.Fatalf("takeover after a clean stop took %s", took)
	}
	waitUntil(t, 10*time.Second, "running replica serving the new generation", func() bool {
		return servedRevision(running) != before.Revision
	})
}

// slowCAS holds up or fails lease writes until a set time, as a database
// hiccup does.
type slowCAS struct {
	storage.BlobCAS
	until atomic.Int64
	fail  bool
}

func (cas *slowCAS) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	until := time.Unix(0, cas.until.Load())
	if wait := time.Until(until); wait > 0 {
		if cas.fail {
			return false, errors.New("database unavailable")
		}
		time.Sleep(wait)
	}
	return cas.BlobCAS.CompareAndSwapBlob(name, expected, replacement)
}

// A renewal held up for half the lease, by a write that fails or one that
// is slow, still lands before the lease runs out: the owner keeps it and
// nobody else may claim it meanwhile.
func TestDelayedLeaseRenewalKeepsOwnership(t *testing.T) {
	for _, fail := range []bool{true, false} {
		name := "slow write"
		if fail {
			name = "failing write"
		}
		t.Run(name, func(t *testing.T) {
			ttl := 2400 * time.Millisecond
			provider := storage.NewMemory()
			service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "cluster"})
			service.leaseTTL = ttl
			lease, owned, err := service.claimInventory(t.Context(), provider, "owner")
			if err != nil || !owned {
				t.Fatalf("claim: owned=%t %v", owned, err)
			}
			cas := &slowCAS{BlobCAS: lease.cas, fail: fail}
			claimed := time.Now()
			cas.until.Store(claimed.Add(inventoryLeaseRenewAfter(ttl) + ttl/2).UnixNano())
			lease.cas = cas
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan error, 1)
			go func() { result <- service.keepInventoryLease(ctx, lease) }()

			other := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "cluster"})
			other.leaseTTL = ttl
			for time.Since(claimed) < ttl+ttl/4 {
				select {
				case err := <-result:
					t.Fatalf("the owner gave up its lease after %s: %v", time.Since(claimed), err)
				default:
				}
				if _, stolen, err := other.claimInventory(t.Context(), provider, "other"); err == nil && stolen {
					t.Fatalf("another replica claimed the lease %s after a delayed renewal began", time.Since(claimed))
				}
				time.Sleep(50 * time.Millisecond)
			}
			cancel()
			if err := <-result; err != nil {
				t.Fatalf("lease keeper ended with %v", err)
			}
			if !lease.expiresAt().After(time.Now()) {
				t.Fatal("the delayed renewal did not extend the lease")
			}
		})
	}
}

// Replicas racing to claim an expired lease never both win, and none takes
// a live lease from its owner.
func TestOnlyOneReplicaClaimsTheInventoryLease(t *testing.T) {
	provider := storage.NewMemory()
	services := make([]*Service, 4)
	for i := range services {
		services[i] = newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "cluster"})
		services[i].leaseTTL = 500 * time.Millisecond
	}
	for round := range 12 {
		// Let the previous round's lease run out.
		time.Sleep(520 * time.Millisecond)
		var winners atomic.Int32
		var group sync.WaitGroup
		for i, service := range services {
			group.Add(1)
			go func() {
				defer group.Done()
				if _, owned, err := service.claimInventory(context.Background(), provider, "replica-"+string(rune('a'+i))); err == nil && owned {
					winners.Add(1)
				}
			}()
		}
		group.Wait()
		if got := winners.Load(); got != 1 {
			t.Fatalf("round %d: %d replicas own the lease", round, got)
		}
		for i, service := range services {
			owner := "late-" + string(rune('a'+i))
			if _, owned, err := service.claimInventory(context.Background(), provider, owner); err == nil && owned {
				t.Fatalf("round %d: %s took a live lease", round, owner)
			}
		}
	}
}

// failingFirstReads is a view of the shared store whose first reads of one
// blob fail, as a database that is not reachable yet at start.
type failingFirstReads struct {
	storage.Provider
	key   string
	fails atomic.Int32
	reads atomic.Int32
}

func (provider *failingFirstReads) ReadBlob(name string) ([]byte, error) {
	if name == provider.key {
		provider.reads.Add(1)
	}
	if name == provider.key && provider.fails.Add(-1) >= 0 {
		return nil, errors.New("database unavailable")
	}
	return provider.Provider.ReadBlob(name)
}

func (provider *failingFirstReads) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

// A restarted replica whose first restore fails still keeps its grace when a
// later pass reads the generation saved before the restart: that generation
// is the one it started with, not a new publication.
func TestRestartGraceSurvivesAFailedFirstRestore(t *testing.T) {
	fixture := newLeaseFixture(t, time.Minute)
	crashedOwner := fixture.start()
	fixture.published(crashedOwner)
	saved := fixture.record().Revision
	fixture.crash()
	crashedOwner.StopCapture()
	view := &failingFirstReads{Provider: fixture.provider, key: inventoryKey(fixture.scope)}
	view.fails.Store(2)
	restarted := fixture.startWith(view)
	waitUntil(t, 30*time.Second, "the saved generation to be served after the failed restore", func() bool {
		// Several passes after the failed ones, each reading the lease.
		return servedRevision(restarted) == saved && view.reads.Load() >= 10
	})
	if view.fails.Load() >= 0 {
		t.Fatal("the first restore did not fail")
	}
	if restarted.inventoryFreshFloor().IsZero() {
		t.Fatal("a failed first restore ended the restart grace when the saved generation was read")
	}
	if record := fixture.record(); record.Owner != "crashed-process" || record.Revision != saved {
		t.Fatalf("the crashed lease changed hands during the test: %+v", record)
	}
}
