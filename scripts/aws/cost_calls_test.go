package awsdeploy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

var costDir = filepath.Join("..", "..", "pkg", "aws", "cost")

// costSDKToIAM maps the SDK packages of the cost fetchers to their IAM
// service prefix, and operations whose IAM action has another name.
var (
	costSDKToIAM = map[string]string{
		"costexplorer":        "ce",
		"budgets":             "budgets",
		"costoptimizationhub": "cost-optimization-hub",
		"computeoptimizer":    "compute-optimizer",
	}
	costOperationToIAM = map[string]string{
		"budgets:DescribeBudgets": "budgets:ViewBudget",
	}
)

// costFetcherActions scans pkg/aws/cost for every SDK request it builds
// (a composite literal of <service>.<Operation>Input) and returns the IAM
// action each needs.
func costFetcherActions(t *testing.T) map[string]bool {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(costDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	positions := token.NewFileSet()
	out := map[string]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(positions, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		imports := sdkImports(file)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || !strings.HasSuffix(sel.Sel.Name, "Input") {
				return true
			}
			service, ok := imports[pkg.Name]
			if !ok {
				return true
			}
			prefix, known := costSDKToIAM[service]
			if !known {
				t.Errorf("%s builds a %s request; the cost roles have no mapping for that service", filepath.Base(name), service)
				return true
			}
			action := prefix + ":" + strings.TrimSuffix(sel.Sel.Name, "Input")
			if mapped, ok := costOperationToIAM[action]; ok {
				action = mapped
			}
			out[action] = true
			return true
		})
	}
	return out
}

// TestCostRolesAllowEveryCostFetcherCall ties the roles to the code: every
// request the cost fetchers send must be allowed by versus-cost-reader and
// by versus-reader's opt-in cost reads, and denied by neither. A new Cost
// Explorer call without a policy change fails here, not in a customer
// account.
func TestCostRolesAllowEveryCostFetcherCall(t *testing.T) {
	required := costFetcherActions(t)
	for _, expected := range []string{"ce:GetCostAndUsage", "ce:GetCostForecast", "budgets:ViewBudget", "cost-optimization-hub:ListRecommendations"} {
		if !required[expected] {
			t.Errorf("cost scan did not see %s; the scan is broken", expected)
		}
	}
	t.Logf("pkg/aws/cost calls %v", keys(required))
	reader := parseRoleTemplate(t, readerArtifact)
	costReader := parseRoleTemplate(t, costReaderArtifact)
	for action := range required {
		if !allowedBy(costReader.allow, action) {
			t.Errorf("versus-cost-reader does not allow %s, which pkg/aws/cost calls", action)
		}
		if !allowedBy(costReader.denyExcept, action) {
			t.Errorf("versus-cost-reader's deny-everything-else statement blocks %s", action)
		}
		if allowedBy(costReader.deny, action) {
			t.Errorf("versus-cost-reader explicitly denies %s", action)
		}
		if !allowedBy(reader.conditionalActions[optionalGate], action) {
			t.Errorf("versus-reader's opt-in cost reads do not include %s", action)
		}
		if allowedBy(reader.deny, action) {
			t.Errorf("versus-reader explicitly denies %s", action)
		}
	}
}
