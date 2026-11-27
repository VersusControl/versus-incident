// Package estate answers the AWS Overview and Topology: one method per
// capability, which the admin routes and the agent tools share. It reads
// the committed snapshots, the issue store, the change feed, the account
// labels and the group corrections. It never calls AWS.
//
// Every count it returns says whether it is known, capped (a lower bound:
// "more than N, not all read") or not read (null), so an account whose
// inventory was never read is never shown as empty.
package estate

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/graph"
	"github.com/VersusControl/versus-incident/pkg/aws/grouping"
	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// ScopeFunc lists the connected accounts with the Regions they are read
// in, from settings. enabled is false when the AWS connector is off. It is
// the same list the coverage page reads, and it is read once per request.
type ScopeFunc func(ctx context.Context) (expected []snapshot.ScopeAccount, enabled bool, err error)

// Errors a caller maps onto its answer.
var (
	// ErrInvalidQuery is a topology query with a value it does not know.
	ErrInvalidQuery = errors.New("estate: invalid query")
	// ErrNotFound is an account, group or resource the stored reads do
	// not hold.
	ErrNotFound = errors.New("estate: not found")
)

// Account read states.
const (
	// ReadFull: every source of the account read.
	ReadFull = snapshot.AccountRead
	// ReadPartial: the account read, but some sources did not, or were
	// capped; its counts are lower bounds.
	ReadPartial = snapshot.AccountPartial
	// ReadNone: nothing was read from the account; its counts are not
	// known and are null.
	ReadNone = snapshot.AccountNotRead
)

// Account roles on the network.
const (
	// RoleHub owns a transit gateway other connected accounts attach to.
	RoleHub = "hub"
	// RoleSpoke attaches to a hub account's transit gateway.
	RoleSpoke = "spoke"
)

// issueReadLimit bounds the open issues one page reads to place on groups
// and resources; past it those counts are capped.
const issueReadLimit = 2000

// Service answers Overview and Topology from the stores.
type Service struct {
	snapshots   *snapshot.Store
	scope       ScopeFunc
	issues      *issues.Actions
	feed        *snapshot.ChangeFeed
	labels      storage.AWSAccountLabelStore
	corrections storage.AWSGroupCorrectionStore
	org         tenancy.OrgScope
	now         func() time.Time
}

// Option configures a Service.
type Option func(*Service)

// WithIssues reads issue counts and the needs-attention list from actions.
// Without it those counts are null.
func WithIssues(actions *issues.Actions) Option { return func(s *Service) { s.issues = actions } }

// WithChangeFeed reads the changes by people from feed. Without it those
// counts are null.
func WithChangeFeed(feed *snapshot.ChangeFeed) Option { return func(s *Service) { s.feed = feed } }

// WithAccountLabels names the accounts a user labelled.
func WithAccountLabels(store storage.AWSAccountLabelStore) Option {
	return func(s *Service) { s.labels = store }
}

// WithGroupCorrections applies the user's renames, merges and moves to the
// groups.
func WithGroupCorrections(store storage.AWSGroupCorrectionStore) Option {
	return func(s *Service) { s.corrections = store }
}

// WithClock sets the time Overview reads at.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// New builds the service over the snapshot store, under one org scope.
// A nil store answers enabled=false.
func New(snapshots *snapshot.Store, org tenancy.OrgScope, scope ScopeFunc, options ...Option) *Service {
	service := &Service{snapshots: snapshots, scope: scope, org: org.Normalized(), now: time.Now}
	for _, option := range options {
		option(service)
	}
	return service
}

// Scope narrows a page to some connected accounts and one Region, like
// the scope bar of the Changes page. Empty fields read everything. Every
// count a page gives is over the scope.
type Scope struct {
	Accounts []string
	Region   string
}

// MaxScopeAccounts bounds how many accounts one scope names.
const MaxScopeAccounts = 50

var (
	scopeAccountPattern = regexp.MustCompile(`^[0-9]{12}$`)
	scopeRegionPattern  = regexp.MustCompile(`^[a-z]{2}(-gov|-iso[a-z]*)?-[a-z]+-[0-9]$`)
)

