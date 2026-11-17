package main

import (
	"fmt"
	"path"
	"regexp"
	"sort"
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

var (
	sidebarLink     = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)`)
	sidebarLinkText = regexp.MustCompile(`\[([^\]\r\n]*)\]\(\s*<?([^)\s>]+)`)
)

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

// ListedOnDev keeps the entries whose route dev's sidebar links to, so the
// site's sidebar lists only what dev's sidebar lists. A kept entry is filed
// under the top-level group dev's sidebar lists it in (a link outside any
// group keeps the configured group) and takes dev's link text. Text or a
// group name that is not plain (see plainText) is not used; the entry keeps
// its configured group or title instead. It returns the routes of the
// entries left out; those pages are still published.
func ListedOnDev(entries []SidebarEntry, devSidebar string) (kept []SidebarEntry, unlisted []string) {
	type devLink struct{ group, title string }
	lines := sidebarLines(devSidebar)
	groupOf := make([]string, len(lines))
	for _, g := range topGroups(lines) {
		for i := g.head + 1; i <= g.last; i++ {
			groupOf[i] = g.name
		}
	}
	links := map[string]devLink{}
	for i, line := range lines {
		texts := map[string]string{}
		for _, m := range sidebarLinkText.FindAllStringSubmatch(line, -1) {
			if r := normalRoute(m[2]); texts[r] == "" {
				texts[r] = strings.TrimSpace(m[1])
			}
		}
		for _, m := range sidebarLink.FindAllStringSubmatch(line, -1) {
			r := normalRoute(m[1])
			if _, seen := links[r]; !seen {
				links[r] = devLink{group: groupOf[i], title: texts[r]}
			}
		}
	}
	for _, e := range entries {
		l, ok := links[normalRoute(e.Route)]
		if !ok {
			unlisted = append(unlisted, e.Route)
			continue
		}
		if plainText(l.group) {
			e.Group = l.group
		}
		if plainText(l.title) {
			e.Title = l.title
		}
		kept = append(kept, e)
	}
	return kept, unlisted
}

// plainText reports whether s is safe to write into the sidebar as link text
// or a group name: one non-empty line with no HTML, link or code syntax.
func plainText(s string) bool {
	return strings.TrimSpace(s) != "" && !strings.ContainsAny(s, "<>[]`\r\n")
}

// AddSidebarEntries adds each entry whose route the sidebar lacks. An entry
// joins the end of its top-level group ("- Group" with no link). A group the
// sidebar does not have yet is placed where dev's sidebar has it (see
// placeNewGroup), or at the end when dev gives no place. Entries of one group
// are added in the order dev's sidebar lists them. Only group names and
// routes are read from dev; no line of dev's sidebar is copied. It returns
// the routes it added.
func AddSidebarEntries(sidebar, devSidebar string, entries []SidebarEntry) (string, []string) {
	var added []string
	devGroups := topGroups(sidebarLines(devSidebar))
	for _, e := range orderLikeDev(entries, devSidebar) {
		if SidebarHas(sidebar, e.Route) {
			continue
		}
		sidebar = addEntry(sidebar, devGroups, e)
		added = append(added, e.Route)
	}
	return sidebar, added
}

// sidebarGroup is a top-level group of a sidebar: a "- Name" line with no
// link, followed by its indented lines.
type sidebarGroup struct {
	name string
	head int // index of the "- Name" line
	last int // index of the group's last indented line, or head
}

func sidebarLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func isIndented(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
}

// topGroups lists the top-level groups in the order they appear.
func topGroups(lines []string) []sidebarGroup {
	var out []sidebarGroup
	for i := 0; i < len(lines); i++ {
		name, ok := strings.CutPrefix(strings.TrimRight(lines[i], " \r"), "- ")
		if !ok || name == "" || strings.Contains(name, "](") {
			continue
		}
		g := sidebarGroup{name: name, head: i, last: i}
		for j := i + 1; j < len(lines) && isIndented(lines[j]); j++ {
			g.last = j
		}
		out = append(out, g)
		i = g.last
	}
	return out
}

