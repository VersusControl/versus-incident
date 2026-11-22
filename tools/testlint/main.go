// Command testlint flags new wall-clock waits in changed Go test files: a
// time.Sleep, or a time.After or context WithTimeout with a literal bound
// under 30 seconds. Only added lines are checked, so existing tests are left
// alone until they are touched. A line carrying "testlint:allow" passes.
//
//	go run ./tools/testlint --base <commit> [--head <commit>]
//	go run ./tools/testlint --diff <file|->
//
// With --base and no --head the working tree is compared with base.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "testlint:", err)
		os.Exit(1)
	}
}

// errFindings marks a run that completed and found problems.
var errFindings = fmt.Errorf("new wall-clock waits in tests")

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("testlint", flag.ContinueOnError)
	base := fs.String("base", "", "base commit")
	head := fs.String("head", "", "head commit; empty compares the working tree with base")
	diffFrom := fs.String("diff", "", "read a unified diff from this file (\"-\" for stdin) instead of git")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var diff io.Reader
	switch {
	case *diffFrom == "-":
		diff = stdin
	case *diffFrom != "":
		data, err := os.ReadFile(*diffFrom)
		if err != nil {
			return err
		}
		diff = bytes.NewReader(data)
	case strings.Trim(*base, "0") == "":
		fmt.Fprintln(stdout, "testlint: no base commit, nothing to compare")
		return nil
	default:
		rev := *base
		if *head != "" {
			rev = *base + "..." + *head
		}
		out, err := exec.Command("git", "diff", "-U0", "--no-renames", "--no-color", rev, "--", "*_test.go").Output()
		if err != nil {
			// A force-push can leave a base that shares no history with head.
			// The lint is advisory about new lines only, so it steps aside.
			fmt.Fprintf(stdout, "::warning::testlint: git diff %s failed, nothing checked: %v\n", rev, err)
			return nil
		}
		diff = bytes.NewReader(out)
	}
	findings, err := CheckDiff(diff)
	if err != nil {
		return err
	}
	for _, f := range findings {
		// GitHub annotation; plain file:line text elsewhere is still readable.
		fmt.Fprintf(stdout, "::error file=%s,line=%d::%s\n", f.File, f.Line, f.Message)
	}
	if len(findings) > 0 {
		return fmt.Errorf("%w: %d line(s)", errFindings, len(findings))
	}
	return nil
}
