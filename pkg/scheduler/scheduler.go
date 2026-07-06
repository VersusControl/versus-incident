// Package scheduler is the generic, in-process recurring-evaluation seam
// on the agent runtime. It runs registered jobs at a fixed interval
// inside the worker lifecycle, so a consumer can re-pull signals and derive
// standing state on a schedule (e.g. learn metric baselines, evaluate SLO
// burn) without adding any new mutation path.
//
// It is deliberately unopinionated and tier-neutral: the scheduler owns
// timing and lifecycle only; the content of every job belongs to the
// consumer. Registration mirrors the middleware.SetOrgResolver /
// SetAuthMiddleware seam — a consumer (the enterprise hooks.Register)
// registers jobs once at boot, before the worker starts. OSS registers
// none, so the scheduler is a no-op and community behaviour is byte-for-
// byte unchanged (no goroutines, no timers).
//
// READ-ONLY CONTRACT (enforced by review + the import-graph guard in
// guard_test.go): a Job MUST be read-only / analyze-kind. It may pull
// signals and derive state, but it MUST NOT mutate cluster state and MUST
// NOT bypass the one emission path (services.CreateIncident) — findings it
// raises flow through that single path exactly like on-demand analyze.
// This package itself imports no write/emit/storage path, so a registered
// job cannot acquire write capability through the scheduler.
package scheduler

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"
)

// defaultJitter caps the random startup delay applied before a job's first
// run, so multiple jobs (and multiple replicas) don't all fire at the same
// instant. It is clamped to the job interval for very short intervals.
const defaultJitter = 5 * time.Second

// Job is one registered recurring evaluation.
type Job struct {
	// Name uniquely identifies the job (used in logs and dedup). Required.
	Name string
	// Interval is the period between runs. Required; must be > 0.
	Interval time.Duration
	// Run executes one evaluation. It MUST honour ctx cancellation and MUST
	// be read-only / analyze-kind (see the package contract). Required.
	Run func(ctx context.Context) error
}

func (j Job) validate() error {
	if j.Name == "" {
		return fmt.Errorf("scheduler: job name is required")
	}
	if j.Interval <= 0 {
		return fmt.Errorf("scheduler: job %q interval must be > 0", j.Name)
	}
	if j.Run == nil {
		return fmt.Errorf("scheduler: job %q Run is required", j.Name)
	}
	return nil
}

// Process-wide registry. A consumer registers jobs at boot; the worker
// snapshots the registry when it constructs its Scheduler.
var (
	mu       sync.Mutex
	registry []Job
)

// Register adds a job to the process-wide registry. Call once at boot,
// before the worker starts. Returns an error for an invalid job or a
// duplicate name. This is the setter the enterprise hooks.Register
// attaches to (mirror of middleware.SetOrgResolver).
func Register(j Job) error {
	if err := j.validate(); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	for _, existing := range registry {
		if existing.Name == j.Name {
			return fmt.Errorf("scheduler: job %q already registered", j.Name)
		}
	}
	registry = append(registry, j)
	return nil
}

// Registered returns a copy of the registered jobs.
func Registered() []Job {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Job, len(registry))
	copy(out, registry)
	return out
}

// Reset clears the registry. Test-only helper to isolate cases; not used
// in production.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	registry = nil
}

// Process-wide job-ownership seam. Under HA / multi-instance,
// exactly one instance must run each registered job — otherwise every
// replica fires the same periodic evaluation and (for the enterprise
// SLO/burn job) pages on-call twice. A consumer installs a predicate at
// boot; the scheduler runs a job only when the predicate owns its name.
//
// It mirrors the SetModeResolver / middleware.SetOrgResolver last-wins
// seam: one process-wide slot, mutex-guarded so a boot-time registration
// is safely visible to the worker goroutine. It is deliberately
// tier-neutral — the scheduler knows nothing about ordinals, hashing, or
// replica counts; the predicate (the enterprise cluster.Identity wires
// `owns(name) := fnv32(name) % count == index` into it) is the consumer's.
//
// DEFAULT = OWN-EVERYTHING. OSS installs no predicate, so every job runs
// exactly as today — community and single-instance are byte-for-byte
// unchanged (one nil-check before Run, no extra goroutines, no timers).
var (
	ownerMu   sync.Mutex
	ownerSlot func(name string) bool
)

