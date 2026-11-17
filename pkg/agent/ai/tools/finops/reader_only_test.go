package finops

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"

	core_finops "github.com/VersusControl/versus-incident/pkg/finops"
)

// The tools are given a Reader, never the full Service.
var _ ServiceFunc = func() core_finops.Reader { return nil }

// TestToolsDependOnlyOnTheReader: the package names nothing from the FinOps
// core that can write, and the Reader it holds has no write method.
func TestToolsDependOnlyOnTheReader(t *testing.T) {
	for _, f := range mustGlob(t, "*.go") {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		alias := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "github.com/VersusControl/versus-incident/pkg/finops" && imp.Name != nil {
				alias = imp.Name.Name
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == alias {
				switch sel.Sel.Name {
				case "Service", "NewService", "Store", "Meter", "Execute":
					t.Errorf("%s uses %s.%s; tools may hold only the Reader", f, alias, sel.Sel.Name)
				}
			}
			return true
		})
	}
	reader := reflect.TypeOf((*core_finops.Reader)(nil)).Elem()
	writes := []string{"SetIncreaseStatus", "DismissSaving", "PutBudgets", "FreezeReport", "SyncIncreases", "MarkIncreaseAlerted"}
	for i := range reader.NumMethod() {
		for _, w := range writes {
			if reader.Method(i).Name == w {
				t.Fatalf("Reader exposes the write %s", w)
			}
		}
	}
	service := reflect.TypeOf(&core_finops.Service{})
	for _, w := range writes {
		if _, ok := service.MethodByName(w); !ok {
			t.Fatalf("the write list is stale: Service has no %s", w)
		}
	}
}
