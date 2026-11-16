//go:build !harness_awsendpoint && !harness_fakeaws

package main

import (
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
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
// does not exist, the variables do nothing (the connector and the cost
// refresh both keep the hardened default transport), and no Go file of the
// release build anywhere in the module names a variable or redirect symbol.
func TestReleaseBuildCannotRedirectAWS(t *testing.T) {
	t.Setenv("VERSUS_HARNESS_AWS_ENDPOINT_URL", "https://127.0.0.1:1")
	t.Setenv("VERSUS_HARNESS_AWS_CA_BUNDLE", filepath.Join(t.TempDir(), "ca.pem"))
	t.Setenv("VERSUS_HARNESS_FAKEAWS_URL", "http://127.0.0.1:1")
	if client := awsHarnessHTTPClient(); client != nil {
		t.Fatal("release build returned a redirecting AWS client")
	}
	cfg := &c.Config{}
	cfg.Connectors.AWS.Budgets.MaxRequestsPerSecond = 2
	connector := registerAWSSettingsController(fiber.New(), storage.NewMemory(), cfg)
	if connector.httpClient != nil {
		t.Fatal("release build handed the AWS connector a transport override")
	}
	conn := c.AWSConnectorConfig{Enable: true}
	conn.OptionalData.CostExplorer = true
	if opts := awsCostOptions(conn, []awscost.Target{{AccountID: "111122223333"}}, nil, connector); opts.HTTPClient != nil {
		t.Fatal("release build overrides the cost adapter's hardened client")
	}

	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := build.Default.ImportDir(directory, 0)
	if err != nil {
		t.Fatal(err)
	}
	ignored := strings.Join(pkg.IgnoredGoFiles, ",")
	if !strings.Contains(ignored, "harness_awsendpoint.go") {
		t.Fatalf("redirect file not excluded from the release build: ignored=%s", ignored)
	}

	root := filepath.Dir(directory)
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
