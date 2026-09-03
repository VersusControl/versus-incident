package kubernetes

import (
	"os"
	"strings"
	"testing"
)

func TestHelmReaderRBACCoversTopologyResources(t *testing.T) {
	template, err := os.ReadFile("../../helm/versus-incident/templates/kubernetes-reader-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body := string(template)
	for _, resourceID := range topologyResourceIDs {
		parts := strings.Split(resourceID, "~")
		resource := parts[len(parts)-1]
		if resource == "secrets" || strings.HasPrefix(resourceID, "gateway.networking.k8s.io~") {
			continue
		}
		if !strings.Contains(body, `"`+resource+`"`) {
			t.Errorf("topology resource %q is missing from chart RBAC", resourceID)
		}
	}
	for _, resource := range []string{"networkpolicies", "customresourcedefinitions"} {
		if !strings.Contains(body, `"`+resource+`"`) {
			t.Errorf("discovery resource %q is missing from chart RBAC", resource)
		}
	}
	for _, forbidden := range []string{`"endpoints"`, `"secrets"`, `"create"`, `"update"`, `"patch"`, `"delete"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("chart RBAC contains forbidden default %s", forbidden)
		}
	}
	if !strings.Contains(body, ".Values.kubernetesReaderRBAC.gatewayAPI") {
		t.Fatal("Gateway API RBAC is not separately guarded")
	}
}

func TestKubernetesDocumentationHasUnindentedProseAndHeadings(t *testing.T) {
	document, err := os.ReadFile("../../src/agent/tools/kubernetes.md")
	if err != nil {
		t.Fatal(err)
	}
	inFence := false
	for lineNumber, line := range strings.Split(string(document), "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		indentedHeading := trimmed != line && strings.HasPrefix(trimmed, "#")
		if !inFence && (indentedHeading || strings.HasPrefix(line, "    ") && strings.TrimSpace(line) != "") {
			t.Errorf("line %d is indented outside a code fence: %q", lineNumber+1, line)
		}
	}
}
