package utils

import "strings"

// The AWS pass runs on the first pass's output and replaces AWS
// credentials a person pastes: access key IDs, and the values of labelled
// secret access keys and session tokens. It only replaces spans with the
// marker and never touches a marker: a value is made only of the
// characters of a key, a secret or a token, which the marker's brackets
// are not. One linear scan: every attempt at a word start reads a bounded
// stretch, or matches and the scan moves past the match.

// awsLabel names an AWS secret value in the forms it is pasted: the
// credentials file, environment and PowerShell (aws_secret_access_key,
// AWS_SECRET_ACCESS_KEY), CLI and STS JSON (SecretAccessKey, SessionToken),
// YAML, and prose ("secret key: …"). session marks a session token.
type awsLabel struct {
	text    string
	session bool
}

var awsLabels = []awsLabel{
	{"aws_secret_access_key", false}, {"aws_session_token", true}, {"secret_access_key", false}, {"secretaccesskey", false},
	{"secret access key", false}, {"session_token", true}, {"sessiontoken", true}, {"session token", true},
	{"secret_key", false}, {"secret key", false},
}

// Shapes of AWS credentials.
const (
	awsKeyIDLength     = 20  // AKIA or ASIA and 16 more
	awsSecretKeyLength = 40  // a secret access key
	awsSessionTokenMin = 100 // a session token is far longer
	awsColonValueMin   = 16  // a value after "label:" must look like a token
)

// redactAWS replaces AWS credentials in s.
func redactAWS(s string) string {
	var out strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if !isWordByte(s[i]) || i > 0 && isWordByte(s[i-1]) {
			i++
			continue
		}
		start, end := awsKeyID(s, i)
		if end >= 0 {
			end = keyIDSecret(s, end)
		}
		if end < 0 {
			start, end = awsLabelledValue(s, i)
		}
		if end < 0 {
			for i < len(s) && isWordByte(s[i]) {
				i++
			}
			continue
		}
		if out.Len() == 0 {
			out.Grow(len(s))
		}
		out.WriteString(s[last:start])
		out.WriteString(redacted)
		last, i = end, end
	}
	if last == 0 {
		return s
	}
	out.WriteString(s[last:])
	return out.String()
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func isKeyIDByte(c byte) bool { return c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' }

// isSecretByte is a character of a secret access key or a session token.
func isSecretByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '/' || c == '+' || c == '='
}

func isQuote(c byte) bool { return c == '"' || c == '\'' || c == '`' }

func skipBlanks(s string, j int) int {
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	return j
}

// lineBreak returns the index after one line break at j (\n or \r\n) and
// its indentation, or -1.
func lineBreak(s string, j int) int {
	if j < len(s) && s[j] == '\r' {
		j++
	}
	if j >= len(s) || s[j] != '\n' {
		return -1
	}
	return skipBlanks(s, j+1)
}

// secretRun returns the end of the run of secret characters from j, read
// no further than limit.
func secretRun(s string, j, limit int) int {
	for j < len(s) && j < limit && isSecretByte(s[j]) {
		j++
	}
	return j
}

// awsKeyID matches an access key ID at i as a whole 20-character token, or
// as a token split by one line break whose joined text is exactly that. It
// returns the span to replace, or end -1.
func awsKeyID(s string, i int) (int, int) {
	if i+4 > len(s) || s[i:i+4] != "AKIA" && s[i:i+4] != "ASIA" {
		return 0, -1
	}
	j := i + 4
	for j < len(s) && j < i+awsKeyIDLength && isKeyIDByte(s[j]) {
		j++
	}
	if j < len(s) && isWordByte(s[j]) {
		return 0, -1
	}
	if j == i+awsKeyIDLength {
		return i, j
	}
	k := lineBreak(s, j)
	if k < 0 {
		return 0, -1
	}
	want := i + awsKeyIDLength - j
	end := k
	for end < len(s) && end-k < want && isKeyIDByte(s[end]) {
		end++
	}
	if end-k != want || end < len(s) && isWordByte(s[end]) {
		return 0, -1
	}
	return i, end
}

