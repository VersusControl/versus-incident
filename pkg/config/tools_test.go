package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLoadToolsFile asserts the tools.yaml loader parses the root-level
// tool-loop knobs and the recent_changes git repos, expands ${VAR}
// references, and round-trips the top-level `tools` key.
func TestLoadToolsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	content := `tools:
  tool_timeout: 30s
  parallel_tools: true
  recent_changes:
    git:
      auth:
        token: ${TOOLS_TEST_TOKEN}
        ssh_key_path: /etc/versus/global_key
      repos:
        - url: ${TOOLS_TEST_REPO}
          branch: main
          service: api
        - url: git@github.com:acme/web.git
          auth:
            ssh_key_path: /etc/versus/web_key
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write tools.yaml: %v", err)
	}
	t.Setenv("TOOLS_TEST_REPO", "https://github.com/acme/api.git")
	t.Setenv("TOOLS_TEST_TOKEN", "secret-token")

	got, err := loadToolsFile(path)
	if err != nil {
		t.Fatalf("loadToolsFile: %v", err)
	}
	if got.ToolTimeout != "30s" {
		t.Fatalf("tool_timeout = %q, want 30s", got.ToolTimeout)
	}
	if !got.ParallelTools {
		t.Fatalf("parallel_tools = %v, want true", got.ParallelTools)
	}
	auth := got.RecentChanges.Git.Auth
	if auth.Token != "secret-token" {
		t.Fatalf("global auth.token = %q, want env-expanded secret-token", auth.Token)
	}
	if auth.SSHKeyPath != "/etc/versus/global_key" {
		t.Fatalf("global auth.ssh_key_path = %q", auth.SSHKeyPath)
	}
	repos := got.RecentChanges.Git.Repos
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2: %+v", len(repos), repos)
	}
	if repos[0].URL != "https://github.com/acme/api.git" {
		t.Fatalf("repos[0].url = %q, want env-expanded api URL", repos[0].URL)
	}
	if repos[0].Branch != "main" || repos[0].Service != "api" {
		t.Fatalf("repos[0] = %+v", repos[0])
	}
	if repos[1].URL != "git@github.com:acme/web.git" {
		t.Fatalf("repos[1].url = %q", repos[1].URL)
	}
	if repos[1].Auth.SSHKeyPath != "/etc/versus/web_key" {
		t.Fatalf("repos[1].auth.ssh_key_path = %q, want per-repo override", repos[1].Auth.SSHKeyPath)
	}
}

// TestLoadToolsFile_DescribeDependencies asserts the tools.yaml loader
// parses the describe_dependencies service-dependency graph.
func TestLoadToolsFile_DescribeDependencies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	content := `tools:
  describe_dependencies:
    services:
      - name: web
        depends_on:
          - api
      - name: api
        depends_on:
          - database
          - cache
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write tools.yaml: %v", err)
	}
	got, err := loadToolsFile(path)
	if err != nil {
		t.Fatalf("loadToolsFile: %v", err)
	}
	svcs := got.DescribeDependencies.Services
	if len(svcs) != 2 {
		t.Fatalf("got %d services, want 2: %+v", len(svcs), svcs)
	}
	if svcs[0].Name != "web" || len(svcs[0].DependsOn) != 1 || svcs[0].DependsOn[0] != "api" {
		t.Fatalf("services[0] = %+v", svcs[0])
	}
	if svcs[1].Name != "api" || len(svcs[1].DependsOn) != 2 {
		t.Fatalf("services[1] = %+v", svcs[1])
	}
}

// TestLoadToolsFile_Empty asserts an empty/absent tools block parses to a
// zero ToolsConfig without error.
func TestLoadToolsFile_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	if err := os.WriteFile(path, []byte("tools: {}\n"), 0o600); err != nil {
		t.Fatalf("write tools.yaml: %v", err)
	}
	got, err := loadToolsFile(path)
	if err != nil {
		t.Fatalf("loadToolsFile: %v", err)
	}
	if !reflect.DeepEqual(got, ToolsConfig{}) {
		t.Fatalf("got = %+v, want zero ToolsConfig", got)
	}
}

// TestLoadToolsFile_ReadError asserts a missing file surfaces a read
// error (the caller only invokes the loader after a successful os.Stat).
func TestLoadToolsFile_ReadError(t *testing.T) {
	_, err := loadToolsFile(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error reading a missing tools.yaml")
	}
}
