package estate

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/graph"
	"github.com/VersusControl/versus-incident/pkg/aws/grouping"
	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// Topology levels.
const (
	LevelAccounts = "accounts"
	LevelGroups   = "groups"
	LevelFocus    = "focus"
)

// Lenses: which links a view draws. Changes marks what changed in the last
// day and draws every link.
const (
	LensNetwork  = "network"
	LensIdentity = "identity"
	LensData     = "data"
	LensChanges  = "changes"
)

// Node kinds in a topology answer.
const (
	NodeKindAccount  = "account"
	NodeKindGroup    = "group"
	NodeKindResource = "resource"
	// NodeKindOther is a link end the reads cannot see: a host name, an
	// organization, a deleted principal, anyone, or a fold of wide grants.
	NodeKindOther = "other"
)

// Link seen-from values. Inside one account a link has none.
const (
	SeenBoth  = "both"
	SeenOne   = "one"
	SeenMixed = "mixed"
)

// lensTypes are the relation types each link lens draws.
var lensTypes = map[string]map[model.RelationType]bool{
	LensNetwork: {model.RelationInVPC: true, model.RelationInSubnet: true, model.RelationSecurityGroup: true, model.RelationRoutesTo: true,
		model.RelationAttachedTo: true, model.RelationAttachedToTransit: true, model.RelationSharedVia: true, model.RelationAliasOf: true,
		model.RelationOrigin: true},
	LensIdentity: {model.RelationTrusts: true, model.RelationMayAccess: true},
	LensData: {model.RelationTriggers: true, model.RelationTriggeredBy: true, model.RelationSubscribes: true, model.RelationTargets: true,
		model.RelationDeadLetter: true},
}

var topologyAccountPattern = regexp.MustCompile(`^[0-9]{12}$`)

// TopologyQuery selects one view. Level empty picks Accounts with two or
// more connected accounts and the one account's Groups otherwise. Group,
// at the Groups level, opens one group. Ref is the resource a Focus view
// starts from; Hops is 1 to 3. Lenses empty draws every link.
type TopologyQuery struct {
	Level   string
	Account string
	Group   string
	Ref     string
	Hops    int
	Lenses  []string
	// Accounts and Region scope the view, like the Changes scope bar:
	// every count is over them. Account, when set, must be one of
	// Accounts if those are named.
	Accounts []string
	Region   string
}

// Valid refuses a query value the topology does not know.
func (query TopologyQuery) Valid() error {
	if err := (Scope{Accounts: query.Accounts, Region: query.Region}).Valid(); err != nil {
		return err
	}
	if query.Account != "" && len(query.Accounts) > 0 && !slices.Contains(query.Accounts, query.Account) {
		return ErrInvalidQuery
	}
	switch query.Level {
	case "", LevelAccounts, LevelGroups, LevelFocus:
	default:
		return ErrInvalidQuery
	}
	if query.Account != "" && !topologyAccountPattern.MatchString(query.Account) {
		return ErrInvalidQuery
	}
	if len(query.Group) > 512 || len(query.Ref) > 2048 || query.Group != "" && !grouping.ValidID(query.Group) {
		return ErrInvalidQuery
	}
	if query.Hops < 0 || query.Hops > graph.MaxHops {
		return ErrInvalidQuery
	}
	if query.Level == LevelFocus && query.Ref == "" {
		return ErrInvalidQuery
	}
	for _, lens := range query.Lenses {
		if _, ok := lensTypes[lens]; !ok && lens != LensChanges {
			return ErrInvalidQuery
		}
	}
	return nil
}

