package signoz

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func TestScrubExactSecretValueRecursivelyCopiesJSONValues(t *testing.T) {
	const secret = "opaque-signoz-key-7f3d"
	original := map[string]any{
		secret + "-key": []any{
			"prefix " + secret + " suffix",
			map[string]any{"nested": secret},
		},
	}

	scrubbed := ScrubExactSecretValue(original, secret).(map[string]any)
	encoded, err := json.Marshal(scrubbed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("scrubbed value contains exact secret: %s", encoded)
	}

	scrubbed["[REDACTED:SIGNOZ_API_KEY]-key"].([]any)[1].(map[string]any)["nested"] = "changed"
	if original[secret+"-key"].([]any)[1].(map[string]any)["nested"] != secret {
		t.Fatal("scrubbing mutated the caller's nested map")
	}
}

func TestScrubExactSecretPreservesEmptySecretAndUsesNonEmptyMarker(t *testing.T) {
	if got := ScrubExactSecret("value", ""); got != "value" {
		t.Fatalf("empty secret changed value to %q", got)
	}
	if got := ScrubExactSecret("opaque-signoz-key", "opaque-signoz-key"); got == "" || strings.Contains(got, "opaque-signoz-key") {
		t.Fatalf("exact collision scrub = %q, want stable non-secret marker", got)
	}
}

func TestScrubSignalExactSecretCopiesEveryProviderControlledField(t *testing.T) {
	const secret = "opaque-signoz-key-7f3d"
	original := core.Signal{
		Source:   "signoz:" + secret,
		Severity: "critical-" + secret,
		Message:  "message " + secret,
		Fields:   map[string]any{secret: []any{secret}},
		Raw:      map[string]any{secret: map[string]any{"value": secret}},
	}

	scrubbed := ScrubSignalExactSecret(original, secret)
	encoded, err := json.Marshal(scrubbed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("scrubbed signal contains exact secret: %s", encoded)
	}
	scrubbed.Fields["[REDACTED:SIGNOZ_API_KEY]"].([]any)[0] = "changed"
	if original.Fields[secret].([]any)[0] != secret || original.Raw[secret].(map[string]any)["value"] != secret {
		t.Fatal("signal scrubbing mutated provider-owned nested data")
	}
}
