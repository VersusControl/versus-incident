package ai

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestResultCache_GetPutTTL(t *testing.T) {
	c := NewResultCache(time.Hour, storage.NewMemory())
	if _, ok := c.Get("p1"); ok {
		t.Fatal("cold cache should miss")
	}
	c.Put("p1", &core.AIFinding{Title: "x", Severity: "low", Confidence: 0.5})
	got, ok := c.Get("p1")
	if !ok {
		t.Fatal("hot cache should hit")
	}
	if got.Title != "x" {
		t.Fatalf("bad title: %q", got.Title)
	}
}

func TestResultCache_Expired(t *testing.T) {
	c := NewResultCache(10*time.Millisecond, nil)
	c.Put("p1", &core.AIFinding{Title: "x"})
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("p1"); ok {
		t.Fatal("expired entry should miss")
	}
}

func TestResultCache_DisabledTTL(t *testing.T) {
	c := NewResultCache(0, nil)
	c.Put("p1", &core.AIFinding{Title: "x"})
	if _, ok := c.Get("p1"); ok {
		t.Fatal("ttl<=0 should disable cache")
	}
}

func TestResultCache_PersistRoundTrip(t *testing.T) {
	store := storage.NewMemory()
	c1 := NewResultCache(time.Hour, store)
	c1.Put("p1", &core.AIFinding{Title: "title-1", Severity: "high", Confidence: 0.9})
	if err := c1.Persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	c2 := NewResultCache(time.Hour, store)
	got, ok := c2.Get("p1")
	if !ok {
		t.Fatal("persisted entry not reloaded")
	}
	if got.Title != "title-1" || got.Severity != "high" {
		t.Fatalf("bad reload: %+v", got)
	}
}

func TestRateLimiter_AllowAndCap(t *testing.T) {
	r := NewRateLimiter(2)
	if !r.Allow() {
		t.Fatal("first call should be allowed")
	}
	if !r.Allow() {
		t.Fatal("second call should be allowed")
	}
	if r.Allow() {
		t.Fatal("third call should be blocked")
	}
}

func TestRateLimiter_AllowKeyEnforcesFairnessAndAggregateCap(t *testing.T) {
	r := NewRateLimiter(4)
	for index := 0; index < 2; index++ {
		if !r.AllowKey("org-a/session-a") {
			t.Fatalf("session A call %d was unexpectedly limited", index)
		}
	}
	if r.AllowKey("org-a/session-a") {
		t.Fatal("one session consumed more than its fair share")
	}
	for index := 0; index < 2; index++ {
		if !r.AllowKey("org-a/session-b") {
			t.Fatalf("session B call %d was unexpectedly limited", index)
		}
	}
	if r.AllowKey("org-a/session-c") {
		t.Fatal("many sessions exceeded the aggregate cap")
	}
	_, used, max := r.Stats()
	if used != 4 || max != 4 {
		t.Fatalf("stats used=%d max=%d", used, max)
	}
}

func TestRateLimiter_Disabled(t *testing.T) {
	r := NewRateLimiter(0)
	for i := 0; i < 5; i++ {
		if !r.Allow() {
			t.Fatalf("max<=0 should allow unconditionally, blocked at i=%d", i)
		}
	}
}

func TestRateLimiter_NilSafe(t *testing.T) {
	var r *RateLimiter
	if !r.Allow() {
		t.Fatal("nil limiter should allow")
	}
}

func TestDistributedRateLimiterSharesAggregateAndFairnessAcrossInstances(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	first := NewDistributedRateLimiter(4, provider, "acme", func() time.Time { return now })
	second := NewDistributedRateLimiter(4, provider, "acme", func() time.Time { return now })
	assertDistributedLimit(t, first, second)
}

func TestDistributedRateLimiterConcurrentCASHasNoLostIncrements(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	first := NewDistributedRateLimiter(10, provider, "concurrent", func() time.Time { return now })
	second := NewDistributedRateLimiter(10, provider, "concurrent", func() time.Time { return now })
	var allowed atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < 40; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			limiter := first
			if index%2 == 1 {
				limiter = second
			}
			if limiter.AllowKey(fmt.Sprintf("session-%d", index)) {
				allowed.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if got := allowed.Load(); got != 10 {
		t.Fatalf("allowed calls = %d, want exactly 10", got)
	}
}

type flakyRateProvider struct {
	storage.Provider
	failList bool
}

func (provider *flakyRateProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

func (provider *flakyRateProvider) ListBlobs(prefix string) ([]storage.Blob, error) {
	if provider.failList {
		provider.failList = false
		return nil, fmt.Errorf("injected cleanup failure")
	}
	return provider.Provider.ListBlobs(prefix)
}

func TestDistributedRateLimiterRetriesPostAdmissionCleanup(t *testing.T) {
	provider := &flakyRateProvider{Provider: storage.NewMemory()}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	limiter := NewDistributedRateLimiter(10, provider, "cleanup", func() time.Time { return now })
	if !limiter.AllowKey("session-a") {
		t.Fatal("initial admission failed")
	}
	now = now.Add(time.Hour)
	provider.failList = true
	if !limiter.AllowKey("session-b") {
		t.Fatal("cleanup failure changed committed admission")
	}
	prefix := "ai-rate/" + limiter.scope + "/"
	blobs, err := provider.Provider.ListBlobs(prefix)
	if err != nil || len(blobs) != 2 {
		t.Fatalf("buckets after failed cleanup=%d err=%v, want 2", len(blobs), err)
	}
	if !limiter.AllowKey("session-c") {
		t.Fatal("second current-hour admission failed")
	}
	blobs, err = provider.Provider.ListBlobs(prefix)
	if err != nil || len(blobs) != 1 || !strings.HasSuffix(blobs[0].Name, bucketKey(now)) {
		t.Fatalf("buckets after cleanup retry=%+v err=%v", blobs, err)
	}
}

func TestDistributedRateLimiterPostgresProviders(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	firstProvider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstProvider.Close() })
	secondProvider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondProvider.Close() })
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	scope := fmt.Sprintf("rate-test-%d", time.Now().UnixNano())
	first := NewDistributedRateLimiter(4, firstProvider, scope, func() time.Time { return now })
	second := NewDistributedRateLimiter(4, secondProvider, scope, func() time.Time { return now })
	assertDistributedLimit(t, first, second)
}

func assertDistributedLimit(t *testing.T, first, second *RateLimiter) {
	t.Helper()
	if !first.AllowKey("session-a") || !second.AllowKey("session-a") {
		t.Fatal("first two calls for one session should be allowed")
	}
	if second.AllowKey("session-a") {
		t.Fatal("one session exceeded its fleet-wide fair share")
	}
	if !second.AllowKey("session-b") || !first.AllowKey("session-b") {
		t.Fatal("second session should consume the remaining aggregate share")
	}
	if first.AllowKey("session-c") {
		t.Fatal("instances exceeded the fleet-wide aggregate cap")
	}
}
