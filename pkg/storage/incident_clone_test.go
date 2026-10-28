package storage_test

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestCloneIncidentRecordDetachesDetectionState(t *testing.T) {
	now := time.Now().UTC()
	original := &storage.IncidentRecord{
		ID: "incident", ChannelsNotified: []string{"slack"}, DetectionFirstSeen: &now,
		DetectionLastSeen: &now, OccurrenceCount: 7,
		Content: map[string]interface{}{
			"Frequency": int64(7),
			"nested":    map[string]interface{}{"samples": []interface{}{"one"}},
		},
	}
	clone := storage.CloneIncidentRecord(original)
	clone.ChannelsNotified[0] = "email"
	clone.Content["Frequency"] = int64(9)
	clone.Content["nested"].(map[string]interface{})["samples"].([]interface{})[0] = "two"
	changed := now.Add(time.Minute)
	*clone.DetectionLastSeen = changed

	if original.ChannelsNotified[0] != "slack" || original.Content["Frequency"] != int64(7) ||
		original.Content["nested"].(map[string]interface{})["samples"].([]interface{})[0] != "one" ||
		!original.DetectionLastSeen.Equal(now) {
		t.Fatalf("clone mutated original: %+v", original)
	}
}
