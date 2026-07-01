package agent

import (
	"context"
	"sync"
)

// -----------------------------------------------------------------------------
// Manual-attribution override seam (Service-Override).
//
// A process-wide chokepoint that lets an operator's manual service correction
// WIN over regex `service_patterns` auto-detection (and over "_unknown"). It
// mirrors the SetLearnExclusion / SetCatalogStore / scheduler.SetOwnership
// last-wins seams: one optional slot, mutex-guarded, nil by default.
//
// The single generic seam serves ALL THREE source types. The OSS log brain
// funnels its post-Extract attribution through ResolveServiceOverride so a
// stored override re-labels future log signals; the enterprise metric/trace
// brains call the SAME chokepoint for their (service, signal) attribution — no
// duplicated logic, and the OSS tree never imports the enterprise module.
//
// OSS ships a working resolver (ServiceOverrideStore, installed at boot) so
// logs override works out of the box in a single-tenant OSS build. When no
// resolver is installed (nil slot) ResolveServiceOverride returns the detected
// service verbatim — one nil-check, no allocation — so a build that wires
// nothing is byte-for-byte unchanged.
// -----------------------------------------------------------------------------

// ServiceOverrideInput carries everything a resolver needs to decide the manual
// attribution for one signal across all three source types. The match key is
// source-appropriate: logs match on the mined Pattern identity or a Message
// substring; metrics/traces match on the Signal (series) name (exact or glob).
type ServiceOverrideInput struct {
	// SourceType is one of OverrideSourceLog / OverrideSourceMetric /
	// OverrideSourceTrace. A rule only matches an input of the SAME type.
	SourceType string
	// Service is the auto-detected attribution BEFORE the override runs —
	// the regex ServiceMatcher.Extract result, "_unknown" when none matched.
	// It is returned unchanged when no override applies.
	Service string
	// Signal is the logical signal / series name (a metric golden-signal or a
	// trace operation label). Empty for plain logs.
	Signal string
	// Pattern is the mined pattern / template identity the log signal folded
	// into (the durable log match key). Empty for metrics/traces.
	Pattern string
	// Message is the raw (redacted) log message, used for substring matching
	// when a log rule keys on a matched substring rather than a pattern id.
	// Empty for metrics/traces.
	Message string
}

// ServiceOverride is the OPTIONAL per-org manual-attribution resolver. It
// returns the operator-chosen service for a signal, or ("", false) when no
// override applies (keep the auto-detected service). Implementations must be
// safe for concurrent use — ticks run one goroutine per source.
type ServiceOverride interface {
	ResolveService(ctx context.Context, in ServiceOverrideInput) (service string, ok bool)
}

// Process-wide single slot. A consumer registers a resolver at boot; the
// brains read it once per attribution. Mutex-guarded so a boot-time
// registration is safely visible to the worker goroutines.
var (
	serviceOverrideMu   sync.Mutex
	serviceOverrideSlot ServiceOverride
)

// SetServiceOverride installs the process-wide manual-attribution resolver.
// Last-wins: a second call replaces the first. Passing nil clears the slot
// (back to "keep the detected service"). Call at boot, before the worker
// starts. OSS installs its ServiceOverrideStore here; an unwired build leaves
// it nil and attribution is byte-for-byte unchanged.
func SetServiceOverride(o ServiceOverride) {
	serviceOverrideMu.Lock()
	defer serviceOverrideMu.Unlock()
	serviceOverrideSlot = o
}

// serviceOverride returns the installed resolver, or nil when none is set.
func serviceOverride() ServiceOverride {
	serviceOverrideMu.Lock()
	defer serviceOverrideMu.Unlock()
	return serviceOverrideSlot
}

// ResolveServiceOverride applies the installed manual-attribution override to a
// detected attribution, returning the operator-chosen service when a rule
// matches, or in.Service unchanged otherwise. It is the single chokepoint the
// OSS log brain and the enterprise metric/trace brains funnel through, so a
// manual override wins over regex detection identically for all three source
// types. A nil resolver (no override wired) returns in.Service — one nil-check,
// no allocation. A resolver that returns ("", true) or a blank service is
// ignored (never blanks a real attribution).
func ResolveServiceOverride(ctx context.Context, in ServiceOverrideInput) string {
	if r := serviceOverride(); r != nil {
		if svc, ok := r.ResolveService(ctx, in); ok && svc != "" {
			return svc
		}
	}
	return in.Service
}
