package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Config is the group map read from the JSON file passed with --config.
type Config struct {
	Groups []Group `json:"groups"`
}

// Group is one feature group. A changed file belongs to the group when the
// last of its Paths that matches the file is a positive pattern; a pattern
// that starts with "!" takes a file back out. All switches on every group.
// Enables names further groups (glob patterns over group names allowed) that
// switch on whenever this one does.
type Group struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Paths       []string `json:"paths"`
	All         bool     `json:"all,omitempty"`
	Enables     []string `json:"enables,omitempty"`
}

// groupName keeps names usable as GitHub step and job output keys.
var groupName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// reservedNames are the output keys the tool writes besides the groups.
var reservedNames = map[string]bool{"full": true, "go": true, "go_packages": true}

// ParseConfig decodes and validates a group map. Unknown fields are an error
// so that a misspelt key does not silently drop a group's paths.
func ParseConfig(data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse group config: %w", err)
	}
	if len(cfg.Groups) == 0 {
		return nil, fmt.Errorf("group config has no groups")
	}
	seen := map[string]bool{}
	for _, g := range cfg.Groups {
		if !groupName.MatchString(g.Name) {
			return nil, fmt.Errorf("group name %q must match %s", g.Name, groupName)
		}
		if reservedNames[g.Name] {
			return nil, fmt.Errorf("group name %q is reserved for a tool output", g.Name)
		}
		if seen[g.Name] {
			return nil, fmt.Errorf("group %q is defined twice", g.Name)
		}
		seen[g.Name] = true
		for _, p := range g.Paths {
			if err := validatePattern(p); err != nil {
				return nil, fmt.Errorf("group %q: %w", g.Name, err)
			}
		}
	}
	for _, g := range cfg.Groups {
		for _, e := range g.Enables {
			if len(cfg.namesMatching(e)) == 0 {
				return nil, fmt.Errorf("group %q enables %q, which names no group", g.Name, e)
			}
		}
	}
	return &cfg, nil
}

func (c *Config) namesMatching(pattern string) []string {
	var out []string
	for _, g := range c.Groups {
		if ok, _ := path.Match(pattern, g.Name); ok {
			out = append(out, g.Name)
		}
	}
	return out
}

func validatePattern(p string) error {
	body := strings.TrimPrefix(p, "!")
	if body == "" || strings.HasPrefix(body, "/") || strings.HasSuffix(body, "/") {
		return fmt.Errorf("path pattern %q must be a non-empty repository-relative glob", p)
	}
	for _, seg := range strings.Split(body, "/") {
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return fmt.Errorf("path pattern %q: ** must be a whole path segment", p)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("path pattern %q: %w", p, err)
		}
	}
	return nil
}

// matchGlob reports whether the slash-separated file path matches pattern.
// "*", "?" and "[...]" work within one path segment; a "**" segment matches
// zero or more whole segments.
func matchGlob(pattern, file string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(file, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for i := 0; i <= len(segs); i++ {
				if matchSegments(rest, segs[i:]) {
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

// matches applies the group's ordered patterns: the last pattern that matches
// decides, and a "!" pattern excludes.
func (g Group) matches(file string) bool {
	in := false
	for _, p := range g.Paths {
		neg := strings.HasPrefix(p, "!")
		if matchGlob(strings.TrimPrefix(p, "!"), file) {
			in = !neg
		}
	}
	return in
}

// Selection is the result of mapping a change set onto the group map.
type Selection struct {
	Groups map[string]bool
	Full   bool
	// Reason says why Full is set, for the job log.
	Reason string
	// Unmatched lists changed files that no group claims.
	Unmatched []string
}

// Select maps changed files (repository-relative, slash-separated) to groups.
// forceAll, a file in an "all" group, or a file no group claims switches on
// every group.
func Select(cfg *Config, files []string, forceAll bool, forceReason string) Selection {
	sel := Selection{Groups: map[string]bool{}}
	for _, g := range cfg.Groups {
		sel.Groups[g.Name] = false
	}
	if forceAll {
		return fullSelection(cfg, forceReason, nil)
	}
	var unmatched []string
	allBy := ""
	for _, f := range files {
		f = strings.TrimPrefix(f, "./")
		if f == "" {
			continue
		}
		hit := false
		for _, g := range cfg.Groups {
			if g.matches(f) {
				hit = true
				sel.Groups[g.Name] = true
				if g.All && allBy == "" {
					allBy = fmt.Sprintf("%s changed (group %s switches on everything)", f, g.Name)
				}
			}
		}
		if !hit {
			unmatched = append(unmatched, f)
		}
	}
	if len(unmatched) > 0 {
		return fullSelection(cfg, fmt.Sprintf("%d changed file(s) match no group, first %s", len(unmatched), unmatched[0]), unmatched)
	}
	if allBy != "" {
		return fullSelection(cfg, allBy, nil)
	}
	// Follow "enables" until nothing new switches on.
	for changed := true; changed; {
		changed = false
		for _, g := range cfg.Groups {
			if !sel.Groups[g.Name] {
				continue
			}
			for _, e := range g.Enables {
				for _, n := range cfg.namesMatching(e) {
					if !sel.Groups[n] {
						sel.Groups[n] = true
						changed = true
					}
				}
			}
		}
	}
	return sel
}

func fullSelection(cfg *Config, reason string, unmatched []string) Selection {
	sel := Selection{Groups: map[string]bool{}, Full: true, Reason: reason, Unmatched: unmatched}
	for _, g := range cfg.Groups {
		sel.Groups[g.Name] = true
	}
	return sel
}

// GoChanged is the "go" output: Go jobs (vet, build, tests) must run. It is
// true on a full run, when any package needs testing, or when a group whose
// name starts with "go-" is on. The package list catches Go that lives
// outside the Go groups, such as the embed file in the UI folder.
func GoChanged(sel Selection, goPackages []string) bool {
	if sel.Full || len(goPackages) > 0 {
		return true
	}
	for name, on := range sel.Groups {
		if on && strings.HasPrefix(name, "go-") {
			return true
		}
	}
	return false
}
