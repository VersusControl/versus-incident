package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// K34 H1: the chart's version and appVersion follow the release, and the
// image tag defaults to "v<appVersion>" so a default install and the one-file
// manifest run the image of the release they came from. The release workflow
// checks the tag against Chart.yaml; these tests keep the chart, its default
// image and the committed manifest in step before a tag exists.

var (
	chartVersionLine    = regexp.MustCompile(`(?m)^version:\s*"?([^"\s]+)"?\s*$`)
	chartAppVersionLine = regexp.MustCompile(`(?m)^appVersion:\s*"?([^"\s]+)"?\s*$`)
	renderedImageLine   = regexp.MustCompile(`(?m)^\s*image:\s*"?(ghcr\.io/versuscontrol/versus-incident:[^"\s]+)"?\s*$`)
)

func chartVersions(t *testing.T, chartYAML []byte) (version, appVersion string) {
	t.Helper()
	versionMatch := chartVersionLine.FindSubmatch(chartYAML)
	appVersionMatch := chartAppVersionLine.FindSubmatch(chartYAML)
	if versionMatch == nil || appVersionMatch == nil {
		t.Fatalf("Chart.yaml has no version or appVersion line:\n%s", chartYAML)
	}
	return string(versionMatch[1]), string(appVersionMatch[1])
}

// renderedImages returns every versus-incident image the render uses.
func renderedImages(rendered []byte) []string {
	var images []string
	for _, match := range renderedImageLine.FindAllSubmatch(rendered, -1) {
		images = append(images, string(match[1]))
	}
	return images
}

func TestHelmChartVersionFollowsAppVersion(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	version, appVersion := chartVersions(t, raw)
	if version != appVersion {
		t.Fatalf("Chart.yaml version %q and appVersion %q drift; a release tag v%s cannot match both", version, appVersion, appVersion)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(appVersion) {
		t.Fatalf("appVersion %q is not a release version", appVersion)
	}
}

// The default image tag must come from appVersion, not from a pinned tag or
// from the chart version: the copied chart gets an appVersion that differs
// from both, and the render must follow it.
func TestHelmDefaultImageTagFollowsAppVersion(t *testing.T) {
	requireRenderableChart(t)
	chart := copyChart(t)
	chartFile := filepath.Join(chart, "Chart.yaml")
	raw, err := os.ReadFile(chartFile)
	if err != nil {
		t.Fatal(err)
	}
	patched := chartAppVersionLine.ReplaceAll(raw, []byte(`appVersion: "97.98.99"`))
	if err := os.WriteFile(chartFile, patched, 0o644); err != nil {
		t.Fatal(err)
	}
	images := renderedImages(helmTemplate(t, chart))
	if len(images) == 0 {
		t.Fatal("the default render has no versus-incident image")
	}
	for _, image := range images {
		if image != "ghcr.io/versuscontrol/versus-incident:v97.98.99" {
			t.Errorf("default image %q does not follow appVersion 97.98.99", image)
		}
	}
	explicit := renderedImages(helmTemplate(t, chart, "--set", "image.tag=dev"))
	if len(explicit) == 0 || explicit[0] != "ghcr.io/versuscontrol/versus-incident:dev" {
		t.Errorf("an explicit image.tag must be used verbatim, got %v", explicit)
	}
}

// The committed kubectl manifest pins the image of the chart's appVersion.
func TestKubectlManifestPinsTheReleaseImage(t *testing.T) {
	chartYAML, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, appVersion := chartVersions(t, chartYAML)
	manifest, err := os.ReadFile("../../examples/kubernetes/k8s-connector-in-cluster-enabled.yaml")
	if err != nil {
		t.Fatal(err)
	}
	images := renderedImages(manifest)
	if len(images) == 0 {
		t.Fatal("the manifest has no versus-incident image")
	}
	want := "ghcr.io/versuscontrol/versus-incident:v" + appVersion
	for _, image := range images {
		if image != want {
			t.Errorf("manifest image %q, want %q", image, want)
		}
	}
}
