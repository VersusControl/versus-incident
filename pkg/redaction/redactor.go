package redaction

import (
	"fmt"
	"regexp"
)

type Redactor struct {
	rules []redactRule
}

type redactRule struct {
	name string
	re   *regexp.Regexp
	repl string
}

var defaultRules = []struct {
	name, pattern, replacement string
}{
	{"jwt", `eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`, ""},
	{"aws_key", `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, ""},
	{"openai_key", `\bsk-[A-Za-z0-9_\-]{20,}`, ""},
	{"slack_token", `\bxox[abcdeprs]-[A-Za-z0-9\-]{10,}`, ""},
	{"basic_auth", `(://)[^/\s:@]+:[^/\s@]+(@)`, `${1}<REDACTED:basic_auth>${2}`},
	{"bearer", `(?i)Authorization:\s*Bearer\s+[A-Za-z0-9._\-]+`, ""},
	{"password", `(?i)\b(?:password|passwd|pwd|secret|token|api[_-]?key)\s*[=:]\s*\S+`, ""},
	{"email", `[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`, ""},
	{"uuid", `\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`, ""},
	{"user_agent", `Mozilla/[0-9.]+\s*\([^)]*\)(?:[^"\n]*?(?:Gecko|Chrome|Safari|Firefox|Edg|Trident|OPR|MSIE)[^"\n]*)?`, ""},
}

var ipRules = []struct{ name, pattern string }{
	{"ipv4", `\b(?:\d{1,3}\.){3}\d{1,3}\b`},
	{"ipv6", `\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`},
}

func NewRedactor(redactIPs bool, extra []string) (*Redactor, []error) {
	redactor := &Redactor{}
	var errs []error
	add := func(name, pattern, replacement string) {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("redactor %q: %w", name, err))
			return
		}
		redactor.rules = append(redactor.rules, redactRule{name: name, re: compiled, repl: replacement})
	}
	for _, rule := range defaultRules {
		add(rule.name, rule.pattern, rule.replacement)
	}
	if redactIPs {
		for _, rule := range ipRules {
			add(rule.name, rule.pattern, "")
		}
	}
	for index, pattern := range extra {
		add(fmt.Sprintf("custom%d", index), pattern, "")
	}
	return redactor, errs
}

func (redactor *Redactor) Scrub(value string) string {
	if redactor == nil || value == "" {
		return value
	}
	for _, rule := range redactor.rules {
		replacement := rule.repl
		if replacement == "" {
			replacement = "<REDACTED:" + rule.name + ">"
		}
		value = rule.re.ReplaceAllString(value, replacement)
	}
	return value
}

func (redactor *Redactor) ScrubFields(fields map[string]interface{}) map[string]interface{} {
	if redactor == nil || fields == nil {
		return fields
	}
	clean := make(map[string]interface{}, len(fields))
	for key, value := range fields {
		clean[key] = redactor.scrubValue(value)
	}
	return clean
}

func (redactor *Redactor) scrubValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case string:
		return redactor.Scrub(typed)
	case []interface{}:
		clean := make([]interface{}, len(typed))
		for index, item := range typed {
			clean[index] = redactor.scrubValue(item)
		}
		return clean
	case map[string]interface{}:
		return redactor.ScrubFields(typed)
	default:
		return value
	}
}