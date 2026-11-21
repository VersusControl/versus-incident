package breaklogs

import "testing"

func TestLevelJSONAndLeadingTokens(t *testing.T) {
	for text, want := range map[string]string{`{"level":"ERROR"}`: "error", `{"severity":"warn"}`: "warning", "[ERROR] failed": "error", "warn: slow": "warning", "debug trace": "debug", "an error later": "info", `{"level":"unknown","severity":"critical"}`: "error"} {
		if got := Level(text); got != want {
			t.Errorf("%q: %s != %s", text, got, want)
		}
	}
}

func TestLevelApplicationLogFormats(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"logback bracketed timestamp and padded level", "[2026-11-21 08:44:20:879] [ERROR  ] [svc,abc,def] request failed", "error"},
		{"logback bracketed warn", "[2026-03-02 10:00:01:002] [WARN   ] [svc] slow", "warning"},
		{"logback comma millis", "2026-03-02 10:00:01,002 ERROR [main] boom", "error"},
		{"spring boot console warn", "2026-11-21 08:44:20.879  WARN 1 --- [main] c.e.Foo : pool low", "warning"},
		{"spring boot console error with zone", "2026-11-21T08:44:20.879+00:00 ERROR 1 --- [main] c.e.Foo : failed", "error"},
		{"spring boot console debug", "2026-11-21 08:44:20.879 DEBUG 1 --- [main] c.e.Foo : detail", "debug"},
		{"zap console tab separated error", "2026-11-21T08:44:15.976Z\terror\tads\tpush failed", "error"},
		{"istio tab separated warn", "2026-11-21T08:44:15.976123Z\twarn\txdsproxy\tretrying", "warning"},
		{"istio tab separated info", "2026-11-21T08:44:15.976Z\tinfo\tads\terror count=0", "info"},
		{"timestamp without fraction", "2026-11-21T08:44:15Z fatal out of memory", "error"},
		{"timestamp with compact zone", "2026-11-21T08:44:15.1-0700 panic: nil map", "error"},
		{"bracketed iso timestamp", "[2026-11-21T08:44:15.976Z] critical disk full", "error"},
		{"klog error", "E1121 08:44:15.976000       1 foo.go:12] boom", "error"},
		{"klog warning", "W1121 08:44:15.976000 1 foo.go:12] slow", "warning"},
		{"klog fatal", "F0102 03:04:05.000001 7 main.go:1] dead", "error"},
		{"klog info", "I1121 08:44:15.976000 1 foo.go:12] error count=0", "info"},
		{"mongodb error", `{"t":{"$date":"2026-11-21T08:44:15.976+00:00"},"s":"E","c":"NETWORK","id":1,"ctx":"conn1","msg":"socket error"}`, "error"},
		{"mongodb warning", `{"t":{"$date":"2026-11-21T08:44:15.976+00:00"},"s":"W","c":"CONTROL","id":2,"ctx":"main","msg":"no auth"}`, "warning"},
		{"mongodb fatal", `{"t":{"$date":"x"},"s":"F","c":"ASSERT","id":3,"ctx":"main","msg":"abort"}`, "error"},
		{"mongodb debug level", `{"t":{"$date":"x"},"s":"D2","c":"QUERY","id":4,"ctx":"conn2","msg":"plan"}`, "debug"},
		{"json level wins over s", `{"level":"warn","s":"E","msg":"x"}`, "warning"},
		{"json severity kept", `{"severity":"ERROR","message":"x"}`, "error"},
	}
	for _, tc := range cases {
		if got := Level(tc.text); got != tc.want {
			t.Errorf("%s: Level(%q) = %s, want %s", tc.name, tc.text, got, tc.want)
		}
	}
}

func TestLevelDoesNotReadWordsInsideTheMessage(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"info prefix then error word", "info: user error count=0", "info"},
		{"leading capitalised error word stays an error line", "Error handling request", "error"},
		{"timestamp then message with error word", "2026-11-21 08:44:20.879 request failed with error", "info"},
		{"timestamp glued to word is not a timestamp", "2026-11-21 08:44:20.879ERROR boom", "info"},
		{"bracketed timestamp without closing bracket", "[2026-11-21 08:44:20.879 ERROR boom", "info"},
		{"partial date", "2026-11-21 error later", "info"},
		{"klog-like word without clock", "E1121 boom", "info"},
		{"klog letter outside vocabulary", "X1121 08:44:15.976000 1 foo.go:12] boom", "info"},
		{"json s without msg is not mongodb", `{"s":"E","value":1}`, "info"},
		{"json s with unknown letter", `{"s":"Q","msg":"x"}`, "info"},
		{"plain message", "connected to database", "info"},
		{"empty line", "", "info"},
		{"timestamp only", "2026-11-21T08:44:15.976Z", "info"},
	}
	for _, tc := range cases {
		if got := Level(tc.text); got != tc.want {
			t.Errorf("%s: Level(%q) = %s, want %s", tc.name, tc.text, got, tc.want)
		}
	}
}

func TestLevelSplitsOnAnyUnicodeSpace(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"carriage return after level", "ERROR\r", "error"},
		{"crlf after level", "error\r\n", "error"},
		{"no-break space after level", "WARN boom", "warning"},
		{"vertical tab after level", "ERROR\vboom", "error"},
		{"timestamp then level and carriage return", "2026-11-21 08:44:20,879 ERROR\r", "error"},
		{"timestamp ended by no-break space", "2026-11-21T08:44:15Z warn boom", "warning"},
		{"leading no-break space", " debug detail", "debug"},
	}
	for _, tc := range cases {
		if got := Level(tc.text); got != tc.want {
			t.Errorf("%s: Level(%q) = %s, want %s", tc.name, tc.text, got, tc.want)
		}
	}
}

func TestLevelRejectsImpossibleKlogHeaders(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"all nines", "W9999 99:99:99 x", "info"},
		{"month zero", "E0021 08:44:15.000000 1 a.go:1] x", "info"},
		{"month thirteen", "E1321 08:44:15.000000 1 a.go:1] x", "info"},
		{"day zero", "E1100 08:44:15.000000 1 a.go:1] x", "info"},
		{"day thirty-two", "E1132 08:44:15.000000 1 a.go:1] x", "info"},
		{"hour twenty-four", "E1121 24:00:00.000000 1 a.go:1] x", "info"},
		{"minute sixty", "E1121 08:60:00.000000 1 a.go:1] x", "info"},
		{"second sixty", "E1121 08:44:60.000000 1 a.go:1] x", "info"},
		{"lowest valid header", "W0101 00:00:00.000000 1 a.go:1] x", "warning"},
		{"highest valid header", "E1231 23:59:59.999999 1 a.go:1] x", "error"},
	}
	for _, tc := range cases {
		if got := Level(tc.text); got != tc.want {
			t.Errorf("%s: Level(%q) = %s, want %s", tc.name, tc.text, got, tc.want)
		}
	}
}
