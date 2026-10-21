package utils

import "testing"

func TestSafeReturnTo(t *testing.T) {
	for _, value := range []string{"", "https://evil.invalid", "//evil.invalid", "/\\evil.invalid", "/%2fevil.invalid", "/%5cevil.invalid", "/%0aevil", "http:evil", "?next=/agent"} {
		if got := SafeReturnTo(value); got != "/" {
			t.Errorf("%q accepted as %q", value, got)
		}
	}
	for _, value := range []string{"/", "/agent/kubernetes?cluster=prod&view=timeline", "/agent/chat?issue=pod.failed", "/agent?name=a%2Fb"} {
		if got := SafeReturnTo(value); got != value {
			t.Errorf("%q => %q", value, got)
		}
	}
}
