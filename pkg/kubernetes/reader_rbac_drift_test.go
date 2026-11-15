package kubernetes

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The reader role is shipped in four places: the Helm chart, the one-file
// kubectl manifest, the RBAC example in the Kubernetes configuration guide and
// the EKS example. These tests keep every copy granting exactly what the
// connector reads, so a default install never shows a kind as unreadable
// because a grant was forgotten in one place.

const (
	readerChartDir      = "../../helm/versus-incident"
	readerManifestPath  = "../../examples/kubernetes/k8s-connector-in-cluster-enabled.yaml"
	readerDocsRolePath  = "../../src/agent/kubernetes/configuration.md"
	readerEKSRolePath   = "../../src/examples/eks-irsa-kubernetes-reader.md"
	readerChartRoleName = "-kubernetes-reader"
)

// readerGrant is one expanded RBAC permission: a verb on a group/resource, or a
// verb on a non-resource URL (group and resource empty, url set).
type readerGrant struct {
	group, resource, url, verb string
}

func (grant readerGrant) String() string {
	if grant.url != "" {
		return grant.url + " " + grant.verb
	}
	group := grant.group
	if group == "" {
		group = "core"
	}
	return group + "/" + grant.resource + " " + grant.verb
}

// readerExtraReads lists what the connector reads beyond the indexed kinds in
// indexedResourceIDs: kinds found through discovery that the views name
// directly, the GitOps and rollout CRDs, metrics, pod logs and the
// ServiceAccount token issuer discovery used to verify projected tokens.
var readerExtraReads = []readerGrant{
	{resource: "pods/log", verb: "get"},
	{resource: "persistentvolumes", verb: "get"},
	{resource: "persistentvolumes", verb: "list"},
	{group: "discovery.k8s.io", resource: "endpointslices", verb: "get"},
	{group: "discovery.k8s.io", resource: "endpointslices", verb: "list"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "get"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "list"},
	{group: "policy", resource: "poddisruptionbudgets", verb: "get"},
	{group: "policy", resource: "poddisruptionbudgets", verb: "list"},
	{group: "storage.k8s.io", resource: "storageclasses", verb: "get"},
	{group: "storage.k8s.io", resource: "storageclasses", verb: "list"},
	{group: "apiextensions.k8s.io", resource: "customresourcedefinitions", verb: "get"},
	{group: "apiextensions.k8s.io", resource: "customresourcedefinitions", verb: "list"},
	{group: "metrics.k8s.io", resource: "nodes", verb: "get"},
	{group: "metrics.k8s.io", resource: "nodes", verb: "list"},
	{group: "metrics.k8s.io", resource: "pods", verb: "get"},
	{group: "metrics.k8s.io", resource: "pods", verb: "list"},
	{group: "argoproj.io", resource: "applications", verb: "get"},
	{group: "argoproj.io", resource: "applications", verb: "list"},
	{group: "argoproj.io", resource: "rollouts", verb: "get"},
	{group: "argoproj.io", resource: "rollouts", verb: "list"},
	{group: "kustomize.toolkit.fluxcd.io", resource: "kustomizations", verb: "get"},
	{group: "kustomize.toolkit.fluxcd.io", resource: "kustomizations", verb: "list"},
	{group: "helm.toolkit.fluxcd.io", resource: "helmreleases", verb: "get"},
	{group: "helm.toolkit.fluxcd.io", resource: "helmreleases", verb: "list"},
	{url: "/.well-known/openid-configuration", verb: "get"},
	{url: "/openid/v1/jwks", verb: "get"},
}

// readerOptionalReads are reads the connector makes that the default role
// deliberately leaves out. Each needs a reason; the default role must not
// grant them, and the product must say plainly what is lost without them.
var readerOptionalReads = map[string]string{
	"core/secrets": "Helm releases; Secret values exposure",
}

// requiredReaderGrants is every read the connector code makes, derived from
// the indexed resource IDs plus readerExtraReads.
func requiredReaderGrants(t *testing.T) map[readerGrant]bool {
	t.Helper()
	required := map[readerGrant]bool{}
	for kind, id := range indexedResourceIDs {
		parts := strings.Split(id, "~")
		if len(parts) != 3 {
			t.Fatalf("indexed kind %s has malformed resource ID %q", kind, id)
		}
		group := parts[0]
		if group == "core" {
			group = ""
		}
		for _, verb := range []string{"get", "list"} {
			required[readerGrant{group: group, resource: parts[2], verb: verb}] = true
		}
	}
	for _, grant := range readerExtraReads {
		required[grant] = true
	}
	return required
}

