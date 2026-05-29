// Package router dispatches AITasks to the right AIAgent and wraps
// each call with per-kind caching and per-kind rate limiting.
//
// The router is intentionally thin: it owns no model code itself. It
// holds one AIAgent per AITaskKind, an optional *ai.ResultCache per
// kind, and an optional *ai.RateLimiter per kind. Callers (the agent
// worker for detect, the admin controller for analyze) pass the
// constructed task in; the router does cache lookup → rate check →
// agent.Run → cache write.
package router

import (
	"context"
	"errors"
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// ErrRateLimited is returned by Run when the per-kind rate limiter
// rejected the call. Callers should treat this as a soft skip
// (record an "emit_quota" outcome, do not retry within the bucket).
var ErrRateLimited = errors.New("router: rate limited")

// ErrNoAgent is returned by Run when no AIAgent is registered for the
// task's kind. The router never falls back across kinds.
var ErrNoAgent = errors.New("router: no agent registered for kind")

// Entry is one wired-up (agent, cache, rate) tuple for a single kind.
// Cache and Rate may be nil — the router treats nil as "no cache" /
// "no rate cap" respectively.
type Entry struct {
	Agent core.AIAgent
	Cache *ai.ResultCache
	Rate  *ai.RateLimiter
}

// Router fans an AITask out to the entry for its kind.
type Router struct {
	entries map[core.AITaskKind]Entry
}

// New builds a Router from the given per-kind entries. nil agents are
// dropped so callers can pass `Entry{}` for kinds they have not wired
// up yet (e.g. analyze remains unset until E4).
func New(entries map[core.AITaskKind]Entry) *Router {
	out := make(map[core.AITaskKind]Entry, len(entries))
	for k, e := range entries {
		if e.Agent == nil {
			continue
		}
		out[k] = e
	}
	return &Router{entries: out}
}

// Has reports whether an agent is registered for the given kind.
func (r *Router) Has(kind core.AITaskKind) bool {
	if r == nil {
		return false
	}
	_, ok := r.entries[kind]
	return ok
}

// Run executes the task on the kind-specific agent, wrapped with
// cache + rate. Order of operations:
//
//  1. Cache lookup on task.CacheKey() — hit short-circuits and returns
//     a *AICallResult with the cached finding (UserPrompt/RawResponse
//     left empty because the model was not called).
//  2. Rate check — false returns ErrRateLimited without calling the
//     agent.
//  3. agent.Run — failure is propagated.
//  4. Cache write on success.
//
// Run never panics on a zero receiver; it returns ErrNoAgent instead.
func (r *Router) Run(ctx context.Context, task core.AITask) (*core.AICallResult, error) {
	if r == nil {
		return nil, ErrNoAgent
	}
	if task == nil {
		return nil, fmt.Errorf("router: nil task")
	}
	entry, ok := r.entries[task.Kind()]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoAgent, task.Kind())
	}

	key := task.CacheKey()
	if entry.Cache != nil && key != "" {
		if cached, hit := entry.Cache.Get(key); hit {
			return &core.AICallResult{Finding: cached}, nil
		}
	}

	if entry.Rate != nil && !entry.Rate.Allow() {
		return nil, ErrRateLimited
	}

	result, err := entry.Agent.Run(ctx, task)
	if err != nil {
		return nil, err
	}

	if entry.Cache != nil && key != "" && result != nil && result.Finding != nil {
		entry.Cache.Put(key, result.Finding)
	}
	return result, nil
}
