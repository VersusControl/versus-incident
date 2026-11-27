package estate

import (
	"context"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/graph"
	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// Overview bounds.
const (
	// MaxNeedsAttention is how many issues Overview lists.
	MaxNeedsAttention = 5
	// MaxPeopleChanges is how many changes by people Overview lists.
	MaxPeopleChanges = 3
)

// OverviewSummary is the summary card. A null count was not read. A
// *_capped count is a lower bound: "more than N, not all read".
// GroupedShare is the percentage (0-100) of resources placed in a group
// rather than Ungrouped; null when nothing was read or no resource was.
// Act now and Worth a look are capped when an account in scope was not
// read in full: its issues are not all known.
type OverviewSummary struct {
	AccountsRead        int    `json:"accounts_read"`
	AccountsTotal       int    `json:"accounts_total"`
	ActNow              *int   `json:"act_now"`
	ActNowCapped        bool   `json:"act_now_capped"`
	WorthALook          *int   `json:"worth_a_look"`
	WorthALookCapped    bool   `json:"worth_a_look_capped"`
	Resources           *int   `json:"resources"`
	ResourcesCapped     bool   `json:"resources_capped"`
	GroupedShare        *int   `json:"grouped_share"`
	GroupedShareCapped  bool   `json:"grouped_share_capped"`
	PeopleChanges24h    *int64 `json:"people_changes_24h"`
	PeopleChangesCapped bool   `json:"people_changes_capped"`
}

// AttentionRow is one issue Overview lists.
type AttentionRow struct {
	ID          string          `json:"id"`
	Severity    issues.Severity `json:"severity"`
	Tier        issues.Tier     `json:"tier"`
	Kind        issues.Kind     `json:"kind"`
	Title       string          `json:"title"`
	Account     string          `json:"account"`
	AccountName string          `json:"account_name,omitempty"`
	Region      string          `json:"region,omitempty"`
	Owner       string          `json:"owner"`
	// Where is a short plain place: "cluster ci-eks", "bucket logs".
	Where      string    `json:"where"`
	Since      time.Time `json:"since,omitzero"`
	SinceKnown bool      `json:"since_known"`
	OpenedAt   time.Time `json:"opened_at"`
}

// AccountRow is one connected account. Read is read, partial or not_read;
// NotReadReason says why when not_read, and why a partial account is
// partial while its first read runs (first_read_in_progress). Resources is
// null when not read and
// capped when partial. OpenIssues is null when the issues are not
// available. Role is hub, spoke or empty.
type AccountRow struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Role            string `json:"role"`
	RoleDetail      string `json:"role_detail,omitempty"`
	Read            string `json:"read"`
	NotReadReason   string `json:"not_read_reason,omitempty"`
	Resources       *int   `json:"resources"`
	ResourcesCapped bool   `json:"resources_capped"`
	OpenIssues      *int   `json:"open_issues"`
}

// OtherAccounts counts the accounts links name that are not connected, and
// how many of them are unknown.
type OtherAccounts struct {
	Count   int `json:"count"`
	Unknown int `json:"unknown"`
}

// PeopleChange is one change by a person that touched exposure or identity.
type PeopleChange struct {
	ID          string          `json:"id"`
	Time        time.Time       `json:"time"`
	Account     string          `json:"account"`
	AccountName string          `json:"account_name,omitempty"`
	Region      string          `json:"region"`
	Concern     changes.Concern `json:"concern"`
	EventName   string          `json:"event_name"`
	Summary     string          `json:"summary"`
	Actor       string          `json:"actor"`
	Resources   []string        `json:"resources"`
}

// Overview is the AWS Overview page.
type Overview struct {
	Enabled bool `json:"enabled"`
	// Organization is set when DescribeOrganization was read.
	Organization *Organization         `json:"organization,omitempty"`
	AsOf         time.Time             `json:"as_of"`
	Summary      OverviewSummary       `json:"summary"`
	Warning      *snapshot.WarningLine `json:"warning"`
	// Regions are the Regions the scope reads, as every AWS page counts
	// them.
	Regions        []string       `json:"regions"`
	NeedsAttention []AttentionRow `json:"needs_attention"`
	Accounts       []AccountRow   `json:"accounts"`
	OtherAccounts  OtherAccounts  `json:"other_accounts"`
	PeopleChanges  []PeopleChange `json:"people_changes"`
}

