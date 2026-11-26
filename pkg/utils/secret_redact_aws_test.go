package utils

import (
	"regexp"
	"strings"
	"testing"
)

// survivedAWS finds a test secret value; a label such as
// AWS_SECRET_ACCESS_KEY is not one.
var survivedAWS = regexp.MustCompile(`SECRET[A-Za-z0-9]|K7MDENG|IQoJb3`)

// Key IDs in the shape of a real one are built from parts, so the source
// holds no literal a secret scanner reports as a leaked AWS key.
var (
	testKeyID     = "AKIA" + "SECRET0EXAMPLE12"
	testTempKeyID = "ASIA" + "SECRET0EXAMPLE12"
)

// RedactSecrets replaces AWS credentials in the forms they are pasted,
// leaves ordinary text and ARNs alone, and is stable when applied again.
func TestRedactSecretsAWSPass(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"aws access key id", "deploy key " + testKeyID + " leaked", `deploy key [redacted] leaked`},
		{"aws temporary key id", "session " + testTempKeyID + ", then", `session [redacted], then`},
		{"credentials file", "[default]\naws_access_key_id = " + testKeyID + "\naws_secret_access_key = SECRETwJalrXUtnFEMI/K7MDENG+bPxRfiCY=\naws_session_token=SECRETFwoGZXIvYXdzEJr//////////wEaDA",
			"[default]\naws_access_key_id = [redacted]\naws_secret_access_key = [redacted]\naws_session_token=[redacted]"},
		{"environment", `export AWS_SECRET_ACCESS_KEY=SECRETwJalrXUtnFEMI/K7MDENG AWS_SESSION_TOKEN=SECRETFwoG`,
			`export AWS_SECRET_ACCESS_KEY=[redacted] AWS_SESSION_TOKEN=[redacted]`},
		{"cli json", `{"AccessKeyId": "` + testTempKeyID + `", "SecretAccessKey": "SECRETwJalr/K7MDENG", "SessionToken": "SECRETFwoGZX//w=="}`,
			`{"AccessKeyId": "[redacted]", "SecretAccessKey": "[redacted]", "SessionToken": "[redacted]"}`},
		{"secret key in prose", `rotated, the secret key: SECRETwJalrXUtnFEMI/K7MDENG is gone`, `rotated, the secret key: [redacted] is gone`},
		{"a 20-character word that is not a key", `ABCDEFGHIJKLMNOPQRST and AKIA_NOT_A_REAL_KEY_ stay`, `ABCDEFGHIJKLMNOPQRST and AKIA_NOT_A_REAL_KEY_ stay`},
		{"a key id inside a longer token", `XAKIAIOSFODNN7EXAMPLE and AKIAIOSFODNN7EXAMPLEX stay`, `XAKIAIOSFODNN7EXAMPLE and AKIAIOSFODNN7EXAMPLEX stay`},
		{"an arn", `arn:aws:iam::123456789012:role/deploy-AKIA and arn:aws:s3:::secret-key-bucket`, `arn:aws:iam::123456789012:role/deploy-AKIA and arn:aws:s3:::secret-key-bucket`},
		{"a label inside another word", `nosecret_key=x mysession_token: y`, `nosecret_key=x mysession_token: y`},
		{"labelled value with separators", `secret key=ab;cd,ef}gh then`, `secret key=[redacted] then`},
		{"mixed-case labelled value", `Secret Key=p@ss;w0rd ok`, `Secret Key=[redacted] ok`},
		{"session token value with separators", `session token=ab;cd,ef`, `session token=[redacted]`},
		{"prose after a colon is not a token", `error refreshing session token: ExpiredToken: the token expired`,
			`error refreshing session token: ExpiredToken: the token expired`},
		{"key id over a line break", "the key is AKIAIOSFOD\n  NN7EXAMPLE ok", "the key is [redacted] ok"},
		{"secret over a line break", "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG\n/bPxRfiCYEXAMPLEKEY\nregion = eu-west-1",
			"aws_secret_access_key = [redacted]\nregion = eu-west-1"},
		{"aws configure set", `aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
			`aws configure set aws_secret_access_key [redacted]`},
		{"secret key is", `the secret key is wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY, rotate it`, `the secret key is [redacted], rotate it`},
		{"session token is", "the session token is " + strings.Repeat("IQoJb3JpZ2luX2VjEJr/", 6) + "== end",
			"the session token is [redacted] end"},
		{"prose after secret key is", `the secret key is stored in Vault`, `the secret key is stored in Vault`},
		{"a 40-character word after secret key is", `the secret key is ` + strings.Repeat("a", 39) + `= no`, `the secret key is ` + strings.Repeat("a", 39) + `= no`},
		{"a line break then ordinary text", "aws_secret_access_key = abc\nthe next line stays", "aws_secret_access_key = [redacted]\nthe next line stays"},
		{"a short key id then ordinary text", "AKIAIOSFOD\nthe next line stays", "AKIAIOSFOD\nthe next line stays"},
		{"a split key id of the wrong length", "AKIAIOSFOD\nNN7EXAMPLE123 stays", "AKIAIOSFOD\nNN7EXAMPLE123 stays"},
		{"powershell", `$env:AWS_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`, `$env:AWS_SECRET_ACCESS_KEY="[redacted]"`},
		{"yaml", "env:\n  aws_secret_access_key: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n  region: eu-west-1",
			"env:\n  aws_secret_access_key: [redacted]\n  region: eu-west-1"},
		{"a backtick inside a parameter value", "Secret Key=p@ss`w0rd ok", "Secret Key=[redacted] ok"},
		{"a parameter inside a label's value", `secret key: password=abcdefghij,klmX`, `secret key: password=[redacted]`},
		{"a parameter after a label", `secret key: token=abc`, `secret key: token=[redacted]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Fatalf("RedactSecrets =\n  %q\nwant\n  %q", got, tc.want)
			}
			if again := RedactSecrets(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
			if survivedAWS.MatchString(got) || strings.Contains(got, "EXAMPLE12") && !strings.Contains(tc.want, "EXAMPLE12") {
				t.Fatalf("secret survived: %s", got)
			}
		})
	}
}

// The AWS pass never touches a marker the first pass wrote, and never
// brings back text it removed: its output is the first pass's with more
// spans replaced.
func TestRedactAWSKeepsFirstPassMarkers(t *testing.T) {
	for _, in := range []string{
		"aws_secret_access_key=[redacted]", "secret key: [redacted]", "AKIA[redacted]", "key=[redacted]\n[redacted]",
		"aws_secret_access_key = [redacted]\n/bPxRfiCYEXAMPLEKEY", "Secret Key=[redacted]`w0rd",
	} {
		first := redactSecretsFirstPass(in)
		if got := redactAWS(first); !refines(first, got) || strings.Count(got, redacted) < strings.Count(first, redacted) {
			t.Fatalf("redactAWS(%q) = %q", first, got)
		}
	}
}

