package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// FallbackTag is used only when the registry has no dev-<sha> tag at all.
const FallbackTag = "dev"

var devTag = regexp.MustCompile(`^dev-([0-9a-f]{7,40})$`)

// packageVersion is the part of a GitHub package version the lookup reads.
type packageVersion struct {
	Metadata struct {
		Container struct {
			Tags []string `json:"tags"`
		} `json:"container"`
	} `json:"metadata"`
}

// ParseVersions reads GitHub package versions: JSON arrays (one per page, as
// `gh api --paginate` prints them), single objects, or a mix.
func ParseVersions(r io.Reader) ([]string, error) {
	dec := json.NewDecoder(r)
	var tags []string
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("read package versions: %w", err)
		}
		raw = bytes.TrimSpace(raw)
		var versions []packageVersion
		if len(raw) > 0 && raw[0] == '[' {
			if err := json.Unmarshal(raw, &versions); err != nil {
				return nil, fmt.Errorf("read package versions: %w", err)
			}
		} else {
			var v packageVersion
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("read package versions: %w", err)
			}
			versions = append(versions, v)
		}
		for _, v := range versions {
			tags = append(tags, v.Metadata.Container.Tags...)
		}
	}
	return tags, nil
}

// PickDevTag returns the dev-<sha> tag whose commit is newest on dev.
// commits lists dev's history newest first (git rev-list order); a tag
// counts only when its short SHA starts one of those commits. found is false
// when no tag qualifies, and the caller falls back to FallbackTag.
func PickDevTag(tags []string, commits []string) (tag string, found bool) {
	best := -1
	for _, t := range tags {
		m := devTag.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		for i, c := range commits {
			if best >= 0 && i >= best {
				break
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(c)), m[1]) {
				best, tag = i, t
				break
			}
		}
	}
	return tag, best >= 0
}

// Affected reports whether a change to the dev branch can change the site:
// a listed page, a docs image or the list itself. The build always runs
// main's tools, so tool changes on dev do not count.
func Affected(cfg *Config, files []string) bool {
	always := []string{
		"src/docs/images/**",
		".github/dev-docs.json",
	}
	for _, f := range files {
		for _, p := range always {
			if matchGlob(p, f) {
				return true
			}
		}
		if rel, ok := strings.CutPrefix(f, "src/"); ok {
			for _, p := range cfg.Pages {
				if matchGlob(p.Path, rel) {
					return true
				}
			}
		}
	}
	return false
}
