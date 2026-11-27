package agent

import (
	"strings"
	"testing"
)

// Tokens in the shape of a real one are built from parts, so the source holds
// no literal a secret scanner reports as a leaked credential.
var openAIProjectKey = "sk-" + "proj-AbCdEf0123456789AbCdEfGhIjKlMnOpQr"

// slackToken is a Slack token of the given type letter (b, p, a, ...) and body.
func slackToken(kind, body string) string { return "xox" + kind + "-" + body }

func TestRedactor_DefaultRules(t *testing.T) {
	r, errs := NewRedactor(false, nil)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors building default redactor: %v", errs)
	}

	cases := []struct {
		name        string
		in          string
		mustNotKeep string // substring that must be redacted out
		mustContain string // tag we expect to see
	}{
		{"email", "user alice@example.com signed in", "alice@example.com", "<REDACTED:email>"},
		{"bearer", "Authorization: Bearer abc.def.ghi", "abc.def.ghi", "<REDACTED:"},
		{"jwt", "token=" + strings.Join([]string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", "eyJzdWIiOiIxMjM0NSJ9", "SflKxw"}, "."), "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", "<REDACTED:"},
		{"uuid", "request 550e8400-e29b-41d4-a716-446655440000 failed", "550e8400-e29b-41d4-a716-446655440000", "<REDACTED:uuid>"},
		{"openai_key", "calling model with " + openAIProjectKey + " failed", openAIProjectKey, "<REDACTED:openai_key>"},
		{"slack_token", "posting via " + slackToken("b", "2410-1234567890-AbCdEfGhIjKl") + " to channel", slackToken("b", "2410-1234567890-AbCdEfGhIjKl"), "<REDACTED:slack_token>"},
		{"slack_user_token", "authed as " + slackToken("p", "9988-7766554433-ZzYyXxWwVvUu") + " now", slackToken("p", "9988-7766554433-ZzYyXxWwVvUu"), "<REDACTED:slack_token>"},
		{"slack_app_token", "app-level token " + slackToken("a", "1010-2020303040-MnBvCxZaSdFg") + " live", slackToken("a", "1010-2020303040-MnBvCxZaSdFg"), "<REDACTED:slack_token>"},
		{"slack_refresh_token", "refresh via " + slackToken("r", "3030-4040505060-LkJhGfDsApQw") + " now", slackToken("r", "3030-4040505060-LkJhGfDsApQw"), "<REDACTED:slack_token>"},
		{"slack_config_token", "reading via " + slackToken("c", "1111-2222333344-QwErTyUiOpAs") + " now", slackToken("c", "1111-2222333344-QwErTyUiOpAs"), "<REDACTED:slack_token>"},
		{"slack_session_token", "session tok " + slackToken("s", "7070-8080909010-QwErTyUiOpXc") + " set", slackToken("s", "7070-8080909010-QwErTyUiOpXc"), "<REDACTED:slack_token>"},
		{"slack_export_token", "export tok " + slackToken("e", "5566-7788990011-ZxCvBnMlKjHg") + " used", slackToken("e", "5566-7788990011-ZxCvBnMlKjHg"), "<REDACTED:slack_token>"},
		{"slack_legacy_token", "legacy cookie " + slackToken("d", "2233-4455667788-PoIuYtReWqAs") + " set", slackToken("d", "2233-4455667788-PoIuYtReWqAs"), "<REDACTED:slack_token>"},
		{"basic_auth", "connecting to postgres://admin:sup3rSecret@db.internal:5432/app", "admin:sup3rSecret", "<REDACTED:basic_auth>"},
		{"user_agent_chrome", `agent="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"`, "Chrome/120.0.0.0", "<REDACTED:user_agent>"},
		{"user_agent_linux", `Mozilla/5.0 ( X11; Linux i686 ) AppleWebKit/534.24 ( KHTML , like Gecko ) Chrome/11.0.696.50 Safari/534.24`, "Chrome/11.0.696.50", "<REDACTED:user_agent>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := r.Scrub(tc.in)
			if strings.Contains(out, tc.mustNotKeep) {
				t.Errorf("expected %q to be redacted out of %q", tc.mustNotKeep, out)
			}
			if !strings.Contains(out, tc.mustContain) {
				t.Errorf("expected %q to contain marker %q", out, tc.mustContain)
			}
		})
	}
}

