package collect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// scanInputs are the request types of read calls billed by the data they
// read rather than free: a DynamoDB table scan or PartiQL statement reads
// (and bills) every item, and S3 Select bills the bytes it scans. A
// collector cannot make one of these calls without naming its input type.
var scanInputs = map[string]bool{
	"ScanInput":                  true,
	"NewScanPaginator":           true,
	"ExecuteStatementInput":      true,
	"BatchExecuteStatementInput": true,
	"SelectObjectContentInput":   true,
}

// No collector source scans a table or a bucket's contents.
func TestCollectorsNeverScanData(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	positions := token.NewFileSet()
	read := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(positions, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		read++
		ast.Inspect(file, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && scanInputs[ident.Name] {
				t.Errorf("%s: %s reads billed data", positions.Position(ident.Pos()), ident.Name)
			}
			return true
		})
	}
	if read < 10 {
		t.Fatalf("read %d collector sources; the scan is not looking at the package", read)
	}
}
