package utils

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// Delivery and on-call errors become text that is stored on incidents,
// returned by the API (including the unauthenticated intake response) and
// logged. Go's HTTP client puts the full request URL in its errors, and several
// channels carry their credential in that URL: the Telegram bot token in the
// path, the Teams Power Automate signature in the query, the Lark hook token
// in the path. RedactSecrets and SafeError strip those before the text leaves
// the process.

var (
	// A URL with a scheme, up to the first character that cannot be part of
	// it in an error message (whitespace, quotes, angle brackets).
	urlInTextRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>` + "`" + `]+`)
	// Telegram-style bot credential: bot<id>:<token>, as a path segment or
	// in free text.
	botTokenRE = regexp.MustCompile(`\bbot[A-Za-z0-9_\-]*:[A-Za-z0-9_\-]+`)
	// Credential-bearing query or form values outside a full URL.
	secretParamRE = regexp.MustCompile(`(?i)\b(sig|signature|token|access_token|key|api_key|apikey|password|secret|code)=[^&\s"'<>]+`)
)

const redacted = "[redacted]"

// RedactSecrets returns s with every URL cut to scheme://host (no userinfo,
// path, query or fragment), Telegram-style bot<id>:<token> credentials
// replaced, and sig=/token=/key= style values replaced. It is idempotent.
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	s = urlInTextRE.ReplaceAllStringFunc(s, redactURL)
	s = botTokenRE.ReplaceAllString(s, "bot"+redacted)
	s = secretParamRE.ReplaceAllString(s, "${1}="+redacted)
	return s
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		scheme, _, _ := strings.Cut(raw, "://")
		return scheme + "://" + redacted
	}
	if u.Host == "" {
		return u.Scheme + "://" + redacted
	}
	return u.Scheme + "://" + u.Host
}

// safeError carries the redacted text of an error while keeping the original
// in the chain for errors.Is / errors.As.
type safeError struct {
	msg string
	err error
}

func (e *safeError) Error() string { return e.msg }
func (e *safeError) Unwrap() error { return e.err }

// SafeError returns err with its message passed through RedactSecrets, so
// anything that formats it (storage, API responses, logs, %w wrapping) gets
// the redacted text. A *url.Error is reduced to its operation, host and
// cause. Nil stays nil, and an already-safe error is returned as is.
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	var already *safeError
	if errors.As(err, &already) && already == err {
		return err
	}
	return &safeError{msg: RedactSecrets(err.Error()), err: err}
}

// SafeErrorText is SafeError(err).Error(), or "" for a nil error.
func SafeErrorText(err error) string {
	if err == nil {
		return ""
	}
	return SafeError(err).Error()
}
