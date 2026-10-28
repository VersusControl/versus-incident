package changes

import (
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func Reconstruct(cluster string, records []index.Record, now time.Time) []Change {
	owners := map[string]index.Record{}
	for _, record := range records {
		owners[record.UID] = record
	}
	groups := map[string][]index.Record{}
	for _, record := range records {
		if (record.Kind == "ReplicaSet" || record.Kind == "ControllerRevision") && record.Revision != "" && len(record.Owners) > 0 {
			owner, exists := owners[record.Owners[0].UID]
			if exists && owner.Namespace == record.Namespace && owner.Kind == record.Owners[0].Kind && (record.Kind == "ReplicaSet" && owner.Kind == "Deployment" || record.Kind == "ControllerRevision" && (owner.Kind == "StatefulSet" || owner.Kind == "DaemonSet")) {
				groups[owner.UID] = append(groups[owner.UID], record)
			}
		}
	}
	result := []Change{}
	reconstructed := map[string]bool{}
	for _, revisions := range groups {
		sort.Slice(revisions, func(left, right int) bool {
			first, _ := strconv.ParseInt(revisions[left].Revision, 10, 64)
			second, _ := strconv.ParseInt(revisions[right].Revision, 10, 64)
			if first == second {
				return revisions[left].UID < revisions[right].UID
			}
			return first < second
		})
		if len(revisions) > 10 {
			revisions = revisions[len(revisions)-10:]
		}
		for position := 1; position < len(revisions); position++ {
			previous, current := revisions[position-1], revisions[position]
			if previous.Summary["revision_history_partial"] == "true" || current.Summary["revision_history_partial"] == "true" || current.CreatedAt.IsZero() || current.CreatedAt.Before(now.Add(-24*time.Hour)) || current.CreatedAt.After(now) || previous.Revision == current.Revision {
				continue
			}
			owner := current.Owners[0]
			reconstructed[owner.UID] = true
			base := Change{Cluster: cluster, Kind: owner.Kind, Namespace: current.Namespace, Name: owner.Name, UID: owner.UID, At: current.CreatedAt, Source: "reconstructed", Revision: current.Revision, Type: RolloutStarted}
			base.ID = changeID(base)
			result = append(result, base)
			if !equalStrings(previous.Images, current.Images) {
				image := base
				image.Type = ImageChanged
				image.Fields = []FieldChange{{Path: "containers.images", From: strings.Join(previous.Images, ", "), To: strings.Join(current.Images, ", ")}}
				image.ID = changeID(image)
				boundChangeFields(&image)
				result = append(result, image)
			}
		}
	}
	for _, record := range records {
		if (record.Kind == "StatefulSet" || record.Kind == "DaemonSet") && !reconstructed[record.UID] && !record.ProgressingTransitionAt.IsZero() && !record.ProgressingTransitionAt.After(now) && record.UpdateRevision != "" && record.CurrentRevision != "" && record.UpdateRevision != record.CurrentRevision {
			change := Change{Cluster: cluster, Kind: record.Kind, Namespace: record.Namespace, Name: record.Name, UID: record.UID, Type: RolloutStarted, At: record.ProgressingTransitionAt, Source: "observed", Revision: record.UpdateRevision, Fields: []FieldChange{{Path: "status.updateRevision", From: record.CurrentRevision, To: record.UpdateRevision}}}
			change.ID = changeID(change)
			result = append(result, change)
		}
	}
	result = append(result, reconstructWarnings(cluster, records, now)...)
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result
}

func reconstructWarnings(cluster string, records []index.Record, now time.Time) []Change {
	type burst struct {
		target index.OwnerRef
		count  int
		at     time.Time
		first  time.Time
	}
	groups := map[string]*burst{}
	for _, record := range records {
		if record.Kind != "Event" || record.EventType != "Warning" || record.Target == nil || record.EventFirstAt.IsZero() || record.EventFirstAt.Before(now.Add(-5*time.Minute)) || record.EventLastAt.After(now) || record.EventLastAt.Before(record.EventFirstAt) || record.EventLastAt.IsZero() || !record.EventFirstAt.UTC().Truncate(5*time.Minute).Equal(record.EventLastAt.UTC().Truncate(5*time.Minute)) {
			continue
		}
		bucket := record.EventFirstAt.UTC().Truncate(5 * time.Minute)
		key := record.Target.UID + "\x00" + record.Target.Kind + "\x00" + record.Target.Namespace + "\x00" + record.Target.Name + "\x00" + bucket.Format(time.RFC3339Nano)
		group := groups[key]
		if group == nil {
			group = &burst{target: *record.Target, first: bucket}
			groups[key] = group
		}
		group.count += max(1, int(record.EventCount))
		if record.EventLastAt.After(group.at) {
			group.at = record.EventLastAt
		}
	}
	result := []Change{}
	for _, group := range groups {
		if group.count < 5 {
			continue
		}
		change := Change{Cluster: cluster, Kind: group.target.Kind, Namespace: group.target.Namespace, Name: group.target.Name, UID: group.target.UID, At: group.at, Type: WarningBurst, Source: "reconstructed", Fields: []FieldChange{{Path: "events.warning_count", To: strconv.Itoa(group.count)}}}
		change.ID = warningBurstID(change, group.first)
		result = append(result, change)
	}
	return result
}

func DecodeRevision(reader io.Reader) ([]string, error) {
	type container struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}
	var patch struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers     []container `json:"containers"`
					InitContainers []container `json:"initContainers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	decoder := json.NewDecoder(io.LimitReader(reader, (4<<20)+1))
	if err := decoder.Decode(&patch); err != nil {
		return nil, errors.New("kubernetes changes: invalid controller revision")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("kubernetes changes: invalid controller revision")
	}
	containers := append(patch.Spec.Template.Spec.Containers, patch.Spec.Template.Spec.InitContainers...)
	if len(containers) == 0 || len(containers) > 16 {
		return nil, errors.New("kubernetes changes: invalid controller revision")
	}
	seen := map[string]bool{}
	images := []string{}
	for _, item := range containers {
		if !safeFilter(item.Name) || item.Name == "" || len(item.Name) > 63 || item.Image == "" || len(item.Image) > 512 || strings.ContainsAny(item.Image, "\x00\r\n") || seen[item.Name] {
			return nil, errors.New("kubernetes changes: invalid controller revision")
		}
		seen[item.Name] = true
		images = append(images, item.Name+"="+item.Image)
	}
	sort.Strings(images)
	return images, nil
}
