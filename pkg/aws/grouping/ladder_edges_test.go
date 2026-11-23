package grouping

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Every spelling of a denied key stays unlearned: environment, stage,
// owner, team and every cost key, in any case and separator.
func TestLearnDeniedKeysInEverySpelling(t *testing.T) {
	for _, key := range []string{"ENV", "Env_Name", "environment", "Environment-Name", "STAGE", "stage-name", "Stage_Name",
		"Owner", "owner_email", "OWNER-NAME", "TEAM", "team_name", "Cost", "cost_centre", "COSTCODE", "Cost-Allocation-Tag",
		"owneremail", "ownerid", "teamid", "envtype", "environmenttype", "DBOwner", "SREOwner", "BUSINESSOWNER", "APPENV"} {
		t.Run(key, func(t *testing.T) {
			sets := joinSets(repeat(30, map[string]string{key: "one"}), repeat(30, map[string]string{key: "two"}), repeat(40, nil))
			if got := Learn(taggedAccount(sets...))[prodAccount]; len(got) != 0 {
				t.Fatalf("%s learned as %v", key, got)
			}
		})
	}
}

// One resource carrying several spellings of the app key takes the first
// spelling with a usable value: an empty, blank or hidden spelling never
// hides a real one, for learning and for placing.
func TestSpellingVariantsWithEmptyValues(t *testing.T) {
	sets := joinSets(
		repeat(10, map[string]string{"App": "", "application": "Checkout"}),
		repeat(10, map[string]string{"APP": "  ", "app_name": "payments"}),
		repeat(10, map[string]string{"App": model.RedactedValue, "Application-Name": "search"}),
		repeat(70, nil))
	resources := taggedAccount(sets...)
	if got := Learn(resources)[prodAccount]; !reflect.DeepEqual(got, []string{"app"}) {
		t.Fatalf("learned = %v", got)
	}
	got := idsOf(Build(resources, nil, nil, Options{}), prodAccount)
	for index, want := range map[int]string{0: "tag:app=checkout", 9: "tag:app=checkout", 10: "tag:app=payments", 20: "tag:app=search"} {
		if id := got[fmt.Sprintf("f%04d", index)]; id != want {
			t.Errorf("f%04d placed in %s, want %s", index, id, want)
		}
	}
	// A resource whose only spelling is blank falls to the next rung.
	blank := withAttr(res(prodAccount, "lambda", "function", "blank", "blank", map[string]string{"app": "  "}), model.AttrStackName, "shop")
	if id := idsOf(Build([]model.Resource{blank}, TagKeys{prodAccount: {"app"}}, nil, Options{}), prodAccount)["blank"]; id != "stack:shop" {
		t.Fatalf("blank app value placed in %s", id)
	}
}

// The known spellings are tried before any other key, in their order,
// whatever their share: kubernetes.io/service-name is a known key even
// though it starts like a controller key.
func TestKnownSpellingsComeFirst(t *testing.T) {
	sets := joinSets(
		repeat(40, map[string]string{"project": "p1", "component": "c1"}), repeat(40, map[string]string{"project": "p2", "component": "c2"}),
		repeat(6, map[string]string{"kubernetes.io/service-name": "web"}), repeat(6, map[string]string{"kubernetes.io/service-name": "api"}),
		repeat(8, nil))
	got := Learn(taggedAccount(sets...))[prodAccount]
	if !reflect.DeepEqual(got, []string{"kubernetes.io/service-name", "component", "project"}) {
		t.Fatalf("learned = %v", got)
	}
}

