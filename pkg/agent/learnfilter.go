package agent

import (
	"context"
	"sync"
)

// -----------------------------------------------------------------------------
// Learn-exclusion seam (X30 "Disable-Learn").
//
// A process-wide chokepoint that lets a consumer drop signals from the agent's
// learning pipeline BEFORE they reach the per-type brain (miner / catalog /
// baseline). It mirrors the SetCatalogStore / scheduler.SetOwnership /
// SetModeResolver last-wins seams: one optional slot, mutex-guarded, nil by
// default so OSS behaviour is unchanged.
//
// The worker consults learnExclusion() inside tickSource, immediately after the
// redact loop and before grouping. Because the exclusion runs upstream of the
// mode switch, it bites identically in training, shadow and detect: an excluded
// (service, signal) never folds into the model in ANY mode.
//
// OSS ships no implementation; the enterprise module installs one (a per-org
// "don't learn this service/signal" policy) via SetLearnExclusion. The OSS tree
// never imports the enterprise package — the seam keeps the dependency
// one-way.
// -----------------------------------------------------------------------------

// LearnExclusion decides whether one signal should be kept out of the learning
// pipeline. ExcludeFromLearning returns true to DROP the signal (it is neither
// learned nor surfaced), false to keep it on the normal path. service is the
// discovered service name ("_unknown" when none could be resolved); signal is
// the logical signal name carried in Signal.Fields[core.FieldSignal] (empty for
// plain logs). Implementations must be safe for concurrent use — ticks run one
// goroutine per source.
type LearnExclusion interface {
	ExcludeFromLearning(ctx context.Context, service, signal string) bool
}

// Process-wide single slot. A consumer registers a policy at boot; the worker
// reads it once per tick. Mutex-guarded so a boot-time registration is safely
// visible to the worker goroutines.
var (
	learnExclusionMu   sync.Mutex
	learnExclusionSlot LearnExclusion
)

// SetLearnExclusion installs the process-wide learn-exclusion policy.
// Last-wins: a second call replaces the first. Passing nil clears the slot
// (back to the inert "learn everything" path). Call at boot, before the worker
// starts. OSS ships none, so the worker runs its current pipeline unchanged.
func SetLearnExclusion(x LearnExclusion) {
	learnExclusionMu.Lock()
	defer learnExclusionMu.Unlock()
	learnExclusionSlot = x
}

// learnExclusion returns the installed policy, or nil when none is set
// (community / OSS — every signal is eligible to learn).
func learnExclusion() LearnExclusion {
	learnExclusionMu.Lock()
	defer learnExclusionMu.Unlock()
	return learnExclusionSlot
}
