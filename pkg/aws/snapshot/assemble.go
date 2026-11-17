package snapshot

import (
	"sort"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// taggableTypes are the resource types a "no tags" finding applies to.
var taggableTypes = map[string]bool{
	"instance": true, "load-balancer": true, "db-instance": true, "db-cluster": true,
	"function": true, "queue": true, "table": true, "bucket": true, "service": true,
	"cluster": true, "topic": true, "cache-cluster": true, "state-machine": true,
	"stream": true, "auto-scaling-group": true, "rest-api": true, "distribution": true,
}

// alarmableTypes are the resource types a "no alarms" finding applies to.
var alarmableTypes = map[string]bool{
	"instance": true, "load-balancer": true, "target-group": true, "db-instance": true,
	"db-cluster": true, "function": true, "queue": true, "table": true, "service": true,
}

// assemble merges one account's generations into a single view. Auxiliary
// units (tags, AppRegistry) enrich other resources; CloudFormation stacks add
// membership; applications are resolved in the configured source order;
// derived flags are only claimed when the unit that would disprove them is
// committed.
func assemble(generations map[string]*Generation, sources []string) accountView {
	if len(sources) == 0 {
		sources = DefaultAppSources()
	}
	resources := make([]model.Resource, 0)
	tagSets := make(map[string]map[string]string)
	tagApps := make(map[string]model.Resource)
	tagPairs := make(map[string]bool)
	alarmPairs := make(map[string]bool)
	var applications []model.Resource
	var health []model.Health
	var flags []model.SecurityFlag

	keys := make([]string, 0, len(generations))
	for key := range generations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		generation := generations[key]
		unit := generation.Unit
		health = append(health, generation.Health...)
		flags = append(flags, generation.Flags...)
		switch unit.Service {
		case TagService:
			tagPairs[pairKey(unit.Account, unit.Region)] = true
			for _, tagged := range generation.Resources {
				tagSets[tagged.ARN] = tagged.Tags
				if tagged.App != "" && tagged.AppSource == AppSourceTags {
					tagApps[tagged.ARN] = tagged
				}
			}
			continue
		case AppRegistryService:
			applications = append(applications, generation.Resources...)
		case AlarmService:
			alarmPairs[pairKey(unit.Account, unit.Region)] = true
		}
		for _, resource := range generation.Resources {
			resources = append(resources, cloneResource(resource))
		}
	}

	byARN := make(map[string]int, len(resources))
	byID := make(map[string]int, len(resources))
	for position, resource := range resources {
		byARN[resource.ARN] = position
		if id := resource.Attributes[model.AttrID]; id != "" {
			byID[id] = position
		}
		if physical := resource.Attributes[model.AttrResourceID]; physical != "" {
			byID[physical] = position
		}
	}
	find := func(target string) (int, bool) {
		if position, ok := byARN[target]; ok {
			return position, true
		}
		position, ok := byID[target]
		return position, ok
	}

	for arn, tags := range tagSets {
		position, ok := byARN[arn]
		if !ok {
			continue
		}
		if resources[position].Tags == nil {
			resources[position].Tags = make(map[string]string, len(tags))
		}
		for key, value := range tags {
			if _, present := resources[position].Tags[key]; !present {
				resources[position].Tags[key] = value
			}
		}
		// Services whose list call carries no tags learn their tag-based
		// application from the tagging API.
		if tagged, ok := tagApps[arn]; ok && resources[position].App == "" {
			resources[position].App, resources[position].AppSource, resources[position].AppTag = tagged.App, AppSourceTags, tagged.AppTag
		}
	}

	// Stack membership: a stack's associated targets name its members.
	stackOf := make(map[int]int)
	for position, resource := range resources {
		if resource.Service != "cloudformation" || resource.Type != "stack" {
			continue
		}
		for _, relation := range resource.Relations {
			if relation.Type != model.RelationAssociated {
				continue
			}
			member, ok := find(relation.Target)
			if !ok || member == position {
				continue
			}
			resources[member].Relations = append(resources[member].Relations, model.Relation{Type: model.RelationPartOfStack, Target: resource.ARN})
			if resources[member].Attributes == nil {
				resources[member].Attributes = make(map[string]string)
			}
			resources[member].Attributes[model.AttrStackName] = resource.Name
			stackOf[member] = position
		}
	}

	// AppRegistry: an application names stacks or resources directly.
	appOf := make(map[int]string)
	for _, application := range applications {
		for _, relation := range application.Relations {
			if relation.Type != model.RelationAssociated {
				continue
			}
			target, ok := find(relation.Target)
			if !ok {
				continue
			}
			appOf[target] = application.Name
		}
	}

	monitored := make(map[string]bool)
	for _, resource := range resources {
		if resource.Type != "alarm" {
			continue
		}
		for _, relation := range resource.Relations {
			if relation.Type == model.RelationMonitors {
				monitored[relation.Target] = true
			}
		}
	}

	for position := range resources {
		resource := &resources[position]
		tagApp, tagSource, tagKey := resource.App, resource.AppSource, resource.AppTag
		resource.App, resource.AppSource, resource.AppTag = "", "", ""
		for _, source := range sources {
			switch source {
			case AppSourceAppRegistry:
				if name := appOf[position]; name != "" {
					resource.App, resource.AppSource = name, AppSourceAppRegistry
				} else if stack, ok := stackOf[position]; ok && appOf[stack] != "" {
					resource.App, resource.AppSource = appOf[stack], AppSourceAppRegistry
				}
			case AppSourceCloudFormation:
				if stack, ok := stackOf[position]; ok {
					resource.App, resource.AppSource = resources[stack].Name, AppSourceCloudFormation
				}
			case AppSourceTags:
				if tagApp != "" && tagSource == AppSourceTags {
					resource.App, resource.AppSource, resource.AppTag = tagApp, AppSourceTags, tagKey
				}
			}
			if resource.App != "" {
				break
			}
		}
		*resource = model.Sanitize(*resource)

		pair := pairKey(resource.Account, resource.Region)
		if taggableTypes[resource.Type] && len(resource.Tags) == 0 && tagPairs[pair] {
			flags = append(flags, model.SecurityFlag{Resource: resource.ARN, Account: resource.Account, Region: resource.Region, Kind: model.FlagNoTags, Summary: "No tags: owner and application cannot be told from tags"})
		}
		if alarmableTypes[resource.Type] && !monitored[resource.ARN] && alarmPairs[pair] {
			flags = append(flags, model.SecurityFlag{Resource: resource.ARN, Account: resource.Account, Region: resource.Region, Kind: model.FlagNoAlarms, Summary: "No CloudWatch alarm watches this resource"})
		}
	}

	sort.Slice(resources, func(left, right int) bool { return resources[left].ARN < resources[right].ARN })
	sort.SliceStable(flags, func(left, right int) bool {
		if flags[left].Resource != flags[right].Resource {
			return flags[left].Resource < flags[right].Resource
		}
		return flags[left].Kind < flags[right].Kind
	})
	if health == nil {
		health = []model.Health{}
	}
	if flags == nil {
		flags = []model.SecurityFlag{}
	}
	return accountView{resources: resources, health: health, flags: flags}
}

func cloneResource(resource model.Resource) model.Resource {
	out := resource
	if resource.Tags != nil {
		out.Tags = make(map[string]string, len(resource.Tags))
		for key, value := range resource.Tags {
			out.Tags[key] = value
		}
	}
	if resource.Attributes != nil {
		out.Attributes = make(map[string]string, len(resource.Attributes))
		for key, value := range resource.Attributes {
			out.Attributes[key] = value
		}
	}
	out.Relations = append([]model.Relation(nil), resource.Relations...)
	return out
}
