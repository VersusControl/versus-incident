package breaklogs

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Level returns the severity of one log line: "error", "warning", "info" or
// "debug". It reads, in order, a JSON level field (level, severity, or the
// one-letter MongoDB "s" field), the klog header, the first word of the line,
// and the first word after a leading application timestamp. A line with no
// recognisable level is "info". It runs once per shown line, so it scans by
// hand instead of using regular expressions.
func Level(text string) string {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	if strings.HasPrefix(trimmed, "{") {
		if level := jsonLevel(trimmed); level != "" {
			return level
		}
	}
	if level := klogLevel(trimmed); level != "" {
		return level
	}
	if level := normalizeLevel(firstToken(trimmed)); level != "" {
		return level
	}
	if rest, ok := skipTimestamp(trimmed); ok {
		if level := normalizeLevel(firstToken(rest)); level != "" {
			return level
		}
	}
	return "info"
}

func jsonLevel(text string) string {
	var object map[string]any
	if json.Unmarshal([]byte(text), &object) != nil {
		return ""
	}
	for _, key := range []string{"level", "severity"} {
		if value, ok := object[key].(string); ok {
			if level := normalizeLevel(value); level != "" {
				return level
			}
		}
	}
	// MongoDB structured logs carry the severity as a single letter in "s"
	// next to a "msg" field; other JSON may use "s" for something else.
	if value, ok := object["s"].(string); ok {
		if _, hasMsg := object["msg"]; hasMsg {
			return mongoLevel(value)
		}
	}
	return ""
}

func mongoLevel(value string) string {
	switch value {
	case "F", "E":
		return "error"
	case "W":
		return "warning"
	case "I":
		return "info"
	case "D", "D1", "D2", "D3", "D4", "D5":
		return "debug"
	}
	return ""
}

// klogLevel reads the klog header "Lmmdd hh:mm:ss", where L is I, W, E or F.
func klogLevel(text string) string {
	if len(text) < 14 || !inRange(text[1:3], 1, 12) || !inRange(text[3:5], 1, 31) || text[5] != ' ' || !clock(text[6:14]) {
		return ""
	}
	switch text[0] {
	case 'E', 'F':
		return "error"
	case 'W':
		return "warning"
	case 'I':
		return "info"
	}
	return ""
}

// firstToken returns the first word of text without surrounding brackets,
// colons or padding, so "[ERROR  ]", "warn:" and "\terror\r" all yield the
// bare level word. Any Unicode space separates words, as in strings.Fields.
func firstToken(text string) string {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	var token string
	if strings.HasPrefix(text, "[") {
		end := strings.IndexByte(text, ']')
		if end < 0 {
			end = len(text)
		}
		token = strings.TrimSpace(text[1:end])
		if space := strings.IndexFunc(token, unicode.IsSpace); space >= 0 {
			token = token[:space]
		}
	} else {
		end := strings.IndexFunc(text, unicode.IsSpace)
		if end < 0 {
			end = len(text)
		}
		token = text[:end]
	}
	return strings.Trim(token, "[]:")
}

// skipTimestamp removes a leading application timestamp, such as
// "2026-11-21T08:44:15.976Z", "2026-11-21 08:44:20,879" or
// "[2026-11-21 08:44:20:879]", and returns the rest of the line. The
// timestamp must be followed by whitespace or the end of the line.
func skipTimestamp(text string) (string, bool) {
	bracket := strings.HasPrefix(text, "[")
	s := text
	if bracket {
		s = s[1:]
	}
	if len(s) < 19 || !digits(s[0:4]) || s[4] != '-' || !digits(s[5:7]) || s[7] != '-' || !digits(s[8:10]) ||
		(s[10] != 'T' && s[10] != ' ') || !clock(s[11:19]) {
		return "", false
	}
	s = s[19:]
	if len(s) > 1 && (s[0] == '.' || s[0] == ',' || s[0] == ':') && isDigit(s[1]) {
		i := 1
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		s = s[i:]
	}
	s = skipZone(s)
	if bracket {
		if !strings.HasPrefix(s, "]") {
			return "", false
		}
		s = s[1:]
	}
	if first, _ := utf8.DecodeRuneInString(s); s != "" && !unicode.IsSpace(first) {
		return "", false
	}
	return s, true
}

// skipZone removes "Z", "+hh:mm", "+hhmm" or "-hh" right after the time.
func skipZone(s string) string {
	if strings.HasPrefix(s, "Z") {
		return s[1:]
	}
	if len(s) < 3 || (s[0] != '+' && s[0] != '-') || !digits(s[1:3]) {
		return s
	}
	rest := s[3:]
	if len(rest) >= 3 && rest[0] == ':' && digits(rest[1:3]) {
		return rest[3:]
	}
	if len(rest) >= 2 && digits(rest[0:2]) {
		return rest[2:]
	}
	return rest
}

func clock(s string) bool {
	return len(s) == 8 && inRange(s[0:2], 0, 23) && s[2] == ':' && inRange(s[3:5], 0, 59) && s[5] == ':' && inRange(s[6:8], 0, 59)
}

// inRange reports whether the two-digit field s is between low and high.
func inRange(s string, low, high int) bool {
	if len(s) != 2 || !digits(s) {
		return false
	}
	value := int(s[0]-'0')*10 + int(s[1]-'0')
	return value >= low && value <= high
}

func digits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func normalizeLevel(value string) string {
	switch strings.ToLower(value) {
	case "fatal", "critical", "error", "err", "panic":
		return "error"
	case "warn", "warning":
		return "warning"
	case "info", "information", "notice":
		return "info"
	case "debug", "trace":
		return "debug"
	}
	return ""
}
