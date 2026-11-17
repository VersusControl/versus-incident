// Command devdocs builds the docs site from main's pages plus the pages that
// exist only on dev, listed in .github/dev-docs.json.
//
//	devdocs validate --config .github/dev-docs.json --dev <dev>/src
//	devdocs build    --config <json> --main <main>/src [--dev <dev>/src] --tag dev-<sha> --out <dir>
//	devdocs pick-tag --versions <package versions JSON> --commits <rev-list file>
//	devdocs affected --config <json> --files <changed files, one per line>
//
// build copies main's site to --out, then for each listed page copies it
// from dev, with any linked file under docs/images that main lacks, puts an
// "In development" badge under its title, points every tagged reference to the configured image at --tag, and
// adds the sidebar entries main's sidebar lacks (when dev has a sidebar, only
// for the pages it lists). Without --dev it copies main's site alone. The repository files are never changed. validate is a
// local check that rejects unknown keys in the list; build warns about them
// and goes on, and fails only when a listed page is missing on dev.
//
// pick-tag prints the dev-<sha> tag whose commit is newest on dev, or "dev"
// with a warning when the registry has no such tag. affected prints true when
// a change to dev can change the site.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "devdocs:", err)
		os.Exit(1)
	}
}

const usage = "usage: devdocs validate|build|pick-tag|affected [flags]"

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "build":
		return runBuild(args[1:], stdout, stderr)
	case "pick-tag":
		return runPickTag(args[1:], stdout, stderr)
	case "affected":
		return runAffected(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q; %s", args[0], usage)
	}
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("devdocs "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func loadConfig(name string) (*Config, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return ParseConfig(data)
}

func runValidate(args []string, stdout, stderr io.Writer) error {
	f := newFlags("validate", stderr)
	configPath := f.String("config", ".github/dev-docs.json", "dev docs list")
	devDir := f.String("dev", "", "dev branch src/ folder")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *devDir == "" {
		return errors.New("validate needs --dev")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	pages, skipped, err := SelectPages(cfg, os.DirFS(*devDir))
	for _, s := range skipped {
		fmt.Fprintf(stderr, "::warning::Dev page not used: %s\n", s)
	}
	if err != nil {
		return err
	}
	for _, p := range pages {
		fmt.Fprintln(stdout, p.Path)
	}
	fmt.Fprintf(stderr, "devdocs: %d dev page(s) found\n", len(pages))
	return nil
}

var validTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func runBuild(args []string, stdout, stderr io.Writer) error {
	f := newFlags("build", stderr)
	configPath := f.String("config", ".github/dev-docs.json", "dev docs list")
	mainDir := f.String("main", "", "main branch src/ folder")
	devDir := f.String("dev", "", "dev branch src/ folder; empty builds main alone")
	tag := f.String("tag", "", "image tag for dev pages, such as dev-1a2b3c4")
	outDir := f.String("out", "", "folder to write the site to; must not exist or be empty")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *mainDir == "" || *outDir == "" {
		return errors.New("build needs --main and --out")
	}
	if err := emptyOrMissing(*outDir); err != nil {
		return err
	}
	site, err := readTree(*mainDir)
	if err != nil {
		return fmt.Errorf("read main site: %w", err)
	}
	if *devDir == "" {
		fmt.Fprintln(stderr, "devdocs: no dev tree given; the site is main's pages alone")
		return writeTree(*outDir, site)
	}
	if !validTag.MatchString(*tag) {
		return fmt.Errorf("--tag %q is not a valid image tag", *tag)
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	cfg, unknown, err := ParseConfigLenient(data)
	if err != nil {
		return err
	}
	for _, k := range unknown {
		fmt.Fprintf(stderr, "::warning::%s: unknown key %s ignored by this docs build\n", *configPath, k)
	}
	report, err := Overlay(cfg, site, os.DirFS(*devDir), *tag)
	if err != nil {
		return err
	}
	if err := writeTree(*outDir, site); err != nil {
		return err
	}
	printReport(stdout, stderr, report, cfg.Image, *tag)
	return nil
}

func printReport(stdout, stderr io.Writer, r *Report, image, tag string) {
	fmt.Fprintf(stdout, "dev pages: %d (%d added, %d replacing main's copy)\n", len(r.Pages), len(r.AddedPages), len(r.ReplacedPages))
	for _, p := range r.Pages {
		fmt.Fprintf(stdout, "  %s", p)
		if n := r.Rewrites[p]; n > 0 {
			fmt.Fprintf(stdout, " (%d image reference(s) -> %s:%s)", n, image, tag)
		}
		fmt.Fprintln(stdout)
	}
	fmt.Fprintf(stdout, "linked files copied from dev: %d\n", len(r.Assets))
	for _, p := range r.SidebarAdded {
		fmt.Fprintf(stdout, "sidebar entry added: %s\n", p)
	}
	for _, p := range r.NotInSidebar {
		fmt.Fprintf(stdout, "no sidebar entry (dev's sidebar does not list it): %s\n", p)
	}
	for _, p := range r.NoHeading {
		fmt.Fprintf(stderr, "::warning file=src/%s::No \"# \" title on this dev page, so it has no In development badge\n", p)
	}
	for _, p := range r.MissingAssets {
		fmt.Fprintf(stderr, "devdocs: %s is linked from a dev page but is not on dev\n", p)
	}
	for _, p := range r.KeptMain {
		fmt.Fprintf(stdout, "linked file kept from main: %s\n", p)
	}
	for _, p := range r.NotCopied {
		fmt.Fprintf(stdout, "linked file not taken from dev (only new files under docs/images are): %s\n", p)
	}
	for _, p := range r.Skipped {
		fmt.Fprintf(stderr, "::warning::Not taken from dev: %s\n", p)
	}
}

func runPickTag(args []string, stdout, stderr io.Writer) error {
	f := newFlags("pick-tag", stderr)
	versionsPath := f.String("versions", "", "package versions JSON from the GitHub packages API")
	commitsPath := f.String("commits", "", "dev history, one commit SHA per line, newest first")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *versionsPath == "" || *commitsPath == "" {
		return errors.New("pick-tag needs --versions and --commits")
	}
	vf, err := os.Open(*versionsPath)
	if err != nil {
		return err
	}
	defer vf.Close()
	tags, err := ParseVersions(vf)
	if err != nil {
		return err
	}
	commits, err := readLines(*commitsPath)
	if err != nil {
		return err
	}
	tag, ok := PickDevTag(tags, commits)
	if !ok {
		fmt.Fprintf(stderr, "::warning::No dev-<sha> image tag in the registry matches a commit on dev; dev pages use :%s\n", FallbackTag)
		tag = FallbackTag
	}
	fmt.Fprintln(stdout, tag)
	return nil
}

func runAffected(args []string, stdout, stderr io.Writer) error {
	f := newFlags("affected", stderr)
	configPath := f.String("config", ".github/dev-docs.json", "dev docs list")
	filesPath := f.String("files", "", "changed files, one per line")
	if err := f.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	files, err := readLines(*filesPath)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, Affected(cfg, files))
	return nil
}

func readLines(name string) ([]string, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []string
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

func emptyOrMissing(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("output folder %s is not empty", dir)
	}
	return nil
}

func readTree(dir string) (Files, error) {
	site := Files{}
	root := os.DirFS(dir)
	err := fs.WalkDir(root, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		site[name] = data
		return nil
	})
	return site, err
}

func writeTree(dir string, site Files) error {
	names := make([]string, 0, len(site))
	for n := range site {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		target := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, site[n], 0o644); err != nil {
			return err
		}
	}
	return nil
}
