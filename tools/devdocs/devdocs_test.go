package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

const image = "ghcr.io/versuscontrol/versus-incident"

const testConfig = `{
  "image": "ghcr.io/versuscontrol/versus-incident",
  "pages": [
    { "path": "in-development/**", "sidebar": { "group": "In Development" } },
    { "path": "agent/aws/quickstart.md", "sidebar": { "group": "AWS", "title": "Quickstart" } }
  ]
}`

func mustConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key": `{"image":"ghcr.io/o/n","pages":[{"path":"a.md"}],"extra":1}`,
		"unknown page key":      `{"image":"ghcr.io/o/n","pages":[{"path":"a.md","title":"x"}]}`,
		"unknown sidebar key":   `{"image":"ghcr.io/o/n","pages":[{"path":"a.md","sidebar":{"group":"G","order":1}}]}`,
		"image with tag":        `{"image":"ghcr.io/o/n:latest","pages":[{"path":"a.md"}]}`,
		"image without owner":   `{"image":"n","pages":[{"path":"a.md"}]}`,
		"no pages":              `{"image":"ghcr.io/o/n","pages":[]}`,
		"absolute path":         `{"image":"ghcr.io/o/n","pages":[{"path":"/a.md"}]}`,
		"parent path":           `{"image":"ghcr.io/o/n","pages":[{"path":"../a.md"}]}`,
		"partial **":            `{"image":"ghcr.io/o/n","pages":[{"path":"a/**.md"}]}`,
		"duplicate":             `{"image":"ghcr.io/o/n","pages":[{"path":"a.md"},{"path":"a.md"}]}`,
		"sidebar without group": `{"image":"ghcr.io/o/n","pages":[{"path":"a.md","sidebar":{"title":"A"}}]}`,
		"trailing data":         `{"image":"ghcr.io/o/n","pages":[{"path":"a.md"}]} {}`,
	}
	for name, data := range cases {
		if _, err := ParseConfig([]byte(data)); err == nil {
			t.Errorf("%s: accepted %s", name, data)
		}
	}
}

func TestCommittedConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "dev-docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image != image {
		t.Errorf("image = %q, want %q", cfg.Image, image)
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"in-development/**", "in-development/aws-harness.md", true},
		{"in-development/**", "in-development/sub/page.md", true},
		{"in-development/**", "in-developmentx/page.md", false},
		{"agent/aws/quickstart.md", "agent/aws/quickstart.md", true},
		{"agent/aws/quickstart.md", "agent/aws/quickstart-old.md", false},
		{"agent/*/quickstart.md", "agent/aws/quickstart.md", true},
		{"agent/*/quickstart.md", "agent/aws/x/quickstart.md", false},
		{"**/quickstart.md", "quickstart.md", true},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.file); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %t, want %t", c.pattern, c.file, got, c.want)
		}
	}
}

func devTree() fstest.MapFS {
	return fstest.MapFS{
		"in-development/aws-harness.md":   {Data: []byte("# AWS Harness\n\nSee ![map](../docs/images/aws-map.png) and [quickstart](/agent/aws/quickstart).\n")},
		"in-development/aws-finops.md":    {Data: []byte("# AWS FinOps Agent\n\nRun `ghcr.io/versuscontrol/versus-incident:latest`.\n")},
		"agent/aws/quickstart.md":         {Data: []byte(quickstart)},
		"agent/other.md":                  {Data: []byte("# Not listed\n")},
		"docs/images/aws-map.png":         {Data: []byte("png-map")},
		"docs/images/aws-dark.png":        {Data: []byte("png-dark")},
		"docs/images/aws-light.png":       {Data: []byte("png-light")},
		"docs/images/unrelated-image.png": {Data: []byte("not linked")},
		"agent/aws/compose.yaml":          {Data: []byte("services: {}")},
	}
}

const quickstart = `# AWS Quickstart

Pick one way to run it.

| Variable | Default |
|---|---|
| ` + "`VERSUS_IMAGE`" + ` | ` + "`ghcr.io/versuscontrol/versus-incident:latest`" + ` |

` + "```sh" + `
docker run --rm ghcr.io/versuscontrol/versus-incident:v1.4.36
docker run --rm ghcr.io/versuscontrol/versus-enterprise:v1.4.36
` + "```" + `

![overview](../../docs/images/aws-dark.png)
<img src="docs/images/aws-light.png" alt="light">
Download [the compose file](./compose.yaml).
`

