// Package grouping sorts the resources of each AWS account into groups for
// the Topology page, by a ladder of rules where the first rule that fits a
// resource wins:
//
//  1. a correction the user made (rename, merge, move);
//  2. a learned application tag;
//  3. an AppRegistry application, or a CloudFormation stack that is not a
//     landing-zone baseline;
//  4. an EKS cluster and namespace, an ECS cluster, or an Auto Scaling
//     group;
//  5. a shared name prefix;
//  6. the VPC, when the account's remaining resources span more than one;
//  7. Ungrouped, which is always a group of its own.
//
// An EKS cluster's node groups, nodes and controller-made resources join
// the cluster group (or its namespace group) before the tag rung: their
// tags describe the node pool or the controller, not an application.
//
// Group IDs are "rule:key" (tag:app=checkout, stack:baseline,
// eks:prod-eks/ns:search, prefix:checkout-, vpc:vpc-0abc), built from
// what a group is, never from its members, so replacing every node keeps
// every ID and a correction keyed on an ID survives re-reads. A correction
// whose group no longer exists is reported, never applied to another
// group.
//
// The package reads what the stores hold and makes no AWS call.
package grouping

import (
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Rules, as a group card names them.
const (
	RuleUser        = "user"
	RuleTag         = "tag"
	RuleAppRegistry = "appregistry"
	RuleStack       = "stack"
	RuleCluster     = "cluster"
	RuleNamespace   = "namespace"
	RuleASG         = "asg"
	RulePrefix      = "prefix"
	RuleVPC         = "vpc"
	RuleUngrouped   = "ungrouped"
)

// UngroupedID is the ID of every account's Ungrouped group.
const UngroupedID = "ungrouped"

// UserGroupPrefix starts the ID of a group a user made by moving a
// resource into a group that did not exist.
const UserGroupPrefix = "user:"

// idPrefixes are the rule prefixes a group ID starts with.
var idPrefixes = []string{"tag:", "appregistry:", "stack:", "eks:", "ecs:", "asg:", "prefix:", "vpc:", UserGroupPrefix}

// ValidID reports whether id has the form of a group ID the ladder or a
// user can make.
func ValidID(id string) bool {
	if id == UngroupedID {
		return true
	}
	for _, prefix := range idPrefixes {
		if rest, ok := strings.CutPrefix(id, prefix); ok {
			return strings.TrimSpace(rest) != ""
		}
	}
	return false
}

// Group is one group of resources in one account. Rule names the rule that
// made it and Key what the rule matched (the tag key, the stack, the
// cluster, the namespace, the prefix, the VPC). Members are resource ARNs,
// sorted; ByType counts them by resource type.
type Group struct {
	ID      string         `json:"id"`
	Account string         `json:"account"`
	Name    string         `json:"name"`
	Rule    string         `json:"rule"`
	Key     string         `json:"key,omitempty"`
	Renamed bool           `json:"renamed,omitempty"`
	Members []string       `json:"members"`
	ByType  map[string]int `json:"by_type"`
}

// Correction kinds.
const (
	CorrectionRename = "rename"
	CorrectionMerge  = "merge"
	CorrectionMove   = "move"
)

// Correction is one correction a user made: rename GroupID to Name, merge
// GroupID into TargetGroupID, or move MemberARN into TargetGroupID.
type Correction struct {
	Account       string
	Kind          string
	GroupID       string
	MemberARN     string
	TargetGroupID string
	Name          string
}

// CorrectionStatus is what became of one correction on this read.
type CorrectionStatus string

const (
	// CorrectionApplied: the groups and the member it names exist, and it
	// was applied.
	CorrectionApplied CorrectionStatus = "applied"
	// CorrectionNoLongerMatches: a group or the member it names is gone;
	// it was not applied, and never to another group.
	CorrectionNoLongerMatches CorrectionStatus = "no_longer_matches"
)

// Result is one grouping: every account's groups, and the status of each
// correction in the order given.
type Result struct {
	Groups      []Group            `json:"groups"`
	Corrections []CorrectionStatus `json:"corrections"`
}

// Options tune a grouping.
type Options struct {
	// Accounts are the connected accounts. A stack name present in every
	// one of them is a baseline. Empty means the accounts the resources
	// come from.
	Accounts []string
}

// groupable reports a resource the grouping places: a stored resource, not
// a policy entry kept beside one.
func groupable(resource model.Resource) bool {
	return resource.ARN != "" && resource.Account != "" && resource.Type != model.ResourcePolicyType
}

// placement is where the ladder put one resource.
type placement struct {
	id, name, rule, key string
}

// Build sorts the resources into groups, account by account. learned are
// the application tag keys per account; nil learns them from resources.
// corrections are applied in the order given, after the ladder.
func Build(resources []model.Resource, learned TagKeys, corrections []Correction, options Options) Result {
	if learned == nil {
		learned = Learn(resources)
	}
	byAccount := map[string][]model.Resource{}
	seen := map[string]bool{}
	for _, resource := range resources {
		if !groupable(resource) || seen[resource.ARN] {
			continue
		}
		seen[resource.ARN] = true
		byAccount[resource.Account] = append(byAccount[resource.Account], resource)
	}
	baseline := baselineStacks(byAccount, options.Accounts)
	result := Result{Groups: []Group{}, Corrections: make([]CorrectionStatus, len(corrections))}
	accounts := make([]string, 0, len(byAccount))
	for account := range byAccount {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	for index := range result.Corrections {
		result.Corrections[index] = CorrectionNoLongerMatches
	}
	for _, account := range accounts {
		members := byAccount[account]
		sort.Slice(members, func(left, right int) bool { return members[left].ARN < members[right].ARN })
		placed := ladder(members, learned[account], baseline)
		var mine []int
		for index, correction := range corrections {
			if correction.Account == account {
				mine = append(mine, index)
			}
		}
		groups := correct(account, members, placed, corrections, mine, result.Corrections)
		result.Groups = append(result.Groups, groups...)
	}
	return result
}

// baselineStacks are the stack names that never make a group: landing-zone
// stacks, and a name every connected account has when there are two or
// more.
func baselineStacks(byAccount map[string][]model.Resource, connected []string) map[string]bool {
	if len(connected) == 0 {
		for account := range byAccount {
			connected = append(connected, account)
		}
	}
	accountsWith := map[string]map[string]bool{}
	out := map[string]bool{}
	for account, resources := range byAccount {
		for _, resource := range resources {
			name := stackOf(resource)
			if name == "" {
				continue
			}
			if landingZoneStack(name) {
				out[name] = true
			}
			if accountsWith[name] == nil {
				accountsWith[name] = map[string]bool{}
			}
			accountsWith[name][account] = true
		}
	}
	if len(connected) < 2 {
		return out
	}
	for name, holders := range accountsWith {
		everywhere := true
		for _, account := range connected {
			if !holders[account] {
				everywhere = false
				break
			}
		}
		if everywhere {
			out[name] = true
		}
	}
	return out
}

// ladder places one account's resources by the rules below corrections.
// members are sorted by ARN.
func ladder(members []model.Resource, families []string, baseline map[string]bool) []placement {
	placed := make([]placement, len(members))
	byKey := map[string]int{}
	eksClusters := map[string]string{}
	for index, resource := range members {
		byKey[resource.ARN] = index
		if id := resource.Attributes[model.AttrID]; id != "" {
			if _, taken := byKey[id]; !taken {
				byKey[id] = index
			}
		}
		if physical := resource.Attributes[model.AttrResourceID]; physical != "" {
			if _, taken := byKey[physical]; !taken {
				byKey[physical] = index
			}
		}
		if resource.Service == "eks" && resource.Type == "cluster" && resource.Name != "" {
			eksClusters[resource.ARN] = resource.Name
		}
	}

	// AppRegistry: an application names resources or stacks.
	appOf := map[int]string{}
	appOfStack := map[string]string{}
	for _, resource := range members {
		if resource.Type != "application" || resource.Name == "" {
			continue
		}
		for _, relation := range resource.Relations {
			if relation.Type != model.RelationAssociated {
				continue
			}
			if target, ok := byKey[relation.Target]; ok {
				if _, taken := appOf[target]; !taken {
					appOf[target] = resource.Name
				}
				if stack := members[target]; stack.Service == "cloudformation" && stack.Type == "stack" {
					appOfStack[stack.Name] = resource.Name
				}
			}
		}
	}
	// Auto Scaling groups name the instances they contain.
	asgOf := map[int]string{}
	for _, resource := range members {
		if resource.Type != "auto-scaling-group" {
			continue
		}
		for _, relation := range resource.Relations {
			if relation.Type != model.RelationContains {
				continue
			}
			if target, ok := byKey[relation.Target]; ok {
				if _, taken := asgOf[target]; !taken {
					asgOf[target] = resource.Name
				}
			}
		}
	}

	for index, resource := range members {
		if cluster, namespace, ok := eksOwner(resource, eksClusters); ok {
			if namespace != "" {
				placed[index] = placement{id: "eks:" + cluster + "/ns:" + namespace, name: namespace, rule: RuleNamespace, key: namespace}
			} else {
				placed[index] = placement{id: "eks:" + cluster, name: cluster, rule: RuleCluster, key: cluster}
			}
			continue
		}
		if place, ok := tagPlacement(resource, families); ok {
			placed[index] = place
			continue
		}
		stack := stackOf(resource)
		if app := appOf[index]; app != "" {
			placed[index] = placement{id: "appregistry:" + app, name: app, rule: RuleAppRegistry, key: app}
			continue
		}
		if app := appOfStack[stack]; stack != "" && app != "" {
			placed[index] = placement{id: "appregistry:" + app, name: app, rule: RuleAppRegistry, key: app}
			continue
		}
		if stack != "" && !baseline[stack] {
			placed[index] = placement{id: "stack:" + stack, name: stack, rule: RuleStack, key: stack}
			continue
		}
		if cluster := ecsCluster(resource); cluster != "" {
			placed[index] = placement{id: "ecs:" + cluster, name: cluster, rule: RuleCluster, key: cluster}
			continue
		}
		if resource.Type == "auto-scaling-group" && resource.Name != "" {
			placed[index] = placement{id: "asg:" + resource.Name, name: resource.Name, rule: RuleASG, key: resource.Name}
			continue
		}
		if name := asgOf[index]; name != "" {
			placed[index] = placement{id: "asg:" + name, name: name, rule: RuleASG, key: name}
		}
	}
	prefixRung(members, placed, baseline)
	vpcRung(members, placed)
	for index := range placed {
		if placed[index].id == "" {
			placed[index] = placement{id: UngroupedID, name: "Ungrouped", rule: RuleUngrouped}
		}
	}
	return placed
}

// tagPlacement places a resource by the first learned family it has a
// usable value for. The ID folds the value's case, so "Checkout" and
// "checkout" are one group.
func tagPlacement(resource model.Resource, families []string) (placement, bool) {
	for _, family := range families {
		if value, ok := familyValue(resource, family); ok {
			return placement{id: "tag:" + family + "=" + strings.ToLower(value), name: value, rule: RuleTag, key: family}, true
		}
	}
	return placement{}, false
}

// ecsCluster is the ECS cluster a resource is or belongs to.
func ecsCluster(resource model.Resource) string {
	if resource.Service != "ecs" {
		return ""
	}
	if resource.Type == "cluster" {
		return resource.Name
	}
	for _, relation := range resource.Relations {
		if relation.Type == model.RelationMemberOf {
			if name := ecsClusterFromARN(relation.Target); name != "" {
				return name
			}
		}
	}
	return ""
}

// prefixRung groups the resources still unplaced that share a name prefix.
// Each prefix is decided on its own, from the unplaced resources only: a
// two-word prefix ("acme-cart-") makes a group when at least
// MinPrefixMembers names start with it; the rest fall back to their first
// word ("orders-"), which makes a group on the same bound. Resources with
// other names never change a prefix's group, so adding or removing them
// keeps every ID. A baseline stack and what it made follow the landing
// zone's naming, which would only rebuild the baseline as a group, so they
// take no part.
func prefixRung(members []model.Resource, placed []placement, baseline map[string]bool) {
	words := make([][]string, len(members))
	twoWords := map[string]int{}
	for index, resource := range members {
		if placed[index].id != "" || baseline[stackOf(resource)] {
			continue
		}
		ws := nameWords(resource)
		if len(ws) < 2 || !usefulWord(ws[0]) {
			continue
		}
		words[index] = ws
		if len(ws) >= 3 && usefulWord(ws[1]) {
			twoWords[ws[0]+"-"+ws[1]]++
		}
	}
	prefixOf := make([]string, len(members))
	oneWord := map[string]int{}
	for index, ws := range words {
		if ws == nil {
			continue
		}
		if len(ws) >= 3 && twoWords[ws[0]+"-"+ws[1]] >= MinPrefixMembers {
			prefixOf[index] = ws[0] + "-" + ws[1]
			continue
		}
		oneWord[ws[0]]++
	}
	for index, ws := range words {
		if ws != nil && prefixOf[index] == "" && oneWord[ws[0]] >= MinPrefixMembers {
			prefixOf[index] = ws[0]
		}
	}
	for index, prefix := range prefixOf {
		if prefix != "" {
			placed[index] = placement{id: "prefix:" + prefix + "-", name: prefix, rule: RulePrefix, key: prefix + "-"}
		}
	}
}

// vpcRung groups the resources still unplaced by their VPC, when they span
// two or more: one VPC would only repeat the account.
func vpcRung(members []model.Resource, placed []placement) {
	vpcs := map[string]bool{}
	names := map[string]string{}
	for index, resource := range members {
		if resource.Type == "vpc" && resource.Name != "" {
			names[vpcOf(resource)] = resource.Name
		}
		if placed[index].id == "" {
			if vpc := vpcOf(resource); vpc != "" {
				vpcs[vpc] = true
			}
		}
	}
	if len(vpcs) < 2 {
		return
	}
	for index, resource := range members {
		if placed[index].id != "" {
			continue
		}
		if vpc := vpcOf(resource); vpc != "" {
			name := names[vpc]
			if name == "" {
				name = vpc
			}
			placed[index] = placement{id: "vpc:" + vpc, name: name, rule: RuleVPC, key: vpc}
		}
	}
}

// correct builds one account's groups from the ladder's placements and
// applies the account's corrections, in order. mine indexes the account's
// corrections in all; statuses receives each one's status.
func correct(account string, members []model.Resource, placed []placement, all []Correction, mine []int, statuses []CorrectionStatus) []Group {
	type building struct {
		group   Group
		members map[string]bool
		names   map[string]int
	}
	groups := map[string]*building{}
	ensure := func(place placement) *building {
		entry := groups[place.id]
		if entry == nil {
			entry = &building{group: Group{ID: place.id, Account: account, Name: place.name, Rule: place.rule, Key: place.key},
				members: map[string]bool{}, names: map[string]int{}}
			groups[place.id] = entry
		}
		return entry
	}
	ensure(placement{id: UngroupedID, name: "Ungrouped", rule: RuleUngrouped})
	where := map[string]string{}
	typeOf := map[string]string{}
	for index, resource := range members {
		entry := ensure(placed[index])
		entry.members[resource.ARN] = true
		entry.names[placed[index].name]++
		where[resource.ARN] = placed[index].id
		typeOf[resource.ARN] = resource.Type
	}
	// A tag group shows the spelling most of its members use.
	for _, entry := range groups {
		best, count := entry.group.Name, -1
		for name, seen := range entry.names {
			if seen > count || (seen == count && name < best) {
				best, count = name, seen
			}
		}
		entry.group.Name = best
	}

	// A group a merge folded away is gone: a later correction naming it
	// no longer matches, and is never aimed at the group it went into.
	known := func(id string) bool { return groups[id] != nil }
	move := func(arn, from, to string) {
		delete(groups[from].members, arn)
		groups[to].members[arn] = true
		where[arn] = to
	}
	for _, index := range mine {
		correction := all[index]
		switch correction.Kind {
		case CorrectionRename:
			name := strings.TrimSpace(correction.Name)
			// A group every member was moved out of is not shown, so it is
			// gone for a rename too.
			if correction.GroupID == UngroupedID || !known(correction.GroupID) || len(groups[correction.GroupID].members) == 0 || name == "" {
				continue
			}
			groups[correction.GroupID].group.Name, groups[correction.GroupID].group.Renamed = name, true
		case CorrectionMerge:
			from, to := correction.GroupID, correction.TargetGroupID
			if from == UngroupedID || to == UngroupedID || !known(from) || !known(to) {
				continue
			}
			// A merge into itself is the neutral correction that undoes an
			// inherited merge: the group stays as it is.
			if from != to {
				for arn := range groups[from].members {
					move(arn, from, to)
				}
				delete(groups, from)
			}
		case CorrectionMove:
			from, ok := where[correction.MemberARN]
			target := correction.TargetGroupID
			if !ok || target == "" {
				continue
			}
			if !known(target) {
				name, isUser := strings.CutPrefix(target, UserGroupPrefix)
				if !isUser || strings.TrimSpace(name) == "" {
					continue
				}
				ensure(placement{id: target, name: strings.TrimSpace(name), rule: RuleUser})
			}
			move(correction.MemberARN, from, target)
		default:
			continue
		}
		statuses[index] = CorrectionApplied
	}

	out := make([]Group, 0, len(groups))
	for _, entry := range groups {
		if len(entry.members) == 0 && entry.group.ID != UngroupedID {
			continue
		}
		group := entry.group
		group.Members = make([]string, 0, len(entry.members))
		group.ByType = map[string]int{}
		for arn := range entry.members {
			group.Members = append(group.Members, arn)
			group.ByType[typeOf[arn]]++
		}
		sort.Strings(group.Members)
		out = append(out, group)
	}
	sort.Slice(out, func(left, right int) bool {
		a, b := out[left], out[right]
		if (a.ID == UngroupedID) != (b.ID == UngroupedID) {
			return b.ID == UngroupedID
		}
		return a.ID < b.ID
	})
	return out
}

// Of returns the groups of one account.
func (result Result) Of(account string) []Group {
	var out []Group
	for _, group := range result.Groups {
		if group.Account == account {
			out = append(out, group)
		}
	}
	return out
}

// GroupOf maps every grouped member ARN to its group ID.
func (result Result) GroupOf() map[string]string {
	out := map[string]string{}
	for _, group := range result.Groups {
		for _, member := range group.Members {
			out[member] = group.ID
		}
	}
	return out
}
