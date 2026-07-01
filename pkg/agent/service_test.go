package agent

import (
	"regexp"
	"testing"
)

// The two REAL log samples reported by the user, drain-masked (`<*>`). Line 1
// is a Spring Boot / Logback console line with RAW ANSI colour escapes around
// the application name; line 2 is a Logback %X MDC layout. Both are kept
// byte-for-byte (ANSI escapes included) so the tests exercise exactly what the
// matcher sees at runtime.
const (
	// The application name "lead-service" is wrapped in a blue SGR
	// ("\x1b[34m…\x1b[m"); the Kafka consumer thread "[mRegister-0-C-1]" is
	// wrapped in a bold-yellow SGR ("\x1b[33;1m…\x1b[m").
	springConsoleLine = "2026-07-01 05:08:14.502  \x1b[34mlead-service\x1b[m  " +
		"\x1b[33;1m[mRegister-0-C-1]\x1b[m  <*> WARN <*>  " +
		"k.c.NetworkClient$DefaultMetadataUpdater : [Consumer clientId=<*>, " +
		"groupId=sit-loan-register-group] Error while fetching metadata with " +
		"correlation id <*> : {<*>=UNKNOWN_TOPIC_OR_PARTITION}"

	mdcBracketLine = "[ 2026-07-01 05:08:04:661 ] [ DEBUG ] [ account-service , " +
		"requestID = , traceID = <*> , spanID = <*> ] [ DEBUG ] [ <*> ] <*> - " +
		"Parsing json error. [ accountNumber = <*> ]"
)

// servicePatterns mirrors the shipped `agent.service_patterns` list (see
// pkg/config/default_config.yaml / config/config.yaml) AFTER this fix. Kept in
// lockstep so the tests prove the shipped defaults extract the right service.
var servicePatterns = []string{
	`(?i)\bservice[._-]?name["\s:=]+"?([A-Za-z0-9._-]+)`,
	`(?i)\b(?:service|svc|app|component)\s*=\s*"?([A-Za-z0-9._-]+)`,
	`(?i)"(?:service|svc|app|component)"\s*:\s*"([A-Za-z0-9._-]+)"`,
	`^\s*\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:[.,]\d{1,9})?\s+([A-Za-z][A-Za-z0-9._-]*)\s+\[`,
	`\[\s*([A-Za-z][A-Za-z0-9._-]*)\s*,\s*(?i:request[_-]?id|trace[_-]?id|span[_-]?id)\b`,
	`\[[^\]]+\]\s+\[([A-Za-z0-9._-]+)\]`,
	`---\s+\[[^\]]*\]\s+\[([A-Za-z0-9._-]+)\]`,
	`([A-Za-z0-9._-]+)\[\d+\]:`,
	`\[([A-Za-z0-9._-]+)\]`,
}

// preFixPatterns is the pattern set as it behaved BEFORE this fix: the three
// key=value/JSON rules plus the bracket/syslog rules, plus the looser
// space-tolerant bracket rule an operator would add to catch Java "[ svc ]"
// lines. This is the "looser/other configured pattern" the falling-through
// matcher landed on.
var preFixPatterns = []string{
	`(?i)\bservice[._-]?name["\s:=]+"?([A-Za-z0-9._-]+)`,
	`(?i)\b(?:service|svc|app|component)\s*=\s*"?([A-Za-z0-9._-]+)`,
	`(?i)"(?:service|svc|app|component)"\s*:\s*"([A-Za-z0-9._-]+)"`,
	`\[[^\]]+\]\s+\[([A-Za-z0-9._-]+)\]`,
	`---\s+\[[^\]]*\]\s+\[([A-Za-z0-9._-]+)\]`,
	`([A-Za-z0-9._-]+)\[\d+\]:`,
	`\[([A-Za-z0-9._-]+)\]`,
	`\[\s*([A-Za-z][A-Za-z0-9._-]*)`, // looser, space-tolerant bracket rule
}

// firstGroupNoGuards mimics the PRE-FIX Extract: first-match-wins over the
// patterns, first capture group, WITHOUT ANSI stripping or the level guard.
func firstGroupNoGuards(t *testing.T, patterns []string, msg string) string {
	t.Helper()
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		if s := re.FindStringSubmatch(msg); len(s) >= 2 && s[1] != "" {
			return s[1]
		}
	}
	return ""
}