// SetOwnership installs the process-wide job-ownership predicate. A job
// runs on this instance only when pred(job.Name) is true. Passing nil
// clears the slot (back to own-everything). Last-wins: a second call
// replaces the first. Call at boot, before the worker starts. This is the
// entry point the enterprise cluster.Identity attaches to (mirror of
// middleware.SetOrgResolver); community OSS installs none.
func SetOwnership(pred func(name string) bool) {
	ownerMu.Lock()
	defer ownerMu.Unlock()
	ownerSlot = pred
}

// ownership returns the installed predicate, or nil when none is set
// (community / single-instance own-everything).
func ownership() func(name string) bool {
	ownerMu.Lock()
	defer ownerMu.Unlock()
	return ownerSlot
}

// Scheduler runs a fixed set of jobs until its context is canceled.
// Construct it with New (explicit jobs) or NewFromRegistry (the
// process-wide registry snapshot).
type Scheduler struct {
	jobs   []Job
	jitter time.Duration
}

// New returns a Scheduler over the given jobs with the default startup
// jitter.
func New(jobs []Job) *Scheduler {
	return &Scheduler{jobs: jobs, jitter: defaultJitter}
}

// NewFromRegistry returns a Scheduler over a snapshot of the process-wide
// registry. The worker calls this; in community mode the snapshot is empty
// and Run is a no-op.
func NewFromRegistry() *Scheduler {
	return New(Registered())
}

// SetJitter overrides the startup jitter (use 0 for deterministic tests).
// Returns the receiver for chaining.
func (s *Scheduler) SetJitter(d time.Duration) *Scheduler {
	s.jitter = d
	return s
}

// Len reports how many jobs the scheduler holds in its snapshot. Run starts
// a goroutine and timer only for the subset this instance owns under the
// installed ownership predicate; with no predicate (community /
// single-instance) the owned set is every job, so Run behaves exactly as
// before.
func (s *Scheduler) Len() int { return len(s.jobs) }

// owned returns the subset of this scheduler's jobs that this instance owns
// under the installed ownership predicate, in registration order.
// With no predicate (community / single-instance) every job is owned and
// the result is the full set — so Run starts exactly the goroutines it does
// today. A job the predicate rejects is never returned here, so Run spins up
// no goroutine and no timer for it.
func (s *Scheduler) owned() []Job {
	pred := ownership()
	if pred == nil {
		return s.jobs
	}
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if pred(j.Name) {
			out = append(out, j)
		}
	}
	return out
}

// Run drives every owned job until ctx is canceled, one goroutine per job,
// and returns once all jobs have stopped. With no owned jobs it returns
// immediately and starts nothing — the OSS runtime is unaffected, and an
// un-owned job (one the ownership predicate rejects) starts no goroutine and
// no timer.
func (s *Scheduler) Run(ctx context.Context) {
	jobs := s.owned()
	if len(jobs) == 0 {
		return
	}
	log.Printf("scheduler: starting %d recurring evaluation job(s)", len(jobs))
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			s.runJob(ctx, j)
		}(j)
	}
	wg.Wait()
	log.Printf("scheduler: all jobs stopped")
}

// runJob drives one job: optional startup jitter, then run-immediately
// followed by run-every-interval. Single-flight is structural — the run is
// synchronous in this goroutine, so the next tick can never start before
// the current run returns.
func (s *Scheduler) runJob(ctx context.Context, j Job) {
	if jit := s.jitterFor(j); jit > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(rand.Int63n(int64(jit)))):
		}
	}

	ticker := time.NewTicker(j.Interval)
	defer ticker.Stop()

	for {
		s.invoke(ctx, j)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) jitterFor(j Job) time.Duration {
	jit := s.jitter
	if jit > j.Interval {
		jit = j.Interval
	}
	return jit
}

// invoke runs the job once, isolating panics and suppressing the error log
// when the failure is just the context being canceled mid-run.
func (s *Scheduler) invoke(ctx context.Context, j Job) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("scheduler: job %q panicked: %v", j.Name, r)
		}
	}()
	if err := j.Run(ctx); err != nil && ctx.Err() == nil {
		log.Printf("scheduler: job %q failed: %v", j.Name, err)
	}
}
