package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"src/**", "src/a.md", true},
		{"src/**", "src/a/b/c.png", true},
		{"src/**", "srcx/a.md", false},
		{"**/*.md", "README.md", true},
		{"**/*.md", "a/b/README.md", true},
		{"**/*.md", "a/b/README.mdx", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"examples/**/README.md", "examples/README.md", true},
		{"examples/**/README.md", "examples/a/b/README.md", true},
		{"cmd/harness_awsendpoint*", "cmd/harness_awsendpoint_test.go", true},
		{"cmd/harness_awsendpoint*", "cmd/main.go", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "x/go.mod", false},
		{"pkg/**/model/*.go", "pkg/aws/model/a.go", true},
		{"pkg/**/model/*.go", "pkg/model/a.go", true},
		{"pkg/**/model/*.go", "pkg/aws/model/sub/a.go", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.file); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %t, want %t", c.pattern, c.file, got, c.want)
		}
	}
}

func TestGroupNegationLastMatchWins(t *testing.T) {
	g := Group{Name: "docs", Paths: []string{"**/*.md", "!pkg/**", "pkg/special/**"}}
	for file, want := range map[string]bool{
		"README.md":              true,
		"pkg/a/README.md":        false,
		"pkg/special/README.md":  true,
		"pkg/special/notes.txt":  true,
		"examples/x/README.md":   true,
		"examples/x/compose.yml": false,
	} {
		if got := g.matches(file); got != want {
			t.Errorf("matches(%q) = %t, want %t", file, got, want)
		}
	}
}

// testConfig is a small map with the same shape as the OSS one: a catch-all
// Go group with exclusions that turns on the other Go groups, a derived image
// group, and an "all" group for CI files.
const testConfig = `{
  "groups": [
    {"name": "docs", "paths": ["src/**", "**/*.md", "!pkg/**"]},
    {"name": "ui", "paths": ["ui/**"], "enables": ["image"]},
    {"name": "go-kube", "paths": ["pkg/kube/**"], "enables": ["image"]},
    {"name": "go-aws", "paths": ["pkg/aws/**"], "enables": ["image"]},
    {"name": "go-shared", "paths": ["go.mod", "pkg/**", "!pkg/kube/**", "!pkg/aws/**"], "enables": ["go-*"]},
    {"name": "image", "paths": ["Dockerfile"]},
    {"name": "ci", "paths": [".github/**"], "all": true}
  ]
}`

func mustConfig(t *testing.T, data string) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func onGroups(sel Selection) []string {
	var on []string
	for name, v := range sel.Groups {
		if v {
			on = append(on, name)
		}
	}
	sort.Strings(on)
	return on
}