func findGroup(groups []sidebarGroup, name string) (sidebarGroup, bool) {
	for _, g := range groups {
		if g.name == name {
			return g, true
		}
	}
	return sidebarGroup{}, false
}

// placeNewGroup finds where a group the sidebar lacks goes, from the group's
// neighbours in dev's sidebar. A group first on dev goes before the first
// group. Otherwise the nearest dev neighbour the sidebar has wins, the one
// before it first at each distance: the new group goes right after a group
// that precedes it on dev, or right before one that follows it. It returns
// the line index to insert at and whether the new group goes before the line
// there; ok is false when dev's sidebar does not give a place.
func placeNewGroup(groups, devGroups []sidebarGroup, name string) (at int, before, ok bool) {
	k := -1
	for i, g := range devGroups {
		if g.name == name {
			k = i
			break
		}
	}
	if k < 0 || len(groups) == 0 {
		return 0, false, false
	}
	if k == 0 {
		return groups[0].head, true, true
	}
	for d := 1; d < len(devGroups); d++ {
		if i := k - d; i >= 0 {
			if g, found := findGroup(groups, devGroups[i].name); found {
				return g.last + 1, false, true
			}
		}
		if i := k + d; i < len(devGroups) {
			if g, found := findGroup(groups, devGroups[i].name); found {
				return g.head, true, true
			}
		}
	}
	return 0, false, false
}

// orderLikeDev sorts the entries of each group into the order dev's sidebar
// lists their routes, keeping every group's entries in the same slots.
// Entries dev's sidebar does not list keep their order after the listed ones.
func orderLikeDev(entries []SidebarEntry, devSidebar string) []SidebarEntry {
	lines := sidebarLines(devSidebar)
	pos := map[string]map[string]int{}
	for _, g := range topGroups(lines) {
		if _, seen := pos[g.name]; seen {
			continue
		}
		routes := map[string]int{}
		for _, line := range lines[g.head+1 : g.last+1] {
			for _, m := range sidebarLink.FindAllStringSubmatch(line, -1) {
				if r := normalRoute(m[1]); !hasKey(routes, r) {
					routes[r] = len(routes)
				}
			}
		}
		pos[g.name] = routes
	}
	out := append([]SidebarEntry(nil), entries...)
	slots := map[string][]int{}
	for i, e := range out {
		slots[e.Group] = append(slots[e.Group], i)
	}
	for group, idx := range slots {
		routes := pos[group]
		rank := func(e SidebarEntry) int {
			if p, ok := routes[normalRoute(e.Route)]; ok {
				return p
			}
			return len(routes)
		}
		sub := make([]SidebarEntry, len(idx))
		for j, i := range idx {
			sub[j] = out[i]
		}
		sort.SliceStable(sub, func(a, b int) bool { return rank(sub[a]) < rank(sub[b]) })
		for j, i := range idx {
			out[i] = sub[j]
		}
	}
	return out
}

func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

func addEntry(sidebar string, devGroups []sidebarGroup, e SidebarEntry) string {
	item := fmt.Sprintf("  - [%s](%s)", e.Title, e.Route)
	lines := sidebarLines(sidebar)
	groups := topGroups(lines)
	if g, ok := findGroup(groups, e.Group); ok {
		return joinLines(insertLines(lines, g.last+1, item))
	}
	at, before, ok := placeNewGroup(groups, devGroups, e.Group)
	switch {
	case !ok:
		return strings.Join(lines, "\n") + "\n\n- " + e.Group + "\n" + item + "\n"
	case before:
		return joinLines(insertLines(lines, at, "- "+e.Group, item, ""))
	default:
		return joinLines(insertLines(lines, at, "", "- "+e.Group, item))
	}
}

func insertLines(lines []string, at int, add ...string) []string {
	out := append([]string{}, lines[:at]...)
	out = append(out, add...)
	return append(out, lines[at:]...)
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}
