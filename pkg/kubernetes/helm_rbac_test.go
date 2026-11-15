package kubernetes

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestKubernetesReaderRBACTemplateIsReadOnly(t *testing.T) {
	template, err := os.ReadFile("../../helm/versus-incident/templates/kubernetes-reader-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	content := string(template)
	verbs := quotedTemplateFieldValues(t, content, "verbs")
	resources := quotedTemplateFieldValues(t, content, "resources")
	discoveryURLs := quotedTemplateFieldValues(t, content, "nonResourceURLs")
	if len(discoveryURLs) != 2 || !containsString(discoveryURLs, "/.well-known/openid-configuration") || !containsString(discoveryURLs, "/openid/v1/jwks") || !regexp.MustCompile(`(?m)nonResourceURLs: \["/\.well-known/openid-configuration", "/openid/v1/jwks"\]\s*\n\s*verbs: \["get"\]`).MatchString(content) {
		t.Error("projected service account discovery must have only fixed get-only URLs")
	}

	for _, forbidden := range []string{"create", "update", "patch", "delete", "watch"} {
		if containsString(verbs, forbidden) {
			t.Errorf("Kubernetes reader RBAC contains forbidden verb %q", forbidden)
		}
	}
	if containsString(resources, "endpoints") {
		t.Error(`Kubernetes reader RBAC must not read "endpoints"; EndpointSlices replace them`)
	}
	for _, resource := range resources {
		if resource == "pods/log" {
			continue
		}
		if strings.Contains(resource, "/") {
			t.Errorf("Kubernetes reader RBAC grants subresource %q; the role is metadata-only", resource)
		}
	}
	if !containsString(verbs, "get") || !containsString(verbs, "list") {
		t.Errorf("Kubernetes reader RBAC must preserve get/list access: %v", verbs)
	}
	if !regexp.MustCompile(`(?m)resources: \["nodes", "namespaces", "pods", "services", "events", "configmaps", "serviceaccounts", "persistentvolumeclaims", "persistentvolumes"\]\s*\n\s*verbs: \["get", "list"\]`).MatchString(content) {
		t.Error("core metadata reads missing or not read-only")
	}
	if !regexp.MustCompile(`(?s)\{\{- if \.Values\.kubernetesReaderRBAC\.secrets \}\}\s*- apiGroups: \[""\]\s*resources: \["secrets"\]\s*verbs: \["list"\]\s*\{\{- end \}\}`).MatchString(content) {
		t.Error("Secret access must be list-only and granted only behind kubernetesReaderRBAC.secrets")
	}
	if !regexp.MustCompile(`(?m)resources: \["deployments", "statefulsets", "daemonsets", "replicasets", "controllerrevisions"\]\s*\n\s*verbs: \["get", "list"\]`).MatchString(content) {
		t.Error("controller revision reads missing or not read-only")
	}
	if !regexp.MustCompile(`(?m)resources: \["pods/log"\]\s*\n\s*verbs: \["get"\]`).MatchString(content) {
		t.Error("Kubernetes reader RBAC must preserve get-only access to pods/log")
	}
	optional := regexp.MustCompile(`(?s)\{\{- if \.Values\.kubernetesReaderRBAC\.optionalKinds \}\}(.*?)\{\{- end \}\}`).FindStringSubmatch(content)
	if optional == nil {
		t.Fatal("optional kinds must be gated by kubernetesReaderRBAC.optionalKinds")
	}
	for _, rule := range []string{
		"apiGroups: [\"gateway.networking.k8s.io\"]\n    resources: [\"gateways\", \"httproutes\"]\n    verbs: [\"get\", \"list\"]",
		"apiGroups: [\"argoproj.io\"]\n    resources: [\"applications\", \"rollouts\"]\n    verbs: [\"get\", \"list\"]",
		"apiGroups: [\"kustomize.toolkit.fluxcd.io\"]\n    resources: [\"kustomizations\"]\n    verbs: [\"get\", \"list\"]",
		"apiGroups: [\"helm.toolkit.fluxcd.io\"]\n    resources: [\"helmreleases\"]\n    verbs: [\"get\", \"list\"]",
	} {
		if !strings.Contains(optional[1], rule) {
			t.Errorf("optional kinds block lacks read-only rule %q", rule)
		}
	}
}

func TestKubernetesActorRBACTemplateUsesExplicitNonDestructiveVerbs(t *testing.T) {
	template, err := os.ReadFile("../../helm/versus-incident/templates/actor-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	content := string(template)
	verbs := quotedTemplateFieldValues(t, content, "verbs")
	resources := quotedTemplateFieldValues(t, content, "resources")
	seenVerbs := map[string]bool{}
	for _, verb := range verbs {
		if verb != "get" && verb != "patch" && verb != "create" {
			t.Errorf("Kubernetes actor RBAC contains forbidden verb %q", verb)
		}
		seenVerbs[verb] = true
	}
	if len(seenVerbs) != 3 {
		t.Fatalf("actor verbs = %v", seenVerbs)
	}
	allowedResources := map[string]bool{
		"deployments": true, "deployments/scale": true, "statefulsets": true,
		"statefulsets/scale": true, "daemonsets": true, "cronjobs": true, "jobs": true, "nodes": true,
	}
	for _, resource := range resources {
		if !allowedResources[resource] {
			t.Errorf("Kubernetes actor RBAC contains unapproved resource %q", resource)
		}
		delete(allowedResources, resource)
	}
	if len(allowedResources) != 0 {
		t.Errorf("Kubernetes actor RBAC is missing scoped resources: %v", allowedResources)
	}
	if !regexp.MustCompile(`(?m)resources: \["jobs"\]\s*\n\s*verbs: \["get", "create"\]`).MatchString(content) {
		t.Fatal("create must be limited to Jobs for CronJob triggers")
	}
}

func quotedTemplateFieldValues(t *testing.T, content, field string) []string {
	t.Helper()
	fieldPattern := regexp.MustCompile(`(?m)^\s*(?:-\s*)?` + regexp.QuoteMeta(field) + `:\s*\[([^]]*)\]`)
	valuePattern := regexp.MustCompile(`"([^"]+)"`)
	var values []string
	for _, fieldMatch := range fieldPattern.FindAllStringSubmatch(content, -1) {
		for _, valueMatch := range valuePattern.FindAllStringSubmatch(fieldMatch[1], -1) {
			values = append(values, valueMatch[1])
		}
	}
	if len(values) == 0 {
		t.Fatalf("no %s entries found in Kubernetes reader RBAC template", field)
	}
	return values
}

// Every metadata-only kind the inventory reads must be granted by the shipped
// reader role, or a default install reports it unreadable forever. Secrets are
// the exception: listing them exposes Secret values to the connector identity,
// so the default role leaves them out and the grant is an explicit opt-in.
func TestKubernetesReaderRBACGrantsEveryMetadataKind(t *testing.T) {
	template, err := os.ReadFile("../../helm/versus-incident/templates/kubernetes-reader-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	withoutOptIn := regexp.MustCompile(`(?s)\{\{- if \.Values\.kubernetesReaderRBAC\.secrets \}\}.*?\{\{- end \}\}`).ReplaceAllString(string(template), "")
	resources := quotedTemplateFieldValues(t, withoutOptIn, "resources")
	granted := map[string]bool{}
	for _, resource := range resources {
		granted[resource] = true
	}
	for kind := range metadataOnlyIndexKinds {
		resource := strings.ToLower(kind) + "s"
		if kind == "Secret" {
			if granted[resource] {
				t.Fatal(`the default reader role must not grant "secrets"; it is opt-in through kubernetesReaderRBAC.secrets`)
			}
			continue
		}
		if !granted[resource] {
			t.Fatalf("metadata-only kind %s needs %q get/list in the shipped reader role", kind, resource)
		}
	}
}
