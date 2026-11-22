// Package graph assembles the AWS topology from what the stores already
// hold: the committed inventory with its typed relations, the connected
// accounts and the account labels. It makes no AWS call. A link that leaves
// one account is joined with the other account's evidence: it is seen from
// both sides when both accounts are connected and each read its end, and
// from one side otherwise, ending on a node that says what we cannot see.
package graph

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Account is one account the deployment knows from settings: connected now,
// or disconnected with the time its data was last read.
type Account struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Connected  bool      `json:"connected"`
	LastReadAt time.Time `json:"last_read_at,omitzero"`
}

// Label is one account label a user set.
type Label struct {
	AccountID string `json:"account_id"`
	Label     string `json:"label"`
	Name      string `json:"name,omitempty"`
}

// Account labels, in the order one wins over the next.
const (
	// LabelConnected is an account in Settings.
	LabelConnected = "connected"
	// LabelSameOrg is the management account of an organization a
	// connected account belongs to.
	LabelSameOrg = "same_org"
	// LabelKnown is an account a user labelled, or an AWS service account.
	LabelKnown = "known"
	// LabelUnknown is every other account.
	LabelUnknown = "unknown"
)

// Sides says whether a link that leaves an account was seen from both of
// its ends.
type Sides string

const (
	// BothSides: both accounts are connected and each read its end.
	BothSides Sides = "both"
	// OneSide: only one end was read; the other is not connected, was not
	// found, or cannot be attributed to an account.
	OneSide Sides = "one"
)

// Node kinds that are not stored resources.
const (
	NodeAccount      = "account"
	NodeOrganization = "organization"
	NodePrincipal    = "unknown-principal"
	NodeAnyone       = "anyone"
	NodeHost         = "host"
	NodeGroup        = "group"
)

// Node is one resource, or one thing a link ends on that we did not read.
// Account is the owning account, empty when it cannot be known (a host name
// with no matching load balancer, a deleted principal). ReadBy lists the
// accounts whose reads hold the node. A dangling node says in Description
// what it stands for. A group node stands for Members.
type Node struct {
	ID          string   `json:"id"`
	Account     string   `json:"account,omitempty"`
	Region      string   `json:"region,omitempty"`
	Service     string   `json:"service,omitempty"`
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	ReadBy      []string `json:"read_by,omitempty"`
	Dangling    bool     `json:"dangling,omitempty"`
	Description string   `json:"description,omitempty"`
	Members     []string `json:"members,omitempty"`
	dns         string
}

// Edge is one link. Sides is set when the link joins two accounts.
// LastReadAt is when the side that is not seen now was last read, when it
// was ever read. Count is how many grants or resources the link stands for.
type Edge struct {
	From       string             `json:"from"`
	To         string             `json:"to"`
	Type       model.RelationType `json:"type"`
	Kind       model.RelationKind `json:"kind"`
	Source     string             `json:"source,omitempty"`
	Access     string             `json:"access,omitempty"`
	Pattern    string             `json:"pattern,omitempty"`
	Count      int                `json:"count"`
	Sides      Sides              `json:"sides,omitempty"`
	LastReadAt time.Time          `json:"last_read_at,omitzero"`
	readBy     []string
}

// AccountCard is one account the graph knows: connected, or named by a
// link. Resources counts the nodes it owns that were read.
type AccountCard struct {
	ID             string    `json:"id"`
	Name           string    `json:"name,omitempty"`
	Label          string    `json:"label"`
	Connected      bool      `json:"connected"`
	LastReadAt     time.Time `json:"last_read_at,omitzero"`
	OrganizationID string    `json:"organization_id,omitempty"`
	Resources      int       `json:"resources"`
}

// Graph is the assembled topology.
type Graph struct {
	Nodes     []Node        `json:"nodes"`
	Edges     []Edge        `json:"edges"`
	Accounts  []AccountCard `json:"accounts"`
	Truncated bool          `json:"truncated,omitempty"`
}

// WideGrantThreshold is how many grants of one kind from resources of one
// type in one account to one principal collapse into a single link.
const WideGrantThreshold = 10

