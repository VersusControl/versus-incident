package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"5 * time.Second":        5 * time.Second,
		"5*time.Second":          5 * time.Second,
		"time.Second":            time.Second,
		"250 * time.Millisecond": 250 * time.Millisecond,
		"time.Millisecond * 250": 250 * time.Millisecond,
		"2 * time.Minute":        2 * time.Minute,
		"1.5 * time.Second":      1500 * time.Millisecond,
	}
	for expr, want := range cases {
		if got, ok := parseDuration(expr); !ok || got != want {
			t.Errorf("parseDuration(%q) = %s, %t; want %s", expr, got, ok, want)
		}
	}
	for _, expr := range []string{"timeout", "scaledTimeout(5 * time.Second)", "d * time.Second"} {
		if _, ok := parseDuration(expr); ok {
			t.Errorf("parseDuration(%q) accepted a non-literal", expr)
		}
	}
}

func TestCheckLine(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		{"\ttime.Sleep(10 * time.Millisecond)", 1},
		{"\ttime.Sleep(d)", 1},
		{"\ttime.Sleep(10 * time.Millisecond) // testlint:allow the ticker must fire once", 0},
		{"\t// time.Sleep(time.Second) used to be here", 0},
		{"\tt.Log(\"http://x\"); time.Sleep(d)", 1},
		{"\tcase <-time.After(2 * time.Second):", 1},
		{"\tcase <-time.After(time.Minute):", 0},
		{"\tcase <-time.After(30 * time.Second):", 0},
		{"\tcase <-time.After(scaledTimeout(2 * time.Second)):", 0},
		{"\tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)", 1},
		{"\tctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)", 1},
		{"\tctx, cancel := context.WithTimeout(ctx, 45*time.Second)", 0},
		{"\tctx, cancel := context.WithTimeout(ctx, timeout)", 0},
		{"\tctx, cancel := context.WithTimeout(ctx, 5*time.Second) // testlint:allow bounds a refused dial", 0},
		{"\ttime.Sleep(time.Second); <-time.After(time.Second)", 2},
	}
	for _, c := range cases {
		if got := CheckLine(c.line); len(got) != c.want {
			t.Errorf("CheckLine(%q) = %q, want %d finding(s)", c.line, got, c.want)
		}
	}
}

// testDiff is a git diff -U0 with one changed test file, one new test file,
// a deleted test file and a non-test file.
const testDiff = `diff --git a/pkg/a/a_test.go b/pkg/a/a_test.go
index 1111111..2222222 100644
--- a/pkg/a/a_test.go
+++ b/pkg/a/a_test.go
@@ -10,0 +11,2 @@ func TestA(t *testing.T) {
+	time.Sleep(time.Second)
+	ok := true
@@ -40 +42 @@ func TestB(t *testing.T) {
-	<-time.After(time.Minute)
+	<-time.After(3 * time.Second)
diff --git a/pkg/a/new_test.go b/pkg/a/new_test.go
new file mode 100644
--- /dev/null
+++ b/pkg/a/new_test.go
@@ -0,0 +1,3 @@
+package a
+
+var _ = func() { time.Sleep(1) }
diff --git a/pkg/a/old_test.go b/pkg/a/old_test.go
deleted file mode 100644
--- a/pkg/a/old_test.go
+++ /dev/null
@@ -1,1 +0,0 @@
-	time.Sleep(time.Second)
diff --git a/pkg/a/a.go b/pkg/a/a.go
--- a/pkg/a/a.go
+++ b/pkg/a/a.go
@@ -1 +1 @@
+	time.Sleep(time.Second)
`

func TestCheckDiff(t *testing.T) {
	findings, err := CheckDiff(strings.NewReader(testDiff))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.File+":"+strconv.Itoa(f.Line))
	}
	want := []string{"pkg/a/a_test.go:11", "pkg/a/a_test.go:42", "pkg/a/new_test.go:3"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("findings at %v, want %v", got, want)
	}
}

func TestRunExitsWithFindings(t *testing.T) {
	var out strings.Builder
	err := run([]string{"--diff", "-"}, strings.NewReader(testDiff), &out)
	if !errors.Is(err, errFindings) {
		t.Fatalf("run error = %v, want findings", err)
	}
	if !strings.Contains(out.String(), "::error file=pkg/a/a_test.go,line=11::") {
		t.Errorf("no annotation for the sleep:\n%s", out.String())
	}
	out.Reset()
	if err := run([]string{"--diff", "-"}, strings.NewReader("+++ b/x_test.go\n@@ -0,0 +1 @@\n+ok := 1\n"), &out); err != nil {
		t.Errorf("clean diff: %v", err)
	}
	if err := run([]string{"--base", ""}, strings.NewReader(""), &out); err != nil {
		t.Errorf("empty base: %v", err)
	}
}
