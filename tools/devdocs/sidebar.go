package main

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// SidebarEntry is one link to add.
type SidebarEntry struct {
	Group string
	Title string
	Route string // docsify route, such as /agent/aws/quickstart
}

// PageRoute is the docsify route of a page path relative to src/.
func PageRoute(p string) string {
	p = strings.TrimSuffix(p, ".md")
	if path.Base(p) == "README" {
		p = path.Dir(p)
		if p == "." {
			return "/"
		}
	}
	return "/" + p
}

var sidebarLink = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)`)

func normalRoute(target string) string {
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	target = strings.TrimSuffix(strings.TrimPrefix(target, "/"), ".md")
	target = strings.TrimSuffix(target, "/")
	if path.Base(target) == "README" {
		target = strings.TrimSuffix(strings.TrimSuffix(target, "README"), "/")
	}
	return "/" + target
}

// SidebarHas reports whether the sidebar already links to route.
func SidebarHas(sidebar, route string) bool {
	for _, m := range sidebarLink.FindAllStringSubmatch(sidebar, -1) {
		if normalRoute(m[1]) == normalRoute(route) {
			return true
		}
	}
	return false
}

// AddSidebarEntries adds each entry whose route the sidebar lacks. An entry
// joins the end of its top-level group ("- Group" with no link); a group that
// does not exist yet is added at the end. It returns the routes it added.
func AddSidebarEntries(sidebar string, entries []SidebarEntry) (string, []string) {
	var added []string
	for _, e := range entries {
		if SidebarHas(sidebar, e.Route) {
			continue
		}
		sidebar = addEntry(sidebar, e)
		added = append(added, e.Route)
	}
	return sidebar, added
}

func addEntry(sidebar string, e SidebarEntry) string {
	item := fmt.Sprintf("  - [%s](%s)", e.Title, e.Route)
	lines := strings.Split(strings.TrimRight(sidebar, "\n"), "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " \r") != "- "+e.Group {
			continue
		}
		last := i
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t") {
				last = j
				continue
			}
			break
		}
		out := append([]string{}, lines[:last+1]...)
		out = append(out, item)
		out = append(out, lines[last+1:]...)
		return strings.Join(out, "\n") + "\n"
	}
	return strings.Join(lines, "\n") + "\n\n- " + e.Group + "\n" + item + "\n"
}
