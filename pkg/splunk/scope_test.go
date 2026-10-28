package splunk

import (
	"errors"
	"testing"
)

func TestParseScope(t *testing.T) {
	for _, search := range []string{"search index=main AND service=api", "index=main"} {
		clauses, err := parseScope(search)
		if err != nil || len(clauses) == 0 || clauses[0].field != "index" {
			t.Fatalf("valid scope rejected: %q: %v", search, err)
		}
	}
	for _, search := range []string{"", "search index=main | stats count", "search index=main OR index=other", "search index=main AND index=other", "search index=main AND source=\"a\\\"b\"", "search index=main AND eval=x", "search index=main [ search index=other ]", "search index=main`macro`"} {
		if _, err := parseScope(search); !errors.Is(err, ErrUnsupportedScope) {
			t.Fatalf("unsafe scope accepted: %q", search)
		}
	}
}
