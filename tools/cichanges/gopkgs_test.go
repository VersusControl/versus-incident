package main

import (
	"reflect"
	"testing"
)

const mod = "example.com/m"

var mainModule = &struct{ Main bool }{Main: true}

// pkgEntries builds what `go list -deps -test -json ./...` reports for one
// module package: the package, its test variant and its test main.
func pkgEntries(name, dir string, imports, testImports []string, extra func(*ListPackage)) []ListPackage {
	path := mod + "/" + dir
	p := ListPackage{
		ImportPath:  path,
		Name:        name,
		Dir:         dir,
		Module:      mainModule,
		Deps:        imports,
		GoFiles:     []string{name + ".go"},
		TestGoFiles: []string{name + "_test.go"},
	}
	if extra != nil {
		extra(&p)
	}
	variant := ListPackage{
		ImportPath: path + " [" + path + ".test]",
		Name:       name,
		Dir:        dir,
		ForTest:    path,
		Module:     mainModule,
		Deps:       append(append([]string{}, imports...), testImports...),
	}
	testMain := ListPackage{
		ImportPath: path + ".test",
		Name:       "main",
		Dir:        dir,
		Module:     mainModule,
		Deps:       append(append([]string{path + " [" + path + ".test]"}, imports...), testImports...),
	}
	return []ListPackage{p, variant, testMain}
}

func p(dir string) string { return mod + "/" + dir }

// fixture: aws/model <- finops <- controllers <- cmd; kube is separate and
// only its tests use the fakes package; tagged holds only build-tagged files.
func fixturePackages() []ListPackage {
	var all []ListPackage
	all = append(all, ListPackage{ImportPath: "fmt", Name: "fmt", Standard: true})
	all = append(all, pkgEntries("model", "pkg/aws/model", []string{"fmt"}, nil, nil)...)
	all = append(all, pkgEntries("finops", "pkg/finops", []string{p("pkg/aws/model"), "fmt"}, nil, func(lp *ListPackage) {
		lp.TestEmbedFiles = []string{"golden/cost.json"}
	})...)
	all = append(all, pkgEntries("controllers", "pkg/controllers", []string{p("pkg/finops"), p("pkg/aws/model")}, nil, func(lp *ListPackage) {
		lp.EmbedFiles = []string{"templates/page.html"}
	})...)
	all = append(all, pkgEntries("main", "cmd", []string{p("pkg/controllers"), p("pkg/finops"), p("pkg/aws/model")}, nil, nil)...)
	all = append(all, pkgEntries("fakes", "tests/fakes", nil, nil, nil)...)
	all = append(all, pkgEntries("kube", "pkg/kube", []string{"fmt"}, []string{p("tests/fakes")}, nil)...)
	// The UI folder holds one Go file that embeds the built UI.
	all = append(all, pkgEntries("ui", "ui", nil, nil, func(lp *ListPackage) {
		lp.GoFiles = []string{"embed.go"}
		lp.TestGoFiles = nil
		lp.EmbedFiles = []string{"dist/index.html"}
	})...)
	all = append(all, ListPackage{
		ImportPath:     p("tests/tagged"),
		Name:           "tagged",
		Dir:            "tests/tagged",
		Module:         mainModule,
		IgnoredGoFiles: []string{"tagged_test.go"},
	})
	return all
}

func TestGoPackages(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  []string
	}{
		{"model change tests its dependents", []string{"pkg/aws/model/model.go"},
			[]string{p("cmd"), p("pkg/aws/model"), p("pkg/controllers"), p("pkg/finops")}},
		{"leaf change tests only itself and importers", []string{"pkg/controllers/controllers.go"},
			[]string{p("cmd"), p("pkg/controllers")}},
		{"test-only import counts as a dependent", []string{"tests/fakes/fakes.go"},
			[]string{p("pkg/kube"), p("tests/fakes")}},
		{"test file change", []string{"pkg/kube/kube_test.go"}, []string{p("pkg/kube")}},
		{"test file change does not reach importers", []string{"pkg/aws/model/model_test.go"}, []string{p("pkg/aws/model")}},
		{"deleted test file", []string{"pkg/aws/model/gone_test.go"}, []string{p("pkg/aws/model")}},
		{"test-only embedded file", []string{"pkg/finops/golden/cost.json"}, []string{p("pkg/finops")}},
		{"deleted go file still maps to its package", []string{"pkg/kube/gone.go"}, []string{p("pkg/kube")}},
		{"testdata file", []string{"pkg/kube/testdata/case.json"}, []string{p("pkg/kube")}},
		{"embedded file", []string{"pkg/controllers/templates/page.html"}, []string{p("cmd"), p("pkg/controllers")}},
		{"non-Go file beside a package is not Go", []string{"pkg/kube/README.md"}, nil},
		{"tag-only package cannot be named to go test", []string{"tests/tagged/tagged_test.go"}, nil},
		{"no Go", []string{"src/a.md"}, nil},
		{"go file in the ui folder", []string{"ui/embed.go"}, []string{p("ui")}},
		{"ui source is not Go", []string{"ui/src/App.tsx", "ui/package.json"}, nil},
		{"go.mod tests everything", []string{"go.mod"}, []string{"./..."}},
		{"go.sum tests everything", []string{"pkg/kube/kube.go", "go.sum"}, []string{"./..."}},
	}
	pkgs := fixturePackages()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := GoPackages(c.files, pkgs); !reflect.DeepEqual(got, c.want) {
				t.Errorf("GoPackages(%v) = %v, want %v", c.files, got, c.want)
			}
		})
	}
}

func TestModuleRelative(t *testing.T) {
	files := []string{"go.mod", "sub/go.mod", "sub/pkg/a.go", "./sub/x.md", "other/b.go"}
	if got, want := ModuleRelative(".", files), []string{"go.mod", "sub/go.mod", "sub/pkg/a.go", "sub/x.md", "other/b.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("root module: got %v, want %v", got, want)
	}
	if got, want := ModuleRelative("sub", files), []string{"go.mod", "pkg/a.go", "x.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sub module: got %v, want %v", got, want)
	}
}

func TestMayTouchGo(t *testing.T) {
	goDirs := map[string]bool{"pkg/a": true, ".": false, "ui": true}
	hasGo := func(dir string) bool { return goDirs[dir] }
	cases := []struct {
		files []string
		want  bool
	}{
		{[]string{"src/a.md", "README.md"}, false},
		{[]string{"pkg/b/c.go"}, true},
		{[]string{"pkg/a/testdata/x.json"}, true},
		{[]string{"ui/src/App.tsx"}, true},
		{[]string{"go.sum"}, true},
	}
	for _, c := range cases {
		if got := MayTouchGo(c.files, hasGo); got != c.want {
			t.Errorf("MayTouchGo(%v) = %t, want %t", c.files, got, c.want)
		}
	}
}
