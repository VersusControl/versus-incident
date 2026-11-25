package grouping

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

type groupShape struct {
	id, name, rule, key string
	size                int
}

func shapes(groups []Group) []groupShape {
	out := make([]groupShape, 0, len(groups))
	for _, group := range groups {
		out = append(out, groupShape{group.ID, group.Name, group.Rule, group.Key, len(group.Members)})
	}
	return out
}

// The prod account of the topology mock-up groups into exactly its six
// groups, each made by the rule the card names, with 468 of 1,200
// resources (39%) left Ungrouped and broken down by type.
func TestProdEstateGivesTheMockupGroups(t *testing.T) {
	resources := prodEstate(0)
	result := Build(resources, nil, nil, Options{})
	want := []groupShape{
		{"eks:prod-eks", "prod-eks", RuleCluster, "prod-eks", 380},
		{"eks:prod-eks/ns:search", "search", RuleNamespace, "search", 52},
		{"stack:baseline", "baseline", RuleStack, "baseline", 130},
		{"tag:app=checkout", "checkout", RuleTag, "app", 96},
		{"tag:service=payments", "payments", RuleTag, "service", 74},
		{UngroupedID, "Ungrouped", RuleUngrouped, "", 468},
	}
	if got := shapes(result.Of(prodAccount)); !reflect.DeepEqual(got, want) {
		t.Fatalf("groups =\n%+v\nwant\n%+v", got, want)
	}
	total := 0
	for _, group := range result.Groups {
		total += len(group.Members)
	}
	ungrouped := result.Of(prodAccount)[5]
	if total != 1200 || len(ungrouped.Members)*100/total != 39 {
		t.Fatalf("ungrouped %d of %d", len(ungrouped.Members), total)
	}
	wantTypes := map[string]int{"volume": 212, "function": 88, "security-group": 64, "role": 51,
		"stack": 3, "config-rule": 40, "vpc": 1, "subnet": 6, "route-table": 3}
	if !reflect.DeepEqual(ungrouped.ByType, wantTypes) {
		t.Fatalf("ungrouped by type = %v", ungrouped.ByType)
	}
	if learned := Learn(resources)[prodAccount]; !reflect.DeepEqual(learned, []string{"app", "service"}) {
		t.Fatalf("learned keys = %v", learned)
	}
	for _, member := range result.Groups[0].Members {
		if strings.HasSuffix(member, "#policy") {
			t.Fatalf("a policy entry was grouped: %s", member)
		}
	}
}

// Replacing every node of the cluster 50 times, as node pools are replaced
// daily, changes no group ID, name, rule or size.
func TestNodeChurnKeepsEveryGroupID(t *testing.T) {
	first := shapes(Build(prodEstate(0), nil, nil, Options{}).Groups)
	for round := 1; round <= 50; round++ {
		resources := prodEstate(round)
		if got := shapes(Build(resources, nil, nil, Options{}).Groups); !reflect.DeepEqual(got, first) {
			t.Fatalf("round %d groups =\n%+v\nwant\n%+v", round, got, first)
		}
	}
	// The nodes really were replaced.
	before, after := map[string]bool{}, 0
	for _, resource := range prodEstate(0) {
		if resource.Type == "instance" {
			before[resource.ARN] = true
		}
	}
	for _, resource := range prodEstate(50) {
		if resource.Type == "instance" && before[resource.ARN] {
			after++
		}
	}
	if after != 20 {
		t.Fatalf("%d instances kept their ARN, want only checkout's 20", after)
	}
}

// learnCase is one account's tag sets for the key learner.
func taggedAccount(sets ...map[string]string) []model.Resource {
	out := make([]model.Resource, 0, len(sets))
	for index, tags := range sets {
		out = append(out, res(prodAccount, "lambda", "function", fmt.Sprintf("f%04d", index), fmt.Sprintf("f%04d", index), tags))
	}
	return out
}

func repeat(count int, tags map[string]string) []map[string]string {
	out := make([]map[string]string, count)
	for index := range out {
		out[index] = tags
	}
	return out
}

