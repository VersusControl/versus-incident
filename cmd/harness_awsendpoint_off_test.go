//go:build !harness_awsendpoint && !harness_fakeaws

package main

import (
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	awscost "github.com/VersusControl/versus-incident/pkg/aws/cost"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// harnessRedirectMarkers are the variable names and symbols only a test
// build may carry.
var harnessRedirectMarkers = []string{
	"VERSUS_HARNESS_AWS_ENDPOINT_URL", "VERSUS_HARNESS_AWS_CA_BUNDLE", "VERSUS_HARNESS_FAKEAWS_URL",
	"harnessRedirect", "newHarnessAWSClient", "harnessDialControl", "loadHarnessCABundle",
}

// TestReleaseBuildCannotRedirectAWS: without a test build tag the redirect
// package compiles no redirect, the variables do nothing (the connector and
// the cost refresh both keep the hardened default transport), and no Go file
// of the release build anywhere in the module names a variable or redirect
// symbol.
func TestReleaseBuildCannotRedirectAWS(t *testing.T) {
	t.Setenv("VERSUS_HARNESS_AWS_ENDPOINT_URL", "https://127.0.0.1:1")
	t.Setenv("VERSUS_HARNESS_AWS_CA_BUNDLE", filepath.Join(t.TempDir(), "ca.pem"))
	t.Setenv("VERSUS_HARNESS_FAKEAWS_URL", "http://127.0.0.1:1")
	cfg := &c.Config{}
	cfg.Connectors.AWS.Budgets.MaxRequestsPerSecond = 2
	connector := mountAWS(fiber.New(), storage.NewMemory(), cfg)
	if connector.HTTPClient != nil {
		t.Fatal("release build handed the AWS connector a transport override")
	}
	conn := c.AWSConnectorConfig{Enable: true}
	conn.OptionalData.CostExplorer = true
	if opts := connector.CostOptions(conn, []awscost.Target{{AccountID: "111122223333"}}, nil); opts.HTTPClient != nil {
		t.Fatal("release build overrides the cost adapter's hardened client")
	}

	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(directory)
	redirect, err := build.Default.ImportDir(filepath.Join(root, "pkg", "aws", "harnessendpoint"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(redirect.IgnoredGoFiles, "endpoint.go") {
		t.Fatalf("redirect file not excluded from the release build: ignored=%v", redirect.IgnoredGoFiles)
	}

	scanned := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		match, err := build.Default.MatchFile(filepath.Dir(path), name)
		if err != nil || !match {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, marker := range harnessRedirectMarkers {
			if strings.Contains(string(content), marker) {
				t.Errorf("%s is in the release build and names %s", path, marker)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d release Go files under %s", scanned, root)
	}
}
