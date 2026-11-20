package storage

import (
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// singleOrgScopes splits a read scope into one scope per organization, in
// read order.
func singleOrgScopes(scope tenancy.OrgScope) []tenancy.OrgScope {
	out := make([]tenancy.OrgScope, 0, len(scope.Read))
	for _, org := range scope.Read {
		out = append(out, tenancy.OrgScope{Write: org, Read: []string{org}})
	}
	return out
}

// awsGenerationBounds is the start of one organization's kept history of
// one unit: the oldest kept generation's sequence number and start time.
// A zero value means the organization keeps no generation of the unit.
type awsGenerationBounds struct {
	firstSeq  uint64
	firstFrom time.Time
}

// yieldsAt reports whether an organization without a generation current
// at asOf leaves the unit to the next organization in read order. It does
// when it never committed the unit, or when asOf is before its first
// generation and that first generation is still kept. After a prune the
// organization's start is unknown, so a time it no longer covers has no
// generation rather than an older organization's stale one. The latest
// view (zero asOf) is never before a kept generation.
func (bounds awsGenerationBounds) yieldsAt(asOf time.Time) bool {
	if bounds.firstSeq == 0 {
		return true
	}
	if bounds.firstSeq != 1 || asOf.IsZero() {
		return false
	}
	return asOf.Before(bounds.firstFrom)
}

// awsUnitPick is the organization that answers for one unit.
type awsUnitPick struct {
	status   AWSUnitStatus
	position int
}

// pickAWSUnits chooses, per unit, the organization that answers for it at
// asOf, given each read organization's unit statuses and generation bounds
// in read order. The first organization with a generation current at asOf
// answers; an organization without one passes the unit on only while it
// yields (see yieldsAt). The answering organization's sync state and
// generation are kept together. A unit no organization answers for keeps
// the first holder's sync state with no generation, so it reads as
// partial. The result is in unit order.
func pickAWSUnits(orgs []string, units [][]AWSUnitStatus, bounds []map[AWSUnit]awsGenerationBounds, asOf time.Time) ([]AWSUnit, map[AWSUnit]awsUnitPick) {
	candidates := map[AWSUnit][]awsUnitPick{}
	order := make([]AWSUnit, 0)
	for position, statuses := range units {
		for _, status := range statuses {
			status.Org = orgs[position]
			if _, seen := candidates[status.Unit]; !seen {
				order = append(order, status.Unit)
			}
			candidates[status.Unit] = append(candidates[status.Unit], awsUnitPick{status: status, position: position})
		}
	}
	sortUnits(order)
	picks := make(map[AWSUnit]awsUnitPick, len(order))
	for _, unit := range order {
		held := candidates[unit]
		chosen := held[0]
		chosen.status.Generation = nil
		for _, candidate := range held {
			if candidate.status.Generation != nil {
				chosen = candidate
				break
			}
			if !bounds[candidate.position][unit].yieldsAt(asOf) {
				break
			}
		}
		picks[unit] = chosen
	}
	return order, picks
}

// mergeAWSUnits is the unit list the whole read scope sees.
func mergeAWSUnits(orgs []string, units [][]AWSUnitStatus, bounds []map[AWSUnit]awsGenerationBounds, asOf time.Time) []AWSUnitStatus {
	order, picks := pickAWSUnits(orgs, units, bounds, asOf)
	out := make([]AWSUnitStatus, 0, len(order))
	for _, unit := range order {
		out = append(out, picks[unit].status)
	}
	return out
}

// mergeAccountViews joins one account's views, read per organization in
// read order, into the view the whole read scope sees. Each unit is
// answered by one organization, chosen by pickAWSUnits, and its sync
// state, generation, resources, health and flags all come from it. After
// a key is added the default organization's snapshots answer for the
// times before the licensed organization's first one, and until it
// commits, so a read before the upgrade and one after it see the same
// resources. A unit no organization answers for keeps the first holder's
// rows, as a single-organization read does.
func mergeAccountViews(orgs []string, views []AWSSnapshotView, bounds []map[AWSUnit]awsGenerationBounds, asOf time.Time) AWSSnapshotView {
	units := make([][]AWSUnitStatus, len(views))
	for position, view := range views {
		units[position] = view.Units
	}
	order, picks := pickAWSUnits(orgs, units, bounds, asOf)
	out := AWSSnapshotView{Units: make([]AWSUnitStatus, 0, len(order)), Resources: make([]AWSResource, 0), Health: make([]AWSHealth, 0), Flags: make([]AWSSecurityFlag, 0)}
	from := func(position int, account, region, service string) bool {
		chosen, ok := picks[AWSUnit{Account: account, Region: region, Service: service}]
		return ok && chosen.position == position
	}
	resources := make([]map[AWSUnit][]AWSResource, len(views))
	health := make([]map[AWSUnit][]AWSHealth, len(views))
	flags := make([]map[AWSUnit][]AWSSecurityFlag, len(views))
	for position, view := range views {
		resources[position], health[position], flags[position] = map[AWSUnit][]AWSResource{}, map[AWSUnit][]AWSHealth{}, map[AWSUnit][]AWSSecurityFlag{}
		for _, row := range view.Resources {
			if from(position, row.Account, row.Region, row.Service) {
				key := AWSUnit{Account: row.Account, Region: row.Region, Service: row.Service}
				resources[position][key] = append(resources[position][key], row)
			}
		}
		for _, row := range view.Health {
			if from(position, row.Account, row.Region, row.Service) {
				key := AWSUnit{Account: row.Account, Region: row.Region, Service: row.Service}
				health[position][key] = append(health[position][key], row)
			}
		}
		for _, row := range view.Flags {
			if from(position, row.Account, row.Region, row.Service) {
				key := AWSUnit{Account: row.Account, Region: row.Region, Service: row.Service}
				flags[position][key] = append(flags[position][key], row)
			}
		}
	}
	// Each organization's rows are already ordered within a unit, and units
	// are emitted in unit order, so the merged rows keep the order a
	// single-organization read gives.
	for _, unit := range order {
		chosen := picks[unit]
		out.Units = append(out.Units, chosen.status)
		out.Resources = append(out.Resources, resources[chosen.position][unit]...)
		out.Health = append(out.Health, health[chosen.position][unit]...)
		out.Flags = append(out.Flags, flags[chosen.position][unit]...)
	}
	return out
}

// mergeAWSUnitHistory is how far back each unit's history reaches across
// the read organizations, by the same rule the views answer with: an
// organization's generations count while every earlier organization in
// read order that keeps generations of the unit still keeps its first one
// and started after them. generations holds each organization's
// generation start times per unit, in read order.
func mergeAWSUnitHistory(generations []map[AWSUnit][]time.Time, bounds []map[AWSUnit]awsGenerationBounds) []AWSUnitHistory {
	byUnit := map[AWSUnit]*AWSUnitHistory{}
	closed := map[AWSUnit]bool{}
	boundary := map[AWSUnit]time.Time{}
	for position, starts := range generations {
		for unit, kept := range starts {
			if closed[unit] {
				continue
			}
			limit, limited := boundary[unit]
			for _, start := range kept {
				if limited && !start.Before(limit) {
					continue
				}
				item := byUnit[unit]
				if item == nil {
					item = &AWSUnitHistory{Unit: unit}
					byUnit[unit] = item
				}
				item.Generations++
				if item.Oldest.IsZero() || start.Before(item.Oldest) {
					item.Oldest = start
				}
			}
			own := bounds[position][unit]
			if own.firstSeq == 0 {
				continue
			}
			if own.firstSeq != 1 {
				closed[unit] = true
				continue
			}
			if !limited || own.firstFrom.Before(limit) {
				boundary[unit] = own.firstFrom
			}
		}
	}
	out := make([]AWSUnitHistory, 0, len(byUnit))
	for _, item := range byUnit {
		out = append(out, *item)
	}
	sortUnitHistory(out)
	return out
}

// answeringOrgOf maps each unit to the organization that answers for it.
func answeringOrgOf(orgs []string, units [][]AWSUnitStatus, bounds []map[AWSUnit]awsGenerationBounds, asOf time.Time) map[AWSUnit]string {
	_, picks := pickAWSUnits(orgs, units, bounds, asOf)
	out := make(map[AWSUnit]string, len(picks))
	for unit, pick := range picks {
		out[unit] = pick.status.Org
	}
	return out
}

// sortAWSResourceRows, sortAWSHealthRows and sortAWSFlagRows put merged
// rows in the order a single-organization read returns them.
func sortAWSResourceRows(rows []AWSResource) {
	sort.SliceStable(rows, func(left, right int) bool {
		a, b := rows[left], rows[right]
		if a.Account != b.Account || a.Region != b.Region || a.Service != b.Service {
			return lessAWSUnit(AWSUnit{a.Account, a.Region, a.Service}, AWSUnit{b.Account, b.Region, b.Service})
		}
		return a.ARN < b.ARN
	})
}

func sortAWSHealthRows(rows []AWSHealth) {
	sort.SliceStable(rows, func(left, right int) bool {
		a, b := rows[left], rows[right]
		return lessStringPairs([][2]string{{a.Account, b.Account}, {a.Region, b.Region}, {a.Service, b.Service}, {a.ARN, b.ARN}, {a.Kind, b.Kind}, {a.Ref, b.Ref}})
	})
}

func sortAWSFlagRows(rows []AWSSecurityFlag) {
	sort.SliceStable(rows, func(left, right int) bool {
		a, b := rows[left], rows[right]
		return lessStringPairs([][2]string{{a.Account, b.Account}, {a.Region, b.Region}, {a.Service, b.Service}, {a.ARN, b.ARN}, {a.Flag, b.Flag}})
	})
}

func lessStringPairs(pairs [][2]string) bool {
	for _, pair := range pairs {
		if pair[0] != pair[1] {
			return pair[0] < pair[1]
		}
	}
	return false
}

func sortUnits(units []AWSUnit) {
	sort.Slice(units, func(left, right int) bool { return lessAWSUnit(units[left], units[right]) })
}
