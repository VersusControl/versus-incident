package agent

import (
	"context"
	"sync"
)

// -----------------------------------------------------------------------------
// Learn-exclusion seam ("Disable-Learn").
//
// A process-wide chokepoint that lets a consumer drop signals from the agent's
// learning pipeline BEFORE they reach the per-type brain (miner / catalog /
// baseline). It mirrors the SetCatalogStore / scheduler.SetOwnership /
// SetModeResolver last-wins seams: one optional slot, mutex-guarded, nil by
// default so OSS behaviour is unchanged.
//
// The worker consults learnExclusion() at two points in tickSource: the
// service / metric-trace-signal grain runs immediately after the redact loop
// and before grouping (ExcludeFromLearning), while the per-log-pattern grain
// runs right after the log brain's Group, before Learn (ExcludeLogPattern) —
// because a plain log carries no pattern identity until it has been grouped.
// Because both hooks run upstream of the mode switch, they bite identically in
// training, shadow and detect: an excluded (service, signal) or (service,
// pattern) never folds into the model in ANY mode.
//
// OSS ships no implementation; the enterprise module installs one (a per-org
// "don't learn this service/signal" policy) via SetLearnExclusion. The OSS tree
// never imports the enterprise package — the seam keeps the dependency
// one-way.
// -----------------------------------------------------------------------------

// LearnExclusion decides whether a signal should be kept out of the learning
// pipeline. It answers at two grains, consulted at two points in the tick:
//
//   - ExcludeFromLearning is the PRE-Group chokepoint (whole-service and
//     metric/trace per-signal exclusion). It returns true to DROP the signal
//     (neither learned nor surfaced), false to keep it. service is the
//     discovered service name ("_unknown" when none could be resolved); signal
//     is the logical signal name carried in Signal.Fields[core.FieldSignal]
//     (empty for plain logs — which is exactly why a single LOG PATTERN cannot
//     be singled out here, see ExcludeLogPattern).
//
//   - ExcludeLogPattern is the POST-Group per-LOG-PATTERN grain. A plain
//     log line carries no signal identity before grouping, so the pre-Group
//     hook can only exclude a log pattern's whole SERVICE. Once the log brain's
//     Group has assigned each observation its stable pattern Key/id, the worker
//     consults this method once per observation to drop just the excluded
//     PATTERNS before Learn — the log analogue of the metric/trace per-signal
//     exclusion. service is the observation's attributed service; patternKey is
//     the pattern's stable Key/id (the miner cluster id — the same identity
//     shown in the patterns list and used by relabel/reassign). It is consulted
//     for LOG brains only; metric/trace signal exclusion stays at the pre-Group
//     hook. A nil resolver (community/OSS) is never installed, so neither method
//     is ever called there.
//
// Implementations must be safe for concurrent use — ticks run one goroutine per
// source.
type LearnExclusion interface {
	ExcludeFromLearning(ctx context.Context, service, signal string) bool
	ExcludeLogPattern(ctx context.Context, service, patternKey string) bool
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
