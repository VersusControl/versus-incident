package logging

import (
	"bytes"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

// captureLog points the standard logger at a buffer with the given flags and
// prefix, and restores the writer, flags, prefix and package level when the
// test ends. Tests that use it must not run in parallel.
func captureLog(t *testing.T, flags int, prefix string) *bytes.Buffer {
	t.Helper()
	oldOut, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	oldLevel := level.Level()
	buf := &bytes.Buffer{}
	log.SetOutput(buf)
	log.SetFlags(flags)
	log.SetPrefix(prefix)
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
		level.Set(oldLevel)
	})
	return buf
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"":          slog.LevelInfo,
		"debug":     slog.LevelDebug,
		"DEBUG":     slog.LevelDebug,
		" Debug ":   slog.LevelDebug,
		"info":      slog.LevelInfo,
		"INFO":      slog.LevelInfo,
		"warn":      slog.LevelWarn,
		"warning":   slog.LevelWarn,
		"WARNING":   slog.LevelWarn,
		"error":     slog.LevelError,
		"Error":     slog.LevelError,
		"verbose":   slog.LevelInfo,
		"trace":     slog.LevelInfo,
		"0":         slog.LevelInfo,
		"warn,info": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLevelNameRoundTrips(t *testing.T) {
	for _, name := range []string{"debug", "info", "warn", "error"} {
		if got := LevelName(ParseLevel(name)); got != name {
			t.Errorf("LevelName(ParseLevel(%q)) = %q", name, got)
		}
	}
}

func TestFilteringPerLevel(t *testing.T) {
	emitAll := func() {
		Debugf("d")
		Infof("i")
		Warnf("w")
		Errorf("e")
	}
	cases := []struct {
		level string
		want  string
	}{
		{"debug", "DEBUG d\ni\nWARN w\nERROR e\n"},
		{"info", "i\nWARN w\nERROR e\n"},
		{"", "i\nWARN w\nERROR e\n"},
		{"warn", "WARN w\nERROR e\n"},
		{"error", "ERROR e\n"},
	}
	for _, tc := range cases {
		t.Run("level="+tc.level, func(t *testing.T) {
			buf := captureLog(t, 0, "")
			Setup(tc.level)
			emitAll()
			if got := buf.String(); got != tc.want {
				t.Fatalf("level %q wrote %q, want %q", tc.level, got, tc.want)
			}
		})
	}
}

func TestEnabled(t *testing.T) {
	captureLog(t, 0, "")
	Setup("warn")
	if Enabled(slog.LevelDebug) || Enabled(slog.LevelInfo) {
		t.Fatal("debug and info must be disabled at warn")
	}
	if !Enabled(slog.LevelWarn) || !Enabled(slog.LevelError) {
		t.Fatal("warn and error must be enabled at warn")
	}
	Setup("debug")
	if !Enabled(slog.LevelDebug) {
		t.Fatal("debug must be enabled at debug")
	}
}

func TestOutputFormatPerLevel(t *testing.T) {
	stamp := `\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `
	cases := []struct {
		name string
		log  func(string, ...any)
		want *regexp.Regexp
	}{
		{"debug", Debugf, regexp.MustCompile(`^` + stamp + `DEBUG kubernetes: inventory kind=Pod records=3\n$`)},
		{"info", Infof, regexp.MustCompile(`^` + stamp + `kubernetes: inventory kind=Pod records=3\n$`)},
		{"warn", Warnf, regexp.MustCompile(`^` + stamp + `WARN kubernetes: inventory kind=Pod records=3\n$`)},
		{"error", Errorf, regexp.MustCompile(`^` + stamp + `ERROR kubernetes: inventory kind=Pod records=3\n$`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t, log.LstdFlags, "")
			Setup("debug")
			tc.log("kubernetes: inventory kind=%s records=%d", "Pod", 3)
			if !tc.want.MatchString(buf.String()) {
				t.Fatalf("%s line %q does not match %s", tc.name, buf.String(), tc.want)
			}
		})
	}
}