// TopologyNode is one card or dot. A null count was not read; a *_capped
// count is a lower bound. Read is set on connected accounts. Hidden counts
// the neighbours the per-hop fan-out cap left out of a Focus view.
type TopologyNode struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	Account     string         `json:"account,omitempty"`
	Type        string         `json:"type,omitempty"`
	Service     string         `json:"service,omitempty"`
	Region      string         `json:"region,omitempty"`
	Group       string         `json:"group,omitempty"`
	Label       string         `json:"label,omitempty"`
	Role        string         `json:"role,omitempty"`
	RoleDetail  string         `json:"role_detail,omitempty"`
	Rule        string         `json:"rule,omitempty"`
	Key         string         `json:"key,omitempty"`
	Renamed     bool           `json:"renamed,omitempty"`
	Read        string         `json:"read,omitempty"`
	NotReadWhy  string         `json:"not_read_reason,omitempty"`
	Description string         `json:"description,omitempty"`
	Dangling    bool           `json:"dangling,omitempty"`
	NotRead     []string       `json:"not_read,omitempty"`
	ByType      map[string]int `json:"by_type,omitempty"`
	LastReadAt  time.Time      `json:"last_read_at,omitzero"`

	Resources        *int `json:"resources,omitempty"`
	ResourcesCapped  bool `json:"resources_capped,omitempty"`
	OpenIssues       *int `json:"open_issues,omitempty"`
	OpenIssuesCapped bool `json:"open_issues_capped,omitempty"`
	ActNow           *int `json:"act_now,omitempty"`
	ActNowCapped     bool `json:"act_now_capped,omitempty"`
	WorthALook       *int `json:"worth_a_look,omitempty"`
	WorthALookCapped bool `json:"worth_a_look_capped,omitempty"`
	// Health and TopIssue are set on resources in a Focus view.
	Health   *NodeHealth `json:"health,omitempty"`
	TopIssue *TopIssue   `json:"top_issue,omitempty"`
	// LastChange is the newest change of the last day on a Focus
	// resource. The feed page is newest first, so one it holds is exact.
	// LastChangeKnown is set on a resource of a connected account with
	// no LastChange: true when the page was read, complete and settled,
	// so there was no change in the window; false when the page was cut
	// off, the window is not settled, or the feed could not be read.
	LastChange      *LastChange `json:"last_change,omitempty"`
	LastChangeKnown *bool       `json:"last_change_known,omitempty"`
	Changes24h      *int        `json:"changes_24h,omitempty"`
	ChangesCapped   bool        `json:"changes_capped,omitempty"`
	Hidden          int         `json:"hidden,omitempty"`
}

// Health states of a node. A status from the snapshots (ok, warning,
// critical, unknown) is the latest signal on it; none means its health
// was read and no signal names it; not_read means its health source was
// not read.
const (
	HealthNone    = "none"
	HealthNotRead = "not_read"
)

// NodeHealth is the latest health signal on a resource.
type NodeHealth struct {
	State   string    `json:"state"`
	Summary string    `json:"summary,omitempty"`
	Since   time.Time `json:"since,omitzero"`
}

// TopIssue is the open issue that leads on a resource: Act now first, then
// queue order. CauseText is set when a likely cause was found.
type TopIssue struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Severity  issues.Severity `json:"severity"`
	Tier      issues.Tier     `json:"tier"`
	CauseText string          `json:"cause_text,omitempty"`
	// CauseRef is the node the likely cause changed, when it names a
	// resource: a node of the view when one matches, else its ARN.
	CauseRef string `json:"cause_ref,omitempty"`
}

// TopologyLink is one link, or the links of one kind between two cards
// summed. Kind is configured, may_access or inferred. SeenFrom is both,
// one or mixed on a link between accounts, empty inside one. BothSides
// counts the summed links seen from both sides. CountCapped is set when
// the account that read the link was not read in full.
type TopologyLink struct {
	From        string             `json:"from"`
	To          string             `json:"to"`
	Type        model.RelationType `json:"type"`
	Kind        model.RelationKind `json:"kind"`
	Source      string             `json:"source,omitempty"`
	Access      string             `json:"access,omitempty"`
	Pattern     string             `json:"pattern,omitempty"`
	SeenFrom    string             `json:"seen_from,omitempty"`
	Count       int                `json:"count"`
	CountCapped bool               `json:"count_capped,omitempty"`
	// Sources breaks a summed link's Count down by the AWS read that found
	// the links under it ("kms:GetKeyPolicy": 2).
	Sources    map[string]int `json:"sources,omitempty"`
	BothSides  int            `json:"both_sides"`
	LastReadAt time.Time      `json:"last_read_at,omitzero"`
}

// ScopeHint says how to narrow a view that holds too many nodes.
type ScopeHint struct {
	Text  string `json:"text"`
	Level string `json:"level,omitempty"`
	Hops  int    `json:"hops,omitempty"`
}

// Topology is one view. When TooMany, Nodes and Links are empty and Hint
// says how to narrow the scope; NodeCount is then a lower bound.
type Topology struct {
	Enabled  bool     `json:"enabled"`
	Level    string   `json:"level"`
	Account  string   `json:"account,omitempty"`
	Group    string   `json:"group,omitempty"`
	Accounts []string `json:"accounts"`
	Region   string   `json:"region,omitempty"`
	// Regions are the Regions the scope reads, as every AWS page counts
	// them.
	Regions      []string              `json:"regions"`
	Ref          string                `json:"ref,omitempty"`
	Hops         int                   `json:"hops,omitempty"`
	Lenses       []string              `json:"lenses"`
	Nodes        []TopologyNode        `json:"nodes"`
	Links        []TopologyLink        `json:"links"`
	NodeCount    int                   `json:"node_count"`
	MaxNodes     int                   `json:"max_nodes"`
	TooMany      bool                  `json:"too_many"`
	Hint         *ScopeHint            `json:"hint,omitempty"`
	FanOutCapped bool                  `json:"fan_out_capped"`
	Warning      *snapshot.WarningLine `json:"warning"`
}

