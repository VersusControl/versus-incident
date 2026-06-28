package agent

import (
	"context"
	"testing"
)

// stubAISettings is a fake AISettingsResolver whose answers are fixed per
// method, so a test can prove the worker / transport read the live slot.
type stubAISettings struct {
	key        string
	keyOK      bool
	enabled    bool
	enabledOK  bool
	keyCalls   int
	enableCall int
}

func (s *stubAISettings) EffectiveKey(context.Context) (string, bool) {
	s.keyCalls++
	return s.key, s.keyOK
}

func (s *stubAISettings) EffectiveEnabled(context.Context) (bool, bool) {
	s.enableCall++
	return s.enabled, s.enabledOK
}

// TestAISettingsResolver_NilByDefault proves OSS registers nothing: the
// slot is nil and the key func is nil, so every call site falls back to the
// static YAML ai config — the byte-for-byte unchanged path.
func TestAISettingsResolver_NilByDefault(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	if r := aiSettingsResolver(); r != nil {
		t.Fatalf("aiSettingsResolver() = %v, want nil in OSS", r)
	}
	if fn := aiSettingsKeyFunc(); fn != nil {
		t.Fatalf("aiSettingsKeyFunc() = non-nil, want nil when no resolver registered")
	}
}

// TestAISettingsResolver_LastWins proves the single slot is last-wins and
// that nil clears it, mirroring SetModeResolver.
func TestAISettingsResolver_LastWins(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	first := &stubAISettings{key: "k1", keyOK: true}
	second := &stubAISettings{key: "k2", keyOK: true}

	SetAISettingsResolver(first)
	if r := aiSettingsResolver(); r != first {
		t.Fatalf("after first register: slot = %v, want first", r)
	}

	SetAISettingsResolver(second)
	if r := aiSettingsResolver(); r != second {
		t.Fatalf("after second register: slot = %v, want second (last-wins)", r)
	}

	SetAISettingsResolver(nil)
	if r := aiSettingsResolver(); r != nil {
		t.Fatalf("after nil register: slot = %v, want nil (cleared)", r)
	}
}

// TestAISettingsKeyFunc_ReadsLiveSlot proves the key func backs onto the
// live slot: it returns the resolver's key when ok, and "" / false when the
// resolver has no opinion. It also proves the func re-reads the slot on
// every call (restart-free hot swap).
func TestAISettingsKeyFunc_ReadsLiveSlot(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	SetAISettingsResolver(&stubAISettings{key: "org-key", keyOK: true})
	fn := aiSettingsKeyFunc()
	if fn == nil {
		t.Fatal("aiSettingsKeyFunc() = nil, want non-nil when a resolver is registered")
	}
	if key, ok := fn(context.Background()); !ok || key != "org-key" {
		t.Fatalf("fn() = (%q,%v), want (org-key,true)", key, ok)
	}

	// Hot-swap to a no-opinion resolver; the SAME func must observe it.
	SetAISettingsResolver(&stubAISettings{keyOK: false})
	if key, ok := fn(context.Background()); ok || key != "" {
		t.Fatalf("after swap fn() = (%q,%v), want (\"\",false)", key, ok)
	}
}

// stubProviderAISettings is a resolver that ALSO implements
// AIProviderResolver, so it can override the model provider at runtime.
type stubProviderAISettings struct {
	stubAISettings
	provider   string
	providerOK bool
}

func (s *stubProviderAISettings) EffectiveProvider(context.Context) (string, bool) {
	return s.provider, s.providerOK
}

// TestAIRuntime_NilResolver_Inert proves the OSS default: with no resolver
// registered aiRuntime() is a zero RuntimeAI (every override func nil), so the
// model holder pins the configured provider and never rebuilds.
func TestAIRuntime_NilResolver_Inert(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	rt := aiRuntime()
	if rt.Provider != nil || rt.Enabled != nil || rt.KeySet != nil {
		t.Fatalf("aiRuntime() with no resolver = %+v, want all-nil funcs", rt)
	}
}

// TestAIRuntime_ProviderResolver proves a resolver that implements
// AIProviderResolver feeds its provider opinion through aiRuntime().Provider,
// while a plain key/enabled-only resolver yields no provider opinion (so it
// never forces a provider rebuild).
func TestAIRuntime_ProviderResolver(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	// Plain resolver (no AIProviderResolver) ⇒ Provider has no opinion.
	SetAISettingsResolver(&stubAISettings{enabled: true, enabledOK: true, key: "k", keyOK: true})
	rt := aiRuntime()
	if rt.Provider == nil {
		t.Fatal("aiRuntime().Provider = nil with a resolver registered, want non-nil")
	}
	if p, ok := rt.Provider(context.Background()); ok {
		t.Fatalf("plain resolver Provider() = (%q,true), want no opinion", p)
	}
	// Enabled/KeySet fold the plain resolver's state.
	if en, ok := rt.Enabled(context.Background()); !ok || !en {
		t.Fatalf("Enabled() = (%v,%v), want (true,true)", en, ok)
	}
	if set, ok := rt.KeySet(context.Background()); !ok || !set {
		t.Fatalf("KeySet() = (%v,%v), want (true,true)", set, ok)
	}

	// Provider-capable resolver ⇒ Provider returns its opinion.
	SetAISettingsResolver(&stubProviderAISettings{provider: "ollama", providerOK: true})
	rt = aiRuntime()
	if p, ok := rt.Provider(context.Background()); !ok || p != "ollama" {
		t.Fatalf("provider resolver Provider() = (%q,%v), want (ollama,true)", p, ok)
	}
}
