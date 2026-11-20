//go:build !harness_awsendpoint && !harness_fakeaws

package harnessendpoint

import (
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestReleaseBuildHasNoRedirect: without a test build tag the redirect file
// is not compiled and the endpoint variables do nothing, so every binary that
// passes HTTPClient() keeps the hardened default AWS transport.
func TestReleaseBuildHasNoRedirect(t *testing.T) {
	t.Setenv("VERSUS_HARNESS_AWS_ENDPOINT_URL", "https://127.0.0.1:1")
	t.Setenv("VERSUS_HARNESS_AWS_CA_BUNDLE", filepath.Join(t.TempDir(), "ca.pem"))
	t.Setenv("VERSUS_HARNESS_FAKEAWS_URL", "http://127.0.0.1:1")
	if client := HTTPClient(); client != nil {
		t.Fatal("release build returned a redirecting AWS client")
	}
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := build.Default.ImportDir(directory, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pkg.IgnoredGoFiles, "endpoint.go") || slices.Contains(pkg.GoFiles, "endpoint.go") {
		t.Fatalf("redirect file not excluded from the release build: go=%v ignored=%v", pkg.GoFiles, pkg.IgnoredGoFiles)
	}
}
