package services

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
	m "github.com/VersusControl/versus-incident/pkg/models"
)

// fakeProvider is a capturing core.AlertProvider: it reports a fixed channel
// name and records every incident handed to it, so a test can assert which
// channels a fan-out actually reached.
type fakeProvider struct {
	name string
	sent []*m.Incident
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) SendAlert(incident *m.Incident) error {
	f.sent = append(f.sent, incident)
	return nil
}

// TestSetEmitInterceptor_NilDefaultInert proves the community default: with no
// interceptor registered the decision is always EmitProceed and no hook is ever
// consulted, so CreateIncident behaves exactly as before.
func TestSetEmitInterceptor_NilDefaultInert(t *testing.T) {
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	SetEmitInterceptor(nil) // ensure clean slate

	d := resolveEmitDecision(map[string]interface{}{"title": "x"}, "team")
	if d.Action != EmitProceed {
		t.Fatalf("nil-default decision = %v, want EmitProceed", d.Action)
	}

	// Registering then clearing must return to the no-op: the spy proves the
	// slot is truly empty (never consulted) after a nil clear.
	var calls int
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		calls++
		return EmitDecision{Action: EmitSuppress}
	})
	SetEmitInterceptor(nil)

	d = resolveEmitDecision(map[string]interface{}{"title": "x"}, "team")
	if d.Action != EmitProceed {
		t.Fatalf("after nil clear decision = %v, want EmitProceed", d.Action)
	}
	if calls != 0 {
		t.Fatalf("cleared interceptor was consulted %d times, want 0", calls)
	}
}

// TestSetEmitInterceptor_LastWins proves the atomic slot keeps the most recent
// registration and that nil clears it — mirroring the admin_audit seam.
func TestSetEmitInterceptor_LastWins(t *testing.T) {
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitDivert, Reason: "first"}
	})
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitSuppress, Reason: "second"}
	})

	d := resolveEmitDecision(nil, "")
	if d.Action != EmitSuppress || d.Reason != "second" {
		t.Fatalf("last-wins decision = %+v, want EmitSuppress/second", d)
	}

	SetEmitInterceptor(nil)
	if got := resolveEmitDecision(nil, ""); got.Action != EmitProceed {
		t.Fatalf("after nil clear = %v, want EmitProceed", got.Action)
	}
}

// TestResolveEmitDecision_FailOpenOnPanic proves a panicking interceptor never
// drops a page: the panic is recovered and the verdict is EmitProceed.
func TestResolveEmitDecision_FailOpenOnPanic(t *testing.T) {
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		panic("boom")
	})

	d := resolveEmitDecision(map[string]interface{}{"title": "x"}, "team")
	if d.Action != EmitProceed {
		t.Fatalf("panicking interceptor decision = %v, want EmitProceed (fail-open)", d.Action)
	}
}

// TestApplyEmitRouting_Proceed proves EmitProceed fans out to the full default
// channel set — the built providers are returned untouched.
func TestApplyEmitRouting_Proceed(t *testing.T) {
	providers := fakeProviders("slack", "telegram", "email")

	got := applyEmitRouting(EmitDecision{Action: EmitProceed}, providers)

	if names := providerChannels(got); !equalSet(names, []string{"slack", "telegram", "email"}) {
		t.Fatalf("EmitProceed channels = %v, want the full default set", names)
	}
}

// TestApplyEmitRouting_Divert proves EmitDivert fans out to ONLY the named
// channels — the primary on-call channels are excluded.
func TestApplyEmitRouting_Divert(t *testing.T) {
	providers := fakeProviders("slack", "telegram", "email")

	got := applyEmitRouting(
		EmitDecision{Action: EmitDivert, DivertChannels: []string{"email"}},
		providers,
	)

	if names := providerChannels(got); !equalSet(names, []string{"email"}) {
		t.Fatalf("EmitDivert channels = %v, want only [email]", names)
	}
}

