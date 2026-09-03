package kubernetes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
		lower := strings.ToLower(string(content))
		for _, forbidden := range []string{"\"os/exec\"", "exec.command", "pkg/common", "k8s.io/client-go", "helm.sh/helm", "/exec", "/proxy", "/helm"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("%s contains forbidden read-only surface %q", name, forbidden)
			}
		}
		credentialAuthFile := strings.HasSuffix(name, "_auth.go") || strings.HasSuffix(name, "auth.go") || strings.HasSuffix(name, "gke_native.go")
		if !credentialAuthFile {
			for _, forbidden := range []string{"methodpost", "methodput", "methodpatch", "methoddelete", "methodconnect", ".post(", ".put(", ".patch(", ".delete("} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("%s contains forbidden read-only surface %q", name, forbidden)
				}
			}
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, content, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			method := strings.ToLower(function.Name.Name)
			for _, prefix := range []string{"create", "update", "delete", "patch", "apply", "mutate", "proxy", "installhelm"} {
				if strings.HasPrefix(method, prefix) {
					t.Errorf("%s declares mutation method %s", name, function.Name.Name)
				}
			}
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
