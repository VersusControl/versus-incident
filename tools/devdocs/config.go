package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Config is .github/dev-docs.json: the dev image and the pages taken from the
// dev branch.
type Config struct {
	Image string `json:"image"`
	Pages []Page `json:"pages"`
}

// Page is one entry of the list. Path is relative to src/ and may use globs;
// "**" matches any number of folders. Sidebar, when set, adds a sidebar entry
// for each matching page that main's sidebar does not already have.
type Page struct {
	Path    string   `json:"path"`
	Sidebar *Sidebar `json:"sidebar,omitempty"`
}

// Sidebar names the sidebar group, and optionally the link text. Without a
// title the page's first heading is used.
type Sidebar struct {
	Group string `json:"group"`
	Title string `json:"title,omitempty"`
}

// ParseConfig decodes and checks the page list. Unknown keys are an error.
// It backs the validate command, which is for local checks; the site build
// reads the config with ParseConfigLenient.
func ParseConfig(data []byte) (*Config, error) {
	return parseConfig(data, true)
}

// ParseConfigLenient is ParseConfig for the site build: the list lives on
// dev, so a key this build does not know yet is ignored and named in the
// returned warnings instead of stopping the deploy.
func ParseConfigLenient(data []byte) (*Config, []string, error) {
	cfg, err := parseConfig(data, false)
	if err != nil {
		return nil, nil, err
	}
	return cfg, unknownKeys(data), nil
}

func parseConfig(data []byte, strict bool) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if strict {
		dec.DisallowUnknownFields()
	}
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse dev docs config: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse dev docs config: trailing data after the JSON object")
	}
	if cfg.Image == "" || strings.Count(cfg.Image, "/") < 2 {
		return nil, fmt.Errorf("image %q must be a registry path such as ghcr.io/owner/name", cfg.Image)
	}
	if last := cfg.Image[strings.LastIndex(cfg.Image, "/")+1:]; strings.ContainsAny(last, ":@") {
		return nil, fmt.Errorf("image %q must not carry a tag or digest; the build adds the tag", cfg.Image)
	}
	if len(cfg.Pages) == 0 {
		return nil, fmt.Errorf("dev docs config lists no pages")
	}
	seen := map[string]bool{}
	for _, p := range cfg.Pages {
		if err := validatePattern(p.Path); err != nil {
			return nil, err
		}
		if seen[p.Path] {
			return nil, fmt.Errorf("page %q is listed twice", p.Path)
		}
		seen[p.Path] = true
		if p.Sidebar != nil && strings.TrimSpace(p.Sidebar.Group) == "" {
			return nil, fmt.Errorf("page %q: sidebar needs a group", p.Path)
		}
	}
	return &cfg, nil
}

func validatePattern(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return fmt.Errorf("page path %q must be a non-empty path relative to src/", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("page path %q must not contain empty, . or .. segments", p)
		}
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return fmt.Errorf("page path %q: ** must be a whole path segment", p)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("page path %q: %w", p, err)
		}
	}
	return nil
}

// matchGlob reports whether the slash-separated path matches pattern. "*",
// "?" and "[...]" work within one segment; a "**" segment matches zero or
// more whole segments.
func matchGlob(pattern, file string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(file, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// DevPage is one Markdown page selected from the dev tree.
type DevPage struct {
	Path    string // relative to src/
	Sidebar *Sidebar
}

// protected files belong to main's site shell and are never taken from dev.
var protected = map[string]bool{"index.html": true, "_sidebar.md": true, "CNAME": true, ".nojekyll": true}

// SelectPages lists the Markdown pages in the dev tree that the config names.
// Every entry must match at least one page, so a renamed or deleted page
// fails the build instead of quietly dropping off the site. Symbolic links
// and the site shell files are never selected; they are returned in skipped.
func SelectPages(cfg *Config, dev fs.FS) (pages []DevPage, skipped []string, err error) {
	var files, links []string
	err = fs.WalkDir(dev, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(name, ".md") {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			links = append(links, name)
			return nil
		}
		files = append(files, name)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("read dev tree: %w", err)
	}
	for _, l := range links {
		for _, entry := range cfg.Pages {
			if matchGlob(entry.Path, l) {
				skipped = append(skipped, l+" (symbolic link)")
				break
			}
		}
	}
	sort.Strings(files)

	byPath := map[string]DevPage{}
	var missing []string
	for _, entry := range cfg.Pages {
		n := 0
		for _, f := range files {
			if matchGlob(entry.Path, f) {
				if protected[f] {
					skipped = append(skipped, f+" (site shell file, always main's)")
					continue
				}
				n++
				if _, ok := byPath[f]; !ok {
					byPath[f] = DevPage{Path: f, Sidebar: entry.Sidebar}
				}
			}
		}
		if n == 0 {
			missing = append(missing, entry.Path)
		}
	}
	if len(missing) > 0 {
		return nil, skipped, fmt.Errorf("listed page(s) not found on dev: %s", strings.Join(missing, ", "))
	}
	pages = make([]DevPage, 0, len(byPath))
	for _, p := range byPath {
		pages = append(pages, p)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Path < pages[j].Path })
	return pages, skipped, nil
}

// unknownKeys names every key the config types do not have, as JSON paths.
func unknownKeys(data []byte) []string {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return nil
	}
	var out []string
	known := func(m map[string]json.RawMessage, where string, keys ...string) {
		allowed := map[string]bool{}
		for _, k := range keys {
			allowed[k] = true
		}
		var extra []string
		for k := range m {
			if !allowed[k] {
				extra = append(extra, where+k)
			}
		}
		sort.Strings(extra)
		out = append(out, extra...)
	}
	known(top, "", "image", "pages")
	var pages []map[string]json.RawMessage
	if json.Unmarshal(top["pages"], &pages) != nil {
		return out
	}
	for i, p := range pages {
		known(p, fmt.Sprintf("pages[%d].", i), "path", "sidebar")
		var sb map[string]json.RawMessage
		if raw, ok := p["sidebar"]; ok && json.Unmarshal(raw, &sb) == nil {
			known(sb, fmt.Sprintf("pages[%d].sidebar.", i), "group", "title")
		}
	}
	return out
}
