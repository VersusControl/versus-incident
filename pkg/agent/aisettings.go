package agent

import (
	"context"
	"sync"
)

// aisettings.go — the single-slot runtime AI-settings resolver seam.
//
// It lets a consumer (the enterprise hooks.Register) override the agent's
// effective AI settings at runtime, so the worker and the chat-model
// transport can re-resolve the effective key / enable flag on every call
// instead of being pinned to the static YAML ai config for the life of the
// process.
//
// It mirrors the mode.go ModeResolver / SetModeResolver seam: one
// process-wide slot, registered once at boot, mutex-guarded. OSS registers
// nothing, so aiSettingsResolver() returns nil and every call site falls
// back to the static YAML ai config — community behaviour is byte-for-byte
// unchanged (one nil-check, no allocations, no goroutines, the outbound
// transport is left as a plain pass-through).

// AISettingsResolver resolves effective AI settings at runtime. ok=false on
// a method means "no opinion" -> caller uses the static YAML ai config.
type AISettingsResolver interface {
	EffectiveKey(ctx context.Context) (key string, ok bool)
	EffectiveEnabled(ctx context.Context) (enabled bool, ok bool)
}

// Process-wide single slot. A consumer registers a resolver at boot; the
// worker reads it once per tick and the chat-model transport reads it once
// per request. Mutex-guarded so a boot-time registration is safely visible
// to the worker goroutine and the HTTP transport.
var (
	aiSettingsMu           sync.Mutex
	aiSettingsResolverSlot AISettingsResolver
)

// SetAISettingsResolver registers the resolver used to override the
// effective AI settings at runtime. Last-wins: a second call replaces the
// first. Passing nil clears the slot (back to the YAML floor). OSS ships
// none, so the worker and transport use the static ai config unchanged.
// This is the entry point the enterprise hooks.Register attaches to (mirror
// of SetModeResolver). Call at boot, before the worker starts.
func SetAISettingsResolver(r AISettingsResolver) {
	aiSettingsMu.Lock()
	defer aiSettingsMu.Unlock()
	aiSettingsResolverSlot = r
}

// aiSettingsResolver returns the registered resolver, or nil when none is
// set (community mode).
func aiSettingsResolver() AISettingsResolver {
	aiSettingsMu.Lock()
	defer aiSettingsMu.Unlock()
	return aiSettingsResolverSlot
}

// aiSettingsKeyFunc returns a per-request key override function backed by
// the registered AISettingsResolver, or nil when none is set. The chat
// model wraps its outbound transport with this function only when it is
// non-nil, so OSS (no resolver) keeps a plain pass-through transport and is
// byte-for-byte unchanged. The returned function re-reads the live slot on
// every call, so a hot-swapped key takes effect without a restart.
func aiSettingsKeyFunc() func(context.Context) (string, bool) {
	if aiSettingsResolver() == nil {
		return nil
	}
	return func(ctx context.Context) (string, bool) {
		if r := aiSettingsResolver(); r != nil {
			return r.EffectiveKey(ctx)
		}
		return "", false
	}
}