// Landing-zone stacks are demoted in any case; a stack shared by every
// connected account is demoted only when two or more accounts are
// connected; the connected accounts default to the accounts the resources
// come from.
func TestBaselineStackDemotion(t *testing.T) {
	stacked := func(account, id, stack string) model.Resource {
		return withAttr(res(account, "lambda", "function", id, id, nil), model.AttrStackName, stack)
	}
	stackResource := func(account, name string) model.Resource {
		return withAttr(res(account, "cloudformation", "stack", name+"/1", name, nil), model.AttrID, name)
	}
	cases := []struct {
		name      string
		resources []model.Resource
		accounts  []string
		want      map[string]string
	}{
		{"landing-zone names in any case", []model.Resource{stacked(prodAccount, "a", "STACKSET-guardrails-1"),
			stacked(prodAccount, "b", "Aws-ControlTower-LogArchive"), stackResource(prodAccount, "StackSet-x")},
			nil, map[string]string{"a": UngroupedID, "b": UngroupedID, "1": UngroupedID}},
		{"one connected account keeps its stack", []model.Resource{stacked(prodAccount, "a", "network-baseline"), stackResource(prodAccount, "network-baseline")},
			[]string{prodAccount}, map[string]string{"a": "stack:network-baseline", "1": "stack:network-baseline"}},
		{"every account from the resources", []model.Resource{stacked(prodAccount, "a", "network-baseline"), stackResource(prodAccount, "network-baseline"),
			stacked(otherAccount, "x", "network-baseline"), stacked(prodAccount, "b", "shop")},
			nil, map[string]string{"a": UngroupedID, "1": UngroupedID, "b": "stack:shop"}},
		{"every connected account, the stack resource too", []model.Resource{stackResource(prodAccount, "network-baseline"),
			stackResource(otherAccount, "network-baseline")},
			[]string{prodAccount, otherAccount}, map[string]string{"1": UngroupedID}},
		{"a baseline's resources make no prefix group", []model.Resource{
			withAttr(res(prodAccount, "config", "config-rule", "r1", "guard-rule-one", nil), model.AttrStackName, "StackSet-guard"),
			withAttr(res(prodAccount, "config", "config-rule", "r2", "guard-rule-two", nil), model.AttrStackName, "StackSet-guard"),
			withAttr(res(prodAccount, "config", "config-rule", "r3", "guard-rule-three", nil), model.AttrStackName, "StackSet-guard")},
			nil, map[string]string{"r1": UngroupedID, "r2": UngroupedID, "r3": UngroupedID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := idsOf(Build(tc.resources, TagKeys{}, nil, Options{Accounts: tc.accounts}), prodAccount)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("placements = %v, want %v", got, tc.want)
			}
		})
	}
}

// named makes count functions named fmt.Sprintf(format, index).
func named(idPrefix, format string, count int) []model.Resource {
	out := make([]model.Resource, 0, count)
	for index := range count {
		id := fmt.Sprintf("%s%02d", idPrefix, index)
		out = append(out, res(prodAccount, "lambda", "function", id, fmt.Sprintf(format, index), nil))
	}
	return out
}

// The prefix rung: names that look like AWS IDs, numeric first words and
// placed resources never make a prefix; a two-word prefix is used when it
// alone has enough members, and the rest fall back to their first word.
func TestPrefixRungEdges(t *testing.T) {
	singles := func(count int) []model.Resource { return named("s", "solo%02dx-thing", count) }
	prefixOf := func(t *testing.T, resources []model.Resource, id string) string {
		t.Helper()
		return idsOf(Build(resources, TagKeys{prodAccount: {"app"}}, nil, Options{}), prodAccount)[id]
	}
	t.Run("names that look like AWS IDs", func(t *testing.T) {
		resources := append(named("a", "snap-0123456789abcdef%d", 3), named("b", "VPCE-0ABCDEF12345678%d", 3)...)
		resources = append(resources, named("c", "eipalloc-00000000000000a%d", 3)...)
		got := idsOf(Build(resources, TagKeys{}, nil, Options{}), prodAccount)
		for id, group := range got {
			if group != UngroupedID {
				t.Errorf("%s placed in %s", id, group)
			}
		}
	})
	t.Run("a numeric first word", func(t *testing.T) {
		if got := prefixOf(t, named("a", "2024-orders-%d", 3), "a00"); got != UngroupedID {
			t.Fatalf("placed in %s", got)
		}
	})
	t.Run("placed resources do not count toward a prefix", func(t *testing.T) {
		resources := named("a", "orders-api-%d", 3)
		resources[0].Tags = map[string]string{"app": "x"}
		resources[1].Tags = map[string]string{"app": "y"}
		if got := prefixOf(t, resources, "a02"); got != UngroupedID {
			t.Fatalf("placed in %s", got)
		}
	})
	t.Run("a two-word prefix with enough members is used", func(t *testing.T) {
		resources := append(named("a", "acme-cart-%d", 3), singles(30)...)
		if got := prefixOf(t, resources, "a00"); got != "prefix:acme-cart-" {
			t.Fatalf("placed in %s", got)
		}
	})
	t.Run("short two-word prefixes fall back to the first word", func(t *testing.T) {
		resources := append(named("a", "acme-cart-%d", 2), named("b", "acme-users-%d", 2)...)
		if got := prefixOf(t, resources, "a00"); got != "prefix:acme-" {
			t.Fatalf("placed in %s", got)
		}
	})
	t.Run("a two-word prefix and its first word side by side", func(t *testing.T) {
		resources := append(named("a", "acme-cart-%d", 3), named("b", "acme-users-%d", 1)...)
		resources = append(resources, named("c", "acme-search-%d", 1)...)
		resources = append(resources, named("d", "acme-billing-%d", 1)...)
		got := idsOf(Build(resources, TagKeys{}, nil, Options{}), prodAccount)
		if got["a00"] != "prefix:acme-cart-" || got["b00"] != "prefix:acme-" || got["c00"] != "prefix:acme-" || got["d00"] != "prefix:acme-" {
			t.Fatalf("placements = %v", got)
		}
	})
	t.Run("an extended prefix needs a useful second word", func(t *testing.T) {
		resources := append(named("a", "acme-cart-%d", 10), named("b", "acme-%d-db", 3)...)
		if got := prefixOf(t, resources, "b00"); got != "prefix:acme-" {
			t.Fatalf("placed in %s", got)
		}
	})
}

