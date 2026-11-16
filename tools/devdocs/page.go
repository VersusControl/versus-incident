package main

import (
	"regexp"
	"strings"
)

// BadgeLine is the "In development" subtitle placed under a dev page's title.
// src/index.html styles the dev-badge class.
const BadgeLine = `<p class="dev-badge">In development</p>`

// InsertBadge puts the badge on its own paragraph right after the page's
// first "# " heading. Front matter at the top is passed over. It returns the
// page unchanged with ok=false when the page has no such heading, and leaves
// a page that already carries the badge as it is.
func InsertBadge(page string) (string, bool) {
	lines := strings.Split(page, "\n")
	start := 0
	if len(lines) > 0 && strings.TrimRight(lines[0], "\r") == "---" {
		for i := 1; i < len(lines); i++ {
			if t := strings.TrimRight(lines[i], "\r"); t == "---" || t == "..." {
				start = i + 1
				break
			}
		}
	}
	inFence := false
	for i := start; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(lines[i], "# ") {
			continue
		}
		// Already badged: the next non-blank line is the badge.
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if strings.TrimSpace(lines[j]) == BadgeLine {
				return page, true
			}
			break
		}
		insert := []string{"", BadgeLine}
		if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" {
			insert = append(insert, "")
		}
		out := append([]string{}, lines[:i+1]...)
		out = append(out, insert...)
		out = append(out, lines[i+1:]...)
		return strings.Join(out, "\n"), true
	}
	return page, false
}

// FirstHeading returns the text of the page's first "# " heading.
func FirstHeading(page string) string {
	inFence := false
	for _, line := range strings.Split(page, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line[2:]), "#"))
		}
	}
	return ""
}

// RewriteImage points every tagged reference to image at tag, anywhere in the
// page: prose, tables, inline code and code blocks. A digest after the tag
// (":v1@sha256:...") is dropped, since it pins the old image. Only the exact
// image is changed; a longer name that starts the same, a host that merely
// ends in the same name, another repository or an untagged reference is left
// alone. A full stop that ends a sentence right after the tag is kept out of
// the tag.
func RewriteImage(page, image, tag string) (string, int) {
	re := regexp.MustCompile(`(^|[^A-Za-z0-9._/-])` + regexp.QuoteMeta(image) +
		`:([A-Za-z0-9_][A-Za-z0-9_.-]{0,127})(@sha256:[0-9a-f]{64})?`)
	n := 0
	out := re.ReplaceAllStringFunc(page, func(m string) string {
		sub := re.FindStringSubmatch(m)
		prefix, old, digest := sub[1], sub[2], sub[3]
		trail := ""
		if digest == "" {
			for strings.HasSuffix(old, ".") {
				old = strings.TrimSuffix(old, ".")
				trail += "."
			}
		}
		if old == "" {
			return m
		}
		if old != tag || digest != "" {
			n++
		}
		return prefix + image + ":" + tag + trail
	})
	return out, n
}