func TestSelectPages(t *testing.T) {
	pages, skipped, err := SelectPages(mustConfig(t), devTree())
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v", skipped)
	}
	var got []string
	for _, p := range pages {
		got = append(got, p.Path)
	}
	want := []string{"agent/aws/quickstart.md", "in-development/aws-finops.md", "in-development/aws-harness.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pages = %v, want %v", got, want)
	}
}

func TestSelectPagesMissingListedPage(t *testing.T) {
	dev := devTree()
	delete(dev, "agent/aws/quickstart.md")
	_, _, err := SelectPages(mustConfig(t), dev)
	if err == nil || !strings.Contains(err.Error(), "agent/aws/quickstart.md") {
		t.Fatalf("want an error naming the missing page, got %v", err)
	}
	empty := fstest.MapFS{"agent/aws/quickstart.md": {Data: []byte("# Q\n")}}
	if _, _, err := SelectPages(mustConfig(t), empty); err == nil || !strings.Contains(err.Error(), "in-development/**") {
		t.Fatalf("want an error for a glob that matches nothing, got %v", err)
	}
}

func TestInsertBadge(t *testing.T) {
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"title then blank line", "# Title\n\nBody\n", "# Title\n\n" + BadgeLine + "\n\nBody\n", true},
		{"title then text", "# Title\nBody\n", "# Title\n\n" + BadgeLine + "\n\nBody\n", true},
		{"title at end of file", "# Title", "# Title\n\n" + BadgeLine, true},
		{"front matter is passed over", "---\ntitle: x\n# not a heading\n---\n# Title\n\nBody\n",
			"---\ntitle: x\n# not a heading\n---\n# Title\n\n" + BadgeLine + "\n\nBody\n", true},
		{"heading inside a code block is not the title", "```sh\n# comment\n```\n# Title\n",
			"```sh\n# comment\n```\n# Title\n\n" + BadgeLine + "\n", true},
		{"no title", "## Only a section\n\nBody\n", "## Only a section\n\nBody\n", false},
		{"front matter and no title", "---\na: b\n---\nBody\n", "---\na: b\n---\nBody\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := InsertBadge(c.in)
			if got != c.want || ok != c.ok {
				t.Errorf("InsertBadge(%q) = %q, %t; want %q, %t", c.in, got, ok, c.want, c.ok)
			}
			again, _ := InsertBadge(got)
			if again != got {
				t.Errorf("second run changed the page:\n%q\n%q", got, again)
			}
		})
	}
}

func TestRewriteImage(t *testing.T) {
	cases := []struct {
		name, in, want string
		n              int
	}{
		{"prose with full stop", "Run ghcr.io/versuscontrol/versus-incident:latest.", "Run ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4.", 1},
		{"inline code", "`ghcr.io/versuscontrol/versus-incident:v1.4.36`", "`ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4`", 1},
		{"table cell", "| `VERSUS_IMAGE` | `ghcr.io/versuscontrol/versus-incident:latest` |", "| `VERSUS_IMAGE` | `ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4` |", 1},
		{"code block", "```sh\nVERSUS_IMAGE=ghcr.io/versuscontrol/versus-incident:main-9f8e7d6 ./run\n```", "```sh\nVERSUS_IMAGE=ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4 ./run\n```", 1},
		{"yaml value", "image: \"ghcr.io/versuscontrol/versus-incident:latest\"", "image: \"ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4\"", 1},
		{"enterprise image untouched", "ghcr.io/versuscontrol/versus-enterprise:latest", "ghcr.io/versuscontrol/versus-enterprise:latest", 0},
		{"longer name untouched", "ghcr.io/versuscontrol/versus-incident-agent:latest", "ghcr.io/versuscontrol/versus-incident-agent:latest", 0},
		{"other registry untouched", "docker.io/versuscontrol/versus-incident:latest", "docker.io/versuscontrol/versus-incident:latest", 0},
		{"host that ends in the registry name untouched", "x.ghcr.io/versuscontrol/versus-incident:v1", "x.ghcr.io/versuscontrol/versus-incident:v1", 0},
		{"path that ends in the image untouched", "mirror.example/ghcr.io/versuscontrol/versus-incident:v1", "mirror.example/ghcr.io/versuscontrol/versus-incident:v1", 0},
		{"digest is dropped", "ghcr.io/versuscontrol/versus-incident:v1@sha256:" + strings.Repeat("ab", 32) + " next", "ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4 next", 1},
		{"digest on the target tag is dropped", "ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4@sha256:" + strings.Repeat("0f", 32), "ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4", 1},
		{"untagged untouched", "repository: ghcr.io/versuscontrol/versus-incident\ntag: v1", "repository: ghcr.io/versuscontrol/versus-incident\ntag: v1", 0},
		{"two on one line", "a ghcr.io/versuscontrol/versus-incident:a b ghcr.io/versuscontrol/versus-incident:b", "a ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4 b ghcr.io/versuscontrol/versus-incident:dev-1a2b3c4", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n := RewriteImage(c.in, image, "dev-1a2b3c4")
			if got != c.want || n != c.n {
				t.Errorf("RewriteImage(%q) = %q, %d; want %q, %d", c.in, got, n, c.want, c.n)
			}
			again, n2 := RewriteImage(got, image, "dev-1a2b3c4")
			if again != got || n2 != 0 {
				t.Errorf("second run changed the page: %q (%d)", again, n2)
			}
		})
	}
}