func joinSets(parts ...[]map[string]string) []map[string]string {
	var out []map[string]string
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// Keys are learned per account by share and distinct values; spellings of
// one idea fold into one key; environment, owner, team and cost keys never
// qualify; empty and hidden values do not count.
func TestLearnTagKeys(t *testing.T) {
	cases := []struct {
		name string
		sets []map[string]string
		want []string
	}{
		{"spellings fold", joinSets(repeat(5, map[string]string{"App": "a"}), repeat(5, map[string]string{"application_name": "b"}),
			repeat(5, map[string]string{"APP": "c"}), repeat(85, nil)), []string{"app"}},
		{"service spellings", joinSets(repeat(6, map[string]string{"ServiceName": "a"}), repeat(6, map[string]string{"service-name": "b"}),
			repeat(88, nil)), []string{"service"}},
		{"below ten percent", joinSets(repeat(4, map[string]string{"app": "a"}), repeat(5, map[string]string{"app": "b"}), repeat(91, nil)), nil},
		{"at ten percent", joinSets(repeat(5, map[string]string{"app": "a"}), repeat(5, map[string]string{"app": "b"}), repeat(90, nil)), []string{"app"}},
		{"one value", repeat(100, map[string]string{"app": "shop"}), nil},
		{"case folds to one value", joinSets(repeat(50, map[string]string{"app": "Shop"}), repeat(50, map[string]string{"app": "shop"})), nil},
		{"empty values", joinSets(repeat(5, map[string]string{"app": "a"}), repeat(5, map[string]string{"app": "b"}),
			repeat(1, map[string]string{"app": ""}), repeat(10, map[string]string{"app": "  "}), repeat(79, nil)), []string{"app"}},
		{"empty values do not reach the share", joinSets(repeat(3, map[string]string{"app": "a"}), repeat(3, map[string]string{"app": "b"}),
			repeat(50, map[string]string{"app": ""}), repeat(44, nil)), nil},
		{"hidden values", joinSets(repeat(50, map[string]string{"app": model.RedactedValue}), repeat(50, map[string]string{"app": model.RedactedValue})), nil},
		{"deny list", joinSets(repeat(25, map[string]string{"env": "a", "Environment": "x", "stage": "s1", "Owner": "o1", "team": "t1",
			"CostCenter": "c1", "cost-allocation": "k1", "Name": "n1"}), repeat(25, map[string]string{"env": "b", "Environment": "y", "stage": "s2",
			"Owner": "o2", "team": "t2", "CostCenter": "c2", "cost-allocation": "k2", "Name": "n2"}), repeat(50, nil)), nil},
		{"controller keys", joinSets(repeat(25, map[string]string{"karpenter.sh/nodepool": "a", "kubernetes.io/created-for/pvc/namespace": "x",
			"aws:cloudformation:stack-name": "s1"}), repeat(25, map[string]string{"karpenter.sh/nodepool": "b",
			"kubernetes.io/created-for/pvc/namespace": "y", "aws:cloudformation:stack-name": "s2"}), repeat(50, nil)), nil},
		{"known spellings first", joinSets(repeat(30, map[string]string{"project": "p1", "service": "s1", "app.kubernetes.io/name": "k1"}),
			repeat(30, map[string]string{"project": "p2", "service": "s2", "app.kubernetes.io/name": "k2"}),
			repeat(20, map[string]string{"component": "c1"}), repeat(20, map[string]string{"component": "c2"})),
			[]string{"service", "app.kubernetes.io/name", "project", "component"}},
		{"too many values", func() []map[string]string {
			var out []map[string]string
			for index := range 201 {
				out = append(out, map[string]string{"app": fmt.Sprint("a", index)})
			}
			return out
		}(), nil},
		{"two hundred values", func() []map[string]string {
			var out []map[string]string
			for index := range 200 {
				out = append(out, map[string]string{"app": fmt.Sprint("a", index)})
			}
			return out
		}(), []string{"app"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Learn(taggedAccount(tc.sets...))[prodAccount]
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("learned = %v, want %v", got, tc.want)
			}
		})
	}
	// Keys are learned per account: a key one account uses is not applied
	// to another that does not.
	mixed := append(taggedAccount(joinSets(repeat(5, map[string]string{"app": "a"}), repeat(5, map[string]string{"app": "b"}))...),
		res(otherAccount, "lambda", "function", "x", "x", map[string]string{"app": "a"}))
	if learned := Learn(mixed); len(learned[otherAccount]) != 0 || len(learned[prodAccount]) != 1 {
		t.Fatalf("per account = %v", learned)
	}
}

