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
}

// Default redaction patterns. Order matters — longer / more specific patterns
// run first so an email isn't partially eaten by an IP rule.
var defaultRedactors = []struct {
	name    string
	pattern string
}{
	// JSON web tokens (header.payload.signature)
	{"jwt", `eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`},
	// AWS access keys (AKIA... / ASIA...)
	{"aws_key", `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`},
	// Bearer / Authorization headers
	{"bearer", `(?i)Authorization:\s*Bearer\s+[A-Za-z0-9._\-]+`},
	// Generic password=... / token=...
	{"password", `(?i)\b(?:password|passwd|pwd|secret|token|api[_-]?key)\s*[=:]\s*\S+`},
	// Email addresses
	{"email", `[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`},
	// UUIDs
	{"uuid", `\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`},
	// User-Agent strings
	{"user_agent", `Mozilla/[0-9.]+\s*\([^)]*\)(?:[^"\n]*?(?:Gecko|Chrome|Safari|Firefox|Edg|Trident|OPR|MSIE)[^"\n]*)?`},
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
		r.rules = append(r.rules, redactRule{name: d.name, re: re})
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
// `<REDACTED:<rule>>`.
func (r *Redactor) Scrub(s string) string {
	if r == nil || len(r.rules) == 0 || s == "" {
		return s
	}
	for _, rule := range r.rules {
		token := "<REDACTED:" + rule.name + ">"
		s = rule.re.ReplaceAllString(s, token)
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
