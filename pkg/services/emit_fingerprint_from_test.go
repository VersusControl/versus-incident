package services

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/utils"
)

// fingerprintShapeCorpus is the set of payload shapes the fingerprint has to
// key consistently: agent emits, webhooks that fall back to a title hash,
// CloudWatch alarms carrying their service in trigger dimensions, Alertmanager
// nested labels, separator/escape-bearing components, duplicate spellings of
// one key, and the degenerate empty payload.
func fingerprintShapeCorpus() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"agent emit": {
			"Source":      "agent:elasticsearch:prod-app",
			"ServiceName": "checkout",
			"PatternID":   "p-123",
			"Severity":    "high",
		},
		"webhook title fallback": {
			"Source":   "webhook",
			"service":  "api",
			"title":    "CPU high",
			"Severity": "warning",
		},
		"webhook title needing normalization": {
			"Source":   "webhook",
			"service":  "api",
			"title":    "  cpu   HIGH  ",
			"Severity": "warning",
		},
		"cloudwatch alarm via trigger dimensions": {
			"Source":        "sns",
			"AlarmName":     "checkout-5xx",
			"NewStateValue": "ALARM",
			"severity":      "warning",
			"Trigger": map[string]interface{}{"Dimensions": []interface{}{
				map[string]interface{}{"name": "ServiceName", "value": "checkout"},
			}},
		},
		"alertmanager nested labels": {
			"source": "alertmanager",
			"labels": map[string]interface{}{
				"service":   "billing",
				"severity":  "critical",
				"alertname": "HighErrorRate",
			},
		},
		"components carrying the separator and escape": {
			"Source":      "sns",
			"ServiceName": `checkout|eu\prod`,
			"PatternID":   `P42|\`,
			"Severity":    "critical",
		},
		"duplicate spellings of several keys": {
			"Source": "sns", "source": "sns-eu",
			"ServiceName": "checkout", "servicename": "billing",
			"AlarmName": "alpha", "alarmname": "beta",
			"Severity": "warning", "severity": "critical",
		},
		"whitespace-only components": {
			"Source":      "   ",
			"ServiceName": "\t",
			"PatternID":   " \n ",
			"Severity":    "",
		},
		"no attribution at all": {
			"detail": "nothing this key set recognises",
		},
		"empty payload": {},
	}
}

// TestEmitFingerprintFrom_AgreesWithEmitFingerprint is the contract the
// enterprise interceptor wires against: a caller that already folded the
// payload gets byte-identical keys from the overload, so an emit path that
// folds once and one that folds twice can never split a dedup bucket.
func TestEmitFingerprintFrom_AgreesWithEmitFingerprint(t *testing.T) {
	for name, content := range fingerprintShapeCorpus() {
		t.Run(name, func(t *testing.T) {
			want := EmitFingerprint(content)
			got := EmitFingerprintFrom(utils.NewFoldedPayload(content))
			if got != want {
				t.Fatalf("EmitFingerprintFrom = %q, EmitFingerprint = %q; want byte-identical", got, want)
			}
		})
	}
}

// TestEmitFingerprintFrom_WarmPayloadAgrees is the realistic call shape: the
// interceptor reads service, severity and source off its payload first and only
// then asks for the key. The fold's caches are warm by then, so this proves the
// caches do not change what the key resolves to.
func TestEmitFingerprintFrom_WarmPayloadAgrees(t *testing.T) {
	for name, content := range fingerprintShapeCorpus() {
		t.Run(name, func(t *testing.T) {
			want := EmitFingerprint(content)

			payload := utils.NewFoldedPayload(content)
			_ = payload.Source()
			_ = payload.Service()
			_ = payload.Severity()
			_ = payload.Title()
			_ = payload.PatternID()

			if got := EmitFingerprintFrom(payload); got != want {
				t.Fatalf("warm payload keyed %q, cold content keyed %q", got, want)
			}
		})
	}
}

// TestEmitFingerprintFrom_StableAcrossReads guards the determinism the store
// depends on: reusing one payload for many reads must key the same every time,
// including for a payload carrying two spellings of one key, where Go's
// randomised map iteration would otherwise show through.
func TestEmitFingerprintFrom_StableAcrossReads(t *testing.T) {
	content := map[string]interface{}{
		"Source": "sns", "source": "sns-eu",
		"ServiceName": "checkout", "servicename": "billing",
		"AlarmName": "alpha", "alarmname": "beta",
		"Severity": "warning", "severity": "critical",
	}
	want := EmitFingerprintFrom(utils.NewFoldedPayload(content))
	for i := 0; i < 1000; i++ {
		if got := EmitFingerprintFrom(utils.NewFoldedPayload(content)); got != want {
			t.Fatalf("EmitFingerprintFrom drifted: %q then %q", want, got)
		}
	}
}

// TestEmitFingerprintFrom_NilPayload pins the degenerate input to the same key
// EmitFingerprint gives an empty content map, so a caller that ends up with a
// nil payload does not create a second all-empty bucket.
func TestEmitFingerprintFrom_NilPayload(t *testing.T) {
	if got, want := EmitFingerprintFrom(nil), EmitFingerprint(nil); got != want {
		t.Fatalf("EmitFingerprintFrom(nil) = %q, EmitFingerprint(nil) = %q", got, want)
	}
}