// topologyRead is what one view draws from.
type topologyRead struct {
	estateRead
	query   TopologyQuery
	keep    func(graph.Edge) bool
	groupOf map[string]string
	nodes   map[string]graph.Node
	roles   map[string]role
}

// Topology reads one view of the topology from the stores: the committed
// inventory, the grouping with the user's corrections, the account labels
// and the open issues. A view holds at most graph.MaxNodes nodes; beyond
// that it is TooMany with a hint, never a cut graph.
func (s *Service) Topology(ctx context.Context, query TopologyQuery) (Topology, error) {
	out := Topology{Lenses: []string{}, Nodes: []TopologyNode{}, Links: []TopologyLink{}, MaxNodes: graph.MaxNodes, Regions: []string{}}
	if err := query.Valid(); err != nil {
		return out, err
	}
	if s.snapshots == nil {
		return out, nil
	}
	ctx = s.snapshots.ReadContext(ctx)
	read, err := s.read(ctx, Scope{Accounts: query.Accounts, Region: query.Region})
	if err != nil || !read.enabled {
		return out, err
	}
	out.Enabled, out.Warning = true, read.coverage.Warning()
	out.Accounts, out.Region, out.Regions = append([]string{}, query.Accounts...), query.Region, read.regions
	if query.Level == "" {
		query.Level = LevelAccounts
		if len(read.accounts) == 1 {
			query.Level, query.Account = LevelGroups, read.accounts[0].scope.Account
		}
	}
	if query.Level == LevelGroups && query.Account == "" {
		if len(read.accounts) != 1 {
			return out, ErrInvalidQuery
		}
		query.Account = read.accounts[0].scope.Account
	}
	if query.Level == LevelFocus && query.Hops == 0 {
		query.Hops = 1
	}
	if query.Account != "" && read.byID[query.Account] == nil {
		return out, ErrNotFound
	}
	sort.Strings(query.Lenses)
	query.Lenses = slices.Compact(query.Lenses)
	out.Level, out.Account, out.Group, out.Ref, out.Hops = query.Level, query.Account, query.Group, query.Ref, query.Hops
	out.Lenses = append(out.Lenses, query.Lenses...)

	view := &topologyRead{estateRead: read, query: query, keep: lensFilter(query.Lenses), groupOf: read.groups.GroupOf(),
		nodes: make(map[string]graph.Node, len(read.graph.Nodes)), roles: roles(read.graph, read.byID)}
	for _, node := range read.graph.Nodes {
		view.nodes[node.ID] = node
	}
	switch {
	case query.Level == LevelAccounts:
		err = view.accountsLevel(&out)
	case query.Level == LevelGroups && query.Group != "":
		err = view.groupLevel(&out)
	case query.Level == LevelGroups:
		err = view.groupsLevel(&out)
	default:
		err = view.focusLevel(&out)
	}
	if err != nil {
		return out, err
	}
	if out.TooMany {
		out.Nodes, out.Links = []TopologyNode{}, []TopologyLink{}
		return out, nil
	}
	if err := s.fillCounts(ctx, view, &out); err != nil {
		return out, err
	}
	if query.Level == LevelFocus {
		if err := s.fillHealth(ctx, view, &out); err != nil {
			return out, err
		}
	}
	return out, nil
}

// globalHealthRegion is the Region whose health read answers for a
// global resource (IAM, the S3 bucket list, CloudFront, Route 53): AWS
// Health reports global events there.
const globalHealthRegion = "us-east-1"