// awsServiceAccounts are AWS-owned accounts a policy commonly names: the
// Elastic Load Balancing log delivery accounts of the main Regions.
var awsServiceAccounts = map[string]string{
	"127311923021": "Elastic Load Balancing (us-east-1)",
	"033677994240": "Elastic Load Balancing (us-east-2)",
	"027434742980": "Elastic Load Balancing (us-west-1)",
	"797873946194": "Elastic Load Balancing (us-west-2)",
	"156460612806": "Elastic Load Balancing (eu-west-1)",
	"054676820928": "Elastic Load Balancing (eu-central-1)",
}

var (
	accountPattern  = regexp.MustCompile(`^[0-9]{12}$`)
	uniqueIDPattern = regexp.MustCompile(`^(AROA|AIDA|AGPA|ANPA|ANVA|AKIA|ASIA)[A-Z0-9]{12,}$`)
)

// arnAccount is the account field of an ARN, empty when it has none.
func arnAccount(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" || !accountPattern.MatchString(parts[4]) {
		return ""
	}
	return parts[4]
}

// roleKey identifies a role by account and name, whatever its path: a
// trust policy may name a role without the path IAM lists it under.
func roleKey(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[2] != "iam" || !strings.HasPrefix(parts[5], "role/") {
		return ""
	}
	return parts[4] + "/" + parts[5][strings.LastIndex(parts[5], "/")+1:]
}

// builder holds the indexes Build joins with.
type builder struct {
	nodes     map[string]*Node
	order     []string
	byDNS     map[string]string
	byRole    map[string]string
	accounts  map[string]Account
	labels    map[string]Label
	orgOf     map[string]string
	orgMgmt   map[string]string
	edges     map[edgeKey]*Edge
	edgeOrder []edgeKey
}

type edgeKey struct {
	from, to        string
	kind            model.RelationType
	how             model.RelationKind
	access, pattern string
}

// Build assembles the graph from stored resources, the accounts in Settings
// and the user's account labels. It reads nothing else and calls nothing.
func Build(resources []model.Resource, accounts []Account, labels []Label) Graph {
	b := &builder{
		nodes: map[string]*Node{}, byDNS: map[string]string{}, byRole: map[string]string{},
		accounts: map[string]Account{}, labels: map[string]Label{}, orgOf: map[string]string{}, orgMgmt: map[string]string{},
		edges: map[edgeKey]*Edge{},
	}
	for _, account := range accounts {
		b.accounts[account.ID] = account
	}
	for _, label := range labels {
		b.labels[label.AccountID] = label
	}
	type held struct {
		holder    string
		reader    string
		relations []model.Relation
	}
	var holders []held
	for _, resource := range resources {
		switch resource.Type {
		case "organization":
			if id := resource.Attributes[model.AttrOrganizationID]; id != "" {
				b.orgOf[resource.Account] = id
				if management := resource.Attributes[model.AttrManagementAccount]; management != "" {
					b.orgMgmt[id] = management
				}
			}
			continue
		case model.ResourcePolicyType:
			subject := resource.Attributes[model.AttrSubject]
			if subject == "" {
				continue
			}
			node := b.node(subject, func() Node {
				return Node{ID: subject, Account: resource.Account, Region: resource.Region, Service: "s3", Type: "bucket", Name: resource.Name}
			})
			node.ReadBy = appendUnique(node.ReadBy, resource.Account)
			holders = append(holders, held{holder: subject, reader: resource.Account, relations: resource.Relations})
			continue
		}
		owner := arnAccount(resource.ARN)
		if owner == "" {
			owner = resource.Account
		}
		node := b.node(resource.ARN, func() Node {
			return Node{ID: resource.ARN, Account: owner, Region: resource.Region, Service: resource.Service, Type: resource.Type, Name: resource.Name}
		})
		if resource.Account == owner {
			// The owner's own read names the resource best.
			node.Service, node.Type, node.Name, node.Region = resource.Service, resource.Type, resource.Name, resource.Region
		}
		node.ReadBy = appendUnique(node.ReadBy, resource.Account)
		if dns := resource.Attributes[model.AttrDNSName]; dns != "" {
			node.dns = model.NormalizeDNSName(dns)
			b.byDNS[node.dns] = node.ID
		}
		if key := roleKey(resource.ARN); key != "" && resource.Type == "role" {
			b.byRole[key] = node.ID
		}
		holders = append(holders, held{holder: resource.ARN, reader: resource.Account, relations: resource.Relations})
	}
	for _, item := range holders {
		for _, relation := range item.relations {
			b.link(item.holder, item.reader, relation)
		}
	}
	b.decide()
	b.collapse()
	return b.graph()
}

