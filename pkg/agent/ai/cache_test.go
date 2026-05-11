package ai

import (
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