func idsOf(result Result, account string) map[string]string {
	out := map[string]string{}
	for _, group := range result.Of(account) {
		for _, member := range group.Members {
			out[model.ResourceID(member)] = group.ID
		}
	}
	return out
}

// Each rung of the ladder, alone and against the rung below it.
func TestLadderRules(t *testing.T) {
	learnApp := TagKeys{prodAccount: {"app"}}
	appTags := func(value string) map[string]string { return map[string]string{"app": value} }
	cluster := model.Resource{ARN: arnOf(prodAccount, "eks", "cluster/prod-eks"), Service: "eks", Type: "cluster", Account: prodAccount,
		Region: prodRegion, Name: "prod-eks"}
	stacked := func(id, stack string, tags map[string]string) model.Resource {
		return withAttr(res(prodAccount, "lambda", "function", id, id, tags), model.AttrStackName, stack)
	}
	cases := []struct {
		name      string
		resources []model.Resource
		learned   TagKeys
		accounts  []string
		want      map[string]string
	}{
		{"tag", []model.Resource{res(prodAccount, "lambda", "function", "a", "a", appTags("Checkout")),
			res(prodAccount, "lambda", "function", "b", "b", appTags("checkout "))},
			learnApp, nil, map[string]string{"a": "tag:app=checkout", "b": "tag:app=checkout"}},
		{"tag beats stack", []model.Resource{stacked("a", "shop", appTags("checkout")), stacked("b", "shop", nil)},
			learnApp, nil, map[string]string{"a": "tag:app=checkout", "b": "stack:shop"}},
		{"a key that was not learned is not used", []model.Resource{stacked("a", "shop", map[string]string{"project": "x"})},
			learnApp, nil, map[string]string{"a": "stack:shop"}},
		{"stack resource and members", []model.Resource{
			withAttr(res(prodAccount, "cloudformation", "stack", "shop/1", "shop", nil), model.AttrID, "shop"), stacked("a", "shop", nil)},
			TagKeys{}, nil, map[string]string{"1": "stack:shop", "a": "stack:shop"}},
		{"stack from the CloudFormation tag", []model.Resource{res(prodAccount, "sqs", "queue", "q", "q",
			map[string]string{"aws:cloudformation:stack-name": "shop"})}, TagKeys{}, nil, map[string]string{"q": "stack:shop"}},
		{"appregistry beats stack", []model.Resource{
			{ARN: arnOf(prodAccount, "servicecatalog", "application/app1"), Service: "appregistry", Type: "application", Account: prodAccount,
				Name: "storefront", Relations: []model.Relation{{Type: model.RelationAssociated, Target: arnOf(prodAccount, "lambda", "function/a")},
					{Type: model.RelationAssociated, Target: arnOf(prodAccount, "cloudformation", "stack/shop/1")}}},
			withAttr(res(prodAccount, "cloudformation", "stack", "shop/1", "shop", nil), model.AttrID, "shop"),
			res(prodAccount, "lambda", "function", "a", "a", nil), stacked("b", "shop", nil), stacked("c", "other", nil)},
			TagKeys{}, nil, map[string]string{"app1": "ungrouped", "a": "appregistry:storefront", "1": "appregistry:storefront",
				"b": "appregistry:storefront", "c": "stack:other"}},
		{"landing-zone stacks are demoted", []model.Resource{stacked("a", "StackSet-AWSControlTowerBP-BASELINE-CONFIG-1", nil),
			stacked("b", "aws-controltower-BaselineRoles", nil), stacked("c", "shop", nil)},
			TagKeys{}, nil, map[string]string{"a": "ungrouped", "b": "ungrouped", "c": "stack:shop"}},
		{"a stack in every connected account is demoted", []model.Resource{stacked("a", "network-baseline", nil),
			withAttr(res(otherAccount, "lambda", "function", "x", "x", nil), model.AttrStackName, "network-baseline"),
			stacked("b", "shop", nil)},
			TagKeys{}, nil, map[string]string{"a": "ungrouped", "b": "stack:shop"}},
		{"a stack missing from one connected account stays", []model.Resource{stacked("a", "network-baseline", nil),
			withAttr(res(otherAccount, "lambda", "function", "x", "x", nil), model.AttrStackName, "network-baseline")},
			TagKeys{}, []string{prodAccount, otherAccount, "123400000003"}, map[string]string{"a": "stack:network-baseline"}},
		{"demoted stack falls to the next rung", []model.Resource{
			withAttr(res(prodAccount, "lambda", "function", "a", "billing-api-a", nil), model.AttrStackName, "StackSet-x"),
			res(prodAccount, "lambda", "function", "b", "billing-api-b", nil), res(prodAccount, "lambda", "function", "c", "billing-api-c", nil),
			res(prodAccount, "lambda", "function", "d", "billing-api-d", appTags("x")), res(prodAccount, "lambda", "function", "e", "billing-api-e", nil)},
			TagKeys{prodAccount: {"app"}}, nil, map[string]string{"a": "ungrouped", "b": "prefix:billing-api-", "c": "prefix:billing-api-", "d": "tag:app=x",
				"e": "prefix:billing-api-"}},
		{"cluster collapse beats tags", []model.Resource{cluster,
			{ARN: arnOf(prodAccount, "eks", "nodegroup/prod-eks/ng/1"), Service: "eks", Type: "node-group", Account: prodAccount, Name: "ng",
				Relations: []model.Relation{{Type: model.RelationMemberOf, Target: cluster.ARN}}},
			res(prodAccount, "ec2", "instance", "i-1", "node", map[string]string{"eks:cluster-name": "prod-eks", "app": "platform"}),
			res(prodAccount, "ec2", "instance", "i-2", "node", map[string]string{"kubernetes.io/cluster/prod-eks": "owned"}),
			res(prodAccount, "ec2", "instance", "i-3", "node", map[string]string{"karpenter.sh/nodepool": "default"}),
			res(prodAccount, "ec2", "volume", "v-1", "v", map[string]string{"kubernetes.io/cluster/prod-eks": model.RedactedValue}),
			withAttr(res(prodAccount, "ec2", "volume", "v-2", "v", map[string]string{"kubernetes.io/cluster/prod-eks": model.RedactedValue}),
				model.AttrClusterTags, "prod-eks=owned"),
			withAttr(res(prodAccount, "ec2", "volume", "v-3", "v", map[string]string{"kubernetes.io/cluster/prod-eks": model.RedactedValue}),
				model.AttrClusterTags, "prod-eks=shared"),
			res(prodAccount, "ec2", "subnet", "s-1", "s", map[string]string{"kubernetes.io/cluster/prod-eks": "shared", "karpenter.sh/discovery": "prod-eks"}),
			res(prodAccount, "ec2", "security-group", "sg-1", "s", map[string]string{"kubernetes.io/cluster/prod-eks": "shared"})},
			learnApp, nil, map[string]string{"prod-eks": "eks:prod-eks", "1": "eks:prod-eks", "i-1": "eks:prod-eks", "i-2": "eks:prod-eks",
				"i-3": "eks:prod-eks", "v-1": "ungrouped", "v-2": "eks:prod-eks", "v-3": "ungrouped", "s-1": "ungrouped", "sg-1": "ungrouped"}},
		{"namespace groups", []model.Resource{cluster,
			res(prodAccount, "ec2", "volume", "v-1", "v", map[string]string{"kubernetes.io/created-for/pvc/namespace": "search"}),
			res(prodAccount, "elasticloadbalancing", "load-balancer", "lb", "lb", map[string]string{"kubernetes.io/service-name": "search/web"}),
			res(prodAccount, "elasticloadbalancing", "target-group", "tg", "tg", map[string]string{"service.k8s.aws/stack": "orders/api",
				"elbv2.k8s.aws/cluster": "other-eks"}),
			res(prodAccount, "ec2", "volume", "v-2", "v", map[string]string{"kubernetes.io/created-for/pvc/namespace": model.RedactedValue,
				"kubernetes.io/cluster/prod-eks": "owned"})},
			TagKeys{}, nil, map[string]string{"prod-eks": "eks:prod-eks", "v-1": "eks:prod-eks/ns:search", "lb": "eks:prod-eks/ns:search",
				"tg": "eks:other-eks/ns:orders", "v-2": "eks:prod-eks"}},
		{"a controller marker with two clusters stays unplaced", []model.Resource{cluster,
			{ARN: arnOf(prodAccount, "eks", "cluster/data-eks"), Service: "eks", Type: "cluster", Account: prodAccount, Name: "data-eks"},
			res(prodAccount, "ec2", "instance", "i-1", "i-1", map[string]string{"karpenter.sh/nodepool": "default"})},
			TagKeys{}, nil, map[string]string{"prod-eks": "eks:prod-eks", "data-eks": "eks:data-eks", "i-1": "ungrouped"}},
		{"ecs cluster", []model.Resource{
			{ARN: arnOf(prodAccount, "ecs", "cluster/web"), Service: "ecs", Type: "cluster", Account: prodAccount, Name: "web"},
			{ARN: arnOf(prodAccount, "ecs", "service/web/api"), Service: "ecs", Type: "service", Account: prodAccount, Name: "api",
				Relations: []model.Relation{{Type: model.RelationMemberOf, Target: arnOf(prodAccount, "ecs", "cluster/web")}}}},
			TagKeys{}, nil, map[string]string{"web": "ecs:web", "api": "ecs:web"}},
		{"auto scaling group", []model.Resource{
			{ARN: arnOf(prodAccount, "autoscaling", "autoScalingGroup:1:autoScalingGroupName/batch"), Service: "autoscaling",
				Type: "auto-scaling-group", Account: prodAccount, Name: "batch",
				Relations: []model.Relation{{Type: model.RelationContains, Target: arnOf(prodAccount, "ec2", "instance/i-1")}}},
			res(prodAccount, "ec2", "instance", "i-1", "i-1", nil), res(prodAccount, "ec2", "instance", "i-2", "i-2", nil)},
			TagKeys{}, nil, map[string]string{"batch": "asg:batch", "i-1": "asg:batch", "i-2": "ungrouped"}},
		{"name prefix", []model.Resource{res(prodAccount, "lambda", "function", "a", "prod-orders-api", nil),
			res(prodAccount, "sqs", "queue", "b", "orders_queue", nil), res(prodAccount, "dynamodb", "table", "c", "Orders.Table", nil),
			res(prodAccount, "lambda", "function", "d", "users-api", nil), res(prodAccount, "lambda", "function", "e", "users-worker", nil),
			res(prodAccount, "lambda", "function", "f", "orders", nil), res(prodAccount, "ec2", "volume", "vol-0123456789abcdef0", "vol-0123456789abcdef0", nil)},
			TagKeys{}, nil, map[string]string{"a": "prefix:orders-", "b": "prefix:orders-", "c": "prefix:orders-", "d": "ungrouped", "e": "ungrouped",
				"f": "ungrouped", "vol-0123456789abcdef0": "ungrouped"}},
		{"an organization-wide first word is extended", func() []model.Resource {
			var out []model.Resource
			for index := range 5 {
				out = append(out, res(prodAccount, "lambda", "function", fmt.Sprint("a", index), fmt.Sprintf("acme-cart-%d", index), nil),
					res(prodAccount, "lambda", "function", fmt.Sprint("b", index), fmt.Sprintf("acme-users-%d", index), nil))
			}
			return out
		}(), TagKeys{}, nil, func() map[string]string {
			out := map[string]string{}
			for index := range 5 {
				out[fmt.Sprint("a", index)], out[fmt.Sprint("b", index)] = "prefix:acme-cart-", "prefix:acme-users-"
			}
			return out
		}()},
		{"vpc when there are two", []model.Resource{
			withAttr(res(prodAccount, "ec2", "vpc", "vpc-1", "shared-net", nil), model.AttrID, "vpc-1"),
			withAttr(res(prodAccount, "ec2", "security-group", "sg-a", "sg-a", nil), model.AttrVPC, "vpc-1"),
			withAttr(res(prodAccount, "ec2", "security-group", "sg-b", "sg-b", nil), model.AttrVPC, "vpc-2"),
			{ARN: arnOf(prodAccount, "rds", "db/x"), Service: "rds", Type: "db-instance", Account: prodAccount, Name: "x",
				Relations: []model.Relation{{Type: model.RelationInVPC, Target: arnOf(prodAccount, "ec2", "vpc/vpc-2")}}},
			res(prodAccount, "iam", "role", "r", "r", nil)},
			TagKeys{}, nil, map[string]string{"vpc-1": "vpc:vpc-1", "sg-a": "vpc:vpc-1", "sg-b": "vpc:vpc-2", "x": "vpc:vpc-2", "r": "ungrouped"}},
		{"one vpc makes no group", []model.Resource{
			withAttr(res(prodAccount, "ec2", "security-group", "sg-a", "sg-a", nil), model.AttrVPC, "vpc-1"),
			withAttr(res(prodAccount, "ec2", "security-group", "sg-b", "sg-b", nil), model.AttrVPC, "vpc-1")},
			TagKeys{}, nil, map[string]string{"sg-a": "ungrouped", "sg-b": "ungrouped"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Build(tc.resources, tc.learned, nil, Options{Accounts: tc.accounts})
			got := idsOf(result, prodAccount)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("placements =\n%v\nwant\n%v", got, tc.want)
			}
			ungrouped := 0
			for _, group := range result.Of(prodAccount) {
				if group.ID == UngroupedID {
					ungrouped++
				}
			}
			if ungrouped != 1 {
				t.Fatalf("Ungrouped groups = %d, want exactly one", ungrouped)
			}
		})
	}
}

