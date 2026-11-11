// Package finops is the cloud-neutral FinOps core: the cost model, the
// driver bridge (why the total moved), increase detection, ownership,
// budgets and forecast, savings rules, the request planner, the report
// model and the Service that the admin routes and the read-only agent tools
// share.
//
// Provider adapters map their billing data into CostLine and the neutral
// inputs here; this package imports no provider SDK. Every number is computed
// in Go, deterministically, in integer micro-units. Gaps are named, never
// reported as zero.
package finops

import (
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/finops/narrative"
)

// Record types of a cost line.
const (
	RecordUsage    = "usage"
	RecordDiscount = "discount"
	RecordCredit   = "credit"
	RecordRefund   = "refund"
	RecordFee      = "fee"
	RecordTax      = "tax"
)

// GlobalRegion is the region of services that have none.
const GlobalRegion = "global"

// UnknownRegion marks cost whose region the source could not determine. It
// is shown as its own value, never folded into Global.
const UnknownRegion = "unknown"

// Unallocated is the owner of cost no rule can assign.
const Unallocated = "Unallocated"

// CostLine is one day of cost for one (account, region, service, usage
// type, record type, resource, team) combination, in the single measure the
// page uses: net amortized cost (what you actually pay, after discounts,
// credits and commitments spread over their term).
type CostLine struct {
	Day         Date   `json:"day"`
	Account     string `json:"account"`
	Region      string `json:"region"`
	Service     string `json:"service"`                // neutral key, e.g. "network.nat"
	ServiceName string `json:"service_name,omitempty"` // provider display name (Details view)
	UsageType   string `json:"usage_type,omitempty"`   // provider value (Details view)
	// UsageGroup is a plain label for the usage type's group
	// ("instances", "disk volumes"), set by the adapter when known.
	UsageGroup string `json:"usage_group,omitempty"`
	RecordType string `json:"record_type"`
	ResourceID string `json:"resource_id,omitempty"` // empty outside the resource window
	Team       string `json:"team,omitempty"`        // allowlisted team tag value; "" when untagged
	App        string `json:"app,omitempty"`
	Env        string `json:"env,omitempty"`

	// Currency is the billing currency; empty means the source's currency.
	Currency           string `json:"currency,omitempty"`
	NetAmortizedMicros Micros `json:"net_amortized_micros"`
	QuantityMicros     int64  `json:"quantity_micros"`
	Unit               string `json:"unit,omitempty"`

	// EstimatedTeam and EstimatedRegion mark a line whose team or region
	// share comes from a proportional split of a coarser exact figure
	// (the source could not read that cross cell). Anything built from an
	// estimated share says so; it is never shown as a fact.
	EstimatedTeam   bool `json:"estimated_team,omitempty"`
	EstimatedRegion bool `json:"estimated_region,omitempty"`
}

// recordType normalizes an empty record type to usage.
func (l CostLine) recordType() string {
	if l.RecordType == "" {
		return RecordUsage
	}
	return l.RecordType
}

// LineKey identifies a cost line across days. Team is not part of the key:
// a tag change does not make a line new.
type LineKey struct {
	Account    string `json:"account"`
	Service    string `json:"service"`
	UsageType  string `json:"usage_type,omitempty"`
	RecordType string `json:"record_type"`
	ResourceID string `json:"resource_id,omitempty"`
}

// Key returns the line's key.
func (l CostLine) Key() LineKey {
	return LineKey{Account: l.Account, Service: l.Service, UsageType: l.UsageType, RecordType: l.recordType(), ResourceID: l.ResourceID}
}

func (k LineKey) String() string {
	return strings.Join([]string{k.Account, k.Service, k.UsageType, k.RecordType, k.ResourceID}, "|")
}

func lessKey(a, b LineKey) bool { return a.String() < b.String() }