func TestLinkedAssets(t *testing.T) {
	got := LinkedAssets("agent/aws/quickstart.md", []byte(quickstart+"\n[page](./other.md) [route](/agent/x) [web](https://example.com/a.png) [up](../../../outside.png)\n```\n![in code](../../docs/images/code.png)\n```\n"))
	want := []string{"agent/aws/compose.yaml", "docs/images/aws-dark.png", "docs/images/aws-light.png"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LinkedAssets = %v, want %v", got, want)
	}
}

const mainSidebar = `- [Introduction](/introduction)

- Kubernetes Connectors
  - [Kubernetes](/agent/connectors/kubernetes)

- In Development
  - [AWS Harness](/in-development/aws-harness)
`

func TestAddSidebarEntries(t *testing.T) {
	entries := []SidebarEntry{
		{Group: "In Development", Title: "AWS Harness", Route: "/in-development/aws-harness"},
		{Group: "In Development", Title: "AWS FinOps Agent", Route: "/in-development/aws-finops"},
		{Group: "AWS", Title: "Quickstart", Route: "/agent/aws/quickstart"},
	}
	got, added := AddSidebarEntries(mainSidebar, entries)
	want := `- [Introduction](/introduction)

- Kubernetes Connectors
  - [Kubernetes](/agent/connectors/kubernetes)

- In Development
  - [AWS Harness](/in-development/aws-harness)
  - [AWS FinOps Agent](/in-development/aws-finops)

- AWS
  - [Quickstart](/agent/aws/quickstart)
`
	if got != want {
		t.Errorf("sidebar:\n%s\nwant:\n%s", got, want)
	}
	if !reflect.DeepEqual(added, []string{"/in-development/aws-finops", "/agent/aws/quickstart"}) {
		t.Errorf("added = %v", added)
	}
	again, added2 := AddSidebarEntries(got, entries)
	if again != got || len(added2) != 0 {
		t.Errorf("second run changed the sidebar (added %v)", added2)
	}
}

func TestSidebarHasMatchesRouteForms(t *testing.T) {
	sidebar := "- [A](/agent/aws/quickstart.md)\n- [B](in-development/x/)\n- [C](/in-development/y?id=top)\n"
	for _, r := range []string{"/agent/aws/quickstart", "/in-development/x", "/in-development/y"} {
		if !SidebarHas(sidebar, r) {
			t.Errorf("SidebarHas(%q) = false", r)
		}
	}
	if SidebarHas(sidebar, "/agent/aws") {
		t.Error("a prefix route matched")
	}
	if PageRoute("in-development/README.md") != "/in-development" || PageRoute("agent/aws/quickstart.md") != "/agent/aws/quickstart" {
		t.Error("PageRoute")
	}
}