// Ungrouped is a group even when nothing is in it.
func TestUngroupedIsNeverHidden(t *testing.T) {
	result := Build([]model.Resource{res(prodAccount, "lambda", "function", "a", "a", map[string]string{"app": "x"})},
		TagKeys{prodAccount: {"app"}}, nil, Options{})
	groups := result.Of(prodAccount)
	if len(groups) != 2 || groups[1].ID != UngroupedID || len(groups[1].Members) != 0 || groups[1].Members == nil {
		t.Fatalf("groups = %+v", groups)
	}
}

// Group IDs come only from what a group is: shuffling the input changes
// nothing.
func TestGroupingDoesNotDependOnOrder(t *testing.T) {
	resources := prodEstate(0)
	want := Build(resources, nil, nil, Options{})
	reversed := append([]model.Resource(nil), resources...)
	sort.Slice(reversed, func(left, right int) bool { return reversed[left].ARN > reversed[right].ARN })
	if got := Build(reversed, nil, nil, Options{}); !reflect.DeepEqual(got, want) {
		t.Fatal("grouping depends on input order")
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"tag:app=checkout": true, "stack:baseline": true, "eks:prod-eks/ns:search": true, "prefix:checkout-": true,
		"vpc:vpc-1": true, "user:receipts": true, UngroupedID: true, "asg:batch": true, "ecs:web": true, "appregistry:x": true,
		"": false, "tag:": false, "user: ": false, "checkout": false, "other:x": false,
	} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) = %v", id, !want)
		}
	}
}