// Valid refuses a scope with a malformed account or Region, or too many
// accounts: the same rules as the coverage page.
func (scope Scope) Valid() error {
	if len(scope.Accounts) > MaxScopeAccounts {
		return ErrInvalidQuery
	}
	for _, account := range scope.Accounts {
		if !scopeAccountPattern.MatchString(account) {
			return ErrInvalidQuery
		}
	}
	if scope.Region != "" && !scopeRegionPattern.MatchString(scope.Region) {
		return ErrInvalidQuery
	}
	return nil
}

// inRegion reports whether something in region falls in the scope's
// Region: the global Region and account-wide reads serve every Region.
func (scope Scope) inRegion(region string) bool {
	return scope.Region == "" || region == scope.Region || region == "" || region == model.GlobalRegion
}

// The graph and grouping builds every page read makes; a search, which
// reads far less, must make neither.
var (
	buildGraph  = graph.Build
	buildGroups = grouping.Build
)

// accountState is one connected account as the stored reads hold it.
type accountState struct {
	scope     snapshot.ScopeAccount
	read      string
	reason    string
	resources int
}

// estateRead is one request's read of the stores: the coverage, the
// resources, the graph and the groups, all filtered by one answer of the
// connected accounts.
type estateRead struct {
	enabled bool
	scope   Scope
	// selected are the connected accounts the scope names, every one when
	// it names none.
	selected map[string]bool
	// organization is the organization the reads found, when one was read.
	organization *Organization
	coverage     issues.CoverageRead
	accounts     []*accountState
	byID         map[string]*accountState
	graph        graph.Graph
	groups       grouping.Result
	// resources counts the grouped resources per account.
	resources map[string]int
	// regions are the Regions the scope reads, from the coverage read.
	regions []string
	// kept are the connected accounts' stored resources in the scope's
	// Region.
	kept []model.Resource
	// nodeIndex holds the graph's nodes by ID.
	nodeIndex map[string]graph.Node
}

// where names an issue's place in a few plain words: the type and name of
// the resource it is on ("cluster ci-eks"), else its group, else the
// account.
func (read estateRead) where(issue issues.Issue) string {
	if node, ok := read.nodeIndex[issue.Owner]; ok && node.Type != "" && node.Name != "" {
		return strings.ReplaceAll(node.Type, "-", " ") + " " + node.Name
	}
	if strings.HasPrefix(issue.Owner, "arn:") {
		parts := strings.SplitN(issue.Owner, ":", 6)
		if len(parts) == 6 {
			kind := parts[5]
			if index := strings.IndexAny(kind, "/:"); index >= 0 {
				kind = kind[:index]
			} else {
				kind = parts[2]
			}
			return strings.ReplaceAll(kind, "-", " ") + " " + model.ResourceID(issue.Owner)
		}
	}
	group := issue.GroupID
	if group == "" {
		group = read.groups.GroupOf()[issue.Owner]
	}
	for _, candidate := range read.groups.Of(issue.Account) {
		if candidate.ID == group && group != grouping.UngroupedID {
			return "group " + candidate.Name
		}
	}
	return "the account"
}

// Organization is the AWS organization the connected accounts belong to,
// as DescribeOrganization answered. AWS gives an organization no name, so
// Name is its ID.
type Organization struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	ManagementAccount string `json:"management_account,omitempty"`
}