func TestRedactor_CredentialNearMisses(t *testing.T) {
	r, _ := NewRedactor(false, nil)

	cases := []struct {
		name   string
		in     string
		marker string // rule tag that must NOT appear
		keep   string // original substring that must survive untouched
	}{
		// "sk-" prefix too short to be a real key.
		{"openai_short", "feature flag sk-beta enabled", "<REDACTED:openai_key>", "sk-beta"},
		// "sk" not on a word boundary (part of "ask-").
		{"openai_word_boundary", "please ask-service to restart", "<REDACTED:openai_key>", "ask-service"},
		// "xoxo" is not a valid Slack token prefix.
		{"slack_bad_prefix", "greeting xoxo-hello-there sent", "<REDACTED:slack_token>", "xoxo-hello-there"},
		// URL with a port but no user:pass@ userinfo.
		{"basic_auth_port_only", "probing http://localhost:8080/health now", "<REDACTED:basic_auth>", "localhost:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := r.Scrub(tc.in)
			if strings.Contains(out, tc.marker) {
				t.Errorf("expected %q NOT to be redacted, but got marker %q in %q", tc.in, tc.marker, out)
			}
			if !strings.Contains(out, tc.keep) {
				t.Errorf("expected %q to survive redaction, got %q", tc.keep, out)
			}
		})
	}
}

func TestRedactor_BasicAuthPreservesSeparator(t *testing.T) {
	r, _ := NewRedactor(false, nil)
	in := "connecting to postgres://admin:sup3rSecret@db.internal:5432/app"
	out := r.Scrub(in)

	// Credentials must be fully removed regardless of the cosmetic separator.
	if strings.Contains(out, "admin") || strings.Contains(out, "sup3rSecret") {
		t.Fatalf("basic_auth creds leaked: %q", out)
	}
	if !strings.Contains(out, "<REDACTED:basic_auth>") {
		t.Fatalf("expected basic_auth marker in %q", out)
	}
	// The scheme separator `://` and the `@host` boundary are preserved, so the
	// output reads `://<REDACTED:basic_auth>@db.internal` rather than swallowing
	// the delimiters into the token.
	if !strings.Contains(out, "://<REDACTED:basic_auth>@db.internal") {
		t.Errorf("expected `://<REDACTED:basic_auth>@db.internal` boundary preserved, got %q", out)
	}
}

func TestRedactor_IPsOptIn(t *testing.T) {
	off, _ := NewRedactor(false, nil)
	if got := off.Scrub("client 10.0.0.1 connected"); !strings.Contains(got, "10.0.0.1") {
		t.Errorf("ipv4 should be preserved when redact_ips=false, got %q", got)
	}
	on, _ := NewRedactor(true, nil)
	if got := on.Scrub("client 10.0.0.1 connected"); strings.Contains(got, "10.0.0.1") {
		t.Errorf("ipv4 should be redacted when redact_ips=true, got %q", got)
	}
}

func TestRedactor_ScrubFields_Recursive(t *testing.T) {
	r, _ := NewRedactor(false, nil)
	in := map[string]interface{}{
		"user":  "bob@example.com",
		"depth": 1,
		"nested": map[string]interface{}{
			"token": "Authorization: Bearer xyz123token",
		},
		"list": []interface{}{"contact a@b.co", 42},
	}
	out := r.ScrubFields(in)
	if got := out["user"].(string); strings.Contains(got, "bob@example.com") {
		t.Errorf("nested email leaked: %s", got)
	}
	if got := out["nested"].(map[string]interface{})["token"].(string); strings.Contains(got, "xyz123token") {
		t.Errorf("nested bearer leaked: %s", got)
	}
}

func TestRedactor_BadExtraPatternIsTolerated(t *testing.T) {
	r, errs := NewRedactor(false, []string{"((bad regex"})
	if len(errs) == 0 {
		t.Fatalf("expected error for bad regex")
	}
	if r == nil {
		t.Fatalf("redactor should still be returned despite bad rule")
	}
	// Default rules still work.
	if got := r.Scrub("a@b.co"); !strings.Contains(got, "<REDACTED:") {
		t.Errorf("expected default rules still active, got %q", got)
	}
}