func (b *builder) node(id string, create func() Node) *Node {
	if node, ok := b.nodes[id]; ok {
		return node
	}
	made := create()
	b.nodes[id] = &made
	b.order = append(b.order, id)
	return &made
}

// target resolves a relation's target to a node, creating the dangling node
// a link ends on when the target was not read.
func (b *builder) target(relation model.Relation) *Node {
	target := relation.Target
	if relation.Type == model.RelationAliasOf || relation.Type == model.RelationOrigin {
		host := model.NormalizeDNSName(target)
		if id, ok := b.byDNS[host]; ok {
			return b.nodes[id]
		}
		if relation.Type == model.RelationOrigin && !strings.Contains(host, ".elb.") {
			return nil
		}
		return b.node("host:"+host, func() Node {
			return Node{ID: "host:" + host, Type: NodeHost, Name: host, Dangling: true, Description: hostDescription(host)}
		})
	}
	switch {
	case target == model.AnyPrincipal:
		return b.node("anyone", func() Node {
			return Node{ID: "anyone", Type: NodeAnyone, Name: "anyone", Dangling: true, Description: "anyone"}
		})
	case strings.HasPrefix(target, model.OrgTargetPrefix):
		id := strings.TrimPrefix(target, model.OrgTargetPrefix)
		return b.node(target, func() Node {
			return Node{ID: target, Type: NodeOrganization, Name: id, Dangling: true, Description: "organization " + id}
		})
	case accountPattern.MatchString(target):
		return b.node("account:"+target, func() Node {
			return Node{ID: "account:" + target, Account: target, Type: NodeAccount, Name: target, Dangling: true}
		})
	case uniqueIDPattern.MatchString(target):
		return b.node("principal:"+target, func() Node {
			return Node{ID: "principal:" + target, Type: NodePrincipal, Name: target, Dangling: true,
				Description: "a deleted or unknown principal"}
		})
	case strings.HasPrefix(target, "arn:"):
		if node, ok := b.nodes[target]; ok {
			return node
		}
		if id, ok := b.byRole[roleKey(target)]; ok {
			return b.nodes[id]
		}
		account := arnAccount(target)
		return b.node(target, func() Node {
			return Node{ID: target, Account: account, Type: arnType(target), Name: model.ResourceID(target), Dangling: true}
		})
	}
	return nil
}

func hostDescription(host string) string {
	if strings.HasSuffix(host, ".cloudfront.net") {
		return "a distribution in an account not connected"
	}
	return "a load balancer in an account not connected"
}

// arnType is the resource type an ARN names ("role", "vpc", "transit-gateway").
func arnType(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	resource := parts[5]
	if index := strings.IndexAny(resource, "/:"); index >= 0 {
		return resource[:index]
	}
	return parts[2]
}

func (b *builder) link(holder, reader string, relation model.Relation) {
	from := b.nodes[holder]
	to := b.target(relation)
	if from == nil || to == nil || from.ID == to.ID {
		return
	}
	kind := relation.Kind
	if kind == "" {
		kind = model.RelationConfigured
	}
	key := edgeKey{from: from.ID, to: to.ID, kind: relation.Type, how: kind, access: relation.Access, pattern: relation.Pattern}
	edge, ok := b.edges[key]
	if !ok {
		edge = &Edge{From: from.ID, To: to.ID, Type: relation.Type, Kind: kind, Source: relation.Source, Access: relation.Access,
			Pattern: relation.Pattern, Count: max(relation.Count, 1)}
		b.edges[key] = edge
		b.edgeOrder = append(b.edgeOrder, key)
	}
	edge.readBy = appendUnique(edge.readBy, reader)
	edge.Count = max(edge.Count, relation.Count)
}

// evidence reports whether account holds its end of a link. It must be
// connected, and its own reads must hold the matching end: for a trust, the
// trusted role; for a resource share, the share as received; for a policy
// grant, the account it names is its own end; otherwise either end of the
// link.
func (b *builder) evidence(account string, edge *Edge, from, to *Node, far bool) bool {
	if !b.accounts[account].Connected {
		return false
	}
	readTo := !to.Dangling && contains(to.ReadBy, account)
	if !far {
		return contains(from.ReadBy, account) || readTo
	}
	switch edge.Type {
	case model.RelationTrusts:
		return readTo && to.Type == "role"
	case model.RelationSharedVia:
		return contains(from.ReadBy, account)
	case model.RelationMayAccess:
		return (to.Type == NodeAccount && to.Account == account) || readTo
	}
	return contains(from.ReadBy, account) || readTo
}

