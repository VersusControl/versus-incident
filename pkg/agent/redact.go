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

	"github.com/VersusControl/versus-incident/pkg/redaction"
)

// Redactor scrubs sensitive substrings before any other component sees them.
// It is intentionally regex-based (not a full parser) — the goal is to make
// it operationally reasonable to send log content to an external LLM, not to
// be a perfect DLP solution. The rules live in pkg/redaction; this type keeps
// the agent's constructor and error messages on top of it.
type Redactor struct {
	inner *redaction.Redactor
}

// NewRedactor builds a Redactor from defaults + user-supplied extra patterns.
// Invalid extra patterns are skipped (with their compile error returned in
// the slice for the caller to log). User rules keep their position in extra
// for their name (custom0, custom1, ...) even when an earlier one is invalid.
func NewRedactor(redactIPs bool, extra []string) (*Redactor, []error) {
	var errs []error
	for i, p := range extra {
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("extra redactor[%d] %q: %w", i, p, err))
		}
	}
	inner, innerErrs := redaction.NewRedactor(redactIPs, extra)
	if len(innerErrs) != len(errs) {
		// The built-in rules always compile, so the shared package reports
		// exactly the invalid extras above; anything else is a broken build.
		errs = append(errs, innerErrs...)
	}
	return &Redactor{inner: inner}, errs
}

// Scrub returns s with every match of every rule replaced by
// `<REDACTED:<rule>>` (or the rule's own replacement template, when set, so a
// rule like basic_auth can keep the delimiters it only matched for context).
func (r *Redactor) Scrub(s string) string {
	if r == nil {
		return s
	}
	return r.inner.Scrub(s)
}

// ScrubFields recursively scrubs string values inside a fields map. Maps and
// slices are walked; non-string scalars are returned untouched, except under
// a field named like a secret (password, api_key, Authorization, ...), whose
// value is replaced whatever its type.
func (r *Redactor) ScrubFields(fields map[string]interface{}) map[string]interface{} {
	if r == nil {
		return fields
	}
	return r.inner.ScrubFields(fields)
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
