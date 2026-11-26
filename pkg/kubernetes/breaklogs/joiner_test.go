package breaklogs

import (
	"fmt"
	"strings"
	"testing"
)

func TestIsErrorFollowsLevel(t *testing.T) {
	for text, want := range map[string]bool{
		`{"level":"error","msg":"x"}`:                      true,
		"E1121 08:44:15.976000 1 foo.go:12] boom":          true,
		"2026-11-21T08:44:15Z fatal out of memory":         true,
		"panic: runtime error: index out of range":         true,
		"WARN slow request":                                false,
		"an error later in the line":                       false,
		"I1121 08:44:15.976000 1 foo.go:12] error count=0": false,
		"": false,
	} {
		if got := IsError(text); got != want {
			t.Errorf("IsError(%q) = %v, want %v (Level %q)", text, got, want, Level(text))
		}
	}
}

func javaTrace(frames int) []string {
	lines := []string{
		"2026-11-21 08:44:20,879 ERROR [main] c.e.OrderService - order failed",
		"java.lang.IllegalStateException: order 42 has no lines",
	}
	for index := range frames {
		lines = append(lines, fmt.Sprintf("\tat com.example.orders.Step%d.run(Step%d.java:%d)", index, index, 10+index))
	}
	return append(lines,
		"Caused by: java.sql.SQLException: connection reset",
		"\tat org.postgresql.core.PGStream.receive(PGStream.java:120)",
		"\t... 38 more",
	)
}

func TestJoinLinesFoldsStackTraces(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		// want is the line count of each joined entry.
		want []int
		// heads are the first lines of each entry.
		heads []string
	}{
		{
			name:  "a 40-frame Java trace with a cause",
			lines: append(javaTrace(40), "2026-11-21 08:44:21,001 INFO [main] c.e.OrderService - next order"),
			want:  []int{45, 1},
			heads: []string{"2026-11-21 08:44:20,879 ERROR [main] c.e.OrderService - order failed", "2026-11-21 08:44:21,001 INFO [main] c.e.OrderService - next order"},
		},
		{
			name: "Java frames written at the margin",
			lines: []string{
				"ERROR request failed",
				"at com.example.Api.handle(Api.java:88)",
				"at java.base/java.lang.Thread.run(Thread.java:833)",
				"at jdk.internal.reflect.NativeMethodAccessorImpl.invoke0(Native Method)",
				"... 3 more",
				"INFO recovered",
			},
			want: []int{5, 1},
		},
		{
			name: "JavaScript frames",
			lines: []string{
				"TypeError: Cannot read properties of undefined (reading 'id')",
				"    at Object.handler (/app/src/index.js:10:15)",
				"    at async Promise.all (index 0)",
				"at /app/node_modules/express/lib/router.js:284:7",
				"listening on 8080",
			},
			want: []int{4, 1},
		},
		{
			name: "a Python traceback with a chained cause",
			lines: []string{
				"2026-11-21 08:44:15,976 - worker - ERROR - task failed",
				"Traceback (most recent call last):",
				`  File "/app/worker.py", line 12, in run`,
				"    result = compute(payload)",
				`  File "/app/compute.py", line 4, in compute`,
				"    return payload['value'] / 0",
				"KeyError: 'value'",
				"",
				"During handling of the above exception, another exception occurred:",
				"",
				"Traceback (most recent call last):",
				`  File "/app/worker.py", line 14, in run`,
				"    raise TaskError('compute failed')",
				"worker.TaskError: compute failed",
				"2026-11-21 08:44:16,002 - worker - INFO - next task",
			},
			want:  []int{14, 1},
			heads: []string{"2026-11-21 08:44:15,976 - worker - ERROR - task failed", "2026-11-21 08:44:16,002 - worker - INFO - next task"},
		},
		{
			name: "a Python traceback whose source line starts with a level word",
			lines: []string{
				"ERROR worker failed",
				"Traceback (most recent call last):",
				`  File "/app/stats.py", line 7, in mean`,
				"    error = total / count",
				"ZeroDivisionError: division by zero",
			},
			want: []int{5},
		},
		{
			name: "a Go panic",
			lines: []string{
				"level=info msg=\"serving\"",
				"panic: runtime error: invalid memory address or nil pointer dereference",
				"[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a2b1c]",
				"",
				"goroutine 1 [running]:",
				"main.(*Server).handle(0x0, 0xc000123456)",
				"\t/app/server.go:42 +0x1c",
				"main.main()",
				"\t/app/main.go:17 +0x25",
				"",
				"goroutine 18 [chan receive]:",
				"net/http.(*conn).serve(0xc0001a2000)",
				"\t/usr/local/go/src/net/http/server.go:2009 +0x5f4",
				"created by net/http.(*Server).Serve in goroutine 1",
				"\t/usr/local/go/src/net/http/server.go:3086 +0x5cb",
				"exit status 2",
				"level=info msg=\"restarted\"",
			},
			want:  []int{1, 15, 1},
			heads: []string{`level=info msg="serving"`, "panic: runtime error: invalid memory address or nil pointer dereference", `level=info msg="restarted"`},
		},
		{
			name: "text that only looks like a frame",
			lines: []string{
				"ERROR replica check failed",
				"at least one replica is unavailable (2/3 ready)",
				"at 10:42 the job restarted",
				"Caused by an operator, see the runbook",
				"goroutine count is 42",
				"main.go(42) looked fine",
				"... and more to come",
				"Traceback written to /tmp/trace.txt",
				"KeyError: 'value'",
			},
			want: []int{1, 1, 1, 1, 1, 1, 1, 1, 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries := JoinLines(test.lines)
			counts := make([]int, len(entries))
			for index, entry := range entries {
				counts[index] = entry.Lines
				if entry.Truncated {
					t.Errorf("entry %d truncated", index)
				}
				if got := strings.Count(entry.Text, "\n") + 1; got != entry.Lines {
					t.Errorf("entry %d reports %d lines and holds %d", index, entry.Lines, got)
				}
			}
			if fmt.Sprint(counts) != fmt.Sprint(test.want) {
				t.Fatalf("entry line counts = %v, want %v\n%+v", counts, test.want, entries)
			}
			for index, head := range test.heads {
				if first, _, _ := strings.Cut(entries[index].Text, "\n"); first != head {
					t.Fatalf("entry %d starts %q, want %q", index, first, head)
				}
			}
		})
	}
}

