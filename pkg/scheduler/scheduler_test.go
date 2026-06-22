package scheduler_test

import (
	"context"
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