// Overview reads the Overview page over a scope. Issue counts come from
// the same summary as the Issues page, the warning from the same coverage
// read, and the changes by people from the same change feed as the Changes
// page; with accounts named, each is read per account and summed.
func (s *Service) Overview(ctx context.Context, scope Scope) (Overview, error) {
	out := Overview{AsOf: s.now().UTC(), NeedsAttention: []AttentionRow{}, Accounts: []AccountRow{}, PeopleChanges: []PeopleChange{}, Regions: []string{}}
	if s.snapshots == nil {
		return out, scope.Valid()
	}
	ctx = s.snapshots.ReadContext(ctx)
	read, err := s.read(ctx, scope)
	if err != nil || !read.enabled {
		return out, err
	}
	out.Enabled = true
	out.Organization = read.organization
	out.Warning = read.coverage.Warning()
	out.Regions = read.regions
	out.Summary.AccountsRead, out.Summary.AccountsTotal = read.coverage.AccountsRead, read.coverage.AccountsTotal

	names := map[string]string{}
	for _, state := range read.byID {
		names[state.scope.Account] = state.scope.Name
	}
	accountIssues := map[string]issues.Summary{}
	if s.issues != nil {
		actNow, worth := 0, 0
		var queue []issues.Issue
		for _, filter := range scopeFilters(scope) {
			summary, err := s.issues.Summary(ctx, filter, read.coverage)
			if err != nil {
				return out, err
			}
			if filter.Account != "" {
				accountIssues[filter.Account] = summary
			}
			actNow, worth = actNow+summary.ActNow(), worth+summary.WorthALook
			rows, err := s.issues.Attention(ctx, filter, MaxNeedsAttention)
			if err != nil {
				return out, err
			}
			queue = append(queue, rows...)
		}
		out.Summary.ActNow, out.Summary.WorthALook = &actNow, &worth
		sortAttention(queue)
		out.NeedsAttention = attention(queue, names, read.where)
	}

	roleOf := roles(read.graph, read.byID)
	total, grouped, anyRead, capped := 0, 0, false, false
	for _, state := range read.accounts {
		account := state.scope.Account
		row := AccountRow{ID: account, Name: state.scope.Name, Role: roleOf[account].name, RoleDetail: roleOf[account].detail, Read: state.read}
		row.NotReadReason = state.reason
		if state.read == ReadNone {
			capped = true
		} else {
			anyRead = true
			total += state.resources
		}
		row.Resources, row.ResourcesCapped = state.resourceCount()
		capped = capped || row.ResourcesCapped
		if s.issues != nil {
			summary, ok := accountIssues[account]
			if !ok {
				if summary, err = s.issues.Summary(ctx, issues.Filter{Account: account, Region: scope.Region}, read.coverage); err != nil {
					return out, err
				}
			}
			open := summary.Open
			row.OpenIssues = &open
		}
		out.Accounts = append(out.Accounts, row)
	}
	for _, group := range read.groups.Groups {
		if group.ID != groupingUngrouped && read.selected[group.Account] && read.byID[group.Account] != nil && read.byID[group.Account].read != ReadNone {
			grouped += len(group.Members)
		}
	}
	// Issues come from the reads: with an account in scope not read in
	// full they are a lower bound. A scope with no account has nothing to
	// count, and its zero is exact.
	if capped {
		out.Summary.ActNowCapped, out.Summary.WorthALookCapped = out.Summary.ActNow != nil, out.Summary.WorthALook != nil
	}
	if anyRead {
		out.Summary.Resources, out.Summary.ResourcesCapped = &total, capped
		// A share of no resources is not a number: it stays null.
		if total > 0 {
			share := (grouped*100 + total/2) / total
			out.Summary.GroupedShare, out.Summary.GroupedShareCapped = &share, capped
		}
	}
	out.OtherAccounts = otherAccounts(read)
	s.peopleChanges(ctx, &out, names, scope)
	return out, nil
}

// scopeFilters are the issue filters a scope reads: one per account it
// names, or one over every account.
func scopeFilters(scope Scope) []issues.Filter {
	if len(scope.Accounts) == 0 {
		return []issues.Filter{{Region: scope.Region}}
	}
	out := make([]issues.Filter, 0, len(scope.Accounts))
	seen := map[string]bool{}
	for _, account := range scope.Accounts {
		if !seen[account] {
			seen[account] = true
			out = append(out, issues.Filter{Account: account, Region: scope.Region})
		}
	}
	return out
}