func TestOverlay(t *testing.T) {
	site := Files{
		"_sidebar.md":                   []byte(mainSidebar),
		"introduction.md":               []byte("# Intro\n"),
		"in-development/aws-harness.md": []byte("# AWS Harness\n\nold main copy\n"),
	}
	r, err := Overlay(mustConfig(t), site, devTree(), "dev-1a2b3c4")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.AddedPages, []string{"agent/aws/quickstart.md", "in-development/aws-finops.md"}) ||
		!reflect.DeepEqual(r.ReplacedPages, []string{"in-development/aws-harness.md"}) {
		t.Errorf("added %v replaced %v", r.AddedPages, r.ReplacedPages)
	}
	if _, ok := site["agent/other.md"]; ok {
		t.Error("an unlisted dev page was published")
	}
	if _, ok := site["docs/images/unrelated-image.png"]; ok {
		t.Error("an unlinked dev image was copied")
	}
	for _, a := range []string{"docs/images/aws-map.png", "docs/images/aws-dark.png", "docs/images/aws-light.png"} {
		if _, ok := site[a]; !ok {
			t.Errorf("linked file %s was not copied", a)
		}
	}
	if _, ok := site["agent/aws/compose.yaml"]; ok || !reflect.DeepEqual(r.NotCopied, []string{"agent/aws/compose.yaml"}) {
		t.Errorf("a linked file outside docs/images was copied (not copied: %v)", r.NotCopied)
	}
	q := string(site["agent/aws/quickstart.md"])
	if !strings.HasPrefix(q, "# AWS Quickstart\n\n"+BadgeLine+"\n") {
		t.Errorf("quickstart has no badge under its title:\n%s", q)
	}
	if strings.Contains(q, "versus-incident:latest") || strings.Contains(q, "versus-incident:v1.4.36") || !strings.Contains(q, "versus-enterprise:v1.4.36") {
		t.Errorf("image rewrite wrong:\n%s", q)
	}
	if r.Rewrites["agent/aws/quickstart.md"] != 2 {
		t.Errorf("rewrites = %v", r.Rewrites)
	}
	if string(site["introduction.md"]) != "# Intro\n" {
		t.Error("a main page changed")
	}
	sb := string(site["_sidebar.md"])
	if !strings.Contains(sb, "  - [AWS FinOps Agent](/in-development/aws-finops)") || !strings.Contains(sb, "- AWS\n  - [Quickstart](/agent/aws/quickstart)") {
		t.Errorf("sidebar:\n%s", sb)
	}
	if strings.Count(sb, "/in-development/aws-harness") != 1 {
		t.Error("an existing sidebar entry was added again")
	}

	// A second overlay onto the built site changes nothing more.
	before := map[string]string{}
	for k, v := range site {
		before[k] = string(v)
	}
	if _, err := Overlay(mustConfig(t), site, devTree(), "dev-1a2b3c4"); err != nil {
		t.Fatal(err)
	}
	for k, v := range site {
		if before[k] != string(v) {
			t.Errorf("second overlay changed %s", k)
		}
	}
}

func TestOverlayReportsPageWithoutTitle(t *testing.T) {
	dev := devTree()
	dev["in-development/notes.md"] = &fstest.MapFile{Data: []byte("Some notes, no title.\n")}
	site := Files{"_sidebar.md": []byte(mainSidebar)}
	r, err := Overlay(mustConfig(t), site, dev, "dev-1a2b3c4")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.NoHeading, []string{"in-development/notes.md"}) {
		t.Errorf("NoHeading = %v", r.NoHeading)
	}
	if string(site["in-development/notes.md"]) != "Some notes, no title.\n" {
		t.Error("a page without a title was changed")
	}
	if !strings.Contains(string(site["_sidebar.md"]), "[notes](/in-development/notes)") {
		t.Error("a page without a title needs its file name as the sidebar text")
	}
}

func TestPickDevTag(t *testing.T) {
	commits := []string{
		"aaaaaaa1111111111111111111111111111111111", // newest, a docs-only push with no image
		"bbbbbbb2222222222222222222222222222222222",
		"ccccccc3333333333333333333333333333333333",
	}
	tags := []string{"dev-bbbbbbb", "latest", "dev", "dev-9999999", "dev-ccccccc", "main-aaaaaaa", "v1.4.36"}
	if tag, ok := PickDevTag(tags, commits); !ok || tag != "dev-bbbbbbb" {
		t.Errorf("PickDevTag = %q, %t; want dev-bbbbbbb", tag, ok)
	}
	if _, ok := PickDevTag([]string{"dev", "dev-9999999", "latest"}, commits); ok {
		t.Error("a tag whose commit is not on dev was picked")
	}
}

