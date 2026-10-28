// Package weborigin validates and compares browser-visible HTTP origins.
package weborigin

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Normalize validates and canonicalizes a browser-visible HTTP(S) origin. A
// root path is accepted and removed; all other URL components are rejected.
func Normalize(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("must be an absolute http/https origin: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if !parsed.IsAbs() || (scheme != "http" && scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("must be an absolute http/https origin without userinfo, path, query, or fragment")
	}

	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("must include a hostname")
	}
	port := parsed.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("contains an invalid port")
		}
	}
	if port == defaultPort(scheme) {
		port = ""
	}
	if port != "" {
		hostname = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		hostname = "[" + hostname + "]"
	}
	return scheme + "://" + hostname, nil
}

// Same reports whether candidate has the same normalized scheme, host, and
// effective port as expected. Referers may include a path and query; Origin
// values may contain only an empty or root path.
func Same(candidate, expected string, allowPath bool) bool {
	candidateURL, err := url.Parse(strings.TrimSpace(candidate))
	if err != nil || candidateURL.User != nil || candidateURL.Host == "" || candidateURL.Fragment != "" {
		return false
	}
	if !allowPath && (candidateURL.RawQuery != "" || (candidateURL.EscapedPath() != "" && candidateURL.EscapedPath() != "/")) {
		return false
	}
	expectedURL, err := url.Parse(expected)
	if err != nil || expectedURL.User != nil || expectedURL.Host == "" {
		return false
	}
	return strings.EqualFold(candidateURL.Scheme, expectedURL.Scheme) &&
		strings.EqualFold(candidateURL.Hostname(), expectedURL.Hostname()) &&
		effectivePort(candidateURL) == effectivePort(expectedURL)
}

func effectivePort(parsed *url.URL) string {
	if port := parsed.Port(); port != "" {
		return port
	}
	return defaultPort(strings.ToLower(parsed.Scheme))
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	if scheme == "http" {
		return "80"
	}
	return ""
}
