package utils

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"token in the URL path", `Post "http://127.0.0.1:1/botFAKE123:SECRETTOKEN/sendMessage": dial tcp 127.0.0.1:1: connect: connection refused`,
			`Post "http://127.0.0.1:1": dial tcp 127.0.0.1:1: connect: connection refused`},
		{"sig in the URL query", `Post "https://prod.westus.logic.azure.com/workflows/abc/triggers/manual/paths/invoke?api-version=2016-06-01&sig=SECRETSIG": EOF`,
			`Post "https://prod.westus.logic.azure.com": EOF`},
		{"hook token in the path", `Post "https://open.larksuite.com/open-apis/bot/v2/hook/SECRETHOOK": timeout`,
			`Post "https://open.larksuite.com": timeout`},
		{"userinfo", `Post "https://admin:SECRETPASS@acme.service-now.com/api/now/table/incident": EOF`,
			`Post "https://acme.service-now.com": EOF`},
		{"bot token in free text", `telegram rejected bot123456:SECRETTOKEN`, `telegram rejected bot[redacted]`},
		{"sig and token values in free text", `retry with sig=SECRETSIG&token=SECRETTOK key=SECRETKEY`, `retry with sig=[redacted]&token=[redacted] key=[redacted]`},
		{"plain text unchanged", `lark API returned non-200 status code: 400, body: bad request`, `lark API returned non-200 status code: 400, body: bad request`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Fatalf("RedactSecrets =\n  %s\nwant\n  %s", got, tc.want)
			}
			if again := RedactSecrets(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
			if strings.Contains(got, "SECRET") {
				t.Fatalf("secret survived: %s", got)
			}
		})
	}
}

// A real *url.Error, wrapped the way channel providers wrap it, comes out
// redacted while staying matchable with errors.As.
func TestSafeErrorRedactsURLErrorAndKeepsTheChain(t *testing.T) {
	ue := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/botFAKE123:SECRETTOKEN/sendMessage", Err: errors.New("connection refused")}
	wrapped := errors.Join(errors.New("telegram: failed to send message"), ue)
	safe := SafeError(wrapped)
	if strings.Contains(safe.Error(), "SECRET") || !strings.Contains(safe.Error(), `"http://127.0.0.1:1"`) {
		t.Fatalf("SafeError = %q, want the URL cut to scheme://host", safe.Error())
	}
	var target *url.Error
	if !errors.As(safe, &target) {
		t.Fatal("errors.As lost the *url.Error")
	}
	if SafeError(safe) != safe || SafeError(nil) != nil || SafeErrorText(nil) != "" {
		t.Fatal("SafeError must be idempotent and nil-preserving")
	}
}