// fillHealth puts on each resource of a Focus view the latest health
// signal the snapshots hold for it, read once per connected account in the
// view. Health is known per account and Region: none when a health source
// of the resource's Region was read and nothing names it, not read when
// none was; a global resource is answered by the account's us-east-1
// health read.
func (s *Service) fillHealth(ctx context.Context, view *topologyRead, out *Topology) error {
	type accountHealth struct {
		read   map[string]bool
		latest map[string]model.Health
	}
	byAccount := map[string]*accountHealth{}
	for index := range out.Nodes {
		node := &out.Nodes[index]
		if node.Kind != NodeKindResource || node.Account == "" {
			continue
		}
		health := byAccount[node.Account]
		if health == nil && view.byID[node.Account] != nil {
			result, err := s.snapshots.Health(ctx, snapshot.Query{Account: node.Account})
			if err != nil {
				return err
			}
			health = &accountHealth{read: map[string]bool{}, latest: map[string]model.Health{}}
			for _, unit := range result.Units {
				if collect.KindOf(unit.Unit.Service) == collect.KindHealth && (!unit.Sync.LastGoodAt.IsZero() || unit.HasData) {
					health.read[unit.Unit.Region] = true
				}
			}
			for _, signal := range result.Health {
				if signal.Resource == "" || !view.scope.inRegion(signal.Region) {
					continue
				}
				if current, ok := health.latest[signal.Resource]; !ok || signal.Since.After(current.Since) {
					health.latest[signal.Resource] = signal
				}
			}
			byAccount[node.Account] = health
		}
		region := node.Region
		if region == "" || region == model.GlobalRegion {
			region = globalHealthRegion
		}
		switch {
		case health == nil || !health.read[region]:
			node.Health = &NodeHealth{State: HealthNotRead}
		default:
			node.Health = &NodeHealth{State: HealthNone}
			if signal, ok := health.latest[node.ID]; ok {
				node.Health = &NodeHealth{State: string(signal.Status), Summary: signal.Summary, Since: signal.Since}
			}
		}
	}
	return nil
}

// accountCounts are one connected account's open issues and last-day
// changes, read from its own stores.
type accountCounts struct {
	open         int
	actNow       int
	worth        int
	tiers        map[string]int
	top          map[string]issues.Issue
	issues       map[string]int
	issuesCapped bool
	issuesRead   bool
	changes      map[string]int
	changeTotal  int
	changeCapped bool
	changesRead  bool
	latest       map[string]LastChange
	// latestComplete says the page behind latest was read, complete and
	// settled.
	latestComplete bool
}

// fillCounts reads, for each connected account a view shows, its own open
// issues (the same summary as Overview) and, under the changes lens, its
// last-day changes, and puts them on the account card, its groups and its
// resources. The reads are bounded by the accounts in the view. A count
// that cannot be read, or belongs to an account that is not connected, is
// left out (not read), never shown as zero.
func (s *Service) fillCounts(ctx context.Context, view *topologyRead, out *Topology) error {
	counts := map[string]*accountCounts{}
	// Only a view that shows an account's groups or resources needs its
	// issues one by one; an account card needs its summary alone.
	placed := map[string]bool{}
	for _, node := range out.Nodes {
		if node.Kind == NodeKindGroup || node.Kind == NodeKindResource {
			placed[node.Account] = true
		}
	}
	for _, node := range out.Nodes {
		if node.Account == "" || view.byID[node.Account] == nil || counts[node.Account] != nil {
			continue
		}
		account := &accountCounts{}
		counts[node.Account] = account
		if s.issues != nil {
			summary, err := s.issues.Summary(ctx, issues.Filter{Account: node.Account, Region: view.scope.Region}, view.coverage)
			if err != nil {
				return err
			}
			var queue []issues.Issue
			if placed[node.Account] {
				if queue, err = s.issues.Queue(ctx, issues.Filter{Account: node.Account, Region: view.scope.Region}, issueReadLimit+1); err != nil {
					return err
				}
			}
			if len(queue) > issueReadLimit {
				queue, account.issuesCapped = queue[:issueReadLimit], true
			}
			account.open, account.actNow, account.worth = summary.Open, summary.ActNow(), summary.WorthALook
			account.issues, account.issuesRead = map[string]int{}, true
			account.tiers, account.top = map[string]int{}, map[string]issues.Issue{}
			for _, issue := range queue {
				group := issue.GroupID
				if group == "" {
					group = view.groupOf[issue.Owner]
				}
				tier := "act:"
				if issue.Tier == issues.TierWorthALook {
					tier = "look:"
				}
				if group != "" {
					account.issues[groupNodeID(issue.Account, group)]++
					account.tiers[tier+groupNodeID(issue.Account, group)]++
				}
				account.issues[issue.Owner]++
				if top, ok := account.top[issue.Owner]; !ok || top.Tier == issues.TierWorthALook && issue.Tier != issues.TierWorthALook {
					account.top[issue.Owner] = issue
				}
			}
		}
		// The changes lens counts changes; a Focus view also names each
		// resource's last change. Both come from one page per account.
		if slices.Contains(view.query.Lenses, LensChanges) || view.query.Level == LevelFocus {
			feed := s.changeCounts(ctx, node.Account, view.scope.Region)
			account.changes, account.changeTotal, account.changeCapped = feed.perResource, feed.total, feed.capped
			account.changesRead = feed.read && slices.Contains(view.query.Lenses, LensChanges)
			account.latest, account.latestComplete = feed.latest, feed.read && feed.complete
		}
	}
	for index := range out.Nodes {
		node := &out.Nodes[index]
		account := counts[node.Account]
		if account == nil {
			continue
		}
		switch {
		case node.Kind == NodeKindAccount && node.Read != "":
			if account.issuesRead {
				open, act, worth := account.open, account.actNow, account.worth
				node.OpenIssues, node.ActNow, node.WorthALook = &open, &act, &worth
			}
			if account.changesRead {
				total := account.changeTotal
				node.Changes24h, node.ChangesCapped = &total, account.changeCapped
			}
		case node.Kind == NodeKindGroup:
			if account.issuesRead {
				open, act, worth := account.issues[node.ID], account.tiers["act:"+node.ID], account.tiers["look:"+node.ID]
				node.OpenIssues, node.OpenIssuesCapped = &open, account.issuesCapped
				node.ActNow, node.ActNowCapped, node.WorthALook, node.WorthALookCapped = &act, account.issuesCapped, &worth, account.issuesCapped
			}
			if account.changesRead {
				node.Changes24h, node.ChangesCapped = account.changeCount(view.membersOf(node.Account, node.Group)...)
			}
		case node.Kind == NodeKindResource:
			if account.issuesRead {
				open := account.issues[node.ID]
				node.OpenIssues, node.OpenIssuesCapped = &open, account.issuesCapped
				if top, ok := account.top[node.ID]; ok && view.query.Level == LevelFocus {
					node.TopIssue = &TopIssue{ID: top.ID, Title: top.Title, Severity: top.Severity, Tier: top.Tier}
					if top.Cause != nil && top.Cause.Text != "" {
						node.TopIssue.CauseText = top.Cause.Text
					}
					if top.Cause.Found() && top.Cause.Resource != "" {
						node.TopIssue.CauseRef = causeRef(top.Cause.Resource, out.Nodes)
					}
				}
			}
			if account.changesRead {
				node.Changes24h, node.ChangesCapped = account.changeCount(node.ID)
			}
			if view.query.Level == LevelFocus {
				if last, ok := account.latest[changes.ResourceKey(node.ID)]; ok {
					node.LastChange = &last
				} else {
					known := account.latestComplete
					node.LastChangeKnown = &known
				}
			}
		}
	}
	return nil
}