// Account kinds and statuses.
const (
	AccountConnected = "connected" // inventory and change history readable
	AccountCostOnly  = "cost_only" // cost known from the payer; no change history

	AccountOK          = "ok"
	AccountNotReadable = "not_readable"
	AccountNew         = "new"
	AccountClosed      = "closed"
)

// Account is one billing account known to the cost source.
type Account struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // connected | cost_only
	Readable  bool   `json:"readable"`
	ErrorCode string `json:"error_code,omitempty"` // closed vocabulary from the adapter
	Owner     string `json:"owner,omitempty"`      // from config
	Group     string `json:"group,omitempty"`      // account group from config
	FirstCost Date   `json:"first_cost,omitzero"`
	ClosedOn  Date   `json:"closed_on,omitzero"`
}

// DisplayName prefers the account name.
func (a Account) DisplayName() string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

// TagStatus describes the owner tag (one key per deployment).
type TagStatus struct {
	Key string `json:"key"`
	// Active reports that the provider applies the key to billing data.
	Active bool `json:"active"`
	// Allowlisted reports that the key's values may be shown.
	Allowlisted bool `json:"allowlisted"`
}

// Owner is the result of the ownership rules.
type Owner struct {
	Name string `json:"name"`
	// Rule is "tag", "account" or "unallocated".
	Rule string `json:"rule"`
	// RuleText explains the matched rule in words.
	RuleText string `json:"rule_text"`
	// Estimated is true when the team behind the rule comes from a
	// proportional split; the owner is then "likely", never a fact.
	Estimated bool `json:"estimated,omitempty"`
}

// Ownership resolves the owner of cost. First match wins: the team tag
// (when its key is allowlisted), then the account owner from config, then
// Unallocated. The rule that matched is always reported.
type Ownership struct {
	Tag           TagStatus
	AccountOwners map[string]string
}

// Resolve returns the owner of a line in the given depth.
func (o Ownership) Resolve(account, team, subject string, d narrative.Depth) Owner {
	return o.ResolveShare(account, team, subject, false, d)
}

// ResolveShare resolves an owner whose team value may be an estimated
// share. The rule order is the same; an estimated result says so in its
// rule text.
func (o Ownership) ResolveShare(account, team, subject string, estimated bool, d narrative.Depth) Owner {
	var out Owner
	switch {
	case team != "" && o.Tag.Allowlisted:
		out = Owner{Name: team, Rule: "tag", RuleText: narrative.OwnerRuleText("tag", o.Tag.Key, team, subject, d)}
	case o.AccountOwners[account] != "":
		owner := o.AccountOwners[account]
		out = Owner{Name: owner, Rule: "account", RuleText: narrative.OwnerRuleText("account", o.Tag.Key, owner, "", d)}
	default:
		out = Owner{Name: Unallocated, Rule: "unallocated", RuleText: narrative.OwnerRuleText("unallocated", o.Tag.Key, "", "", d)}
	}
	if estimated {
		out.Estimated = true
		out.RuleText += " " + narrative.EstimatedMark
	}
	return out
}

// teamShare sums cost per team value ("" is untagged and competes as
// Unallocated) and remembers which values include an estimated share.
type teamShare struct {
	cost map[string]Micros
	est  map[string]bool
}

func (t *teamShare) add(l CostLine) {
	if t.cost == nil {
		t.cost, t.est = map[string]Micros{}, map[string]bool{}
	}
	t.cost[l.Team] += l.NetAmortizedMicros
	if l.EstimatedTeam {
		t.est[l.Team] = true
	}
}

// top returns the team with the most cost; ties go to the first value in
// order. The untagged value can win: most cost untagged means Unallocated.
func (t *teamShare) top() (string, bool) {
	best, found := "", false
	var bestAmt Micros
	for _, k := range sortedKeys(t.cost) {
		if !found || t.cost[k] > bestAmt {
			best, bestAmt, found = k, t.cost[k], true
		}
	}
	return best, t.est[best]
}

// sortedKeys returns map keys in order, for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
