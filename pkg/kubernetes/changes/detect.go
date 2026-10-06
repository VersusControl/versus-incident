package changes

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

type Type string

const (
	Created         Type = "created"
	Deleted         Type = "deleted"
	ImageChanged    Type = "image_changed"
	ReplicasChanged Type = "replicas_changed"
	SpecChanged     Type = "spec_changed"
)

type FieldChange struct {
	Path string `json:"path"`
	From string `json:"from"`
	To   string `json:"to"`
}

type Change struct {
	ID        string        `json:"id"`
	Cluster   string        `json:"cluster"`
	Kind      string        `json:"kind"`
	Namespace string        `json:"namespace,omitempty"`
	Name      string        `json:"name"`
	UID       string        `json:"uid"`
	Type      Type          `json:"type"`
	Fields    []FieldChange `json:"fields,omitempty"`
	At        time.Time     `json:"at"`
	Service   string        `json:"service,omitempty"`
}

// Detect converts one applied projected index delta into a safe typed change.
func Detect(cluster string, delta index.Delta, at time.Time) (Change, bool) {
	if delta.Resync || cluster == "" || at.IsZero() {
		return Change{}, false
	}
	oldRecord, newRecord := delta.Old, delta.New
	var record *index.Record
	change := Change{Cluster: cluster, At: at.UTC()}
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
	change.Service = record.Labels["app.kubernetes.io/name"]
	if change.Service == "" {
		change.Service = record.Labels["app"]
	}
	change.ID = changeID(change)
	return change, true
}

func changeID(change Change) string {
	value := change.Cluster + "\x00" + change.UID + "\x00" + string(change.Type) + "\x00" + change.At.Format(time.RFC3339Nano)
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