// otherAccounts counts the accounts not connected that the scope's
// accounts link to, and how many of them are unknown.
func otherAccounts(read estateRead) OtherAccounts {
	labels := map[string]string{}
	for _, card := range read.graph.Accounts {
		labels[card.ID] = card.Label
	}
	seen := map[string]bool{}
	var out OtherAccounts
	count := func(id string) {
		if id == "" || read.byID[id] != nil || seen[id] {
			return
		}
		if _, known := labels[id]; !known {
			return
		}
		seen[id] = true
		out.Count++
		if labels[id] == graph.LabelUnknown {
			out.Unknown++
		}
	}
	for _, link := range graph.Accounts(read.graph).Links {
		switch {
		case read.selected[link.From]:
			count(link.To)
		case read.selected[link.To]:
			count(link.From)
		}
	}
	return out
}

// sortAttention orders issues read per account as one queue: Act now
// first, then the most severe, the newest, and by ID.
func sortAttention(list []issues.Issue) {
	sort.SliceStable(list, func(left, right int) bool {
		a, b := list[left], list[right]
		if (a.Tier == issues.TierWorthALook) != (b.Tier == issues.TierWorthALook) {
			return b.Tier == issues.TierWorthALook
		}
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		if !a.OpenedAt.Equal(b.OpenedAt) {
			return a.OpenedAt.After(b.OpenedAt)
		}
		return a.ID < b.ID
	})
}

// attention shapes the needs-attention rows, already Act now first.
func attention(queue []issues.Issue, names map[string]string, where func(issues.Issue) string) []AttentionRow {
	out := make([]AttentionRow, 0, MaxNeedsAttention)
	for _, issue := range queue {
		if len(out) == MaxNeedsAttention {
			break
		}
		out = append(out, AttentionRow{ID: issue.ID, Severity: issue.Severity, Tier: issue.Tier, Kind: issue.Kind, Title: issue.Title,
			Account: issue.Account, AccountName: names[issue.Account], Region: issue.Region, Owner: issue.Owner, Where: where(issue),
			Since: issue.Since, SinceKnown: issue.SinceKnown, OpenedAt: issue.OpenedAt})
	}
	return out
}

// peopleChanges reads the last day's changes by people from the change
// feed: the count the Changes page shows, and the newest that touched
// exposure or identity, over the scope. A feed that cannot be read leaves
// both null.
func (s *Service) peopleChanges(ctx context.Context, out *Overview, names map[string]string, scope Scope) {
	if s.feed == nil {
		return
	}
	accounts := []string{""}
	if len(scope.Accounts) > 0 {
		accounts = nil
		for _, filter := range scopeFilters(scope) {
			accounts = append(accounts, filter.Account)
		}
	}
	var count int64
	capped := false
	var items []snapshot.FeedItem
	for _, account := range accounts {
		answer, err := s.feed.Answer(ctx, snapshot.FeedRequest{Window: snapshot.FeedWindowDay, ActorClass: string(model.ActorPerson),
			Account: account, Region: scope.Region, Limit: snapshot.MaxFeedLimit})
		if err != nil {
			return
		}
		count += answer.Summary.ByActorClass[string(model.ActorPerson)]
		capped = capped || answer.Summary.TotalIsLowerBound
		items = append(items, answer.Items...)
	}
	out.Summary.PeopleChanges24h, out.Summary.PeopleChangesCapped = &count, capped
	sort.SliceStable(items, func(left, right int) bool { return items[left].Time.After(items[right].Time) })
	for _, item := range items {
		if len(out.PeopleChanges) == MaxPeopleChanges {
			break
		}
		concern := changes.ConcernOf(item.Service, item.EventName)
		if concern == "" || item.ActorClass != string(model.ActorPerson) {
			continue
		}
		resources := item.Resources
		if resources == nil {
			resources = []string{}
		}
		out.PeopleChanges = append(out.PeopleChanges, PeopleChange{ID: item.ID, Time: item.Time, Account: item.Account,
			AccountName: names[item.Account], Region: item.Region, Concern: concern, EventName: item.EventName, Summary: item.Summary,
			Actor: item.Actor, Resources: resources})
	}
}
