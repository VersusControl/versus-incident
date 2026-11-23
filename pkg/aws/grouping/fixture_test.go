package grouping

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/settings"
)

// The prod account of the topology mock-up: 1,200 resources that group into
// checkout (tag app), payments (tag service), search (namespace on
// prod-eks), baseline (stack), prod-eks (cluster) and Ungrouped, which
// holds 468 (39%). It carries the mess real accounts have: three spellings
// of the application key, a case variant of a value, empty values,
// environment, team and cost keys on everything, a node pool tagged with an
// application, and landing-zone stacks.
const (
	prodAccount  = "123400000002"
	otherAccount = "123400000004"
	prodRegion   = "eu-south-2"
	prodVPC      = "vpc-0200000000000b002"
	prodCluster  = "prod-eks"
)

func arnOf(account, service, resource string) string {
	return model.ARN("aws", service, prodRegion, account, resource)
}

func res(account, service, typ, id, name string, tags map[string]string) model.Resource {
	return model.Resource{ARN: arnOf(account, service, typ+"/"+id), Service: service, Type: typ, Account: account,
		Region: prodRegion, Name: name, Tags: tags}
}

func withAttr(resource model.Resource, key, value string) model.Resource {
	if resource.Attributes == nil {
		resource.Attributes = map[string]string{}
	}
	resource.Attributes[key] = value
	return resource
}

func hexID(prefix string, n int) string { return fmt.Sprintf("%s-%017x", prefix, n) }

// background tags real accounts put on nearly everything.
func background(tags map[string]string) map[string]string {
	if tags == nil {
		tags = map[string]string{}
	}
	tags["env"] = "prod"
	tags["Team"] = "shop"
	tags["CostCenter"] = "4200"
	return tags
}

// collected passes resources through what a collector does before it
// stores them, with the default tag-value allowlist: the cluster markers
// are recorded from the tag values, then every value whose key is not
// allowlisted is redacted.
func collected(resources []model.Resource) []model.Resource {
	allowlist := map[string]bool{}
	for _, key := range settings.DefaultTagValueAllowlist() {
		allowlist[key] = true
	}
	out := make([]model.Resource, 0, len(resources))
	for _, resource := range resources {
		if clusters := model.ClusterTagAttribute(resource.Tags); clusters != "" {
			resource = withAttr(resource, model.AttrClusterTags, clusters)
		}
		model.RedactName(&resource, allowlist)
		out = append(out, model.Sanitize(resource))
	}
	return out
}

// prodEstate builds the prod account as the inventory holds it, after the
// collector's redaction with the default allowlist. round names the node
// generation: every node and its root volume get new IDs per round, as a
// node pool replaced overnight would.
func prodEstate(round int) []model.Resource {
	return collected(rawProdEstate(round))
}

