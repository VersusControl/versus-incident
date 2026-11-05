package aws

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	// sdkModulePath is the AWS SDK module every check below recognises.
	sdkModulePath = "github.com/aws/aws-sdk-go-v2"
	// loadDefaultConfigImport is the ambient configuration loader this surface
	// must never use; probe credentials come only from the assumed reader role.
	loadDefaultConfigImport = sdkModulePath + "/config"
	// sdkServicePath marks the generated clients that carry the operations.
	sdkServicePath = sdkModulePath + "/service/"
)

// allowedOperationPrefixes is the complete read-only operation surface.
var allowedOperationPrefixes = []string{
	"Describe",
	"List",
	"Get",
	"Lookup",
	"Search",
	"AssumeRole",
	"GetCallerIdentity",
}

// nonOperationMethods are exported client methods that only read local
// configuration and are not AWS operations.
var nonOperationMethods = map[string]bool{
	"Options": true,
}

func TestAWSProductionSurfaceIsReadOnly(t *testing.T) {
	roots := []string{".", filepath.Join("..", "awsauth")}
	var files []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(name, ".go") {
				return nil
			}
			files = append(files, name)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) == 0 {
		t.Fatal("no AWS production files found")
	}
	sort.Strings(files)

	operations := 0
	for _, name := range files {
		positions, parsed := parseAWSFile(t, name)
		violations, found := scanReadOnlySurface(positions, parsed)
		operations += found
		for _, violation := range violations {
			t.Errorf("%s: %s", name, violation)
		}
	}
	if operations == 0 {
		t.Fatal("no AWS SDK operations found to check")
	}
}

// TestReadOnlySurfaceScannerSeesEveryClientShape proves the scan itself: a
// struct-field client, a constructor-assigned client, an inline chain and a
// package-qualified call are all caught, while read-only calls pass.
func TestReadOnlySurfaceScannerSeesEveryClientShape(t *testing.T) {
	const writes = `package scratch

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type holder struct {
	ec2Client *ec2.Client
}

func (h holder) fieldWrite(ctx context.Context) {
	_, _ = h.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{})
}

func localWrite(ctx context.Context) {
	store := s3.NewFromConfig(aws.Config{})
	_, _ = store.PutObject(ctx, &s3.PutObjectInput{})
}

func chainedWrite(ctx context.Context) {
	_, _ = s3.NewFromConfig(aws.Config{}).DeleteObject(ctx, &s3.DeleteObjectInput{})
}

func packageWrite(ctx context.Context) {
	_, _ = s3.PutObject(ctx, &s3.PutObjectInput{})
}
`
	positions, parsed := parseTestSource(t, writes)
	violations, operations := scanReadOnlySurface(positions, parsed)
	if operations != 4 {
		t.Fatalf("operations = %d, want 4 (violations: %v)", operations, violations)
	}
	for _, operation := range []string{"TerminateInstances", "PutObject", "DeleteObject"} {
		if !containsViolation(violations, operation) {
			t.Fatalf("scan missed %s: %v", operation, violations)
		}
	}

	const reads = `package scratch

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type reader struct {
	ec2Client *ec2.Client
}

func (r reader) read(ctx context.Context) {
	_, _ = r.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{})
	buckets := s3.NewFromConfig(aws.Config{})
	_, _ = buckets.ListBuckets(ctx, &s3.ListBucketsInput{})
	_, _ = s3.NewFromConfig(aws.Config{}).GetObject(ctx, &s3.GetObjectInput{})
	_ = aws.String("read-only helper")
}
`
	positions, parsed = parseTestSource(t, reads)
	violations, operations = scanReadOnlySurface(positions, parsed)
	if len(violations) != 0 {
		t.Fatalf("read-only source reported %v", violations)
	}
	if operations != 3 {
		t.Fatalf("read-only operations = %d, want 3", operations)
	}
}

func parseAWSFile(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	positions := token.NewFileSet()
	parsed, err := parser.ParseFile(positions, name, content, 0)
	if err != nil {
		t.Fatal(err)
	}
	return positions, parsed
}