// EKS collapse with more than one cluster: a controller marker alone
// cannot be placed, but a cluster named by a tag places it; shared cluster
// tags are not ownership in any case; two owner tags pick one cluster the
// same way on every run.
func TestEKSCollapseWithSeveralClusters(t *testing.T) {
	cluster := func(name string) model.Resource {
		return model.Resource{ARN: arnOf(prodAccount, "eks", "cluster/"+name), Service: "eks", Type: "cluster", Account: prodAccount,
			Region: prodRegion, Name: name}
	}
	node := func(id string, tags map[string]string) model.Resource {
		return res(prodAccount, "ec2", "instance", id, id, tags)
	}
	resources := []model.Resource{cluster("prod-eks"), cluster("data-eks"),
		node("i-1", map[string]string{"karpenter.sh/nodepool": "default", "karpenter.sh/discovery": "data-eks"}),
		node("i-2", map[string]string{"karpenter.sh/nodepool": "default", "kubernetes.io/cluster/data-eks": "owned"}),
		node("i-3", map[string]string{"karpenter.sh/nodepool": "default", "kubernetes.io/cluster/data-eks": "Shared"}),
		node("i-4", map[string]string{"kubernetes.io/cluster/prod-eks": "SHARED"}),
		node("i-5", map[string]string{"kubernetes.io/cluster/prod-eks": "owned", "kubernetes.io/cluster/data-eks": "owned"}),
		node("i-6", map[string]string{"eks:nodegroup-name": "ng-1"}),
		{ARN: arnOf(prodAccount, "eks", "nodegroup/data-eks/ng-2/uuid"), Service: "eks", Type: "node-group", Account: prodAccount, Name: "ng-2"},
		withAttr(res(prodAccount, "ec2", "subnet", "subnet-1", "subnet-1", map[string]string{"eks:cluster-name": "data-eks"}), model.AttrVPC, "vpc-1"),
	}
	want := map[string]string{"prod-eks": "eks:prod-eks", "data-eks": "eks:data-eks", "i-1": "eks:data-eks", "i-2": "eks:data-eks",
		"i-3": UngroupedID, "i-4": UngroupedID, "i-5": "eks:data-eks", "i-6": UngroupedID, "uuid": "eks:data-eks", "subnet-1": UngroupedID}
	for run := range 50 {
		if got := idsOf(Build(resources, TagKeys{}, nil, Options{}), prodAccount); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d placements =\n%v\nwant\n%v", run, got, want)
		}
	}
	// With one cluster, an eks: or karpenter marker alone joins it.
	single := []model.Resource{cluster("prod-eks"), node("i-6", map[string]string{"eks:nodegroup-name": "ng-1"}),
		node("i-7", map[string]string{"karpenter.k8s.aws/ec2nodeclass": "default"}),
		node("i-8", map[string]string{"kubernetes.io/cluster/prod-eks": "shared"})}
	got := idsOf(Build(single, TagKeys{}, nil, Options{}), prodAccount)
	if got["i-6"] != "eks:prod-eks" || got["i-7"] != "eks:prod-eks" || got["i-8"] != UngroupedID {
		t.Fatalf("one cluster placements = %v", got)
	}
}

