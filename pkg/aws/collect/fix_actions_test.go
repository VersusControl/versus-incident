package collect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
)

// iamRenames are the detail calls whose IAM action is not named after the
// SDK operation.
var iamRenames = map[string]string{
	"GetPublicAccessBlock": "GetBucketPublicAccessBlock",
	"GetBucketEncryption":  "GetEncryptionConfiguration",
}

// detailOperations scans the collector sources for every operation a unit
// names in a detail or cap gap, keyed by the unit service whose run
// function names it.
func detailOperations(t *testing.T) map[string][]string {
	t.Helper()
	serviceOf := map[string]string{}
	for _, entry := range specs {
		name := runtime.FuncForPC(reflect.ValueOf(entry.run).Pointer()).Name()
		serviceOf[name[strings.LastIndex(name, ".")+1:]] = entry.service
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	positions := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(positions, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				callExpr, ok := node.(*ast.CallExpr)
				if !ok || len(callExpr.Args) == 0 {
					return true
				}
				selector, ok := callExpr.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "detailGap" && selector.Sel.Name != "capGap" {
					return true
				}
				literal, ok := callExpr.Args[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Errorf("%s: %s names its operation with a non-literal", positions.Position(callExpr.Pos()), selector.Sel.Name)
					return true
				}
				operation, _ := strconv.Unquote(literal.Value)
				service, ok := serviceOf[function.Name.Name]
				if !ok {
					t.Errorf("%s: %s(%q) is outside a unit's run function", positions.Position(callExpr.Pos()), selector.Sel.Name, operation)
					return true
				}
				if !slices.Contains(out[service], operation) {
					out[service] = append(out[service], operation)
				}
				return true
			})
		}
	}
	return out
}

// Copy fix pastes a row's fix into the reader policy, so every detail call
// a unit can name in a gap has its own fix, and that fix holds the IAM
// action the call needs.
func TestEveryDetailOperationFixNamesItsOwnAction(t *testing.T) {
	operations := detailOperations(t)
	if len(operations) < 10 {
		t.Fatalf("found detail operations for %d services; the scan is broken", len(operations))
	}
	for service, names := range operations {
		for _, operation := range names {
			fix, ok := catalog.OperationFix(service, operation)
			if !ok {
				t.Errorf("%s %s has no fix of its own", service, operation)
				continue
			}
			action := operation
			if renamed, ok := iamRenames[operation]; ok {
				action = renamed
			}
			found := false
			for _, entry := range fix {
				if strings.HasSuffix(entry, ":"+action) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s %s fix = %v, want an action named %s", service, operation, fix, action)
			}
			if got := catalog.FixFor(service, operation); !slices.Equal(got, fix) {
				t.Errorf("%s %s FixFor = %v, want %v", service, operation, got, fix)
			}
		}
	}
	if fix := catalog.FixFor("lambda", ""); !slices.Equal(fix, catalog.FixActionsFor("lambda")) {
		t.Errorf("whole-unit fix = %v, want the service's", fix)
	}
}
