package breaklogs

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Level returns the severity of one log line: "error", "warning", "info" or
// "debug". It reads, in order, a JSON level field (level, severity, the
// one-letter MongoDB "s" field, or the pino/bunyan numeric level), the klog
// header, the first word of the line, and the line after a leading
// application timestamp: its first word, an nginx "[level]", or a Python
// logging " - name - LEVEL - " field. Last it reads a logfmt "level=" or
// "lvl=" key on a line made of key=value pairs. A line with no recognisable
// level is "info". It runs once per shown line, so it scans by hand instead
// of using regular expressions.
func Level(text string) string {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	if strings.HasPrefix(trimmed, "{") {
		// Words inside a JSON line belong to its values, so only the JSON
		// fields decide its level.
		if level := jsonLevel(trimmed); level != "" {
			return level
		}
		return "info"
	}
	if level := klogLevel(trimmed); level != "" {
		return level
	}
	if level := normalizeLevel(firstToken(trimmed)); level != "" {
		return level
	}
	if rest, slashDate, ok := skipTimestamp(trimmed); ok {
		if level := normalizeLevel(firstToken(rest)); level != "" {
			return level
		}
		if slashDate {
			if level := nginxLevel(rest); level != "" {
				return level
			}
		}
		if level := pythonLevel(rest); level != "" {
			return level
		}
		if level := logfmtLevel(rest); level != "" {
			return level
		}
	}
	if level := logfmtLevel(trimmed); level != "" {
		return level
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
	// pino and bunyan write the level as a number, and some shippers turn
	// that number into a string. A bare number named "level" may mean
	// anything (a zoom or game level), so it is read only next to a field
	// both loggers always write.
	if !pinoLike(object) {
		return ""
	}
	switch value := object["level"].(type) {
	case float64:
		if level := numericLevel(value); level != "" {
			return level
		}
	case string:
		if number, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			if level := numericLevel(number); level != "" {
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

// pinoLike reports whether a JSON object carries a field pino and bunyan
// write on every line: the message, the time, bunyan's format version, or
// the process and host.
func pinoLike(object map[string]any) bool {
	for _, key := range []string{"msg", "time", "v", "pid", "hostname"} {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

// numericLevel maps the pino and bunyan level numbers. Other numbers are
// left unread, because a JSON "level" number may mean something else.
func numericLevel(value float64) string {
	switch value {
	case 60, 50:
		return "error"
	case 40:
		return "warning"
	case 30:
		return "info"
	case 20, 10:
		return "debug"
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
// "2026-11-21T08:44:15.976Z", "2026-11-21 08:44:20,879",
// "[2026-11-21 08:44:20:879]" or nginx's "2026/11/21 08:44:20", and returns
// the rest of the line and whether the date used slashes. The timestamp must
// be followed by whitespace or the end of the line.
func skipTimestamp(text string) (string, bool, bool) {
	bracket := strings.HasPrefix(text, "[")
	s := text
	if bracket {
		s = s[1:]
	}
	if len(s) < 19 || !digits(s[0:4]) || !digits(s[5:7]) || !digits(s[8:10]) || !clock(s[11:19]) {
		return "", false, false
	}
	dash := s[4] == '-' && s[7] == '-' && (s[10] == 'T' || s[10] == ' ')
	slash := s[4] == '/' && s[7] == '/' && s[10] == ' '
	if !dash && !slash {
		return "", false, false
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
			return "", false, false
		}
		s = s[1:]
	}
	if first, _ := utf8.DecodeRuneInString(s); s != "" && !unicode.IsSpace(first) {
		return "", false, false
	}
	return s, slash, true
}

// nginxLevel reads the bracketed level nginx writes after its timestamp,
// including the syslog words nginx uses for its most severe entries. These
// words are read only here: as the first word of an ordinary line, "alert"
// is more often a message than a level.
func nginxLevel(rest string) string {
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	if !strings.HasPrefix(rest, "[") {
		return ""
	}
	switch strings.ToLower(firstToken(rest)) {
	case "emerg", "alert", "crit":
		return "error"
	}
	return ""
}

// pythonLevel reads the level of Python's logging format
// "asctime - name - LEVEL - message", or "asctime - LEVEL - message", given
// the text after the timestamp. The level is a whole " - " separated field.
// A first field that is a level word is the level: logger names are module
// names, while a message often holds " - " and a level-like word, as in
// "ts - INFO - Warning - disk ok".
func pythonLevel(rest string) string {
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	if !strings.HasPrefix(rest, "- ") {
		return ""
	}
	fields := strings.SplitN(rest[2:], " - ", 3)
	if len(fields) < 2 {
		return ""
	}
	if level := normalizeLevel(strings.TrimSpace(fields[0])); level != "" {
		return level
	}
	if len(fields) >= 3 {
		return normalizeLevel(strings.TrimSpace(fields[1]))
	}
	return ""
}

// logfmtScanLimit bounds how much of a logfmt line is scanned for the level
// key; loggers write it near the start.
const logfmtScanLimit = 4096

// logfmtLevel reads a "level=" or "lvl=" key, with a bare or double-quoted
// value, from a logfmt line such as logrus's
// `time="2026-11-21T08:44:20Z" level=error msg="push failed"`. The line must
// start with a key=value pair, so a sentence that happens to contain
// "level=error" is not read, and quoted values are skipped whole, so a key
// written inside a message is not read either.
func logfmtLevel(text string) string {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	if len(text) > logfmtScanLimit {
		text = text[:logfmtScanLimit]
	}
	first := true
	for text != "" {
		keyEnd := 0
		for keyEnd < len(text) && isKeyByte(text[keyEnd]) {
			keyEnd++
		}
		if keyEnd == 0 || keyEnd == len(text) || text[keyEnd] != '=' {
			if first {
				return ""
			}
			text = skipLogfmtWord(text)
			continue
		}
		first = false
		key := text[:keyEnd]
		value, rest := logfmtValue(text[keyEnd+1:])
		if strings.EqualFold(key, "level") || strings.EqualFold(key, "lvl") {
			return normalizeLevel(value)
		}
		text = strings.TrimLeftFunc(rest, unicode.IsSpace)
	}
	return ""
}

func isKeyByte(b byte) bool {
	return b == '_' || b == '.' || b == '-' || isDigit(b) || (b|0x20 >= 'a' && b|0x20 <= 'z')
}

// logfmtValue splits a bare or double-quoted value from the rest of the line.
func logfmtValue(text string) (string, string) {
	if strings.HasPrefix(text, `"`) {
		for i := 1; i < len(text); i++ {
			switch text[i] {
			case '\\':
				i++
			case '"':
				return text[1:i], text[i+1:]
			}
		}
		return text[1:], ""
	}
	end := strings.IndexFunc(text, unicode.IsSpace)
	if end < 0 {
		return text, ""
	}
	return text[:end], text[end:]
}

// skipLogfmtWord drops one word that is not a key=value pair, skipping a
// quoted span whole.
func skipLogfmtWord(text string) string {
	if strings.HasPrefix(text, `"`) {
		_, rest := logfmtValue(text)
		return strings.TrimLeftFunc(rest, unicode.IsSpace)
	}
	end := strings.IndexFunc(text, unicode.IsSpace)
	if end < 0 {
		return ""
	}
	return strings.TrimLeftFunc(text[end:], unicode.IsSpace)
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
