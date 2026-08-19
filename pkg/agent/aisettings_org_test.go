package agent

import "testing"

// stubOrgAISettings is a fake resolver that ALSO implements the org-scoped
// getter, standing in for the enterprise runtime AI-settings resolver.
type stubOrgAISettings struct {
	stubAISettings
	org      string
	enabled  bool
	provider string
	ok       bool
	calls    int
}

func (s *stubOrgAISettings) EffectiveAISettingsForOrg(org string) (bool, string, bool) {
	s.calls++
	s.org = org
	return s.enabled, s.provider, s.ok
}

// TestEffectiveAISettingsForOrg_NoResolver proves the community path: with no
// resolver registered the static YAML values come back untouched.
func TestEffectiveAISettingsForOrg_NoResolver(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	enabled, provider := EffectiveAISettingsForOrg("acme", false, "openai")
	if enabled {
		t.Errorf("enabled = true, want the static false")
	}
	if provider != "openai" {
		t.Errorf("provider = %q, want the static %q", provider, "openai")
	}

	enabled, provider = EffectiveAISettingsForOrg("", true, "")
	if !enabled || provider != "" {
		t.Errorf("got (%v, %q), want the static (true, \"\")", enabled, provider)
	}
}

// TestEffectiveAISettingsForOrg_KeyEnabledOnlyResolver proves a resolver that
// does NOT implement the org-scoped getter has no opinion, so the static
// values still win.
func TestEffectiveAISettingsForOrg_KeyEnabledOnlyResolver(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	SetAISettingsResolver(&stubAISettings{enabled: true, enabledOK: true})

	enabled, provider := EffectiveAISettingsForOrg("acme", false, "openai")
	if enabled {
		t.Errorf("enabled = true, want the static false from a getter-less resolver")
	}
	if provider != "openai" {
		t.Errorf("provider = %q, want the static %q", provider, "openai")
	}
}

// TestEffectiveAISettingsForOrg_OverrideWins proves the runtime override is
// reported when the resolver answers, and that the request's org is passed
// through to it.
func TestEffectiveAISettingsForOrg_OverrideWins(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	stub := &stubOrgAISettings{enabled: true, provider: "ollama", ok: true}
	SetAISettingsResolver(stub)

	enabled, provider := EffectiveAISettingsForOrg("acme", false, "openai")
	if !enabled {
		t.Errorf("enabled = false, want true from the runtime override")
	}
	if provider != "ollama" {
		t.Errorf("provider = %q, want %q from the runtime override", provider, "ollama")
	}
	if stub.org != "acme" {
		t.Errorf("resolver saw org %q, want %q", stub.org, "acme")
	}
}

// TestEffectiveAISettingsForOrg_OverrideCanDisable proves the override is
// authoritative in both directions, not an OR with the YAML floor.
func TestEffectiveAISettingsForOrg_OverrideCanDisable(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	SetAISettingsResolver(&stubOrgAISettings{enabled: false, ok: true})

	if enabled, _ := EffectiveAISettingsForOrg("acme", true, "openai"); enabled {
		t.Errorf("enabled = true, want false from the runtime override")
	}
}

// TestEffectiveAISettingsForOrg_FailSoft proves an unavailable override
// (ok=false, the shape a store error takes) degrades to the static values, and
// that a blank or unsupported provider never displaces the configured one.
func TestEffectiveAISettingsForOrg_FailSoft(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	SetAISettingsResolver(&stubOrgAISettings{enabled: true, provider: "ollama", ok: false})
	enabled, provider := EffectiveAISettingsForOrg("acme", false, "openai")
	if enabled {
		t.Errorf("enabled = true, want the static false when the override is unavailable")
	}
	if provider != "openai" {
		t.Errorf("provider = %q, want the static %q", provider, "openai")
	}

	SetAISettingsResolver(&stubOrgAISettings{enabled: true, provider: "  ", ok: true})
	if _, provider = EffectiveAISettingsForOrg("acme", false, "openai"); provider != "openai" {
		t.Errorf("provider = %q, want the static %q for a blank override", provider, "openai")
	}

	SetAISettingsResolver(&stubOrgAISettings{enabled: true, provider: "not-a-provider", ok: true})
	if _, provider = EffectiveAISettingsForOrg("acme", false, "openai"); provider != "openai" {
		t.Errorf("provider = %q, want the static %q for an unsupported override", provider, "openai")
	}
}
