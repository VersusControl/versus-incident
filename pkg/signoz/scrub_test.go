package signoz

import (
	"encoding/json"
	"reflect"
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

// TestBoundValueReplacesValuesUnderSecretFieldNames covers response fields
// named like secrets, in rows and in discovery maps: the value is replaced,
// fields that only contain a secret word are kept, and objects are still
// walked field by field.
func TestBoundValueReplacesValuesUnderSecretFieldNames(t *testing.T) {
	service := &Service{}
	rows := map[string]any{"rows": []any{map[string]any{
		"body": "login ok", "password": "hunter2", "token_count": float64(5), "has_token": true,
		"credentials": []any{"a", "b"},
		"secret":      map[string]any{"name": "db-creds", "api_key": "k-123"},
		"api_token":   []any{"hunter2", map[string]any{"a": float64(1), "db_password": "hunter3"}, []any{"hunter4"}},
		"env":         []any{map[string]any{"name": "DB_PASSWORD", "value": "hunter5"}, map[string]any{"name": "PORT", "value": "8080"}},
	}}}
	got, _, _ := service.boundValue(rows, responseRows, 10)
	want := map[string]any{"rows": []any{map[string]any{
		"body": "login ok", "password": "<REDACTED:secret>", "token_count": float64(5), "has_token": true,
		"credentials": []any{"<REDACTED:secret>", "<REDACTED:secret>"},
		"secret":      map[string]any{"name": "db-creds", "api_key": "<REDACTED:secret>"},
		// A list under a secret name is replaced item by item even when it
		// also holds an object, which is walked field by field.
		"api_token": []any{"<REDACTED:secret>", map[string]any{"a": float64(1), "db_password": "<REDACTED:secret>"}, []any{"<REDACTED:secret>"}},
		"env":       []any{map[string]any{"name": "DB_PASSWORD", "value": "<REDACTED:secret>"}, map[string]any{"name": "PORT", "value": "8080"}},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %#v, want %#v", got, want)
	}
	discovery := map[string]any{"values": map[string]any{"password": []any{"hunter2"}, "service.name": []any{"api"}}}
	got, _, _ = service.boundValue(discovery, responseDiscovery, 10)
	wantDiscovery := map[string]any{"values": map[string]any{"password": []any{"<REDACTED:secret>"}, "service.name": []any{"api"}}}
	if !reflect.DeepEqual(got, wantDiscovery) {
		t.Fatalf("discovery = %#v, want %#v", got, wantDiscovery)
	}
}
