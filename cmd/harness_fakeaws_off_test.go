//go:build !harness_fakeaws

package main

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	awscost "github.com/VersusControl/versus-incident/pkg/aws/cost"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestReleaseBuildCannotRedirectAWS: without the harness_fakeaws tag the
// redirect does not exist, the variable does nothing (the connector and the
// cost refresh both keep the hardened default transport), and the only file
// that names it is excluded from the default build.
func TestReleaseBuildCannotRedirectAWS(t *testing.T) {
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
	for _, name := range append(append([]string{}, pkg.GoFiles...), pkg.CgoFiles...) {
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "VERSUS_HARNESS_FAKEAWS_URL") || strings.Contains(string(content), "harnessRedirect") {
			t.Fatalf("%s is in the release build and carries the AWS redirect", name)
		}
	}
	ignored := strings.Join(pkg.IgnoredGoFiles, ",")
	if !strings.Contains(ignored, "harness_fakeaws.go") {
		t.Fatalf("redirect file not excluded from the release build: ignored=%s", ignored)
	}
}
