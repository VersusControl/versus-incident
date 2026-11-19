package suspect

import "github.com/VersusControl/versus-incident/pkg/aws/model"

// Related scores inventory resources by their relation to the anchor's own
// resources (narrowed): the resources themselves and the scaling groups
// that contain them; then the resources of their applications; then their
// network path — the route tables routing to their NAT gateways or attached
// to their subnets, those subnets, and the workloads in subnets routed
// through their NAT gateways. Keys are inventory ARNs.
func Related(all, narrowed []model.Resource) map[string]Scope {
	scopes := map[string]Scope{}
	set := func(arn string, scope Scope) {
		if arn != "" && scope > scopes[arn] {
			scopes[arn] = scope
		}
	}
	apps, nats, subnets := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, resource := range narrowed {
		set(resource.ARN, ScopeResource)
		if resource.App != "" {
			apps[resource.App] = true
		}
		if resource.Type == "nat-gateway" {
			nats[resource.ARN] = true
		}
		for _, subnet := range relationTargets(resource, model.RelationInSubnet) {
			if resource.Type != "nat-gateway" {
				subnets[subnet] = true
			}
		}
	}
	routedSubnets := map[string]bool{}
	for _, resource := range all {
		switch resource.Type {
		case "auto-scaling-group":
			for _, member := range relationTargets(resource, model.RelationContains) {
				if scopes[member] == ScopeResource {
					set(resource.ARN, ScopeResource)
				}
			}
		case "route-table":
			viaNAT := false
			for _, target := range relationTargets(resource, model.RelationRoutesTo) {
				viaNAT = viaNAT || nats[target]
			}
			attached := relationTargets(resource, model.RelationAttachedTo)
			ownSubnet := false
			for _, subnet := range attached {
				ownSubnet = ownSubnet || subnets[subnet]
			}
			if viaNAT || ownSubnet {
				set(resource.ARN, ScopeNetwork)
			}
			if viaNAT {
				for _, subnet := range attached {
					routedSubnets[subnet] = true
				}
			}
		}
	}
	for _, resource := range all {
		if resource.App != "" && apps[resource.App] {
			set(resource.ARN, ScopeApplication)
		}
		if resource.Type == "subnet" && (subnets[resource.ARN] || routedSubnets[resource.ARN]) {
			set(resource.ARN, ScopeNetwork)
		}
		for _, subnet := range relationTargets(resource, model.RelationInSubnet) {
			if routedSubnets[subnet] {
				set(resource.ARN, ScopeNetwork)
			}
		}
	}
	return scopes
}

func relationTargets(resource model.Resource, kind model.RelationType) []string {
	var out []string
	for _, relation := range resource.Relations {
		if relation.Type == kind {
			out = append(out, relation.Target)
		}
	}
	return out
}

// Resolve finds the inventory resource a canonical ARN names, comparing
// through Key, so the name form of an Auto Scaling group finds the group
// with its generated ID.
func Resolve(arn string, inventory []model.Resource) (model.Resource, bool) {
	key := Key(arn)
	if key == "" {
		return model.Resource{}, false
	}
	for _, resource := range inventory {
		if Key(resource.ARN) == key {
			return resource, true
		}
	}
	return model.Resource{}, false
}