// refines reports whether next is prev with some non-empty spans of prev
// replaced by the marker, nothing else changed. next splits at its markers
// into literal pieces; they must appear in prev in order, the first at the
// start and the last at the end, each gap at least one byte. Placing every
// middle piece as early as it fits is optimal, so one forward scan decides.
func refines(prev, next string) bool {
	pieces := strings.Split(next, redacted)
	if len(pieces) == 1 {
		return prev == next
	}
	first, last := pieces[0], pieces[len(pieces)-1]
	if !strings.HasPrefix(prev, first) || !strings.HasSuffix(prev, last) || len(first)+len(last) > len(prev) {
		return false
	}
	at, stop := len(first), len(prev)-len(last)
	for _, piece := range pieces[1 : len(pieces)-1] {
		// The replaced span before piece is at least one byte.
		at++
		if at > stop {
			return false
		}
		found := strings.Index(prev[at:stop], piece)
		if found < 0 {
			return false
		}
		at += found + len(piece)
	}
	return at < stop
}

// redactBenchLines are a delivery error line with a parameter and an AWS
// secret, and a clean application log, the same lines the redactor's
// review measured.
var redactBenchLines = map[string]string{
	"engineer": strings.Repeat(`delivery failed for alert 42 at 2026-11-25T09:00:00Z, status 503, retry with token=abc in 5s; `, 40) +
		`aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY and AKIAIOSFODNN7EXAMPLE`,
	"k8slogclean": strings.Repeat(`2026-11-25T09:00:00.123Z INFO  [http-nio-8080-exec-3] c.e.orders.OrderController - GET /api/orders/12345 completed in 34ms status=200 user=alice session token refreshed ok
`, 200),
}