// The fixture is what the inventory holds after the collector's redaction
// with the default allowlist: cluster tag values are hidden, so the nodes
// reach their cluster only through the recorded marker, while the
// application keys, in any case, stay visible.
func TestProdEstateIsRedactedLikeTheInventory(t *testing.T) {
	hidden, visible := 0, 0
	for _, resource := range prodEstate(0) {
		if value, ok := resource.Tags["kubernetes.io/cluster/"+prodCluster]; ok && resource.Type == "instance" {
			if value != model.RedactedValue || resource.Attributes[model.AttrClusterTags] != prodCluster+"=owned" {
				t.Fatalf("node %s: cluster tag %q, marker %q", resource.ARN, value, resource.Attributes[model.AttrClusterTags])
			}
			hidden++
		}
		if resource.Tags["App"] == "Checkout" {
			visible++
		}
	}
	if hidden != 240 || visible != 32 {
		t.Fatalf("hidden cluster tags %d, visible App values %d", hidden, visible)
	}
}

// Most services name a resource by the ID in its ARN: functions, buckets,
// queues, tables and roles. Those names make prefix groups; a name that
// redaction put in place of a hidden Name tag does not.
func TestPrefixGroupsFromRealARNNames(t *testing.T) {
	var resources []model.Resource
	for _, name := range []string{"orders-api", "orders-worker", "orders-cron"} {
		resources = append(resources, model.Resource{ARN: "arn:aws:lambda:eu-south-2:" + prodAccount + ":function:" + name, Service: "lambda",
			Type: "function", Account: prodAccount, Region: prodRegion, Name: name})
	}
	for _, name := range []string{"orders-assets", "orders-logs", "orders-exports"} {
		resources = append(resources, model.Resource{ARN: "arn:aws:s3:::" + name, Service: "s3", Type: "bucket", Account: prodAccount,
			Region: prodRegion, Name: name})
	}
	for _, name := range []string{"orders-q1", "orders-q2"} {
		resources = append(resources, model.Resource{ARN: "arn:aws:sqs:eu-south-2:" + prodAccount + ":" + name, Service: "sqs", Type: "queue",
			Account: prodAccount, Region: prodRegion, Name: name})
	}
	for _, id := range []string{"web-a", "web-b", "web-c"} {
		resources = append(resources, model.Resource{ARN: "arn:aws:ec2:eu-south-2:" + prodAccount + ":instance/" + id, Service: "ec2",
			Type: "instance", Account: prodAccount, Region: prodRegion, Name: id, Tags: map[string]string{model.NameTagKey: model.RedactedValue}})
	}
	result := Build(resources, TagKeys{}, nil, Options{})
	orders := groupByID(t, result, "prefix:orders-")
	if len(orders.Members) != 8 || orders.ByType["function"] != 3 || orders.ByType["bucket"] != 3 || orders.ByType["queue"] != 2 {
		t.Fatalf("orders = %d %v", len(orders.Members), orders.ByType)
	}
	if ungrouped := groupByID(t, result, UngroupedID); len(ungrouped.Members) != 3 {
		t.Fatalf("redacted names grouped: ungrouped holds %d", len(ungrouped.Members))
	}
}

