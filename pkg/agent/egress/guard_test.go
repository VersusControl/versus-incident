package egress

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGuardScrubsPartsAndJSONKeys(t *testing.T) {
	secret := "sk-012345678901234567890123"
	result, err := Apply(context.Background(), NewDefaultGuard(), Request{Parts: []Part{
		{Kind: PartUser, Text: "message " + secret},
		{Kind: PartToolResult, Text: `{"` + secret + `":"` + secret + `"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range result.Parts {
		if strings.Contains(part.Text, secret) {
			t.Fatalf("secret survived in %q", part.Text)
		}
	}
	if result.Redactions == 0 {
		t.Fatal("expected redaction count")
	}
}

type panicGuard struct{}

func (panicGuard) Apply(context.Context, Request) (Result, error) { panic("sensitive detail") }

func TestGuardFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, guard := range map[string]Guard{"nil": nil, "panic": panicGuard{}} {
		t.Run(name, func(t *testing.T) {
			if _, err := Apply(context.Background(), guard, Request{}); !errors.Is(err, ErrEgressBlocked) {
				t.Fatalf("error = %v, want ErrEgressBlocked", err)
			}
		})
	}
	if _, err := Apply(ctx, NewDefaultGuard(), Request{}); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("cancelled error = %v, want ErrEgressBlocked", err)
	}
}

func decodeJSON(t *testing.T, text string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	return value
}

// TestGuardReplacesValuesUnderSecretKeys covers payloads that are valid
// JSON. The guard decodes them, so a value arrives on its own with nothing a
// rule can match; the field name is what marks it as a secret.
func TestGuardReplacesValuesUnderSecretKeys(t *testing.T) {
	cases := []struct {
		name, in, want string
		redactions     int
	}{
		{"password", `{"password":"hunter2"}`, `{"password":"<REDACTED:secret>"}`, 1},
		{"prefixed and mixed case keys", `{"DB_PASSWORD":"hunter2","x-api-key":"k-1","clientSecret":"s-1"}`, `{"DB_PASSWORD":"<REDACTED:secret>","clientSecret":"<REDACTED:secret>","x-api-key":"<REDACTED:secret>"}`, 1},
		{"authorization", `{"headers":{"Authorization":"Bearer abc.def"}}`, `{"headers":{"Authorization":"<REDACTED:secret>"}}`, 1},
		{"nested", `{"a":{"b":{"access_token":"t-1","user":"bob"}}}`, `{"a":{"b":{"access_token":"<REDACTED:secret>","user":"bob"}}}`, 1},
		{"array of objects", `[{"token":"t-1"},{"token":"t-2","n":1}]`, `[{"token":"<REDACTED:secret>"},{"n":1,"token":"<REDACTED:secret>"}]`, 1},
		{"array under a secret key", `{"credentials":["a",7,true,null,{"user":"bob","secret":"s-1"}]}`, `{"credentials":["<REDACTED:secret>","<REDACTED:secret>",true,null,{"secret":"<REDACTED:secret>","user":"bob"}]}`, 1},
		{"name and value", `{"env":[{"name":"DB_PASSWORD","value":"hunter2"},{"name":"PORT","value":"8080"}]}`, `{"env":[{"name":"DB_PASSWORD","value":"<REDACTED:secret>"},{"name":"PORT","value":"8080"}]}`, 1},
		{"header list", `[{"name":"Cookie","value":"sid=hunter2"},{"name":"Accept","value":"*/*"}]`, `[{"name":"Cookie","value":"<REDACTED:secret>"},{"name":"Accept","value":"*/*"}]`, 1},
		{"cookie and more key names", `{"cookie":"sid=x","signing_key":"k","SecretString":"s","ssh_key":"p"}`, `{"cookie":"<REDACTED:secret>","signing_key":"<REDACTED:secret>","SecretString":"<REDACTED:secret>","ssh_key":"<REDACTED:secret>"}`, 1},
		{"number", `{"pin_token":1234}`, `{"pin_token":"<REDACTED:secret>"}`, 1},
		{"object under a secret key", `{"secret":{"name":"db-creds","mounted":true}}`, `{"secret":{"mounted":true,"name":"db-creds"}}`, 0},
		{"booleans and null kept", `{"has_token":true,"client_secret":null,"password":false}`, `{"client_secret":null,"has_token":true,"password":false}`, 0},
		{"keys that only contain a secret word", `{"token_count":5,"max_tokens":4096,"secret_name":"db-creds","password_changed_at":"2026-11-01"}`, `{"max_tokens":4096,"password_changed_at":"2026-11-01","secret_name":"db-creds","token_count":5}`, 0},
		{"pair inside a string value", `{"msg":"login db_password=hunter2 ok"}`, `{"msg":"login db_password=<REDACTED:secret> ok"}`, 1},
		{"already replaced", `{"password":"<REDACTED:secret>"}`, `{"password":"<REDACTED:secret>"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Apply(context.Background(), NewDefaultGuard(), Request{Parts: []Part{{Kind: PartToolResult, Text: tc.in}}})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := decodeJSON(t, result.Parts[0].Text), decodeJSON(t, tc.want); !reflect.DeepEqual(got, want) {
				t.Fatalf("guard(%s)\n got  %s\n want %s", tc.in, result.Parts[0].Text, tc.want)
			}
			if result.Redactions != tc.redactions {
				t.Fatalf("guard(%s) counted %d redactions, want %d", tc.in, result.Redactions, tc.redactions)
			}
		})
	}
}

const markerPrefix = "REDACTED"

// upperScrubber stands in for any scrubber a caller plugs in: the guard must
// replace values under secret keys whatever the scrubber does with strings.
type upperScrubber struct{}

func (upperScrubber) Scrub(value string) string { return strings.ReplaceAll(value, "bob", "BOB") }

func TestGuardReplacesSecretFieldsWithAnyScrubber(t *testing.T) {
	result, err := Apply(context.Background(), NewRedactorGuard(upperScrubber{}), Request{Parts: []Part{
		{Kind: PartToolResult, Text: `{"user":"bob","password":"hunter2","bob":1}`},
		{Kind: PartUser, Text: `bob said hi`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"BOB":1,"password":"<REDACTED:secret>","user":"BOB"}`
	if !reflect.DeepEqual(decodeJSON(t, result.Parts[0].Text), decodeJSON(t, want)) {
		t.Fatalf("json part = %s, want %s", result.Parts[0].Text, want)
	}
	if got, want := result.Parts[1].Text, `BOB said hi`; got != want {
		t.Fatalf("text part = %s, want %s", got, want)
	}
	if result.Redactions != 2 {
		t.Fatalf("redactions = %d, want 2", result.Redactions)
	}
}

// TestGuardLeavesCleanPayloadsAsTheyWere pins what the guard does not touch:
// text and JSON with nothing to redact come back with no redaction counted.
func TestGuardLeavesCleanPayloadsAsTheyWere(t *testing.T) {
	parts := []Part{
		{Kind: PartSystem, Text: "You are an on-call assistant."},
		{Kind: PartUser, Text: "why did checkout restart 3 times?"},
		{Kind: PartToolResult, Text: `{"items":[{"n":1,"name":"checkout","ready":true}],"total":1}`},
		{Kind: PartToolArgs, Text: `{"namespace":"shop","tail":100}`},
		{Kind: PartEmbedding, Text: ""},
	}
	result, err := Apply(context.Background(), NewDefaultGuard(), Request{Parts: parts})
	if err != nil {
		t.Fatal(err)
	}
	for index, part := range result.Parts {
		if part != parts[index] {
			t.Errorf("part %d = %+v, want %+v", index, part, parts[index])
		}
		if strings.Contains(part.Text, markerPrefix) {
			t.Errorf("part %d holds a marker: %s", index, part.Text)
		}
	}
	if result.Redactions != 0 {
		t.Fatalf("redactions = %d, want 0", result.Redactions)
	}
}
