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

	t.Run("distinct cloudwatch alarms on one service do not collide", func(t *testing.T) {
		// Both alarms share source, service and severity; only the alarm name
		// differs. Before AlarmName joined the title key set both hashed to the
		// same degenerate key and deduped each other.
		alarm := func(name string) map[string]interface{} {
			return map[string]interface{}{
				"Source":        "sns",
				"AlarmName":     name,
				"NewStateValue": "ALARM",
				"severity":      "warning",
				"Trigger": map[string]interface{}{"Dimensions": []interface{}{
					map[string]interface{}{"name": "ServiceName", "value": "checkout"},
				}},
			}
		}
		five := alarm("checkout-5xx")
		latency := alarm("checkout-latency")
		if EmitFingerprint(five) == EmitFingerprint(latency) {
			t.Fatalf("distinct CloudWatch alarms shared fingerprint %q", EmitFingerprint(five))
		}
		// The same alarm firing again must still fold onto one key.
		if EmitFingerprint(five) != EmitFingerprint(alarm("checkout-5xx")) {
			t.Fatal("the same CloudWatch alarm produced different fingerprints")
		}
	})

	t.Run("duplicate spellings of a title key keep one dedup bucket", func(t *testing.T) {
		// A payload carrying two spellings of the same title key used to be
		// titled by whichever spelling map iteration yielded, so the same alert
		// landed in two dedup buckets and lost its suppression state.
		seen := map[string]int{}
		for i := 0; i < 2000; i++ {
			seen[EmitFingerprint(map[string]interface{}{
				"Source":    "sns",
				"service":   "checkout",
				"severity":  "warning",
				"AlarmName": "alpha",
				"alarmname": "beta",
			})]++
		}
		if len(seen) != 1 {
			t.Fatalf("duplicate title spellings produced %d dedup buckets: %v", len(seen), seen)
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

	t.Run("a pattern verdict does not move the severity slot", func(t *testing.T) {
		// An agent finding carries no real severity; the pattern later acquires
		// an operator-set verdict. That must not re-key the finding into a new
		// dedup bucket, and it must not put operator free text in the slot the
		// priority floor reads.
		finding := map[string]interface{}{
			"Source":      "agent:elasticsearch:prod-app",
			"ServiceName": "checkout",
			"PatternID":   "p-123",
		}
		labelled := cloneContent(finding)
		labelled["Verdict"] = "known"
		if EmitFingerprint(finding) != EmitFingerprint(labelled) {
			t.Fatalf("verdict changed the fingerprint: %q vs %q", EmitFingerprint(finding), EmitFingerprint(labelled))
		}
		if got := ExtractSeverity(labelled); got != "" {
			t.Fatalf("ExtractSeverity of a verdict-only finding = %q, want empty", got)
		}
		critical := cloneContent(finding)
		critical["verdict"] = "critical"
		if got := ExtractSeverity(critical); got != "" {
			t.Fatalf("operator-typed verdict surfaced as severity %q; the priority floor must not be steerable this way", got)
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
