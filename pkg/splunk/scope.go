package splunk

import (
	"errors"
	"regexp"
	"strings"
)

var ErrUnsupportedScope = errors.New("splunk: configured search cannot be safely scoped")

var literal = regexp.MustCompile(`^[A-Za-z0-9_./@-]{1,128}$`)

var allowedFields = map[string]bool{
	"index": true, "source": true, "sourcetype": true, "host": true,
	"service": true, "severity": true, "level": true, "environment": true,
}

type clause struct{ field, value string }

func parseScope(search string) ([]clause, error) {
	search = strings.TrimSpace(search)
	search = strings.TrimPrefix(search, "search ")
	parts := strings.Split(search, " AND ")
	if len(parts) > 12 {
		return nil, ErrUnsupportedScope
	}
	result := make([]clause, 0, len(parts))
	indexes := 0
	for _, part := range parts {
		field, value, ok := strings.Cut(part, "=")
		if !ok || !allowedFields[field] || !literal.MatchString(value) {
			return nil, ErrUnsupportedScope
		}
		if field == "index" {
			indexes++
		}
		result = append(result, clause{field, value})
	}
	if indexes != 1 {
		return nil, ErrUnsupportedScope
	}
	return result, nil
}
