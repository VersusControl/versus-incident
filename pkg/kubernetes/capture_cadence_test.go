package kubernetes

import (
	"context"
	"sync"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// captureCadenceFixture is an owned continuous capture loop on a virtual
// clock, so a test can run a minute of one-second capture passes at once. The
// published inventory revision is what a new inventory publication changes.
type captureCadenceFixture struct {
	t       *testing.T
	service *Service
	loop    *captureLoop
	cache   *inventoryCache
	mu      sync.Mutex
	now     time.Time
	start   time.Time
	// state returns the inventory state of one kind at the given time.
	state func(kind string, now time.Time) kubeindex.KindStatus
}

func newCaptureCadenceFixture(t *testing.T) *captureCadenceFixture {
	t.Helper()
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	start := time.Now().UTC().Truncate(time.Second)
	fixture := &captureCadenceFixture{t: t, service: service, now: start, start: start}
	fixture.state = func(kind string, now time.Time) kubeindex.KindStatus {
		return kubeindex.KindStatus{State: "ready", ObservedAt: now, LastAttemptAt: now}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fixture.loop = &captureLoop{ctx: ctx, cancel: cancel, done: make(chan struct{}), owner: "owner", owned: true, ownedContext: ctx, next: map[string]time.Time{}, success: map[string]time.Time{}, completed: map[string]time.Time{}, gapFrom: map[string]time.Time{}, status: CaptureStatus{Mode: "continuous"}, now: fixture.clock}
	fixture.cache = &inventoryCache{generation: inventoryGeneration{Revision: "first"}}
	scope := service.indexScope()
	service.changes.mu.Lock()
	service.changes.captures[scope] = fixture.loop
	service.changes.inventories[scope] = fixture.cache
	service.changes.mu.Unlock()
	if err := service.changeStore().ClaimLease("owner", start, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *captureCadenceFixture) clock() time.Time {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.now
}

// publish stands for a new inventory publication.
func (fixture *captureCadenceFixture) publish(revision string) {
	fixture.cache.mu.Lock()
	fixture.cache.generation.Revision = revision
	fixture.cache.mu.Unlock()
}

// tick runs one capture pass at the current virtual time and returns the
// kinds it captured.
func (fixture *captureCadenceFixture) tick() []string {
	t := fixture.t
	t.Helper()
	var captured []string
	// Keep the history lease, as the loop's renewal does.
	if err := fixture.service.changeStore().RenewLease("owner", fixture.clock(), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	fixture.service.captureTick(fixture.loop, func(due []string) {
		captured = due
		now := fixture.clock()
		snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{}}
		for _, kind := range due {
			snapshot.Kinds[kind] = fixture.state(kind, now)
		}
		if err := fixture.service.captureCycle(fixture.service.changeStore(), nil, snapshot, due, now, "owner"); err != nil {
			t.Fatalf("capture cycle at %s: %v", now.Sub(fixture.start), err)
		}
	})
	return captured
}

// minute runs sixty one-second capture passes and returns how many ran a
// capture cycle, and how often each kind was captured.
func (fixture *captureCadenceFixture) minute() (int, map[string]int) {
	cycles, perKind := 0, map[string]int{}
	for range 60 {
		if captured := fixture.tick(); len(captured) > 0 {
			cycles++
			for _, kind := range captured {
				perKind[kind]++
			}
		}
		fixture.advance(time.Second)
	}
	return cycles, perKind
}

func (fixture *captureCadenceFixture) advance(d time.Duration) {
	fixture.mu.Lock()
	fixture.now = fixture.now.Add(d)
	fixture.mu.Unlock()
}

// maxCapturesPerMinute bounds how often a kind whose inventory state does not
// move may be captured: a short backoff while it may still settle, then
// about its own cadence. Before the backoff such a kind was captured on every
// one-second pass, sixty times a minute.
const maxCapturesPerMinute = 8

// A kind whose inventory does not move, because it is still warming, holds
// no objects the reader may list, or was not listed again since its last
// capture, does not run a capture cycle every second.
func TestCaptureCyclesPerMinuteStayBoundedForAKindThatDoesNotMove(t *testing.T) {
	cases := []struct {
		name, kind string
		state      func(start, now time.Time) kubeindex.KindStatus
	}{
		{"kind still warming", "Job", func(_, _ time.Time) kubeindex.KindStatus {
			return kubeindex.KindStatus{State: "warming", Error: "syncing"}
		}},
		{"kind never listed", "HorizontalPodAutoscaler", func(_, _ time.Time) kubeindex.KindStatus {
			return kubeindex.KindStatus{}
		}},
		{"kind not listed again since its last capture", "Pod", func(start, _ time.Time) kubeindex.KindStatus {
			return kubeindex.KindStatus{State: "ready", ObservedAt: start, LastAttemptAt: start}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCaptureCadenceFixture(t)
			fixture.state = func(kind string, now time.Time) kubeindex.KindStatus {
				if kind == tc.kind {
					return tc.state(fixture.start, now)
				}
				return kubeindex.KindStatus{State: "ready", ObservedAt: now, LastAttemptAt: now}
			}
			cycles, perKind := fixture.minute()
			if perKind[tc.kind] > maxCapturesPerMinute || cycles > maxCapturesPerMinute+2 {
				t.Fatalf("%s was captured %d times and ran %d capture cycles in one minute, want at most %d captures", tc.kind, perKind[tc.kind], cycles, maxCapturesPerMinute)
			}
		})
	}
}

// Kinds whose inventory moves keep their own cadence: the 30-second kinds
// twice a minute, the rest once.
func TestCaptureCyclesPerMinuteForHealthyKindsFollowTheirCadence(t *testing.T) {
	fixture := newCaptureCadenceFixture(t)
	cycles, perKind := fixture.minute()
	if cycles != 2 || perKind["Pod"] != 2 || perKind["Job"] != 1 || perKind["ConfigMap"] != 1 {
		t.Fatalf("healthy kinds ran %d cycles in one minute, captures %v", cycles, perKind)
	}
}

// untilWaiting runs passes until kind waits out a retry delay longer than
// one pass.
func (fixture *captureCadenceFixture) untilWaiting(kind string) {
	t := fixture.t
	t.Helper()
	for range 120 {
		fixture.loop.mu.Lock()
		next, retrying := fixture.loop.next[kind], fixture.loop.retries[kind] > 0
		fixture.loop.mu.Unlock()
		if retrying && next.After(fixture.clock().Add(time.Second)) {
			return
		}
		fixture.tick()
		fixture.advance(time.Second)
	}
	t.Fatalf("%s never waited out a retry delay", kind)
}

func capturedKind(captured []string, kind string) bool {
	for _, name := range captured {
		if name == kind {
			return true
		}
	}
	return false
}

// A kind waiting out its retry delay is captured on the next pass after the
// inventory publishes again, whether the publication lists it again or
// brings it out of warming, and its delay starts over. Without a publication
// it waits.
func TestCaptureRetryDelayEndsAtTheNextInventoryPublication(t *testing.T) {
	cases := []struct {
		name, kind string
		stuck      func(start time.Time) kubeindex.KindStatus
	}{
		{"kind listed again", "Pod", func(start time.Time) kubeindex.KindStatus {
			return kubeindex.KindStatus{State: "ready", ObservedAt: start, LastAttemptAt: start}
		}},
		{"kind becomes ready", "Job", func(time.Time) kubeindex.KindStatus {
			return kubeindex.KindStatus{State: "warming", Error: "syncing"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCaptureCadenceFixture(t)
			var moved time.Time
			fixture.state = func(kind string, now time.Time) kubeindex.KindStatus {
				if kind != tc.kind {
					return kubeindex.KindStatus{State: "ready", ObservedAt: now, LastAttemptAt: now}
				}
				if !moved.IsZero() {
					return kubeindex.KindStatus{State: "ready", ObservedAt: moved, LastAttemptAt: moved}
				}
				return tc.stuck(fixture.start)
			}
			fixture.minute()
			fixture.untilWaiting(tc.kind)
			if captured := fixture.tick(); capturedKind(captured, tc.kind) {
				t.Fatalf("%s was captured inside its delay with no new publication", tc.kind)
			}
			fixture.advance(time.Second)

			moved = fixture.clock()
			fixture.publish("second")
			if captured := fixture.tick(); !capturedKind(captured, tc.kind) {
				t.Fatalf("%s was not captured on the pass after a new publication: captured %v", tc.kind, captured)
			}
			fixture.loop.mu.Lock()
			success, retries, next := fixture.loop.success[tc.kind], fixture.loop.retries[tc.kind], fixture.loop.next[tc.kind]
			fixture.loop.mu.Unlock()
			cadence := fixture.service.collectionCadence(captureCadences[tc.kind])
			if !success.Equal(moved) || retries != 0 || !next.Equal(moved.Add(cadence)) {
				t.Fatalf("%s after the publication: success %s (want %s), retries %d, next %s", tc.kind, success, moved, retries, next)
			}
		})
	}
}

// The revision a retrying kind waits on is read before the pass, so a
// publication that lands while the pass runs still makes the kind due on the
// next pass.
func TestCapturePublicationDuringAPassCountsAsNew(t *testing.T) {
	fixture := newCaptureCadenceFixture(t)
	fixture.state = func(kind string, now time.Time) kubeindex.KindStatus {
		if kind == "Job" {
			return kubeindex.KindStatus{State: "warming", Error: "syncing"}
		}
		return kubeindex.KindStatus{State: "ready", ObservedAt: now, LastAttemptAt: now}
	}
	fixture.minute()
	fixture.untilWaiting("Job")
	fixture.loop.mu.Lock()
	fixture.loop.next["Pod"] = time.Time{}
	fixture.loop.mu.Unlock()
	fixture.service.captureTick(fixture.loop, func(due []string) {
		if capturedKind(due, "Job") {
			t.Fatal("Job was due inside its delay")
		}
		fixture.publish("during")
	})
	if captured := fixture.tick(); !capturedKind(captured, "Job") {
		t.Fatalf("a publication made during the previous pass did not make Job due: captured %v", captured)
	}
}