// Each prefix is decided on its own: adding or removing resources with
// other names changes no prefix group's ID or size, and a correction on
// one stays applied.
func TestPrefixGroupsDoNotDependOnUnrelatedResources(t *testing.T) {
	build := func(others int) Result {
		var resources []model.Resource
		for index, app := range []string{"checkout", "checkout", "checkout", "search", "search", "search", "billing", "billing", "billing", "auth", "auth"} {
			id := fmt.Sprintf("f%02d", index)
			resources = append(resources, res(prodAccount, "lambda", "function", id, "acme-"+app+"-"+id, nil))
		}
		for index := range others {
			resources = append(resources, res(prodAccount, "ec2", "instance", fmt.Sprintf("n%03d", index), fmt.Sprintf("node%03d-pool-x", index), nil))
		}
		return Build(resources, TagKeys{}, []Correction{{Account: prodAccount, Kind: CorrectionRename, GroupID: "prefix:acme-checkout-", Name: "Checkout"}}, Options{})
	}
	shape := func(result Result) []groupShape {
		var out []groupShape
		for _, group := range result.Of(prodAccount) {
			if group.ID != UngroupedID {
				out = append(out, groupShape{group.ID, group.Name, group.Rule, group.Key, len(group.Members)})
			}
		}
		return out
	}
	want := []groupShape{
		{"prefix:acme-billing-", "acme-billing", RulePrefix, "acme-billing-", 3},
		{"prefix:acme-checkout-", "Checkout", RulePrefix, "acme-checkout-", 3},
		{"prefix:acme-search-", "acme-search", RulePrefix, "acme-search-", 3},
	}
	for _, others := range []int{0, 30, 40, 400} {
		result := build(others)
		if got := shape(result); !reflect.DeepEqual(got, want) {
			t.Fatalf("with %d others groups =\n%+v\nwant\n%+v", others, got, want)
		}
		if result.Corrections[0] != CorrectionApplied {
			t.Fatalf("with %d others the rename = %s", others, result.Corrections[0])
		}
	}
}

// Acronyms split from the word after them, and the keys that only look
// like a denied word stay learnable.
func TestKeyWordsAndKeysThatStayLearnable(t *testing.T) {
	for key, want := range map[string][]string{
		"DBOwner": {"db", "owner"}, "SREOwner": {"sre", "owner"}, "BusinessOwner": {"business", "owner"},
		"BUSINESSOWNER": {"businessowner"}, "app_env": {"app", "env"}, "service-cost-center": {"service", "cost", "center"},
		"AppEnvironment": {"app", "environment"}, "k8sApp": {"k8s", "app"},
	} {
		if got := keyWords(key); !reflect.DeepEqual(got, want) {
			t.Errorf("keyWords(%q) = %v, want %v", key, got, want)
		}
	}
	for _, key := range []string{"envoy", "app", "App", "application", "Application", "service", "Service"} {
		sets := joinSets(repeat(30, map[string]string{key: "one"}), repeat(30, map[string]string{key: "two"}), repeat(40, nil))
		if got := Learn(taggedAccount(sets...))[prodAccount]; len(got) != 1 {
			t.Errorf("%s not learned: %v", key, got)
		}
	}
}