func TestJoinerBoundsAnEntry(t *testing.T) {
	t.Run("at most 64 lines", func(t *testing.T) {
		entries := JoinLines(append(javaTrace(200), "INFO next"))
		if len(entries) != 2 || entries[0].Lines != MaxJoinedLines || !entries[0].Truncated || entries[1].Text != "INFO next" {
			t.Fatalf("entries = %d, first has %d lines truncated=%v", len(entries), entries[0].Lines, entries[0].Truncated)
		}
	})
	t.Run("at most 16 KiB", func(t *testing.T) {
		lines := []string{"ERROR big"}
		for range 40 {
			lines = append(lines, "\tat "+strings.Repeat("x", 1000)+".run(X.java:1)")
		}
		entries := JoinLines(lines)
		if len(entries) != 1 || len(entries[0].Text) > MaxJoinedBytes || !entries[0].Truncated {
			t.Fatalf("entry is %d bytes, truncated=%v", len(entries[0].Text), entries[0].Truncated)
		}
	})
	t.Run("a first line over 16 KiB is cut", func(t *testing.T) {
		entries := JoinLines([]string{strings.Repeat("é", MaxJoinedBytes)})
		if len(entries) != 1 || len(entries[0].Text) > MaxJoinedBytes || !entries[0].Truncated || !strings.HasSuffix(entries[0].Text, "é") {
			t.Fatalf("entry is %d bytes, truncated=%v", len(entries[0].Text), entries[0].Truncated)
		}
	})
}