// causeRef is the node of a view a likely cause's resource names: the node
// with its ARN, or one with the same resource key (a role with or without
// its path, a function with a version); else the ARN itself.
func causeRef(resource string, nodes []TopologyNode) string {
	key := changes.ResourceKey(resource)
	for _, node := range nodes {
		if node.ID == resource {
			return node.ID
		}
	}
	for _, node := range nodes {
		if key != "" && changes.ResourceKey(node.ID) == key {
			return node.ID
		}
	}
	return resource
}

func (account *accountCounts) changeCount(arns ...string) (*int, bool) {
	count := 0
	for _, arn := range arns {
		if key := changes.ResourceKey(arn); key != "" {
			count += account.changes[key]
		}
	}
	return &count, account.changeCapped
}

// membersOf lists the members of one group of an account.
func (view *topologyRead) membersOf(account, id string) []string {
	for _, group := range view.groups.Of(account) {
		if group.ID == id {
			return group.Members
		}
	}
	return nil
}

// lensFilter admits the links the lenses draw; nil draws every link.
func lensFilter(lenses []string) func(graph.Edge) bool {
	types := map[model.RelationType]bool{}
	for _, lens := range lenses {
		for relation := range lensTypes[lens] {
			types[relation] = true
		}
	}
	if len(types) == 0 {
		return nil
	}
	return func(edge graph.Edge) bool { return types[edge.Type] }
}

// filtered is the graph with only the links the lenses draw.
func (view *topologyRead) filtered() graph.Graph {
	if view.keep == nil {
		return view.graph
	}
	out := view.graph
	out.Edges = make([]graph.Edge, 0, len(view.graph.Edges))
	for _, edge := range view.graph.Edges {
		if view.keep(edge) {
			out.Edges = append(out.Edges, edge)
		}
	}
	return out
}

// partialRead reports whether account's reads are lower bounds.
func (view *topologyRead) partialRead(account string) bool {
	state := view.byID[account]
	return state != nil && state.read != ReadFull
}

func seenFrom(count, both int) string {
	switch {
	case both == 0:
		return SeenOne
	case both >= count:
		return SeenBoth
	}
	return SeenMixed
}

