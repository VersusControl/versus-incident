package breaklogs

import (
	"strings"
	"testing"
)

// TestLevelReadsLevelAfterAppTimestampOrKlogHeader pins three real formats
// that each used to read as "info": a zap console line, a logback line and a
// klog line.
func TestLevelReadsLevelAfterAppTimestampOrKlogHeader(t *testing.T) {
	for _, text := range []string{
		"2026-11-21T08:44:15.976Z\terror\tads\tpush failed",
		"[2026-11-21 08:44:20:879] [ERROR  ] boom",
		"E1121 08:44:15.976000 1 foo.go:12] boom",
	} {
		if got := Level(text); got != "error" {
			t.Errorf("Level(%q) = %q, want \"error\"", text, got)
		}
	}
}

func TestLevelEdgeCases(t *testing.T) {
	long := strings.Repeat("x", 16*1024)
	cases := []struct {
		name, text, want string
	}{
		{"empty", "", "info"},
		{"whitespace only", " \t ", "info"},
		{"iso timestamp only", "2026-11-21T08:44:15.976Z", "info"},
		{"space timestamp only", "2026-11-21 08:44:20,879", "info"},
		{"bracketed timestamp only", "[2026-11-21 08:44:20:879]", "info"},
		{"timestamp then whitespace only", "2026-11-21T08:44:15Z \t ", "info"},
		{"timestamp glued to level", "2026-11-21T08:44:15.976Zerror boom", "info"},
		{"timestamp glued to punctuation", "2026-11-21T08:44:15.976Z|error|boom", "info"},
		{"bracketed timestamp glued to level", "[2026-11-21 08:44:20:879][ERROR] boom", "info"},
		{"klog-looking prefix without clock", "I0000 not a time", "info"},
		{"klog-looking error prefix without clock", "E0000 not a time error", "info"},
		{"klog prefix too short", "E1121 08:44", "info"},
		{"klog lowercase letter", "e1121 08:44:15.976000 1 foo.go:12] boom", "info"},
		{"mongodb json without msg", `{"t":{"$date":"2026-11-21T08:44:15.976+00:00"},"s":"E","c":"NETWORK","id":1}`, "info"},
		{"mongodb json s not a string", `{"s":1,"msg":"x"}`, "info"},
		{"malformed json with level", `{"level":"error"`, "info"},
		{"malformed json brace only", "{", "info"},
		{"json array", `[{"level":"error"}]`, "info"},
		{"json null level", `{"level":null,"msg":"x"}`, "info"},
		{"16 KiB error line", "ERROR " + long, "error"},
		{"16 KiB timestamped warn line", "2026-11-21 08:44:20.879  WARN " + long, "warning"},
		{"16 KiB single token", long, "info"},
		{"16 KiB json mongodb", `{"s":"W","msg":"` + long + `"}`, "warning"},
		{"non-ascii message after level", "ERROR ünïcødé 日本語 сбой", "error"},
		{"non-ascii first word", "日本語 error", "info"},
		{"non-ascii after timestamp", "2026-11-21 08:44:20.879 ПРЕДУПРЕЖДЕНИЕ сбой", "info"},
		{"non-ascii lookalike level", "ERRÖR boom", "info"},
		{"invalid utf-8", "\xff\xfe error", "info"},
		{"invalid utf-8 after level", "warn \xff\xfe", "warning"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Level(tc.text); got != tc.want {
				shown := tc.text
				if len(shown) > 80 {
					shown = shown[:80] + "..."
				}
				t.Errorf("Level(%q) = %q, want %q", shown, got, tc.want)
			}
		})
	}
}

func FuzzLevel(f *testing.F) {
	for _, seed := range []string{
		"",
		"{",
		"[",
		"]",
		"[]",
		"E1121 08:44:15.976000 1 foo.go:12] boom",
		"I0000 not a time",
		"E1121",
		"2026-11-21T08:44:15.976Z\terror\tads\tpush failed",
		"[2026-11-21 08:44:20:879] [ERROR  ] boom",
		"[2026-11-21 08:44:20:879",
		"2026-11-21T08:44:15.976+",
		"2026-11-21T08:44:15.976+07:",
		"2026-11-21T08:44:15.1-0700 panic: nil map",
		"2026-11-21 08:44:20.",
		"2026-11-21 08:44:20,879 ",
		`{"t":{"$date":"x"},"s":"E","c":"NETWORK","msg":"x"}`,
		`{"level":"warn"}`,
		`{"level":"error"`,
		"ERRÖR \xff",
	} {
		f.Add(seed)
	}
	known := map[string]bool{"error": true, "warning": true, "info": true, "debug": true}
	f.Fuzz(func(t *testing.T, text string) {
		got := Level(text)
		if !known[got] {
			t.Fatalf("Level(%q) = %q, not a known level", text, got)
		}
		if Level(text) != got {
			t.Fatalf("Level(%q) is not deterministic", text)
		}
	})
}
