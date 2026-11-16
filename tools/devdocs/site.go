package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Files is an in-memory site: paths relative to src/ mapped to contents.
type Files map[string][]byte

var (
	mdTarget   = regexp.MustCompile(`!?\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+[^)]*)?\)`)
	htmlTarget = regexp.MustCompile(`(?i)\b(?:src|href|srcset)\s*=\s*"([^"]+)"`)
	hasScheme  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	inlineSpan = regexp.MustCompile("`[^`]*`")
)

// LinkedAssets lists the non-Markdown files a page links to, as paths
// relative to src/. Markdown links resolve against the page's folder (the
// site sets docsify's relativePath) or the site root for "/..."; raw HTML
// attributes resolve against the site root, as the browser does. Pages,
// anchors, external links, code and paths that leave the site are skipped.
func LinkedAssets(pagePath string, page []byte) []string {
	seen := map[string]bool{}
	add := func(base, target string) {
		if hasScheme.MatchString(target) || strings.HasPrefix(target, "//") || strings.HasPrefix(target, "#") {
			return
		}
		if i := strings.IndexAny(target, "?#"); i >= 0 {
			target = target[:i]
		}
		if target == "" {
			return
		}
		var p string
		if strings.HasPrefix(target, "/") {
			p = path.Clean(strings.TrimPrefix(target, "/"))
		} else {
			p = path.Clean(path.Join(base, target))
		}
		if p == "." || p == ".." || strings.HasPrefix(p, "../") {
			return
		}
		ext := path.Ext(p)
		if ext == "" || ext == ".md" {
			return
		}
		seen[p] = true
	}
	dir := path.Dir(pagePath)
	inFence := false
	sc := bufio.NewScanner(strings.NewReader(string(page)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		line = inlineSpan.ReplaceAllString(line, "")
		for _, m := range mdTarget.FindAllStringSubmatch(line, -1) {
			add(dir, m[1])
		}
		for _, m := range htmlTarget.FindAllStringSubmatch(line, -1) {
			for _, part := range strings.Split(m[1], ",") {
				if f := strings.Fields(part); len(f) > 0 {
					add(".", f[0])
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Report collects what a build did, for the job log.
type Report struct {
	Pages         []string
	AddedPages    []string
	ReplacedPages []string
	Assets        []string
	Badged        []string
	NoHeading     []string
	Rewrites      map[string]int
	SidebarAdded  []string
	MissingAssets []string
	// KeptMain are linked files main already has; main's copy is kept.
	KeptMain []string
	// NotCopied are linked files outside docs/images, which come from main only.
	NotCopied []string
	// Skipped are symbolic links and site shell files, never taken from dev.
	Skipped []string
}

// assetDir is the only folder dev may add files to.
const assetDir = "docs/images/"

// isSymlink reports whether name, or any folder on its path, is a symbolic
// link in fsys.
func isSymlink(fsys fs.FS, name string) bool {
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err := fs.Lstat(fsys, strings.Join(parts[:i+1], "/"))
		if err != nil {
			return false
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// Overlay applies the dev pages to a copy of main's site in place: it copies
// each listed page from dev, adds the badge, rewrites the image to tag, and
// adds the missing sidebar entries. A file a dev page links to is copied only
// when it is under docs/images and main does not have it, so dev never
// replaces anything main publishes except the listed pages themselves.
func Overlay(cfg *Config, site Files, dev fs.FS, tag string) (*Report, error) {
	pages, skipped, err := SelectPages(cfg, dev)
	if err != nil {
		return nil, err
	}
	r := &Report{Rewrites: map[string]int{}, Skipped: skipped}
	assets := map[string]bool{}
	var entries []SidebarEntry
	for _, p := range pages {
		data, err := fs.ReadFile(dev, p.Path)
		if err != nil {
			return nil, fmt.Errorf("read dev page %s: %w", p.Path, err)
		}
		r.Pages = append(r.Pages, p.Path)
		if _, ok := site[p.Path]; ok {
			r.ReplacedPages = append(r.ReplacedPages, p.Path)
		} else {
			r.AddedPages = append(r.AddedPages, p.Path)
		}
		for _, a := range LinkedAssets(p.Path, data) {
			assets[a] = true
		}

		text, ok := InsertBadge(string(data))
		if ok {
			r.Badged = append(r.Badged, p.Path)
		} else {
			r.NoHeading = append(r.NoHeading, p.Path)
		}
		text, n := RewriteImage(text, cfg.Image, tag)
		if n > 0 {
			r.Rewrites[p.Path] = n
		}
		site[p.Path] = []byte(text)

		if p.Sidebar != nil {
			title := p.Sidebar.Title
			if title == "" {
				title = FirstHeading(string(data))
			}
			if title == "" {
				title = strings.TrimSuffix(path.Base(p.Path), ".md")
			}
			entries = append(entries, SidebarEntry{Group: p.Sidebar.Group, Title: title, Route: PageRoute(p.Path)})
		}
	}

	names := make([]string, 0, len(assets))
	for a := range assets {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		_, inMain := site[a]
		switch {
		case protected[a] || !strings.HasPrefix(a, assetDir):
			r.NotCopied = append(r.NotCopied, a)
			continue
		case inMain:
			r.KeptMain = append(r.KeptMain, a)
			continue
		case isSymlink(dev, a):
			r.Skipped = append(r.Skipped, a+" (symbolic link)")
			continue
		}
		data, err := fs.ReadFile(dev, a)
		if err != nil {
			// docscheck reports the broken link with its page and line.
			r.MissingAssets = append(r.MissingAssets, a)
			continue
		}
		site[a] = data
		r.Assets = append(r.Assets, a)
	}

	if len(entries) > 0 {
		sidebar, added := AddSidebarEntries(string(site["_sidebar.md"]), entries)
		if len(added) > 0 {
			site["_sidebar.md"] = []byte(sidebar)
		}
		r.SidebarAdded = added
	}
	return r, nil
}