// accountNode is the card of an account: connected, with its read state
// and counts, or named by a link, with its label.
func (view *topologyRead) accountNode(card graph.AccountCard) TopologyNode {
	node := TopologyNode{ID: graph.AccountEndpointPrefix + card.ID, Kind: NodeKindAccount, Name: card.Name, Account: card.ID, Label: card.Label,
		LastReadAt: card.LastReadAt}
	if node.Name == "" {
		node.Name = card.ID
	}
	state := view.byID[card.ID]
	if state == nil {
		return node
	}
	node.Role, node.RoleDetail, node.Read = view.roles[card.ID].name, view.roles[card.ID].detail, state.read
	node.NotReadWhy = state.reason
	node.Resources, node.ResourcesCapped = state.resourceCount()
	return node
}

func (view *topologyRead) accountsLevel(out *Topology) error {
	summary := graph.Accounts(view.filtered())
	out.NodeCount = len(summary.Accounts)
	if out.NodeCount > graph.MaxNodes {
		out.TooMany = true
		out.Hint = &ScopeHint{Text: "Too many accounts to draw at once; open one account's groups.", Level: LevelGroups}
		return nil
	}
	// A scope draws its accounts and what their links reach.
	drawn := map[string]bool{}
	var links []graph.AccountLink
	for _, link := range summary.Links {
		if !view.selected[link.From] && !view.selected[link.To] {
			continue
		}
		links = append(links, link)
		drawn[link.From], drawn[link.To] = true, true
	}
	for _, card := range summary.Accounts {
		if view.selected[card.ID] || drawn[card.ID] {
			out.Nodes = append(out.Nodes, view.accountNode(card))
		}
	}
	others := map[string]bool{}
	for _, link := range links {
		to := graph.AccountEndpointPrefix + link.To
		switch {
		case link.To == "":
			to = "other:" + link.Far
			if !others[to] {
				others[to] = true
				out.Nodes = append(out.Nodes, TopologyNode{ID: to, Kind: NodeKindOther, Name: link.Far, Description: link.Far, Dangling: true})
			}
		case strings.HasPrefix(link.To, model.OrgTargetPrefix):
			to = link.To
			if !others[to] {
				others[to] = true
				out.Nodes = append(out.Nodes, TopologyNode{ID: to, Kind: NodeKindOther, Type: graph.NodeOrganization,
					Name: strings.TrimPrefix(link.To, model.OrgTargetPrefix), Description: "organization " + strings.TrimPrefix(link.To, model.OrgTargetPrefix)})
			}
		}
		item := TopologyLink{From: graph.AccountEndpointPrefix + link.From, To: to, Type: link.Type, Kind: link.Kind,
			Source: link.Source, SeenFrom: seenFrom(link.Count, link.BothSides), Count: link.Count, BothSides: link.BothSides,
			CountCapped: view.partialRead(link.From), LastReadAt: link.LastReadAt}
		if link.Source != "" {
			item.Sources = map[string]int{link.Source: link.Count}
		}
		out.Links = append(out.Links, item)
	}
	out.NodeCount = len(out.Nodes)
	if out.NodeCount > graph.MaxNodes {
		out.TooMany = true
		out.Hint = &ScopeHint{Text: "Too many accounts to draw at once; open one account's groups.", Level: LevelGroups}
	}
	return nil
}

// groupNodeID is a group card's ID in a view: unique across accounts.
func groupNodeID(account, group string) string { return "group:" + account + "/" + group }

// endpointNode is the card a summed group link ends on outside the
// account's groups.
func (view *topologyRead) endpointNode(end, far string) (string, TopologyNode) {
	switch {
	case end == "" && far != "":
		id := "other:" + far
		return id, TopologyNode{ID: id, Kind: NodeKindOther, Name: far, Description: far, Dangling: true}
	case strings.HasPrefix(end, graph.AccountEndpointPrefix):
		account := strings.TrimPrefix(end, graph.AccountEndpointPrefix)
		card := graph.AccountCard{ID: account, Label: graph.LabelUnknown}
		for _, known := range view.graph.Accounts {
			if known.ID == account {
				card = known
			}
		}
		return end, view.accountNode(card)
	case strings.HasPrefix(end, model.OrgTargetPrefix):
		name := strings.TrimPrefix(end, model.OrgTargetPrefix)
		return end, TopologyNode{ID: end, Kind: NodeKindOther, Type: graph.NodeOrganization, Name: name, Description: "organization " + name}
	}
	return "", TopologyNode{}
}

func (view *topologyRead) groupCard(group grouping.Group) TopologyNode {
	state := view.byID[group.Account]
	count := len(group.Members)
	node := TopologyNode{ID: groupNodeID(group.Account, group.ID), Kind: NodeKindGroup, Name: group.Name, Account: group.Account,
		Group: group.ID, Rule: group.Rule, Key: group.Key, Renamed: group.Renamed, ByType: group.ByType,
		Resources: &count, ResourcesCapped: state != nil && state.read != ReadFull}
	return node
}