// TestFilterProvidersByChannel covers the divert channel restriction: only
// named channels survive, unknown names are ignored, and an empty set yields no
// providers.
func TestFilterProvidersByChannel(t *testing.T) {
	providers := fakeProviders("slack", "telegram", "email")

	t.Run("subset kept", func(t *testing.T) {
		got := filterProvidersByChannel(providers, []string{"slack", "email"})
		if names := providerChannels(got); !equalSet(names, []string{"slack", "email"}) {
			t.Fatalf("got %v, want [slack email]", names)
		}
	})

	t.Run("unknown name ignored", func(t *testing.T) {
		got := filterProvidersByChannel(providers, []string{"slack", "pager"})
		if names := providerChannels(got); !equalSet(names, []string{"slack"}) {
			t.Fatalf("got %v, want [slack]", names)
		}
	})

	t.Run("empty names yields none", func(t *testing.T) {
		if got := filterProvidersByChannel(providers, nil); len(got) != 0 {
			t.Fatalf("got %d providers, want 0", len(got))
		}
	})

	t.Run("case and whitespace tolerant", func(t *testing.T) {
		got := filterProvidersByChannel(providers, []string{" Slack "})
		if names := providerChannels(got); !equalSet(names, []string{"slack"}) {
			t.Fatalf("got %v, want [slack]", names)
		}
	})
}

// TestCreateIncident_SuppressSkipsFanOut proves EmitSuppress short-circuits
// before any config resolution or provider building: CreateIncident returns nil
// without error even though no global config is loaded in this test (a proceed
// path would reach GetConfig and panic). This is the "no primary fan-out, no
// error" guarantee.
func TestCreateIncident_SuppressSkipsFanOut(t *testing.T) {
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitSuppress, Reason: "dedup"}
	})

	content := map[string]interface{}{"title": "disk full", "Status": "firing"}
	if err := CreateIncident("team", &content); err != nil {
		t.Fatalf("EmitSuppress CreateIncident returned error %v, want nil", err)
	}
}

// TestEmitFingerprint covers the composite key: stable across calls, distinct
// for distinct (source, service, pattern, severity), a sane webhook fallback
// when PatternID is absent, and no dependence on any raw payload field.
func TestEmitFingerprint(t *testing.T) {
	agent := map[string]interface{}{
		"Source":      "agent:elasticsearch:prod-app",
		"ServiceName": "checkout",
		"PatternID":   "p-123",
		"Severity":    "high",
	}

	t.Run("stable across calls", func(t *testing.T) {
		fp1 := EmitFingerprint(agent)
		fp2 := EmitFingerprint(agent)
		if fp1 != fp2 {
			t.Fatal("fingerprint not stable across calls")
		}
	})

	t.Run("distinct on severity", func(t *testing.T) {
		other := cloneContent(agent)
		other["Severity"] = "low"
		if EmitFingerprint(agent) == EmitFingerprint(other) {
			t.Fatal("severity change did not change fingerprint")
		}
	})

	t.Run("distinct on service", func(t *testing.T) {
		other := cloneContent(agent)
		other["ServiceName"] = "billing"
		if EmitFingerprint(agent) == EmitFingerprint(other) {
			t.Fatal("service change did not change fingerprint")
		}
	})

	t.Run("distinct on pattern", func(t *testing.T) {
		other := cloneContent(agent)
		other["PatternID"] = "p-999"
		if EmitFingerprint(agent) == EmitFingerprint(other) {
			t.Fatal("pattern change did not change fingerprint")
		}
	})

	t.Run("webhook title fallback distinguishes distinct titles", func(t *testing.T) {
		a := map[string]interface{}{"Source": "webhook", "service": "api", "title": "CPU high", "Severity": "warning"}
		b := map[string]interface{}{"Source": "webhook", "service": "api", "title": "Disk full", "Severity": "warning"}
		if EmitFingerprint(a) == EmitFingerprint(b) {
			t.Fatal("distinct webhook titles produced the same fingerprint")
		}
		// Cosmetic-only differences (case, spacing) must fold to one key.
		c := map[string]interface{}{"Source": "webhook", "service": "api", "title": "  cpu   HIGH  ", "Severity": "warning"}
		if EmitFingerprint(a) != EmitFingerprint(c) {
			t.Fatal("cosmetically identical titles produced different fingerprints")
		}
	})

	t.Run("ignores raw payload fields", func(t *testing.T) {
		withRaw := cloneContent(agent)
		withRaw["Raw"] = "SECRET-TOKEN-abc123"
		withRaw["raw"] = "another secret"
		if EmitFingerprint(agent) != EmitFingerprint(withRaw) {
			t.Fatal("fingerprint changed with an unrelated raw field; it must read only the composite keys")
		}
	})
}

func fakeProviders(names ...string) []core.AlertProvider {
	out := make([]core.AlertProvider, 0, len(names))
	for _, n := range names {
		out = append(out, &fakeProvider{name: n})
	}
	return out
}

func providerChannels(providers []core.AlertProvider) []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.Name())
	}
	return out
}

func equalSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

func cloneContent(src map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