// keyIDSecret extends a key ID that ends at j over the secret access key
// pasted right after it ("AKIA…:secret", "AKIA…,secret", "AKIA… secret"):
// one ':', ',' or whitespace character, any blanks, then a strict
// 40-character secret token. Anything else leaves the key ID alone.
func keyIDSecret(s string, j int) int {
	k := j
	if k >= len(s) || s[k] != ':' && s[k] != ',' && s[k] != ' ' && s[k] != '\t' && s[k] != '\n' && s[k] != '\r' {
		return j
	}
	if s[k] == '\r' && k+1 < len(s) && s[k+1] == '\n' {
		k++
	}
	k = skipBlanks(s, k+1)
	end := secretRun(s, k, k+awsSecretKeyLength+1)
	if end-k != awsSecretKeyLength || end < len(s) && isWordByte(s[end]) || strings.Contains(s[k:end], "=") {
		return j
	}
	return end
}

// hasPrefixFold reports whether s has prefix at i, ignoring ASCII case.
func hasPrefixFold(s string, i int, prefix string) bool {
	return i+len(prefix) <= len(s) && strings.EqualFold(s[i:i+len(prefix)], prefix)
}

// awsLabelledValue matches an AWS label at i and returns the span of its
// value, or end -1.
func awsLabelledValue(s string, i int) (int, int) {
	for _, label := range awsLabels {
		if !hasPrefixFold(s, i, label.text) {
			continue
		}
		j := i + len(label.text)
		if j < len(s) && isWordByte(s[j]) {
			continue
		}
		if start, end := awsLabelValue(s, j, label.session); end >= 0 {
			return start, end
		}
	}
	return 0, -1
}

// awsLabelValue reads the value after a label ending at j.
//
// After '=' the value is the run of secret characters that follows,
// quoted or not. After ':' that run must be at least awsColonValueMin
// long, so prose such as "session token: ExpiredToken" stays. A secret
// access key wrapped over one line break is joined when the two parts make
// exactly 40 characters. Without a separator, after whitespace or the word
// "is", only a strict token counts: a 40-character secret access key, or a
// session token of awsSessionTokenMin or more characters.
func awsLabelValue(s string, j int, session bool) (int, int) {
	after := j
	if j < len(s) && isQuote(s[j]) {
		j++
	}
	j = skipBlanks(s, j)
	if j < len(s) && (s[j] == '=' || s[j] == ':') {
		separator := s[j]
		j = skipBlanks(s, j+1)
		if j < len(s) && isQuote(s[j]) {
			j++
		}
		end := secretRun(s, j, len(s))
		if end == j || separator == ':' && end-j < awsColonValueMin {
			return 0, -1
		}
		if !session {
			end = wrappedSecret(s, j, end)
		}
		return j, end
	}
	k := skipBlanks(s, after)
	if k == after {
		return 0, -1
	}
	if hasPrefixFold(s, k, "is") && k+2 < len(s) && (s[k+2] == ' ' || s[k+2] == '\t') {
		k = skipBlanks(s, k+2)
	}
	if k < len(s) && isQuote(s[k]) {
		k++
	}
	limit := k + awsSecretKeyLength + 1
	if session {
		limit = len(s)
	}
	end := secretRun(s, k, limit)
	if end < len(s) && (isSecretByte(s[end]) || isWordByte(s[end])) {
		return 0, -1
	}
	switch {
	case session && end-k >= awsSessionTokenMin:
		return k, end
	case !session && end-k == awsSecretKeyLength && !strings.Contains(s[k:end], "="):
		return k, end
	}
	return 0, -1
}

// wrappedSecret extends a secret that ends at a line break when the first
// run on the next line completes it to exactly 40 characters.
func wrappedSecret(s string, start, end int) int {
	length := end - start
	if length >= awsSecretKeyLength {
		return end
	}
	k := lineBreak(s, end)
	if k < 0 {
		return end
	}
	next := secretRun(s, k, k+awsSecretKeyLength)
	if length+next-k != awsSecretKeyLength || next < len(s) && (isSecretByte(s[next]) || isWordByte(s[next])) {
		return end
	}
	return next
}
