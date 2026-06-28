package agent

import (
	"context"
	"sync"
)

// mode.go — the single-slot lifecycle-mode resolver seam.
//
// It lets a consumer (the enterprise hooks.Register) override the agent's
// effective lifecycle mode (training | shadow | detect) at runtime, so the
// worker can re-resolve the mode on every poll cycle instead of being
// pinned to the static YAML cfg.Mode for the life of the process.
//
// It mirrors the middleware.SetOrgResolver / SetAuthMiddleware last-wins
// seam: one process-wide slot, registered once at boot, mutex-guarded.
// OSS registers nothing, so modeResolver() returns nil and the worker
// falls back to cfg.Mode — community behaviour is byte-for-byte unchanged
// (one nil-check per tick, no allocations, no goroutines).

// ModeResolver resolves the effective lifecycle mode at runtime. ok=false
// means "no opinion" -> the worker uses the static YAML cfg.Mode.
type ModeResolver interface {
	Mode(ctx context.Context, agent string) (mode string, ok bool)
}

// Process-wide single slot. A consumer registers a resolver at boot; the
// worker reads it once per tick. Mutex-guarded so a boot-time registration
// is safely visible to the worker goroutine.
var (
	modeMu           sync.Mutex
	modeResolverSlot ModeResolver
)

// SetModeResolver registers the resolver used to override the effective
// lifecycle mode at runtime. Last-wins: a second call replaces the first.
// Passing nil clears the slot (back to the YAML floor). OSS ships none, so
// the worker uses cfg.Mode unchanged. This is the entry point the
// enterprise hooks.Register attaches to (mirror of
// middleware.SetOrgResolver). Call at boot, before the worker starts.
func SetModeResolver(r ModeResolver) {
	modeMu.Lock()
	defer modeMu.Unlock()
	modeResolverSlot = r
}

// modeResolver returns the registered resolver, or nil when none is set
// (community mode).
func modeResolver() ModeResolver {
	modeMu.Lock()
	defer modeMu.Unlock()
	return modeResolverSlot
}

// isValidMode reports whether m is one of the three known lifecycle modes.
// An unknown mode from a resolver is treated as "no opinion" so the worker
// fails closed to the YAML floor.
func isValidMode(m string) bool {
	switch m {
	case "training", "shadow", "detect":
		return true
	default:
		return false
	}
}

// orgModeGetter is the optional capability a registered ModeResolver may also
// implement to answer a synchronous, org-scoped lookup of the current mode
// override — the shape read-only HTTP surfaces need (no worker tick context).
// The enterprise *runtimemode.Resolver satisfies it; community OSS registers
// no resolver at all, so EffectiveModeForOrg returns the YAML floor unchanged.
type orgModeGetter interface {
	Get(org string) (mode string, ok bool)
}

// EffectiveModeForOrg returns the runtime-effective lifecycle mode for org,
// for read-only display surfaces such as the admin config endpoint and the
// dashboard top bar. It consults the registered ModeResolver's optional
// org-scoped getter and falls back to yamlFloor when there is no resolver, no
// override, or an invalid stored value. Community OSS registers no resolver,
// so it returns yamlFloor byte-for-byte unchanged.
func EffectiveModeForOrg(org, yamlFloor string) string {
	if r := modeResolver(); r != nil {
		if g, ok := r.(orgModeGetter); ok {
			if m, ok := g.Get(org); ok && isValidMode(m) {
				return m
			}
		}
	}
	return yamlFloor
}
