package graylog

import "testing"

func TestParseScope(t *testing.T) {
	for _, query := range []string{"", "*", "level:ERROR", "level:3 AND service:api"} {
		if _, err := parseScope(query); err != nil {
			t.Errorf("valid scope %q: %v", query, err)
		}
	}
	for _, query := range []string{"service:api OR service:admin", "service:api AND (level:3)", "service:*", "service:api*", "service:/api/", "service:api/prod", "service:\"api prod\"", "service:api\nAND level:3", "service:api AND ", "service:api:other"} {
		if _, err := parseScope(query); err == nil {
			t.Errorf("unsupported scope %q accepted", query)
		}
	}
}
