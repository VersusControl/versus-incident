package signalsources

import "testing"

// TestKindOf_DefaultsAndRegistered verifies the taxonomy lookup: the six
// built-in OSS log types resolve to KindLogs, an unknown type defaults to
// KindLogs, and types registered through the seam (as the enterprise module
// does for prometheus/traces) resolve to their declared kind. The enterprise
// types are registered here in-test to keep this OSS test OSS-only.
func TestKindOf_DefaultsAndRegistered(t *testing.T) {
	// Built-in OSS log types (registered by this package's init()).
	for _, typ := range []string{"elasticsearch", "file", "loki", "cloudwatchlogs", "graylog", "splunk"} {
		if got := KindOf(typ); got != KindLogs {
			t.Errorf("KindOf(%q) = %q, want %q", typ, got, KindLogs)
		}
	}

	// Unknown/unregistered types default to logs (back-compat: behave exactly
	// as before the taxonomy existed).
	if got := KindOf("unknown-type"); got != KindLogs {
		t.Errorf("KindOf(unknown-type) = %q, want %q (default)", got, KindLogs)
	}

	// Enterprise types register through the same seam. Mirror that here.
	RegisterKind("prometheus", KindMetrics)
	RegisterKind("traces", KindTraces)
	if got := KindOf("prometheus"); got != KindMetrics {
		t.Errorf("KindOf(prometheus) = %q, want %q", got, KindMetrics)
	}
	if got := KindOf("traces"); got != KindTraces {
		t.Errorf("KindOf(traces) = %q, want %q", got, KindTraces)
	}
}

// TestRegisterKind_Panics asserts the wiring-bug guards: empty type, empty
// kind, and duplicate registration each panic (like Register).
func TestRegisterKind_Panics(t *testing.T) {
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic, got none", name)
			}
		}()
		fn()
	}

	assertPanics("empty type", func() { RegisterKind("", KindLogs) })
	assertPanics("empty kind", func() { RegisterKind("kind-test-empty", "") })

	const dup = "kind-test-dup"
	RegisterKind(dup, KindLogs)
	assertPanics("duplicate", func() { RegisterKind(dup, KindLogs) })
}