func readerGrantOptional(grant readerGrant) (string, bool) {
	group := grant.group
	if group == "" {
		group = "core"
	}
	reason, ok := readerOptionalReads[group+"/"+grant.resource]
	return reason, ok
}

type readerRBACRule struct {
	APIGroups       []string `yaml:"apiGroups"`
	Resources       []string `yaml:"resources"`
	NonResourceURLs []string `yaml:"nonResourceURLs"`
	Verbs           []string `yaml:"verbs"`
}

type readerRBACObject struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules []readerRBACRule `yaml:"rules"`
}

// readerClusterRoleGrants decodes a multi-document YAML stream and returns
// the expanded grants of the single ClusterRole whose name matches.
func readerClusterRoleGrants(t *testing.T, source string, content []byte, nameMatches func(string) bool) map[readerGrant]bool {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	var roles []readerRBACObject
	for {
		var object readerRBACObject
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: invalid YAML: %v", source, err)
		}
		if object.Kind == "ClusterRole" && nameMatches(object.Metadata.Name) {
			roles = append(roles, object)
		}
	}
	if len(roles) != 1 {
		t.Fatalf("%s: want exactly one reader ClusterRole, found %d", source, len(roles))
	}
	grants := map[readerGrant]bool{}
	for _, rule := range roles[0].Rules {
		for _, verb := range rule.Verbs {
			for _, url := range rule.NonResourceURLs {
				grants[readerGrant{url: url, verb: verb}] = true
			}
			for _, group := range rule.APIGroups {
				for _, resource := range rule.Resources {
					grants[readerGrant{group: group, resource: resource, verb: verb}] = true
				}
			}
		}
	}
	return grants
}

// readerMarkdownRole extracts the reader ClusterRole from the first fenced
// YAML block that defines one, after the given heading when it is set.
func readerMarkdownRole(t *testing.T, path, heading string) map[readerGrant]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if heading != "" {
		index := strings.Index(content, heading)
		if index < 0 {
			t.Fatalf("%s: heading %q not found", path, heading)
		}
		content = content[index:]
	}
	blocks := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(content, -1)
	for _, block := range blocks {
		if strings.Contains(block[1], "kind: ClusterRole\n") {
			return readerClusterRoleGrants(t, path, []byte(block[1]), func(string) bool { return true })
		}
	}
	t.Fatalf("%s: no YAML block defines a ClusterRole", path)
	return nil
}

// renderDefaultReaderChart renders the chart with the Kubernetes connector on
// (connectors.kubernetes.enabled=true) and otherwise default values, which is
// when the chart creates the reader role. It skips when Helm or the chart
// dependencies are not available locally.
func renderDefaultReaderChart(t *testing.T, args ...string) []byte {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed; the committed manifest check covers the default role")
	}
	if entries, _ := os.ReadDir(filepath.Join(readerChartDir, "charts")); len(entries) == 0 {
		t.Skip("chart dependencies are not built; run `helm dependency build helm/versus-incident`")
	}
	base := []string{"template", "versus-incident", readerChartDir, "--namespace", "versus", "--set", "connectors.kubernetes.enabled=true"}
	command := exec.Command(helm, append(base, args...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("helm template with the connector on failed: %v\n%s", err, stderr.String())
	}
	return output
}

func sortedReaderGrants(grants map[readerGrant]bool) []string {
	result := make([]string, 0, len(grants))
	for grant := range grants {
		result = append(result, grant.String())
	}
	sort.Strings(result)
	return result
}

func assertReaderRoleCoversCode(t *testing.T, source string, granted map[readerGrant]bool) {
	t.Helper()
	missing := map[readerGrant]bool{}
	for grant := range requiredReaderGrants(t) {
		if granted[grant] {
			continue
		}
		if _, optional := readerGrantOptional(grant); optional {
			continue
		}
		missing[grant] = true
	}
	if len(missing) > 0 {
		t.Errorf("%s: the reader role lacks reads the connector makes: %v", source, sortedReaderGrants(missing))
	}
	for grant := range granted {
		if reason, optional := readerGrantOptional(grant); optional {
			t.Errorf("%s: the default reader role grants %s, which is optional (%s)", source, grant, reason)
		}
		if grant.verb != "get" && grant.verb != "list" {
			t.Errorf("%s: the reader role grants non-read verb %s", source, grant)
		}
	}
}

