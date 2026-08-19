package services

import (
	"strings"
	"testing"
)

// legacyFirstString is the ad-hoc content accessor EmitFingerprint used for its
// source and pattern components before they were routed through the shared
// extraction: it copies the whole payload into a lowercased map, so two
// spellings of one key collapse and the winner is whichever Go's randomised map
// iteration wrote last. It is kept here only to prove the non-determinism the
// shared accessor closes is real, so the guard below cannot pass vacuously.
func legacyFirstString(content map[string]interface{}, keys ...string) string {
	lower := make(map[string]interface{}, len(content))
	for k, v := range content {
		lower[strings.ToLower(k)] = v
	}
	for _, k := range keys {
		if v, ok := lower[strings.ToLower(k)]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// legacyKeyedFingerprint composes the fingerprint exactly as EmitFingerprint
// does today EXCEPT that source and pattern are read with legacyFirstString.
// The accessor is therefore the only variable between the two compositions.
func legacyKeyedFingerprint(content map[string]interface{}) string {
	source := legacyFirstString(content, "source")
	service := ExtractService(content)
	pattern := legacyFirstString(content, "patternid", "pattern_id", "key")
	severity := ExtractSeverity(content)
	if pattern == "" {
		if title := ExtractTitle(content); title != "" {
			pattern = "t:" + hashNormalized(title)
		}
	}
	parts := []string{source, service, pattern, severity}
	for i, p := range parts {
		parts[i] = escapeFingerprintComponent(p)
	}
	return strings.Join(parts, string(fingerprintSeparator))
}

// TestEmitFingerprint_DuplicateSpellingsOneBucket is the dedup guarantee: a
// payload carrying two spellings of `source` or of the pattern key must land in
// ONE bucket. Under the old ad-hoc accessor the winner was decided by map
// iteration, so the same alert alternated between two fingerprints — two
// suppression states for one alert, each seeing roughly half the traffic and so
// neither reaching a threshold.
//
// The legacy composition is exercised alongside so the guard cannot pass
// vacuously: it must still split.
func TestEmitFingerprint_DuplicateSpellingsOneBucket(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"duplicate source spellings": {
			"Source": "sns", "source": "sns-eu",
			"ServiceName": "checkout", "PatternID": "P1", "Severity": "critical",
		},
		"duplicate pattern spellings": {
			"Source": "sns", "ServiceName": "checkout",
			"PatternID": "P1", "patternid": "P2", "Severity": "critical",
		},
		"duplicate spellings, neither exact": {
			"SOURCE": "sns", "Source": "sns-eu",
			"ServiceName": "checkout", "PATTERNID": "P1", "PatternId": "P2",
			"Severity": "critical",
		},
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			shared := map[string]struct{}{}
			legacy := map[string]struct{}{}
			for i := 0; i < 2000; i++ {
				shared[EmitFingerprint(content)] = struct{}{}
				legacy[legacyKeyedFingerprint(content)] = struct{}{}
			}
			if len(shared) != 1 {
				t.Fatalf("EmitFingerprint produced %d distinct keys for one payload, want 1: %v", len(shared), keysOfSet(shared))
			}
			if len(legacy) < 2 {
				t.Fatalf("guard is vacuous: the pre-fix accessor resolved %v deterministically (%v)", content, keysOfSet(legacy))
			}
		})
	}
}

// TestEmitFingerprint_SourceAndPatternMatchDurableSource proves the fingerprint
// reads a payload's source through the same accessor the durable Source column
// does, so a payload carrying two spellings cannot be grouped under one value
// and displayed under another.
func TestEmitFingerprint_SourceAndPatternMatchDurableSource(t *testing.T) {
	content := map[string]interface{}{
		"PatternID":   "P1",
		"Source":      "agent:elasticsearch:prod",
		"ServiceName": "checkout",
		"Severity":    "critical",
	}
	durable := resolveSource(content, "")
	fp := EmitFingerprint(content)
	if !strings.HasPrefix(fp, durable+string(fingerprintSeparator)) {
		t.Fatalf("fingerprint %q does not lead with the durable source %q", fp, durable)
	}
}

// TestEmitFingerprint_WhitespaceOnlyComponentsAreEmpty proves the shared
// accessor's trimming reaches the fingerprint: a padded value is grouped with
// its trimmed twin, and a whitespace-only one is treated as absent rather than
// becoming its own bucket.
func TestEmitFingerprint_WhitespaceOnlyComponentsAreEmpty(t *testing.T) {
	padded := EmitFingerprint(map[string]interface{}{
		"source": "  sns  ", "ServiceName": "checkout", "PatternID": "  P1  ", "Severity": "critical",
	})
	trimmed := EmitFingerprint(map[string]interface{}{
		"source": "sns", "ServiceName": "checkout", "PatternID": "P1", "Severity": "critical",
	})
	if padded != trimmed {
		t.Fatalf("padded fingerprint %q != trimmed %q", padded, trimmed)
	}

	blank := EmitFingerprint(map[string]interface{}{
		"source": "   ", "ServiceName": "checkout", "PatternID": "P1", "Severity": "critical",
	})
	absent := EmitFingerprint(map[string]interface{}{
		"ServiceName": "checkout", "PatternID": "P1", "Severity": "critical",
	})
	if blank != absent {
		t.Fatalf("whitespace-only source fingerprint %q != absent-source %q", blank, absent)
	}
}

func keysOfSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}