func TestSelect(t *testing.T) {
	cfg := mustConfig(t, testConfig)
	everything := []string{"ci", "docs", "go-aws", "go-kube", "go-shared", "image", "ui"}
	cases := []struct {
		name     string
		files    []string
		forceAll bool
		wantOn   []string
		wantFull bool
	}{
		{name: "docs only", files: []string{"src/a.md", "README.md"}, wantOn: []string{"docs"}},
		{name: "markdown inside a package is Go, not docs", files: []string{"pkg/kube/README.md"}, wantOn: []string{"go-kube", "image"}},
		{name: "one feature group", files: []string{"pkg/aws/model/x.go"}, wantOn: []string{"go-aws", "image"}},
		{name: "shared package turns on every Go group", files: []string{"pkg/config/c.go"}, wantOn: []string{"go-aws", "go-kube", "go-shared", "image"}},
		{name: "ui", files: []string{"ui/src/App.tsx"}, wantOn: []string{"image", "ui"}},
		{name: "ci file is full", files: []string{".github/workflows/dev.yml"}, wantOn: everything, wantFull: true},
		{name: "unmatched file is full", files: []string{"src/a.md", "Makefile"}, wantOn: everything, wantFull: true},
		{name: "forced", files: nil, forceAll: true, wantOn: everything, wantFull: true},
		{name: "no changes", files: nil, wantOn: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel := Select(cfg, c.files, c.forceAll, "forced")
			if got := onGroups(sel); !reflect.DeepEqual(got, c.wantOn) {
				t.Errorf("groups on = %v, want %v", got, c.wantOn)
			}
			if sel.Full != c.wantFull {
				t.Errorf("full = %t, want %t", sel.Full, c.wantFull)
			}
			if len(sel.Groups) != len(cfg.Groups) {
				t.Errorf("selection has %d groups, want every configured group (%d)", len(sel.Groups), len(cfg.Groups))
			}
		})
	}
	if sel := Select(cfg, []string{"src/a.md", "Makefile"}, false, ""); !reflect.DeepEqual(sel.Unmatched, []string{"Makefile"}) {
		t.Errorf("unmatched = %v, want [Makefile]", sel.Unmatched)
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":     `{"groups":[{"name":"a","path":["x/**"]}]}`,
		"no groups":         `{"groups":[]}`,
		"bad name":          `{"groups":[{"name":"Go Shared","paths":["x"]}]}`,
		"reserved name":     `{"groups":[{"name":"full","paths":["x"]}]}`,
		"duplicate":         `{"groups":[{"name":"a","paths":["x"]},{"name":"a","paths":["y"]}]}`,
		"absolute path":     `{"groups":[{"name":"a","paths":["/x"]}]}`,
		"partial **":        `{"groups":[{"name":"a","paths":["x/**.go"]}]}`,
		"bad glob":          `{"groups":[{"name":"a","paths":["x/["]}]}`,
		"unknown enables":   `{"groups":[{"name":"a","paths":["x"],"enables":["b"]}]}`,
		"not json":          `groups: []`,
		"trailing slash":    `{"groups":[{"name":"a","paths":["x/"]}]}`,
		"empty negation":    `{"groups":[{"name":"a","paths":["!"]}]}`,
		"name starts digit": `{"groups":[{"name":"1a","paths":["x"]}]}`,
	}
	for name, data := range cases {
		if _, err := ParseConfig([]byte(data)); err == nil {
			t.Errorf("%s: ParseConfig accepted %s", name, data)
		}
	}
}

// runTool runs the command in a scratch directory with no git and no Go
// source, so only the group mapping and the output format are exercised.
func runTool(t *testing.T, githubOutput string, args ...string) (stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "groups.json")
	if err := os.WriteFile(cfgPath, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_OUTPUT", githubOutput)
	var out, errOut strings.Builder
	full := append([]string{"--config", cfgPath, "--module-dir", dir}, args...)
	if err := run(full, &out, &errOut); err != nil {
		t.Fatalf("run %v: %v\nstderr: %s", full, err, errOut.String())
	}
	return out.String(), errOut.String()
}

func TestRunOutputFormat(t *testing.T) {
	listDir := t.TempDir()
	list := filepath.Join(listDir, "files.txt")
	if err := os.WriteFile(list, []byte("src/a.md\n\nREADME.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := runTool(t, "", "--files", list)
	want := "docs=true\nui=false\ngo-kube=false\ngo-aws=false\ngo-shared=false\nimage=false\nci=false\nfull=false\ngo=false\ngo_packages=\n"
	if got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunEmptyOrZeroBaseIsFull(t *testing.T) {
	for _, base := range []string{"", "0000000000000000000000000000000000000000"} {
		got, stderr := runTool(t, "", "--base", base, "--head", "abc123")
		if !strings.Contains(got, "full=true\n") || !strings.Contains(got, "go=true\n") || !strings.Contains(got, "go_packages=./...\n") || strings.Contains(got, "=false") {
			t.Errorf("base %q: want every group on and go_packages=./..., got:\n%s", base, got)
		}
		if !strings.Contains(stderr, "no usable base commit") {
			t.Errorf("base %q: stderr does not explain the full run: %s", base, stderr)
		}
	}
}

func TestRunWritesGitHubOutput(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "github_output")
	if err := os.WriteFile(outFile, []byte("earlier=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := runTool(t, outFile, "--all")
	if stdout != "" {
		t.Errorf("stdout should be empty when GITHUB_OUTPUT is set, got %q", stdout)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "earlier=1\n") {
		t.Errorf("GITHUB_OUTPUT was not appended to: %q", got)
	}
	if !strings.Contains(got, "ci=true\n") || !strings.Contains(got, "full=true\n") || !strings.Contains(got, "go_packages=./...\n") {
		t.Errorf("GITHUB_OUTPUT missing full-run lines: %q", got)
	}
	if !strings.Contains(stderr, "full=true") {
		t.Errorf("the decision is not echoed to the log: %q", stderr)
	}
}

// TestCommittedOSSConfig checks the repository's own group map against the
// scenarios CI depends on. The map is committed source, read in place.
func TestCommittedOSSConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "ci-groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, string(data))
	goGroups := []string{"go-agent", "go-aws", "go-controllers", "go-kubernetes", "go-shared", "go-storage"}

	cases := []struct {
		name     string
		files    []string
		wantOn   []string
		wantFull bool
	}{
		{"docs only", []string{"src/agent/quickstart.md", "README.md", "src/docs/images/a.png", "examples/docker-compose/README.md"}, []string{"docs"}, false},
		{"kubernetes only", []string{"pkg/kubernetes/rbac.go"}, []string{"go-kubernetes", "helm", "image"}, false},
		{"aws model", []string{"pkg/aws/model/cost.go"}, []string{"go-aws", "image"}, false},
		{"aws endpoint harness file", []string{"cmd/harness_awsendpoint.go"}, []string{"go-aws", "image"}, false},
		{"config turns on all Go groups and the chart", []string{"pkg/config/config.go"}, append(append([]string{}, goGroups...), "helm", "image"), false},
		{"go.mod", []string{"go.mod"}, append(append([]string{}, goGroups...), "image"), false},
		{"chart only", []string{"helm/versus-incident/values.yaml"}, []string{"helm"}, false},
		{"ui only", []string{"ui/src/App.tsx"}, []string{"image", "ui"}, false},
		{"routes", []string{"pkg/routes/api.go"}, []string{"go-controllers", "image"}, false},
		{"workflow", []string{".github/workflows/dev.yml"}, nil, true},
		{"the change tool itself", []string{"tools/cichanges/main.go"}, nil, true},
		{"docs build tool and its workflow", []string{"tools/devdocs/sidebar.go", ".github/workflows/docs.yml"}, []string{"docs", "docs-build"}, false},
		{"docs page list", []string{".github/dev-docs.json"}, []string{"docs", "docs-build"}, false},
		{"docs workflow beside the main workflow", []string{".github/workflows/docs.yml", ".github/workflows/dev.yml"}, nil, true},
		{"group map", []string{".github/ci-groups.json"}, nil, true},
		{"other workflow", []string{".github/workflows/release.yaml"}, nil, true},
		{"link checker", []string{"tools/docscheck/main.go"}, nil, true},
		{"test lint", []string{"tools/testlint/lint.go"}, nil, true},
		{"race shard script and lists", []string{"scripts/ci-tests.sh", ".github/race-shards/kubernetes-rbac.txt"}, nil, true},
		{"local CI script", []string{"scripts/ci-local.sh"}, nil, true},
		{"root ignore list only", []string{".gitignore"}, []string{"metadata"}, false},
		{"editor settings and a stray macOS file", []string{".editorconfig", ".DS_Store", "examples/.DS_Store"}, []string{"docs", "metadata"}, false},
		{"ignore list inside the ui keeps the ui group", []string{"ui/.gitignore"}, []string{"image", "metadata", "ui"}, false},
		{"github forms and code owners", []string{".github/ISSUE_TEMPLATE/bug_report.yml", ".github/pull_request_template.md", ".github/FUNDING.yml", ".github/CODEOWNERS", "CODEOWNERS"}, []string{"metadata"}, false},
		{"image ignore list builds the image", []string{".dockerignore"}, []string{"image"}, false},
		{"dependabot config is still CI", []string{".github/dependabot.yml"}, nil, true},
		{"git attributes can change checked-out bytes", []string{".gitattributes"}, nil, true},
		{"unknown root file", []string{"src/a.md", "Makefile"}, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel := Select(cfg, c.files, false, "")
			if sel.Full != c.wantFull {
				t.Fatalf("full = %t (%s), want %t", sel.Full, sel.Reason, c.wantFull)
			}
			if c.wantFull {
				return
			}
			want := append([]string{}, c.wantOn...)
			sort.Strings(want)
			if got := onGroups(sel); !reflect.DeepEqual(got, want) {
				t.Errorf("groups on = %v, want %v", got, want)
			}
		})
	}
}

// TestCommittedOSSConfigDocsAndMetadataPush replays a merged docs change that
// also edited the root ignore list and deleted a committed macOS folder file.
// It runs the tool on the committed group map and then applies the
// workflow's own not_code pattern, read from dev.yml, the way the push-to-main
// step does: no group outside not_code may be on, or main reruns everything.
func TestCommittedOSSConfigDocsAndMetadataPush(t *testing.T) {
	files := []string{
		".gitignore",
		"src/_sidebar.md",
		"src/docs/.DS_Store",
		"src/docs/images/anomaly-baseline-ewma.svg",
		"src/docs/images/anomaly-grouping.svg",
		"src/docs/images/anomaly-hard-to-catch.svg",
		"src/docs/images/anomaly-learning-timeline.svg",
		"src/docs/images/anomaly-memory-trend.svg",
		"src/docs/images/anomaly-sibling-mad.svg",
		"src/docs/images/anomaly-six-layers.svg",
		"src/docs/images/anomaly-version-compare.svg",
		"src/docs/images/anomaly-worker-ha.svg",
		"src/in-development/kubernetes-anomaly-detection.md",
	}
	notCode := workflowNotCode(t)

	decide := func(t *testing.T, files []string) string {
		t.Helper()
		dir := t.TempDir()
		list := filepath.Join(dir, "files.txt")
		if err := os.WriteFile(list, []byte(strings.Join(files, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfgPath, err := filepath.Abs(filepath.Join("..", "..", ".github", "ci-groups.json"))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("GITHUB_OUTPUT", "")
		var out, errOut strings.Builder
		// The module dir holds no Go source, so the package list stays empty.
		args := []string{"--config", cfgPath, "--module-dir", t.TempDir(), "--files", list}
		if err := run(args, &out, &errOut); err != nil {
			t.Fatalf("run: %v\nstderr: %s", err, errOut.String())
		}
		return out.String()
	}
	codeGroupsOn := func(decision string) []string {
		var on []string
		for _, line := range strings.Split(strings.TrimSpace(decision), "\n") {
			if !notCode.MatchString(line) && strings.HasSuffix(line, "=true") {
				on = append(on, line)
			}
		}
		return on
	}

	t.Run("merged docs change", func(t *testing.T) {
		got := decide(t, files)
		for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
			name, value, _ := strings.Cut(line, "=")
			want := "false"
			switch name {
			case "docs", "metadata":
				want = "true"
			case "go_packages":
				want = ""
			}
			if value != want {
				t.Errorf("%s=%s, want %s\nfull decision:\n%s", name, value, want, got)
			}
		}
		if on := codeGroupsOn(got); len(on) != 0 {
			t.Errorf("push to main would rerun the full suite; code groups on: %v", on)
		}
	})

	t.Run("ignore list only", func(t *testing.T) {
		got := decide(t, []string{".gitignore"})
		if !strings.Contains(got, "metadata=true\n") || !strings.Contains(got, "full=false\n") || !strings.Contains(got, "go=false\n") {
			t.Errorf("want metadata only, no full run and no Go jobs, got:\n%s", got)
		}
		if on := codeGroupsOn(got); len(on) != 0 {
			t.Errorf("an ignore-list change counts as code: %v", on)
		}
	})

	t.Run("unclaimed file still runs everything", func(t *testing.T) {
		cfg := mustConfig(t, readCommittedConfig(t))
		sel := Select(cfg, []string{"src/_sidebar.md", ".gitignore", "build.unknown"}, false, "")
		if !sel.Full || !reflect.DeepEqual(sel.Unmatched, []string{"build.unknown"}) {
			t.Errorf("full = %t, unmatched = %v, want a full run blamed on build.unknown", sel.Full, sel.Unmatched)
		}
		if on := codeGroupsOn(decide(t, []string{"build.unknown"})); len(on) == 0 {
			t.Error("a full run is not seen as code by the workflow's not_code check")
		}
	})
}

// workflowNotCode reads the not_code pattern from the committed workflow so
// the test and the push-to-main check cannot drift apart.
func workflowNotCode(t *testing.T) *regexp.Regexp {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "dev.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*not_code='([^']*)'\s*$`).FindSubmatch(data)
	if m == nil {
		t.Fatal("dev.yml has no not_code='...' line")
	}
	re, err := regexp.Compile(string(m[1]))
	if err != nil {
		t.Fatalf("not_code pattern %q: %v", m[1], err)
	}
	return re
}

func readCommittedConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "ci-groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestCommittedOSSConfigDocsBuild follows a change to the docs site build
// through every output the workflow reads: the link check runs, the tool's
// own package is the only one tested, and no Go group (which picks the race
// shards and the image) switches on. The package list is what go list
// reports for the tool and a few module packages around it.
func TestCommittedOSSConfigDocsBuild(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "ci-groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, string(data))
	var pkgs []ListPackage
	pkgs = append(pkgs, ListPackage{ImportPath: "fmt", Name: "fmt", Standard: true})
	pkgs = append(pkgs, pkgEntries("main", "tools/devdocs", []string{"fmt"}, nil, func(lp *ListPackage) {
		lp.GoFiles = []string{"config.go", "main.go", "page.go", "sidebar.go", "site.go", "tag.go"}
		lp.TestGoFiles = []string{"devdocs_test.go"}
	})...)
	pkgs = append(pkgs, pkgEntries("main", "tools/cichanges", []string{"fmt"}, nil, nil)...)
	pkgs = append(pkgs, pkgEntries("main", "tools/docscheck", []string{"fmt"}, nil, nil)...)
	pkgs = append(pkgs, pkgEntries("config", "pkg/config", []string{"fmt"}, nil, nil)...)
	pkgs = append(pkgs, pkgEntries("main", "cmd", []string{p("pkg/config"), "fmt"}, nil, nil)...)

	files := []string{"tools/devdocs/sidebar.go", ".github/workflows/docs.yml"}
	sel := Select(cfg, files, false, "")
	if sel.Full {
		t.Fatalf("full run (%s), want only the docs groups", sel.Reason)
	}
	if got, want := onGroups(sel), []string{"docs", "docs-build"}; !reflect.DeepEqual(got, want) {
		t.Errorf("groups on = %v, want %v", got, want)
	}
	for name, on := range sel.Groups {
		if on && strings.HasPrefix(name, "go-") {
			t.Errorf("Go group %s is on; it would start a race shard", name)
		}
	}
	goPkgs := GoPackages(files, pkgs)
	if want := []string{p("tools/devdocs")}; !reflect.DeepEqual(goPkgs, want) {
		t.Errorf("go_packages = %v, want %v", goPkgs, want)
	}
	if !GoChanged(sel, goPkgs) {
		t.Error("go = false: the Test job would skip the tool's tests and vet")
	}

	// The workflow and page list alone touch no Go package, so the Test job
	// stays off.
	listOnly := []string{".github/workflows/docs.yml", ".github/dev-docs.json"}
	sel = Select(cfg, listOnly, false, "")
	if goPkgs := GoPackages(listOnly, pkgs); len(goPkgs) != 0 || GoChanged(sel, goPkgs) {
		t.Errorf("workflow and page list only: go_packages = %v, go = %t, want none and false", goPkgs, GoChanged(sel, goPkgs))
	}
}

// TestNegationInAllGroup checks that a "!" pattern takes a file out of an
// "all" group, and that the file then needs another group to claim it.
func TestNegationInAllGroup(t *testing.T) {
	cfg := mustConfig(t, `{
  "groups": [
    {"name": "docs", "paths": ["src/**"]},
    {"name": "site", "paths": ["tools/site/**", ".github/workflows/site.yml"], "enables": ["docs"]},
    {"name": "ci", "paths": [".github/**", "!.github/workflows/site.yml"], "all": true}
  ]
}`)
	sel := Select(cfg, []string{".github/workflows/site.yml"}, false, "")
	if sel.Full || !reflect.DeepEqual(onGroups(sel), []string{"docs", "site"}) {
		t.Errorf("excluded workflow: full = %t (%s), on = %v, want docs and site only", sel.Full, sel.Reason, onGroups(sel))
	}
	if sel := Select(cfg, []string{".github/workflows/ci.yml"}, false, ""); !sel.Full {
		t.Error("another workflow is no longer a full run")
	}
	bare := mustConfig(t, `{"groups": [{"name": "ci", "paths": [".github/**", "!.github/workflows/site.yml"], "all": true}]}`)
	if sel := Select(bare, []string{".github/workflows/site.yml"}, false, ""); !sel.Full || !reflect.DeepEqual(sel.Unmatched, []string{".github/workflows/site.yml"}) {
		t.Errorf("an excluded file no group claims: full = %t, unmatched = %v, want a full run", sel.Full, sel.Unmatched)
	}
}

func TestGoChanged(t *testing.T) {
	cfg := mustConfig(t, testConfig)
	cases := []struct {
		name     string
		files    []string
		packages []string
		want     bool
	}{
		{"docs only", []string{"src/a.md"}, nil, false},
		{"ui source only", []string{"ui/src/App.tsx"}, nil, false},
		// Go inside the UI folder: no go- group is on, but a package changed.
		{"go file in the ui folder", []string{"ui/embed.go"}, []string{"example.com/m/ui"}, true},
		{"go group with no package to test", []string{"pkg/aws/fixtures/a.json"}, nil, true},
		{"full run", []string{".github/workflows/dev.yml"}, []string{"./..."}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := GoChanged(Select(cfg, c.files, false, ""), c.packages); got != c.want {
				t.Errorf("GoChanged = %t, want %t", got, c.want)
			}
		})
	}
}