func assertReaderRolesMatch(t *testing.T, wantSource string, want map[readerGrant]bool, gotSource string, got map[readerGrant]bool) {
	t.Helper()
	extra, missing := map[readerGrant]bool{}, map[readerGrant]bool{}
	for grant := range got {
		if !want[grant] {
			extra[grant] = true
		}
	}
	for grant := range want {
		if !got[grant] {
			missing[grant] = true
		}
	}
	if len(extra) > 0 || len(missing) > 0 {
		t.Errorf("%s differs from %s: missing %v, extra %v", gotSource, wantSource, sortedReaderGrants(missing), sortedReaderGrants(extra))
	}
}

func readerManifestRole(t *testing.T) map[readerGrant]bool {
	t.Helper()
	raw, err := os.ReadFile(readerManifestPath)
	if err != nil {
		t.Fatalf("the one-file kubectl manifest is missing: %v", err)
	}
	return readerClusterRoleGrants(t, readerManifestPath, raw, func(name string) bool { return strings.HasSuffix(name, readerChartRoleName) })
}

func TestReaderRoleOptionalReadsHaveReasons(t *testing.T) {
	required := requiredReaderGrants(t)
	for key, reason := range readerOptionalReads {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("optional read %s needs a reason", key)
		}
		found := false
		for grant := range required {
			if _, optional := readerGrantOptional(grant); optional && strings.HasSuffix(key, "/"+grant.resource) {
				found = true
			}
		}
		if !found {
			t.Errorf("optional read %s is not read by the connector; remove it from the list", key)
		}
	}
}

func TestDefaultChartReaderRoleCoversConnectorReads(t *testing.T) {
	rendered := renderDefaultReaderChart(t)
	granted := readerClusterRoleGrants(t, "helm template (default values)", rendered, func(name string) bool { return strings.HasSuffix(name, readerChartRoleName) })
	assertReaderRoleCoversCode(t, "chart default role", granted)
}

func TestKubectlManifestReaderRoleCoversConnectorReads(t *testing.T) {
	assertReaderRoleCoversCode(t, readerManifestPath, readerManifestRole(t))
}

func TestReaderRoleCopiesMatchTheChart(t *testing.T) {
	manifest := readerManifestRole(t)
	t.Run("chart", func(t *testing.T) {
		rendered := renderDefaultReaderChart(t)
		chart := readerClusterRoleGrants(t, "helm template (default values)", rendered, func(name string) bool { return strings.HasSuffix(name, readerChartRoleName) })
		assertReaderRolesMatch(t, readerManifestPath, manifest, "chart default role", chart)
	})
	t.Run("configuration guide", func(t *testing.T) {
		assertReaderRolesMatch(t, readerManifestPath, manifest, readerDocsRolePath, readerMarkdownRole(t, readerDocsRolePath, "## Kubernetes RBAC"))
	})
	t.Run("EKS example", func(t *testing.T) {
		assertReaderRolesMatch(t, readerManifestPath, manifest, readerEKSRolePath, readerMarkdownRole(t, readerEKSRolePath, ""))
	})
}

// The Secret opt-in grants list only. A get would let a describe fetch a whole
// Secret, values included, while list is all Helm release discovery needs.
func TestOptInSecretGrantIsListOnly(t *testing.T) {
	rendered := renderDefaultReaderChart(t, "--set", "kubernetesReaderRBAC.secrets=true")
	granted := readerClusterRoleGrants(t, "helm template (secrets opt-in)", rendered, func(name string) bool { return strings.HasSuffix(name, readerChartRoleName) })
	if !granted[readerGrant{resource: "secrets", verb: "list"}] {
		t.Error("the Secret opt-in must grant list on secrets")
	}
	for grant := range granted {
		if grant.resource == "secrets" && grant.verb != "list" {
			t.Errorf("the Secret opt-in grants %s; only list is allowed", grant)
		}
	}
}
