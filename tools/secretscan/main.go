// Command secretscan fails when a code, config or docs file holds a literal
// shaped like a real credential: an AWS access key ID or secret access key, a
// GitHub, Slack or OpenAI token, a JWT, or a private key block. Public secret
// scanners raise an alert for such a literal even when it is made up, so a
// test builds the value it needs at runtime and docs use the AWS-documented
// examples or a placeholder such as <access-key-id>.
//
//	go run ./tools/secretscan [--root <dir>]
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "secretscan:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("secretscan", flag.ContinueOnError)
	root := flags.String("root", ".", "directory to scan")
	if err := flags.Parse(args); err != nil {
		return err
	}
	found, err := scanTree(os.DirFS(*root))
	if err != nil {
		return err
	}
	for _, f := range found {
		fmt.Fprintf(stdout, "%s:%d: %s %q; build the value at runtime, or use an AWS-documented example or a placeholder\n", f.Path, f.Line, f.Rule, f.Match)
	}
	if len(found) > 0 {
		return fmt.Errorf("%d credential-shaped literals", len(found))
	}
	return nil
}
