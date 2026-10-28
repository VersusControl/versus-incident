package utils

import (
	"net/url"
	"strings"
	"unicode"
)

// SafeReturnTo accepts only local absolute paths, including a query string.
// Check the decoded path too, so intermediaries cannot turn escapes into an origin.
func SafeReturnTo(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return "/"
	}
	decoded, err := url.PathUnescape(value)
	if err != nil || strings.HasPrefix(decoded, "//") || strings.ContainsAny(decoded, "\\") || strings.IndexFunc(decoded, unicode.IsControl) >= 0 {
		return "/"
	}
	return value
}
