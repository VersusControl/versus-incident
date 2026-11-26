package breaklogs

import (
	"strings"
	"unicode/utf8"
)

// IsError reports whether Level reads text as an error line.
func IsError(text string) bool { return Level(text) == "error" }

const (
	// MaxJoinedLines is the most lines one joined entry keeps, its first
	// line included.
	MaxJoinedLines = 64
	// MaxJoinedBytes is the most bytes one joined entry keeps, the line
	// breaks between its lines included.
	MaxJoinedBytes = 16 << 10
)

// JoinedEntry is one log entry: a line and the stack-trace lines folded into
// it, joined by "\n".
type JoinedEntry struct {
	Text string
	// Lines counts the lines kept in Text.
	Lines int
	// Truncated is set when continuation lines were dropped, or the first
	// line cut, to stay within MaxJoinedLines and MaxJoinedBytes.
	Truncated bool
}

// traceState is what kind of multi-line trace the open entry is in, which
// decides the lines that continue it beyond the ones that always do.
type traceState int

const (
	traceNone traceState = iota
	// tracePython is inside a Python traceback, before its exception line.
	tracePython
	// tracePythonDone follows a traceback's exception line, where a chained
	// traceback may still follow.
	tracePythonDone
	// traceGo is inside a Go panic or goroutine dump.
	traceGo
)

// Joiner folds stack-trace continuation lines into the line before them, so a
// trace becomes one entry: indented lines, a Java exception line, "at …"
// frames, "Caused by:", "Suppressed:" and "... N more", a Python traceback through its exception
// line, and a Go panic with its goroutine headers and frames. Feed it lines in
// order with Add and call Flush at the end. The zero value is ready to use. It
// is not safe for concurrent use.
type Joiner struct {
	entry strings.Builder
	lines int
	open  bool
	cut   bool
	state traceState
	// headError is set when the entry's first line is an error line, and
	// traced once the entry has taken in a stack frame.
	headError bool
	traced    bool
}

// Add takes the next line. When the line starts a new entry, the entry before
// it is complete and returned with true.
func (joiner *Joiner) Add(line string) (JoinedEntry, bool) {
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if joiner.open && joiner.continues(line) {
		joiner.append(line)
		return JoinedEntry{}, false
	}
	previous, ok := joiner.Flush()
	joiner.start(line)
	return previous, ok
}

// Flush returns the open entry, if any, and leaves the joiner empty.
func (joiner *Joiner) Flush() (JoinedEntry, bool) {
	if !joiner.open {
		return JoinedEntry{}, false
	}
	text := strings.TrimRight(joiner.entry.String(), "\n")
	entry := JoinedEntry{Text: text, Lines: strings.Count(text, "\n") + 1, Truncated: joiner.cut}
	joiner.entry.Reset()
	joiner.lines, joiner.open, joiner.cut, joiner.state = 0, false, false, traceNone
	joiner.headError, joiner.traced = false, false
	return entry, true
}

// JoinLines joins a whole block of lines at once.
func JoinLines(lines []string) []JoinedEntry {
	var joiner Joiner
	entries := []JoinedEntry{}
	for _, line := range lines {
		if entry, ok := joiner.Add(line); ok {
			entries = append(entries, entry)
		}
	}
	if entry, ok := joiner.Flush(); ok {
		entries = append(entries, entry)
	}
	return entries
}

func (joiner *Joiner) start(line string) {
	joiner.open = true
	if len(line) > MaxJoinedBytes {
		line = cutRunes(line, MaxJoinedBytes)
		joiner.cut = true
	}
	joiner.entry.WriteString(line)
	joiner.lines = 1
	joiner.headError = IsError(line)
	trimmed := strings.TrimLeft(line, " \t")
	switch {
	case strings.HasPrefix(trimmed, "panic: ") || strings.HasPrefix(trimmed, "fatal error: ") || goroutineHeader(trimmed):
		joiner.state = traceGo
	case strings.HasPrefix(trimmed, "Traceback (most recent call last):"):
		joiner.state = tracePython
	default:
		joiner.state = traceNone
	}
}

func (joiner *Joiner) append(line string) {
	if joiner.lines >= MaxJoinedLines || joiner.entry.Len()+1+len(line) > MaxJoinedBytes {
		joiner.cut = true
		return
	}
	joiner.entry.WriteByte('\n')
	joiner.entry.WriteString(line)
	joiner.lines++
}

