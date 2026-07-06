package agent

import (
	"context"
	"sync"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
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

// AIProviderResolver is an OPTIONAL extension of AISettingsResolver: a
// registered resolver that ALSO implements it can override the model PROVIDER
// (openai | deepseek | qwen | ollama | claude | gemini) at runtime, so an
// operator's provider change rebuilds the agent's model on its next run
// without a process restart. ok=false ⇒ no opinion (use the configured
// provider). A resolver that does not implement this interface simply has no
// provider opinion — composition is additive, so the existing key/enabled
// resolver keeps working unchanged. OSS registers nothing.
type AIProviderResolver interface {
	EffectiveProvider(ctx context.Context) (provider string, ok bool)
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

// aiRuntime builds the einowrap.RuntimeAI that the detect/analyze model
// holders fold into their rebuild signature. When no resolver is registered
// (OSS) it returns a zero RuntimeAI — every override func is nil, so the
// holder pins the configured provider and builds the model exactly once,
// keeping community behaviour byte-for-byte unchanged. Each func re-reads the
// live slot, so a hot-swapped resolver takes effect without a restart.
//
// Provider is wired only when the registered resolver ALSO implements
// AIProviderResolver; otherwise it returns no opinion, so a key/enabled-only
// resolver (today's backend) never forces a provider rebuild.
func aiRuntime() einowrap.RuntimeAI {
	if aiSettingsResolver() == nil {
		return einowrap.RuntimeAI{}
	}
	return einowrap.RuntimeAI{
		Provider: func(ctx context.Context) (string, bool) {
			if pr, ok := aiSettingsResolver().(AIProviderResolver); ok && pr != nil {
				return pr.EffectiveProvider(ctx)
			}
			return "", false
		},
		Enabled: func(ctx context.Context) (bool, bool) {
			if r := aiSettingsResolver(); r != nil {
				return r.EffectiveEnabled(ctx)
			}
			return false, false
		},
		KeySet: func(ctx context.Context) (bool, bool) {
			if r := aiSettingsResolver(); r != nil {
				_, ok := r.EffectiveKey(ctx)
				return ok, true
			}
			return false, false
		},
	}
}
