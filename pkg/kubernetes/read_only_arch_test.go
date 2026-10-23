package kubernetes

import (
	"bytes"
	"go/ast"
	"go/format"
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

func TestKubernetesProductionSurfaceIsReadOnly(t *testing.T) {
	roots := []struct {
		path       string
		controller bool
	}{
		{path: "."},
		{path: filepath.Join("..", "agent", "ai", "tools", "k8s")},
		{path: filepath.Join("..", "controllers"), controller: true},
	}
	var files []string
	for _, root := range roots {
		err := filepath.WalkDir(root.path, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			base := filepath.Base(name)
			if entry.IsDir() || !strings.HasSuffix(base, ".go") || strings.HasSuffix(base, "_test.go") {
				return nil
			}
			if root.controller && !strings.HasPrefix(base, "kubernetes") {
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
		t.Fatal("no Kubernetes production files found")
	}
	sort.Strings(files)
	for _, name := range files {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		positions := token.NewFileSet()
		parsed, err := parser.ParseFile(positions, name, content, 0)
		if err != nil {
			t.Fatal(err)
		}
		surface := string(content)
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Name.Name == "auditKubernetesSettings" {
				ast.Inspect(function.Body, func(node ast.Node) bool {
					expression, ok := node.(*ast.BinaryExpr)
					if ok && allowedLocalSettingsAuditComparison(name, function, expression) {
						start, end := positions.Position(expression.Pos()).Offset, positions.Position(expression.End()).Offset
						surface = surface[:start] + strings.Repeat(" ", end-start) + surface[end:]
					}
					return true
				})
			}
			if !ok || function.Name.Name != "Register" || function.Recv == nil || len(function.Recv.List) != 1 {
				continue
			}
			receiver, ok := function.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			kind, ok := receiver.X.(*ast.Ident)
			if !ok || kind.Name != "KubernetesAdminController" {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if ok && allowedLocalSettingsRoute(name, call) {
					start, end := positions.Position(call.Pos()).Offset, positions.Position(call.End()).Offset
					surface = strings.ReplaceAll(surface, string(content[start:end]), "")
				}
				return true
			})
		}
		lower := strings.ToLower(string(content))
		for _, forbidden := range []string{"\"os/exec\"", "exec.command", "pkg/common", "k8s.io/client-go", "helm.sh/helm", "/exec", "/proxy", "/helm"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("%s contains forbidden read-only surface %q", name, forbidden)
			}
		}
		credentialAuthFile := strings.HasSuffix(name, "_auth.go") || strings.HasSuffix(name, "auth.go") || strings.HasSuffix(name, "gke_native.go")
		if !credentialAuthFile {
			for _, forbidden := range []string{"methodpost", "methodput", "methodpatch", "methoddelete", "methodconnect", ".post(", ".put(", ".patch(", ".delete("} {
				if strings.Contains(strings.ToLower(surface), forbidden) {
					t.Errorf("%s contains forbidden read-only surface %q", name, forbidden)
				}
			}
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			method := strings.ToLower(function.Name.Name)
			if filepath.Clean(name) == filepath.Join("breaklogs", "settings.go") && function.Name.Name == "UpdateSettings" && function.Recv == nil {
				continue
			}
			if allowedUsageHistoryStorageFunction(name, function) {
				continue
			}
			for _, prefix := range []string{"create", "update", "delete", "patch", "apply", "mutate", "proxy", "installhelm"} {
				if strings.HasPrefix(method, prefix) {
					t.Errorf("%s declares mutation method %s", name, function.Name.Name)
				}
			}
		}
	}
}