// orgConfirmed reports whether a connected account reads itself as a
// member of the organization.
func (b *builder) orgConfirmed(org string) bool {
	for account, id := range b.orgOf {
		if id == org && b.accounts[account].Connected {
			return true
		}
	}
	return false
}

// decide sets the sides of every link that leaves an account, and the last
// read time of the side not seen now.
func (b *builder) decide() {
	for _, key := range b.edgeOrder {
		edge := b.edges[key]
		from, to := b.nodes[edge.From], b.nodes[edge.To]
		near, far := from.Account, to.Account
		switch {
		case to.Type == NodeOrganization:
			edge.Sides = OneSide
			if b.evidence(near, edge, from, to, false) && b.orgConfirmed(to.Name) {
				edge.Sides = BothSides
			}
			continue
		case far == "":
			edge.Sides = OneSide
			continue
		case near == far:
			continue
		}
		nearSeen, farSeen := b.evidence(near, edge, from, to, false), b.evidence(far, edge, from, to, true)
		if nearSeen && farSeen {
			edge.Sides = BothSides
			continue
		}
		edge.Sides = OneSide
		unseen := far
		if !nearSeen {
			unseen = near
		}
		edge.LastReadAt = b.accounts[unseen].LastReadAt
	}
}

// collapse merges wide grants: more than WideGrantThreshold links of one
// kind, access and resource pattern from resources of one type in one
// account to one principal become one link from a group node, with the
// count and the name prefix the resources share.
func (b *builder) collapse() {
	type groupKey struct {
		account, nodeType, to string
		kind                  model.RelationType
		access, pattern       string
		sides                 Sides
	}
	groups := map[groupKey][]edgeKey{}
	var order []groupKey
	for _, key := range b.edgeOrder {
		edge := b.edges[key]
		if edge.Kind != model.RelationKindMayAccess {
			continue
		}
		from := b.nodes[edge.From]
		group := groupKey{account: from.Account, nodeType: from.Type, to: edge.To, kind: edge.Type, access: edge.Access,
			pattern: edge.Pattern, sides: edge.Sides}
		if _, ok := groups[group]; !ok {
			order = append(order, group)
		}
		groups[group] = append(groups[group], key)
	}
	removed := map[edgeKey]bool{}
	for _, group := range order {
		keys := groups[group]
		if len(keys) <= WideGrantThreshold {
			continue
		}
		var members, names []string
		first := b.edges[keys[0]]
		readBy := []string{}
		var lastRead time.Time
		for _, key := range keys {
			edge := b.edges[key]
			members = append(members, edge.From)
			names = append(names, b.nodes[edge.From].Name)
			readBy = appendUnique(readBy, edge.readBy...)
			if edge.LastReadAt.After(lastRead) {
				lastRead = edge.LastReadAt
			}
			removed[key] = true
		}
		sort.Strings(members)
		pattern := commonPrefix(names) + "*"
		id := "group:" + group.account + "/" + group.nodeType + "/" + pattern + "/" + string(group.kind) + "/" + group.access + "/" + group.to
		node := b.node(id, func() Node {
			return Node{ID: id, Account: group.account, Type: NodeGroup, Name: group.nodeType + " matching " + pattern, Members: members}
		})
		node.ReadBy = appendUnique(node.ReadBy, readBy...)
		if group.pattern != "" {
			pattern = group.pattern
		}
		key := edgeKey{from: id, to: group.to, kind: group.kind, how: first.Kind, access: group.access, pattern: pattern}
		b.edges[key] = &Edge{From: id, To: group.to, Type: group.kind, Kind: first.Kind, Source: first.Source, Access: group.access,
			Pattern: pattern, Count: len(keys), Sides: group.sides, LastReadAt: lastRead, readBy: readBy}
		b.edgeOrder = append(b.edgeOrder, key)
	}
	if len(removed) == 0 {
		return
	}
	kept := b.edgeOrder[:0]
	for _, key := range b.edgeOrder {
		if removed[key] {
			delete(b.edges, key)
			continue
		}
		kept = append(kept, key)
	}
	b.edgeOrder = kept
}

