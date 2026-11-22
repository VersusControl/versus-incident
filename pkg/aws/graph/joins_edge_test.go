package graph

import (
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// A relation built in code with no kind, the shape a row stored before
// kinds existed has once loaded, joins as a configured link.
func TestRelationWithoutKindJoinsAsConfigured(t *testing.T) {
	first := "arn:aws:sqs:eu-south-2:" + hub + ":orders"
	second := "arn:aws:sqs:eu-south-2:" + hub + ":orders-dlq"
	resources := []model.Resource{
		res(hub, first, "sqs", "queue", "orders", model.Relation{Type: model.RelationDeadLetter, Target: second}),
		res(hub, second, "sqs", "queue", "orders-dlq"),
	}
	edge := edgeBetween(t, Build(resources, connected(hub), nil), first, second, model.RelationDeadLetter)
	if edge.Kind != model.RelationConfigured || edge.Count != 1 || edge.Sides != "" {
		t.Fatalf("edge = %+v", edge)
	}
}

// DNS forms are normalised on both ends: the load balancer's own name may
// carry a trailing dot or capitals, a distribution matches the same way, and
// two forms of one host no account answers to end on one dangling node.
func TestDNSFormsNormalisedOnBothEnds(t *testing.T) {
	balancer := "arn:aws:elasticloadbalancing:eu-south-2:" + spoke + ":loadbalancer/app/web/1"
	lb := res(spoke, balancer, "elbv2", "load-balancer", "web")
	lb.Attributes = map[string]string{model.AttrDNSName: "Web-1.EU-South-2.elb.amazonaws.com."}
	distribution := "arn:aws:cloudfront::" + hub + ":distribution/E1"
	cdn := res(hub, distribution, "cloudfront", "distribution", "E1")
	cdn.Attributes = map[string]string{model.AttrDNSName: "d1a2b3c4d5e6f7.cloudfront.net"}
	record := func(name, target string) model.Resource {
		return res(dns, "arn:aws:route53:::hostedzone/Z1/record/"+name, "route53", "dns-record", name,
			rel(model.RelationAliasOf, model.RelationConfigured, target))
	}
	resources := []model.Resource{
		lb, cdn,
		record("www", "dualstack.web-1.eu-south-2.elb.amazonaws.com"),
		record("assets", "D1A2B3C4D5E6F7.CloudFront.net."),
		record("old-a", "DualStack.Gone-9.eu-south-2.elb.amazonaws.com."),
		record("old-b", "gone-9.eu-south-2.elb.amazonaws.com"),
		record("lookalike", "xdualstack.web-1.eu-south-2.elb.amazonaws.com"),
	}
	g := Build(resources, connected(hub, spoke, dns), nil)
	if edge := edgeBetween(t, g, "arn:aws:route53:::hostedzone/Z1/record/www", balancer, model.RelationAliasOf); edge.Sides != BothSides {
		t.Fatalf("www sides = %q", edge.Sides)
	}
	if edge := edgeBetween(t, g, "arn:aws:route53:::hostedzone/Z1/record/assets", distribution, model.RelationAliasOf); edge.Sides != BothSides {
		t.Fatalf("assets sides = %q", edge.Sides)
	}
	gone := "host:gone-9.eu-south-2.elb.amazonaws.com"
	edgeBetween(t, g, "arn:aws:route53:::hostedzone/Z1/record/old-a", gone, model.RelationAliasOf)
	edgeBetween(t, g, "arn:aws:route53:::hostedzone/Z1/record/old-b", gone, model.RelationAliasOf)
	hosts := 0
	for _, node := range g.Nodes {
		if node.Type == NodeHost {
			hosts++
		}
	}
	// The gone host once, and the lookalike that must not match the balancer.
	if hosts != 2 {
		t.Fatalf("host nodes = %d, want 2: %+v", hosts, g.Nodes)
	}
	if edge := edgeBetween(t, g, "arn:aws:route53:::hostedzone/Z1/record/lookalike", "host:xdualstack.web-1.eu-south-2.elb.amazonaws.com",
		model.RelationAliasOf); edge.Sides != OneSide {
		t.Fatalf("lookalike sides = %q", edge.Sides)
	}
}

func tableGrants(account, role, access string, count int) []model.Resource {
	var out []model.Resource
	for index := range count {
		name := fmt.Sprintf("prod-orders-%04d", index)
		out = append(out, res(account, "arn:aws:dynamodb:eu-south-2:"+account+":table/"+name, "dynamodb", "table", name,
			model.Relation{Type: model.RelationMayAccess, Kind: model.RelationKindMayAccess, Target: role, Access: access}))
	}
	return out
}

func edgesTo(g Graph, to string) []Edge {
	var out []Edge
	for _, edge := range g.Edges {
		if edge.To == to {
			out = append(out, edge)
		}
	}
	return out
}

// Collapse starts above WideGrantThreshold: that many grants stay separate
// links, one more becomes a single link carrying the count. Grants of
// different access classes never fold into one link.
func TestWideGrantCollapseThresholdAndAccessClass(t *testing.T) {
	role := "arn:aws:iam::" + spoke + ":role/reporting"
	at := Build(tableGrants(hub, role, model.AccessRead, WideGrantThreshold), connected(hub, spoke), nil)
	if got := edgesTo(at, role); len(got) != WideGrantThreshold {
		t.Fatalf("at the threshold: %d links, want %d", len(got), WideGrantThreshold)
	}
	for _, node := range at.Nodes {
		if node.Type == NodeGroup {
			t.Fatalf("group made at the threshold: %+v", node)
		}
	}
	reader := res(spoke, role, "iam", "role", "reporting")
	above := Build(append(tableGrants(hub, role, model.AccessRead, WideGrantThreshold+1), reader), connected(hub, spoke), nil)
	got := edgesTo(above, role)
	if len(got) != 1 || got[0].Count != WideGrantThreshold+1 || got[0].Pattern != "prod-orders-*" || got[0].Sides != BothSides {
		t.Fatalf("above the threshold = %+v", got)
	}

	resources := tableGrants(hub, role, model.AccessRead, 11)
	for index, resource := range tableGrants(hub, role, model.AccessWrite, 11) {
		resources[index].Relations = append(resources[index].Relations, resource.Relations...)
	}
	mixed := edgesTo(Build(resources, connected(hub, spoke), nil), role)
	if len(mixed) != 2 || mixed[0].Access == mixed[1].Access || mixed[0].Count != 11 || mixed[1].Count != 11 {
		t.Fatalf("read and write grants = %+v", mixed)
	}
}

// A grant written as a prefix pattern is one link with the count the
// collector gave it, on the graph and at the Accounts level.
func TestPrefixGrantKeepsItsCount(t *testing.T) {
	role := "arn:aws:iam::" + spoke + ":role/reporting"
	bucket := res(hub, "arn:aws:s3:::exports", "s3", "bucket", "exports",
		model.Relation{Type: model.RelationMayAccess, Kind: model.RelationKindMayAccess, Target: role, Access: model.AccessRead,
			Pattern: "prod-*", Count: 910})
	g := Build([]model.Resource{bucket, res(spoke, role, "iam", "role", "reporting")}, connected(hub, spoke), nil)
	edges := edgesTo(g, role)
	if len(edges) != 1 || edges[0].Count != 910 || edges[0].Pattern != "prod-*" {
		t.Fatalf("prefix grant = %+v", edges)
	}
	view := Accounts(g)
	if len(view.Links) != 1 || view.Links[0].Count != 910 || view.Links[0].BothSides != 910 {
		t.Fatalf("accounts links = %+v", view.Links)
	}
}

// A trust may name a role without the path IAM lists it under; it still
// joins the listed role and is seen from both sides.
func TestTrustJoinsARoleListedUnderAPath(t *testing.T) {
	listed := "arn:aws:iam::" + spoke + ":role/team/ci"
	trusting := res(hub, "arn:aws:iam::"+hub+":role/deploy", "iam", "role", "deploy",
		rel(model.RelationTrusts, model.RelationKindMayAccess, "arn:aws:iam::"+spoke+":role/ci"))
	g := Build([]model.Resource{trusting, res(spoke, listed, "iam", "role", "ci")}, connected(hub, spoke), nil)
	if edge := edgeBetween(t, g, trusting.ARN, listed, model.RelationTrusts); edge.Sides != BothSides {
		t.Fatalf("sides = %q", edge.Sides)
	}
	for _, node := range g.Nodes {
		if node.Dangling {
			t.Fatalf("dangling node for a listed role: %+v", node)
		}
	}
}

// An organization trust is seen from both sides only when a connected
// account reads itself as a member; otherwise from one side.
func TestOrganizationTrustWithoutAConnectedMemberIsOneSided(t *testing.T) {
	org := res(hub, "arn:aws:organizations::123400000090:organization/o-a1b2c3d4e5", "organizations", "organization", "o-a1b2c3d4e5")
	org.Attributes = map[string]string{model.AttrOrganizationID: "o-a1b2c3d4e5", model.AttrManagementAccount: "123400000090"}
	role := res(spoke, "arn:aws:iam::"+spoke+":role/shared", "iam", "role", "shared",
		rel(model.RelationTrusts, model.RelationKindMayAccess, model.OrgTarget("o-a1b2c3d4e5")))
	accounts := []Account{{ID: hub, Connected: false, LastReadAt: lastRead}, {ID: spoke, Connected: true, LastReadAt: lastRead}}
	for name, resources := range map[string][]model.Resource{
		"member read by a disconnected account": {org, role},
		"no member read":                        {role},
	} {
		t.Run(name, func(t *testing.T) {
			g := Build(resources, accounts, nil)
			if edge := edgeBetween(t, g, role.ARN, "org:o-a1b2c3d4e5", model.RelationTrusts); edge.Sides != OneSide {
				t.Fatalf("sides = %q", edge.Sides)
			}
			for _, card := range g.Accounts {
				if card.ID == "" || card.ID == "org:o-a1b2c3d4e5" {
					t.Fatalf("organization made an account card: %+v", card)
				}
				if card.ID == "123400000090" && card.Label == LabelSameOrg {
					t.Fatalf("management account labelled same_org with no connected member: %+v", card)
				}
			}
		})
	}
}

// Unknown principal IDs of every kind read as a deleted or unknown
// principal, never as an account, and sum under one far end.
func TestUnknownPrincipalIDsNeverBecomeAccounts(t *testing.T) {
	policy := res(hub, "arn:aws:s3:::exports#policy", "s3-policy", "resource-policy", "exports",
		rel(model.RelationMayAccess, model.RelationKindMayAccess, "AIDAJQABLZS4A3QDU576Q"),
		rel(model.RelationMayAccess, model.RelationKindMayAccess, "AROAEXAMPLEDELETED002"))
	policy.Attributes = map[string]string{model.AttrSubject: "arn:aws:s3:::exports"}
	g := Build([]model.Resource{res(hub, "arn:aws:s3:::exports", "s3", "bucket", "exports"), policy}, connected(hub), nil)
	for _, id := range []string{"AIDAJQABLZS4A3QDU576Q", "AROAEXAMPLEDELETED002"} {
		node := nodeOf(t, g, "principal:"+id)
		if node.Account != "" || node.Type != NodePrincipal || !node.Dangling {
			t.Fatalf("principal %s = %+v", id, node)
		}
	}
	if len(g.Accounts) != 1 || g.Accounts[0].ID != hub {
		t.Fatalf("accounts = %+v", g.Accounts)
	}
	view := Accounts(g)
	if len(view.Links) != 1 || view.Links[0].Count != 2 || view.Links[0].To != "" || view.Links[0].Far != "a deleted or unknown principal" ||
		view.Links[0].BothSides != 0 {
		t.Fatalf("links = %+v", view.Links)
	}
}

// Disconnecting the account that holds a link, or dropping it from the
// account list altogether, keeps the link: seen from one side, with the
// last read time when one is known.
func TestDisconnectedHolderKeepsItsLinks(t *testing.T) {
	trusting := res(hub, "arn:aws:iam::"+hub+":role/deploy", "iam", "role", "deploy",
		rel(model.RelationTrusts, model.RelationKindMayAccess, "arn:aws:iam::"+spoke+":role/ci"))
	caller := res(spoke, "arn:aws:iam::"+spoke+":role/ci", "iam", "role", "ci")
	resources := []model.Resource{trusting, caller}
	earlier := lastRead.Add(-6 * time.Hour)
	disconnected := Build(resources, []Account{{ID: hub, LastReadAt: earlier}, {ID: spoke, Connected: true, LastReadAt: lastRead}}, nil)
	edge := edgeBetween(t, disconnected, trusting.ARN, caller.ARN, model.RelationTrusts)
	if edge.Sides != OneSide || !edge.LastReadAt.Equal(earlier) {
		t.Fatalf("holder disconnected: %+v", edge)
	}
	if node := nodeOf(t, disconnected, trusting.ARN); node.Dangling {
		t.Fatalf("the holder's own resource turned dangling: %+v", node)
	}
	removed := Build(resources, connected(spoke), nil)
	if edge := edgeBetween(t, removed, trusting.ARN, caller.ARN, model.RelationTrusts); edge.Sides != OneSide {
		t.Fatalf("holder removed from settings: %+v", edge)
	}
	if len(Accounts(removed).Links) != 1 {
		t.Fatalf("accounts links = %+v", Accounts(removed).Links)
	}
}

// A connected far account that did not read the far end holds no evidence:
// the link is seen from one side and the end says it was not read.
func TestConnectedFarAccountWithoutTheEndIsOneSided(t *testing.T) {
	trusting := res(hub, "arn:aws:iam::"+hub+":role/deploy", "iam", "role", "deploy",
		rel(model.RelationTrusts, model.RelationKindMayAccess, "arn:aws:iam::"+spoke+":role/removed"))
	g := Build([]model.Resource{trusting, res(spoke, "arn:aws:iam::"+spoke+":role/other", "iam", "role", "other")}, connected(hub, spoke), nil)
	if edge := edgeBetween(t, g, trusting.ARN, "arn:aws:iam::"+spoke+":role/removed", model.RelationTrusts); edge.Sides != OneSide {
		t.Fatalf("sides = %q", edge.Sides)
	}
	if node := nodeOf(t, g, "arn:aws:iam::"+spoke+":role/removed"); !node.Dangling || node.Description != "a role not read in account "+spoke {
		t.Fatalf("far end = %+v", node)
	}
}
