package loki

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

var ErrUnsupportedScope = errors.New("loki: configured query is not a selector-only scope")
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
var matcherToken = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)(=~|!~|!=|=)("(?:[^"\\]|\\.)*")$`)

type matcher struct {
	name, operator, literal string
}

func parseSelector(query string) ([]matcher, error) {
	query = strings.TrimSpace(query)
	if len(query) < 2 || query[0] != '{' {
		return nil, ErrUnsupportedScope
	}
	quoted, escaped, end := false, false, -1
	for index := 1; index < len(query); index++ {
		switch {
		case escaped:
			escaped = false
		case quoted && query[index] == '\\':
			escaped = true
		case query[index] == '"':
			quoted = !quoted
		case !quoted && query[index] == '}':
			end = index
		}
		if end >= 0 {
			break
		}
	}
	if quoted || end != len(query)-1 || end < 1 {
		return nil, ErrUnsupportedScope
	}
	body := query[1:end]
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	var parts []string
	start := 0
	quoted, escaped = false, false
	for index := range body {
		switch {
		case escaped:
			escaped = false
		case quoted && body[index] == '\\':
			escaped = true
		case body[index] == '"':
			quoted = !quoted
		case !quoted && body[index] == ',':
			parts = append(parts, body[start:index])
			start = index + 1
		}
	}
	parts = append(parts, body[start:])
	result := make([]matcher, 0, len(parts))
	for _, part := range parts {
		match := matcherToken.FindStringSubmatch(strings.TrimSpace(part))
		if match == nil {
			return nil, ErrUnsupportedScope
		}
		value, err := strconv.Unquote(match[3])
		if err != nil {
			return nil, ErrUnsupportedScope
		}
		if match[2] == "=~" || match[2] == "!~" {
			if _, err := regexp.Compile(value); err != nil {
				return nil, ErrUnsupportedScope
			}
		}
		result = append(result, matcher{match[1], match[2], strconv.Quote(value)})
	}
	return result, nil
}

func renderSelector(matchers []matcher) string {
	parts := make([]string, len(matchers))
	for index, match := range matchers {
		parts[index] = match.name + match.operator + match.literal
	}
	return "{" + strings.Join(parts, ",") + "}"
}
