package breaks

import (
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

type LinkedChange struct {
	Change   changes.Change `json:"change"`
	Relation string         `json:"relation"`
	LeadSec  int64          `json:"lead_seconds"`
}
type Result struct {
	Changes     []LinkedChange
	MoreChanges int
	History     string
	Gaps        []changes.Gap
	Rebuilt     []string
	Untracked   []string
	Partial     bool
}

func Correlate(item Break, records []index.Record, edges graph.Graph, history changes.Page, partial bool, deniedKinds ...map[string]bool) Result {
	result := Result{Changes: []LinkedChange{}, History: "gap"}
	from, to := item.BrokeAt.Add(-30*time.Minute), item.BrokeAt.Add(2*time.Minute)
	byUID := map[string]index.Record{}
	byName := map[string]index.Record{}
	for _, record := range records {
		byUID[record.UID] = record
		byName[record.Kind+"\x00"+record.Namespace+"\x00"+record.Name] = record
	}
	family := map[string]bool{item.Root.UID: true}
	for _, record := range records {
		if Root(record, byUID).UID == item.Root.UID {
			family[record.UID] = true
		}
	}
	relations := map[string]string{item.Root.UID: "same_object"}
	for uid := range family {
		if uid != item.Root.UID {
			relations[uid] = "owned_by"
		}
	}
	for _, edge := range edges.Edges {
		if edge.Type == graph.Configures && family[edge.To] {
			relations[edge.From] = "used_by"
		}
		if edge.Type == graph.Uses && family[edge.From] {
			relations[edge.To] = "used_by"
		}
		if edge.Type == graph.Scales && family[edge.To] {
			relations[edge.From] = "scaled_by"
		}
	}
	needed := map[string]bool{"Pod": true, item.Root.Kind: true, "Event": true}
	untracked := map[string]bool{}
	for uid := range family {
		record := byUID[uid]
		if record.Summary["revision_history_partial"] == "true" || record.Summary["break_evidence_partial"] == "true" || record.Summary["references_partial"] == "true" {
			partial = true
		}
		if record.Kind != "" {
			needed[record.Kind] = true
		}
		for reference, kind := range map[string]string{"config_maps": "ConfigMap", "secrets": "Secret", "persistent_volume_claims": "PersistentVolumeClaim", "service_accounts": "ServiceAccount"} {
			for _, name := range record.References[reference] {
				tracked := kind != "PersistentVolumeClaim" && kind != "ServiceAccount"
				if tracked {
					needed[kind] = true
				} else {
					untracked[kind] = true
				}
				found := false
				if candidate, exists := byName[kind+"\x00"+record.Namespace+"\x00"+name]; exists {
					relations[candidate.UID] = "used_by"
					found = true
				}
				if !found && tracked {
					if len(deniedKinds) > 0 && deniedKinds[0][kind] {
						untracked[kind] = true
						delete(needed, kind)
					} else {
						partial = true
					}
				}
			}
		}
		if record.Kind == "Pod" && record.NodeName != "" && Rule(record, item.Evidence.To) != "" {
			needed["Node"] = true
			if node, exists := byName["Node\x00\x00"+record.NodeName]; exists {
				relations[node.UID] = "on_node"
			}
		}
	}
	needed["HorizontalPodAutoscaler"] = true
	covered := !partial
	result.Partial = partial
	for kind := range needed {
		interval, exists := history.Coverage[kind]
		if !exists || interval.From.After(from) || interval.To.Before(to) {
			covered = false
		}
	}
	for _, gap := range history.Gaps {
		if !gap.To.Before(from) && !gap.From.After(to) {
			result.Gaps = append(result.Gaps, gap)
			covered = false
		}
	}
	if covered {
		result.History = "complete"
	}
	if !covered && len(result.Gaps) == 0 {
		result.Gaps = []changes.Gap{{From: from, To: to}}
	}
	rebuilt := map[string]bool{}
	for _, change := range history.Items {
		relation := relations[change.UID]
		if relation == "" || change.At.Before(from) || change.At.After(to) || (change.Kind == "Pod" || change.Kind == "Event") && (change.Type == changes.Created || change.Type == changes.Deleted) {
			continue
		}
		if change.Source == "reconstructed" {
			if change.Type == changes.ImageChanged {
				rebuilt["image"] = true
			}
			if change.Type == changes.ReplicasChanged {
				rebuilt["replicas"] = true
			}
		}
		result.Changes = append(result.Changes, LinkedChange{Change: change, Relation: relation, LeadSec: int64(item.BrokeAt.Sub(change.At).Seconds())})
		if change.FieldsTruncated {
			result.Partial = true
		}
	}
	for kind := range rebuilt {
		result.Rebuilt = append(result.Rebuilt, kind)
	}
	for kind := range untracked {
		result.Untracked = append(result.Untracked, kind)
	}
	sort.Strings(result.Rebuilt)
	sort.Strings(result.Untracked)
	if !covered && len(result.Rebuilt) > 0 {
		result.History = "reconstructed"
	}
	sort.Slice(result.Changes, func(left, right int) bool {
		first, second := result.Changes[left], result.Changes[right]
		if relationRank(first.Relation) != relationRank(second.Relation) {
			return relationRank(first.Relation) < relationRank(second.Relation)
		}
		firstDistance, secondDistance := item.BrokeAt.Sub(first.Change.At), item.BrokeAt.Sub(second.Change.At)
		if firstDistance < 0 {
			firstDistance = -firstDistance
		}
		if secondDistance < 0 {
			secondDistance = -secondDistance
		}
		if firstDistance != secondDistance {
			return firstDistance < secondDistance
		}
		return first.Change.ID < second.Change.ID
	})
	if len(result.Changes) > 3 {
		result.MoreChanges = len(result.Changes) - 3
		result.Changes = result.Changes[:3]
	}
	sort.Slice(result.Changes, func(left, right int) bool {
		if result.Changes[left].Change.At.Equal(result.Changes[right].Change.At) {
			return result.Changes[left].Change.ID < result.Changes[right].Change.ID
		}
		return result.Changes[left].Change.At.After(result.Changes[right].Change.At)
	})
	return result
}

func relationRank(value string) int {
	for rank, relation := range []string{"same_object", "owned_by", "used_by", "scaled_by", "on_node"} {
		if value == relation {
			return rank
		}
	}
	return 99
}
