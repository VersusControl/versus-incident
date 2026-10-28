package graylog

import (
	"errors"
	"regexp"
	"strings"
)

var ErrUnsupportedScope = errors.New("graylog: configured query cannot be safely scoped")

var fieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]{0,63}$`)
var exactValue = regexp.MustCompile(`^[A-Za-z0-9_./@-]{1,128}$`)
var scopeValue = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,128}$`)

type clause struct {
	field string
	value string
}

func parseScope(query string) ([]clause, error) {
	query = strings.TrimSpace(query)
	if query == "" || query == "*" {
		return nil, nil
	}
	parts := strings.Split(query, " AND ")
	if len(parts) > 12 {
		return nil, ErrUnsupportedScope
	}
	result := make([]clause, 0, len(parts))
	for _, part := range parts {
		field, value, ok := strings.Cut(part, ":")
		if !ok || !fieldName.MatchString(field) || !scopeValue.MatchString(value) {
			return nil, ErrUnsupportedScope
		}
		result = append(result, clause{field: field, value: value})
	}
	return result, nil
}
