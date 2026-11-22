package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AllowMarker on a line lets a wait through, for the rare test where a real
// sleep or a short fixed bound is the behaviour under test. Write why next to
// it, for example: time.Sleep(10 * time.Millisecond) // testlint:allow lets the ticker fire once
const AllowMarker = "testlint:allow"

// shortBound is the limit below which a fixed timeout counts as a
// wall-clock wait that a loaded or race-instrumented runner can overrun.
const shortBound = 30 * time.Second

// Finding is one flagged added line.
type Finding struct {
	File    string
	Line    int
	Message string
}

func (f Finding) String() string { return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Message) }

var (
	hunkHeader  = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
	sleepCall   = regexp.MustCompile(`\btime\.Sleep\(`)
	durationPat = `((?:\d+(?:\.\d+)?\s*\*\s*)?time\.(?:Nanosecond|Microsecond|Millisecond|Second|Minute|Hour)(?:\s*\*\s*\d+(?:\.\d+)?)?)`
	afterCall   = regexp.MustCompile(`\btime\.After\(\s*` + durationPat + `\s*\)`)
	timeoutCall = regexp.MustCompile(`\bWithTimeout\([^,()]*(?:\([^()]*\))?[^,()]*,\s*` + durationPat + `\s*\)`)
	durationLit = regexp.MustCompile(`^(?:(\d+(?:\.\d+)?)\s*\*\s*)?time\.(Nanosecond|Microsecond|Millisecond|Second|Minute|Hour)(?:\s*\*\s*(\d+(?:\.\d+)?))?$`)
	units       = map[string]time.Duration{
		"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond,
		"Second": time.Second, "Minute": time.Minute, "Hour": time.Hour,
	}
)

// parseDuration reads a literal such as "5 * time.Second", "time.Minute" or
// "time.Millisecond * 250". ok is false for anything else.
func parseDuration(expr string) (time.Duration, bool) {
	m := durationLit.FindStringSubmatch(strings.TrimSpace(expr))
	if m == nil {
		return 0, false
	}
	factor := 1.0
	for _, s := range []string{m[1], m[3]} {
		if s == "" {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		factor *= v
	}
	return time.Duration(factor * float64(units[m[2]])), true
}

// codePart drops a trailing // comment, ignoring // inside string literals.
func codePart(line string) string {
	inStr, inRaw := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inRaw:
			if c == '`' {
				inRaw = false
			}
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '`':
			inRaw = true
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}

// CheckLine returns the problems on one added test line.
func CheckLine(line string) []string {
	if strings.Contains(line, AllowMarker) {
		return nil
	}
	code := codePart(line)
	var out []string
	if sleepCall.MatchString(code) {
		out = append(out, "new time.Sleep in a test; wait for a signal or poll with the shared wait helper instead (or add // "+AllowMarker+" <why>)")
	}
	for _, c := range []struct {
		re   *regexp.Regexp
		name string
	}{{afterCall, "time.After"}, {timeoutCall, "WithTimeout"}} {
		for _, m := range c.re.FindAllStringSubmatch(code, -1) {
			if d, ok := parseDuration(m[1]); ok && d < shortBound {
				out = append(out, fmt.Sprintf("new %s with a fixed %s bound; a bound under %s overruns on a loaded or race-instrumented runner (scale it, wait for a signal, or add // %s <why>)", c.name, d, shortBound, AllowMarker))
			}
		}
	}
	return out
}

// CheckDiff reads a unified diff (git diff -U0) and checks the added lines of
// *_test.go files.
func CheckDiff(r io.Reader) ([]Finding, error) {
	var findings []Finding
	file, line := "", 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		text := sc.Text()
		switch {
		case strings.HasPrefix(text, "+++ "):
			name := strings.TrimPrefix(text, "+++ ")
			if name == "/dev/null" {
				file = ""
			} else {
				file = strings.TrimPrefix(name, "b/")
			}
			continue
		case strings.HasPrefix(text, "--- "), strings.HasPrefix(text, "diff --git "):
			continue
		case strings.HasPrefix(text, "@@"):
			if m := hunkHeader.FindStringSubmatch(text); m != nil {
				line, _ = strconv.Atoi(m[1])
			}
			continue
		}
		// The lint's own tests hold example lines it must flag.
		if !strings.HasSuffix(file, "_test.go") || strings.HasPrefix(file, "tools/testlint/") {
			continue
		}
		switch {
		case strings.HasPrefix(text, "+"):
			for _, msg := range CheckLine(text[1:]) {
				findings = append(findings, Finding{File: file, Line: line, Message: msg})
			}
			line++
		case strings.HasPrefix(text, " "):
			line++
		}
	}
	return findings, sc.Err()
}
