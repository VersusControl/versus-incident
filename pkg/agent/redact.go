// Package agent contains the AI agent mode pipeline:
//
//	SignalSource → Redact → Detector pipeline (Regex → Miner → Catalog →
//	Frequency) → (training: persist | shadow: log | detect: AI → Incident)
//
// Training mode is end-to-end. Shadow and detect honor the same
// classification path; the AI analyzer call in detect mode is wired up
// separately from the worker.
package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// Redactor scrubs sensitive substrings before any other component sees them.
// It is intentionally regex-based (not a full parser) — the goal is to make
// it operationally reasonable to send log content to an external LLM, not to
// be a perfect DLP solution.
type Redactor struct {
	rules []redactRule
}

type redactRule struct {
	name string
	re   *regexp.Regexp
	// repl is an optional replacement template ($1 / ${1} capture refs). When
	// empty the whole match is replaced by the fixed `<REDACTED:name>` token;
	// when set it lets a rule preserve surrounding delimiters it had to match
	// for context (e.g. basic_auth keeps the `://` and `@host` boundary).
	repl string
}

// Default redaction patterns. Order matters — longer / more specific patterns
// run first so an email isn't partially eaten by an IP rule.
var defaultRedactors = []struct {
	name    string
	pattern string
	repl    string // optional replacement template; empty ⇒ whole-match token
}{
	// JSON web tokens (header.payload.signature)
	{name: "jwt", pattern: `eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`},
	// AWS access keys (AKIA... / ASIA...)
	{name: "aws_key", pattern: `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`},
	// OpenAI-style API keys (sk-..., incl. project keys sk-proj-...). The
	// {20,} floor keeps short "sk-foo" identifiers from matching.
	{name: "openai_key", pattern: `\bsk-[A-Za-z0-9_\-]{20,}`},
	// Slack tokens: bot (xoxb), user (xoxp), app (xoxa), refresh (xoxr),
	// config (xoxc/xoxs), export (xoxe), legacy/cookie (xoxd). The letter
	// class deliberately EXCLUDES "o" so a near-miss like "xoxo-…" stays
	// untouched. The {10,} floor avoids matching a bare prefix.
	{name: "slack_token", pattern: `\bxox[abcdeprs]-[A-Za-z0-9\-]{10,}`},
	// Basic-auth credentials embedded in a URL (scheme://user:pass@host).
	// The `://` and the trailing `@` are matched only for context and put back
	// via the replacement template, so the credentials are fully scrubbed while
	// the separators survive: `://user:pass@host` → `://<REDACTED:basic_auth>@host`.
	{name: "basic_auth", pattern: `(://)[^/\s:@]+:[^/\s@]+(@)`, repl: `${1}<REDACTED:basic_auth>${2}`},
	// Bearer / Authorization headers
	{name: "bearer", pattern: `(?i)Authorization:\s*Bearer\s+[A-Za-z0-9._\-]+`},
	// Generic password=... / token=...
	{name: "password", pattern: `(?i)\b(?:password|passwd|pwd|secret|token|api[_-]?key)\s*[=:]\s*\S+`},
	// Email addresses
	{name: "email", pattern: `[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`},
	// UUIDs
	{name: "uuid", pattern: `\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`},
	// User-Agent strings
	{name: "user_agent", pattern: `Mozilla/[0-9.]+\s*\([^)]*\)(?:[^"\n]*?(?:Gecko|Chrome|Safari|Firefox|Edg|Trident|OPR|MSIE)[^"\n]*)?`},
}

// IPv4/IPv6 redactor — opt-in because it removes a lot of useful context.
var ipRedactors = []struct {
	name    string
	pattern string
}{
	{"ipv4", `\b(?:\d{1,3}\.){3}\d{1,3}\b`},
	// Simplified IPv6 (matches most real-world addresses)
	{"ipv6", `\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`},
}

// NewRedactor builds a Redactor from defaults + user-supplied extra patterns.
// Invalid extra patterns are skipped (with their compile error returned in
// the slice for the caller to log).
func NewRedactor(redactIPs bool, extra []string) (*Redactor, []error) {
	var errs []error
	r := &Redactor{}

	for _, d := range defaultRedactors {
		re, err := regexp.Compile(d.pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("default redactor %q: %w", d.name, err))
			continue
		}
		r.rules = append(r.rules, redactRule{name: d.name, re: re, repl: d.repl})
	}
	if redactIPs {
		for _, d := range ipRedactors {
			re, err := regexp.Compile(d.pattern)
			if err != nil {
				errs = append(errs, fmt.Errorf("ip redactor %q: %w", d.name, err))
				continue
			}
			r.rules = append(r.rules, redactRule{name: d.name, re: re})
		}
	}
	for i, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("extra redactor[%d] %q: %w", i, p, err))
			continue
		}
		r.rules = append(r.rules, redactRule{name: fmt.Sprintf("custom%d", i), re: re})
	}
	return r, errs
}

// Scrub returns s with every match of every rule replaced by
// `<REDACTED:<rule>>` (or the rule's own replacement template, when set, so a
// rule like basic_auth can keep the delimiters it only matched for context).
func (r *Redactor) Scrub(s string) string {
	if r == nil || len(r.rules) == 0 || s == "" {
		return s
	}
	for _, rule := range r.rules {
		repl := rule.repl
		if repl == "" {
			repl = "<REDACTED:" + rule.name + ">"
		}
		s = rule.re.ReplaceAllString(s, repl)
	}
	return s
}

// ScrubFields recursively scrubs string values inside a fields map. Maps and
// slices are walked; non-string scalars are returned untouched.
func (r *Redactor) ScrubFields(fields map[string]interface{}) map[string]interface{} {
	if r == nil || fields == nil {
		return fields
	}
	out := make(map[string]interface{}, len(fields))
	for k, v := range fields {
		out[k] = r.scrubValue(v)
	}
	return out
}

func (r *Redactor) scrubValue(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return r.Scrub(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, item := range t {
			out[i] = r.scrubValue(item)
		}
		return out
	case map[string]interface{}:
		return r.ScrubFields(t)
	default:
		return v
	}
}

// stripControlChars normalizes whitespace before pattern mining — collapses
// runs of whitespace and removes common log noise so structurally-identical
// messages don't end up in different clusters because of formatting.
func stripControlChars(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	// collapse runs of spaces
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}