// Fifty rounds of node replacement, where the node pool also scales in
// and out, change no group ID and leave every correction applied.
func TestNodeReplacementWithScalingKeepsIDsAndCorrections(t *testing.T) {
	corrections := []Correction{
		{Account: prodAccount, Kind: CorrectionRename, GroupID: "eks:prod-eks", Name: "Prod cluster"},
		{Account: prodAccount, Kind: CorrectionMerge, GroupID: "eks:prod-eks/ns:search", TargetGroupID: "eks:prod-eks"},
		{Account: prodAccount, Kind: CorrectionMove, MemberARN: arnOf(prodAccount, "lambda", "function/fn003"), TargetGroupID: "tag:service=payments"},
	}
	ids := func(result Result) []string {
		var out []string
		for _, group := range result.Groups {
			out = append(out, group.ID+"|"+group.Name)
		}
		return out
	}
	first := Build(prodEstate(0), nil, corrections, Options{})
	want := ids(first)
	for round := 1; round <= 50; round++ {
		keep := (round * 37) % 200
		var resources []model.Resource
		nodes := 0
		for _, resource := range prodEstate(round) {
			if resource.Type == "instance" && resource.Tags["eks:cluster-name"] != "" {
				if nodes >= keep {
					continue
				}
				nodes++
			}
			resources = append(resources, resource)
		}
		result := Build(resources, nil, corrections, Options{})
		if got := ids(result); !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d (%d nodes) groups =\n%v\nwant\n%v", round, keep, got, want)
		}
		for index, status := range result.Corrections {
			if status != CorrectionApplied {
				t.Fatalf("round %d correction %d = %s", round, index, status)
			}
		}
	}
}

// A rename aimed at a group a correction merged away reads "no longer
// matches": the group it names is gone, and the rename lands on no other
// group.
func TestRenameOfAMergedAwayGroup(t *testing.T) {
	corrections := []Correction{
		{Account: prodAccount, Kind: CorrectionMerge, GroupID: "tag:service=payments", TargetGroupID: "tag:app=checkout"},
		{Account: prodAccount, Kind: CorrectionRename, GroupID: "tag:service=payments", Name: "Payments"},
	}
	result := Build(prodEstate(0), nil, corrections, Options{})
	checkout := groupByID(t, result, "tag:app=checkout")
	if checkout.Renamed || checkout.Name != "checkout" || len(checkout.Members) != 170 {
		t.Fatalf("checkout = %s renamed=%v %d", checkout.Name, checkout.Renamed, len(checkout.Members))
	}
	want := []CorrectionStatus{CorrectionApplied, CorrectionNoLongerMatches}
	if !reflect.DeepEqual(result.Corrections, want) {
		t.Fatalf("statuses = %v, want %v", result.Corrections, want)
	}
}

// A rename made before the merge still applies; the merge then folds the
// renamed group away and the target keeps its own name. A merge into a
// group that is gone moves nothing.
func TestRenameThenMergeAndMergeIntoAGoneGroup(t *testing.T) {
	corrections := []Correction{
		{Account: prodAccount, Kind: CorrectionRename, GroupID: "tag:service=payments", Name: "Payments"},
		{Account: prodAccount, Kind: CorrectionMerge, GroupID: "tag:service=payments", TargetGroupID: "tag:app=checkout"},
		{Account: prodAccount, Kind: CorrectionMerge, GroupID: "stack:baseline", TargetGroupID: "tag:app=gone"},
	}
	result := Build(prodEstate(0), nil, corrections, Options{})
	want := []CorrectionStatus{CorrectionApplied, CorrectionApplied, CorrectionNoLongerMatches}
	if !reflect.DeepEqual(result.Corrections, want) {
		t.Fatalf("statuses = %v", result.Corrections)
	}
	if checkout := groupByID(t, result, "tag:app=checkout"); checkout.Renamed || len(checkout.Members) != 170 {
		t.Fatalf("checkout = %+v", checkout.Name)
	}
	if baseline := groupByID(t, result, "stack:baseline"); len(baseline.Members) != 130 {
		t.Fatalf("baseline holds %d", len(baseline.Members))
	}
}

// Owner, environment and cost keys with a qualifier in front (the
// business-owner, technical-owner and cost-center tags of common tagging
// guides) are owner, environment and cost keys too, and are never learned
// as application keys.
func TestLearnDeniesQualifiedOwnerEnvironmentAndCostKeys(t *testing.T) {
	for _, key := range []string{"BusinessOwner", "technical-owner", "ApplicationOwner", "service_owner", "app_env", "AppEnvironment",
		"service-cost-center", "AppTeam"} {
		t.Run(key, func(t *testing.T) {
			sets := joinSets(repeat(30, map[string]string{key: "one"}), repeat(30, map[string]string{key: "two"}), repeat(40, nil))
			if got := Learn(taggedAccount(sets...))[prodAccount]; len(got) != 0 {
				t.Fatalf("%s learned as %v", key, got)
			}
		})
	}
}
