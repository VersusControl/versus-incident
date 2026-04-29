package agent

import (
	"fmt"
	"regexp"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// RegexRule is a compiled user-defined rule plus its severity tag.
type RegexRule struct {
	Name     string
	Severity string
	Pattern  *regexp.Regexp
}

// RegexMatcher is a small helper around a list of RegexRule plus an optional
// "default" pattern. It acts as the agent's pre-filter: only signals whose
// message matches at least one rule (named or default) are forwarded to the
// pattern miner / catalog. Set `regex.default_pattern: ".*"` to learn from
// every line.
type RegexMatcher struct {
	defaultRule *RegexRule
	rules       []RegexRule
}

// NewRegexMatcher compiles user-supplied rules. Compilation errors are
// reported but the matcher is still returned with the rules that did
// compile, so a single bad rule cannot disable the entire pipeline.
func NewRegexMatcher(cfg config.AgentRegexConfig) (*RegexMatcher, []error) {
	var errs []error
	m := &RegexMatcher{}

	if cfg.DefaultPattern != "" {
		re, err := regexp.Compile(cfg.DefaultPattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("regex.default_pattern %q: %w", cfg.DefaultPattern, err))
		} else {
			m.defaultRule = &RegexRule{Name: "default", Severity: "low", Pattern: re}
		}
	}

	for _, r := range cfg.Rules {
		if r.Pattern == "" || r.Name == "" {
			errs = append(errs, fmt.Errorf("regex rule with empty name or pattern: %+v", r))
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("regex rule %q: %w", r.Name, err))
			continue
		}
		m.rules = append(m.rules, RegexRule{Name: r.Name, Severity: r.Severity, Pattern: re})
	}
	return m, errs
}

// MatchResult is the (possibly empty) tag returned by Match.
type MatchResult struct {
	RuleName string // empty when no rule matched
	Severity string
	Default  bool // true when only the default pattern matched
}

// Matched reports whether at least one rule (named or default) hit. The
// worker uses this to decide whether a signal is interesting enough to learn
// from. To train on every line, set `regex.default_pattern: ".*"`.
func (m MatchResult) Matched() bool {
	return m.RuleName != ""
}

// Match runs explicit rules first (in declaration order — first hit wins),
// then falls back to the default pattern.
func (m *RegexMatcher) Match(message string) MatchResult {
	if m == nil || message == "" {
		return MatchResult{}
	}
	for _, r := range m.rules {
		if r.Pattern.MatchString(message) {
			return MatchResult{RuleName: r.Name, Severity: r.Severity}
		}
	}
	if m.defaultRule != nil && m.defaultRule.Pattern.MatchString(message) {
		return MatchResult{RuleName: m.defaultRule.Name, Severity: m.defaultRule.Severity, Default: true}
	}
	return MatchResult{}
}