// rawProdEstate is the prod account as AWS answers, before redaction.
func rawProdEstate(round int) []model.Resource {
	var out []model.Resource
	add := func(resources ...model.Resource) { out = append(out, resources...) }

	// checkout: 96 resources tagged with the app key in three spellings,
	// each also carrying the service key.
	checkoutTypes := []struct {
		service, typ string
		count        int
	}{{"lambda", "function", 30}, {"sqs", "queue", 10}, {"dynamodb", "table", 6}, {"elasticloadbalancing", "load-balancer", 2},
		{"elasticloadbalancing", "target-group", 4}, {"s3", "bucket", 4}, {"ec2", "instance", 20}, {"ec2", "security-group", 10}, {"iam", "role", 10}}
	n := 0
	for _, kind := range checkoutTypes {
		for index := range kind.count {
			tags := background(map[string]string{"service": "checkout-api"})
			switch n % 3 {
			case 0:
				tags["app"] = "checkout"
			case 1:
				tags["App"] = "Checkout"
			default:
				tags["application"] = "checkout"
			}
			resource := res(prodAccount, kind.service, kind.typ, fmt.Sprintf("checkout-%s-%d", kind.typ, index), fmt.Sprintf("checkout-%s-%d", kind.typ, index), tags)
			if kind.typ == "instance" || kind.typ == "security-group" {
				resource = withAttr(resource, model.AttrVPC, prodVPC)
			}
			add(resource)
			n++
		}
	}

	// payments: 74 resources tagged with the service key in two cases,
	// and an app key left empty.
	paymentTypes := []struct {
		service, typ string
		count        int
	}{{"lambda", "function", 30}, {"dynamodb", "table", 10}, {"sqs", "queue", 10}, {"secretsmanager", "secret", 10}, {"iam", "role", 14}}
	n = 0
	for _, kind := range paymentTypes {
		for index := range kind.count {
			tags := background(map[string]string{"app": " "})
			if n%4 == 0 {
				tags["Service"] = "Payments"
			} else {
				tags["service"] = "payments"
			}
			add(res(prodAccount, kind.service, kind.typ, fmt.Sprintf("payments-%s-%d", kind.typ, index), fmt.Sprintf("payments-%s-%d", kind.typ, index), tags))
			n++
		}
	}

	// search: 52 resources the cluster's controllers made for the search
	// namespace.
	for index := range 48 {
		add(res(prodAccount, "ec2", "volume", hexID("vol", 0x5000+index), hexID("vol", 0x5000+index), map[string]string{
			"kubernetes.io/cluster/" + prodCluster: "owned", "kubernetes.io/created-for/pvc/namespace": "search",
			"kubernetes.io/created-for/pvc/name": fmt.Sprintf("data-search-%d", index)}))
	}
	for index := range 2 {
		add(res(prodAccount, "elasticloadbalancing", "load-balancer", fmt.Sprintf("k8s-search-web-%d", index), fmt.Sprintf("k8s-search-web-%d", index),
			map[string]string{"kubernetes.io/service-name": "search/web", "elbv2.k8s.aws/cluster": prodCluster}))
		add(res(prodAccount, "elasticloadbalancing", "target-group", fmt.Sprintf("k8s-search-tg-%d", index), fmt.Sprintf("k8s-search-tg-%d", index),
			map[string]string{"service.k8s.aws/stack": "search/web", "elbv2.k8s.aws/cluster": prodCluster}))
	}

	// baseline: the account's own shared stack and the 129 resources it
	// made.
	add(withAttr(res(prodAccount, "cloudformation", "stack", "baseline/1", "baseline", background(nil)), model.AttrID, "baseline"))
	baselineTypes := []struct {
		service, typ string
		count        int
	}{{"kms", "key", 20}, {"secretsmanager", "secret", 30}, {"iam", "role", 40}, {"sns", "topic", 9}, {"logs", "log-group", 30}}
	for _, kind := range baselineTypes {
		for index := range kind.count {
			add(withAttr(res(prodAccount, kind.service, kind.typ, fmt.Sprintf("baseline-%s-%d", kind.typ, index), fmt.Sprintf("baseline-%s-%d", kind.typ, index),
				background(nil)), model.AttrStackName, "baseline"))
		}
	}

	// prod-eks: the cluster, its node groups and their Auto Scaling
	// groups, 240 nodes with their root volumes, and the cluster's
	// security groups. The managed nodes carry an application tag their
	// node group propagates.
	clusterARN := arnOf(prodAccount, "eks", "cluster/"+prodCluster)
	add(model.Resource{ARN: clusterARN, Service: "eks", Type: "cluster", Account: prodAccount, Region: prodRegion, Name: prodCluster,
		Tags: background(map[string]string{"app": "platform"})})
	for index := range 4 {
		group := fmt.Sprintf("ng-%d", index)
		add(model.Resource{ARN: arnOf(prodAccount, "eks", "nodegroup/"+prodCluster+"/"+group+"/uuid"), Service: "eks", Type: "node-group",
			Account: prodAccount, Region: prodRegion, Name: group, Relations: []model.Relation{{Type: model.RelationMemberOf, Target: clusterARN}}})
		add(res(prodAccount, "autoscaling", "auto-scaling-group", "eks-"+group, "eks-"+group+"-a1b2",
			map[string]string{"eks:cluster-name": prodCluster, "kubernetes.io/cluster/" + prodCluster: "owned"}))
	}
	for index := range 200 {
		id := hexID("i", round<<20+index)
		add(withAttr(res(prodAccount, "ec2", "instance", id, "prod-eks-node", background(map[string]string{
			"eks:cluster-name": prodCluster, "kubernetes.io/cluster/" + prodCluster: "owned", "app": "platform", "Name": "prod-eks-node"})),
			model.AttrVPC, prodVPC))
	}
	for index := range 40 {
		id := hexID("i", round<<20+500+index)
		add(withAttr(res(prodAccount, "ec2", "instance", id, id, map[string]string{
			"karpenter.sh/nodepool": "default", "kubernetes.io/cluster/" + prodCluster: "owned"}), model.AttrVPC, prodVPC))
	}
	for index := range 120 {
		id := hexID("vol", round<<20+0x100000000+index)
		add(res(prodAccount, "ec2", "volume", id, id, map[string]string{"kubernetes.io/cluster/" + prodCluster: "owned"}))
	}
	for index := range 11 {
		id := hexID("sg", 0x7000+index)
		add(withAttr(res(prodAccount, "ec2", "security-group", id, "eks-cluster-sg-prod-eks", map[string]string{"aws:eks:cluster-name": prodCluster}),
			model.AttrVPC, prodVPC))
	}

	// Ungrouped: unattached volumes, functions, security groups and roles
	// with names that share nothing, the landing-zone stacks and what they
	// made, and the network.
	for index := range 212 {
		id := hexID("vol", 0x9000+index)
		add(res(prodAccount, "ec2", "volume", id, id, nil))
	}
	for index := range 88 {
		add(res(prodAccount, "lambda", "function", fmt.Sprintf("fn%03d", index), fmt.Sprintf("handler%03d", index), map[string]string{"app": ""}))
	}
	for index := range 64 {
		add(withAttr(res(prodAccount, "ec2", "security-group", hexID("sg", 0x8000+index), fmt.Sprintf("rules%03d", index), nil), model.AttrVPC, prodVPC))
	}
	for index := range 51 {
		add(res(prodAccount, "iam", "role", fmt.Sprintf("r%03d", index), fmt.Sprintf("role%03d", index), nil))
	}
	landing := []string{"StackSet-AWSControlTowerBP-BASELINE-CLOUDTRAIL-0a1b", "StackSet-AWSControlTowerBP-BASELINE-CONFIG-2c3d",
		"aws-controltower-BaselineRoles"}
	for index, name := range landing {
		add(withAttr(res(prodAccount, "cloudformation", "stack", name+"/"+fmt.Sprint(index), name, nil), model.AttrID, name))
	}
	for index := range 40 {
		name := landing[index%len(landing)]
		add(withAttr(res(prodAccount, "config", "config-rule", fmt.Sprintf("cr%03d", index), "aws-controltower-"+fmt.Sprintf("rule-%03d", index), nil),
			model.AttrStackName, name))
	}
	add(withAttr(res(prodAccount, "ec2", "vpc", prodVPC, "prod", map[string]string{"Name": "prod"}), model.AttrID, prodVPC))
	for index := range 6 {
		id := hexID("subnet", 0x100+index)
		add(withAttr(res(prodAccount, "ec2", "subnet", id, id, map[string]string{"kubernetes.io/cluster/" + prodCluster: "shared"}), model.AttrVPC, prodVPC))
	}
	for index := range 3 {
		id := hexID("rtb", 0x200+index)
		add(withAttr(res(prodAccount, "ec2", "route-table", id, id, nil), model.AttrVPC, prodVPC))
	}
	// A policy entry kept beside a bucket is not a resource of its own.
	add(model.Resource{ARN: arnOf(prodAccount, "s3", "bucket/checkout-bucket-0") + "#policy", Service: "s3", Type: model.ResourcePolicyType, Account: prodAccount})
	return out
}
