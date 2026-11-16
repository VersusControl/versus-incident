// Command docscheck reports internal links and images in the docs site that
// point at files that do not exist.
//
//	go run ./tools/docscheck [--root src]
//
// The site is rendered by docsify with relativePath on, so a Markdown link or
// image resolves against the page's own folder, a link that starts with "/"
// resolves against the site root, and a page link may leave out ".md" or
// name a folder that has a README.md. Raw HTML src and href attributes are
// resolved by the browser against the site root. External links, anchors and
// fenced code blocks are not checked.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
)

func main() {
	root := flag.String("root", "src", "docs site root")
	flag.Parse()
	broken, err := Check(os.DirFS(*root))
	if err != nil {
		fmt.Fprintln(os.Stderr, "docscheck:", err)
		os.Exit(2)
	}
	for _, b := range broken {
		// GitHub turns this into an annotation on the file and line.
		fmt.Printf("::error file=%s,line=%d::Broken link %s\n", path.Join(*root, b.File), b.Line, b.Target)
	}
	if len(broken) > 0 {
		fmt.Fprintf(os.Stderr, "docscheck: %d broken internal link(s)\n", len(broken))
		os.Exit(1)
	}
	fmt.Println("docscheck: every internal link and image resolves")
}

// Broken is one link whose target is missing.
type Broken struct {
	File   string
	Line   int
	Target string
}

var (
	mdLink   = regexp.MustCompile(`!?\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+[^)]*)?\)`)
	htmlAttr = regexp.MustCompile(`(?i)\b(?:src|href)\s*=\s*"([^"]+)"`)
	scheme   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// Check walks every Markdown file under the site root.
func Check(site fs.FS) ([]Broken, error) {
	var broken []Broken
	err := fs.WalkDir(site, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(name, ".md") {
			return nil
		}
		f, err := site.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		found, err := checkFile(site, name, f)
		broken = append(broken, found...)
		return err
	})
	sort.Slice(broken, func(i, j int) bool {
		if broken[i].File != broken[j].File {
			return broken[i].File < broken[j].File
		}
		return broken[i].Line < broken[j].Line
	})
	return broken, err
}

func checkFile(site fs.FS, name string, r io.Reader) ([]Broken, error) {
	var broken []Broken
	dir := path.Dir(name)
	inFence := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if t := strings.TrimSpace(text); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		text = stripInlineCode(text)
		for _, m := range mdLink.FindAllStringSubmatch(text, -1) {
			if !resolves(site, dir, m[1]) {
				broken = append(broken, Broken{File: name, Line: line, Target: m[1]})
			}
		}
		for _, m := range htmlAttr.FindAllStringSubmatch(text, -1) {
			if !resolves(site, ".", m[1]) {
				broken = append(broken, Broken{File: name, Line: line, Target: m[1]})
			}
		}
	}
	return broken, sc.Err()
}

var inlineCode = regexp.MustCompile("`[^`]*`")

func stripInlineCode(s string) string { return inlineCode.ReplaceAllString(s, "") }

// resolves reports whether target, found in a page under dir, names a file in
// the site. Targets the check does not own (external, anchors) count as fine.
func resolves(site fs.FS, dir, target string) bool {
	if scheme.MatchString(target) || strings.HasPrefix(target, "//") || strings.HasPrefix(target, "#") {
		return true
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return true
	}
	var p string
	if strings.HasPrefix(target, "/") {
		p = path.Clean(strings.TrimPrefix(target, "/"))
	} else {
		p = path.Clean(path.Join(dir, target))
	}
	if p == "." {
		return true
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		// Leaves the site; it cannot be served.
		return false
	}
	for _, candidate := range []string{p, p + ".md", path.Join(p, "README.md")} {
		if info, err := fs.Stat(site, candidate); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}