func TestJoinerStreamsEntriesAsTheyComplete(t *testing.T) {
	var joiner Joiner
	if _, ok := joiner.Flush(); ok {
		t.Fatal("an empty joiner flushed an entry")
	}
	if _, ok := joiner.Add("ERROR first\n"); ok {
		t.Fatal("the first line completed an entry")
	}
	if _, ok := joiner.Add("\tat a.B.c(B.java:1)\r\n"); ok {
		t.Fatal("a frame completed an entry")
	}
	entry, ok := joiner.Add("INFO second")
	if !ok || entry.Text != "ERROR first\n\tat a.B.c(B.java:1)" || entry.Lines != 2 {
		t.Fatalf("entry = %+v, %v", entry, ok)
	}
	last, ok := joiner.Flush()
	if !ok || last.Text != "INFO second" {
		t.Fatalf("last = %+v, %v", last, ok)
	}
	if _, ok := joiner.Flush(); ok {
		t.Fatal("a second flush returned an entry")
	}
	if _, ok := joiner.Add("  indented first line"); ok {
		t.Fatal("an indented first line completed an entry")
	}
	if first, _ := joiner.Flush(); first.Text != "  indented first line" {
		t.Fatalf("an indented first line = %q", first.Text)
	}
}

// The start of a trace folds only into an error line or an open trace; after
// an info line it starts its own entry, and a level padded with a space is a
// new entry, not an indented frame.
func TestJoinerDoesNotFoldATraceIntoAnInfoLine(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []int
		heads []string
	}{
		{
			name: "an uncaught Python exception after an info line",
			lines: []string{
				"2026-11-21 08:44:15,976 - worker - INFO - task started",
				"Traceback (most recent call last):",
				`  File "/app/worker.py", line 12, in run`,
				"ZeroDivisionError: division by zero",
			},
			want:  []int{1, 3},
			heads: []string{"2026-11-21 08:44:15,976 - worker - INFO - task started", "Traceback (most recent call last):"},
		},
		{
			name: "a Java printStackTrace after an info line",
			lines: []string{
				"INFO request received",
				"java.net.ConnectException: Connection refused",
				"\tat java.base/sun.nio.ch.Net.connect0(Native Method)",
				"\tat java.base/sun.nio.ch.Net.connect(Net.java:579)",
				"Caused by: java.io.IOException: reset",
			},
			want:  []int{1, 4},
			heads: []string{"INFO request received", "java.net.ConnectException: Connection refused"},
		},
		{
			name: "log4j levels padded to five characters",
			lines: []string{
				"ERROR [main] - payment failed",
				"java.lang.IllegalStateException: no card",
				"\tat com.example.Pay.run(Pay.java:9)",
				" INFO [main] - next request",
				" WARN [main] - slow",
				"DEBUG [main] - detail",
			},
			want:  []int{3, 1, 1, 1},
			heads: []string{"ERROR [main] - payment failed", " INFO [main] - next request", " WARN [main] - slow", "DEBUG [main] - detail"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries := JoinLines(test.lines)
			counts := make([]int, len(entries))
			for index, entry := range entries {
				counts[index] = entry.Lines
			}
			if fmt.Sprint(counts) != fmt.Sprint(test.want) {
				t.Fatalf("entry line counts = %v, want %v\n%+v", counts, test.want, entries)
			}
			for index, head := range test.heads {
				if first, _, _ := strings.Cut(entries[index].Text, "\n"); first != head {
					t.Fatalf("entry %d starts %q, want %q", index, first, head)
				}
			}
		})
	}
}

func TestPaddedLevelNeedsASeparator(t *testing.T) {
	for text, want := range map[string]bool{
		"INFO [main] - next":  true,
		"WARN: slow":          true,
		"[ERROR] failed":      true,
		"error = total / one": false,
		"error.go:12 failed":  false,
		"errors were counted": false,
	} {
		if got := paddedLevel(text); got != want {
			t.Errorf("paddedLevel(%q) = %v, want %v", text, got, want)
		}
	}
	entries := JoinLines([]string{"INFO config loaded", "    error = 1", "    warn: true"})
	if len(entries) != 2 || entries[0].Lines != 2 {
		t.Fatalf("entries = %+v", entries)
	}
}
