package agent

import (
	"fmt"
	"regexp"
)

// ServiceMatcher extracts a service name from a log message using an ordered
// list of regexes. The first pattern that matches wins; the FIRST capture
// group is returned. A nil/empty matcher returns "" — service detection is
// off and every signal is attributed to "_unknown" in the worker. There is
// no built-in default list: operators who want service detection MUST
// configure `agent.service_patterns` (or set `AGENT_SERVICE_PATTERNS`).
type ServiceMatcher struct {
	patterns []*regexp.Regexp
}

// NewServiceMatcher compiles the supplied regexes. Bad patterns are reported
// in the returned error slice but do not prevent the matcher from being
// built with whatever did compile, so a single typo cannot disable the
// entire pipeline. An empty/nil `patterns` list yields a matcher whose
// Extract always returns "" (service detection disabled).
func NewServiceMatcher(patterns []string) (*ServiceMatcher, []error) {
	var errs []error
	m := &ServiceMatcher{}
	for i, p := range patterns {
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("service_patterns[%d] %q: %w", i, p, err))
			continue
		}
		if re.NumSubexp() < 1 {
			errs = append(errs, fmt.Errorf("service_patterns[%d] %q: missing capture group", i, p))
			continue
		}
		m.patterns = append(m.patterns, re)
	}
	return m, errs
}

// Extract returns the first capture group of the first matching pattern, or
// "" when nothing matches.
func (m *ServiceMatcher) Extract(message string) string {
	if m == nil || message == "" {
		return ""
	}
	for _, re := range m.patterns {
		sub := re.FindStringSubmatch(message)
		if len(sub) >= 2 && sub[1] != "" {
			return sub[1]
		}
	}
	return ""
}
