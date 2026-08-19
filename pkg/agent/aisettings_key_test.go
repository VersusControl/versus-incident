package agent

import (
	"context"
	"testing"
)

// stubAIKeyResolver is a fake AISettingsResolver that also answers the
// org-scoped key-set question the read-only admin surface asks.
type stubAIKeyResolver struct {
	keySet bool
	ok     bool
	sawOrg string
}

func (s *stubAIKeyResolver) EffectiveKey(context.Context) (string, bool)   { return "", false }
func (s *stubAIKeyResolver) EffectiveEnabled(context.Context) (bool, bool) { return false, false }
func (s *stubAIKeyResolver) EffectiveAIKeySetForOrg(org string) (bool, bool) {
	s.sawOrg = org
	return s.keySet, s.ok
}

// TestEffectiveAIKeySetForOrg_NoResolver proves the community path: with
// nothing registered the YAML floor decides, and a whitespace-only key counts
// as unset.
func TestEffectiveAIKeySetForOrg_NoResolver(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	if !EffectiveAIKeySetForOrg("acme", "sk-yaml") {
		t.Error("a YAML key must report set with no resolver")
	}
	if EffectiveAIKeySetForOrg("acme", "") {
		t.Error("no YAML key must report unset with no resolver")
	}
	if EffectiveAIKeySetForOrg("acme", "   ") {
		t.Error("a whitespace-only YAML key must report unset")
	}
}

// TestEffectiveAIKeySetForOrg_OverrideWins is the reported bug: an org whose
// credential lives only in the runtime override must report configured.
func TestEffectiveAIKeySetForOrg_OverrideWins(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	stub := &stubAIKeyResolver{keySet: true, ok: true}
	SetAISettingsResolver(stub)

	if !EffectiveAIKeySetForOrg("acme", "") {
		t.Error("a runtime-only key must report set")
	}
	if stub.sawOrg != "acme" {
		t.Errorf("resolver saw org %q, want the request org %q", stub.sawOrg, "acme")
	}
}

// TestEffectiveAIKeySetForOrg_OverrideCanReportUnset proves the override is
// authoritative both ways, so a cleared credential stops being advertised.
func TestEffectiveAIKeySetForOrg_OverrideCanReportUnset(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	SetAISettingsResolver(&stubAIKeyResolver{keySet: false, ok: true})

	if EffectiveAIKeySetForOrg("acme", "sk-yaml") {
		t.Error("the runtime override reported no key; the YAML floor must not override it")
	}
}

// TestEffectiveAIKeySetForOrg_FailSoft proves a resolver with no opinion — or
// one that does not implement the optional getter at all — degrades to the YAML
// floor rather than reporting a false answer either way.
func TestEffectiveAIKeySetForOrg_FailSoft(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })

	SetAISettingsResolver(&stubAIKeyResolver{keySet: false, ok: false})
	if !EffectiveAIKeySetForOrg("acme", "sk-yaml") {
		t.Error("no opinion must fall back to the YAML floor (set)")
	}
	if EffectiveAIKeySetForOrg("acme", "") {
		t.Error("no opinion must fall back to the YAML floor (unset)")
	}

	// A resolver that implements only the base interface has no opinion here.
	SetAISettingsResolver(&stubAISettings{key: "org-key", keyOK: true})
	if !EffectiveAIKeySetForOrg("acme", "sk-yaml") {
		t.Error("a key/enabled-only resolver must fall back to the YAML floor (set)")
	}
	if EffectiveAIKeySetForOrg("acme", "") {
		t.Error("a key/enabled-only resolver must fall back to the YAML floor (unset)")
	}
}
