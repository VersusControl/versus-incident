package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadAgentSourcesFile asserts the agent_sources.yaml loader parses the
// top-level `sources:` LIST and, critically, expands ${VAR} references nested
// inside a list item's `options:` block — the exact path the enterprise
// `prometheus` source uses for its address. A per-key AllKeys() loop would NOT
// expand these because list items are not individually addressable string
// keys; the whole-document os.ExpandEnv approach does.
func TestLoadAgentSourcesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent_sources.yaml")
	content := `sources:
  - name: prod-metrics
    type: prometheus
    enable: true
    options:
      address: ${PROM_ADDRESS}
  - name: app-logs
    type: file
    enable: true
    file:
      path: /var/log/app.log
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write agent_sources.yaml: %v", err)
	}
	t.Setenv("PROM_ADDRESS", "http://prometheus.monitoring.svc:9090")

	got, err := loadAgentSourcesFile(path)
	if err != nil {
		t.Fatalf("loadAgentSourcesFile: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sources, want 2: %+v", len(got), got)
	}

	prom := got[0]
	if prom.Name != "prod-metrics" || prom.Type != "prometheus" || !prom.Enable {
		t.Fatalf("sources[0] = %+v", prom)
	}
	addr, ok := prom.Options["address"].(string)
	if !ok {
		t.Fatalf("sources[0].options.address not a string: %#v", prom.Options["address"])
	}
	if addr != "http://prometheus.monitoring.svc:9090" {
		t.Fatalf("sources[0].options.address = %q, want env-expanded address", addr)
	}

	logs := got[1]
	if logs.Name != "app-logs" || logs.Type != "file" || logs.File.Path != "/var/log/app.log" {
		t.Fatalf("sources[1] = %+v", logs)
	}
}

// TestLoadAgentSourcesFile_Empty asserts an absent/empty sources list parses
// to a nil slice without error.
func TestLoadAgentSourcesFile_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent_sources.yaml")
	if err := os.WriteFile(path, []byte("sources: []\n"), 0o600); err != nil {
		t.Fatalf("write agent_sources.yaml: %v", err)
	}
	got, err := loadAgentSourcesFile(path)
	if err != nil {
		t.Fatalf("loadAgentSourcesFile: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d sources, want 0: %+v", len(got), got)
	}
}

// TestLoadAgentSourcesFile_ReadError asserts a missing file surfaces a read
// error (the caller only invokes the loader after a successful os.Stat).
func TestLoadAgentSourcesFile_ReadError(t *testing.T) {
	_, err := loadAgentSourcesFile(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error reading a missing agent_sources.yaml")
	}
}
