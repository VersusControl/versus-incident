package main

import (
	"bufio"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// A finding is one credential-shaped literal in a file.
type finding struct {
	Path  string
	Line  int
	Rule  string
	Match string
}

// rule is one credential shape. A match is checked on the whole line, then
// refined by accept, which can reject a shape that only looks alike (for
// example a 40-character run that is a plain word).
type rule struct {
	name   string
	re     *regexp.Regexp
	accept func(match string) bool
}

// allowed are the example values AWS publishes in its documentation. Secret
// scanners know them as examples, so tests and docs may use them verbatim.
var allowed = map[string]bool{
	"AKIAIOSFODNN7EXAMPLE":                     true,
	"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY": true,
}

// escape is a backslash escape written in a string literal (\n, \t, \r, \"),
// so a credential that follows one, as in "key\nAKIA...", is still found.
const escape = `\\[nrt"']`

// The shapes follow what public secret scanners match: the fixed prefix of
// each provider and a body as long as a real one. A short placeholder such as
// "xoxb-test" or "sk-test-key" is not a match. Every pattern keeps the
// character before the credential outside the shape, so a word that merely
// ends in the prefix (disk-, task-) is not a match either.
var rules = []rule{
	{name: "AWS access key ID", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9]|` + escape + `)((?:AKIA|ASIA)[A-Z0-9]{16})(?:$|[^A-Za-z0-9])`)},
	{name: "AWS secret access key", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9/+=]|` + escape + `)([A-Za-z0-9/+]{40})(?:$|[^A-Za-z0-9/+=])`), accept: mixedCase},
	{name: "GitHub token", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9_]|` + escape + `)(gh[pousr]_[A-Za-z0-9]{20,})`)},
	{name: "Slack token", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9_]|` + escape + `)(xox[a-z]-[0-9]{4,}-[A-Za-z0-9-]*)`)},
	{name: "OpenAI key", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9_-]|` + escape + `)(sk-(?:proj-)?[A-Za-z0-9_-]{20,})`)},
	{name: "JWT", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9_-]|` + escape + `)(eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{3,})`)},
	{name: "private key block", re: regexp.MustCompile(`(-----BEGIN [A-Z ]*PRIVATE KEY-----)`)},
}

// mixedCase keeps a 40-character run that has an upper-case letter, a
// lower-case letter and a digit, as a generated secret does; a path, a hex
// digest or a word run does not.
func mixedCase(s string) bool {
	var upper, lower, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return upper && lower && digit
}

// scanned are the file kinds that hold code, config, scripts, templates,
// test fixtures and prose. Dockerfiles and Makefiles are matched by name.
var scanned = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".mjs": true, ".py": true, ".sh": true, ".sql": true,
	".yaml": true, ".yml": true, ".json": true, ".toml": true, ".ini": true, ".md": true,
	".tmpl": true, ".tpl": true, ".args": true, ".assert": true, ".example": true,
}

// scannedFile reports whether the scan reads the file at path.
func scannedFile(path string) bool {
	name := filepath.Base(path)
	if name == "Makefile" || strings.HasPrefix(name, "Dockerfile") {
		return true
	}
	return scanned[strings.ToLower(filepath.Ext(name))]
}

// skipped are directories that hold no committed source: version control,
// installed packages, build output and local-only test data.
var skipped = map[string]bool{".git": true, "node_modules": true, "dist": true, "testdata": true, "book": true, "tmp": true, ".worktrees": true}

// scanLines reports every credential-shaped literal in r.
func scanLines(path string, r io.Reader) ([]finding, error) {
	var found []finding
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		for _, rule := range rules {
			for _, groups := range rule.re.FindAllStringSubmatch(text, -1) {
				match := groups[1]
				if allowed[match] || (rule.accept != nil && !rule.accept(match)) {
					continue
				}
				found = append(found, finding{Path: path, Line: line, Rule: rule.name, Match: match})
			}
		}
	}
	return found, scanner.Err()
}

// scanTree walks root and scans every file of a scanned kind.
func scanTree(fsys fs.FS) ([]finding, error) {
	var found []finding
	err := fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && skipped[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !scannedFile(path) {
			return nil
		}
		file, err := fsys.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		more, err := scanLines(path, file)
		found = append(found, more...)
		return err
	})
	return found, err
}