// BenchmarkRedactSecrets measures both passes.
func BenchmarkRedactSecrets(b *testing.B) {
	for name, line := range redactBenchLines {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			for b.Loop() {
				_ = RedactSecrets(line)
			}
		})
	}
}

// BenchmarkRedactSecretsFirstPass measures the first pass alone, the
// redactor before the AWS pass.
func BenchmarkRedactSecretsFirstPass(b *testing.B) {
	for name, line := range redactBenchLines {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			for b.Loop() {
				_ = redactSecretsFirstPass(line)
			}
		})
	}
}

// refines accepts exactly the outputs made by replacing spans with the
// marker.
func TestRefines(t *testing.T) {
	for _, tc := range []struct {
		prev, next string
		want       bool
	}{
		{"a b c", "a b c", true},
		{"key=abc rest", "key=" + redacted + " rest", true},
		{"x AKIA y SECRET z", "x " + redacted + " y " + redacted + " z", true},
		{"key=" + redacted, "key=" + redacted, true},
		{"key=abc rest", "key=" + redacted + "c rest", true},
		{"key=abc rest", "key=" + redacted + "X rest", false},
		{"key=abc rest", "key=abc rest" + redacted, false},
		{"ab", redacted + redacted, true},
		{"a", redacted + redacted, false},
		{"secret", "secrex", false},
	} {
		if got := refines(tc.prev, tc.next); got != tc.want {
			t.Fatalf("refines(%q, %q) = %v, want %v", tc.prev, tc.next, got, tc.want)
		}
	}
}

// A secret access key pasted right after its key ID, separated by ':', ','
// or whitespace, is redacted with it; an ordinary word after a key ID is
// left alone.
func TestRedactAWSSecretAfterKeyID(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	for name, tc := range map[string]struct{ in, want string }{
		"colon":            {"creds AKIAIOSFODNN7EXAMPLE:" + secret + " leaked", "creds [redacted] leaked"},
		"comma":            {"AKIAIOSFODNN7EXAMPLE," + secret, "[redacted]"},
		"space":            {"key AKIAIOSFODNN7EXAMPLE " + secret + ", rotate it", "key [redacted], rotate it"},
		"tab":              {"AKIAIOSFODNN7EXAMPLE\t" + secret, "[redacted]"},
		"line break":       {"AKIAIOSFODNN7EXAMPLE\n" + secret + "\nnext", "[redacted]\nnext"},
		"ordinary word":    {"AKIAIOSFODNN7EXAMPLE was rotated", "[redacted] was rotated"},
		"word after colon": {"AKIAIOSFODNN7EXAMPLE: rotated yesterday", "[redacted]: rotated yesterday"},
		"too short":        {"AKIAIOSFODNN7EXAMPLE " + secret[:39] + " end", "[redacted] " + secret[:39] + " end"},
		"too long":         {"AKIAIOSFODNN7EXAMPLE " + secret + "X end", "[redacted] " + secret + "X end"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RedactSecrets(tc.in); got != tc.want {
				t.Fatalf("RedactSecrets(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