func TestInfoMatchesLogPrintfByteForByte(t *testing.T) {
	// Without a timestamp the two lines can be compared exactly, including
	// the standard logger's prefix and short-file flag.
	buf := captureLog(t, log.Lshortfile, "versus ")
	Setup("info")
	log.Printf("kubernetes: cluster %s connected", "prod")
	Infof("kubernetes: cluster %s connected", "prod")
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", buf.String())
	}
	// Both name this test file; only the line number differs.
	strip := regexp.MustCompile(`:\d+: `)
	a, b := strip.ReplaceAllString(lines[0], ": "), strip.ReplaceAllString(lines[1], ": ")
	if a != b {
		t.Fatalf("Infof line %q differs from log.Printf line %q", lines[1], lines[0])
	}
	if !strings.HasPrefix(lines[1], "versus logging_test.go:") {
		t.Fatalf("Infof must report the caller's file, got %q", lines[1])
	}
}

func TestHelpersFollowLogWriterSetAfterSetup(t *testing.T) {
	captureLog(t, 0, "")
	Setup("info")
	later := &bytes.Buffer{}
	log.SetOutput(later)
	Warnf("storage: postgres unreachable")
	if got := later.String(); got != "WARN storage: postgres unreachable\n" {
		t.Fatalf("helper did not follow the current log writer: %q", got)
	}
}

func TestLogPrintfUnchangedAfterSetup(t *testing.T) {
	buf := captureLog(t, log.LstdFlags, "")
	stamp := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} report: renderer unavailable\n$`)
	for _, value := range []string{"debug", "info", "warn", "error"} {
		buf.Reset()
		Setup(value)
		log.Printf("report: renderer unavailable")
		if !stamp.MatchString(buf.String()) {
			t.Fatalf("after Setup(%q) log.Printf wrote %q", value, buf.String())
		}
	}
	if slog.Default().Enabled(t.Context(), slog.LevelDebug) {
		t.Fatal("Setup must not install a debug slog default")
	}
}

func TestUnknownLevelWarnsOnceAndUsesInfo(t *testing.T) {
	buf := captureLog(t, 0, "")
	got := Setup("verbose")
	if got != slog.LevelInfo || Level() != slog.LevelInfo {
		t.Fatalf("unknown level resolved to %v (package %v), want info", got, Level())
	}
	want := "WARN logging: LOG_LEVEL=\"verbose\" is not a level; using info. Set LOG_LEVEL to debug, info, warn or error\n"
	if buf.String() != want {
		t.Fatalf("unknown level wrote %q, want %q", buf.String(), want)
	}
	buf.Reset()
	Debugf("hidden")
	Infof("shown")
	if buf.String() != "shown\n" {
		t.Fatalf("after fallback wrote %q, want only the info line", buf.String())
	}
}

func TestKnownLevelsWriteNothingOnSetup(t *testing.T) {
	buf := captureLog(t, 0, "")
	for _, value := range []string{"", "debug", "info", "warn", "warning", "error"} {
		Setup(value)
	}
	if buf.Len() != 0 {
		t.Fatalf("Setup with a known level wrote %q", buf.String())
	}
}

func TestEveryWriterNamesTheCallersFile(t *testing.T) {
	buf := captureLog(t, log.Lshortfile, "")
	Setup("debug")
	Debugf("debug line")
	Infof("info line")
	Warnf("warn line")
	Errorf("error line")
	var th Throttle
	th.Fail(slog.LevelWarn, "k", "cause", "throttled failure")
	th.Clear("k", "throttled clear")
	Setup("bogus")
	got := lines(buf.String())
	if len(got) != 7 {
		t.Fatalf("want 7 lines, got %q", got)
	}
	for _, l := range got {
		if !strings.HasPrefix(l, "logging_test.go:") {
			t.Errorf("line does not name the caller's file: %q", l)
		}
	}
}