// TestServiceMatcher_RootCause pins WHY the agent returned the wrong token for
// both real lines before the fix, and proves the exact mechanism for line 1
// (raw ANSI bytes defeat the positional anchor).
func TestServiceMatcher_RootCause(t *testing.T) {
	// Line 1: no rule targets the positional service, and the ANSI escapes hide
	// "lead-service", so the loop falls through to the single-bracket rule which
	// grabs the Kafka consumer thread id.
	if got := firstGroupNoGuards(t, preFixPatterns, springConsoleLine); got != "mRegister-0-C-1" {
		t.Fatalf("repro line1: got %q, expected the (wrong) thread id %q", got, "mRegister-0-C-1")
	}
	// Line 2: the space-padded brackets defeat every shipped rule; the looser
	// bracket rule then grabs the LEVEL from "[ DEBUG ]".
	if got := firstGroupNoGuards(t, preFixPatterns, mdcBracketLine); got != "DEBUG" {
		t.Fatalf("repro line2: got %q, expected the (wrong) level %q", got, "DEBUG")
	}

	// Mechanism for line 1: with the ANSI escapes present a positional
	// "first token after the timestamp" pattern CANNOT match (the byte after
	// the timestamp whitespace is ESC, not a letter). Stripping the escapes
	// restores "lead-service" as the leading token.
	positional := regexp.MustCompile(`^\s*\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:[.,]\d{1,9})?\s+([A-Za-z][A-Za-z0-9._-]*)`)
	if positional.FindStringSubmatch(springConsoleLine) != nil {
		t.Fatalf("positional pattern must NOT match while ANSI escapes wrap the token")
	}
	if s := positional.FindStringSubmatch(stripANSI(springConsoleLine)); len(s) < 2 || s[1] != "lead-service" {
		t.Fatalf("after stripANSI the positional pattern must capture lead-service, got %v", s)
	}
}

// TestServiceMatcher_SpringConsoleAndMDCFixed is the reproduction turned GREEN:
// the shipped patterns + the Extract guards now yield the correct service for
// both real lines.
func TestServiceMatcher_SpringConsoleAndMDCFixed(t *testing.T) {
	m, errs := NewServiceMatcher(servicePatterns)
	if len(errs) != 0 {
		t.Fatalf("shipped patterns must compile cleanly, got %v", errs)
	}
	if got := m.Extract(springConsoleLine); got != "lead-service" {
		t.Errorf("Spring console line: Extract = %q, want %q", got, "lead-service")
	}
	if got := m.Extract(mdcBracketLine); got != "account-service" {
		t.Errorf("MDC bracket line: Extract = %q, want %q", got, "account-service")
	}
}

// TestServiceMatcher_LevelGuard proves the level guard never attributes a
// signal to a bare log level, and that a greedy positional pattern that
// captured a level falls through to the real service — with no regression to
// the existing key=value rule.
func TestServiceMatcher_LevelGuard(t *testing.T) {
	m, errs := NewServiceMatcher([]string{
		// A greedy positional rule that grabs the first token after the timestamp.
		`^\s*\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:[.,]\d{1,9})?\s+([A-Za-z][A-Za-z0-9._-]*)`,
		// A real service, later in the line, as a key=value pair.
		`(?i)\b(?:service|svc|app|component)\s*=\s*"?([A-Za-z0-9._-]+)`,
	})
	if len(errs) != 0 {
		t.Fatalf("compile errs: %v", errs)
	}

	// First token is the LEVEL: the guard skips it and the service= rule wins.
	if got := m.Extract(`2026-07-01 05:08:04.661 DEBUG service=account-service parsing`); got != "account-service" {
		t.Errorf("level guard: Extract = %q, want %q", got, "account-service")
	}
	// A non-level first token is still returned by the positional rule.
	if got := m.Extract(`2026-07-01 05:08:04.661 account-service is up`); got != "account-service" {
		t.Errorf("non-level positional: Extract = %q, want %q", got, "account-service")
	}
	// Every level word is refused.
	for _, lvl := range []string{"TRACE", "DEBUG", "INFO", "WARN", "WARNING", "ERROR", "FATAL", "warn", "Error"} {
		if logLevelRe.MatchString(lvl) != true {
			t.Errorf("logLevelRe should match level %q", lvl)
		}
	}
	// A service that merely CONTAINS a level substring is NOT a level.
	if logLevelRe.MatchString("error-service") {
		t.Errorf("logLevelRe must only match a BARE level, not %q", "error-service")
	}

	// No regression: a plain service=foo line still extracts foo.
	m2, _ := NewServiceMatcher([]string{`(?i)\b(?:service|svc|app|component)\s*=\s*"?([A-Za-z0-9._-]+)`})
	if got := m2.Extract(`time=... level=info service=foo msg="hi"`); got != "foo" {
		t.Errorf("key=value regression: Extract = %q, want %q", got, "foo")
	}
}

// TestStripANSI covers the escape-stripping helper directly, including the
// no-ESC fast path.
func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[34mlead-service\x1b[m", "lead-service"},
		{"\x1b[33;1m[thread]\x1b[0m rest", "[thread] rest"},
		{"plain text, no escapes", "plain text, no escapes"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripANSI(tc.in); got != tc.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
