package main

import (
	"path"
	"sort"
	"strings"
)

// allPackages is the go_packages value that means "every package".
const allPackages = "./..."

// ListPackage holds the fields the tool reads from `go list -deps -test -json`.
// Dir is relative to the module root and slash-separated; the git wrapper
// converts go list's absolute paths before calling GoPackages.
type ListPackage struct {
	ImportPath      string
	Name            string
	Dir             string
	ForTest         string
	DepOnly         bool
	Standard        bool
	Module          *struct{ Main bool }
	Deps            []string
	GoFiles         []string
	CgoFiles        []string
	SFiles          []string
	OtherFiles      []string
	IgnoredGoFiles  []string
	TestGoFiles     []string
	XTestGoFiles    []string
	EmbedFiles      []string
	TestEmbedFiles  []string
	XTestEmbedFiles []string
}

// moduleFiles are files whose change can alter every package's build.
var moduleFiles = map[string]bool{"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true}

// ModuleRelative keeps the changed files that sit inside the module and makes
// them relative to it. moduleRel is the module directory relative to the
// repository root ("." for the root).
func ModuleRelative(moduleRel string, files []string) []string {
	moduleRel = path.Clean(moduleRel)
	var out []string
	for _, f := range files {
		f = path.Clean(strings.TrimPrefix(f, "./"))
		switch {
		case moduleRel == ".":
			out = append(out, f)
		case strings.HasPrefix(f, moduleRel+"/"):
			out = append(out, strings.TrimPrefix(f, moduleRel+"/"))
		}
	}
	return out
}

// TouchesModuleFile reports whether a module-relative change set includes
// go.mod, go.sum or a workspace file at the module root.
func TouchesModuleFile(files []string) bool {
	for _, f := range files {
		if moduleFiles[f] {
			return true
		}
	}
	return false
}

// MayTouchGo is the cheap check that decides whether go list is worth
// running: a changed .go file, or a changed file in or below a directory
// that holds Go source. hasGo reports whether a module-relative directory
// holds a .go file.
func MayTouchGo(files []string, hasGo func(dir string) bool) bool {
	for _, f := range files {
		if strings.HasSuffix(f, ".go") || moduleFiles[f] {
			return true
		}
		for d := path.Dir(f); ; d = path.Dir(d) {
			if hasGo(d) {
				return true
			}
			if d == "." {
				break
			}
		}
	}
	return false
}

// GoPackages returns the sorted import paths of the module packages a change
// set affects: the packages that own a changed file, and every module
// package whose build or test depends on a package whose non-test code
// changed. files are relative to the module root. A package owns a file in
// its directory that is Go source or one of its listed files, an embedded
// file, or anything under its testdata directory. A test file, a test-only
// embedded file or a testdata file affects only its own package, because no
// other package can import a test. A module-file change returns ./... .
func GoPackages(files []string, pkgs []ListPackage) []string {
	if TouchesModuleFile(files) {
		return []string{allPackages}
	}

	// Module packages, by import path. A directory whose files are all
	// excluded by build tags is listed but cannot be named to go test, which
	// testable checks.
	local := map[string]ListPackage{}
	for _, p := range pkgs {
		if p.Module == nil || !p.Module.Main || p.Standard || p.ForTest != "" || isTestMain(p) || p.DepOnly {
			continue
		}
		local[p.ImportPath] = p
	}

	changed := map[string]bool{}
	selected := map[string]bool{}
	for importPath, p := range local {
		build, test := owns(p, files)
		if build {
			changed[importPath] = true
		}
		if (build || test) && testable(p) {
			selected[importPath] = true
		}
	}

	for _, p := range pkgs {
		owner := ownerOf(p)
		lp, ok := local[owner]
		if !ok || !testable(lp) {
			continue
		}
		if changed[stripVariant(p.ImportPath)] {
			selected[owner] = true
			continue
		}
		for _, d := range p.Deps {
			if changed[stripVariant(d)] {
				selected[owner] = true
				break
			}
		}
	}
	var out []string
	for p := range selected {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ownerOf names the package whose `go test` builds entry p: a test variant
// and a generated test main both belong to the package under test.
func ownerOf(p ListPackage) string {
	if p.ForTest != "" {
		return p.ForTest
	}
	if isTestMain(p) {
		return strings.TrimSuffix(p.ImportPath, ".test")
	}
	return stripVariant(p.ImportPath)
}

func isTestMain(p ListPackage) bool {
	return p.Name == "main" && strings.HasSuffix(p.ImportPath, ".test")
}

// stripVariant drops the " [x.test]" suffix go list adds to test variants.
func stripVariant(importPath string) string {
	if i := strings.Index(importPath, " ["); i >= 0 {
		return importPath[:i]
	}
	return importPath
}

func testable(p ListPackage) bool {
	return len(p.GoFiles)+len(p.CgoFiles)+len(p.TestGoFiles)+len(p.XTestGoFiles) > 0
}

// owns reports whether the change set touches p's non-test code (build) or
// only its tests (test).
func owns(p ListPackage, files []string) (build, test bool) {
	dir := path.Clean(p.Dir)
	buildFiles := map[string]bool{}
	for _, list := range [][]string{p.GoFiles, p.CgoFiles, p.SFiles, p.OtherFiles, p.IgnoredGoFiles, p.EmbedFiles} {
		for _, f := range list {
			buildFiles[path.Join(dir, f)] = true
		}
	}
	testFiles := map[string]bool{}
	for _, list := range [][]string{p.TestGoFiles, p.XTestGoFiles, p.TestEmbedFiles, p.XTestEmbedFiles} {
		for _, f := range list {
			testFiles[path.Join(dir, f)] = true
		}
	}
	testdata := path.Join(dir, "testdata") + "/"
	for _, f := range files {
		switch {
		case buildFiles[f]:
			build = true
		case testFiles[f], strings.HasPrefix(f, testdata):
			test = true
		case path.Dir(f) == dir && strings.HasSuffix(f, "_test.go"):
			// A deleted test file, which go list no longer lists.
			test = true
		case path.Dir(f) == dir && strings.HasSuffix(f, ".go"):
			// A deleted source file.
			build = true
		}
	}
	return build, test
}