// commonPrefix is the longest prefix every name shares, cut back to the
// last separator so the pattern reads as names do ("prod-orders-*", not
// "prod-orders-0*").
func commonPrefix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	prefix := names[0]
	for _, name := range names[1:] {
		for !strings.HasPrefix(name, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	if cut := strings.LastIndexAny(prefix, "-_./:"); cut >= 0 {
		return prefix[:cut+1]
	}
	return prefix
}

// labelFor names an account: connected, the management account of a
// connected account's organization, labelled or an AWS service account,
// or unknown. A user's name is kept whatever the label.
func (b *builder) labelFor(id string) (string, string) {
	name := b.accounts[id].Name
	if label, ok := b.labels[id]; ok && label.Name != "" {
		name = label.Name
	}
	switch {
	case b.accounts[id].Connected:
		return LabelConnected, name
	case b.sameOrg(id):
		return LabelSameOrg, name
	}
	if _, ok := b.labels[id]; ok {
		return LabelKnown, name
	}
	if service, ok := awsServiceAccounts[id]; ok {
		if name == "" {
			name = service
		}
		return LabelKnown, name
	}
	return LabelUnknown, name
}

func (b *builder) sameOrg(id string) bool {
	for org, management := range b.orgMgmt {
		if management == id && b.orgConfirmed(org) {
			return true
		}
	}
	return false
}

func (b *builder) graph() Graph {
	out := Graph{Nodes: make([]Node, 0, len(b.nodes)), Edges: make([]Edge, 0, len(b.edges)), Accounts: []AccountCard{}}
	cards := map[string]*AccountCard{}
	card := func(id string) *AccountCard {
		if existing, ok := cards[id]; ok {
			return existing
		}
		label, name := b.labelFor(id)
		made := &AccountCard{ID: id, Name: name, Label: label, Connected: b.accounts[id].Connected,
			LastReadAt: b.accounts[id].LastReadAt, OrganizationID: b.orgOf[id]}
		cards[id] = made
		return made
	}
	for id := range b.accounts {
		card(id)
	}
	for _, id := range b.order {
		node := *b.nodes[id]
		sort.Strings(node.ReadBy)
		if node.Account != "" {
			account := card(node.Account)
			if !node.Dangling && node.Type != NodeGroup {
				account.Resources++
			}
			if node.Dangling && node.Description == "" {
				node.Description = b.describe(node)
			}
		}
		out.Nodes = append(out.Nodes, node)
	}
	for _, key := range b.edgeOrder {
		out.Edges = append(out.Edges, *b.edges[key])
	}
	for _, item := range cards {
		out.Accounts = append(out.Accounts, *item)
	}
	sort.Slice(out.Nodes, func(left, right int) bool { return out.Nodes[left].ID < out.Nodes[right].ID })
	sortEdges(out.Edges)
	sort.Slice(out.Accounts, func(left, right int) bool { return out.Accounts[left].ID < out.Accounts[right].ID })
	return out
}

// describe says what a dangling node in a known account stands for.
func (b *builder) describe(node Node) string {
	what := "a resource"
	switch node.Type {
	case NodeAccount:
		what = "the account"
	case "role":
		what = "a role"
	case "user":
		what = "a user"
	case "vpc":
		what = "a network"
	case "transit-gateway":
		what = "a transit gateway"
	case "loadbalancer":
		what = "a load balancer"
	}
	if b.accounts[node.Account].Connected {
		return what + " not read in account " + node.Account
	}
	if node.Type == NodeAccount {
		return "account " + node.Account + " (not connected)"
	}
	return what + " in account " + node.Account + " (not connected)"
}

func sortEdges(edges []Edge) {
	sort.Slice(edges, func(left, right int) bool {
		a, b := edges[left], edges[right]
		switch {
		case a.From != b.From:
			return a.From < b.From
		case a.To != b.To:
			return a.To < b.To
		case a.Type != b.Type:
			return a.Type < b.Type
		case a.Access != b.Access:
			return a.Access < b.Access
		}
		return a.Pattern < b.Pattern
	})
}

func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		if value != "" && !contains(list, value) {
			list = append(list, value)
		}
	}
	return list
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
