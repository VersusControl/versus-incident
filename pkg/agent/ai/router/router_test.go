package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// fakeAgent is a deterministic AIAgent used by the router tests.
type fakeAgent struct {
	kind     core.AITaskKind
	calls    int
	response *core.AICallResult
	err      error
}

func (f *fakeAgent) Name() string          { return "fake" }
func (f *fakeAgent) Kind() core.AITaskKind { return f.kind }
func (f *fakeAgent) Run(_ context.Context, _ core.AITask) (*core.AICallResult, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

func detectTask(patternID string) core.DetectTask {
	return core.DetectTask{Result: core.AgentResult{PatternID: patternID}}
}

func TestRouter_HappyPath_CallsAgent(t *testing.T) {
	finding := &core.AIFinding{Title: "t", Summary: "s"}
	agent := &fakeAgent{kind: core.AITaskDetect, response: &core.AICallResult{Finding: finding, Model: "m"}}
	cache := ai.NewResultCache(time.Hour, nil)
	rate := ai.NewRateLimiter(10)

	r := New(map[core.AITaskKind]Entry{
		core.AITaskDetect: {Agent: agent, Cache: cache, Rate: rate},
	})

	got, err := r.Run(context.Background(), detectTask("p1"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Finding == nil || got.Finding.Title != "t" {
		t.Fatalf("unexpected result: %#v", got)
	}
	if agent.calls != 1 {
		t.Fatalf("agent.calls = %d, want 1", agent.calls)
	}
	// Cache should now contain the finding.
	if _, hit := cache.Get("p1"); !hit {
		t.Fatalf("expected cache populated after run")
	}
}

func TestRouter_CacheHit_ShortCircuits(t *testing.T) {
	agent := &fakeAgent{kind: core.AITaskDetect, response: &core.AICallResult{Finding: &core.AIFinding{Title: "fresh"}}}
	cache := ai.NewResultCache(time.Hour, nil)
	cache.Put("p1", &core.AIFinding{Title: "cached"})
	rate := ai.NewRateLimiter(10)

	r := New(map[core.AITaskKind]Entry{
		core.AITaskDetect: {Agent: agent, Cache: cache, Rate: rate},
	})

	got, err := r.Run(context.Background(), detectTask("p1"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Finding == nil || got.Finding.Title != "cached" {
		t.Fatalf("expected cached finding, got %#v", got)
	}
	if agent.calls != 0 {
		t.Fatalf("agent.calls = %d, want 0 (cache hit)", agent.calls)
	}
}

func TestRouter_RateLimited(t *testing.T) {
	agent := &fakeAgent{kind: core.AITaskDetect, response: &core.AICallResult{Finding: &core.AIFinding{Title: "t"}}}
	cache := ai.NewResultCache(time.Hour, nil)
	rate := ai.NewRateLimiter(1)
	// Burn the single allowed call so the next request trips the limiter.
	if !rate.Allow() {
		t.Fatalf("limiter rejected first call")
	}

	r := New(map[core.AITaskKind]Entry{
		core.AITaskDetect: {Agent: agent, Cache: cache, Rate: rate},
	})

	_, err := r.Run(context.Background(), detectTask("p1"))
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if agent.calls != 0 {
		t.Fatalf("agent.calls = %d, want 0 when rate-limited", agent.calls)
	}
}

func TestRouter_NoAgent_ForKind(t *testing.T) {
	r := New(map[core.AITaskKind]Entry{})
	_, err := r.Run(context.Background(), detectTask("p1"))
	if !errors.Is(err, ErrNoAgent) {
		t.Fatalf("err = %v, want ErrNoAgent", err)
	}
}

func TestRouter_NilTask(t *testing.T) {
	agent := &fakeAgent{kind: core.AITaskDetect}
	r := New(map[core.AITaskKind]Entry{core.AITaskDetect: {Agent: agent}})
	if _, err := r.Run(context.Background(), nil); err == nil {
		t.Fatalf("expected error for nil task")
	}
}

func TestRouter_EmptyCacheKey_SkipsCache(t *testing.T) {
	agent := &fakeAgent{kind: core.AITaskDetect, response: &core.AICallResult{Finding: &core.AIFinding{Title: "t"}}}
	cache := ai.NewResultCache(time.Hour, nil)
	r := New(map[core.AITaskKind]Entry{
		core.AITaskDetect: {Agent: agent, Cache: cache},
	})

	// Empty pattern id ⇒ empty cache key ⇒ no cache lookup and no
	// cache write, but the agent still runs.
	if _, err := r.Run(context.Background(), detectTask("")); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if agent.calls != 1 {
		t.Fatalf("agent.calls = %d, want 1", agent.calls)
	}
	if cache.Len() != 0 {
		t.Fatalf("cache populated despite empty key: len=%d", cache.Len())
	}
}

func TestRouter_AgentError_NoCacheWrite(t *testing.T) {
	agent := &fakeAgent{kind: core.AITaskDetect, err: errors.New("boom")}
	cache := ai.NewResultCache(time.Hour, nil)
	r := New(map[core.AITaskKind]Entry{
		core.AITaskDetect: {Agent: agent, Cache: cache, Rate: ai.NewRateLimiter(10)},
	})

	if _, err := r.Run(context.Background(), detectTask("p1")); err == nil {
		t.Fatalf("expected error")
	}
	if _, hit := cache.Get("p1"); hit {
		t.Fatalf("cache should not be populated on agent error")
	}
}

func TestRouter_DropsNilAgentsAtConstruction(t *testing.T) {
	r := New(map[core.AITaskKind]Entry{
		core.AITaskDetect:  {Agent: nil},
		core.AITaskAnalyze: {Agent: &fakeAgent{kind: core.AITaskAnalyze}},
	})
	if r.Has(core.AITaskDetect) {
		t.Fatalf("nil-agent kind should not be registered")
	}
	if !r.Has(core.AITaskAnalyze) {
		t.Fatalf("analyze kind missing")
	}
}
