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
		"2026/11/21 08:44:20 [crit] x",
		"2026-11-21 08:44:20,879 - app - ERROR - x",
		`time="x" level="error`,
		`a=b level=`,
		`{"level":50}`,
		`{"level":50,"v":0}`,
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

// TestLevelReadsLogfmtPythonNginxAndNumericJSON pins the level of four more
// common formats, built from real lines: logfmt (logrus, go-kit), Python's
// logging module, the nginx error log, and pino/bunyan numeric JSON levels.
func TestLevelReadsLogfmtPythonNginxAndNumericJSON(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"logrus text error", `time="2026-11-21T08:44:20Z" level=error msg="push failed" component=ads`, "error"},
		{"logrus text warning", `time="2026-11-21T08:44:20Z" level=warning msg="slow reply"`, "warning"},
		{"go-kit lvl key", `ts=2026-11-21T08:44:20.123Z caller=main.go:42 lvl=warn msg="retrying"`, "warning"},
		{"level key first", `level=debug ts=2026-11-21T08:44:20Z msg="cache hit"`, "debug"},
		{"quoted level value", `ts=2026-11-21T08:44:20Z level="error" msg="boom"`, "error"},
		{"logfmt after iso timestamp", `2026-11-21T08:44:20.976Z level=error msg="dial tcp: refused"`, "error"},
		{"logfmt fatal", `time="2026-11-21T08:44:20Z" level=fatal msg="cannot start"`, "error"},
		{"logfmt mixed case key and value", `ts=2026-11-21T08:44:20Z Level=ERROR msg=x`, "error"},
		{"logfmt mixed case value", `time="2026-11-21T08:44:20Z" level=Warn msg=x`, "warning"},
		{"python default format error", "2026-11-21 08:44:20,879 - payments.worker - ERROR - charge failed", "error"},
		{"python default format warning", "2026-11-21 08:44:20,879 - root - WARNING - disk at 91%", "warning"},
		{"python critical", "2026-11-21 08:44:20,879 - app - CRITICAL - out of memory", "error"},
		{"python without name", "2026-11-21 08:44:20,879 - ERROR - charge failed", "error"},
		{"python mixed case level", "2026-11-21 08:44:20,879 - app - Error - charge failed", "error"},
		{"python debug", "2026-11-21 08:44:20,879 - urllib3.connectionpool - DEBUG - Starting new HTTPS connection", "debug"},
		{"nginx error", "2026/11/21 08:44:20 [error] 29#29: *1 open() \"/usr/share/nginx/html/x\" failed (2: No such file or directory)", "error"},
		{"nginx warn", "2026/11/21 08:44:20 [warn] 1#1: conflicting server name \"_\" on 0.0.0.0:80, ignored", "warning"},
		{"nginx crit", "2026/11/21 08:44:20 [crit] 1#1: *3 SSL_do_handshake() failed", "error"},
		{"nginx emerg", "2026/11/21 08:44:20 [emerg] 1#1: bind() to 0.0.0.0:80 failed (98: Address in use)", "error"},
		{"nginx alert", "2026/11/21 08:44:20 [alert] 1#1: worker process 30 exited on signal 9", "error"},
		{"nginx notice", "2026/11/21 08:44:20 [notice] 1#1: start worker processes", "info"},
		{"pino fatal", `{"level":60,"time":1763714660000,"pid":1,"hostname":"api-0","msg":"crash"}`, "error"},
		{"pino error", `{"level":50,"time":1763714660000,"pid":1,"hostname":"api-0","msg":"request failed"}`, "error"},
		{"pino warn", `{"level":40,"time":1763714660000,"msg":"slow"}`, "warning"},
		{"pino info", `{"level":30,"time":1763714660000,"msg":"listening"}`, "info"},
		{"pino debug", `{"level":20,"time":1763714660000,"msg":"detail"}`, "debug"},
		{"pino trace", `{"level":10,"time":1763714660000,"msg":"enter"}`, "debug"},
		{"bunyan error", `{"name":"api","hostname":"api-0","pid":1,"level":50,"msg":"boom","time":"2026-11-21T08:44:20.000Z","v":0}`, "error"},
		{"numeric level as a string", `{"level":"50","msg":"request failed"}`, "error"},
		{"numeric warn as a string", `{"level":"40","msg":"slow"}`, "warning"},
		{"pino level next to time only", `{"level":50,"time":1763714660000}`, "error"},
		{"bunyan level next to v only", `{"level":40,"v":0}`, "warning"},
		{"pino level next to pid and hostname", `{"level":60,"pid":1,"hostname":"api-0"}`, "error"},
		{"python level then message with a level word", "2026-11-21 08:44:20,879 - INFO - Warning - disk ok", "info"},
		{"python level then dashed message", "2026-11-21 08:44:20,879 - ERROR - retry - 3 of 5", "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Level(tc.text); got != tc.want {
				t.Errorf("Level(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestLevelNewFormatsIgnoreLevelWordsInTheMessage keeps the message body from
// deciding the level: an "error" word or a "level=error" text inside a value
// or a sentence leaves the line at its own level.
func TestLevelNewFormatsIgnoreLevelWordsInTheMessage(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"logfmt info with error in message", `time="2026-11-21T08:44:20Z" level=info msg="retrying after error"`, "info"},
		{"logfmt level text inside quoted message", `ts=2026-11-21T08:44:20Z msg="set level=error on sink" level=info`, "info"},
		{"logfmt level text inside quoted message only", `ts=2026-11-21T08:44:20Z msg="set level=error on sink"`, "info"},
		{"logfmt escaped quote inside message", `ts=2026-11-21T08:44:20Z msg="say \"level=error\" now" level=debug`, "debug"},
		{"sentence containing level=error", "applying config with level=error to the sink", "info"},
		{"timestamped sentence containing level=error", "2026-11-21 08:44:20.879 applying level=error to the sink", "info"},
		{"logfmt unknown level value", `ts=2026-11-21T08:44:20Z level=verbose msg="error"`, "info"},
		{"logfmt key that only ends in level", `ts=2026-11-21T08:44:20Z loglevel=error msg=x`, "info"},
		{"json string value with level=error", `{"msg":"set level=error on sink"}`, "info"},
		{"json message with error word", `{"msg":"error while reading","pid":1}`, "info"},
		{"json level info and error message", `{"level":30,"msg":"error count=0"}`, "info"},
		{"json unknown numeric level", `{"level":35,"msg":"x"}`, "info"},
		{"json numeric level without a pino field", `{"level":50}`, "info"},
		{"json zoom level", `{"zoom":3,"level":50,"x":1,"y":2}`, "info"},
		{"json game level as a string", `{"player":"ana","level":"60"}`, "info"},
		{"json numeric level outside pino scale", `{"level":3,"msg":"x"}`, "info"},
		{"json non-numeric level string", `{"level":"high","msg":"x"}`, "info"},
		{"malformed json with level=error text", `{"msg":"level=error"`, "info"},
		{"python info with error in message", "2026-11-21 08:44:20,879 - app - INFO - error budget at 5%", "info"},
		{"python name that is a level word reads as the level", "2026-11-21 08:44:20,879 - error - INFO - started", "error"},
		{"python message with dashed error", "2026-11-21 08:44:20,879 - app - INFO - step - ERROR - none", "info"},
		{"dash without python fields", "2026-11-21 08:44:20,879 - starting error recovery", "info"},
		{"nginx notice with error in message", "2026/11/21 08:44:20 [notice] 1#1: error_log set", "info"},
		{"alert as first word is not a level", "alert sent to on-call", "info"},
		{"slash date without clock", "2026/11/21 [error] boom", "info"},
		{"slash date with T separator", "2026/11/21T08:44:20 [error] boom", "info"},
		{"mixed separators", "2026/11-21 08:44:20 [error] boom", "info"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Level(tc.text); got != tc.want {
				t.Errorf("Level(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}
