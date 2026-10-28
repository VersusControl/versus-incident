package breaklogs

import (
	"encoding/json"
	"strings"
)

func Level(text string) string {
	var object map[string]any
	if json.Unmarshal([]byte(text), &object) == nil {
		for _, key := range []string{"level", "severity"} {
			if value, ok := object[key].(string); ok {
				if level := normalizeLevel(value); level != "" {
					return level
				}
			}
		}
	}
	words := strings.Fields(text)
	if len(words) > 0 {
		if level := normalizeLevel(strings.Trim(words[0], "[]: ")); level != "" {
			return level
		}
	}
	return "info"
}

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
