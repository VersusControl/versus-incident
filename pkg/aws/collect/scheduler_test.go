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
