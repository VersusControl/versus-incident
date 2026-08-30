package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigNormalizesPublicHostFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("public_host: https://Console.Example:443/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfigFromPath(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.PublicHost != "https://console.example" {
		t.Fatalf("PublicHost = %q, want %q", loaded.PublicHost, "https://console.example")
	}
}

func TestLoadConfigAllowsEmptyPublicHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("public_host: ''\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfigFromPath(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.PublicHost != "" {
		t.Fatalf("PublicHost = %q, want empty", loaded.PublicHost)
	}
}

func TestLoadConfigRejectsInvalidPublicHost(t *testing.T) {
	tests := []string{
		"https://user@console.example",
		"https://console.example/admin",
		"https://console.example?mode=admin",
		"https://console.example#admin",
		"ftp://console.example",
		"https://console.example:70000",
	}
	for _, publicHost := range tests {
		t.Run(publicHost, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("public_host: "+publicHost+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfigFromPath(path)
			if err == nil {
				t.Fatal("loadConfigFromPath() error = nil, want invalid public_host error")
			}
			if !strings.Contains(err.Error(), "public_host") {
				t.Fatalf("loadConfigFromPath() error = %q, want public_host context", err)
			}
		})
	}
}
