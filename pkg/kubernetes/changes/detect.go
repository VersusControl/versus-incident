package changes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

type Type string

const (
	Created          Type = "created"
	Deleted          Type = "deleted"
	ImageChanged     Type = "image_changed"
	ReplicasChanged  Type = "replicas_changed"
	SpecChanged      Type = "spec_changed"
	RolloutStarted   Type = "rollout_started"
	RolloutCompleted Type = "rollout_completed"
	RolloutStalled   Type = "rollout_stalled"
	ConfigRefChanged Type = "config_ref_changed"
	ConfigChanged    Type = "config_changed"
	WarningBurst     Type = "warning_burst"
)

type FieldChange struct {
	Path string `json:"path"`
	From string `json:"from"`
	To   string `json:"to"`
}

type Change struct {
	RootUID         string        `json:"root_uid,omitempty"`
	ID              string        `json:"id"`
	Cluster         string        `json:"cluster"`
	Kind            string        `json:"kind"`
	Namespace       string        `json:"namespace,omitempty"`
	Name            string        `json:"name"`
	UID             string        `json:"uid"`
	Type            Type          `json:"type"`
	Fields          []FieldChange `json:"fields,omitempty"`
	At              time.Time     `json:"at"`
	Service         string        `json:"service,omitempty"`
	Source          string        `json:"source,omitempty"`
	Revision        string        `json:"revision,omitempty"`
	FieldsTruncated bool          `json:"fields_truncated,omitempty"`
	Coalesced       bool          `json:"coalesced,omitempty"`
	warning         *warningObservation
}

type warningObservation struct {
	UID         string
	Count       int64
	Previous    int64
	HasPrevious bool
	First       time.Time
	PreviousAt  time.Time
}

// Detect converts one applied projected index delta into a safe typed change.
func Detect(cluster string, delta index.Delta, at time.Time) (Change, bool) {
	if delta.Resync || cluster == "" || at.IsZero() {
		return Change{}, false
	}
	oldRecord, newRecord := delta.Old, delta.New
	var record *index.Record
	change := Change{Cluster: cluster, At: at.UTC(), Source: "observed"}
	switch {
	case oldRecord == nil && newRecord != nil:
		record = newRecord
		change.Type = Created
	case oldRecord != nil && newRecord == nil:
		record = oldRecord
		change.Type = Deleted
	case oldRecord != nil && newRecord != nil:
		record = newRecord
		if !equalStrings(oldRecord.Images, newRecord.Images) {
			change.Type = ImageChanged
			change.Fields = append(change.Fields, FieldChange{Path: "containers.images", From: strings.Join(oldRecord.Images, ", "), To: strings.Join(newRecord.Images, ", ")})
		}
		if oldRecord.Replicas != newRecord.Replicas {
			if change.Type == "" {
				change.Type = ReplicasChanged
			}
			change.Fields = append(change.Fields, FieldChange{Path: "spec.replicas", From: strconv.FormatInt(int64(oldRecord.Replicas), 10), To: strconv.FormatInt(int64(newRecord.Replicas), 10)})
		}
		if oldRecord.Generation != newRecord.Generation && change.Type == "" {
			change.Type = SpecChanged
			change.Fields = append(change.Fields, FieldChange{Path: "metadata.generation", From: strconv.FormatInt(oldRecord.Generation, 10), To: strconv.FormatInt(newRecord.Generation, 10)})
		}
	default:
		return Change{}, false
	}
	if change.Type == "" || record == nil {
		return Change{}, false
	}
	change.Kind, change.Namespace, change.Name, change.UID = record.Kind, record.Namespace, record.Name, record.UID
	change.RootUID = record.UID
	for _, owner := range record.Owners {
		if owner.UID != "" && (!owner.ControllerKnown || owner.Controller) {
			change.RootUID = owner.UID
			break
		}
	}
	if change.Type == ImageChanged {
		change.Revision = record.Revision
	}
	change.Service = record.Labels["app.kubernetes.io/name"]
	if change.Service == "" {
		change.Service = record.Labels["app"]
	}
	change.ID = observedChangeID(change, record)
	boundChangeFields(&change)
	return change, true
}

