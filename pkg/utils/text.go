package utils

import "strings"

// NormalizeSeverity coerces a free-form severity string into one of the
// canonical alert severities (`critical`, `high`, `medium`, `low`).
// Unknown / empty values fall back to "medium".
func NormalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return "medium"
	}
}

// SeverityRank maps a canonical severity to an orderable rank (critical
// highest). Unknown / empty severities rank lowest (0) so they never act
// as a floor over a real AI-assigned severity.
func SeverityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// ExtractJSONObject pulls the first balanced {...} block out of s.
// Returns "" when no balanced object is present.
//
// This is intentionally simpler than a full JSON tokenizer — it is
// designed for tolerating LLM-style output that may wrap a single
// JSON object in code fences or preamble. A depth counter with
// string-escape handling is enough for the degraded-output cases we
// care about; nested objects inside string literals do not throw it
// off.
func ExtractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// OneLine collapses newlines into spaces and truncates to maxLen
// runes (well, bytes — callers pass log lines that are ASCII-dominant).
// A trailing ellipsis "…" is appended on truncation. maxLen <= 0
// disables truncation.
func OneLine(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if maxLen > 0 && len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}
