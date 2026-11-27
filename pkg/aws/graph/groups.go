package graph

import (
	"sort"

	"github.com/VersusControl/versus-incident/pkg/aws/grouping"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// AccountEndpointPrefix starts a group link end that is another account
// rather than a group: "account:123400000003".
const AccountEndpointPrefix = "account:"

// GroupCard is one group at the Groups level: what made it and how many
// resources of each type it holds.
type GroupCard struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Rule      string         `json:"rule"`
	Key       string         `json:"key,omitempty"`
	Renamed   bool           `json:"renamed,omitempty"`
	Resources int            `json:"resources"`
	ByType    map[string]int `json:"by_type"`
}

// GroupLink summarises the links of one kind from one group to another.
// An end outside the account is "account:<id>" or an organization target;
// an end no account can be named for has To empty and Far saying what it
// is.
type GroupLink struct {
	From      string             `json:"from"`
	To        string             `json:"to,omitempty"`
	Far       string             `json:"far,omitempty"`
	Type      model.RelationType `json:"type"`
	Kind      model.RelationKind `json:"kind"`
	Count     int                `json:"count"`
	BothSides int                `json:"both_sides"`
	// Sources breaks Count down by the read that found the links
	// ("kms:GetKeyPolicy": 2); a link with no source is not in it.
	Sources map[string]int `json:"sources,omitempty"`
}

// GroupsView is the Groups level of one account: its groups as cards and
// the links between them, and to and from other accounts, summarised by
// kind with counts. Resources is how many resources the account holds.
type GroupsView struct {
	Account   string      `json:"account"`
	Resources int         `json:"resources"`
	Groups    []GroupCard `json:"groups"`
	Links     []GroupLink `json:"links"`
}

// Groups collapses the graph's nodes of one account into its groups. A
// node of the account no group holds (a fold of wide grants) stands with
// its first member's group; a link inside one group is not shown.
func Groups(g Graph, account string, groups []grouping.Group) GroupsView {
	view := GroupsView{Account: account, Groups: []GroupCard{}, Links: []GroupLink{}}
	groupOf := map[string]string{}
	for _, group := range groups {
		if group.Account != account {
			continue
		}
		view.Groups = append(view.Groups, GroupCard{ID: group.ID, Name: group.Name, Rule: group.Rule, Key: group.Key,
			Renamed: group.Renamed, Resources: len(group.Members), ByType: group.ByType})
		view.Resources += len(group.Members)
		for _, member := range group.Members {
			groupOf[member] = group.ID
		}
	}
	nodes := make(map[string]Node, len(g.Nodes))
	for _, node := range g.Nodes {
		nodes[node.ID] = node
	}
	endpoint := func(node Node) (end, far string) {
		if node.Account == account {
			if id, ok := groupOf[node.ID]; ok {
				return id, ""
			}
			for _, member := range node.Members {
				if id, ok := groupOf[member]; ok {
					return id, ""
				}
			}
			return grouping.UngroupedID, ""
		}
		switch {
		case node.Type == NodeOrganization:
			return model.OrgTarget(node.Name), ""
		case node.Account != "":
			return AccountEndpointPrefix + node.Account, ""
		case node.Type == NodeHost:
			return "", "accounts not connected"
		}
		return "", node.Description
	}
	type key struct {
		from, to, far string
		kind          model.RelationType
		how           model.RelationKind
	}
	links := map[key]*GroupLink{}
	for _, edge := range g.Edges {
		from, to := nodes[edge.From], nodes[edge.To]
		if from.Account != account && to.Account != account {
			continue
		}
		fromEnd, _ := endpoint(from)
		toEnd, far := endpoint(to)
		if fromEnd == "" || (fromEnd == toEnd && far == "") {
			continue
		}
		k := key{from: fromEnd, to: toEnd, far: far, kind: edge.Type, how: edge.Kind}
		link := links[k]
		if link == nil {
			link = &GroupLink{From: fromEnd, To: toEnd, Far: far, Type: edge.Type, Kind: edge.Kind}
			links[k] = link
		}
		link.Count += max(edge.Count, 1)
		if edge.Sides == BothSides {
			link.BothSides += max(edge.Count, 1)
		}
		if edge.Source != "" {
			if link.Sources == nil {
				link.Sources = map[string]int{}
			}
			link.Sources[edge.Source] += max(edge.Count, 1)
		}
	}
	for _, link := range links {
		view.Links = append(view.Links, *link)
	}
	sort.Slice(view.Links, func(left, right int) bool {
		a, b := view.Links[left], view.Links[right]
		switch {
		case a.From != b.From:
			return a.From < b.From
		case a.To != b.To:
			return a.To < b.To
		case a.Far != b.Far:
			return a.Far < b.Far
		case a.Type != b.Type:
			return a.Type < b.Type
		}
		return a.Kind < b.Kind
	})
	return view
}