// read reads everything one page needs from the stores, once, over the
// scope. The graph holds every connected account's reads in the scope's
// Region, so a link to an account outside the scope keeps its sides; the
// counts and the account rows hold the scope's accounts only. ctx must
// carry the snapshot store's read filter. An account the scope names that
// is not connected is ErrNotFound.
func (s *Service) read(ctx context.Context, scope Scope) (estateRead, error) {
	out := estateRead{byID: map[string]*accountState{}, resources: map[string]int{}, scope: scope, selected: map[string]bool{}}
	if err := scope.Valid(); err != nil {
		return out, err
	}
	if s.snapshots == nil || s.scope == nil {
		return out, nil
	}
	expected, enabled, err := s.scope(ctx)
	if err != nil || !enabled {
		return out, err
	}
	out.enabled = true
	connectedIDs := map[string]bool{}
	for _, entry := range expected {
		connectedIDs[entry.Account] = true
	}
	for _, account := range scope.Accounts {
		if !connectedIDs[account] {
			return out, ErrNotFound
		}
		out.selected[account] = true
	}
	if len(out.selected) == 0 {
		for account := range connectedIDs {
			out.selected[account] = true
		}
	}
	inScope := make([]snapshot.ScopeAccount, 0, len(expected))
	for _, entry := range expected {
		if out.selected[entry.Account] {
			inScope = append(inScope, entry)
		}
	}
	view, err := s.snapshots.Coverage(ctx, snapshot.CoverageScope{Region: scope.Region, Expected: inScope, ExpectedKnown: true})
	if err != nil {
		return out, err
	}
	out.coverage = issues.CoverageFromView(view)
	result, err := s.snapshots.Resources(ctx, snapshot.Query{PolicyEntries: true})
	if err != nil {
		return out, err
	}
	states := map[string]snapshot.CoverageAccount{}
	for _, state := range view.Accounts {
		states[state.Account] = state
	}
	if len(inScope) < len(expected) {
		// Accounts outside the scope still hold the far ends of its
		// links: their states come from the same read over every account.
		every, err := s.snapshots.Coverage(ctx, snapshot.CoverageScope{Region: scope.Region, Expected: expected, ExpectedKnown: true})
		if err != nil {
			return out, err
		}
		for _, state := range every.Accounts {
			if _, ok := states[state.Account]; !ok {
				states[state.Account] = state
			}
		}
	}
	out.regions = view.Regions
	accounts := make([]graph.Account, 0, len(expected))
	for _, entry := range expected {
		// The coverage read decides every account's state, so Overview,
		// Topology, Coverage and Changes agree. An account outside the
		// scope is not in it: its state is not read.
		state := &accountState{scope: entry, read: ReadNone, reason: snapshot.AccountNotReadYet}
		if covered, ok := states[entry.Account]; ok {
			state.read, state.reason = covered.Read, covered.Reason
		}
		if out.selected[entry.Account] {
			out.accounts = append(out.accounts, state)
		}
		out.byID[entry.Account] = state
		accounts = append(accounts, graph.Account{ID: entry.Account, Name: entry.Name, Connected: true, LastReadAt: entry.LastReadAt})
	}
	sort.Slice(out.accounts, func(left, right int) bool {
		return out.accounts[left].scope.Account < out.accounts[right].scope.Account
	})
	labels, err := s.accountLabels(ctx)
	if err != nil {
		return out, err
	}
	// Only the connected accounts' reads are drawn: rows the store still
	// holds for an account removed from settings are left out, so its
	// links read from the other side stay, seen from one side.
	kept := make([]model.Resource, 0, len(result.Resources))
	for _, resource := range result.Resources {
		if _, ok := out.byID[resource.Account]; !ok || !scope.inRegion(resource.Region) {
			continue
		}
		if resource.Type == "organization" && out.organization == nil && out.selected[resource.Account] {
			if id := resource.Attributes[model.AttrOrganizationID]; id != "" {
				out.organization = &Organization{ID: id, Name: id, ManagementAccount: resource.Attributes[model.AttrManagementAccount]}
			}
		}
		kept = append(kept, resource)
	}
	out.kept = kept
	out.graph = buildGraph(kept, accounts, labels)
	out.nodeIndex = make(map[string]graph.Node, len(out.graph.Nodes))
	for _, node := range out.graph.Nodes {
		out.nodeIndex[node.ID] = node
	}
	corrections, err := s.groupCorrections(ctx)
	if err != nil {
		return out, err
	}
	connected := make([]string, 0, len(expected))
	for _, entry := range expected {
		connected = append(connected, entry.Account)
	}
	out.groups = buildGroups(kept, nil, corrections, grouping.Options{Accounts: connected})
	for _, group := range out.groups.Groups {
		out.resources[group.Account] += len(group.Members)
	}
	for _, state := range out.accounts {
		state.resources = out.resources[state.scope.Account]
	}
	return out, nil
}

func (s *Service) accountLabels(ctx context.Context) ([]graph.Label, error) {
	if s.labels == nil {
		return nil, nil
	}
	stored, err := s.labels.AWSAccountLabels(ctx, s.org)
	if err != nil {
		return nil, err
	}
	out := make([]graph.Label, 0, len(stored))
	for _, label := range stored {
		out = append(out, graph.Label{AccountID: label.AccountID, Label: label.Label, Name: label.Name})
	}
	return out, nil
}

