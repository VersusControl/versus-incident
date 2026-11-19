package cost

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/aws/suspect"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// Inventory is the collected AWS inventory and change history the FinOps
// adapter reads: the snapshot store. Reading it never calls AWS.
type Inventory interface {
	Resources(ctx context.Context, q snapshot.Query) (snapshot.ResourcesResult, error)
	Changes(ctx context.Context, q snapshot.Query, since, until time.Time) (snapshot.ChangesResult, error)
}

// lineTypes maps a neutral service key to the inventory types its cost
// belongs to.
var lineTypes = map[string][]string{
	"network.nat":          {"nat-gateway"},
	"compute.vm":           {"instance", "auto-scaling-group"},
	"compute.container":    {"service", "cluster"},
	"compute.kubernetes":   {"cluster", "node-group"},
	"compute.function":     {"function"},
	"database.relational":  {"db-instance", "db-cluster"},
	"database.keyvalue":    {"table"},
	"database.cache":       {"cache-cluster"},
	"storage.object":       {"bucket"},
	"network.loadbalancer": {"load-balancer", "target-group"},
	"network.cdn":          {"distribution"},
	"network.endpoint":     {"vpc-endpoint"},
	"messaging.queue":      {"queue"},
	"messaging.stream":     {"stream"},
}

// SuspectFinder ranks the changes that likely started a cost increase,
// from the collected change history and inventory only.
type SuspectFinder struct {
	inv       Inventory
	resources ResourceLiner
}

// ResourceLiner gives resource-level cost lines, when the provider bills
// them, to narrow a line to the resources that actually cost.
type ResourceLiner interface {
	ResourceLines(ctx context.Context, r finops.DateRange) ([]finops.CostLine, error)
}

// WithResourceLines narrows suspects with resource-level cost.
func (f *SuspectFinder) WithResourceLines(r ResourceLiner) *SuspectFinder {
	f.resources = r
	return f
}

var (
	_ finops.SuspectFinder = (*SuspectFinder)(nil)
	_ finops.SuspectRanker = (*SuspectFinder)(nil)
)

// NewSuspectFinder builds the finder over the snapshot store.
func NewSuspectFinder(inv Inventory) *SuspectFinder { return &SuspectFinder{inv: inv} }

// Suspect returns the best candidate, or the state that says why there is
// none.
func (f *SuspectFinder) Suspect(ctx context.Context, d finops.Detected) (finops.SuspectView, *finops.UsageSignal, error) {
	list, sig, err := f.Suspects(ctx, d)
	if err != nil || len(list) == 0 {
		return finops.SuspectView{}, sig, err
	}
	return list[0], sig, nil
}

// Suspects ranks up to finops.MaxSuspects candidate changes on the line's
// resources, their applications, their scaling groups and their network
// path, through the shared suspect engine. The anchor start is the end of
// the increase's first day: daily cost cannot place a change inside that
// day, and no change after it is a candidate. The engine searches the line's
// Region and the account's global stream, matches canonical ARNs only, and
// reports none_found only when the whole window is read and settled, else
// history_incomplete; it never guesses. The usage signal comes from billed
// quantity in the FinOps core, so it is left nil here.
func (f *SuspectFinder) Suspects(ctx context.Context, d finops.Detected) ([]finops.SuspectView, *finops.UsageSignal, error) {
	region := RegionOf(d.ServiceName, d.Key.UsageType)
	if region == finops.GlobalRegion || region == finops.UnknownRegion {
		region = ""
	}
	inv, err := f.inv.Resources(ctx, snapshot.Query{Account: d.Key.Account, Region: region})
	if err != nil {
		return nil, nil, err
	}
	hasData := false
	for _, u := range inv.Units {
		hasData = hasData || u.HasData
	}
	if !hasData {
		return []finops.SuspectView{{State: "collectors_pending"}}, nil, nil
	}
	ids, err := f.resourceIDs(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	anchor := suspect.Anchor{
		Kind:     suspect.AnchorCost,
		ID:       d.Key.Account + "/" + d.Key.Service + "/" + d.Key.UsageType,
		Account:  d.Key.Account,
		Start:    d.StartedOn.Time().Add(24 * time.Hour),
		Lookback: suspect.CostLookback,
		Targets:  suspect.Related(inv.Resources, narrowResources(inv.Resources, d, ids)),
		Limit:    finops.MaxSuspects,
	}
	if region != "" {
		anchor.Regions = []string{region}
	}
	source := suspect.NewSnapshot(f.inv)
	result, err := suspect.Find(ctx, anchor, source, source)
	if err != nil {
		return nil, nil, err
	}
	// A line that narrows to no resource matches nothing; like an empty
	// match it is "none found" only over a complete window.
	switch {
	case result.Status == suspect.StatusFound:
	case result.Status == suspect.StatusNone,
		result.Status == suspect.StatusResourceUnknown && result.Complete:
		return []finops.SuspectView{{State: "none_found"}}, nil, nil
	default:
		return []finops.SuspectView{{State: "history_incomplete"}}, nil, nil
	}
	tieNote := "Several changes fit this cost line equally well"
	if len(ids) == 0 {
		tieNote += "; turn on resource-level data in Cost Explorer to narrow it to the resource that billed"
	}
	out := make([]finops.SuspectView, 0, len(result.Candidates))
	for _, c := range result.Candidates {
		sv := finops.SuspectView{State: "found", Summary: c.Change.Summary, Actor: actorName(c.Change.Actor),
			At: c.Change.Time.UTC(), Confidence: c.Confidence, Ref: "change:" + c.Change.ID}
		if c.Tied {
			sv.Tied, sv.Note = true, tieNote
		}
		out = append(out, sv)
	}
	return out, nil, nil
}

// actorName is the role or user name of the principal, never a session.
func actorName(a model.Actor) string {
	if a.PrincipalARN != "" {
		name := a.PrincipalARN
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		} else if i := strings.LastIndex(name, ":"); i >= 0 {
			name = name[i+1:]
		}
		if name != "" {
			return name
		}
	}
	if a.Class != "" && a.Class != model.ActorUnknown {
		return string(a.Class)
	}
	return ""
}