// The history bridge mutates only Versus storage, not the Kubernetes API.
// This exception skips the mutation-name heuristic for exactly one signature;
// file-wide transport prohibitions still run, and every body call is checked.
func allowedUsageHistoryStorageFunction(name string, function *ast.FuncDecl) bool {
	if filepath.Clean(name) != "usage_history_bridge.go" || function.Name.Name != "updateUsageManifest" || function.Recv == nil || len(function.Recv.List) != 1 || function.Body == nil {
		return false
	}
	field := function.Recv.List[0]
	if len(field.Names) != 1 || field.Names[0].Name != "service" {
		return false
	}
	pointer, ok := field.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	owner, ok := pointer.X.(*ast.Ident)
	if !ok || owner.Name != "Service" {
		return false
	}
	var signature bytes.Buffer
	positions := token.NewFileSet()
	if format.Node(&signature, positions, function.Type) != nil || signature.String() != "func(ctx context.Context, provider storage.Provider, updates []usageInterest, now time.Time) error" {
		return false
	}
	allowed := map[string]bool{
		"len": true, "make": true, "append": true, "delete": true,
		"ctx.Err": true, "now.Sub": true, "old.Opened.After": true, "old.Touched.After": true,
		"usageBridgePrefix": true, "usageRecordKey": true, "service.cacheKey": true, "readUsageManifest": true,
		"json.Marshal": true, "json.Unmarshal": true, "bytes.Equal": true,
		"service.guardedUsageCacheWrite": true, "cas.CompareAndSwapBlob": true, "provider.ReadBlob": true,
	}
	safe := true
	protected := map[string]bool{"service": true, "ctx": true, "provider": true, "updates": true, "now": true, "cas": true, "len": true, "make": true, "append": true, "delete": true, "json": true, "bytes": true, "storage": true}
	casBound, casCalled := false, false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.AssignStmt:
			for _, left := range value.Lhs {
				id, ok := left.(*ast.Ident)
				if !ok || !protected[id.Name] {
					continue
				}
				validCAS := false
				if id.Name == "cas" && value.Tok == token.DEFINE && len(value.Rhs) == 1 && !casBound {
					assertion, ok := value.Rhs[0].(*ast.TypeAssertExpr)
					if ok {
						source, sourceOK := assertion.X.(*ast.Ident)
						target, targetOK := assertion.Type.(*ast.SelectorExpr)
						if sourceOK && source.Name == "provider" && targetOK {
							pkg, pkgOK := target.X.(*ast.Ident)
							validCAS = pkgOK && pkg.Name == "storage" && target.Sel.Name == "BlobCAS"
						}
					}
				}
				if validCAS {
					casBound = true
				} else {
					safe = false
				}
			}
		case *ast.ValueSpec:
			for _, name := range value.Names {
				if protected[name.Name] {
					safe = false
				}
			}
		case *ast.RangeStmt:
			for _, expression := range []ast.Expr{value.Key, value.Value} {
				if id, ok := expression.(*ast.Ident); ok && protected[id.Name] {
					safe = false
				}
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		var target bytes.Buffer
		if format.Node(&target, positions, call.Fun) != nil || !allowed[target.String()] {
			safe = false
		}
		if target.String() == "cas.CompareAndSwapBlob" {
			casCalled = true
		}
		return true
	})
	return safe && (!casCalled || casBound)
}

