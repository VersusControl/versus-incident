package kubernetes

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestExpiringBearerSourceRefreshesOnceWithSkew(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	calls := 0
	source := &expiringBearerSource{
		now:  func() time.Time { return now },
		skew: time.Minute,
		refresh: func(context.Context) (string, time.Time, error) {
			calls++
			return "token", now.Add(2 * time.Minute), nil
		},
	}
	var wait sync.WaitGroup
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := source.Authorization(context.Background()); err != nil {
				t.Errorf("authorization: %v", err)
			}
		}()
	}
	wait.Wait()
	if calls != 1 {
		t.Fatalf("refresh calls = %d", calls)
	}
	now = now.Add(61 * time.Second)
	if _, err := source.Authorization(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("refresh calls after skew = %d", calls)
	}
}

func TestExpiringBearerSourceCanceledWaiterReturns(t *testing.T) {
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	source := &expiringBearerSource{
		now: time.Now,
		refresh: func(context.Context) (string, time.Time, error) {
			close(refreshStarted)
			<-releaseRefresh
			return "token", time.Now().Add(time.Hour), nil
		},
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := source.Authorization(context.Background())
		firstDone <- err
	}()
	<-refreshStarted

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := source.Authorization(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("blocked waiter error = %v after %v", err, time.Since(started))
	}
	close(releaseRefresh)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}