func TestParseVersions(t *testing.T) {
	in := `[{"id":1,"metadata":{"container":{"tags":["dev","dev-bbbbbbb"]}}},{"id":2,"metadata":{"container":{"tags":[]}}}]
[{"id":3,"metadata":{"container":{"tags":["v1.4.36","latest"]}}}]
{"id":4,"metadata":{"container":{"tags":["dev-ccccccc"]}}}`
	got, err := ParseVersions(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dev", "dev-bbbbbbb", "v1.4.36", "latest", "dev-ccccccc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
	if _, err := ParseVersions(strings.NewReader(`{"message":`)); err == nil {
		t.Error("broken JSON was accepted")
	}
}

func TestAffected(t *testing.T) {
	cfg := mustConfig(t)
	for files, want := range map[string]bool{
		"src/in-development/aws-harness.md":  true,
		"src/agent/aws/quickstart.md":        true,
		"src/docs/images/new.png":            true,
		".github/dev-docs.json":              true,
		"tools/devdocs/main.go":              false,
		".github/workflows/docs.yml":         false,
		"src/agent/connectors/kubernetes.md": false,
		"pkg/aws/cost/backend.go":            false,
	} {
		if got := Affected(cfg, []string{files}); got != want {
			t.Errorf("Affected(%s) = %t, want %t", files, got, want)
		}
	}
}

func TestBuildCommand(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, data string) {
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cfg.json", testConfig)
	write("main/_sidebar.md", mainSidebar)
	write("main/.nojekyll", "")
	for name, f := range devTree() {
		write("dev/"+name, string(f.Data))
	}
	var out, errOut strings.Builder
	args := []string{"build", "--config", filepath.Join(tmp, "cfg.json"), "--main", filepath.Join(tmp, "main"),
		"--dev", filepath.Join(tmp, "dev"), "--tag", "dev-1a2b3c4", "--out", filepath.Join(tmp, "site")}
	if err := run(args, &out, &errOut); err != nil {
		t.Fatalf("build: %v\n%s", err, errOut.String())
	}
	q, err := os.ReadFile(filepath.Join(tmp, "site", "agent", "aws", "quickstart.md"))
	if err != nil || !strings.Contains(string(q), BadgeLine) {
		t.Fatalf("built quickstart: %v %q", err, q)
	}
	if _, err := os.Stat(filepath.Join(tmp, "site", ".nojekyll")); err != nil {
		t.Error("main's dot files were not copied")
	}
	if err := run(args, &out, &errOut); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("a second build into the same folder must refuse, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "dev", "agent", "aws", "quickstart.md")); err != nil {
		t.Fatal(err)
	}
	if d, _ := os.ReadFile(filepath.Join(tmp, "dev", "agent", "aws", "quickstart.md")); strings.Contains(string(d), BadgeLine) {
		t.Error("the dev source was changed")
	}
	bad := append(append([]string{}, args[:len(args)-2]...), "--out", filepath.Join(tmp, "site2"))
	bad[8] = "bad tag"
	if err := run(bad, &out, &errOut); err == nil {
		t.Error("an invalid tag was accepted")
	}
}

// Files a dev page links to never replace main's: an image main already has
// stays main's, files outside docs/images and the site shell come from main
// only, and symbolic links are never followed.
func TestOverlayKeepsMainFiles(t *testing.T) {
	page := "# AWS Harness\n\n" +
		"![map](../docs/images/aws-map.png) ![new](../docs/images/aws-new.png)\n" +
		"[shell](/index.html) [sidebar](/_sidebar.md) <a href=\"CNAME\">x</a> <img src=\".nojekyll\">\n" +
		"![linked](../docs/images/evil.png) ![in linked folder](../docs/images/linked/x.png)\n"
	dev := fstest.MapFS{
		"in-development/aws-harness.md": {Data: []byte(page)},
		"in-development/link.md":        {Data: []byte("aws-harness.md"), Mode: fs.ModeSymlink},
		"agent/aws/quickstart.md":       {Data: []byte("# Q\n")},
		"docs/images/aws-map.png":       {Data: []byte("dev map")},
		"docs/images/aws-new.png":       {Data: []byte("dev new")},
		"docs/images/evil.png":          {Data: []byte("../../secret.txt"), Mode: fs.ModeSymlink},
		"docs/images/linked":            {Data: []byte("../../other"), Mode: fs.ModeSymlink},
		"other/x.png":                   {Data: []byte("outside")},
		"secret.txt":                    {Data: []byte("secret")},
		"index.html":                    {Data: []byte("dev shell")},
		"_sidebar.md":                   {Data: []byte("- dev sidebar")},
		"CNAME":                         {Data: []byte("dev.example")},
		".nojekyll":                     {Data: []byte("dev")},
	}
	site := Files{
		"_sidebar.md":             []byte(mainSidebar),
		"index.html":              []byte("main shell"),
		"CNAME":                   []byte("docs.example"),
		".nojekyll":               []byte(""),
		"docs/images/aws-map.png": []byte("main map"),
	}
	r, err := Overlay(mustConfig(t), site, dev, "dev-1a2b3c4")
	if err != nil {
		t.Fatal(err)
	}
	if string(site["docs/images/aws-map.png"]) != "main map" {
		t.Error("main's image was replaced by dev's")
	}
	if string(site["docs/images/aws-new.png"]) != "dev new" {
		t.Error("a new dev image was not copied")
	}
	for name, want := range map[string]string{"index.html": "main shell", "CNAME": "docs.example", ".nojekyll": ""} {
		if string(site[name]) != want {
			t.Errorf("%s = %q, want main's %q", name, site[name], want)
		}
	}
	if strings.Contains(string(site["_sidebar.md"]), "dev sidebar") {
		t.Error("dev's sidebar replaced main's")
	}
	for _, name := range []string{"docs/images/evil.png", "docs/images/linked/x.png", "in-development/link.md"} {
		if _, ok := site[name]; ok {
			t.Errorf("symbolic link %s was followed", name)
		}
	}
	if !reflect.DeepEqual(r.KeptMain, []string{"docs/images/aws-map.png"}) {
		t.Errorf("KeptMain = %v", r.KeptMain)
	}
	if !reflect.DeepEqual(r.NotCopied, []string{".nojekyll", "index.html"}) {
		t.Errorf("NotCopied = %v", r.NotCopied)
	}
	wantSkipped := []string{"in-development/link.md (symbolic link)", "docs/images/evil.png (symbolic link)", "docs/images/linked/x.png (symbolic link)"}
	if !reflect.DeepEqual(r.Skipped, wantSkipped) {
		t.Errorf("Skipped = %v, want %v", r.Skipped, wantSkipped)
	}
}

func TestSelectPagesNeverTakesShellFiles(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"image":"ghcr.io/o/n","pages":[{"path":"*.md"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	dev := fstest.MapFS{"_sidebar.md": {Data: []byte("- x")}, "guide.md": {Data: []byte("# G")}}
	pages, skipped, err := SelectPages(cfg, dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].Path != "guide.md" {
		t.Errorf("pages = %v", pages)
	}
	if !reflect.DeepEqual(skipped, []string{"_sidebar.md (site shell file, always main's)"}) {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestParseConfigLenient(t *testing.T) {
	data := []byte(`{"image":"ghcr.io/o/n","owner":"x","pages":[{"path":"a.md","order":2,"sidebar":{"group":"G","icon":"i"}}]}`)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("strict parsing accepted unknown keys")
	}
	cfg, unknown, err := ParseConfigLenient(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pages[0].Path != "a.md" || cfg.Pages[0].Sidebar.Group != "G" {
		t.Errorf("known keys lost: %+v", cfg.Pages[0])
	}
	want := []string{"owner", "pages[0].order", "pages[0].sidebar.icon"}
	if !reflect.DeepEqual(unknown, want) {
		t.Errorf("unknown = %v, want %v", unknown, want)
	}
	if _, _, err := ParseConfigLenient([]byte(`{"image":"ghcr.io/o/n","pages":[]}`)); err == nil {
		t.Error("lenient parsing skipped validation")
	}
}