func parseTestSource(t *testing.T, source string) (*token.FileSet, *ast.File) {
	t.Helper()
	positions := token.NewFileSet()
	parsed, err := parser.ParseFile(positions, "scratch.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	return positions, parsed
}

func containsViolation(violations []string, operation string) bool {
	for _, violation := range violations {
		if strings.Contains(violation, `"`+operation+`"`) {
			return true
		}
	}
	return false
}

// scanReadOnlySurface reports every SDK operation outside the read-only
// surface and counts the operations it inspected. Receivers are resolved from
// declared client types, constructor assignments and struct-field names, so a
// client is checked whatever it is called. Names are resolved per function, so
// a variable that happens to be called "client" but is not an SDK client does
// not create a false positive.
func scanReadOnlySurface(positions *token.FileSet, file *ast.File) ([]string, int) {
	packages := map[string]bool{}
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, `"`)
		if !strings.HasPrefix(path, sdkServicePath) {
			continue
		}
		alias := path[strings.LastIndex(path, "/")+1:]
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		packages[alias] = true
	}

	fields := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		structure, ok := node.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range structure.Fields.List {
			if isSDKClientType(field.Type, packages) {
				for _, name := range field.Names {
					fields[name.Name] = true
				}
			}
		}
		return true
	})

	scanner := surfaceScanner{positions: positions, packages: packages, fields: fields}
	packageClients := map[string]bool{}
	for _, declaration := range file.Decls {
		if general, ok := declaration.(*ast.GenDecl); ok {
			collectDeclaredClients(general, packages, packageClients)
		}
	}

	violations := make([]string, 0)
	operations := 0
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			clients := copyNames(packageClients)
			addClientFields(typed.Recv, packages, clients)
			addClientFields(typed.Type.Params, packages, clients)
			addClientFields(typed.Type.Results, packages, clients)
			if typed.Body == nil {
				continue
			}
			collectDeclaredClients(typed.Body, packages, clients)
			found, count := scanner.scanCalls(typed.Body, clients)
			violations = append(violations, found...)
			operations += count
		case *ast.GenDecl:
			found, count := scanner.scanCalls(typed, packageClients)
			violations = append(violations, found...)
			operations += count
		}
	}
	return violations, operations
}

type surfaceScanner struct {
	positions *token.FileSet
	packages  map[string]bool
	fields    map[string]bool
}

func (scanner surfaceScanner) scanCalls(root ast.Node, clients map[string]bool) ([]string, int) {
	violations := make([]string, 0)
	operations := 0
	ast.Inspect(root, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		line := scanner.positions.Position(call.Pos()).Line
		if receiver, ok := clientReceiver(selector.X, clients, scanner.fields, scanner.packages); ok {
			if nonOperationMethods[selector.Sel.Name] {
				return true
			}
			operations++
			if !hasAllowedPrefix(selector.Sel.Name) {
				violations = append(violations, "line "+strconv.Itoa(line)+" calls SDK operation \""+selector.Sel.Name+"\" on "+receiver+" outside the read-only surface")
			}
			return true
		}
		if identifier, ok := selector.X.(*ast.Ident); ok && scanner.packages[identifier.Name] {
			if strings.HasPrefix(selector.Sel.Name, "New") {
				return true
			}
			operations++
			violations = append(violations, "line "+strconv.Itoa(line)+" calls \""+identifier.Name+"."+selector.Sel.Name+"\" through an SDK client package")
		}
		return true
	})
	return violations, operations
}

func collectDeclaredClients(root ast.Node, packages, clients map[string]bool) {
	ast.Inspect(root, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.ValueSpec:
			if isSDKClientType(typed.Type, packages) {
				for _, name := range typed.Names {
					clients[name.Name] = true
				}
			}
			for index, name := range typed.Names {
				if index < len(typed.Values) && isSDKConstructor(typed.Values[index], packages) {
					clients[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			if len(typed.Lhs) != len(typed.Rhs) {
				return true
			}
			for index, value := range typed.Rhs {
				if !isSDKConstructor(value, packages) {
					continue
				}
				if name, ok := typed.Lhs[index].(*ast.Ident); ok {
					clients[name.Name] = true
				}
			}
		}
		return true
	})
}

func addClientFields(list *ast.FieldList, packages, clients map[string]bool) {
	if list == nil {
		return
	}
	for _, field := range list.List {
		if !isSDKClientType(field.Type, packages) {
			continue
		}
		for _, name := range field.Names {
			clients[name.Name] = true
		}
	}
}

func copyNames(source map[string]bool) map[string]bool {
	clone := make(map[string]bool, len(source))
	for name := range source {
		clone[name] = true
	}
	return clone
}

func clientReceiver(expr ast.Expr, clients, fields, packages map[string]bool) (string, bool) {
	switch typed := expr.(type) {
	case *ast.Ident:
		if clients[typed.Name] {
			return typed.Name, true
		}
	case *ast.ParenExpr:
		return clientReceiver(typed.X, clients, fields, packages)
	case *ast.SelectorExpr:
		if fields[typed.Sel.Name] {
			return typed.Sel.Name, true
		}
	case *ast.CallExpr:
		if isSDKConstructor(typed, packages) {
			return "a freshly built client", true
		}
	}
	return "", false
}

func isSDKClientType(expr ast.Expr, packages map[string]bool) bool {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return isSDKClientType(typed.X, packages)
	case *ast.SelectorExpr:
		identifier, ok := typed.X.(*ast.Ident)
		return ok && packages[identifier.Name] && strings.HasSuffix(typed.Sel.Name, "Client")
	}
	return false
}

func isSDKConstructor(expr ast.Expr, packages map[string]bool) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	if !ok || !packages[identifier.Name] {
		return false
	}
	switch selector.Sel.Name {
	case "NewFromConfig", "NewFromClient", "NewFromOptions", "New":
		return true
	}
	return false
}

func hasAllowedPrefix(operation string) bool {
	for _, prefix := range allowedOperationPrefixes {
		if strings.HasPrefix(operation, prefix) {
			return true
		}
	}
	return false
}
