package agent

import (
	"context"
	"strings"
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

// orgAISettingsGetter is the optional capability a registered
// AISettingsResolver may also implement to answer a synchronous, org-scoped
// lookup of the current AI override — the shape read-only HTTP surfaces need
// (they have no worker ctx carrying the org). ok=false means "no opinion".
// Community OSS registers no resolver at all, so EffectiveAISettingsForOrg
// returns the YAML floor unchanged.
type orgAISettingsGetter interface {
	EffectiveAISettingsForOrg(org string) (enabled bool, provider string, ok bool)
}

// EffectiveAISettingsForOrg returns the runtime-effective AI enable flag and
// model provider for org, for read-only display surfaces such as the admin
// config endpoint. It consults the registered AISettingsResolver's optional
// org-scoped getter and falls back to the YAML floor when there is no
// resolver, no override, or an unsupported provider value — so a resolver that
// cannot answer degrades to the configured values instead of reporting a false
// "on". Community OSS registers no resolver, so it returns the floor
// byte-for-byte unchanged.
func EffectiveAISettingsForOrg(org string, yamlEnabled bool, yamlProvider string) (enabled bool, provider string) {
	enabled, provider = yamlEnabled, yamlProvider
	r := aiSettingsResolver()
	if r == nil {
		return enabled, provider
	}
	g, ok := r.(orgAISettingsGetter)
	if !ok {
		return enabled, provider
	}
	en, p, ok := g.EffectiveAISettingsForOrg(org)
	if !ok {
		return enabled, provider
	}
	enabled = en
	if p = strings.TrimSpace(p); p != "" && einowrap.IsSupportedProvider(p) {
		provider = p
	}
	return enabled, provider
}

// orgAIKeyGetter is the optional capability a registered AISettingsResolver may
// also implement to answer, for a read-only display surface, whether an AI
// credential is configured for org. It reports a BOOLEAN, never the key and
// never a masked prefix, so the seam that tells the UI "a key is set" cannot
// become a key-disclosure path. ok=false means "no opinion". Community OSS
// registers no resolver at all, so EffectiveAIKeySetForOrg reports the YAML
// floor unchanged.
type orgAIKeyGetter interface {
	EffectiveAIKeySetForOrg(org string) (keySet bool, ok bool)
}

// EffectiveAIKeySetForOrg reports whether an AI credential is configured for
// org at RUNTIME — the enterprise per-org override when it has an opinion, else
// the YAML floor. The admin config endpoint used to report the YAML floor only,
// so an org whose key lives solely in the runtime override rendered as "no key
// configured" while the worker was happily calling the model with it.
//
// It resolves through the same registered AISettingsResolver as the AI enable
// flag, so the two cannot disagree, and it returns only the set/unset fact.
func EffectiveAIKeySetForOrg(org, yamlKey string) bool {
	keySet := strings.TrimSpace(yamlKey) != ""
	r := aiSettingsResolver()
	if r == nil {
		return keySet
	}
	g, ok := r.(orgAIKeyGetter)
	if !ok {
		return keySet
	}
	if set, ok := g.EffectiveAIKeySetForOrg(org); ok {
		return set
	}
	return keySet
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
