package collect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(step time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(step)
	clock.mu.Unlock()
}

func unitAttempts(t *testing.T, store *snapshot.Store, service string) time.Time {
	t.Helper()
	statuses, err := store.Sync(context.Background(), snapshot.Query{Account: productionAccount, Service: service})
	if err != nil || len(statuses) == 0 {
		t.Fatalf("%s statuses = %+v, %v", service, statuses, err)
	}
	return statuses[0].Sync.AttemptedAt
}

func TestSchedulerRunsEachKindOnItsInterval(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	clock := &fakeClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	storeClock := &fakeClock{now: clock.now}
	h.store = snapshot.New(h.provider, h.store.Scope(), snapshot.WithClock(storeClock.Now))
	h.collector.store = h.store
	enabled := true
	scheduler := NewScheduler(h.collector,
		func(context.Context) []Target { return []Target{target(productionAccount)} },
		func() Intervals {
			return Intervals{Inventory: 15 * time.Minute, Health: 2 * time.Minute, Changes: 5 * time.Minute}
		},
		func() bool { return enabled },
		WithSchedulerClock(clock.Now))

	scheduler.RunDue(context.Background())
	firstHealth, firstInventory := unitAttempts(t, h.store, "ecs"), unitAttempts(t, h.store, "lambda")
	if firstHealth.IsZero() || firstInventory.IsZero() {
		t.Fatal("first pass did not run every kind")
	}

	clock.Advance(3 * time.Minute)
	storeClock.Advance(3 * time.Minute)
	scheduler.RunDue(context.Background())
	if !unitAttempts(t, h.store, "ecs").After(firstHealth) {
		t.Fatal("health did not rerun after its interval")
	}
	if !unitAttempts(t, h.store, "lambda").Equal(firstInventory) {
		t.Fatal("inventory reran before its interval")
	}

	scheduler.Trigger(productionAccount)
	storeClock.Advance(time.Second)
	scheduler.RunDue(context.Background())
	if !unitAttempts(t, h.store, "lambda").After(firstInventory) {
		t.Fatal("trigger did not force a full sync")
	}

	enabled = false
	requests := h.fake.TotalRequests()
	clock.Advance(time.Hour)
	scheduler.RunDue(context.Background())
	if h.fake.TotalRequests() != requests {
		t.Fatal("disabled scheduler still called AWS")
	}
}

