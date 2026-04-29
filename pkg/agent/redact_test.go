package agent

import (
	"strings"
	"testing"
)

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
		{"jwt", "token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NSJ9.SflKxw", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", "<REDACTED:"},
		{"uuid", "request 550e8400-e29b-41d4-a716-446655440000 failed", "550e8400-e29b-41d4-a716-446655440000", "<REDACTED:uuid>"},
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