func (view *topologyRead) groupsLevel(out *Topology) error {
	account := view.query.Account
	state := view.byID[account]
	groups := view.groups.Of(account)
	summary := graph.Groups(view.filtered(), account, groups)
	if state.read == ReadNone {
		// Nothing read: one card that says so, never an empty account.
		out.Nodes = append(out.Nodes, view.accountNode(graph.AccountCard{ID: account, Name: state.scope.Name, Label: graph.LabelConnected,
			Connected: true}))
		out.NodeCount = 1
		return nil
	}
	if len(groups) > graph.MaxNodes {
		out.NodeCount, out.TooMany = len(groups), true
		out.Hint = &ScopeHint{Text: "Too many groups to draw at once; search for a resource and focus on it.", Level: LevelFocus, Hops: 1}
		return nil
	}
	seen := map[string]bool{}
	for _, group := range groups {
		node := view.groupCard(group)
		seen[node.ID] = true
		out.Nodes = append(out.Nodes, node)
	}
	end := func(id, far string) string {
		if id != "" && grouping.ValidID(id) && !strings.HasPrefix(id, graph.AccountEndpointPrefix) && !strings.HasPrefix(id, model.OrgTargetPrefix) {
			node := groupNodeID(account, id)
			if !seen[node] && id == grouping.UngroupedID {
				seen[node] = true
				count := 0
				out.Nodes = append(out.Nodes, TopologyNode{ID: node, Kind: NodeKindGroup, Name: "Ungrouped", Account: account, Group: id,
					Resources: &count})
			}
			return node
		}
		nodeID, node := view.endpointNode(id, far)
		if nodeID != "" && !seen[nodeID] {
			seen[nodeID] = true
			out.Nodes = append(out.Nodes, node)
		}
		return nodeID
	}
	for _, link := range summary.Links {
		from, to := end(link.From, ""), end(link.To, link.Far)
		if from == "" || to == "" {
			continue
		}
		cross := !strings.HasPrefix(from, "group:") || !strings.HasPrefix(to, "group:")
		item := TopologyLink{From: from, To: to, Type: link.Type, Kind: link.Kind, Count: link.Count, BothSides: link.BothSides,
			CountCapped: state.read != ReadFull, Sources: link.Sources}
		if cross {
			item.SeenFrom = seenFrom(link.Count, link.BothSides)
		}
		out.Links = append(out.Links, item)
	}
	out.NodeCount = len(out.Nodes)
	if out.NodeCount > graph.MaxNodes {
		out.TooMany = true
		out.Hint = &ScopeHint{Text: "Too many groups to draw at once; search for a resource and focus on it.", Level: LevelFocus, Hops: 1}
	}
	return nil
}

// resourceNode is one graph node as a view draws it.
func (view *topologyRead) resourceNode(node graph.Node, hidden int) TopologyNode {
	out := TopologyNode{ID: node.ID, Kind: NodeKindResource, Name: node.Name, Account: node.Account, Type: node.Type, Service: node.Service,
		Region: node.Region, Group: view.groupOf[node.ID], Description: node.Description, Dangling: node.Dangling, NotRead: node.NotRead,
		Hidden: hidden}
	switch {
	case node.Type == graph.NodeAccount:
		out.Kind = NodeKindAccount
		for _, card := range view.graph.Accounts {
			if card.ID == node.Account {
				out.Label = card.Label
			}
		}
	case node.Dangling || node.Type == graph.NodeGroup || node.Type == graph.NodeHost || node.Type == graph.NodeOrganization ||
		node.Type == graph.NodeAnyone || node.Type == graph.NodePrincipal:
		out.Kind = NodeKindOther
	}
	return out
}

func (view *topologyRead) edgeLink(edge graph.Edge) TopologyLink {
	from := view.nodes[edge.From]
	link := TopologyLink{From: edge.From, To: edge.To, Type: edge.Type, Kind: edge.Kind, Source: edge.Source, Access: edge.Access,
		Pattern: edge.Pattern, Count: max(edge.Count, 1), LastReadAt: edge.LastReadAt, CountCapped: view.partialRead(from.Account)}
	switch edge.Sides {
	case graph.BothSides:
		link.SeenFrom, link.BothSides = SeenBoth, link.Count
	case graph.OneSide:
		link.SeenFrom = SeenOne
	}
	return link
}

