// Command cichanges works out which CI job groups a change needs.
//
// It lists the files changed between two commits with
// `git diff --name-only base...head`, maps them onto the feature groups in a
// JSON group map, and writes one `<group>=true|false` line per group, then
// `full=true|false`, `go=true|false` and `go_packages=<import paths>`, to
// $GITHUB_OUTPUT (or stdout when that is unset). go is true when the Go jobs
// must run: a full run, a package to test, or a "go-" group switched on. Every group switches on, and go_packages is
// ./..., when --all is set, the base is empty or all zeros, git fails, a
// changed file matches no group, or a changed file is in an "all" group.
//
//	go run ./tools/cichanges --config .github/ci-groups.json \
//	  --base <sha> --head <sha> [--all] [--module-dir .]
//
// --module-dir names the Go module to inspect and the git repository to diff
// (the repository that holds it); it defaults to the current directory. Group
// paths are relative to that repository's root. --files reads the changed
// file list, relative to --module-dir, from a file ("-" for stdin) instead of
// git, for local dry runs.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "cichanges:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("cichanges", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", ".github/ci-groups.json", "group map (JSON)")
	base := fs.String("base", "", "base commit; empty or all zeros runs everything")
	head := fs.String("head", "HEAD", "head commit")
	all := fs.Bool("all", false, "switch on every group")
	moduleDir := fs.String("module-dir", ".", "Go module directory, inside the git repository to diff")
	filesFrom := fs.String("files", "", "read changed files from this file (\"-\" for stdin) instead of git")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return err
	}
	modAbs, err := filepath.Abs(*moduleDir)
	if err != nil {
		return err
	}
	// git reports the repository root with symlinks resolved; match it.
	if resolved, rerr := filepath.EvalSymlinks(modAbs); rerr == nil {
		modAbs = resolved
	}

	forceAll, reason := *all, "--all is set"
	var files []string
	repoRoot := modAbs
	switch {
	case forceAll:
	case *filesFrom != "":
		// No git here: the listed paths are relative to --module-dir.
		files, err = readFileList(*filesFrom)
		if err != nil {
			return err
		}
	case strings.Trim(*base, "0") == "":
		forceAll, reason = true, "no usable base commit (first push, or a run with no range)"
	default:
		top, terr := gitToplevel(modAbs)
		if terr == nil {
			repoRoot = top
			files, terr = gitChangedFiles(top, *base, *head)
		}
		if terr != nil {
			fmt.Fprintf(stderr, "cichanges: git failed, running everything: %v\n", terr)
			forceAll, reason = true, "git could not list the changed files"
		}
	}

	sel := Select(cfg, files, forceAll, reason)
	var goPkgs []string
	if sel.Full {
		goPkgs = []string{allPackages}
	} else {
		goPkgs = goPackagesFor(repoRoot, modAbs, files, stderr)
	}

	fmt.Fprintf(stderr, "cichanges: %d changed file(s)\n", len(files))
	if sel.Full {
		fmt.Fprintf(stderr, "cichanges: full run: %s\n", sel.Reason)
	}
	for _, f := range sel.Unmatched {
		fmt.Fprintf(stderr, "cichanges: no group claims %s\n", f)
	}

	var out bytes.Buffer
	for _, g := range cfg.Groups {
		fmt.Fprintf(&out, "%s=%t\n", g.Name, sel.Groups[g.Name])
	}
	fmt.Fprintf(&out, "full=%t\n", sel.Full)
	fmt.Fprintf(&out, "go=%t\n", GoChanged(sel, goPkgs))
	fmt.Fprintf(&out, "go_packages=%s\n", strings.Join(goPkgs, " "))

	if path := os.Getenv("GITHUB_OUTPUT"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.Write(out.Bytes()); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		// Echo to the log as well, so the job shows what was decided.
		_, err = stderr.Write(out.Bytes())
		return err
	}
	_, err = stdout.Write(out.Bytes())
	return err
}

// goPackagesFor runs go list only when a change could touch Go, and falls back
// to ./... when go list fails, so a broken listing never narrows the tests.
func goPackagesFor(repoRoot, modAbs string, files []string, stderr io.Writer) []string {
	modRel, err := filepath.Rel(repoRoot, modAbs)
	if err != nil || strings.HasPrefix(modRel, "..") {
		fmt.Fprintf(stderr, "cichanges: module %s is outside repository %s, testing every package\n", modAbs, repoRoot)
		return []string{allPackages}
	}
	modFiles := ModuleRelative(filepath.ToSlash(modRel), files)
	if TouchesModuleFile(modFiles) {
		return []string{allPackages}
	}
	if !MayTouchGo(modFiles, func(dir string) bool { return dirHasGo(filepath.Join(modAbs, filepath.FromSlash(dir))) }) {
		return nil
	}
	pkgs, err := goList(modAbs)
	if err != nil {
		fmt.Fprintf(stderr, "cichanges: go list failed, testing every package: %v\n", err)
		return []string{allPackages}
	}
	return GoPackages(modFiles, pkgs)
}

func dirHasGo(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

func readFileList(name string) ([]string, error) {
	var r io.Reader = os.Stdin
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var files []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			files = append(files, line)
		}
	}
	return files, sc.Err()
}

func gitToplevel(dir string) (string, error) {
	out, err := command(dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitChangedFiles lists files changed on head since it forked from base.
// Renames are split into a delete and an add so both paths count.
func gitChangedFiles(repoRoot, base, head string) ([]string, error) {
	out, err := command(repoRoot, "git", "diff", "--name-only", "--no-renames", "-z", base+"..."+head, "--")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// goList reads `go list -e -deps -test -json ./...` and makes each module
// package's directory relative to the module root.
func goList(modAbs string) ([]ListPackage, error) {
	out, err := command(modAbs, "go", "list", "-e", "-deps", "-test", "-json", "./...")
	if err != nil {
		return nil, err
	}
	var pkgs []ListPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p ListPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		if p.Module != nil && p.Module.Main && p.Dir != "" {
			rel, err := filepath.Rel(modAbs, p.Dir)
			if err != nil {
				return nil, err
			}
			p.Dir = filepath.ToSlash(rel)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

func command(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