func observedChangeID(change Change, record *index.Record) string {
	if change.Revision != "" && change.Type != Created && change.Type != Deleted && change.Type != ConfigChanged && change.Type != ConfigRefChanged {
		return changeID(change)
	}
	version := record.ResourceVersion
	if change.Type == Created || change.Type == Deleted {
		version = "lifecycle"
	} else if change.Type == RolloutStalled && !record.ProgressingTransitionAt.IsZero() {
		version = record.ProgressingTransitionAt.UTC().Format(time.RFC3339Nano)
	} else if record.Generation > 0 && change.Type != ConfigChanged && change.Type != ConfigRefChanged {
		version = "generation:" + strconv.FormatInt(record.Generation, 10)
	}
	identity := []string{change.Cluster, change.Kind, change.Namespace, change.UID, string(change.Type), version}
	if version == "" {
		for _, field := range change.Fields {
			identity = append(identity, field.Path, field.To)
		}
	}
	encoded, _ := json.Marshal(identity)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:16])
}

func changeID(change Change) string {
	value := change.Cluster + "\x00" + change.UID + "\x00" + string(change.Type) + "\x00" + change.At.Format(time.RFC3339Nano)
	if change.Revision != "" {
		value = change.Cluster + "\x00" + change.UID + "\x00" + string(change.Type) + "\x00revision:" + change.Revision
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func DetectAll(cluster string, delta index.Delta, at time.Time) []Change {
	var result []Change
	if base, ok := Detect(cluster, delta, at); ok && base.Kind != "Event" {
		result = append(result, base)
	}
	oldRecord, record := delta.Old, delta.New
	if record == nil || delta.Resync || cluster == "" || at.IsZero() {
		return result
	}
	add := func(kind Type, fields []FieldChange, when time.Time) {
		if when.IsZero() {
			when = at
		}
		change := Change{Cluster: cluster, Kind: record.Kind, Namespace: record.Namespace, Name: record.Name, UID: record.UID, Type: kind, Fields: fields, At: when.UTC(), Source: "observed", Revision: record.Revision}
		if kind == RolloutStarted && record.Kind == "ReplicaSet" && len(record.Owners) > 0 && record.Owners[0].Kind == "Deployment" {
			owner := record.Owners[0]
			change.Kind, change.Name, change.UID = owner.Kind, owner.Name, owner.UID
		}
		change.ID = observedChangeID(change, record)
		boundChangeFields(&change)
		result = append(result, change)
	}
	if oldRecord != nil {
		if (record.Kind == "ConfigMap" || record.Kind == "Secret") && oldRecord.ResourceVersion != "" && record.ResourceVersion != oldRecord.ResourceVersion {
			add(ConfigChanged, []FieldChange{{Path: "metadata.resourceVersion", From: oldRecord.ResourceVersion, To: record.ResourceVersion}}, at)
		}
		if !equalReferences(oldRecord.References, record.References) {
			add(ConfigRefChanged, []FieldChange{{Path: "references", From: referenceNames(oldRecord.References), To: referenceNames(record.References)}}, at)
		}
		if workload(record.Kind) && record.Generation > oldRecord.Generation {
			add(RolloutStarted, []FieldChange{{Path: "metadata.generation", From: strconv.FormatInt(oldRecord.Generation, 10), To: strconv.FormatInt(record.Generation, 10)}}, at)
		}
		if workload(record.Kind) && completed(*record) && (!completed(*oldRecord) || record.Generation != oldRecord.Generation) {
			add(RolloutCompleted, nil, at)
		}
		if workload(record.Kind) && stalled(*record) && !stalled(*oldRecord) {
			add(RolloutStalled, nil, record.ProgressingTransitionAt)
		}
	}
	if record.Kind == "ReplicaSet" && oldRecord == nil && record.Revision != "" {
		add(RolloutStarted, nil, record.CreatedAt)
	}
	if record.Kind == "Event" && record.EventType == "Warning" && record.Target != nil && record.UID != "" && record.EventCount > 0 && !record.EventLastAt.IsZero() && !record.EventFirstAt.IsZero() && !record.EventLastAt.Before(record.EventFirstAt) && (oldRecord == nil || record.EventCount > oldRecord.EventCount) {
		add(WarningBurst, nil, record.EventLastAt)
		change := &result[len(result)-1]
		change.UID, change.Kind, change.Name, change.Namespace = record.Target.UID, record.Target.Kind, record.Target.Name, record.Target.Namespace
		change.Revision = ""
		change.Fields = []FieldChange{{Path: "events.warning_count", To: strconv.Itoa(int(record.EventCount))}}
		change.ID = warningBurstID(*change, record.EventLastAt)
		change.warning = &warningObservation{UID: record.UID, Count: int64(record.EventCount), First: record.EventFirstAt}
		if oldRecord != nil && oldRecord.UID == record.UID {
			change.warning.Previous, change.warning.HasPrevious, change.warning.PreviousAt = int64(oldRecord.EventCount), true, oldRecord.EventLastAt
		}
	}
	return result
}

func DetectContinuous(cluster string, delta index.Delta, at time.Time) []Change {
	result := DetectAll(cluster, delta, at)
	for position := range result {
		change := &result[position]
		if change.Kind != "Pod" || change.Type != Created && change.Type != Deleted {
			continue
		}
		identity := *change
		record := delta.New
		if record == nil {
			record = delta.Old
		}
		if record != nil {
			for _, owner := range record.Owners {
				if owner.UID != "" && (!owner.ControllerKnown || owner.Controller) {
					identity.UID = owner.UID
					break
				}
			}
		}
		identity.At = time.Time{}
		if record != nil && !record.CreatedAt.IsZero() {
			identity.At = record.CreatedAt.UTC().Truncate(5 * time.Minute)
		}
		identity.Revision = ""
		change.ID = changeID(identity)
		change.Coalesced = true
	}
	return result
}

func warningBurstID(change Change, first time.Time) string {
	if first.IsZero() {
		first = change.At
	}
	change.At = first.UTC().Truncate(5 * time.Minute)
	return changeID(change)
}

func boundChangeFields(change *Change) {
	for position := range change.Fields {
		field := &change.Fields[position]
		if len(field.From) > 1024 {
			field.From = strings.ToValidUTF8(field.From[:1021], "?") + "..."
			change.FieldsTruncated = true
		}
		if len(field.To) > 1024 {
			field.To = strings.ToValidUTF8(field.To[:1021], "?") + "..."
			change.FieldsTruncated = true
		}
	}
}

func workload(kind string) bool {
	return kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet"
}
func completed(record index.Record) bool {
	return record.Generation > 0 && record.ObservedGeneration == record.Generation && record.Updated == record.Replicas && record.Ready == record.Replicas
}
func stalled(record index.Record) bool {
	return record.ProgressingStatus == "False" && record.ProgressingReason == "ProgressDeadlineExceeded"
}
func equalReferences(left, right map[string][]string) bool {
	for _, kind := range []string{"config_maps", "secrets", "persistent_volume_claims", "service_accounts"} {
		if len(left[kind]) != len(right[kind]) || len(left[kind]) > 32 {
			return false
		}
		first := append([]string(nil), left[kind]...)
		second := append([]string(nil), right[kind]...)
		sort.Strings(first)
		sort.Strings(second)
		if !equalStrings(first, second) {
			return false
		}
	}
	return true
}
func referenceNames(refs map[string][]string) string {
	var names []string
	for _, kind := range []string{"config_maps", "secrets", "persistent_volume_claims", "service_accounts"} {
		for _, name := range refs[kind][:min(len(refs[kind]), 32)] {
			names = append(names, kind+"/"+name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