func relationTargets(r model.Resource, t model.RelationType) []string {
	var out []string
	for _, rel := range r.Relations {
		if rel.Type == t {
			out = append(out, rel.Target)
		}
	}
	return out
}

func vpcOf(r model.Resource) string {
	if v := r.Attributes[model.AttrVPC]; v != "" {
		return v
	}
	for _, t := range relationTargets(r, model.RelationInVPC) {
		if i := strings.LastIndex(t, "/"); i >= 0 {
			return t[i+1:]
		}
	}
	return ""
}

// resourceIDs are the resources billed on the line around and since the
// start day, when resource-level cost is available.
func (f *SuspectFinder) resourceIDs(ctx context.Context, d finops.Detected) ([]string, error) {
	if f.resources == nil {
		return nil, nil
	}
	lines, err := f.resources.ResourceLines(ctx, finops.DateRange{Start: d.StartedOn.AddDays(-2), End: d.StartedOn.AddDays(60)})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range lines {
		if l.Account == d.Key.Account && l.Service == d.Key.Service && l.UsageType == d.Key.UsageType && l.ResourceID != "" && !seen[l.ResourceID] {
			seen[l.ResourceID] = true
			out = append(out, l.ResourceID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// matchesID reports whether a resource is the billed resource ID (an ID or
// an ARN).
func matchesID(r model.Resource, id string) bool {
	return r.ARN == id || r.Attributes[model.AttrID] == id || strings.HasSuffix(r.ARN, "/"+id)
}

// narrowResources picks the line's actual resources: the billed resource
// IDs when resource-level cost exists; otherwise what the usage type names
// (the instance type of BoxUsage:<type>, the NAT gateways of the line's
// region); otherwise the one resource of the line's type, if there is only
// one. It never widens to every resource of a type.
func narrowResources(all []model.Resource, d finops.Detected, ids []string) []model.Resource {
	types := map[string]bool{}
	for _, t := range lineTypes[d.Key.Service] {
		types[t] = true
	}
	var out []model.Resource
	if len(ids) > 0 {
		for _, r := range all {
			for _, id := range ids {
				if matchesID(r, id) {
					out = append(out, r)
					break
				}
			}
		}
		return out
	}
	ut := d.Key.UsageType
	switch {
	case d.Key.Service == "network.nat":
		for _, r := range all {
			if r.Type == "nat-gateway" && r.State != "deleted" {
				out = append(out, r)
			}
		}
		return out
	case d.Key.Service == "compute.vm" && strings.Contains(ut, "Usage:"):
		size := ut[strings.LastIndex(ut, ":")+1:]
		for _, r := range all {
			if r.Type == "instance" && r.Attributes[model.AttrSize] == size {
				out = append(out, r)
			}
		}
		return out
	}
	for _, r := range all {
		if types[r.Type] {
			out = append(out, r)
		}
	}
	if len(out) != 1 {
		return nil
	}
	return out
}