func TestSchedulerStartStopsWithContext(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	ran := make(chan struct{}, 1)
	scheduler := NewScheduler(h.collector,
		func(context.Context) []Target {
			select {
			case ran <- struct{}{}:
			default:
			}
			return nil
		},
		func() Intervals { return Intervals{} },
		nil, WithTick(time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Start(ctx)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not run")
	}
	cancel()
	select {
	case <-scheduler.Stopped():
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop")
	}
}

// Bucket and key policy reads are metered: they run on the first pass and
// then no more often than every six hours, whatever the inventory interval.
func TestPolicyUnitsRunAtMostEverySixHours(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	clock := &fakeClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	storeClock := &fakeClock{now: clock.now}
	h.store = snapshot.New(h.provider, h.store.Scope(), snapshot.WithClock(storeClock.Now))
	h.collector.store = h.store
	scheduler := NewScheduler(h.collector,
		func(context.Context) []Target { return []Target{target(productionAccount)} },
		func() Intervals {
			return Intervals{Inventory: 15 * time.Minute, Health: 2 * time.Minute, Changes: 5 * time.Minute}
		},
		func() bool { return true },
		WithSchedulerClock(clock.Now))
	advance := func(step time.Duration) {
		clock.Advance(step)
		storeClock.Advance(step)
		scheduler.RunDue(context.Background())
	}
	scheduler.RunDue(context.Background())
	firstPolicy, firstKeys := unitAttempts(t, h.store, "s3-policy"), unitAttempts(t, h.store, "kms")
	firstInventory := unitAttempts(t, h.store, "lambda")
	if firstPolicy.IsZero() || firstKeys.IsZero() {
		t.Fatal("first pass did not read the policies")
	}
	for range 23 {
		advance(15 * time.Minute)
	}
	if !unitAttempts(t, h.store, "lambda").After(firstInventory) {
		t.Fatal("inventory did not rerun")
	}
	if !unitAttempts(t, h.store, "s3-policy").Equal(firstPolicy) || !unitAttempts(t, h.store, "kms").Equal(firstKeys) {
		t.Fatal("policy reads ran again before six hours")
	}
	advance(15 * time.Minute)
	if !unitAttempts(t, h.store, "s3-policy").After(firstPolicy) || !unitAttempts(t, h.store, "kms").After(firstKeys) {
		t.Fatal("policy reads did not run after six hours")
	}
	if got := (Intervals{Inventory: 12 * time.Hour}).of(KindPolicy); got != 12*time.Hour {
		t.Fatalf("policy interval with a slower inventory = %s", got)
	}
}

// The policy cadence lives in the store: a scheduler started again on the
// same store, or a second replica's scheduler beside the first, makes no
// policy read inside six hours of the last one.
func TestPolicyCadenceSurvivesRestartAndIsSharedByReplicas(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	clock := &fakeClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	h.store = snapshot.New(h.provider, h.store.Scope(), snapshot.WithClock(clock.Now))
	h.collector.store = h.store
	newScheduler := func() *Scheduler {
		collector := New(h.store, h.limiter, WithHTTPClient(h.fake.HTTPClient()), WithSleep(noSleep), WithRetries(0))
		return NewScheduler(collector,
			func(context.Context) []Target { return []Target{target(productionAccount)} },
			func() Intervals {
				return Intervals{Inventory: 15 * time.Minute, Health: 2 * time.Minute, Changes: 5 * time.Minute}
			},
			func() bool { return true },
			WithSchedulerClock(clock.Now))
	}
	policyReads := func() int64 {
		return h.fake.OperationCount("kms:ListKeys") + h.fake.OperationCount("ecr:DescribeRepositories")
	}
	first := newScheduler()
	first.RunDue(context.Background())
	reads := policyReads()
	if reads == 0 {
		t.Fatal("the first pass made no policy read")
	}
	firstAttempt := unitAttempts(t, h.store, "kms")

	clock.Advance(time.Hour)
	restarted := newScheduler()
	restarted.RunDue(context.Background())
	replica := newScheduler()
	replica.Trigger(productionAccount)
	replica.RunDue(context.Background())
	if got := policyReads(); got != reads {
		t.Fatalf("policy reads after a restart and a second replica = %d, want %d", got, reads)
	}
	if !unitAttempts(t, h.store, "kms").Equal(firstAttempt) {
		t.Fatal("a policy unit was attempted again inside six hours")
	}

	clock.Advance(5*time.Hour + time.Minute)
	restarted.RunDue(context.Background())
	replica.RunDue(context.Background())
	if got := policyReads(); got == reads {
		t.Fatal("no policy read after six hours")
	}
	after := policyReads()
	replica.RunDue(context.Background())
	first.RunDue(context.Background())
	if got := policyReads(); got != after {
		t.Fatalf("a second replica read the policies again: %d, want %d", got, after)
	}
}

// The cadence is per unit: a Region added after the last policy read reads
// its own policy units at once and leaves every other unit alone.
func TestNewRegionReadsOnlyItsOwnPolicyUnits(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	clock := &fakeClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	h.store = snapshot.New(h.provider, h.store.Scope(), snapshot.WithClock(clock.Now))
	h.collector.store = h.store
	regions := []string{testRegion}
	scheduler := NewScheduler(h.collector,
		func(context.Context) []Target { return []Target{target(productionAccount, regions...)} },
		func() Intervals {
			return Intervals{Inventory: 15 * time.Minute, Health: 2 * time.Minute, Changes: 5 * time.Minute}
		},
		func() bool { return true },
		WithSchedulerClock(clock.Now))
	attempts := func() map[string]time.Time {
		statuses, err := h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]time.Time{}
		for _, status := range statuses {
			if KindOf(status.Unit.Service) == KindPolicy {
				out[status.Unit.Region+"/"+status.Unit.Service] = status.Sync.AttemptedAt
			}
		}
		return out
	}
	scheduler.RunDue(context.Background())
	before := attempts()
	if len(before) != 3 {
		t.Fatalf("policy units after the first pass = %v", before)
	}
	clock.Advance(time.Hour)
	regions = []string{testRegion, "us-west-2"}
	scheduler.RunDue(context.Background())
	after := attempts()
	for key, at := range before {
		if !after[key].Equal(at) {
			t.Errorf("%s was read again inside six hours", key)
		}
	}
	for _, service := range []string{serviceKMS, serviceECR} {
		if at := after["us-west-2/"+service]; !at.Equal(clock.Now()) {
			t.Errorf("us-west-2 %s attempted at %v, want now", service, at)
		}
	}
}

// A policy unit that was never attempted runs on the next trigger, even
// when this process read the account's policies a moment ago: the account
// was purged and connected again, so its units start over. A unit inside
// its interval is still not read again by a trigger.
func TestTriggerRunsPolicyUnitsNeverAttempted(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	clock := &fakeClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	h.store = snapshot.New(h.provider, h.store.Scope(), snapshot.WithClock(clock.Now))
	h.collector.store = h.store
	scheduler := NewScheduler(h.collector,
		func(context.Context) []Target { return []Target{target(productionAccount)} },
		func() Intervals {
			return Intervals{Inventory: 15 * time.Minute, Health: 2 * time.Minute, Changes: 5 * time.Minute}
		},
		func() bool { return true },
		WithSchedulerClock(clock.Now))
	scheduler.RunDue(context.Background())
	first := unitAttempts(t, h.store, "kms")
	if first.IsZero() {
		t.Fatal("the first pass made no policy read")
	}

	clock.Advance(time.Minute)
	scheduler.Trigger(productionAccount)
	scheduler.RunDue(context.Background())
	if !unitAttempts(t, h.store, "kms").Equal(first) {
		t.Fatal("a trigger read a policy unit again inside its interval")
	}

	if err := h.store.PurgeAccount(productionAccount); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	scheduler.Trigger(productionAccount)
	scheduler.RunDue(context.Background())
	for _, service := range []string{"kms", "ecr", "s3-policy"} {
		if at := unitAttempts(t, h.store, service); !at.Equal(clock.Now()) {
			t.Errorf("%s attempted at %v after the purge, want now", service, at)
		}
	}
}
