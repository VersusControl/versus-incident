package cost

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// Inventory is the collected AWS inventory and change history the FinOps
// adapter reads: the snapshot store. Reading it never calls AWS.
type Inventory interface {
	Resources(ctx context.Context, q snapshot.Query) (snapshot.ResourcesResult, error)
	Changes(ctx context.Context, q snapshot.Query, since, until time.Time) (snapshot.ChangesResult, error)
}

// Suspect window around an increase's first day: changes from 48 hours
// before it starts to the end of that day (cost lags the change).
const (
	suspectBefore = 48 * time.Hour
	suspectAfter  = 24 * time.Hour
)

// Scope ranks, best first.
const (
	scopeNone = iota
	scopeNetwork
	scopeApplication
	scopeResource
)

// costShaping are change events that typically move cost: scaling,
// instance type, storage class, route table, endpoint removal, new NAT,
// log retention, replicas and deployments that change what runs.
var costShaping = map[string]bool{
	"UpdateService": true, "CreateService": true, "RegisterTaskDefinition": true,
	"UpdateAutoScalingGroup": true, "SetDesiredCapacity": true, "PutScalingPolicy": true,
	"ModifyInstanceAttribute": true, "RunInstances": true, "ModifyDBInstance": true,
	"ModifyDBCluster": true, "CreateDBInstanceReadReplica": true, "CreateDBInstance": true,
	"CreateReplicationGroup": true, "ModifyReplicationGroup": true,
	"PutBucketLifecycleConfiguration": true, "DeleteBucketLifecycle": true,
	"CreateRoute": true, "ReplaceRoute": true, "DeleteRoute": true, "CreateRouteTable": true,
	"AssociateRouteTable": true, "ReplaceRouteTableAssociation": true,
	"DeleteVpcEndpoints": true, "CreateNatGateway": true,
	"PutRetentionPolicy": true, "DeleteRetentionPolicy": true,
	"UpdateFunctionConfiguration": true, "PutProvisionedConcurrencyConfig": true,
	"UpdateTable": true, "CreateNodegroup": true, "UpdateNodegroupConfig": true,
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

type candidate struct {
	change  model.Change
	scope   int
	shaping bool
	score   int64
}

// Suspects ranks up to finops.MaxSuspects candidate changes on the line's
// resources, their applications, their scaling groups and their network
// path, in the window around the start day. With no candidate it reports
// none_found only when the change history covers the whole window, else
// history_incomplete; it never guesses. The usage signal comes from billed
// quantity in the FinOps core, so it is left nil here.
func (f *SuspectFinder) Suspects(ctx context.Context, d finops.Detected) ([]finops.SuspectView, *finops.UsageSignal, error) {
	region := RegionOf(d.ServiceName, d.Key.UsageType)
	if region == finops.GlobalRegion || region == finops.UnknownRegion {
		region = ""
	}
	q := snapshot.Query{Account: d.Key.Account, Region: region}
	inv, err := f.inv.Resources(ctx, q)
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
	start := d.StartedOn.Time()
	ids, err := f.resourceIDs(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	scopes := relatedScopes(inv.Resources, narrowResources(inv.Resources, d, ids))
	changes, err := f.inv.Changes(ctx, q, start.Add(-suspectBefore), start.Add(suspectAfter))
	if err != nil {
		return nil, nil, err
	}
	var found []candidate
	for _, c := range changes.Changes {
		if c.ErrorCode != "" {
			continue
		}
		best := scopeNone
		for _, arn := range c.Resources {
			best = max(best, scopes[arn])
		}
		if best == scopeNone {
			continue
		}
		cand := candidate{change: c, scope: best, shaping: costShaping[c.EventName]}
		// Closer to the start day ranks higher; a change before the start is
		// more likely the cause than one after it.
		distance := c.Time.Sub(start)
		if distance < 0 {
			distance = -distance
		} else {
			distance += 12 * time.Hour
		}
		closeness := int64((suspectBefore + suspectAfter - distance) / time.Minute)
		cand.score = int64(best)*1_000_000 + closeness
		if cand.shaping {
			cand.score += 500_000
		}
		found = append(found, cand)
	}
	if len(found) == 0 {
		if changes.Complete {
			return []finops.SuspectView{{State: "none_found"}}, nil, nil
		}
		return []finops.SuspectView{{State: "history_incomplete"}}, nil, nil
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].score != found[j].score {
			return found[i].score > found[j].score
		}
		return found[i].change.ID < found[j].change.ID
	})
	// Two or more candidates at the top rank: none is singled out. They are
	// all listed as Possible, closest in time first.
	tiedAtTop := len(found) > 1 && found[1].scope == found[0].scope
	if tiedAtTop {
		top := found[0].scope
		sort.SliceStable(found, func(i, j int) bool {
			ti, tj := found[i].scope == top, found[j].scope == top
			if ti != tj {
				return ti
			}
			if ti {
				di, dj := absDuration(found[i].change.Time.Sub(start)), absDuration(found[j].change.Time.Sub(start))
				if di != dj {
					return di < dj
				}
				return found[i].change.ID < found[j].change.ID
			}
			return found[i].score > found[j].score
		})
	}
	tieNote := "Several changes fit this cost line equally well"
	if len(ids) == 0 {
		tieNote += "; turn on resource-level data in Cost Explorer to narrow it to the resource that billed"
	}
	out := make([]finops.SuspectView, 0, finops.MaxSuspects)
	for i, c := range found {
		if i == finops.MaxSuspects {
			break
		}
		confidence := "possible"
		if !tiedAtTop && c.scope == found[0].scope && (c.scope >= scopeApplication || (c.scope == scopeNetwork && c.shaping)) {
			confidence = "likely"
		}
		sv := finops.SuspectView{State: "found", Summary: c.change.Summary, Actor: actorName(c.change.Actor),
			At: c.change.Time.UTC(), Confidence: confidence, Ref: "change:" + c.change.ID}
		if tiedAtTop && c.scope == found[0].scope {
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

// relatedScopes scores resource ARNs by their relation to the line's
// narrowed resources: the resources themselves and the scaling groups that
// contain them; then the resources of their applications; then their
// network path — the route tables routing to their NAT gateways or
// attached to their subnets, those subnets, and the workloads in subnets
// routed through their NAT gateways (the traffic that NAT bills).
func relatedScopes(all, narrowed []model.Resource) map[string]int {
	scopes := map[string]int{}
	set := func(arn string, s int) {
		if arn != "" && s > scopes[arn] {
			scopes[arn] = s
		}
	}
	apps, nats, subnets := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range narrowed {
		set(r.ARN, scopeResource)
		if r.App != "" {
			apps[r.App] = true
		}
		if r.Type == "nat-gateway" {
			nats[r.ARN] = true
		}
		for _, s := range relationTargets(r, model.RelationInSubnet) {
			if r.Type != "nat-gateway" {
				subnets[s] = true
			}
		}
	}
	routedSubnets := map[string]bool{}
	for _, r := range all {
		switch r.Type {
		case "auto-scaling-group":
			for _, member := range relationTargets(r, model.RelationContains) {
				if scopes[member] == scopeResource {
					set(r.ARN, scopeResource)
				}
			}
		case "route-table":
			viaNAT := false
			for _, t := range relationTargets(r, model.RelationRoutesTo) {
				viaNAT = viaNAT || nats[t]
			}
			attached := relationTargets(r, model.RelationAttachedTo)
			ownSubnet := false
			for _, s := range attached {
				ownSubnet = ownSubnet || subnets[s]
			}
			if viaNAT || ownSubnet {
				set(r.ARN, scopeNetwork)
			}
			if viaNAT {
				for _, s := range attached {
					routedSubnets[s] = true
				}
			}
		}
	}
	for _, r := range all {
		if r.App != "" && apps[r.App] {
			set(r.ARN, scopeApplication)
		}
		if r.Type == "subnet" && (subnets[r.ARN] || routedSubnets[r.ARN]) {
			set(r.ARN, scopeNetwork)
		}
		for _, s := range relationTargets(r, model.RelationInSubnet) {
			if routedSubnets[s] {
				set(r.ARN, scopeNetwork)
			}
		}
	}
	return scopes
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
