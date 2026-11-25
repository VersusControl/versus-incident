package grouping

import (
	"regexp"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// ---- CloudFormation ----

// stackTagKey is the tag CloudFormation puts on what a stack creates.
const stackTagKey = "aws:cloudformation:stack-name"

// stackOf is the name of the stack that made a resource, or of the stack
// the resource is.
func stackOf(resource model.Resource) string {
	if resource.Service == "cloudformation" && resource.Type == "stack" {
		return resource.Name
	}
	if name := strings.TrimSpace(resource.Attributes[model.AttrStackName]); name != "" {
		return name
	}
	if value, ok := usableValue(resource.Tags[stackTagKey]); ok {
		return value
	}
	return ""
}

// landingZoneStack reports a stack name a landing zone deploys: a StackSet
// instance or a Control Tower stack.
func landingZoneStack(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, "stackset-") || strings.HasPrefix(lower, "aws-controltower-")
}

// ---- EKS ----

// clusterNameKeys carry the name of the EKS cluster a resource belongs to.
var clusterNameKeys = []string{
	"eks:cluster-name", "aws:eks:cluster-name", "eks:eks-cluster-name", "elbv2.k8s.aws/cluster",
	"alpha.eksctl.io/cluster-name", "karpenter.sh/discovery",
}

// ownerFromClusterTags is the cluster a kubernetes.io/cluster/<name> tag
// says owns the resource, the first by name when several do. The owned or
// shared marker comes from the attribute collectors record before
// redaction; without it, only a visible "owned" value counts, so a hidden
// value, which may be "shared", is never read as ownership.
func ownerFromClusterTags(resource model.Resource) string {
	markers := model.ParseClusterTags(resource.Attributes[model.AttrClusterTags])
	if len(markers) == 0 {
		markers = model.ParseClusterTags(model.ClusterTagAttribute(resource.Tags))
	}
	owner := ""
	for name, marker := range markers {
		if marker == model.ClusterOwned && (owner == "" || name < owner) {
			owner = name
		}
	}
	return owner
}

// controllerMarkers start keys only a cluster's controllers write.
var controllerMarkers = []string{"karpenter.sh/", "karpenter.k8s.aws/", "eks:", "aws:eks:"}

// namespaceKeys carry the Kubernetes namespace a controller made a
// resource for. A value of the form namespace/name gives its namespace.
var namespaceKeys = []string{
	"kubernetes.io/created-for/pvc/namespace", "kubernetes.io/service-name", "service.k8s.aws/stack", "ingress.k8s.aws/stack",
}

// networkTypes are the shared network resources a cluster only borrows:
// a subnet tagged for a cluster still belongs to the network.
var networkTypes = map[string]bool{
	"vpc": true, "subnet": true, "route-table": true, "internet-gateway": true, "nat-gateway": true,
	"network-acl": true, "transit-gateway": true, "transit-gateway-attachment": true,
}

// eksOwner says which EKS cluster owns a resource, and for what namespace,
// when the resource is the cluster, one of its node groups, or something a
// controller made for it. clusters names the account's clusters by ARN.
func eksOwner(resource model.Resource, clusters map[string]string) (cluster, namespace string, owned bool) {
	if resource.Service == "eks" {
		switch resource.Type {
		case "cluster":
			return resource.Name, "", resource.Name != ""
		case "node-group":
			for _, relation := range resource.Relations {
				if relation.Type == model.RelationMemberOf {
					if name := clusters[relation.Target]; name != "" {
						return name, "", true
					}
					if name := eksClusterFromARN(relation.Target); name != "" {
						return name, "", true
					}
				}
			}
			if name := eksClusterFromARN(resource.ARN); name != "" {
				return name, "", true
			}
		}
	}
	if networkTypes[resource.Type] {
		return "", "", false
	}
	for _, key := range clusterNameKeys {
		if value, ok := usableValue(resource.Tags[key]); ok {
			cluster = value
			break
		}
	}
	marked := cluster != ""
	if cluster == "" {
		cluster = ownerFromClusterTags(resource)
		marked = cluster != ""
	}
	for key := range resource.Tags {
		for _, prefix := range controllerMarkers {
			if strings.HasPrefix(key, prefix) {
				marked = true
			}
		}
	}
	for _, key := range namespaceKeys {
		if value, ok := usableValue(resource.Tags[key]); ok {
			if before, _, found := strings.Cut(value, "/"); found {
				value = before
			} else if key != namespaceKeys[0] {
				continue
			}
			if value = strings.TrimSpace(value); value != "" {
				namespace = value
				marked = true
				break
			}
		}
	}
	if !marked {
		return "", "", false
	}
	if cluster == "" {
		// A controller marker without a cluster name belongs to the
		// account's only cluster; with several, it cannot be placed.
		names := map[string]bool{}
		for _, name := range clusters {
			names[name] = true
		}
		if len(names) != 1 {
			return "", "", false
		}
		for name := range names {
			cluster = name
		}
	}
	return cluster, namespace, true
}