func (s *Service) groupCorrections(ctx context.Context) ([]grouping.Correction, error) {
	if s.corrections == nil {
		return nil, nil
	}
	stored, err := s.corrections.AWSGroupCorrections(ctx, s.org, "")
	if err != nil {
		return nil, err
	}
	out := make([]grouping.Correction, 0, len(stored))
	for _, correction := range stored {
		out = append(out, grouping.Correction{Account: correction.AccountID, Kind: correction.Kind, GroupID: correction.GroupID,
			MemberARN: correction.MemberARN, TargetGroupID: correction.TargetGroupID, Name: correction.Name})
	}
	return out, nil
}

// resourceCount is an account's resource count: null when nothing was
// read, capped when only part was.
func (state *accountState) resourceCount() (*int, bool) {
	if state.read == ReadNone {
		return nil, false
	}
	count := state.resources
	return &count, state.read == ReadPartial
}

// role is an account's place on the network and the relation that made
// it so.
type role struct {
	name, detail string
}

// roles names each connected account's place on the network: the hub
// owns a transit gateway another connected account attaches to, and a
// spoke attaches to one.
func roles(g graph.Graph, connected map[string]*accountState) map[string]role {
	out := map[string]role{}
	links := graph.Accounts(g).Links
	for _, link := range links {
		if link.Type != model.RelationAttachedToTransit || link.To == "" || connected[link.To] == nil || connected[link.From] == nil {
			continue
		}
		out[link.To] = role{name: RoleHub, detail: "transit gateway"}
	}
	for _, link := range links {
		if link.Type != model.RelationAttachedToTransit || out[link.To].name != RoleHub || connected[link.From] == nil ||
			out[link.From].name == RoleHub {
			continue
		}
		hub := connected[link.To].scope.Name
		if hub == "" {
			hub = link.To
		}
		out[link.From] = role{name: RoleSpoke, detail: "attached to the transit gateway in " + hub}
	}
	return out
}

// LastChange is the latest change of the last day touching a resource.
type LastChange struct {
	Time      time.Time `json:"time"`
	Actor     string    `json:"actor"`
	EventName string    `json:"event_name"`
}

// accountChanges is one account's changes of the last day, from one page
// of the change feed: per resource (by resource key) the count and the
// latest change, and the total.
type accountChanges struct {
	perResource map[string]int
	latest      map[string]LastChange
	total       int
	capped      bool
	// complete is set when the page held every change of a settled
	// window: a resource with no change in it had none.
	complete bool
	read     bool
}

// changeCounts reads one account's changes of the last day from one page
// of the change feed. The total counts every change of the account, with
// or without a resource. capped is set when the feed holds more than the
// page, or its window was not read in full; read is false when the feed
// cannot be read.
func (s *Service) changeCounts(ctx context.Context, account, region string) accountChanges {
	if s.feed == nil {
		return accountChanges{}
	}
	answer, err := s.feed.Answer(ctx, snapshot.FeedRequest{Window: snapshot.FeedWindowDay, Account: account, Region: region,
		Limit: snapshot.MaxFeedLimit})
	if err != nil {
		return accountChanges{}
	}
	out := accountChanges{perResource: map[string]int{}, latest: map[string]LastChange{}, read: true,
		capped:   answer.NextCursor != nil || answer.Summary.TotalIsLowerBound,
		complete: answer.NextCursor == nil && !answer.Summary.TotalIsLowerBound && answer.Complete}
	for _, item := range answer.Items {
		if item.Account != account {
			continue
		}
		weight := 1
		if item.Count != nil && *item.Count > 1 {
			weight = int(*item.Count)
		}
		out.total += weight
		for _, arn := range item.Resources {
			key := changes.ResourceKey(arn)
			if key == "" {
				continue
			}
			out.perResource[key] += weight
			if last, ok := out.latest[key]; !ok || item.Time.After(last.Time) {
				out.latest[key] = LastChange{Time: item.Time, Actor: item.Actor, EventName: item.EventName}
			}
		}
	}
	return out
}

// groupingUngrouped is the group that holds what no rule placed.
const groupingUngrouped = grouping.UngroupedID
