package tools

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCatalogIsCompleteOrderedAndUnique(t *testing.T) {
	wantGroups := []Group{
		GroupVersus, GroupVersus, GroupVersus, GroupVersus, GroupVersus, GroupVersus,
		GroupVersus, GroupVersus, GroupVersus, GroupVersus, GroupVersus,
		GroupCommon, GroupCommon, GroupCommon, GroupCommon, GroupCommon, GroupCommon,
		GroupK8s, GroupK8s, GroupK8s, GroupK8s, GroupK8s,
	}
	got := Catalog()
	if len(got) != len(wantGroups) {
		t.Fatalf("Catalog() has %d tools, want %d", len(got), len(wantGroups))
	}
	seen := make(map[string]struct{}, len(got))
	for i, tool := range got {
		if tool.Group != wantGroups[i] {
			t.Errorf("Catalog()[%d].Group = %q, want %q", i, tool.Group, wantGroups[i])
		}
		if tool.Name == "" || tool.DisplayName == "" || tool.Description == "" {
			t.Errorf("Catalog()[%d] has incomplete metadata: %#v", i, tool)
		}
		if _, duplicate := seen[tool.Name]; duplicate {
			t.Errorf("duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = struct{}{}
	}
}

func TestCatalogDestinationsAreExactAndSafe(t *testing.T) {
	want := map[string][2]string{
		"get_system_overview": {docsVersus, ""}, "list_services": {docsVersus, ""}, "get_service": {docsVersus, ""},
		"get_incident": {docsVersus, ""}, "search_incidents": {docsVersus, ""}, "list_patterns": {docsVersus, ""},
		"get_pattern": {docsVersus, ""}, "list_analyses": {docsVersus, "/analyses"}, "get_alert_decision": {docsVersus, "/agent/decisions"},
		"list_capabilities": {docsVersus, ""}, "get_detection_health": {docsVersus, ""},
		"get_related_logs":      {"https://docs.versusincident.com/#/agent/data-sources", "/agent/logs"},
		"query_metrics":         {"https://docs.versusincident.com/#/agent/data-sources/prometheus", "/agent/metrics"},
		"query_traces":          {"https://docs.versusincident.com/#/agent/data-sources/traces", "/agent/traces"},
		"find_runbook":          {"https://docs.versusincident.com/#/agent/tools/find-runbook", "/agent/runbooks"},
		"recent_changes":        {"https://docs.versusincident.com/#/agent/tools/recent-changes", ""},
		"describe_dependencies": {docsTools + "?id=describe_dependencies", ""},
		"get_cluster_overview":  {docsKubernetes, ""}, "list_workloads": {docsKubernetes, ""}, "get_workload": {docsKubernetes, ""},
		"get_k8s_topology": {docsKubernetes, ""}, "list_k8s_events": {docsKubernetes, ""},
	}
	got := make(map[string][2]string, len(want))
	for _, metadata := range Catalog() {
		got[metadata.Name] = [2]string{metadata.DocsURL, metadata.UIPath}
		if safeDocsURL(metadata.DocsURL) != metadata.DocsURL || safeUIPath(metadata.UIPath) != metadata.UIPath {
			t.Errorf("%s has unsafe destinations: %#v", metadata.Name, metadata)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog destinations = %#v, want %#v", got, want)
	}
}

func TestCatalogDocumentationRoutesAreCanonical(t *testing.T) {
	want := map[string]string{
		"all":        "https://docs.versusincident.com/#/agent/tools/tools",
		"versus":     "https://docs.versusincident.com/#/agent/tools/tools?id=versus-tools",
		"kubernetes": "https://docs.versusincident.com/#/agent/tools/tools?id=kubernetes-tools",
	}
	got := map[string]string{
		"all":        docsTools,
		"versus":     docsVersus,
		"kubernetes": docsKubernetes,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog documentation routes = %#v, want %#v", got, want)
	}
}

func TestCatalogDocumentationTargetsExist(t *testing.T) {
	docsRoot := filepath.Join("..", "..", "..", "..", "src")
	sidebar, err := os.ReadFile(filepath.Join(docsRoot, "_sidebar.md"))
	if err != nil {
		t.Fatal(err)
	}
	anchorMarkers := map[string]string{
		"versus-tools":          "## Versus tools",
		"common-tools":          "## Common tools",
		"kubernetes-tools":      "## Kubernetes tools",
		"describe_dependencies": "### `describe_dependencies` :id=describe_dependencies",
	}
	checkedRoutes := make(map[string]struct{})
	for _, metadata := range Catalog() {
		destination, err := url.Parse(metadata.DocsURL)
		if err != nil {
			t.Errorf("parse %s documentation URL %q: %v", metadata.Name, metadata.DocsURL, err)
			continue
		}
		route, rawQuery, _ := strings.Cut(destination.Fragment, "?")
		relativePath := filepath.FromSlash(strings.TrimPrefix(route, "/") + ".md")
		contents, err := os.ReadFile(filepath.Join(docsRoot, relativePath))
		if err != nil {
			t.Errorf("%s documentation target %s: %v", metadata.Name, relativePath, err)
			continue
		}
		if _, checked := checkedRoutes[route]; !checked {
			if !strings.Contains(string(sidebar), "]("+route+")") {
				t.Errorf("documentation route %s is not active in _sidebar.md", route)
			}
			checkedRoutes[route] = struct{}{}
		}
		anchorValues, err := url.ParseQuery(rawQuery)
		if err != nil {
			t.Errorf("parse %s documentation anchor: %v", metadata.Name, err)
			continue
		}
		if id := anchorValues.Get("id"); id != "" {
			marker, known := anchorMarkers[id]
			if !known || !strings.Contains(string(contents), marker) {
				t.Errorf("%s documentation target %s is missing Docsify anchor %q", metadata.Name, relativePath, id)
			}
		}
	}
}

func TestCatalogDestinationValidationRejectsUnsafeValues(t *testing.T) {
	for _, value := range []string{
		"http://docs.versusincident.com/#/agent/tools/tools",
		"https://evil.example/#/agent/tools/tools",
		"https://docs.versusincident.com/agent/tools/tools",
		"https://docs.versusincident.com:443/#/agent/tools/tools",
		"https://docs.versusincident.com/#/%2f%2fevil.example",
		"https://docs.versusincident.com/#/agent%0Atools",
		"https://docs.versusincident.com/#/agent%5ctools",
	} {
		if got := safeDocsURL(value); got != "" {
			t.Errorf("safeDocsURL(%q) = %q, want empty", value, got)
		}
	}
	for _, value := range []string{"//evil.example/path", "/\\evil", "/%5cevil", "/%2f%2fevil", "/path%0Aname", "/path?next=//evil", "https://evil.example/path"} {
		if got := safeUIPath(value); got != "" {
			t.Errorf("safeUIPath(%q) = %q, want empty", value, got)
		}
	}
}

func TestCatalogDeclaresCompoundRunbookCapabilities(t *testing.T) {
	for _, tool := range Catalog() {
		if tool.Name != "find_runbook" {
			continue
		}
		if tool.Requirement.Kind != RequirementCapability || len(tool.Requirement.Capabilities) != 2 {
			t.Fatalf("find_runbook requirement = %#v, want two capabilities", tool.Requirement)
		}
		return
	}
	t.Fatal("find_runbook missing from catalog")
}

func TestCatalogCopyIsDetached(t *testing.T) {
	got := Catalog()
	got[0].Name = "changed"
	got[14].Requirement.Capabilities[0] = "changed"
	if Catalog()[0].Name == "changed" {
		t.Fatal("Catalog returned mutable backing storage")
	}
	if Catalog()[14].Requirement.Capabilities[0] == "changed" {
		t.Fatal("Catalog returned mutable requirement capabilities")
	}
}
