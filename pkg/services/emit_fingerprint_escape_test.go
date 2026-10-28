package services

import (
	"regexp"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/utils"
)

// unescapedFingerprint is the composition EmitFingerprint used before the
// components were escaped: the four labels joined raw. It is kept here only to
// prove the collision the escaping closes is real, so the guard below cannot
// pass vacuously.
func unescapedFingerprint(content map[string]interface{}) string {
	source := utils.ExtractSource(content)
	service := ExtractService(content)
	pattern := utils.ExtractPatternID(content)
	severity := ExtractSeverity(content)
	if pattern == "" {
		if title := ExtractTitle(content); title != "" {
			pattern = "t:" + hashNormalized(title)
		}
	}
	return strings.Join([]string{source, service, pattern, severity}, "|")
}

// TestEmitFingerprint_SeparatorInjection proves a crafted PatternID can no
// longer be walked into a victim's dedup bucket. The victim's service label
// legitimately carries the separator ("checkout|eu"), so joining the four
// labels raw let an attacker shift the component boundary — service
// "checkout" plus PatternID "eu|P42" rendered the identical key, and every
// suppression decision the victim's bucket accumulated then applied to the
// attacker's alerts and back.
func TestEmitFingerprint_SeparatorInjection(t *testing.T) {
	victim := map[string]interface{}{
		"Source":      "sns",
		"ServiceName": "checkout|eu",
		"PatternID":   "P42",
		"Severity":    "critical",
	}
	attacker := map[string]interface{}{
		"Source":      "sns",
		"ServiceName": "checkout",
		"PatternID":   "eu|P42",
		"Severity":    "critical",
	}

	if unescapedFingerprint(victim) != unescapedFingerprint(attacker) {
		t.Fatalf("the crafted payloads no longer collide unescaped (%q vs %q); the guard below proves nothing",
			unescapedFingerprint(victim), unescapedFingerprint(attacker))
	}
	if got, want := EmitFingerprint(victim), EmitFingerprint(attacker); got == want {
		t.Fatalf("crafted PatternID landed in the victim's dedup bucket: %q", got)
	}
}

// TestEmitFingerprint_ComponentsStayDistinct walks a matrix of components that
// contain the separator and the escape character. Distinct component tuples
// must always produce distinct fingerprints — the escaping has to be injective,
// not merely different from the old output.
func TestEmitFingerprint_ComponentsStayDistinct(t *testing.T) {
	values := []string{"", "a", "a|b", "|", "||", `a\b`, `a\|b`, `\`, `\\`, "a|", "|a"}

	seen := map[string][4]string{}
	for _, source := range values {
		for _, service := range values {
			for _, pattern := range values {
				for _, severity := range values {
					tuple := [4]string{source, service, pattern, severity}
					content := map[string]interface{}{
						"Source":      source,
						"ServiceName": service,
						"PatternID":   pattern,
						"Severity":    severity,
					}
					fp := EmitFingerprint(content)
					if prev, dup := seen[fp]; dup {
						t.Fatalf("fingerprint %q shared by %q and %q", fp, prev, tuple)
					}
					seen[fp] = tuple
				}
			}
		}
	}
}

// TestEmitFingerprint_Deterministic pins the two properties the enterprise
// store depends on: the same payload always keys the same, and a payload
// carrying two spellings of one key keys the same on every read even though Go
// randomises map iteration.
func TestEmitFingerprint_Deterministic(t *testing.T) {
	crafted := map[string]interface{}{
		"Source":      "sns",
		"ServiceName": `checkout|eu\prod`,
		"PatternID":   `P42|\`,
		"Severity":    "critical",
	}
	want := EmitFingerprint(crafted)
	for i := 0; i < 1000; i++ {
		if got := EmitFingerprint(crafted); got != want {
			t.Fatalf("fingerprint drifted on read %d: %q vs %q", i, got, want)
		}
	}

	duplicateSpellings := map[string]interface{}{
		"Source":    "sns",
		"service":   "checkout",
		"severity":  "warning",
		"AlarmName": "alpha",
		"alarmname": "beta",
		"ALARMNAME": "gamma",
	}
	buckets := map[string]struct{}{}
	for i := 0; i < 2000; i++ {
		buckets[EmitFingerprint(duplicateSpellings)] = struct{}{}
	}
	if len(buckets) != 1 {
		t.Fatalf("duplicate title spellings produced %d dedup buckets: %v", len(buckets), buckets)
	}
}

// TestHashNormalized_TruncatedSHA256 pins the title hash's shape and its
// folding. The hash guards a dedup bucket an attacker must not be able to
// steer into, so it is a truncated cryptographic digest rather than a fast
// non-cryptographic one; the width stays fixed hex so anything keyed on it
// sees the same shape.
func TestHashNormalized_TruncatedSHA256(t *testing.T) {
	hexOnly := regexp.MustCompile(`^[0-9a-f]{32}$`)

	got := hashNormalized("CPU high")
	if !hexOnly.MatchString(got) {
		t.Fatalf("hashNormalized = %q, want %d lowercase hex chars", got, titleHashHexLen)
	}
	// The leading 128 bits of SHA-256("cpu high").
	if want := "f7fdefc909fe3d3f8c813a06066b978c"; got != want {
		t.Fatalf("hashNormalized(%q) = %q, want %q", "CPU high", got, want)
	}

	// Cosmetic differences fold to one key; real differences do not.
	if hashNormalized("  cpu   HIGH  ") != got {
		t.Fatal("cosmetically identical titles hashed differently")
	}
	if hashNormalized("Disk full") == got {
		t.Fatal("distinct titles hashed identically")
	}

	// The fallback component carries the marker plus the fixed-width hash.
	fp := EmitFingerprint(map[string]interface{}{
		"Source": "webhook", "service": "api", "title": "CPU high", "Severity": "warning",
	})
	parts := strings.Split(fp, "|")
	if len(parts) != 4 {
		t.Fatalf("fingerprint %q split into %d parts, want 4", fp, len(parts))
	}
	if !strings.HasPrefix(parts[2], "t:") || !hexOnly.MatchString(strings.TrimPrefix(parts[2], "t:")) {
		t.Fatalf("title fallback component = %q, want t:<%d hex>", parts[2], titleHashHexLen)
	}
}