// groupLevel opens one group: its members, the links between them, and
// the links to other groups and accounts summed per card.
func (view *topologyRead) groupLevel(out *Topology) error {
	account := view.query.Account
	var group *grouping.Group
	for _, candidate := range view.groups.Of(account) {
		if candidate.ID == view.query.Group {
			group = &candidate
			break
		}
	}
	if group == nil {
		return ErrNotFound
	}
	if len(group.Members) > graph.MaxNodes {
		out.NodeCount, out.TooMany = len(group.Members), true
		out.Hint = &ScopeHint{Text: "Too many resources in this group to draw at once; search for one and focus on it.", Level: LevelFocus, Hops: 1}
		return nil
	}
	members := map[string]bool{}
	for _, member := range group.Members {
		members[member] = true
		if node, ok := view.nodes[member]; ok {
			out.Nodes = append(out.Nodes, view.resourceNode(node, 0))
		}
	}
	seen := map[string]bool{}
	summed := map[[4]string]int{}
	sources := map[[4]string]map[string]int{}
	var order [][4]string
	for _, edge := range view.filtered().Edges {
		fromIn, toIn := members[edge.From], members[edge.To]
		if !fromIn && !toIn {
			continue
		}
		if fromIn && toIn {
			out.Links = append(out.Links, view.edgeLink(edge))
			continue
		}
		outside, inside := edge.To, edge.From
		if toIn {
			outside, inside = edge.From, edge.To
		}
		far := view.cardOf(account, view.nodes[outside], seen, out)
		key := [4]string{inside, far, string(edge.Type), string(edge.Kind)}
		if !fromIn {
			key = [4]string{far, inside, string(edge.Type), string(edge.Kind)}
		}
		if _, ok := summed[key]; !ok {
			order = append(order, key)
		}
		summed[key] += max(edge.Count, 1)
		if edge.Source != "" {
			if sources[key] == nil {
				sources[key] = map[string]int{}
			}
			sources[key][edge.Source] += max(edge.Count, 1)
		}
	}
	for _, key := range order {
		out.Links = append(out.Links, TopologyLink{From: key[0], To: key[1], Type: model.RelationType(key[2]), Kind: model.RelationKind(key[3]),
			Count: summed[key], CountCapped: view.partialRead(account), Sources: sources[key]})
	}
	out.NodeCount = len(out.Nodes)
	if out.NodeCount > graph.MaxNodes {
		out.TooMany = true
		out.Hint = &ScopeHint{Text: "Too many links out of this group to draw at once; pick a lens or focus on one resource.", Level: LevelFocus, Hops: 1}
	}
	return nil
}

// cardOf is the card a link out of an opened group ends on: another group
// of the account, the other account, or what the reads cannot see.
func (view *topologyRead) cardOf(account string, node graph.Node, seen map[string]bool, out *Topology) string {
	add := func(item TopologyNode) string {
		if !seen[item.ID] {
			seen[item.ID] = true
			out.Nodes = append(out.Nodes, item)
		}
		return item.ID
	}
	if node.Account == account && !node.Dangling {
		id := view.groupOf[node.ID]
		if id == "" {
			id = grouping.UngroupedID
		}
		for _, group := range view.groups.Of(account) {
			if group.ID == id {
				return add(view.groupCard(group))
			}
		}
		return add(TopologyNode{ID: groupNodeID(account, id), Kind: NodeKindGroup, Name: "Ungrouped", Account: account, Group: id})
	}
	if node.Account != "" && node.Account != account {
		_, item := view.endpointNode(graph.AccountEndpointPrefix+node.Account, "")
		return add(item)
	}
	return add(view.resourceNode(node, 0))
}

// focusLevel walks from one resource, at most Hops links, at most
// graph.MaxFanOut new neighbours per node per hop and graph.MaxNodes nodes.
func (view *topologyRead) focusLevel(out *Topology) error {
	if _, ok := view.nodes[view.query.Ref]; !ok {
		return ErrNotFound
	}
	focus := graph.Focus(view.graph, view.query.Ref, view.query.Hops, graph.MaxNodes, graph.MaxFanOut, view.keep)
	out.NodeCount = focus.Reached
	if focus.TooMany {
		out.TooMany = true
		out.Hint = &ScopeHint{Text: "More than 300 resources within reach; draw fewer hops or pick a lens.", Level: LevelFocus, Hops: focus.FitsHops}
		return nil
	}
	out.FanOutCapped = len(focus.Hidden) > 0
	for _, node := range focus.Graph.Nodes {
		out.Nodes = append(out.Nodes, view.resourceNode(node, focus.Hidden[node.ID]))
	}
	for _, edge := range focus.Graph.Edges {
		out.Links = append(out.Links, view.edgeLink(edge))
	}
	return nil
}