// eksClusterFromARN is the cluster name in an EKS cluster or node group
// ARN.
func eksClusterFromARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[2] != "eks" {
		return ""
	}
	resource := parts[5]
	for _, prefix := range []string{"cluster/", "nodegroup/"} {
		if rest, ok := strings.CutPrefix(resource, prefix); ok {
			name, _, _ := strings.Cut(rest, "/")
			return name
		}
	}
	return ""
}

// ecsClusterFromARN is the cluster name in an ECS cluster ARN.
func ecsClusterFromARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[2] != "ecs" {
		return ""
	}
	name, ok := strings.CutPrefix(parts[5], "cluster/")
	if !ok {
		return ""
	}
	return name
}

// ---- Name prefixes ----

// MinPrefixMembers is how many resources must share a prefix for it to
// make a group.
const MinPrefixMembers = 3

// skippedNameWords lead a name without saying what it belongs to:
// environment words and the provider's own.
var skippedNameWords = map[string]bool{
	"prod": true, "production": true, "prd": true, "dev": true, "develop": true, "development": true,
	"stage": true, "staging": true, "stg": true, "test": true, "testing": true, "qa": true, "uat": true,
	"sandbox": true, "sbx": true, "preprod": true, "nonprod": true, "aws": true, "amazon": true, "default": true,
}

var (
	nameSeparators = regexp.MustCompile(`[-_./: ]+`)
	idLikeName     = regexp.MustCompile(`^[a-z]{1,12}-[0-9a-f]{8,}$`)
)

// nameWords are the words of a resource's own name, without the leading
// environment and provider words. Most services name a resource by its ID
// in the ARN (a function, a bucket, a queue, a role), and that name counts.
// A name that looks like a generated AWS ID, or that redaction put in place
// of a hidden Name tag, has none.
func nameWords(resource model.Resource) []string {
	name := strings.ToLower(strings.TrimSpace(resource.Name))
	if name == "" || name == strings.ToLower(model.RedactedValue) || idLikeName.MatchString(name) || strings.HasPrefix(name, "arn:") {
		return nil
	}
	if resource.Tags[model.NameTagKey] == model.RedactedValue && resource.Name == model.ResourceID(resource.ARN) {
		return nil
	}
	var words []string
	for _, word := range nameSeparators.Split(name, -1) {
		if word != "" {
			words = append(words, word)
		}
	}
	for len(words) > 0 && skippedNameWords[words[0]] {
		words = words[1:]
	}
	return words
}

// usefulWord is a word a prefix may be made of: three characters or more,
// not only digits.
func usefulWord(word string) bool {
	return len(word) >= 3 && strings.Trim(word, "0123456789") != ""
}

// ---- VPC ----

// vpcOf is the VPC a resource is in, or is.
func vpcOf(resource model.Resource) string {
	if resource.Type == "vpc" {
		if id := resource.Attributes[model.AttrID]; id != "" {
			return id
		}
		return model.ResourceID(resource.ARN)
	}
	if id := strings.TrimSpace(resource.Attributes[model.AttrVPC]); id != "" {
		return id
	}
	for _, relation := range resource.Relations {
		if relation.Type == model.RelationInVPC {
			if id := model.ResourceID(relation.Target); id != "" && id != model.RedactedValue {
				return id
			}
		}
	}
	return ""
}
