package utils

import (
	"strings"
	"testing"
)

func TestNormalizeSeverity(t *testing.T) {
	cases := map[string]string{
		"critical": "critical",
		"  HIGH ":  "high",
		"Medium":   "medium",
		"low":      "low",
		"":         "medium",
		"unknown":  "medium",
		"PAGE":     "medium",
	}
	for in, want := range cases {
		if got := NormalizeSeverity(in); got != want {
			t.Errorf("NormalizeSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractJSONObject_NestedAndStrings(t *testing.T) {
	in := `prefix {"a":"with } brace","b":{"c":1}} suffix`
	got := ExtractJSONObject(in)
	if !strings.HasPrefix(got, `{"a"`) || !strings.HasSuffix(got, `}}`) {
		t.Fatalf("bad extract: %q", got)
	}
}

func TestExtractJSONObject_NoBrace(t *testing.T) {
	if got := ExtractJSONObject("no json here"); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestExtractJSONObject_Unbalanced(t *testing.T) {
	if got := ExtractJSONObject(`{"a":1`); got != "" {
		t.Fatalf("unbalanced should yield empty, got %q", got)
	}
}

func TestExtractJSONObject_EscapedQuoteInString(t *testing.T) {
	in := `{"a":"he said \"hi}\" then left"}`
	got := ExtractJSONObject(in)
	if got != in {
		t.Fatalf("escape handling broke: %q", got)
	}
}

func TestOneLine(t *testing.T) {
	if got := OneLine("hello\nworld\r!", 0); got != "hello world !" {
		t.Fatalf("newline collapse: %q", got)
	}
	if got := OneLine("abcdef", 3); got != "abc…" {
		t.Fatalf("truncation: %q", got)
	}
	if got := OneLine("abc", 10); got != "abc" {
		t.Fatalf("no-op: %q", got)
	}
}
