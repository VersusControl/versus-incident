package limit

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (clock *fakeClock) Now() time.Time { return clock.now }

func TestUsageCountsGrantsAcrossRegions(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	manager := newManager(2, clock.Now)
	ctx := context.Background()

	for index := 0; index < 3; index++ {
		if err := manager.Wait(ctx, "111111111111", "us-east-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.Wait(ctx, "111111111111", "eu-west-1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Wait(ctx, "222222222222", "us-east-1"); err != nil {
		t.Fatal(err)
	}

	usage := manager.Usage("111111111111")
	if usage.BudgetRPS != 4 {
		t.Fatalf("expected budget 4 across two regions, got %d", usage.BudgetRPS)
	}
	if usage.UsedRPS != 0.07 {
		t.Fatalf("expected 0.07 used rps, got %v", usage.UsedRPS)
	}
	if other := manager.Usage("222222222222"); other.UsedRPS != 0.02 || other.BudgetRPS != 2 {
		t.Fatalf("unknown account leaked into the meter: %#v", other)
	}
}

func TestUsageDropsGrantsOutsideWindow(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	manager := newManager(10, clock.Now)
	if err := manager.Wait(context.Background(), "111111111111", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(Window)
	if usage := manager.Usage("111111111111"); usage.UsedRPS != 0 {
		t.Fatalf("stale grant stayed in the window: %#v", usage)
	}
}

func TestThrottlingBacksOffThenRecovers(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	manager := newManager(1000, clock.Now)
	ctx := context.Background()

	if pause := manager.Throttled("111111111111", "us-east-1"); pause != baseBackoff {
		t.Fatalf("expected first pause %v, got %v", baseBackoff, pause)
	}
	if err := manager.Wait(ctx, "111111111111", "us-east-1"); !errors.Is(err, ErrBackoff) {
		t.Fatalf("expected backoff, got %v", err)
	}
	clock.now = clock.now.Add(baseBackoff)
	if err := manager.Wait(ctx, "111111111111", "us-east-1"); err != nil {
		t.Fatalf("expected recovery after the pause, got %v", err)
	}

	if pause := manager.Throttled("111111111111", "us-east-1"); pause != 2*baseBackoff {
		t.Fatalf("expected doubled pause %v, got %v", 2*baseBackoff, pause)
	}
	manager.Succeeded("111111111111", "us-east-1")
	if pause := manager.Throttled("111111111111", "us-east-1"); pause != baseBackoff {
		t.Fatalf("success did not reset the pause: %v", pause)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	manager := newManager(1000, clock.Now)
	var pause time.Duration
	for index := 0; index < 10; index++ {
		pause = manager.Throttled("111111111111", "us-east-1")
	}
	if pause != maxBackoff {
		t.Fatalf("expected capped pause %v, got %v", maxBackoff, pause)
	}
}

func TestNonPositiveRateIsGuarded(t *testing.T) {
	manager := NewManager(0)
	if manager.maxRPS != 1 {
		t.Fatalf("expected a one request guard, got %d", manager.maxRPS)
	}
}

func TestExpectFillsTheAccountBudget(t *testing.T) {
	manager := NewManager(2)
	manager.Expect("111111111111", []string{"us-east-1", "eu-west-1", "ap-south-1"})
	usage := manager.Usage("111111111111")
	if usage.BudgetRPS != 6 || usage.UsedRPS != 0 {
		t.Fatalf("unexpected budget before any call: %#v", usage)
	}
}