func TestUsageHistoryStorageAllowanceRejectsKubernetesMutations(t *testing.T) {
	for _, test := range []struct {
		body    string
		allowed bool
	}{
		{`cas := provider.(storage.BlobCAS); _, err := cas.CompareAndSwapBlob("history", nil, nil); return err`, true},
		{`cas := provider.(storage.BlobCAS); return service.guardedUsageCacheWrite(ctx, func() error { _, err := cas.CompareAndSwapBlob("history", nil, nil); return err })`, true},
		{`return service.client.DeletePod(ctx, "shop", "api")`, false},
		{`_, err := http.NewRequest(http.MethodPost, "/pods", nil); return err`, false},
		{`return client.Put("/pods")`, false},
		{`return service.guardedUsageCacheWrite(ctx, func() error { return client.Delete("/pods") })`, false},
		{`return service.client.GetJSON(ctx, "/pods", nil)`, false},
		{`return applyWorkload(ctx)`, false},
		{`delete := service.client.Delete; delete(ctx); return nil`, false},
		{`return cas.CompareAndSwapBlob("pods", nil, nil)`, false},
	} {
		source := `package kubernetes; func(service *Service) updateUsageManifest(ctx context.Context, provider storage.Provider, updates []usageInterest, now time.Time) error {` + test.body + `}`
		parsed, err := parser.ParseFile(token.NewFileSet(), "usage_history_bridge.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		function := parsed.Decls[0].(*ast.FuncDecl)
		if got := allowedUsageHistoryStorageFunction("usage_history_bridge.go", function); got != test.allowed {
			t.Errorf("body %s allowed=%v want=%v", test.body, got, test.allowed)
		}
		if allowedUsageHistoryStorageFunction("client.go", function) || allowedUsageHistoryStorageFunction(filepath.Join("nested", "usage_history_bridge.go"), function) {
			t.Fatal("storage allowance escaped its exact file")
		}
		function.Name = ast.NewIdent("updateDeployment")
		if allowedUsageHistoryStorageFunction("usage_history_bridge.go", function) {
			t.Fatal("storage allowance escaped its exact function")
		}
	}
}

func allowedLocalSettingsAuditComparison(name string, function *ast.FuncDecl, expression *ast.BinaryExpr) bool {
	if filepath.Clean(name) != filepath.Join("..", "controllers", "kubernetes_break_logs.go") || function.Name.Name != "auditKubernetesSettings" || function.Recv != nil {
		return false
	}
	var formatted bytes.Buffer
	if format.Node(&formatted, token.NewFileSet(), expression) != nil {
		return false
	}
	switch formatted.String() {
	case `ctx.Method() == fiber.MethodPut && path == "/api/admin/kubernetes/agent-settings"`,
		`ctx.Method() == fiber.MethodPost && path == "/api/admin/kubernetes/agent-settings/test-link"`:
		return true
	default:
		return false
	}
}

func TestLocalSettingsAuditExceptionsStayNarrow(t *testing.T) {
	function := &ast.FuncDecl{Name: ast.NewIdent("auditKubernetesSettings")}
	name := filepath.Join("..", "controllers", "kubernetes_break_logs.go")
	for _, test := range []struct {
		expression string
		allowed    bool
	}{
		{`ctx.Method() == fiber.MethodPut && path == "/api/admin/kubernetes/agent-settings"`, true},
		{`ctx.Method() == fiber.MethodPost && path == "/api/admin/kubernetes/agent-settings/test-link"`, true},
		{`ctx.Method() == fiber.MethodPut && path == "/api/admin/kubernetes/pods"`, false},
		{`ctx.Method() == fiber.MethodDelete && path == "/api/admin/kubernetes/agent-settings"`, false},
		{`client.Method() == fiber.MethodPut && path == "/api/admin/kubernetes/agent-settings"`, false},
		{`ctx.Method() == fiber.MethodPut && client.Put("/pods")`, false},
		{`ctx.Method() == fiber.MethodPost && path == "/api/admin/kubernetes/agent-settings"`, false},
	} {
		expression, err := parser.ParseExpr(test.expression)
		if err != nil {
			t.Fatal(err)
		}
		binary := expression.(*ast.BinaryExpr)
		if got := allowedLocalSettingsAuditComparison(name, function, binary); got != test.allowed {
			t.Errorf("%s allowed=%v", test.expression, got)
		}
		if allowedLocalSettingsAuditComparison("client.go", function, binary) || allowedLocalSettingsAuditComparison(name, &ast.FuncDecl{Name: ast.NewIdent("readPods")}, binary) || allowedLocalSettingsAuditComparison(name, &ast.FuncDecl{Name: function.Name, Recv: &ast.FieldList{}}, binary) {
			t.Error("audit exception escaped its owning function")
		}
	}
}

func allowedLocalSettingsRoute(name string, call *ast.CallExpr) bool {
	if filepath.Clean(name) != filepath.Join("..", "controllers", "kubernetes_admin.go") || len(call.Args) != 3 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || receiver.Name != "group" {
		return false
	}
	path, ok := call.Args[0].(*ast.BasicLit)
	if !ok || path.Kind != token.STRING {
		return false
	}
	route, err := strconv.Unquote(path.Value)
	if err != nil {
		return false
	}
	handler := ""
	switch {
	case selector.Sel.Name == "Put" && route == "/agent-settings":
		handler = "saveAgentSettings"
	case selector.Sel.Name == "Put" && route == "/issue-alerts":
		handler = "saveIssueAlerts"
	case selector.Sel.Name == "Post" && route == "/agent-settings/test-link":
		handler = "testLogLink"
	default:
		return false
	}
	for position, expected := range []string{"requireSettingsWrite", handler} {
		argument, ok := call.Args[position+1].(*ast.SelectorExpr)
		if !ok || argument.Sel.Name != expected {
			return false
		}
		owner, ok := argument.X.(*ast.Ident)
		if !ok || owner.Name != "controller" {
			return false
		}
	}
	return true
}

func TestLocalSettingsRouteExceptionsStayNarrow(t *testing.T) {
	for _, test := range []struct {
		expression string
		allowed    bool
	}{
		{`group.Put("/agent-settings", controller.requireSettingsWrite, controller.saveAgentSettings)`, true},
		{`group.Put("/issue-alerts", controller.requireSettingsWrite, controller.saveIssueAlerts)`, true},
		{`group.Put("/issue-alerts", controller.saveIssueAlerts)`, false},
		{`group.Put("/issue-alerts", controller.requireInfrastructureView, controller.saveIssueAlerts)`, false},
		{`group.Put("/issue-alerts", controller.requireSettingsWrite, controller.saveAgentSettings)`, false},
		{`client.Put("/issue-alerts", controller.requireSettingsWrite, controller.saveIssueAlerts)`, false},
		{`group.Post("/agent-settings/test-link", controller.requireSettingsWrite, controller.testLogLink)`, true},
		{`group.Put("/pods", controller.requireSettingsWrite, controller.saveAgentSettings)`, false},
		{`group.Put("/agent-settings", controller.saveAgentSettings)`, false},
		{`group.Put("/agent-settings", controller.requireInfrastructureView, controller.saveAgentSettings)`, false},
		{`group.Post("/agent-settings/test-link", controller.requireSettingsWrite, controller.logs)`, false},
		{`client.Put("/agent-settings", controller.requireSettingsWrite, controller.saveAgentSettings)`, false},
	} {
		expression, err := parser.ParseExpr(test.expression)
		if err != nil {
			t.Fatal(err)
		}
		call := expression.(*ast.CallExpr)
		if got := allowedLocalSettingsRoute(filepath.Join("..", "controllers", "kubernetes_admin.go"), call); got != test.allowed {
			t.Errorf("%s allowed=%v", test.expression, got)
		}
		if allowedLocalSettingsRoute("client.go", call) {
			t.Error("exception escaped its controller")
		}
	}
}

func TestContainerDoesNotInstallCloudAuthenticationCLIs(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(content))
	for _, forbidden := range []string{"aws-cli", "azure-cli", "kubelogin", "google-cloud-cli", "gke-gcloud-auth-plugin"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("Dockerfile installs forbidden cloud authentication CLI %q", forbidden)
		}
	}
}