// continues reports whether line belongs to the open entry, and moves the
// trace state on.
func (joiner *Joiner) continues(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed != "" && len(trimmed) < len(line) {
		// An indented line: a Java, Python or Go frame, or any other
		// indented part of a multi-line entry. A line that starts with a
		// level word and a separator is a new entry whose level is padded,
		// as log4j's %5p writes " INFO", even after a Java trace, whose
		// indented lines are "at" frames. Inside a Python traceback an
		// indented line is source, such as "    error = total / count".
		if joiner.state != tracePython && paddedLevel(trimmed) {
			return false
		}
		if strings.HasPrefix(trimmed, "at ") {
			joiner.traced = true
		}
		return true
	}
	if joiner.state == tracePython && trimmed != "" && !strings.HasPrefix(trimmed, "Traceback (most recent call last):") {
		// The first line back at the margin is the exception itself.
		joiner.state = tracePythonDone
		return true
	}
	// The start of a trace folds only into an error line or a trace already
	// open; after any other line it starts an entry of its own, as an
	// uncaught exception printed after an info line does.
	traceOpen := joiner.headError || joiner.traced || joiner.state != traceNone
	switch {
	case strings.HasPrefix(trimmed, "Caused by:"), strings.HasPrefix(trimmed, "Suppressed:"), javaMore(trimmed), atFrame(trimmed):
		joiner.traced = true
		return true
	case traceOpen && javaException(trimmed):
		return true
	case traceOpen && strings.HasPrefix(trimmed, "Traceback (most recent call last):"):
		joiner.state = tracePython
		return true
	case goroutineHeader(trimmed):
		joiner.state = traceGo
		return true
	}
	switch joiner.state {
	case tracePython:
		return true
	case tracePythonDone:
		if trimmed == "During handling of the above exception, another exception occurred:" || trimmed == "The above exception was the direct cause of the following exception:" {
			joiner.state = tracePython
			return true
		}
		return trimmed == ""
	case traceGo:
		return trimmed == "" || strings.HasPrefix(trimmed, "[signal ") || strings.HasPrefix(trimmed, "created by ") || strings.HasPrefix(trimmed, "exit status ") || goFrame(trimmed)
	}
	return false
}

// paddedLevel reports a line that opens with a level word followed by a
// separator: "INFO [main] …", "WARN: …", "[ERROR] …". A word used as a name,
// as in "error = 1" or "error.go", is not one.
func paddedLevel(text string) bool {
	if strings.HasPrefix(text, "[") {
		end := strings.IndexByte(text, ']')
		return end > 0 && normalizeLevel(strings.TrimSpace(text[1:end])) != ""
	}
	end := strings.IndexAny(text, " \t:")
	if end <= 0 || normalizeLevel(text[:end]) == "" {
		return false
	}
	rest := strings.TrimLeft(text[end:], " \t")
	return !strings.HasPrefix(rest, "=")
}

// goroutineHeader matches "goroutine 1 [running]:".
func goroutineHeader(text string) bool {
	rest, ok := strings.CutPrefix(text, "goroutine ")
	if !ok {
		return false
	}
	number, state, found := strings.Cut(rest, " [")
	return found && number != "" && digits(number) && strings.HasSuffix(state, "]:")
}

// goFrame matches a Go stack function line such as "main.(*Server).run(0x1)"
// or "main.main()": a dotted name with no spaces, then its arguments.
func goFrame(text string) bool {
	open := strings.IndexByte(text, '(')
	if open <= 0 || !strings.HasSuffix(text, ")") {
		return false
	}
	name := text[:open]
	return strings.Contains(name, ".") && !strings.ContainsAny(name, " \t")
}

// atFrame matches a Java or JavaScript frame at the margin, as some loggers
// write them: "at pkg.Class.method(File.java:12)", "at fn (/app/x.js:3:9)",
// "at /app/x.js:3:9" or "at X.y(Native Method)". Ordinary text that starts
// with "at" does not have a source position.
func atFrame(text string) bool {
	rest, ok := strings.CutPrefix(text, "at ")
	if !ok || rest == "" {
		return false
	}
	if strings.HasSuffix(rest, ")") {
		open := strings.LastIndexByte(rest, '(')
		if open < 0 {
			return false
		}
		inner := rest[open+1 : len(rest)-1]
		if inner == "Native Method" || inner == "Unknown Source" {
			return true
		}
		return sourcePosition(inner) && !strings.ContainsAny(strings.TrimSuffix(rest[:open], " "), " \t")
	}
	return !strings.ContainsAny(rest, " \t") && sourcePosition(rest)
}

// sourcePosition matches a location that ends in ":<line>" or
// ":<line>:<column>".
func sourcePosition(text string) bool {
	colon := strings.LastIndexByte(text, ':')
	if colon <= 0 || !digits(text[colon+1:]) {
		return false
	}
	head := text[:colon]
	if previous := strings.LastIndexByte(head, ':'); previous > 0 && digits(head[previous+1:]) {
		head = head[:previous]
	}
	return head != "" && strings.ContainsAny(head, "./")
}

// javaException matches the exception line a Java logger writes under its
// message: "java.lang.IllegalStateException: text", a dotted class name
// ending in Exception, Error or Throwable.
func javaException(text string) bool {
	name, _, _ := strings.Cut(text, ": ")
	if !strings.Contains(name, ".") || strings.ContainsAny(name, " \t") {
		return false
	}
	return strings.HasSuffix(name, "Exception") || strings.HasSuffix(name, "Error") || strings.HasSuffix(name, "Throwable")
}

// javaMore matches "... 12 more".
func javaMore(text string) bool {
	rest, ok := strings.CutPrefix(text, "... ")
	if !ok {
		return false
	}
	number, ok := strings.CutSuffix(rest, " more")
	return ok && number != "" && digits(number)
}

func cutRunes(text string, limit int) string {
	for limit > 0 && limit < len(text) && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
