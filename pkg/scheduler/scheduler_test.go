package scheduler_test

import (
	"context"
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/scheduler"
)

// newSched builds a jitter-free scheduler so timing is deterministic.
func newSched(jobs ...scheduler.Job) *scheduler.Scheduler {
	return scheduler.New(jobs).SetJitter(0)
}

func TestScheduler_RunsAtInterval(t *testing.T) {
	var runs int32
	s := newSched(scheduler.Job{
		Name:     "counter",
		Interval: 20 * time.Millisecond,
		Run: func(ctx context.Context) error {
			atomic.AddInt32(&runs, 1)
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	// Run-immediately + a few intervals ⇒ several runs.
	time.Sleep(120 * time.Millisecond)
	cancel()
	<-done

	if n := atomic.LoadInt32(&runs); n < 3 {
		t.Fatalf("expected the job to run several times, got %d", n)
	}
}

func TestScheduler_SingleFlight(t *testing.T) {
	var (
		concurrent int32
		maxSeen    int32
	)
	s := newSched(scheduler.Job{
		Name:     "slow",
		Interval: 5 * time.Millisecond, // shorter than the run time
		Run: func(ctx context.Context) error {
			cur := atomic.AddInt32(&concurrent, 1)
			for {
				old := atomic.LoadInt32(&maxSeen)
				if cur <= old || atomic.CompareAndSwapInt32(&maxSeen, old, cur) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond) // run is slower than interval
			atomic.AddInt32(&concurrent, -1)
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	if maxSeen > 1 {
		t.Fatalf("job overlapped itself: max concurrent = %d, want 1", maxSeen)
	}
}

func TestScheduler_GracefulShutdownCancelsInFlight(t *testing.T) {
	started := make(chan struct{})
	var canceled int32
	s := newSched(scheduler.Job{
		Name:     "blocker",
		Interval: time.Hour,
		Run: func(ctx context.Context) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done() // block until shutdown
			atomic.StoreInt32(&canceled, 1)
			return ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	<-started // ensure the job is in-flight
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	if atomic.LoadInt32(&canceled) != 1 {
		t.Fatal("in-flight job was not canceled on shutdown")
	}
}

func TestScheduler_NoJobsIsNoOp(t *testing.T) {
	s := scheduler.New(nil)
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
	// Run must return immediately even with a never-canceled context.
	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with no jobs should return immediately")
	}
}

func TestRegistry_RegisterAndSnapshot(t *testing.T) {
	scheduler.Reset()
	t.Cleanup(scheduler.Reset)

	noop := func(ctx context.Context) error { return nil }
	if err := scheduler.Register(scheduler.Job{Name: "j1", Interval: time.Minute, Run: noop}); err != nil {
		t.Fatalf("Register j1: %v", err)
	}
	if err := scheduler.Register(scheduler.Job{Name: "j2", Interval: time.Minute, Run: noop}); err != nil {
		t.Fatalf("Register j2: %v", err)
	}
	if got := len(scheduler.Registered()); got != 2 {
		t.Fatalf("Registered = %d, want 2", got)
	}
	// NewFromRegistry snapshots the registry.
	if scheduler.NewFromRegistry().Len() != 2 {
		t.Fatal("NewFromRegistry should snapshot both jobs")
	}
}

func TestRegister_RejectsInvalidAndDuplicate(t *testing.T) {
	scheduler.Reset()
	t.Cleanup(scheduler.Reset)
	noop := func(ctx context.Context) error { return nil }

	cases := []scheduler.Job{
		{Name: "", Interval: time.Minute, Run: noop}, // missing name
		{Name: "x", Interval: 0, Run: noop},          // bad interval
		{Name: "x", Interval: time.Minute, Run: nil}, // missing Run
	}
	for i, j := range cases {
		if err := scheduler.Register(j); err == nil {
			t.Fatalf("case %d: expected Register to reject %+v", i, j)
		}
	}

	if err := scheduler.Register(scheduler.Job{Name: "dup", Interval: time.Minute, Run: noop}); err != nil {
		t.Fatalf("Register dup: %v", err)
	}
	if err := scheduler.Register(scheduler.Job{Name: "dup", Interval: time.Minute, Run: noop}); err == nil {
		t.Fatal("expected duplicate name to be rejected")
	}
}

func TestScheduler_PanicIsIsolated(t *testing.T) {
	var runs int32
	var once sync.Once
	s := newSched(scheduler.Job{
		Name:     "panicky",
		Interval: 15 * time.Millisecond,
		Run: func(ctx context.Context) error {
			atomic.AddInt32(&runs, 1)
			once.Do(func() { panic("boom") })
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(90 * time.Millisecond)
	cancel()
	<-done

	// A panic on the first run must not kill the job goroutine.
	if n := atomic.LoadInt32(&runs); n < 2 {
		t.Fatalf("job did not recover after panic: runs=%d", n)
	}
}

// ---------------------------------------------------------------------------
// Job-ownership seam
//
// Under HA / multi-instance, a consumer installs an ownership predicate so
// each registered job runs on exactly one instance. These tests drive the
// SAME hash-ownership the enterprise cluster.Identity wires in:
//
//	owns(name) := fnv32(name) % count == index
//
// The OSS scheduler itself is agnostic — it only consults the installed
// predicate — but exercising it through the production predicate documents
// the intended use and proves the seam forms a stable partition.
// ---------------------------------------------------------------------------

// owns is the hash-ownership predicate the enterprise consumer installs.
func owns(name string, index, count int) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int(h.Sum32())%count == index
}

// jobNames is a fixed set large enough to split non-trivially across shards.
var jobNames = []string{
	"intel-slo", "intel-burn", "metric-baseline", "trace-baseline",
	"runbook-reindex", "oncall-sync", "license-refresh", "cursor-compact",
}

// runOwned starts a jitter-free scheduler over jobs and returns the set of
// job names whose Run actually executed. Each owned job signals once on its
// first run; un-owned jobs start no goroutine, so they can never signal.
// The function cancels as soon as every expected-owned job has run.
func runOwned(t *testing.T, jobs []scheduler.Job, expected map[string]bool) map[string]bool {
	t.Helper()

	var mu sync.Mutex
	ran := make(map[string]bool)
	var wg sync.WaitGroup
	wg.Add(len(expected))

	built := make([]scheduler.Job, 0, len(jobs))
	for _, j := range jobs {
		name := j.Name
		built = append(built, scheduler.Job{
			Name:     name,
			Interval: 10 * time.Millisecond,
			Run: func(ctx context.Context) error {
				mu.Lock()
				first := !ran[name]
				ran[name] = true
				mu.Unlock()
				if first {
					wg.Done()
				}
				return nil
			},
		})
	}

	s := scheduler.New(built).SetJitter(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	// Wait until every expected-owned job has run at least once.
	doneRunning := make(chan struct{})
	go func() { wg.Wait(); close(doneRunning) }()
	select {
	case <-doneRunning:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("timed out waiting for owned jobs to run")
	}

	// A short grace period to catch any un-owned job that wrongly started a
	// goroutine/timer, then stop.
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]bool, len(ran))
	for k, v := range ran {
		out[k] = v
	}
	return out
}

// expectedOwned filters jobNames by the hash predicate for (index, count).
func expectedOwned(index, count int) map[string]bool {
	out := make(map[string]bool)
	for _, n := range jobNames {
		if owns(n, index, count) {
			out[n] = true
		}
	}
	return out
}

func makeJobs(names []string) []scheduler.Job {
	noop := func(ctx context.Context) error { return nil }
	jobs := make([]scheduler.Job, 0, len(names))
	for _, n := range names {
		jobs = append(jobs, scheduler.Job{Name: n, Interval: time.Hour, Run: noop})
	}
	return jobs
}

func TestOwnership_CountOneOwnsAll(t *testing.T) {
	t.Cleanup(func() { scheduler.SetOwnership(nil) })

	// No predicate installed: own-everything (community / single-instance).
	expected := make(map[string]bool, len(jobNames))
	for _, n := range jobNames {
		expected[n] = true
	}
	ran := runOwned(t, makeJobs(jobNames), expected)
	if len(ran) != len(jobNames) {
		t.Fatalf("count==0-predicate (own-everything) ran %d jobs, want %d", len(ran), len(jobNames))
	}

	// An explicit count==1 predicate must also own everything.
	scheduler.SetOwnership(func(name string) bool { return owns(name, 0, 1) })
	ran = runOwned(t, makeJobs(jobNames), expected)
	if len(ran) != len(jobNames) {
		t.Fatalf("count==1 ran %d jobs, want %d", len(ran), len(jobNames))
	}
}

func TestOwnership_CountTwoIsDisjointPartition(t *testing.T) {
	t.Cleanup(func() { scheduler.SetOwnership(nil) })

	// Index 0.
	scheduler.SetOwnership(func(name string) bool { return owns(name, 0, 2) })
	ran0 := runOwned(t, makeJobs(jobNames), expectedOwned(0, 2))

	// Index 1.
	scheduler.SetOwnership(func(name string) bool { return owns(name, 1, 2) })
	ran1 := runOwned(t, makeJobs(jobNames), expectedOwned(1, 2))

	assertPartition(t, []map[string]bool{ran0, ran1})

	// Each instance ran exactly the jobs it owns — no more, no less.
	if !sameSet(ran0, expectedOwned(0, 2)) {
		t.Fatalf("index 0 ran %v, want %v", keys(ran0), keys(expectedOwned(0, 2)))
	}
	if !sameSet(ran1, expectedOwned(1, 2)) {
		t.Fatalf("index 1 ran %v, want %v", keys(ran1), keys(expectedOwned(1, 2)))
	}
}

func TestOwnership_StableAcrossRestarts(t *testing.T) {
	t.Cleanup(func() { scheduler.SetOwnership(nil) })
	scheduler.SetOwnership(func(name string) bool { return owns(name, 1, 3) })

	first := runOwned(t, makeJobs(jobNames), expectedOwned(1, 3))
	// "Restart": construct and run a fresh scheduler with the same predicate.
	second := runOwned(t, makeJobs(jobNames), expectedOwned(1, 3))

	if !sameSet(first, second) {
		t.Fatalf("ownership not stable across restarts: %v vs %v", keys(first), keys(second))
	}
}

func TestOwnership_ChangesWithCount(t *testing.T) {
	t.Cleanup(func() { scheduler.SetOwnership(nil) })

	// Across each count, the per-index owned sets must form a disjoint
	// partition covering every job.
	for _, count := range []int{1, 2, 3, 5} {
		sets := make([]map[string]bool, 0, count)
		for index := 0; index < count; index++ {
			scheduler.SetOwnership(func(name string) bool { return owns(name, index, count) })
			expected := expectedOwned(index, count)
			ran := runOwned(t, makeJobs(jobNames), expected)
			if !sameSet(ran, expected) {
				t.Fatalf("count=%d index=%d ran %v, want %v", count, index, keys(ran), keys(expected))
			}
			sets = append(sets, ran)
		}
		assertPartition(t, sets)
	}

	// Rescaling moves at least one job to a different owner: the count=2
	// partition must differ from the count=3 partition.
	owner2 := ownerByCount(2)
	owner3 := ownerByCount(3)
	moved := false
	for _, n := range jobNames {
		if owner2[n] != owner3[n] {
			moved = true
			break
		}
	}
	if !moved {
		t.Fatal("rescaling 2->3 moved no job; expected ownership to change with count")
	}
}

func TestOwnership_UnownedJobStartsNothing(t *testing.T) {
	t.Cleanup(func() { scheduler.SetOwnership(nil) })

	var ran int32
	// A predicate that owns nothing: the scheduler must start no goroutine
	// and no timer, and Run must return immediately.
	scheduler.SetOwnership(func(name string) bool { return false })
	s := scheduler.New([]scheduler.Job{{
		Name:     "lonely",
		Interval: 5 * time.Millisecond,
		Run: func(ctx context.Context) error {
			atomic.AddInt32(&ran, 1)
			return nil
		},
	}}).SetJitter(0)

	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with no owned jobs should return immediately")
	}

	// Give any errant timer a chance to fire — it must not, because no
	// goroutine was ever started for the un-owned job.
	time.Sleep(40 * time.Millisecond)
	if n := atomic.LoadInt32(&ran); n != 0 {
		t.Fatalf("un-owned job ran %d times; want 0 (no goroutine/timer)", n)
	}
}

// TestOwns_Predicate exercises the Owns query companion to SetOwnership: it is
// the gate a wall-clock action (e.g. the daily report send) checks OUTSIDE the
// Scheduler.Run loop so exactly one replica fires under HA.
func TestOwns_Predicate(t *testing.T) {
	t.Cleanup(func() { scheduler.SetOwnership(nil) })

	// No predicate installed → own-everything (community / single-instance).
	scheduler.SetOwnership(nil)
	if !scheduler.Owns("report-daily-digest") {
		t.Fatal("Owns must be true with no predicate (own-everything)")
	}

	// A predicate that owns nothing → Owns is false for every name.
	scheduler.SetOwnership(func(string) bool { return false })
	if scheduler.Owns("report-daily-digest") {
		t.Fatal("Owns must be false when the predicate owns nothing")
	}

	// A selective predicate is consulted by name.
	scheduler.SetOwnership(func(name string) bool { return name == "mine" })
	if !scheduler.Owns("mine") {
		t.Fatal("Owns(mine) must be true")
	}
	if scheduler.Owns("theirs") {
		t.Fatal("Owns(theirs) must be false")
	}
}

// ---------------------------------------------------------------------------
// Ownership test helpers
// ---------------------------------------------------------------------------

// ownerByCount maps each job name to its owning index for the given count.
func ownerByCount(count int) map[string]int {
	out := make(map[string]int, len(jobNames))
	for _, n := range jobNames {
		for index := 0; index < count; index++ {
			if owns(n, index, count) {
				out[n] = index
				break
			}
		}
	}
	return out
}

// assertPartition checks the sets are pairwise disjoint and together cover
// every job name exactly once (a stable partition of the registered jobs).
func assertPartition(t *testing.T, sets []map[string]bool) {
	t.Helper()
	seen := make(map[string]int)
	for _, s := range sets {
		for name := range s {
			seen[name]++
		}
	}
	for _, n := range jobNames {
		switch seen[n] {
		case 0:
			t.Fatalf("job %q owned by no instance (not a covering partition)", n)
		case 1:
			// exactly one owner — correct
		default:
			t.Fatalf("job %q owned by %d instances (not disjoint)", n, seen[n])
		}
	}
	if len(seen) != len(jobNames) {
		t.Fatalf("partition covered %d distinct jobs, want %d", len(seen), len(jobNames))
	}
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
